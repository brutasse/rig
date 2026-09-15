package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// browserTimeout bounds how long the browser flow waits for the
// authorization callback.
const browserTimeout = 5 * time.Minute

// tokenReceivedHTML is the self-contained page shown in the browser once
// the authorization callback lands. No external assets, light/dark aware.
const tokenReceivedHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Token received</title>
<style>
  :root {
    --bg: #f5f6f8;
    --card: #ffffff;
    --border: rgba(0, 0, 0, 0.08);
    --shadow: 0 1px 2px rgba(0, 0, 0, 0.04), 0 8px 24px rgba(0, 0, 0, 0.06);
    --text: #1a1d21;
    --muted: #6b7280;
    --accent: #0f7d64;
    --accent-bg: #e7f4ef;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #111315;
      --card: #1a1d21;
      --border: rgba(255, 255, 255, 0.08);
      --shadow: 0 1px 2px rgba(0, 0, 0, 0.5), 0 8px 24px rgba(0, 0, 0, 0.4);
      --text: #e8eaed;
      --muted: #9aa0a6;
      --accent: #4cc38f;
      --accent-bg: rgba(76, 195, 143, 0.12);
    }
  }
  body {
    margin: 0;
    min-height: 100vh;
    display: grid;
    place-items: center;
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    max-width: 22rem;
    margin: 1.5rem;
    padding: 3rem 2.5rem 2.75rem;
    text-align: center;
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 12px;
    box-shadow: var(--shadow);
    animation: rise 240ms ease-out;
  }
  .badge {
    display: inline-flex;
    width: 3rem;
    height: 3rem;
    align-items: center;
    justify-content: center;
    border-radius: 50%;
    background: var(--accent-bg);
    color: var(--accent);
    margin-bottom: 1.25rem;
  }
  h1 {
    margin: 0 0 0.5rem;
    font-size: 1.25rem;
    font-weight: 600;
    letter-spacing: -0.01em;
  }
  p {
    margin: 0;
    color: var(--muted);
    font-size: 0.9375rem;
    line-height: 1.5;
  }
  @keyframes rise {
    from { opacity: 0; transform: translateY(4px); }
    to { opacity: 1; transform: none; }
  }
</style>
</head>
<body>
  <main class="card">
    <div class="badge" aria-hidden="true">
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg>
    </div>
    <h1>Token received</h1>
    <p>You may close this window.</p>
  </main>
</body>
</html>`

// browserFlow runs the authorization-code flow with PKCE (RFC 7636): it
// opens the issuer's authorization endpoint in a browser, receives the
// code on a local listener, and exchanges it for a token.
func (c *Client) browserFlow(ctx context.Context, out io.Writer) (string, error) {
	if c.d.AuthorizationEndpoint == "" {
		return "", errors.New("issuer does not advertise an authorization_endpoint (browser flow unavailable)")
	}
	verifier, err := randomToken()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, err := randomToken()
	if err != nil {
		return "", err
	}

	// Bind the callback listener before opening the browser, so the
	// redirect never races us.
	ln, cb, err := c.callbackListener()
	if err != nil {
		return "", err
	}
	defer ln.Close()

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(cb.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			resCh <- result{err: errors.New("callback state mismatch")}
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			resCh <- result{err: errors.New("callback without code")}
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		resCh <- result{code: code}
		io.WriteString(w, tokenReceivedHTML)
	})
	srv := &http.Server{Handler: mux}
	defer srv.Close()
	go srv.Serve(ln)

	authURL := c.d.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {cb.String()},
		"scope":                 {"openid"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	fmt.Fprintf(out, "Open this URL in a browser to authenticate:\n\n    %s\n\nWaiting for the callback...\n", authURL)
	openBrowser(authURL)

	select {
	case res := <-resCh:
		if res.err != nil {
			return "", res.err
		}
		return c.exchange(ctx, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {res.code},
			"redirect_uri":  {cb.String()},
			"client_id":     {c.cfg.ClientID},
			"code_verifier": {verifier},
		})
	case <-time.After(browserTimeout):
		return "", errors.New("timed out waiting for browser authorization")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// callbackListener binds the listener that receives the authorization
// callback. It returns the listener and the redirect URI to present to
// the IdP: the configured loopback URI verbatim, or an ephemeral
// http://127.0.0.1:PORT/callback listener when none is configured.
// The caller serves the callback on uri.Path.
func (c *Client) callbackListener() (net.Listener, *url.URL, error) {
	if c.cfg.RedirectURI != "" {
		u, err := url.Parse(c.cfg.RedirectURI)
		if err != nil {
			return nil, nil, err
		}
		host := u.Hostname()
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, u.Port()))
		if err != nil {
			return nil, nil, fmt.Errorf("callback listener %s: %w", c.cfg.RedirectURI, err)
		}
		return ln, u, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("callback listener: %w", err)
	}
	uri, _ := url.Parse("http://" + ln.Addr().String() + "/callback")
	return ln, uri, nil
}

// randomToken returns a 43-character base64url random string, the
// minimum PKCE verifier length.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowser opens rawURL in the system browser where possible.
// Failures are ignored: the URL is always printed for manual use.
var openBrowser = func(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		if xdg, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command(xdg, rawURL)
		}
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}

// canOpenBrowser reports whether a browser can be opened on this system.
// It is a variable so tests can simulate headless environments.
var canOpenBrowser = func() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		_, err := exec.LookPath("xdg-open")
		return err == nil
	}
}
