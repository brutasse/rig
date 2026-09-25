package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/brutasse/rig/internal/ednlit"
	"github.com/brutasse/rig/internal/oidc"
	"github.com/brutasse/rig/internal/proxy"
	"github.com/brutasse/rig/internal/workspace"
)

// oidcAuth is the lazily resolved OIDC auth state of one command: which of
// the workspace's repositories sit behind an OIDC gate (:auth :oidc), the
// gate each one fronts, the bearer resolved per repository (the gate's
// RIG_TOKEN_<GATE> environment variable, else the gate's cached token,
// else a fresh negotiation), and the local auth proxy that carries the
// bearers for resolver traffic.
type oidcAuth struct {
	once  sync.Once
	urls  map[string]string // repo id -> real repo URL
	toks  map[string]string // repo id -> bearer
	gates map[string]string // repo id -> gate name (error messages)
	err   error

	proxyOnce sync.Once
	proxy     *proxy.Proxy
	proxyErr  error
}

func (o *opts) auth() *oidcAuth {
	if o.oidcA == nil {
		o.oidcA = &oidcAuth{}
	}
	return o.oidcA
}

// resolve scans the workspace manifests for :auth :oidc repositories and,
// when any exist (and we are online), binds each to its gate and obtains
// the bearer. It runs once per command; the error (when no gate fronts a
// repo or no token can be obtained) is returned every time so callers can
// fail fast.
func (o *opts) resolve(ctx context.Context, root *workspace.Root) error {
	a := o.auth()
	a.once.Do(func() {
		urls, err := oidcRepos(root)
		if err != nil {
			a.err = err
			return
		}
		if o.offline || len(urls) == 0 {
			return
		}
		a.urls = urls
		store, err := o.store()
		if err != nil {
			a.err = err
			return
		}
		cfg, err := oidc.Load()
		if err != nil {
			a.err = err
			return
		}
		r := oidc.NewResolver(cfg, store.Root)
		a.toks = make(map[string]string, len(urls))
		a.gates = make(map[string]string, len(urls))
		ids := make([]string, 0, len(urls))
		for id := range urls {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			g, err := cfg.Gate(urls[id])
			if err != nil {
				a.err = fmt.Errorf("repo %q (%s) is marked :auth :oidc, but %v", id, urls[id], err)
				return
			}
			tok, err := r.Token(ctx, g, "")
			if err != nil {
				a.err = fmt.Errorf("no token for gate %q (repo %q): set %s or run 'rig auth get %s' (%v)", g.Name, id, g.EnvVar(), g.Name, err)
				return
			}
			a.toks[id] = tok
			a.gates[id] = g.Name
		}
	})
	return a.err
}

// kernelEnv returns the extra environment for the kernel subprocess:
// RIG_REPO_TOKENS, an EDN {repo-id bearer} map for the :auth :oidc
// repositories, so the resolver can authenticate its repository probes.
// Nil when the workspace has no :auth :oidc repos.
func (o *opts) kernelEnv(ctx context.Context, root *workspace.Root) ([]string, error) {
	if err := o.resolve(ctx, root); err != nil {
		return nil, err
	}
	if len(o.oidcA.toks) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(o.oidcA.toks))
	for id, tok := range o.oidcA.toks {
		m[id] = tok
	}
	return []string{"RIG_REPO_TOKENS=" + ednStringMap(m)}, nil
}

// kernelProxy returns the extra environment for kernel commands that run
// resolution: RIG_PROXY_REPOS, an EDN {repo-id proxy-base-url} map pointing
// each :auth :oidc repo at the local auth proxy, which forwards the
// resolver's Maven traffic to the repo's real URL with its bearer attached
// (tools.deps cannot be given a bearer of its own). The proxy is started
// once per command; callers must defer closeProxy. Nil when the workspace
// has no marked repos or no token (--offline).
func (o *opts) kernelProxy(ctx context.Context, root *workspace.Root) ([]string, error) {
	if err := o.resolve(ctx, root); err != nil {
		return nil, err
	}
	if len(o.oidcA.toks) == 0 {
		return nil, nil
	}
	var env []string
	o.oidcA.proxyOnce.Do(func() {
		p, err := proxy.Start(o.oidcA.urls, o.oidcA.toks)
		o.oidcA.proxyErr = err
		if err != nil {
			return
		}
		o.oidcA.proxy = p
		m := make(map[string]string, len(o.oidcA.urls))
		for id := range o.oidcA.urls {
			m[id] = p.Base(id)
		}
		bts := ednStringMap(m)
		env = append(env, "RIG_PROXY_REPOS="+bts)
	})
	return env, o.oidcA.proxyErr
}

// ednStringMap encodes a string->string map as an EDN literal
// ({"id" "value", ...}), for RIG_PROXY_REPOS and RIG_REPO_TOKENS: the
// kernel reads it with clojure.edn/read-string (JSON's ":" separator is not
// EDN).
func ednStringMap(m map[string]string) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ednString(id))
		b.WriteByte(' ')
		b.WriteString(ednString(m[id]))
	}
	b.WriteByte('}')
	return b.String()
}

// ednString quotes s as an EDN string.
func ednString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// closeProxy stops the auth proxy started by kernelProxy. Idempotent.
func (o *opts) closeProxy() {
	if o.oidcA == nil {
		return
	}
	if p := o.oidcA.proxy; p != nil {
		p.Close()
		o.oidcA.proxy = nil
	}
}

// bearerFor returns the fetch client's BearerFor hook: the bearer token for
// repositories marked :auth :oidc (per the gate fronting each repo), "" for
// any other repository.
func (o *opts) bearerFor(ctx context.Context, root *workspace.Root) func(repo string) (string, bool) {
	if root == nil {
		return func(repo string) (string, bool) { return "", false }
	}
	return func(repo string) (string, bool) {
		if err := o.resolve(ctx, root); err != nil {
			return "", false
		}
		tok, ok := o.oidcA.toks[repo]
		return tok, ok
	}
}

// oidcRepos returns, for every :mvn/repos entry marked :auth :oidc in the
// workspace's manifests (root + every module manifest, when present), the
// repo id mapped to its :url.
func oidcRepos(root *workspace.Root) (map[string]string, error) {
	urls := map[string]string{}
	for _, m := range modulePaths(root) {
		data, err := os.ReadFile(filepath.Join(root.Dir, m, "deps.edn"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		name := m + "/deps.edn"
		if m == "." {
			name = "deps.edn"
		}
		v, err := ednlit.ParseNamed(name, string(data))
		if err != nil {
			return nil, err
		}
		man, ok := v.(ednlit.Map)
		if !ok {
			continue
		}
		rv, ok := ednlitGet(man, ednlit.Keyword{NS: "mvn", Name: "repos"})
		if !ok {
			continue
		}
		repos, ok := rv.(ednlit.Map)
		if !ok {
			continue
		}
		for _, p := range repos {
			spec, ok := p.V.(ednlit.Map)
			if !ok {
				continue
			}
			av, ok := ednlitGet(spec, ednlit.Keyword{Name: "auth"})
			if !ok {
				continue
			}
			auth, ok := av.(ednlit.Keyword)
			if !ok || auth.NS != "" || auth.Name != "oidc" {
				continue
			}
			iv, ok := ednlitGet(spec, ednlit.Keyword{Name: "url"})
			id, iok := p.K.(string)
			url, uok := iv.(string)
			if iok && uok {
				urls[id] = url
			}
		}
	}
	return urls, nil
}

// modulePaths returns the manifest paths to scan: the root manifest's
// :rig/modules (the authoritative module list) plus the root itself.
func modulePaths(root *workspace.Root) []string {
	data, err := os.ReadFile(filepath.Join(root.Dir, "deps.edn"))
	if err != nil {
		return []string{"."}
	}
	v, err := ednlit.ParseNamed("deps.edn", string(data))
	if err != nil {
		return []string{"."}
	}
	man, ok := v.(ednlit.Map)
	if !ok {
		return []string{"."}
	}
	mv, ok := ednlitGet(man, ednlit.Keyword{NS: "rig", Name: "modules"})
	if !ok {
		return []string{"."}
	}
	mods, ok := mv.([]any)
	if !ok {
		return []string{"."}
	}
	seen := map[string]bool{".": true}
	var out []string
	for _, m := range mods {
		if s, ok := m.(string); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return append(out, ".")
}

// ednlitGet looks up keyword k in m.
func ednlitGet(m ednlit.Map, k ednlit.Keyword) (any, bool) {
	for _, p := range m {
		if kw, ok := p.K.(ednlit.Keyword); ok && kw == k {
			return p.V, true
		}
	}
	return nil, false
}
