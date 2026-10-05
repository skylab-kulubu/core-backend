// Package doorqr is the signed door QR that lets a guest check themselves in
// to a Session (docs/guest-self-check-in.md).
//
// Door staff show a short-lived token on a screen at the door; it rotates
// every RefreshAfter. A guest scans it, and Guest check-in sends it back with
// the guest's e-mail. Core alone mints and reads these tokens, so the key is
// symmetric and never published, unlike SkyPass's.
//
// The token is kept short so the QR stays small enough to scan from a screen
// across a doorway: `v1.<exp>.<jti>.<mac>`. The Session id travels in the
// URL path and the Event id is not sent at all; both are bound into the MAC.
package doorqr

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

var (
	// ErrInvalid is a token that is missing, malformed, signed with another
	// key, or minted for another Session or Event.
	ErrInvalid = errors.New("doorqr: invalid")
	// ErrExpired is a genuine token past its expiry.
	ErrExpired = errors.New("doorqr: expired")
	// ErrUsedUp is a genuine token that has already let MaxUses guests in.
	ErrUsedUp = errors.New("doorqr: used up")
)

// Mode is what Guest check-in asks of a guest (GUEST_SELF_CHECKIN_MODE).
type Mode string

const (
	// ModeOpen takes a guest's e-mail alone, as before the door QR. A token
	// that is sent is still checked.
	ModeOpen Mode = "open"
	// ModeQR requires a valid door QR token on every guest check-in.
	ModeQR Mode = "qr"
)

const (
	ModeEnv         = "GUEST_SELF_CHECKIN_MODE"
	TTLEnv          = "DOOR_QR_TTL"
	MaxUsesEnv      = "DOOR_QR_MAX_USES"
	GuestURLEnv     = "DOOR_QR_GUEST_URL"
	SessionGraceEnv = "DOOR_QR_SESSION_GRACE"

	// DefaultTTL is how long a door QR token is accepted. The screen asks
	// for a new one every quarter of it (RefreshAfter), so a guest who has
	// just scanned has at least three quarters left to type an e-mail.
	DefaultTTL = 60 * time.Second
	MinTTL     = 30 * time.Second
	MaxTTL     = 10 * time.Minute
	// DefaultMaxUses is how many guests one token checks in (successful or
	// already checked in). Failed attempts do not count; the route's
	// per-address budget limits those.
	DefaultMaxUses = 20
	// DefaultSessionGrace is how long before a Session's start and after its
	// end guests may check themselves in.
	DefaultSessionGrace = 30 * time.Minute
	MaxSessionGrace     = 12 * time.Hour

	// ClockLeeway is how far a token's expiry may lie beyond now + TTL: a
	// replica whose clock runs slightly ahead still has its tokens accepted.
	ClockLeeway = 5 * time.Second
	// MaxTokenBytes is the longest token looked at; anything longer is
	// refused before it is parsed.
	MaxTokenBytes = 1024

	// Version prefixes every token and names the key derivation.
	Version = "v1"
	// QueryParam carries the token in the URL the QR encodes.
	QueryParam = "dq"
	// SessionPlaceholder is replaced by the Session id in DOOR_QR_GUEST_URL.
	SessionPlaceholder = "{sessionId}"

	keyInfo   = "skylab core door-qr v1"
	macDomain = "door-qr v1\x00"
	jtiBytes  = 6
	macBytes  = 12
	// maxTracked bounds the use counts kept in memory. Only genuine tokens
	// are counted, and minting is rate limited, so it is not reached in use.
	maxTracked = 10000
)

var b64 = base64.RawURLEncoding

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
	// SessionGrace widens a Session's time window on both sides. Negative
	// means the default; zero means none.
	SessionGrace time.Duration
}

// ConfigFromEnv reads the settings. Unset values take their defaults; a value
// that does not parse is an error, so a typo cannot quietly reopen guest
// check-in or disable a limit.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{Mode: ModeOpen, TTL: DefaultTTL, MaxUses: DefaultMaxUses, SessionGrace: DefaultSessionGrace}
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
	if raw := strings.TrimSpace(getenv(SessionGraceEnv)); raw != "" {
		grace, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", SessionGraceEnv, err)
		}
		if grace < 0 || grace > MaxSessionGrace {
			return Config{}, fmt.Errorf("%s: %s is outside 0–%s", SessionGraceEnv, grace, MaxSessionGrace)
		}
		cfg.SessionGrace = grace
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

// Gate mints and admits door QR tokens and counts guest check-ins.
type Gate struct {
	cfg Config
	key []byte
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu        sync.Mutex
	uses      map[string]use
	maxTrack  int
	lastSweep time.Time

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
	if cfg.SessionGrace < 0 {
		cfg.SessionGrace = DefaultSessionGrace
	}
	return &Gate{cfg: cfg, key: append([]byte(nil), key...), uses: map[string]use{}, maxTrack: maxTracked}
}

// NewEphemeralGate makes an open-mode gate with a random key: what a service
// built without one (tests, tools) uses.
func NewEphemeralGate() *Gate {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return NewGate(key, Config{SessionGrace: -1})
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

// SessionOpen reports whether guests may check in now to a Session with this
// schedule: not cancelled, and within [start − grace, end + grace]. A missing
// start or end leaves that side open.
func (g *Gate) SessionOpen(start, end *time.Time, cancelled bool) bool {
	if cancelled {
		return false
	}
	now := g.clock()
	if start != nil && now.Before(start.Add(-g.cfg.SessionGrace)) {
		return false
	}
	if end != nil && now.After(end.Add(g.cfg.SessionGrace)) {
		return false
	}
	return true
}

func (g *Gate) mac(sessionID, eventID uuid.UUID, exp int64, jti []byte) []byte {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(macDomain))
	m.Write(sessionID[:])
	m.Write(eventID[:])
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(exp))
	m.Write(e[:])
	m.Write(jti)
	return m.Sum(nil)[:macBytes]
}

// Mint signs a token for one Session of one Event.
func (g *Gate) Mint(sessionID, eventID uuid.UUID) (Pass, error) {
	now := g.clock().Truncate(time.Second)
	exp := now.Add(g.cfg.TTL)
	jti := make([]byte, jtiBytes)
	if _, err := rand.Read(jti); err != nil {
		return Pass{}, err
	}
	token := Version + "." + strconv.FormatInt(exp.Unix(), 36) + "." + b64.EncodeToString(jti) + "." +
		b64.EncodeToString(g.mac(sessionID, eventID, exp.Unix(), jti))
	g.mu.Lock()
	g.sweepLocked(now)
	g.mu.Unlock()
	return Pass{
		Token:        token,
		URL:          g.passURL(sessionID, token),
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

// Use is one admitted check-in on a token. Release gives the use back when
// the check-in did not happen.
type Use struct {
	g   *Gate
	key string
}

// Release returns the use to the token: a failed attempt (an e-mail without a
// Ticket, a server error) does not spend a guest's place.
func (u Use) Release() {
	if u.g == nil {
		return
	}
	u.g.mu.Lock()
	defer u.g.mu.Unlock()
	if cur, ok := u.g.uses[u.key]; ok && cur.n > 0 {
		cur.n--
		u.g.uses[u.key] = cur
	}
}

// Admit checks a token for a guest check-in on sessionID, whose Event is
// eventID, and reserves one of its uses. The caller releases the use if the
// check-in fails.
func (g *Gate) Admit(raw string, sessionID, eventID uuid.UUID) (Use, error) {
	if len(raw) > MaxTokenBytes {
		return Use{}, ErrInvalid
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 4 || parts[0] != Version {
		return Use{}, ErrInvalid
	}
	expUnix, err := strconv.ParseInt(parts[1], 36, 64)
	if err != nil || expUnix <= 0 || strconv.FormatInt(expUnix, 36) != parts[1] {
		return Use{}, ErrInvalid
	}
	jti, err := b64.DecodeString(parts[2])
	if err != nil || len(jti) != jtiBytes {
		return Use{}, ErrInvalid
	}
	mac, err := b64.DecodeString(parts[3])
	if err != nil || len(mac) != macBytes {
		return Use{}, ErrInvalid
	}
	if !hmac.Equal(mac, g.mac(sessionID, eventID, expUnix, jti)) {
		return Use{}, ErrInvalid
	}
	now := g.clock()
	exp := time.Unix(expUnix, 0).UTC()
	if !now.Before(exp) {
		return Use{}, ErrExpired
	}
	// No token is minted to live longer than TTL; one that claims to is
	// from a clock far ahead or another configuration.
	if exp.After(now.Add(g.cfg.TTL + ClockLeeway)) {
		return Use{}, ErrInvalid
	}
	key := parts[1] + "." + parts[2]
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, tracked := g.uses[key]
	if !tracked && len(g.uses) >= g.maxTrack {
		g.sweepLocked(now)
		if len(g.uses) >= g.maxTrack {
			return Use{}, ErrUsedUp
		}
	}
	if cur.n >= g.cfg.MaxUses {
		return Use{}, ErrUsedUp
	}
	g.uses[key] = use{n: cur.n + 1, exp: exp}
	if now.Sub(g.lastSweep) > g.cfg.TTL {
		g.sweepLocked(now)
	}
	return Use{g: g, key: key}, nil
}

// sweepLocked forgets the use counts of tokens that have expired.
func (g *Gate) sweepLocked(now time.Time) {
	g.lastSweep = now
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
	OutcomeSessionClosed
	OutcomeRequired
	OutcomeInvalid
	OutcomeExpired
	OutcomeUsedUp
	OutcomeFailed
	outcomeCount
)

var outcomeNames = [outcomeCount]string{
	"checked_in", "already_checked_in", "not_found", "bad_request", "session_closed", "door_qr_required",
	"door_qr_invalid", "door_qr_expired", "door_qr_used_up", "failed",
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
