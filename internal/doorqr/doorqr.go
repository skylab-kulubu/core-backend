// Package doorqr is the signed door QR that lets a guest check themselves in
// to a Session, the rotating attendance QR (the Session QR) that a member
// scans with sky-app or a guest with their phone's camera, and the static
// attendance QR, one per Session, for a printout or a screen that cannot
// refresh (docs/guest-self-check-in.md).
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

// Class is what a token was minted for. It decides how many check-ins one
// token admits, and it is covered by the MAC, so a door token cannot be
// relabelled to get the larger budget of a projection screen.
type Class string

const (
	// ClassDoor is the door screen: guests pass it one by one.
	ClassDoor Class = "door"
	// ClassDisplay is the attendance screen on a projector or OBS: a whole
	// room scans the same frame.
	ClassDisplay Class = "display"
	// ClassStatic is the static attendance QR: the same token for a Session
	// every time, for a printout or a screen that cannot refresh. It never
	// expires and has no use cap; the Session's time window bounds it.
	ClassStatic Class = "static"
)

// ParseClass reads a class name; empty is ClassDoor.
func ParseClass(raw string) (Class, bool) {
	switch Class(strings.TrimSpace(raw)) {
	case "", ClassDoor:
		return ClassDoor, true
	case ClassDisplay:
		return ClassDisplay, true
	case ClassStatic:
		return ClassStatic, true
	default:
		return "", false
	}
}

// Mode is what a self check-in asks for: Guest check-in reads
// GUEST_SELF_CHECKIN_MODE, a member's own check-in (check-in/me) reads
// SESSION_QR_MODE.
type Mode string

const (
	// ModeOpen takes a check-in without a token (a guest's e-mail alone; a
	// member's token alone, the static Session QR). A token that is sent is
	// still checked.
	ModeOpen Mode = "open"
	// ModeQR requires a valid door or attendance QR token on every check-in.
	ModeQR Mode = "qr"
)

const (
	ModeEnv         = "GUEST_SELF_CHECKIN_MODE"
	TTLEnv          = "DOOR_QR_TTL"
	MaxUsesEnv      = "DOOR_QR_MAX_USES"
	GuestURLEnv     = "DOOR_QR_GUEST_URL"
	SessionGraceEnv = "DOOR_QR_SESSION_GRACE"
	// SessionModeEnv is the mode of a member's own check-in (check-in/me):
	// open keeps the static Session QR working, qr retires it.
	SessionModeEnv = "SESSION_QR_MODE"
	// DisplayMaxUsesEnv is how many check-ins one attendance screen token
	// admits.
	DisplayMaxUsesEnv = "ATTENDANCE_QR_MAX_USES"
	// StaticEnv turns the static attendance QR on or off: on (the default)
	// mints and accepts it, off refuses both, so every static QR already
	// printed or shown stops checking anyone in.
	StaticEnv = "DOOR_QR_STATIC"

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
	// DefaultDisplayMaxUses is the same for a projection screen's token: a
	// hall of a few hundred people scans one 15-second frame.
	DefaultDisplayMaxUses = 400
	// DefaultSessionGrace is how long before a Session's start and after its
	// end the door QR is minted and guests may check themselves in: the door
	// screen can be set up well ahead and stays usable after an overrun.
	// DOOR_QR_SESSION_GRACE may narrow it, never widen it.
	DefaultSessionGrace = 12 * time.Hour
	MaxSessionGrace     = DefaultSessionGrace

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

	// DisplayField marks a display token: `v1.<exp>.<jti>.a.<mac>`. A door
	// token keeps the original four fields.
	DisplayField = "a"
	// StaticField marks a static token: `v1.s.<mac>`, no expiry and no jti.
	StaticField = "s"

	keyInfo          = "skylab core door-qr v1"
	macDomain        = "door-qr v1\x00"
	macDomainDisplay = "door-qr display v1\x00"
	macDomainStatic  = "door-qr static v1\x00"
	jtiBytes         = 6
	macBytes         = 12
	// maxTracked bounds the use counts kept in memory. Only genuine tokens
	// are counted, and minting is rate limited, so it is not reached in use.
	maxTracked = 10000
)

var b64 = base64.RawURLEncoding

// Config is the door QR's settings.
type Config struct {
	// Mode is Guest check-in's (GUEST_SELF_CHECKIN_MODE).
	Mode Mode
	// SessionMode is a member's own check-in's (SESSION_QR_MODE).
	SessionMode Mode
	TTL         time.Duration
	// MaxUses caps a door token, DisplayMaxUses a display token.
	MaxUses        int
	DisplayMaxUses int
	// GuestURL is the page a guest's camera opens, with SessionPlaceholder
	// where the Session id goes. Empty: the Session's API address
	// (`PUBLIC_API_ORIGIN/v1/sessions/{id}`), the shape the Session QR
	// already has, so sky-app's scanner reads it as that Session.
	GuestURL string
	// SessionGrace widens a Session's time window on both sides. Negative
	// means the default; zero means none.
	SessionGrace time.Duration
	// StaticOff refuses the static attendance QR (DOOR_QR_STATIC=off).
	StaticOff bool
}

// ConfigFromEnv reads the settings. Unset values take their defaults; a value
// that does not parse is an error, so a typo cannot quietly reopen guest
// check-in or disable a limit.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Mode: ModeOpen, SessionMode: ModeOpen, TTL: DefaultTTL, MaxUses: DefaultMaxUses,
		DisplayMaxUses: DefaultDisplayMaxUses, SessionGrace: DefaultSessionGrace,
	}
	for _, setting := range []struct {
		env  string
		dest *Mode
	}{{ModeEnv, &cfg.Mode}, {SessionModeEnv, &cfg.SessionMode}} {
		switch mode := Mode(strings.TrimSpace(getenv(setting.env))); mode {
		case "":
		case ModeOpen, ModeQR:
			*setting.dest = mode
		default:
			return Config{}, fmt.Errorf("%s: %q is neither %q nor %q", setting.env, mode, ModeOpen, ModeQR)
		}
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
	for _, setting := range []struct {
		env  string
		dest *int
	}{{MaxUsesEnv, &cfg.MaxUses}, {DisplayMaxUsesEnv, &cfg.DisplayMaxUses}} {
		if raw := strings.TrimSpace(getenv(setting.env)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return Config{}, fmt.Errorf("%s: %q is not a positive whole number", setting.env, raw)
			}
			*setting.dest = n
		}
	}
	if raw := strings.TrimSpace(getenv(GuestURLEnv)); raw != "" {
		u, err := url.Parse(strings.ReplaceAll(raw, SessionPlaceholder, uuid.Nil.String()))
		if err != nil || u.Scheme != "https" || u.Host == "" || !strings.Contains(raw, SessionPlaceholder) {
			return Config{}, fmt.Errorf("%s: want an https URL containing %s", GuestURLEnv, SessionPlaceholder)
		}
		cfg.GuestURL = raw
	}
	switch raw := strings.TrimSpace(getenv(StaticEnv)); raw {
	case "", "on":
	case "off":
		cfg.StaticOff = true
	default:
		return Config{}, fmt.Errorf("%s: %q is neither %q nor %q", StaticEnv, raw, "on", "off")
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
	Class        Class
	MaxUses      int
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

	counts     [2][outcomeCount]atomic.Uint64
	selfCounts [2][outcomeCount]atomic.Uint64
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
	if cfg.SessionMode == "" {
		cfg.SessionMode = ModeOpen
	}
	if cfg.DisplayMaxUses <= 0 {
		cfg.DisplayMaxUses = DefaultDisplayMaxUses
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

// SessionMode reports whether a member's own check-in requires a door or
// attendance QR token, that is whether the static Session QR is retired.
func (g *Gate) SessionMode() Mode {
	if g == nil {
		return ModeOpen
	}
	return g.cfg.SessionMode
}

// StaticOn reports whether the static attendance QR is minted and accepted.
func (g *Gate) StaticOn() bool {
	return g != nil && !g.cfg.StaticOff
}

// SessionWindow is the window SessionOpen applies to a Session with this
// schedule: nil on a side the schedule leaves open.
func (g *Gate) SessionWindow(start, end *time.Time) (from, until *time.Time) {
	if start != nil {
		t := start.Add(-g.cfg.SessionGrace).UTC()
		from = &t
	}
	if end != nil {
		t := end.Add(g.cfg.SessionGrace).UTC()
		until = &t
	}
	return from, until
}

func (g *Gate) maxUses(class Class) int {
	if class == ClassDisplay {
		return g.cfg.DisplayMaxUses
	}
	return g.cfg.MaxUses
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

func (g *Gate) mac(class Class, sessionID, eventID uuid.UUID, exp int64, jti []byte) []byte {
	m := hmac.New(sha256.New, g.key)
	// The class picks the domain label, so a door token and a display token
	// never share a MAC input.
	if class == ClassDisplay {
		m.Write([]byte(macDomainDisplay))
	} else {
		m.Write([]byte(macDomain))
	}
	m.Write(sessionID[:])
	m.Write(eventID[:])
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(exp))
	m.Write(e[:])
	m.Write(jti)
	return m.Sum(nil)[:macBytes]
}

// staticMAC is the static token's tag: it covers only the Session and the
// Event, so the token is the same every time it is minted.
func (g *Gate) staticMAC(sessionID, eventID uuid.UUID) []byte {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(macDomainStatic))
	m.Write(sessionID[:])
	m.Write(eventID[:])
	return m.Sum(nil)[:macBytes]
}

// ErrStaticOff is a static token minted or presented while
// DOOR_QR_STATIC=off.
var ErrStaticOff = errors.New("doorqr: static qr off")

// Mint signs a door token for one Session of one Event.
func (g *Gate) Mint(sessionID, eventID uuid.UUID) (Pass, error) {
	return g.MintFor(ClassDoor, sessionID, eventID)
}

// MintFor signs a token of the given class for one Session of one Event. A
// static Pass has no ExpiresAt, no RefreshAfter and no MaxUses.
func (g *Gate) MintFor(class Class, sessionID, eventID uuid.UUID) (Pass, error) {
	now := g.clock().Truncate(time.Second)
	if class == ClassStatic {
		if g.cfg.StaticOff {
			return Pass{}, ErrStaticOff
		}
		token := Version + "." + StaticField + "." + b64.EncodeToString(g.staticMAC(sessionID, eventID))
		return Pass{Token: token, URL: g.passURL(sessionID, token), Class: ClassStatic, IssuedAt: now}, nil
	}
	if class != ClassDisplay {
		class = ClassDoor
	}
	exp := now.Add(g.cfg.TTL)
	jti := make([]byte, jtiBytes)
	if _, err := rand.Read(jti); err != nil {
		return Pass{}, err
	}
	token := Version + "." + strconv.FormatInt(exp.Unix(), 36) + "." + b64.EncodeToString(jti) + "."
	if class == ClassDisplay {
		token += DisplayField + "."
	}
	token += b64.EncodeToString(g.mac(class, sessionID, eventID, exp.Unix(), jti))
	g.mu.Lock()
	g.sweepLocked(now)
	g.mu.Unlock()
	return Pass{
		Token:        token,
		URL:          g.passURL(sessionID, token),
		Class:        class,
		MaxUses:      g.maxUses(class),
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
// the check-in did not happen. A static token's Use counts nothing.
type Use struct {
	g      *Gate
	key    string
	static bool
}

// Static reports whether the token admitted was a static attendance QR.
func (u Use) Static() bool { return u.static }

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

// Admit checks a token (door or display) for a check-in on sessionID, whose
// Event is eventID, and reserves one of its uses. The caller releases the use
// if the check-in fails.
func (g *Gate) Admit(raw string, sessionID, eventID uuid.UUID) (Use, error) {
	if len(raw) > MaxTokenBytes {
		return Use{}, ErrInvalid
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) == 3 && parts[0] == Version && parts[1] == StaticField {
		return g.admitStatic(parts[2], sessionID, eventID)
	}
	class := ClassDoor
	switch {
	case len(parts) == 5 && parts[3] == DisplayField:
		class = ClassDisplay
		parts = []string{parts[0], parts[1], parts[2], parts[4]}
	case len(parts) != 4:
		return Use{}, ErrInvalid
	}
	if parts[0] != Version {
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
	if !hmac.Equal(mac, g.mac(class, sessionID, eventID, expUnix, jti)) {
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
	key := string(class) + "." + parts[1] + "." + parts[2]
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, tracked := g.uses[key]
	if !tracked && len(g.uses) >= g.maxTrack {
		g.sweepLocked(now)
		if len(g.uses) >= g.maxTrack {
			return Use{}, ErrUsedUp
		}
	}
	if cur.n >= g.maxUses(class) {
		return Use{}, ErrUsedUp
	}
	g.uses[key] = use{n: cur.n + 1, exp: exp}
	if now.Sub(g.lastSweep) > g.cfg.TTL {
		g.sweepLocked(now)
	}
	return Use{g: g, key: key}, nil
}

// admitStatic checks a static token's tag. The Session's time window, which
// bounds it, is the caller's: the Session's schedule is not in the token.
func (g *Gate) admitStatic(rawMAC string, sessionID, eventID uuid.UUID) (Use, error) {
	mac, err := b64.DecodeString(rawMAC)
	if err != nil || len(mac) != macBytes || !hmac.Equal(mac, g.staticMAC(sessionID, eventID)) {
		return Use{}, ErrInvalid
	}
	if g.cfg.StaticOff {
		return Use{}, ErrStaticOff
	}
	return Use{static: true}, nil
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
	if g == nil {
		return
	}
	count(&g.counts, withToken, outcome)
}

// RecordSelf counts a member's own check-in (check-in/me) by whether it
// carried a token. The switch to SESSION_QR_MODE=qr waits on
// door_qr="absent": each is an app that still reads the static Session QR.
func (g *Gate) RecordSelf(withToken bool, outcome Outcome) {
	if g == nil {
		return
	}
	count(&g.selfCounts, withToken, outcome)
}

func count(counts *[2][outcomeCount]atomic.Uint64, withToken bool, outcome Outcome) {
	if outcome < 0 || outcome >= outcomeCount {
		return
	}
	i := 0
	if withToken {
		i = 1
	}
	counts[i][outcome].Add(1)
}

// Prometheus renders the counters and the mode in the text exposition
// format. Nobody, no Session and no Event is named. The switch to qr waits on
// door_qr="absent" check-ins: each is a guest who would be refused.
func (g *Gate) Prometheus() string {
	if g == nil {
		return ""
	}
	var out strings.Builder
	writeCounts(&out, "skylab_guest_self_checkin", &g.counts, g.cfg.Mode)
	writeCounts(&out, "skylab_session_self_checkin", &g.selfCounts, g.cfg.SessionMode)
	return out.String()
}

func writeCounts(out *strings.Builder, name string, counts *[2][outcomeCount]atomic.Uint64, current Mode) {
	out.WriteString("# TYPE " + name + "_total counter\n")
	for i, presence := range [2]string{"absent", "present"} {
		for outcome := range outcomeCount {
			out.WriteString(name + `_total{door_qr="` + presence + `",outcome="` + outcomeNames[outcome] +
				`"} ` + strconv.FormatUint(counts[i][outcome].Load(), 10) + "\n")
		}
	}
	out.WriteString("# TYPE " + name + "_mode gauge\n")
	for _, mode := range [2]Mode{ModeOpen, ModeQR} {
		v := "0"
		if current == mode {
			v = "1"
		}
		out.WriteString(name + `_mode{mode="` + string(mode) + `"} ` + v + "\n")
	}
}
