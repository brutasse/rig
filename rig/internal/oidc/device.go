package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// deviceFlow runs the OAuth 2.0 device authorization grant (RFC 8628):
// it obtains a device code from the issuer, prints the user code, and
// polls the token endpoint until the user authorizes or the code
// expires.
func (c *Client) deviceFlow(ctx context.Context, out io.Writer) (string, error) {
	if c.d.DeviceAuthorizationEndpoint == "" {
		return "", errors.New("issuer does not advertise a device_authorization_endpoint (device flow unavailable)")
	}
	form := url.Values{"scope": {"openid"}}
	if c.cfg.ClientID != "" {
		form.Set("client_id", c.cfg.ClientID)
	}
	st, body, err := httpPostForm(ctx, c.http, c.d.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return "", fmt.Errorf("device authorization: %w", err)
	}
	if st < 200 || st >= 300 {
		return "", fmt.Errorf("device authorization: status %d: %s", st, truncate(string(body), 200))
	}
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &dev); err != nil {
		return "", fmt.Errorf("parse device authorization response: %w", err)
	}
	if dev.DeviceCode == "" || dev.UserCode == "" {
		return "", errors.New("device authorization response missing device_code or user_code")
	}
	uri := dev.VerificationURIComplete
	if uri == "" {
		uri = dev.VerificationURI
	}
	fmt.Fprintf(out, "To authenticate, open this URL in a browser:\n\n    %s\n\nand enter the code: %s\n\nWaiting for authorization...\n", uri, dev.UserCode)
	openBrowser(uri)

	interval := time.Duration(dev.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	deadline := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if dev.ExpiresIn <= 0 {
		deadline = time.Now().Add(5 * time.Minute)
	}

	for {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", errors.New("device code expired before authorization")
		}
		poll := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dev.DeviceCode},
		}
		if c.cfg.ClientID != "" {
			poll.Set("client_id", c.cfg.ClientID)
		}
		st, body, err := httpPostForm(ctx, c.http, c.d.TokenEndpoint, poll)
		if err != nil {
			return "", fmt.Errorf("token polling: %w", err)
		}
		if st >= 200 && st < 300 {
			return c.finish(ctx, body)
		}
		var pe struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &pe)
		switch pe.Error {
		case "authorization_pending":
			// keep polling at the issuer's interval
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return "", errors.New("device code expired before authorization")
		default:
			return "", tokenError(st, body)
		}
	}
}
