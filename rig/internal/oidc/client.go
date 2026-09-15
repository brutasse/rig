// Client negotiates OIDC tokens with an identity provider on behalf of a
// user: authorization-code flow with PKCE through a browser, or the
// device-code flow (RFC 8628). Issued tokens are verified against the
// issuer's JWKS (signature, iss, aud, exp) before being cached, so only
// tokens the gate would accept are ever used.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultAudience = "pier"
	defaultClientID = "rig"

	// noExpiryFallback bounds the cached lifetime of tokens whose
	// expiry cannot be determined from the token or the token response.
	noExpiryFallback = 10 * time.Minute
)

// ClientConfig is one gate's negotiation configuration. The Resolver fills
// it from the gate entry of the config file; tests build it directly.
type ClientConfig struct {
	// WellKnown is the issuer's OpenID discovery URL. Required.
	WellKnown string
	// Audience is the audience the resulting token must carry.
	Audience string
	// ClientID is the OAuth client identifier. Many IdPs (e.g. Keycloak
	// public clients) accept any client_id; some require a registered
	// one.
	ClientID string
	// Issuer optionally pins the expected iss claim. Defaults to the
	// issuer field of the discovery document.
	Issuer string
	// RedirectURI is the loopback redirect URI of the browser flow,
	// e.g. "http://127.0.0.1:8080/callback". It is sent verbatim to the
	// IdP and the client listens on it; it must match exactly what is
	// registered on the IdP client. Empty means an ephemeral
	// http://127.0.0.1:PORT/callback listener.
	RedirectURI string
	// CachePath is where the negotiated token is cached.
	CachePath string
}

func (c *ClientConfig) fill() error {
	if c.WellKnown == "" {
		return errors.New("well-known is required")
	}
	if u, err := url.Parse(c.WellKnown); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("well-known must be an http(s) URL, got %q", c.WellKnown)
	}
	if c.Audience == "" {
		c.Audience = defaultAudience
	}
	if c.ClientID == "" {
		c.ClientID = defaultClientID
	}
	if c.RedirectURI != "" {
		u, err := url.Parse(c.RedirectURI)
		if err != nil || u.Scheme != "http" || u.Port() == "" || !isLoopback(u.Hostname()) {
			return fmt.Errorf("redirect-uri must be an http loopback URI with an explicit port (e.g. http://127.0.0.1:8080/callback), got %q", c.RedirectURI)
		}
		if u.Path == "" {
			c.RedirectURI = u.String() + "/callback"
		}
	}
	return nil
}

// isLoopback reports whether host is a loopback host the callback listener
// can bind.
func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

// Discovery is the subset of the OpenID discovery document the client uses.
type Discovery struct {
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

func discover(ctx context.Context, c *http.Client, wellKnown string) (*Discovery, error) {
	body, err := httpGet(ctx, c, wellKnown)
	if err != nil {
		return nil, err
	}
	var d Discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("parse discovery document: %w", err)
	}
	if d.Issuer == "" || d.TokenEndpoint == "" {
		return nil, errors.New("discovery document missing issuer or token_endpoint")
	}
	return &d, nil
}

// Client negotiates tokens with one issuer.
type Client struct {
	cfg  *ClientConfig
	d    *Discovery
	ver  *Verifier
	http *http.Client
}

// New loads the discovery document and prepares a Client. The expected
// issuer is cfg.Issuer, or the discovery document's issuer when unset.
func New(ctx context.Context, cfg *ClientConfig) (*Client, error) {
	if err := cfg.fill(); err != nil {
		return nil, err
	}
	http := &http.Client{Timeout: 15 * time.Second}
	d, err := discover(ctx, http, cfg.WellKnown)
	if err != nil {
		return nil, err
	}
	iss := cfg.Issuer
	if iss == "" {
		iss = d.Issuer
	}
	return &Client{
		cfg:  cfg,
		d:    d,
		ver:  NewVerifier(cfg.WellKnown, iss, cfg.Audience),
		http: http,
	}, nil
}

// Token returns a valid token, using the cache when it holds a fresh
// one and negotiating with the IdP otherwise. flow selects "browser" or
// "device"; empty means auto (browser when a browser can be opened,
// device otherwise). Progress output goes to out.
func (c *Client) Token(ctx context.Context, flow string, out io.Writer) (string, error) {
	if t, err := loadCache(c.cfg.CachePath); err == nil && t.Valid() {
		return t.Value, nil
	}
	var (
		tok string
		err error
	)
	switch flow {
	case "browser":
		tok, err = c.browserFlow(ctx, out)
	case "device":
		tok, err = c.deviceFlow(ctx, out)
	case "":
		if c.d.AuthorizationEndpoint != "" && canOpenBrowser() {
			tok, err = c.browserFlow(ctx, out)
		} else if c.d.DeviceAuthorizationEndpoint != "" {
			tok, err = c.deviceFlow(ctx, out)
		} else {
			return "", errors.New("issuer supports neither flow (no authorization_endpoint, no device_authorization_endpoint)")
		}
	default:
		return "", fmt.Errorf("unknown flow %q (want browser, device or empty)", flow)
	}
	if err != nil {
		return "", err
	}
	return tok, nil
}

// exchange posts form to the token endpoint and processes the response.
func (c *Client) exchange(ctx context.Context, form url.Values) (string, error) {
	st, body, err := httpPostForm(ctx, c.http, c.d.TokenEndpoint, form)
	if err != nil {
		return "", err
	}
	if st < 200 || st >= 300 {
		return "", tokenError(st, body)
	}
	return c.finish(ctx, body)
}

// finish verifies the access_token of a successful token endpoint
// response, caches it and returns it.
func (c *Client) finish(ctx context.Context, body []byte) (string, error) {
	var resp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if resp.AccessToken == "" {
		return "", errors.New("token endpoint returned no access_token")
	}
	claims, err := c.ver.Verify(ctx, resp.AccessToken)
	if err != nil {
		return "", fmt.Errorf("issued token rejected: %w", err)
	}
	if c.cfg.CachePath != "" {
		if err := cacheToken(c.cfg.CachePath, resp.AccessToken, resp.ExpiresIn, claims); err != nil {
			fmt.Fprintf(os.Stderr, "rig: cannot cache the OIDC token: %v\n", err)
		}
	}
	return resp.AccessToken, nil
}

// tokenError turns a failed token endpoint response into an error,
// preferring the RFC 6749 error fields.
func tokenError(status int, body []byte) error {
	var e struct {
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		if e.ErrorDesc != "" {
			e.Error += ": " + e.ErrorDesc
		}
		return fmt.Errorf("token endpoint: %s", e.Error)
	}
	msg := truncate(string(body), 200)
	return fmt.Errorf("token endpoint: status %d: %s", status, msg)
}

// httpPostForm posts form to rawURL and returns the response status and
// body. A non-nil err indicates a transport-level failure; HTTP error
// statuses are returned for the caller to interpret.
func httpPostForm(ctx context.Context, c *http.Client, rawURL string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	const max = 1 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(b) > max {
		return resp.StatusCode, nil, errors.New("response too large")
	}
	return resp.StatusCode, b, nil
}

// truncate trims s and bounds it to n bytes, for error messages.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
