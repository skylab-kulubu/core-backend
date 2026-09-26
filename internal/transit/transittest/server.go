// Package transittest is a fake OpenBao for tests: AppRole login, token
// renewal and the Transit encrypt and decrypt endpoints of one key, on a
// clock the test moves. Its ciphertext looks like Transit's
// ("vault:v<version>:<base64>") and is real AES-GCM under one random key per
// key version, so a rotated key still decrypts what older versions wrapped.
package transittest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// Clock is the time the fake and its clients share.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type token struct {
	expires    time.Time
	maxExpires time.Time
}

// Server is the fake. Its role is core-media's: a token TTL of one hour,
// renewable up to 24 hours.
type Server struct {
	*httptest.Server
	Clock    *Clock
	RoleID   string
	SecretID string
	Mount    string
	Key      string
	TTL      time.Duration
	MaxTTL   time.Duration

	mu       sync.Mutex
	keys     [][]byte
	tokens   map[string]*token
	sealed   bool
	refusing bool
	logins   int
	renewals int
}

// NewServer starts a fake with key version 1 of the Transit key and closes
// it when the test ends.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		Clock:    &Clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)},
		RoleID:   "core-media-role-id",
		SecretID: "core-media-secret-id",
		Mount:    "transit/test",
		Key:      "media",
		TTL:      time.Hour,
		MaxTTL:   24 * time.Hour,
		tokens:   map[string]*token{},
	}
	s.keys = [][]byte{randomKey()}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Config is a client configuration for this fake, on its clock.
func (s *Server) Config() transit.Config {
	return transit.Config{
		Addr: s.URL, Mount: s.Mount, Key: s.Key, RoleID: s.RoleID, SecretID: s.SecretID, Now: s.Clock.Now,
	}
}

// Rotate adds a key version; new wraps use it, older versions still unwrap.
func (s *Server) Rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, randomKey())
}

// RevokeTokens revokes every token issued so far.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]*token{}
}

// RefuseRenewalsAndLogins refuses every renewal and login from now on;
// tokens already issued keep working until they expire.
func (s *Server) RefuseRenewalsAndLogins() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusing = true
}

// Seal answers every request with 503, as a sealed OpenBao does.
func (s *Server) Seal() { s.setSealed(true) }

// Unseal serves again.
func (s *Server) Unseal() { s.setSealed(false) }

func (s *Server) setSealed(sealed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = sealed
}

// Logins counts AppRole logins.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// Renewals counts token renewals.
func (s *Server) Renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

func randomKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		writeErrors(w, http.StatusServiceUnavailable, "Vault is sealed")
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeErrors(w, http.StatusMethodNotAllowed, "unsupported operation")
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := s.Clock.Now()
	switch r.URL.Path {
	case "/v1/auth/approle/login":
		if s.refusing || body["role_id"] != s.RoleID || body["secret_id"] != s.SecretID {
			writeErrors(w, http.StatusBadRequest, "invalid role or secret ID")
			return
		}
		s.logins++
		id := "s." + base64.RawURLEncoding.EncodeToString(randomKey()[:12])
		s.tokens[id] = &token{expires: now.Add(s.TTL), maxExpires: now.Add(s.MaxTTL)}
		writeAuth(w, id, s.TTL)
		return
	case "/v1/auth/token/renew-self":
		id := r.Header.Get("X-Vault-Token")
		tok := s.liveToken(id, now)
		if tok == nil || s.refusing {
			writeErrors(w, http.StatusForbidden, "permission denied")
			return
		}
		s.renewals++
		tok.expires = now.Add(s.TTL)
		if tok.expires.After(tok.maxExpires) {
			tok.expires = tok.maxExpires
		}
		writeAuth(w, id, tok.expires.Sub(now))
		return
	}
	if s.liveToken(r.Header.Get("X-Vault-Token"), now) == nil {
		writeErrors(w, http.StatusForbidden, "permission denied")
		return
	}
	switch r.URL.Path {
	case "/v1/" + s.Mount + "/encrypt/" + s.Key:
		plaintext, err := base64.StdEncoding.DecodeString(body["plaintext"])
		if err != nil {
			writeErrors(w, http.StatusBadRequest, "failed to base64-decode plaintext")
			return
		}
		version := len(s.keys)
		ciphertext := fmt.Sprintf("vault:v%d:%s", version, base64.StdEncoding.EncodeToString(seal(s.keys[version-1], plaintext)))
		writeData(w, map[string]any{"ciphertext": ciphertext, "key_version": version})
	case "/v1/" + s.Mount + "/decrypt/" + s.Key:
		plaintext, err := s.decrypt(body["ciphertext"])
		if err != nil {
			writeErrors(w, http.StatusBadRequest, err.Error())
			return
		}
		writeData(w, map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plaintext)})
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/"+s.Mount+"/decrypt/") {
			// The mount is there; the key is not. (Encrypt would create
			// it, which core's policy does not grant: permission denied.)
			writeErrors(w, http.StatusBadRequest, "encryption key not found")
			return
		}
		writeErrors(w, http.StatusForbidden, "permission denied")
	}
}

func (s *Server) liveToken(id string, now time.Time) *token {
	tok, ok := s.tokens[id]
	if !ok || !now.Before(tok.expires) {
		return nil
	}
	return tok
}

func (s *Server) decrypt(ciphertext string) ([]byte, error) {
	rest, ok := strings.CutPrefix(ciphertext, "vault:v")
	if !ok {
		return nil, fmt.Errorf("invalid ciphertext: no prefix")
	}
	number, encoded, ok := strings.Cut(rest, ":")
	version, err := strconv.Atoi(number)
	if !ok || err != nil || version < 1 {
		return nil, fmt.Errorf("invalid ciphertext: no version")
	}
	if version > len(s.keys) {
		return nil, fmt.Errorf("invalid ciphertext: version is too new")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid ciphertext: could not decode")
	}
	return open(s.keys[version-1], raw)
}

func gcm(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}

func seal(key, plaintext []byte) []byte {
	aead := gcm(key)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil)
}

func open(key, raw []byte) ([]byte, error) {
	aead := gcm(key)
	if len(raw) < aead.NonceSize() {
		return nil, fmt.Errorf("invalid ciphertext: too short")
	}
	plaintext, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("cipher: message authentication failed")
	}
	return plaintext, nil
}

func writeAuth(w http.ResponseWriter, id string, lease time.Duration) {
	writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]any{
		"client_token": id, "lease_duration": int(lease / time.Second), "renewable": true,
	}})
}

func writeData(w http.ResponseWriter, data map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func writeErrors(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"errors": []string{message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
