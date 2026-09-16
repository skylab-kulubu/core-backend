package authn

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type jwkRSA struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDoc struct {
	Keys []jwkRSA `json:"keys"`
}

type JWKS struct {
	url    string
	client *http.Client
	mu     sync.Mutex
	keys   map[string]*rsa.PublicKey
	expiry time.Time
}

func NewJWKS(url string) *JWKS {
	return &JWKS{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
		keys:   map[string]*rsa.PublicKey{},
	}
}

func (j *JWKS) Verify(token string) error {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))
	_, err := parser.Parse(token, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, err := j.key(kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	})
	return err
}

func (j *JWKS) key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if time.Now().After(j.expiry) {
		if err := j.refreshLocked(); err != nil {
			return nil, err
		}
	}
	key, ok := j.keys[kid]
	if ok {
		return key, nil
	}
	if err := j.refreshLocked(); err != nil {
		return nil, err
	}
	key, ok = j.keys[kid]
	if !ok {
		return nil, ErrInvalidToken
	}
	return key, nil
}

func (j *JWKS) refreshLocked() error {
	resp, err := j.client.Get(j.url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("authn: jwks fetch failed")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	next := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		pub, err := rsaPublic(k.N, k.E)
		if err != nil {
			continue
		}
		next[k.Kid] = pub
	}
	j.keys = next
	j.expiry = time.Now().Add(10 * time.Minute)
	return nil
}

func rsaPublic(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if n.Sign() <= 0 || e < 2 {
		return nil, ErrInvalidToken
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

func ParseAndVerify(token string, verify func(string) error) (Identity, error) {
	if verify != nil {
		if err := verify(token); err != nil {
			return Identity{}, ErrInvalidToken
		}
	}
	return ParseAccessToken(token)
}
