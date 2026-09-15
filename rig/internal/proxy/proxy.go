// Package proxy runs a loopback HTTP reverse proxy that injects an
// Authorization bearer into the Maven resolver traffic of :auth :oidc
// repositories. tools.deps' create-basis (MIMA) cannot be given a bearer,
// so instead of the real repo URL, the kernel resolves through this proxy:
// it forwards every request to the repo's real URL with the repo's token
// attached.
//
// The listener binds to 127.0.0.1 on an ephemeral port; the tokens never
// leave process memory; the proxy dies with the rig command.
package proxy

import (
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

type Proxy struct {
	// targets maps a repository id to the repo's real base URL.
	targets map[string]string
	// tokens maps a repository id to the repo's bearer token.
	tokens map[string]string
	ln     net.Listener
	srv    *http.Server
}

// Start binds a loopback listener and returns the proxy serving
// /r/<id>/<rest...> -> targets[id]/<rest...> with tokens[id] attached.
func Start(targets, tokens map[string]string) (*Proxy, error) {
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

func serve(w http.ResponseWriter, r *http.Request, targets, tokens map[string]string) {
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

	out, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	if tok := tokens[id]; tok != "" {
		out.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
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
