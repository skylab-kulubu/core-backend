package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	errTokenInvalid    = errors.New("token invalid")
	errTokenForbidden  = errors.New("token forbidden")
	errKeysUnavailable = errors.New("token keys unavailable")
)

// claims holds the parts of a Keycloak access token the stub reads.
type claims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Expiry          json.Number     `json:"exp"`
	AuthorizedParty string          `json:"azp"`
	Audience        json.RawMessage `json:"aud"`
	ResourceAccess  map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

func (c claims) audiences() []string {
	var one string
	if json.Unmarshal(c.Audience, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(c.Audience, &many) == nil {
		return many
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// verifier checks RS256 access tokens against the realm JWKS.
type verifier struct {
	issuer  string
	jwksURL string
	client  *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newVerifier(issuer, jwksURL string) *verifier {
	return &verifier{issuer: issuer, jwksURL: jwksURL, client: &http.Client{Timeout: 5 * time.Second}}
}

// verify returns the claims of a valid bearer token: signature, issuer and expiry.
func (v *verifier) verify(ctx context.Context, authorization string) (claims, error) {
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || raw == "" {
		return claims{}, errTokenInvalid
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims{}, errTokenInvalid
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return claims{}, errTokenInvalid
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return claims{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims{}, errTokenInvalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return claims{}, errTokenInvalid
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return claims{}, errTokenInvalid
	}
	exp, err := c.Expiry.Int64()
	if err != nil || time.Now().Unix() >= exp || c.Issuer != v.issuer || c.Subject == "" {
		return claims{}, errTokenInvalid
	}
	return c, nil
}

func decodeSegment(segment string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	return decoder.Decode(into)
}

func (v *verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	if time.Since(v.fetched) < 5*time.Second && v.keys != nil {
		return nil, errTokenInvalid
	}
	keys, err := v.fetch(ctx)
	if err != nil {
		return nil, errKeysUnavailable
	}
	v.keys, v.fetched = keys, time.Now()
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, errTokenInvalid
}

func (v *verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return keys, nil
}
