package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GhaWellKnown is the OpenID discovery URL of the GitHub Actions
// issuer. A gate whose well-known is this negotiates the GitHub Actions
// flow: the runner's short-lived id-token request credentials, set by the
// workflow's id-token: write permission. Tests point it at a fake issuer.
var GhaWellKnown = "https://token.actions.githubusercontent.com/.well-known/openid-configuration"

const (
	ghaRequestURLEnv   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	ghaRequestTokenEnv = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// isGHA reports whether the gate fronts the GitHub Actions issuer.
func isGHA(g *Gate) bool {
	return strings.TrimSuffix(g.WellKnown, "/") == GhaWellKnown
}

// ghaToken exchanges the runner's id-token request credentials for an id
// token carrying the gate's audience. The token is verified against the
// issuer's JWKS before use (signature, iss, aud, exp). It returns the
// token and its verified claims.
func ghaToken(ctx context.Context, g *Gate) (string, map[string]any, error) {
	reqURL := strings.TrimSpace(os.Getenv(ghaRequestURLEnv))
	reqTok := strings.TrimSpace(os.Getenv(ghaRequestTokenEnv))
	if reqURL == "" || reqTok == "" {
		return "", nil, fmt.Errorf("no GitHub Actions id-token request credentials (%s, %s): the workflow needs the id-token: write permission", ghaRequestURLEnv, ghaRequestTokenEnv)
	}
	u, err := url.Parse(reqURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", nil, fmt.Errorf("%s is not a URL: %q", ghaRequestURLEnv, reqURL)
	}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	// The expected issuer, from the discovery document (the GitHub
	// Actions document carries no token endpoint: the exchange endpoint
	// is the env var).
	body, err := httpGet(ctx, httpClient, g.WellKnown)
	if err != nil {
		return "", nil, fmt.Errorf("fetch the discovery document: %w", err)
	}
	var disc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(body, &disc); err != nil {
		return "", nil, fmt.Errorf("parse the discovery document: %w", err)
	}
	if disc.Issuer == "" {
		return "", nil, fmt.Errorf("discovery document missing issuer")
	}
	q := u.Query()
	q.Set("audience", g.Audience)
	u.RawQuery = q.Encode()
	bts, err := json.Marshal(map[string]string{"audience": g.Audience})
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(bts)))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+reqTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("request the id token: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, fmt.Errorf("request the id token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		msg := truncate(string(b), 200)
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return "", nil, fmt.Errorf("id token request: status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Value == "" {
		return "", nil, fmt.Errorf("id token response carries no value: %s", truncate(string(b), 200))
	}
	ver := NewVerifier(g.WellKnown, disc.Issuer, g.Audience)
	claims, err := ver.Verify(ctx, out.Value)
	if err != nil {
		return "", nil, fmt.Errorf("issued token rejected: %w", err)
	}
	return out.Value, claims, nil
}

// claimExpiry returns the exp claim of a verified token, zero when the
// claim is absent.
func claimExpiry(claims map[string]any) time.Time {
	switch v := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(v), 0)
	case int64:
		return time.Unix(v, 0)
	}
	return time.Time{}
}
