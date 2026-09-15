// Verifier verifies OIDC tokens issued by one gate's identity provider.
//
// Verification is stateless: each token is checked against the issuer's
// JWKS (fetched from its well-known document and cached). Signature,
// issuer, audience, expiry and standard time claims are validated.

package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrUnknownIssuer is returned when the token issuer is not the
	// gate's issuer.
	ErrUnknownIssuer = errors.New("unknown token issuer")
	// ErrMalformed is returned when the token is not a decodable JWT.
	ErrMalformed = errors.New("malformed token")
	// ErrJWKS is returned when the issuer's keys cannot be fetched; the
	// token may be fine, the fetch failed.
	ErrJWKS = errors.New("JWKS unavailable")
	// ErrBadSignature is returned when the signature check fails: wrong
	// key, unknown kid, or a disallowed signing method.
	ErrBadSignature = errors.New("bad signature")
	// ErrWrongAudience is returned when the token's aud claim does not
	// carry the gate's audience.
	ErrWrongAudience = errors.New("audience mismatch")
	// ErrExpired is returned when the token's exp claim is in the past.
	ErrExpired = errors.New("token expired")
	// ErrNotValidYet is returned when the token's nbf or iat claim is in
	// the future.
	ErrNotValidYet = errors.New("token not valid yet")
	// ErrBadToken is returned for any other verification failure.
	ErrBadToken = errors.New("invalid token")
)

// Verifier verifies tokens for one issuer.
type Verifier struct {
	wellKnown   string // the issuer's discovery URL
	expectedISS string // the expected iss claim
	audience    string // the audience the token must carry
	http        *http.Client
	ttl         time.Duration
	miss        time.Duration // minimum interval between kid-miss JWKS refetches

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey // kid -> key
	fetchedAt time.Time
	nextMiss  time.Time // minimum interval between kid-miss refetches
}

// NewVerifier builds a Verifier for the issuer at wellKnown. expectedISS
// pins the iss claim (the discovery document's issuer when empty); audience
// is the single audience the token must carry.
func NewVerifier(wellKnown, expectedISS, audience string) *Verifier {
	return &Verifier{
		wellKnown:   wellKnown,
		expectedISS: expectedISS,
		audience:    audience,
		http:        &http.Client{Timeout: 10 * time.Second},
		ttl:         time.Hour,
		miss:        30 * time.Second,
	}
}

// Verify validates raw (the JWT string) and returns its claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (map[string]any, error) {
	iss, kid, err := peek(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	want := v.expectedISS
	if want == "" {
		want = iss
	}
	if iss != want {
		return nil, fmt.Errorf("%w: token iss %q, want %q", ErrUnknownIssuer, iss, want)
	}
	keys, err := v.jwks(ctx, kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJWKS, err)
	}
	if !hasKey(keys, kid) {
		return nil, fmt.Errorf("%w: unknown key id %q (JWKS may be stale)", ErrBadSignature, kid)
	}
	claims := jwt.MapClaims{}
	token, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(want),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(30*time.Second),
	).ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, ok := keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown key id %q", kid)
		}
		return k, nil
	})
	if err != nil || !token.Valid {
		return nil, classify(err, v.audience, claims)
	}
	return map[string]any(claims), nil
}

// classify maps a jwt parse failure to a sentinel error, naming the
// failing claim values.
func classify(err error, wantAud string, claims jwt.MapClaims) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return fmt.Errorf("%w: exp %s", ErrExpired, claimTime(claims, "exp"))
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return fmt.Errorf("%w: token aud %v, want %q", ErrWrongAudience, claims["aud"], wantAud)
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing) && claims["aud"] == nil:
		return fmt.Errorf("%w: missing aud claim, want %q", ErrWrongAudience, wantAud)
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return fmt.Errorf("%w: nbf %s", ErrNotValidYet, claimTime(claims, "nbf"))
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return fmt.Errorf("%w: iat %s", ErrNotValidYet, claimTime(claims, "iat"))
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	case errors.Is(err, jwt.ErrTokenMalformed):
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	default:
		return fmt.Errorf("%w: %v", ErrBadToken, err)
	}
}

// claimTime renders a time claim in RFC 3339, or a placeholder when the
// claim is absent or not a number.
func claimTime(claims jwt.MapClaims, key string) string {
	if f, ok := claims[key].(float64); ok {
		return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
	}
	return "<missing>"
}

// peek decodes the header and payload of a compact JWT without
// verifying it, to read the iss claim and the key id.
func peek(raw string) (iss, kid string, err error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", "", errors.New("want three base64url segments")
	}
	hd, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("header: %w", err)
	}
	var h struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hd, &h); err != nil {
		return "", "", fmt.Errorf("header: %w", err)
	}
	pl, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("payload: %w", err)
	}
	var p struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(pl, &p); err != nil {
		return "", "", fmt.Errorf("payload: %w", err)
	}
	return p.Iss, h.Kid, nil
}

// jwks returns the issuer's public keys, refreshing them when stale or
// when the requested kid is unknown (rate-limited by miss). Stale keys are
// kept on fetch failure.
func (v *Verifier) jwks(ctx context.Context, kid string) (map[string]crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	stale := v.keys == nil || time.Now().After(v.fetchedAt.Add(v.ttl))
	kidMiss := kid != "" && !hasKey(v.keys, kid) && time.Now().After(v.nextMiss)
	if stale || kidMiss {
		set, err := fetchKeys(ctx, v.http, v.wellKnown)
		if err == nil {
			v.keys = set
			v.fetchedAt = time.Now()
			v.nextMiss = time.Now().Add(v.miss)
		} else if v.keys == nil {
			// No usable keys at all: report the fetch failure.
			return nil, err
		}
	}
	return v.keys, nil
}

func hasKey(keys map[string]crypto.PublicKey, kid string) bool {
	_, ok := keys[kid]
	return ok
}

type jwkDoc struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (d jwkDoc) parse() (map[string]crypto.PublicKey, bool) {
	if len(d.Keys) == 0 {
		return nil, false
	}
	out := make(map[string]crypto.PublicKey, len(d.Keys))
	for _, k := range d.Keys {
		var key crypto.PublicKey
		switch k.Kty {
		case "RSA":
			n, err := base64.RawURLEncoding.DecodeString(k.N)
			if err != nil {
				continue
			}
			e, err := base64.RawURLEncoding.DecodeString(k.E)
			if err != nil {
				continue
			}
			key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				continue
			}
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil {
				continue
			}
			y, err := base64.RawURLEncoding.DecodeString(k.Y)
			if err != nil {
				continue
			}
			key = &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		default:
			continue
		}
		if k.Kid != "" {
			out[k.Kid] = key
		}
	}
	return out, true
}

// fetchKeys loads the JWKS for a well-known URL. The URL may point at an
// OpenID discovery document or directly at a JWKS document.
func fetchKeys(ctx context.Context, c *http.Client, wellKnown string) (map[string]crypto.PublicKey, error) {
	body, err := httpGet(ctx, c, wellKnown)
	if err != nil {
		return nil, err
	}
	var probe struct {
		JWKSURI string `json:"jwks_uri"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.JWKSURI != "" {
		body, err = httpGet(ctx, c, probe.JWKSURI)
		if err != nil {
			return nil, err
		}
	}
	var doc jwkDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	keys, ok := doc.parse()
	if !ok {
		return nil, errors.New("no usable keys in JWKS")
	}
	return keys, nil
}

func httpGet(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	const max = 1 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, errors.New("response too large")
	}
	return b, nil
}
