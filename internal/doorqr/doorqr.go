// Package doorqr is the signed door QR that lets a guest check themselves in
// to a Session (docs/guest-self-check-in.md).
//
// Door staff show a short-lived token on a screen at the door; it rotates
// every RefreshAfter. A guest scans it, and Guest check-in sends it back with
// the guest's e-mail. Core alone mints and reads these tokens, so the key is
// symmetric (HS256) and never published, unlike SkyPass's.
package doorqr

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

var (
	// ErrInvalid is a token that is missing, malformed, signed with another
	// key or algorithm, or minted for another Session or Event.
	ErrInvalid = errors.New("doorqr: invalid")
	// ErrExpired is a genuine token past its expiry.
	ErrExpired = errors.New("doorqr: expired")
	// ErrUsedUp is a genuine token that has already admitted MaxUses
	// check-in attempts.
	ErrUsedUp = errors.New("doorqr: used up")
)

// Mode is what Guest check-in asks of a guest (GUEST_SELF_CHECKIN_MODE).
type Mode string

const (
	// ModeOpen takes a guest's e-mail alone, as before the door QR. A token
	// that is sent is still checked. The default until the door screens
	// ship.
	ModeOpen Mode = "open"
	// ModeQR requires a valid door QR token on every guest check-in.
	ModeQR Mode = "qr"
)

const (
	ModeEnv     = "GUEST_SELF_CHECKIN_MODE"
	TTLEnv      = "DOOR_QR_TTL"
	MaxUsesEnv  = "DOOR_QR_MAX_USES"
	GuestURLEnv = "DOOR_QR_GUEST_URL"

	// DefaultTTL is how long a door QR token is accepted. The screen asks
	// for a new one every quarter of it, so a guest who has just scanned has
	// at least three quarters left to type an e-mail.
	DefaultTTL = 120 * time.Second
	MinTTL     = 30 * time.Second
	MaxTTL     = 10 * time.Minute
	// DefaultMaxUses is how many guest check-in attempts one token admits.
	DefaultMaxUses = 50

	// TokenType is the JWT `typ` header of a door QR token. Tokens without
	// it are refused, so no other HS256 token can pass as one.
	TokenType = "door-qr+jwt"
	// Kid names the key derivation, for a later rotation.
	Kid = "dq1"
	// QueryParam carries the token in the URL the QR encodes.
	QueryParam = "dq"
	// SessionPlaceholder is replaced by the Session id in DOOR_QR_GUEST_URL.
	SessionPlaceholder = "{sessionId}"

	keyInfo = "skylab core door-qr v1"
)

// Config is the door QR's settings.
type Config struct {
	Mode    Mode
	TTL     time.Duration
	MaxUses int
	// GuestURL is the page a guest's camera opens, with SessionPlaceholder
	// where the Session id goes. Empty: the Session's API address
	// (`PUBLIC_API_ORIGIN/v1/sessions/{id}`), the shape the Session QR
	// already has, so sky-app's scanner reads it as that Session.
	GuestURL string
}

// ConfigFromEnv reads the settings. Unset values take their defaults; a value
// that does not parse is an error, so a typo cannot quietly reopen guest
// check-in or disable the cap.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{Mode: ModeOpen, TTL: DefaultTTL, MaxUses: DefaultMaxUses}
	switch mode := Mode(strings.TrimSpace(getenv(ModeEnv))); mode {
	case "":
	case ModeOpen, ModeQR:
		cfg.Mode = mode
	default:
		return Config{}, fmt.Errorf("%s: %q is neither %q nor %q", ModeEnv, mode, ModeOpen, ModeQR)
	}
	if raw := strings.TrimSpace(getenv(TTLEnv)); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", TTLEnv, err)
		}
		if ttl < MinTTL || ttl > MaxTTL {
			return Config{}, fmt.Errorf("%s: %s is outside %s–%s", TTLEnv, ttl, MinTTL, MaxTTL)
		}
		cfg.TTL = ttl
	}
	if raw := strings.TrimSpace(getenv(MaxUsesEnv)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("%s: %q is not a positive whole number", MaxUsesEnv, raw)
		}
		cfg.MaxUses = n
	}
	if raw := strings.TrimSpace(getenv(GuestURLEnv)); raw != "" {
		u, err := url.Parse(strings.ReplaceAll(raw, SessionPlaceholder, uuid.Nil.String()))
		if err != nil || u.Scheme != "https" || u.Host == "" || !strings.Contains(raw, SessionPlaceholder) {
			return Config{}, fmt.Errorf("%s: want an https URL containing %s", GuestURLEnv, SessionPlaceholder)
		}
		cfg.GuestURL = raw
	}
	return cfg, nil
}

// DeriveKey derives the door QR's HMAC key from a longer-lived secret
// (HKDF-SHA256, RFC 5869), so the door QR needs no secret of its own yet
// never signs with the secret it came from.
func DeriveKey(secret []byte) []byte {
	key, err := hkdf.Key(sha256.New, secret, nil, keyInfo, 32)
	if err != nil {
		// Only a length beyond 255 hash blocks fails.
		panic(err)
	}
	return key
}

// Pass is a minted door QR.
type Pass struct {
	Token        string
	URL          string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	RefreshAfter time.Duration
}

type claims struct {
	SessionID string `json:"sid"`
	EventID   string `json:"eid"`
	jwt.RegisteredClaims
}

// Gate mints and admits door QR tokens and counts guest check-ins.
type Gate struct {
	cfg Config
	key []byte
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu   sync.Mutex
	uses map[string]use

	counts [2][outcomeCount]atomic.Uint64
}

type use struct {
	n   int
	exp time.Time
}

// NewGate makes a gate; zero Config fields take their defaults.
func NewGate(key []byte, cfg Config) *Gate {
	if cfg.Mode == "" {
		cfg.Mode = ModeOpen
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.MaxUses <= 0 {
		cfg.MaxUses = DefaultMaxUses
	}
	return &Gate{cfg: cfg, key: append([]byte(nil), key...), uses: map[string]use{}}
}

// NewEphemeralGate makes an open-mode gate with a random key: what a service
// built without one (tests, tools) uses.
func NewEphemeralGate() *Gate {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return NewGate(key, Config{})
}

func (g *Gate) clock() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Mode reports whether guest check-in requires a door QR.
func (g *Gate) Mode() Mode {
	if g == nil {
		return ModeOpen
	}
	return g.cfg.Mode
}

// Mint signs a token for one Session of one Event.
func (g *Gate) Mint(sessionID, eventID uuid.UUID) (Pass, error) {
	now := g.clock().Truncate(time.Second)
	exp := now.Add(g.cfg.TTL)
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return Pass{}, err
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		SessionID: sessionID.String(),
		EventID:   eventID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        base64.RawURLEncoding.EncodeToString(jti),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	tok.Header["typ"] = TokenType
	tok.Header["kid"] = Kid
	signed, err := tok.SignedString(g.key)
	if err != nil {
		return Pass{}, err
	}
	g.sweep(now)
	return Pass{
		Token:        signed,
		URL:          g.passURL(sessionID, signed),
		IssuedAt:     now,
		ExpiresAt:    exp,
		RefreshAfter: g.cfg.TTL / 4,
	}, nil
}

func (g *Gate) passURL(sessionID uuid.UUID, token string) string {
	base := qr.SessionURL(sessionID.String())
	if g.cfg.GuestURL != "" {
		base = strings.ReplaceAll(g.cfg.GuestURL, SessionPlaceholder, sessionID.String())
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set(QueryParam, token)
	u.RawQuery = q.Encode()
	return u.String()
}

// Admit checks a token for a guest check-in on sessionID, whose Event is
// eventID, and spends one of its uses.
func (g *Gate) Admit(raw string, sessionID, eventID uuid.UUID) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrInvalid
	}
	now := g.clock()
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	var c claims
	_, err := parser.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		if t.Header["typ"] != TokenType || t.Header["kid"] != Kid {
			return nil, ErrInvalid
		}
		return g.key, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return ErrExpired
		}
		return ErrInvalid
	}
	if c.ID == "" || c.SessionID != sessionID.String() || c.EventID != eventID.String() {
		return ErrInvalid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.uses[c.ID]
	if u.n >= g.cfg.MaxUses {
		return ErrUsedUp
	}
	g.uses[c.ID] = use{n: u.n + 1, exp: c.ExpiresAt.Time}
	return nil
}

// sweep forgets the use counts of tokens that have expired.
func (g *Gate) sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, u := range g.uses {
		if !now.Before(u.exp) {
			delete(g.uses, id)
		}
	}
}

func (g *Gate) trackedTokens() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.uses)
}

// Outcome is how a guest check-in ended.
type Outcome int

const (
	OutcomeCheckedIn Outcome = iota
	OutcomeAlready
	OutcomeNotFound
	OutcomeBadRequest
	OutcomeRequired
	OutcomeInvalid
	OutcomeExpired
	OutcomeUsedUp
	OutcomeFailed
	outcomeCount
)

var outcomeNames = [outcomeCount]string{
	"checked_in", "already_checked_in", "not_found", "bad_request", "door_qr_required", "door_qr_invalid",
	"door_qr_expired", "door_qr_used_up", "failed",
}

// Record counts a guest check-in by whether it carried a door QR token.
func (g *Gate) Record(withToken bool, outcome Outcome) {
	if g == nil || outcome < 0 || outcome >= outcomeCount {
		return
	}
	i := 0
	if withToken {
		i = 1
	}
	g.counts[i][outcome].Add(1)
}

// Prometheus renders the counters and the mode in the text exposition
// format. Nobody, no Session and no Event is named. The switch to qr waits on
// door_qr="absent" check-ins: each is a guest who would be refused.
func (g *Gate) Prometheus() string {
	if g == nil {
		return ""
	}
	var out strings.Builder
	out.WriteString("# TYPE skylab_guest_self_checkin_total counter\n")
	for i, presence := range [2]string{"absent", "present"} {
		for outcome := range outcomeCount {
			out.WriteString(`skylab_guest_self_checkin_total{door_qr="` + presence + `",outcome="` + outcomeNames[outcome] +
				`"} ` + strconv.FormatUint(g.counts[i][outcome].Load(), 10) + "\n")
		}
	}
	out.WriteString("# TYPE skylab_guest_self_checkin_mode gauge\n")
	for _, mode := range [2]Mode{ModeOpen, ModeQR} {
		v := "0"
		if g.cfg.Mode == mode {
			v = "1"
		}
		out.WriteString(`skylab_guest_self_checkin_mode{mode="` + string(mode) + `"} ` + v + "\n")
	}
	return out.String()
}
