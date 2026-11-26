// Package raffle is an Event's raffle (docs/raffle.md): its organizer
// chooses who may win by attendance and the Ticket, previews how many
// that is, and draws winners and alternates among them. The draw is
// random by crypto/rand and verifiable: the record keeps the eligible
// Tickets (ids only), their list's hash, the seed and the algorithm, so
// anyone given them can draw the same order again. A winner who did not
// come gives their place to the next alternate, with a reason; nothing is
// drawn again and no record is deleted.
package raffle

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Limits of a raffle.
const (
	MaxWinners    = 100
	MaxAlternates = 100
	MaxTitle      = 120
	MinReason     = 3
	MaxReason     = 300
	// MaxSessions bounds Criteria.SessionIDs.
	MaxSessions = 100
	// MaxCandidates is how many masked names a preview lists.
	MaxCandidates = 1000
	// DefaultTitle names a raffle drawn without a title.
	DefaultTitle = "Çekiliş"
)

// Criteria are who may win: every condition set must hold, and an
// excluded person never does. A cancelled or waitlisted Ticket never
// takes part (docs/ticket-cancellation.md).
type Criteria struct {
	// MinCounted: at least this many check-ins on the Event's live counted
	// Sessions, the certificate rule's count (docs/certificate-rule.md);
	// nil sets none.
	MinCounted *int `json:"minCounted"`
	// SessionIDs are Sessions to have checked in on: all of them
	// (SessionMatch "all") or one of them ("any"). Empty sets none.
	SessionIDs   []uuid.UUID `json:"sessionIds"`
	SessionMatch string      `json:"sessionMatch"`
	// EnteredOn: a Venue entry on that date (YYYY-MM-DD, the door's day),
	// "any" for one on any day, "" for none needed (docs/venue-entry.md).
	EnteredOn string `json:"enteredOn"`
	// TicketType: "REGISTERED", "GUEST", or "" for both.
	TicketType string `json:"ticketType"`
	// CertificateEligible: the Event's certificate rule makes the Ticket
	// eligible (certificate.Eligible).
	CertificateEligible bool `json:"certificateEligible"`
	// ExcludePastWinners leaves out who won one of the Event's earlier
	// raffles (and still holds the place).
	ExcludePastWinners bool `json:"excludePastWinners"`
	// ExcludeOrganizers leaves out the members of the Event's Owner team
	// (and its sub-teams); ExcludeCoOrganizers those of its Co-organizer
	// teams and its Co-organizer people; ExcludeDoorStaff its door staff.
	// A guest Ticket is matched by its e-mail address.
	ExcludeOrganizers   bool `json:"excludeOrganizers"`
	ExcludeDoorStaff    bool `json:"excludeDoorStaff"`
	ExcludeCoOrganizers bool `json:"excludeCoOrganizers"`
}

// Session match modes.
const (
	MatchAll = "all"
	MatchAny = "any"
)

// EnteredAny is Criteria.EnteredOn for an entry on any day.
const EnteredAny = "any"

// Ticket types, as the tickets table keeps them.
const (
	TypeRegistered = "REGISTERED"
	TypeGuest      = "GUEST"
)

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ErrInvalid is a request the raffle does not take (400 raffle_invalid).
var ErrInvalid = errors.New("raffle: invalid")

// normalize checks c and fills its defaults.
func (c Criteria) normalize() (Criteria, error) {
	if c.MinCounted != nil && (*c.MinCounted < 0 || *c.MinCounted > 1000) {
		return c, ErrInvalid
	}
	if len(c.SessionIDs) > MaxSessions {
		return c, ErrInvalid
	}
	ids := slices.Clone(c.SessionIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	c.SessionIDs = slices.Compact(ids)
	if c.SessionIDs == nil {
		c.SessionIDs = []uuid.UUID{}
	}
	switch c.SessionMatch {
	case "":
		c.SessionMatch = MatchAll
	case MatchAll, MatchAny:
	default:
		return c, ErrInvalid
	}
	switch {
	case c.EnteredOn == "", c.EnteredOn == EnteredAny:
	case datePattern.MatchString(c.EnteredOn):
		if _, err := time.Parse(time.DateOnly, c.EnteredOn); err != nil {
			return c, ErrInvalid
		}
	default:
		return c, ErrInvalid
	}
	switch c.TicketType {
	case "", TypeRegistered, TypeGuest:
	default:
		return c, ErrInvalid
	}
	return c, nil
}

// Request is a draw: its title, who may win and how many win.
type Request struct {
	Title      string   `json:"title"`
	Criteria   Criteria `json:"criteria"`
	Winners    int      `json:"winners"`
	Alternates int      `json:"alternates"`
}

func (r Request) normalize() (Request, error) {
	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" {
		r.Title = DefaultTitle
	}
	if utf8.RuneCountInString(r.Title) > MaxTitle || r.Winners < 1 || r.Winners > MaxWinners ||
		r.Alternates < 0 || r.Alternates > MaxAlternates {
		return r, ErrInvalid
	}
	c, err := r.Criteria.normalize()
	if err != nil {
		return r, err
	}
	r.Criteria = c
	return r, nil
}

// normalizeReason trims a forfeit's reason and checks its length.
func normalizeReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(reason)
	if n < MinReason || n > MaxReason {
		return "", ErrInvalid
	}
	return reason, nil
}

// Entry statuses.
const (
	StatusWinner    = "winner"
	StatusAlternate = "alternate"
	StatusForfeited = "forfeited"
)

// Person is who did something, as an answer names them.
type Person struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Entry is one drawn place: Position is its place in the draw's order
// (1 first). The first Winners places won; the rest are alternates in
// order, of whom the first left becomes a winner when one forfeits.
type Entry struct {
	Position   int       `json:"position"`
	Status     string    `json:"status"`
	TicketID   uuid.UUID `json:"ticketId"`
	Name       string    `json:"name"`
	MaskedMail string    `json:"maskedEmail"`
	TicketType string    `json:"ticketType"`
	// PromotedAt: an alternate who became a winner.
	PromotedAt    *time.Time `json:"promotedAt"`
	ForfeitedAt   *time.Time `json:"forfeitedAt"`
	ForfeitReason *string    `json:"forfeitReason"`
	ForfeitedBy   *Person    `json:"forfeitedBy"`
	// email is the address MaskedMail masks; never answered.
	email string
}

// Raffle is one draw as it is kept.
type Raffle struct {
	ID             uuid.UUID `json:"id"`
	EventID        uuid.UUID `json:"eventId"`
	Title          string    `json:"title"`
	Criteria       Criteria  `json:"criteria"`
	EligibleCount  int       `json:"eligibleCount"`
	WinnerCount    int       `json:"winnerCount"`
	AlternateCount int       `json:"alternateCount"`
	DrawnAt        time.Time `json:"drawnAt"`
	DrawnBy        *Person   `json:"drawnBy"`
	ListHash       string    `json:"listHash"`
	Seed           string    `json:"seed"`
	Algorithm      string    `json:"algorithm"`
	Entries        []Entry   `json:"entries,omitempty"`
	// eligible are the eligible Tickets in their list's order: only the
	// verification answers them.
	eligible []uuid.UUID
}

// Summary is a raffle on the Event's list.
type Summary struct {
	ID             uuid.UUID `json:"id"`
	Title          string    `json:"title"`
	Criteria       Criteria  `json:"criteria"`
	EligibleCount  int       `json:"eligibleCount"`
	WinnerCount    int       `json:"winnerCount"`
	AlternateCount int       `json:"alternateCount"`
	DrawnAt        time.Time `json:"drawnAt"`
	DrawnBy        *Person   `json:"drawnBy"`
}

// Verification is what anyone needs to draw a raffle's order again.
type Verification struct {
	Algorithm string      `json:"algorithm"`
	Seed      string      `json:"seed"`
	ListHash  string      `json:"listHash"`
	TicketIDs []uuid.UUID `json:"ticketIds"`
	Order     []uuid.UUID `json:"order"`
}

// Excluded counts who an exclusion left out of a preview, each counted
// under the first that matched: past winners, organizers, door staff,
// co-organizers.
type Excluded struct {
	PastWinners  int `json:"pastWinners"`
	Organizers   int `json:"organizers"`
	DoorStaff    int `json:"doorStaff"`
	CoOrganizers int `json:"coOrganizers"`
}

// MaskedCandidate is an eligible Ticket as the preview lists it: a masked
// name.
type MaskedCandidate struct {
	Name       string `json:"name"`
	TicketType string `json:"ticketType"`
}

// Preview is how many may win under a Criteria, and who, masked.
type Preview struct {
	Eligible   int               `json:"eligible"`
	Excluded   Excluded          `json:"excluded"`
	Candidates []MaskedCandidate `json:"candidates"`
	Truncated  bool              `json:"truncated"`
}

// Errors the routes answer.
var (
	ErrNotFound      = errors.New("raffle: not found")
	ErrForbidden     = errors.New("raffle: forbidden")
	ErrTooFew        = errors.New("raffle: too few candidates")
	ErrNotWinner     = errors.New("raffle: entry is not a winner")
	ErrNoAlternates  = errors.New("raffle: no alternates left")
	ErrEventNotFound = errors.New("raffle: event not found")
)

// TooFewError is ErrTooFew with the eligible count.
type TooFewError struct{ Eligible int }

func (e *TooFewError) Error() string        { return ErrTooFew.Error() }
func (e *TooFewError) Is(target error) bool { return target == ErrTooFew }

// MaskName keeps each word's first two letters: "Ayşe Yılmaz" → "Ay***
// Yı***". An empty name stays empty.
func MaskName(name string) string {
	words := strings.Fields(name)
	for i, w := range words {
		r := []rune(w)
		if len(r) > 2 {
			r = r[:2]
		}
		words[i] = string(r) + "***"
	}
	return strings.Join(words, " ")
}

// MaskEmail keeps the first letter of the local part and of the domain and
// the top-level domain: "ayse@gmail.com" → "a***@g***.com".
func MaskEmail(email string) string {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	local, domain := []rune(email[:at]), email[at+1:]
	tld := ""
	if dot := strings.LastIndex(domain, "."); dot > 0 {
		tld = domain[dot:]
		domain = domain[:dot]
	}
	d := []rune(domain)
	if len(d) == 0 {
		return string(local[0]) + "***@***" + tld
	}
	return string(local[0]) + "***@" + string(d[0]) + "***" + tld
}
