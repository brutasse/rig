package oidc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// cachedToken is one gate's token cache on disk.
type cachedToken struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
	Sub       string    `json:"sub,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
}

// Valid reports whether the cached token can still be used, with a 30s
// safety margin.
func (t *cachedToken) Valid() bool {
	return t != nil && t.Value != "" && time.Now().Add(30*time.Second).Before(t.ExpiresAt)
}

func loadCache(path string) (*cachedToken, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t cachedToken
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.Value == "" {
		return nil, errors.New("empty cache")
	}
	return &t, nil
}

// cacheToken stores the issued token. The expiry comes from the token's
// exp claim, the token response's expires_in, or a fallback bound when
// neither is known.
func cacheToken(path, token string, expiresIn int64, claims map[string]any) error {
	var exp time.Time
	if v, ok := claims["exp"].(float64); ok {
		exp = time.Unix(int64(v), 0)
	} else if v, ok := claims["exp"].(int64); ok {
		exp = time.Unix(v, 0)
	}
	if exp.IsZero() && expiresIn > 0 {
		exp = time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	if exp.IsZero() {
		exp = time.Now().Add(noExpiryFallback)
	}
	sub, _ := claims["sub"].(string)
	t := &cachedToken{Value: token, ExpiresAt: exp, Sub: sub, IssuedAt: time.Now()}
	return writeCache(path, t)
}

func writeCache(path string, t *cachedToken) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
