// Package mailpref keeps the Announcement preference (Duyuru tercihi, ADR
// 0067): which News mail (duyuru, haber) a person wants, per source
// (club-wide or one team), with one master switch, and the signed one-click
// link of every such mail that turns its categories off. Only explicit
// choices are stored; everything else is computed from the current Keycloak
// team tree. See docs/mail-preferences.md.
package mailpref

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Kind is a News kind (Karar D6): each source has a switch for each.
type Kind string

const (
	KindAnnouncement Kind = "duyuru"
	KindNews         Kind = "haber"
)

// Kinds are the kinds in the order screens show them.
var Kinds = []Kind{KindAnnouncement, KindNews}

func (k Kind) valid() bool { return k == KindAnnouncement || k == KindNews }

// Scope is what a category covers.
type Scope string

const (
	// ScopeAll is the master switch: off, no duyuru or haber mail at all.
	ScopeAll Scope = "all"
	// ScopeClub is club-wide mail (the "all Members" audience).
	ScopeClub Scope = "club"
	// ScopeTeam is one team's mail, the team named by its Keycloak Group id.
	ScopeTeam Scope = "team"
)

// Category is one switch: the master switch, or a source times a kind.
// Its text form is "all", "club:<kind>" or "team:<groupId>:<kind>".
type Category struct {
	Scope   Scope
	GroupID string
	Kind    Kind
}

// Master is the master switch.
func Master() Category { return Category{Scope: ScopeAll} }

// Club is club-wide mail of kind k.
func Club(k Kind) Category { return Category{Scope: ScopeClub, Kind: k} }

// Team is the mail of kind k of the team whose Group id is groupID.
func Team(groupID string, k Kind) Category {
	return Category{Scope: ScopeTeam, GroupID: groupID, Kind: k}
}

// groupIDPattern is a Keycloak Group id (a UUID in production) or a test
// directory's id: no separator, no space, at most 64 characters.
var groupIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Valid reports whether c is one of the three shapes.
func (c Category) Valid() bool {
	switch c.Scope {
	case ScopeAll:
		return c.GroupID == "" && c.Kind == ""
	case ScopeClub:
		return c.GroupID == "" && c.Kind.valid()
	case ScopeTeam:
		return groupIDPattern.MatchString(c.GroupID) && c.Kind.valid()
	}
	return false
}

func (c Category) String() string {
	switch c.Scope {
	case ScopeAll:
		return string(ScopeAll)
	case ScopeClub:
		return string(ScopeClub) + ":" + string(c.Kind)
	}
	return string(ScopeTeam) + ":" + c.GroupID + ":" + string(c.Kind)
}

// source is c's source: c without its kind.
func (c Category) source() Category { return Category{Scope: c.Scope, GroupID: c.GroupID} }

// ParseCategory reads a category's text form.
func ParseCategory(raw string) (Category, error) {
	parts := strings.Split(raw, ":")
	var c Category
	switch {
	case len(parts) == 1 && parts[0] == string(ScopeAll):
		c = Master()
	case len(parts) == 2 && parts[0] == string(ScopeClub):
		c = Club(Kind(parts[1]))
	case len(parts) == 3 && parts[0] == string(ScopeTeam):
		c = Team(parts[1], Kind(parts[2]))
	default:
		return Category{}, ErrInvalid
	}
	if !c.Valid() {
		return Category{}, ErrInvalid
	}
	return c, nil
}

// Source is where an explicit choice was made.
type Source string

const (
	SourceAccountCenter   Source = "account_center"
	SourceUnsubscribeLink Source = "unsubscribe_link"
	SourceAdmin           Source = "admin"
)

// Setting is one explicit choice: a row.
type Setting struct {
	Category  Category
	Enabled   bool
	Source    Source
	UpdatedAt time.Time
}

// Change is one change of a person's choices. Enabled nil forgets the
// explicit choice: the category goes back to what it inherits or its
// default (a team no longer followed).
type Change struct {
	Category Category
	Enabled  *bool
}

var (
	// ErrDisabled is any preference route while KeyEnv is unset.
	ErrDisabled = errors.New("mail preference: off")
	// ErrInvalid is a request core cannot read.
	ErrInvalid = errors.New("mail preference: invalid")
	// ErrUnknownTeam is a team that does not exist or that the person may
	// not follow (a hidden team outside their own teams).
	ErrUnknownTeam = errors.New("mail preference: unknown team")
	// ErrLink is a link token that does not read.
	ErrLink = errors.New("mail preference: invalid link")
	// ErrSubjectInactive is a write for a person being erased or erased.
	ErrSubjectInactive = errors.New("mail preference: account subject is not active")
	// ErrTeamsUnavailable is Keycloak not answering the team tree.
	ErrTeamsUnavailable = errors.New("mail preference: teams unavailable")
)

// KeyEnv holds the link key: 32 random bytes, unpadded base64url (an OpenBao
// reference in production). Unset: preferences are off and their routes
// answer 503. It is its own key, not CONTACT_CONSENT_KEY: the consents stay
// off in production until the retention sweep runs, and a preference link
// must not depend on that. Do not rotate it: every link already mailed
// stops working.
const KeyEnv = "MAIL_PREFERENCE_KEY"

// AccountCenterURLEnv is the Account center page the unsubscribe page links
// to; DefaultAccountCenterURL when unset.
const (
	AccountCenterURLEnv     = "MAIL_PREFERENCE_ACCOUNT_CENTER_URL"
	DefaultAccountCenterURL = "https://my.yildizskylab.com"
)

// linkPurpose is the derivation purpose of the link MAC key.
const linkPurpose = "mail-preference unsubscribe link v1"

// Config is the preference settings.
type Config struct {
	// Enabled is false when KeyEnv is unset.
	Enabled bool
	key     []byte
	// LinkOrigin is core's public address, where the links point
	// (PUBLIC_API_ORIGIN).
	LinkOrigin string
	// AccountCenterURL is the Account center page the unsubscribe page
	// links to.
	AccountCenterURL string
}

// ConfigFromEnv reads the preference settings. Without KeyEnv preferences
// are off and nothing else is read. Errors name variables, never values.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	raw := strings.TrimSpace(getenv(KeyEnv))
	if raw == "" {
		return Config{}, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return Config{}, fmt.Errorf("%s must be an unpadded base64url-encoded 32-byte key", KeyEnv)
	}
	origin, err := httpURL(getenv("PUBLIC_API_ORIGIN"))
	if err != nil || origin == "" {
		return Config{}, fmt.Errorf("%s needs PUBLIC_API_ORIGIN, core's public http(s) address, for its links", KeyEnv)
	}
	center, err := httpURL(getenv(AccountCenterURLEnv))
	if err != nil {
		return Config{}, fmt.Errorf("%s must be an http(s) address", AccountCenterURLEnv)
	}
	if center == "" {
		center = DefaultAccountCenterURL
	}
	return Config{Enabled: true, key: key, LinkOrigin: origin, AccountCenterURL: center}, nil
}

// httpURL reads an absolute http(s) address without its trailing slash; ""
// when raw is blank.
func httpURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("not an http(s) address")
	}
	return raw, nil
}

// TestConfig is a Config for tests and tools: key must be 32 bytes.
func TestConfig(key []byte, origin string) Config {
	return Config{
		Enabled: true, key: key, LinkOrigin: strings.TrimRight(origin, "/"),
		AccountCenterURL: DefaultAccountCenterURL,
	}
}

func (c Config) derive(purpose string) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}
