// Package proxy runs a loopback HTTP reverse proxy that injects an
// Authorization bearer into the Maven resolver traffic of :auth :oidc
// repositories. tools.deps' create-basis (MIMA) cannot be given a bearer,
// so instead of the real repo URL, the kernel resolves through this proxy:
// it forwards every request to the repo's real URL with the repo's token
// attached.
//
// The listener binds to 127.0.0.1 on an ephemeral port; the tokens never
// leave process memory; the proxy dies with the rig command. When the
// upstream rejects the bearer (401/403), the proxy asks its supplier for a
// fresh bearer and retries the request once: a short-lived token (e.g. a
// GitHub Actions id token) can expire mid-run.
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// hop-by-hop headers must not be forwarded.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// TokenSource supplies the bearer of one repository.
type TokenSource interface {
	// Token returns the current bearer.
	Token(ctx context.Context) (string, error)
	// Refresh returns a fresh bearer; called when the upstream rejected
	// the current one (401/403).
	Refresh(ctx context.Context) (string, error)
}

type Proxy struct {
	// targets maps a repository id to the repo's real base URL.
	targets map[string]string
	// tokens maps a repository id to the supplier of its bearer.
	tokens map[string]TokenSource
	ln     net.Listener
	srv    *http.Server
}

// Start binds a loopback listener and returns the proxy serving
// /r/<id>/<rest...> -> targets[id]/<rest...> with tokens[id]'s bearer
// attached.
func Start(targets map[string]string, tokens map[string]TokenSource) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, targets, tokens)
	})
	p := &Proxy{targets: targets, tokens: tokens, ln: ln, srv: &http.Server{Handler: mux}}
	go p.srv.Serve(ln)
	return p, nil
}

// Base returns the proxy URL to use as the :url of the repository with id
// (the repo id stays in the manifest, so lock attribution is unchanged).
func (p *Proxy) Base(id string) string {
	return "http://" + p.ln.Addr().String() + "/r/" + id
}

// Addr is the loopback address:port the proxy listens on.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Close stops the listener.
func (p *Proxy) Close() error { return p.srv.Close() }

func serve(w http.ResponseWriter, r *http.Request, targets map[string]string, tokens map[string]TokenSource) {
	// Path: /r/<id>/<rest...>
	rest := strings.TrimPrefix(r.URL.Path, "/r/")
	id, rest, _ := strings.Cut(rest, "/")
	target, ok := targets[id]
	if !ok {
		http.Error(w, "unknown repository", http.StatusNotFound)
		return
	}
	target = strings.TrimSuffix(target, "/")
	url := target + "/" + rest
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	var (
		tok string
		err error
		src TokenSource
	)
	if src, ok = tokens[id]; ok {
		if tok, err = src.Token(r.Context()); err != nil {
			http.Error(w, "bearer: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	resp, err := doRequest(r, url, tok)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	// The bearer may have been rejected (401/403): a short-lived token
	// can expire mid-run. Ask the supplier for a fresh bearer and retry
	// once — only when the request carries no body (Maven traffic is
	// GET/HEAD). A failed refresh, or an unchanged bearer, reports the
	// auth failure (the original response body is consumed).
	if src != nil && r.ContentLength == 0 &&
		(resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		resp.Body.Close()
		fresh, rerr := src.Refresh(r.Context())
		if rerr != nil || fresh == tok {
			http.Error(w, "upstream: "+resp.Status, resp.StatusCode)
			return
		}
		resp2, err2 := doRequest(r, url, fresh)
		if err2 != nil {
			http.Error(w, "upstream: "+err2.Error(), http.StatusBadGateway)
			return
		}
		resp = resp2
	}
	forward(w, r, resp)
}

// doRequest performs one upstream request: the method, the headers (minus
// hop-by-hop) and the body of r, the bearer attached.
func doRequest(r *http.Request, url, tok string) (*http.Response, error) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	if tok != "" {
		out.Header.Set("Authorization", "Bearer "+tok)
	}
	return http.DefaultClient.Do(out)
}

// forward streams the upstream response back: the headers (minus
// hop-by-hop), the status, and the body (HEAD carries none).
func forward(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		io.Copy(w, resp.Body)
	}
}
