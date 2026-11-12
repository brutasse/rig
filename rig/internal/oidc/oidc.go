// Package oidc resolves the bearer tokens for OIDC-protected Maven
// repositories (:auth :oidc).
//
// The organization's OIDC gates live in one shared config file
// (~/.config/rig/auth.yaml): each gate is an identity provider (its
// well-known discovery URL) plus the audience and client id its tokens
// must carry, and the repository URL prefixes it fronts. A repository
// marked :auth :oidc is bound to the gate that fronts its :url.
//
// Each gate's bearer is resolved on demand: the RIG_TOKEN_<GATE>
// environment variable when set, else the gate's cached token in the state
// dir, else a fresh negotiation with the issuer (browser or device flow,
// verified against the issuer's JWKS before use). A gate fronting the
// GitHub Actions issuer negotiates with the runner's id-token endpoint
// (the workflow's id-token: write permission) instead: the token is
// short-lived, so it is re-negotiated when the registry rejects it or
// before it expires.
package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultConfigRel = ".config/rig/auth.yaml"

var gateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// urlList is a YAML field that accepts either a single string or a list
// of strings.
type urlList []string

func (l *urlList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*l = []string{s}
		return nil
	case yaml.SequenceNode:
		if err := node.Decode((*[]string)(l)); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("must be a string or a list of strings")
}

// Gate is one OIDC gate: an identity provider and the audience/client its
// tokens must carry, plus the repository URL prefixes it fronts.
type Gate struct {
	// Name is the config key; the environment variable is RIG_TOKEN_<NAME>.
	Name string `yaml:"-"`
	// URLs are the repository URL prefixes the gate fronts. Empty fronts
	// every :auth :oidc repository (allowed for a single gate only).
	URLs urlList `yaml:"url"`
	// WellKnown is the issuer's OpenID discovery URL. Required.
	WellKnown string `yaml:"well-known"`
	// Audience is the audience the token must carry (default "pier").
	Audience string `yaml:"audience"`
	// ClientID is the OAuth client identifier (default "rig").
	ClientID string `yaml:"client-id"`
	// RedirectURI pins the browser flow's loopback redirect (optional).
	RedirectURI string `yaml:"redirect-uri"`
	// Scopes are the OAuth scopes the negotiated token must carry
	// (default [openid]). IdPs such as Dex gate token claims on scope:
	// groups and email appear only when their scope is requested.
	Scopes []string `yaml:"scopes"`
}

// EnvVar is the environment variable carrying this gate's token.
func (g *Gate) EnvVar() string {
	return "RIG_TOKEN_" + strings.ReplaceAll(strings.ToUpper(g.Name), "-", "_")
}

// Identity is the gate's effective configuration: the cache key. Renaming
// a gate does not invalidate its cached token.
func (g *Gate) Identity() string {
	return g.WellKnown + "\x00" + g.Audience + "\x00" + g.ClientID + "\x00" + strings.Join(g.Scopes, " ")
}

// Config is the gate configuration of one machine (the shared org file).
type Config struct {
	Gates map[string]*Gate `yaml:"gates"`
	// Path is where the config was read from, for error messages ("(not
	// found)" when the file does not exist).
	Path string
}

// DefaultPath is the config location: ~/.config/rig/auth.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, defaultConfigRel), nil
}

// Load reads the default gate config. A missing file is an empty config:
// repositories marked :auth :oidc then fail with the no-gate error naming
// the default path.
func Load() (*Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return load(path)
}

func load(path string) (*Config, error) {
	cfg := &Config{Gates: map[string]*Gate{}, Path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Path = path + " (not found)"
			return cfg, nil
		}
		return nil, err
	}
	var file struct {
		Gates map[string]*Gate `yaml:"gates"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for name, g := range file.Gates {
		if g == nil {
			g = &Gate{}
		}
		g.Name = name
		if err := g.fill(); err != nil {
			return nil, fmt.Errorf("%s: gate %q: %v", path, name, err)
		}
		cfg.Gates[name] = g
	}
	open := 0
	for _, g := range cfg.Gates {
		if len(g.URLs) == 0 {
			open++
		}
	}
	if open > 1 {
		return nil, fmt.Errorf("%s: at most one gate may omit url; %d do", path, open)
	}
	return cfg, nil
}

func (g *Gate) fill() error {
	if !gateNameRe.MatchString(g.Name) {
		return fmt.Errorf("name %q must match %s", g.Name, gateNameRe)
	}
	if g.WellKnown == "" {
		return errors.New("well-known is required")
	}
	u, err := url.Parse(g.WellKnown)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("well-known must be an http(s) URL, got %q", g.WellKnown)
	}
	if g.Audience == "" {
		g.Audience = defaultAudience
	}
	if g.ClientID == "" {
		g.ClientID = defaultClientID
	}
	if len(g.Scopes) == 0 {
		g.Scopes = []string{defaultScope}
	}
	for _, s := range g.Scopes {
		if s == "" || strings.ContainsAny(s, " \t") {
			return fmt.Errorf("scope %q must be a non-empty token without spaces", s)
		}
	}
	if g.RedirectURI != "" {
		u, err := url.Parse(g.RedirectURI)
		if err != nil || u.Scheme != "http" || u.Port() == "" || !isLoopback(u.Hostname()) {
			return fmt.Errorf("redirect-uri must be an http loopback URI with an explicit port (e.g. http://127.0.0.1:8080/callback), got %q", g.RedirectURI)
		}
		if u.Path == "" {
			g.RedirectURI = u.String() + "/callback"
		}
	}
	for _, raw := range g.URLs {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url must be an http(s) URL prefix, got %q", raw)
		}
	}
	return nil
}

// Gate returns the gate that fronts repoURL: the single gate whose url
// prefixes match, else the single url-less gate (the default), else an
// error naming what is wrong.
func (c *Config) Gate(repoURL string) (*Gate, error) {
	var (
		matches []*Gate
		open    []*Gate
	)
	for _, g := range c.Gates {
		if len(g.URLs) == 0 {
			open = append(open, g)
			continue
		}
		for _, u := range g.URLs {
			if strings.HasPrefix(repoURL, u) {
				matches = append(matches, g)
				break
			}
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		return nil, fmt.Errorf("repository %s is fronted by several gates (%s); tighten the gate urls", repoURL, gateNames(matches))
	case len(open) == 1:
		return open[0], nil
	case len(open) > 1:
		return nil, fmt.Errorf("several gates in %s omit url (%s); add url prefixes", c.Path, gateNames(open))
	default:
		return nil, fmt.Errorf("no gate in %s fronts %s; add a gate with a matching url prefix", c.Path, repoURL)
	}
}

func gateNames(gs []*Gate) string {
	names := make([]string, len(gs))
	for i, g := range gs {
		names[i] = g.Name
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Names returns the configured gate names, sorted.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Gates))
	for n := range c.Gates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolver resolves the bearer of each gate. A gate's bearer is resolved
// on demand: the RIG_TOKEN_<GATE> environment variable when set, else the
// gate's cached token, else a fresh negotiation (progress output to
// stderr). A gate fronting the GitHub Actions issuer negotiates with the
// runner's id-token endpoint — the token is short-lived, so the held token
// is re-negotiated when the registry rejects it (Invalidate) or when it is
// within expiryMargin of its expiry. A token from the environment variable
// is used as-is; the other flows negotiate at most once per command.
type Resolver struct {
	cfg *Config
	dir string // state dir; per-gate token caches under <dir>/oidc

	mu  sync.Mutex
	sts map[string]*gateState
}

// gateState is the resolved bearer of one gate for the lifetime of the
// command.
type gateState struct {
	mu      sync.Mutex
	token   string
	expires time.Time // zero when unknown
	static  bool      // env-var token: never re-negotiated
	invalid bool      // the registry rejected the held token (401/403)
	have    bool
	err     error     // the first negotiation failure, stable for the command
	next    time.Time // earliest time for the next re-negotiation (throttle)
}

const expiryMargin = 30 * time.Second // a token is stale this long before its expiry

var jitThrottle = 5 * time.Second // minimum interval between re-negotiations of a rejected token

// NewResolver builds a Resolver over the gate config, caching per-gate
// tokens under dir (the state dir).
func NewResolver(cfg *Config, dir string) *Resolver {
	return &Resolver{cfg: cfg, dir: dir, sts: map[string]*gateState{}}
}

// Config is the resolver's gate config.
func (r *Resolver) Config() *Config { return r.cfg }

// CacheFile is the gate's token cache file under the state dir.
func (r *Resolver) CacheFile(g *Gate) string {
	sum := sha256.Sum256([]byte(g.Identity()))
	return filepath.Join(r.dir, "oidc", hex.EncodeToString(sum[:])[:16]+".json")
}

func (r *Resolver) state(g *Gate) *gateState {
	id := g.Identity()
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sts[id]
	if !ok {
		st = &gateState{}
		r.sts[id] = st
	}
	return st
}

// Token returns the bearer for gate. The first call resolves it: the gate's
// environment variable when set, else the per-gate cache, else a fresh
// negotiation — the GitHub Actions flow for a gate fronting the GitHub
// Actions issuer, else the issuer's browser or device-code flow (verified
// against the issuer's JWKS before use). Later calls return the held
// token; the GitHub Actions token is re-negotiated when the registry
// rejected it or when it is within expiryMargin of its expiry. flow selects
// "browser" or "device" for the issuer's flows; empty means auto.
func (r *Resolver) Token(ctx context.Context, g *Gate, flow string) (string, error) {
	st := r.state(g)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.have {
		if st.err != nil {
			return "", st.err
		}
		if err := r.resolve(ctx, st, g, flow); err != nil {
			st.err = err
			return "", err
		}
	} else if isGHA(g) && !st.static {
		expiring := !st.expires.IsZero() && !time.Now().Add(expiryMargin).Before(st.expires)
		if (expiring || st.invalid) && (expiring || time.Now().After(st.next)) {
			// A failed re-negotiation keeps the held token: it may
			// still be accepted, and the registry is the judge.
			_ = r.reneg(ctx, st, g, flow)
		}
	}
	return st.token, nil
}

// Invalidate marks the held bearer of gate as rejected by the registry
// (401/403): the next Token call re-negotiates it for the gates that can
// (the GitHub Actions ones). It is a no-op for the others.
func (r *Resolver) Invalidate(g *Gate) {
	st := r.state(g)
	st.mu.Lock()
	st.invalid = true
	st.mu.Unlock()
}

// resolve is the first-resolution chain: the env var, the cache, the
// negotiation. It leaves the state resolved (have) or with a stable error.
func (r *Resolver) resolve(ctx context.Context, st *gateState, g *Gate, flow string) error {
	if t := strings.TrimSpace(os.Getenv(g.EnvVar())); t != "" {
		st.token = t
		st.static = true
		st.have = true
		return nil
	}
	if t, err := loadCache(r.CacheFile(g)); err == nil && t.Valid() {
		st.token = t.Value
		st.expires = t.ExpiresAt
		st.have = true
		return nil
	}
	if isGHA(g) && flow != "" {
		return fmt.Errorf("flow %q does not apply: the gate negotiates with GitHub Actions", flow)
	}
	return r.reneg(ctx, st, g, flow)
}

// reneg negotiates a fresh bearer and records it in the state: the GitHub
// Actions flow for a gate fronting the GitHub Actions issuer, else the
// issuer's browser or device-code flow (flow selects; empty means auto).
func (r *Resolver) reneg(ctx context.Context, st *gateState, g *Gate, flow string) error {
	if isGHA(g) {
		tok, claims, err := ghaToken(ctx, g)
		if err != nil {
			return err
		}
		st.token = tok
		st.expires = claimExpiry(claims)
		st.have = true
		st.invalid = false
		st.next = time.Now().Add(jitThrottle)
		if err := cacheToken(r.CacheFile(g), tok, 0, claims); err != nil {
			fmt.Fprintf(os.Stderr, "rig: cannot cache the OIDC token: %v\n", err)
		}
		return nil
	}
	c, err := New(ctx, &ClientConfig{
		WellKnown:   g.WellKnown,
		Audience:    g.Audience,
		ClientID:    g.ClientID,
		RedirectURI: g.RedirectURI,
		Scopes:      g.Scopes,
		CachePath:   r.CacheFile(g),
	})
	if err != nil {
		return err
	}
	tok, err := c.Token(ctx, flow, os.Stderr)
	if err != nil {
		return err
	}
	st.token = tok
	st.have = true
	st.invalid = false
	return nil
}
