// Package commpref keeps the communication preferences: for each channel
// (e-mail today; the in-app inbox, push and SMS later), which News
// (duyuru, haber) a person wants, per source (club-wide or one team), with
// one master switch per channel (the Duyuru tercihi of ADR 0067), and the
// signed one-click link of every such mail that turns its categories off.
// Only explicit choices are stored; everything else is computed from the
// current Keycloak team tree. Transactional messages never read them. See
// docs/communication-preferences.md.
package commpref

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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

// Channel is how a message reaches a person. A preference is kept per
// channel, so a person may keep a category on one channel and turn it off
// on another.
type Channel string

const (
	ChannelEmail Channel = "email"
)

// Channels are the channels core keeps preferences for, in the order
// screens show them. A new one (inbox, push, sms) is added here and to the
// table's channel CHECK together.
var Channels = []Channel{ChannelEmail}

func (c Channel) valid() bool { return slices.Contains(Channels, c) }

// ParseChannel reads a channel's name.
func ParseChannel(raw string) (Channel, error) {
	if c := Channel(raw); c.valid() {
		return c, nil
	}
	return "", ErrInvalid
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
	Channel   Channel
	Category  Category
	Enabled   bool
	Source    Source
	UpdatedAt time.Time
}

// Change is one change of a person's choices. Enabled nil forgets the
// explicit choice: the category goes back to what it inherits or its
// default (a team no longer followed).
type Change struct {
	Channel  Channel
	Category Category
	Enabled  *bool
}

var (
	// ErrDisabled is any preference route while KeyEnv is unset.
	ErrDisabled = errors.New("communication preference: off")
	// ErrInvalid is a request core cannot read.
	ErrInvalid = errors.New("communication preference: invalid")
	// ErrUnknownTeam is a team that does not exist or that the person may
	// not follow (a hidden team outside their own teams).
	ErrUnknownTeam = errors.New("communication preference: unknown team")
	// ErrFollowMembersOnly is someone in no /UYELER Group turning a team
	// on: only a Member follows (Karar D7).
	ErrFollowMembersOnly = errors.New("communication preference: only a Member follows a team")
	// ErrLink is a link token that does not read.
	ErrLink = errors.New("communication preference: invalid link")
	// ErrSubjectInactive is a write for a person being erased or erased.
	ErrSubjectInactive = errors.New("communication preference: account subject is not active")
	// ErrTeamsUnavailable is Keycloak not answering the team tree.
	ErrTeamsUnavailable = errors.New("communication preference: teams unavailable")
)

// KeyEnv holds version 1 of the link key: 32 random bytes, unpadded
// base64url (an OpenBao reference in production). It is its own key, not
// CONTACT_CONSENT_KEY: the consents stay off in production until the
// retention sweep runs, and a preference link must not depend on that.
// Newer versions are KeyVersionEnvPrefix<n>; with no version set at all
// preferences are off and their routes answer 503.
const KeyEnv = "COMMUNICATION_PREFERENCE_KEY"

// The key ring (abonelik-cikisi 13, ADR 0072 Consequences): a link carries
// the version of the key that signed it, so a new key does not break the
// links already mailed.
const (
	// KeyVersionEnvPrefix and a version from 2 to 255 name each newer key:
	// COMMUNICATION_PREFERENCE_KEY_V2, … Version 1 is KeyEnv, never
	// COMMUNICATION_PREFERENCE_KEY_V1.
	KeyVersionEnvPrefix = "COMMUNICATION_PREFERENCE_KEY_V"
	// CurrentKeyEnv is the version new links are signed with (1–255).
	// Unset: 1. Version 1 signs format 1 links, which carry no version, so
	// an image from before the ring reads them; a newer version signs
	// format 2.
	CurrentKeyEnv = "COMMUNICATION_PREFERENCE_KEY_CURRENT"
)

// AccountCenterURLEnv is the Account center page the unsubscribe page links
// to; DefaultAccountCenterURL when unset.
const (
	AccountCenterURLEnv     = "COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL"
	DefaultAccountCenterURL = "https://my.yildizskylab.com"
)

// linkPurpose is the derivation purpose of the link MAC key.
const linkPurpose = "communication-preference unsubscribe link v1"

// Config is the preference settings.
type Config struct {
	// Enabled is false when no key version is set.
	Enabled bool
	// keys are the ring: every version set, by version. A version whose
	// variable is gone (a leaked key) reads no link from the next start.
	keys map[byte][]byte
	// current signs new links.
	current byte
	// LinkOrigin is core's public address, where the links point
	// (PUBLIC_API_ORIGIN).
	LinkOrigin string
	// AccountCenterURL is the Account center page the unsubscribe page
	// links to.
	AccountCenterURL string
}

// ConfigFromEnv reads the preference settings. With no key version set
// preferences are off and nothing else is read. Errors name variables,
// never values; any error stops core.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	keys, current, err := keyRingFromEnv(getenv)
	if err != nil || len(keys) == 0 {
		return Config{}, err
	}
	origin, err := httpURL(getenv("PUBLIC_API_ORIGIN"))
	if err != nil || origin == "" {
		return Config{}, fmt.Errorf("%s needs PUBLIC_API_ORIGIN, core's public http(s) address, for its links", keyEnvOf(current))
	}
	center, err := httpURL(getenv(AccountCenterURLEnv))
	if err != nil {
		return Config{}, fmt.Errorf("%s must be an http(s) address", AccountCenterURLEnv)
	}
	if center == "" {
		center = DefaultAccountCenterURL
	}
	return Config{Enabled: true, keys: keys, current: current, LinkOrigin: origin, AccountCenterURL: center}, nil
}

// keyRingFromEnv reads KeyEnv (version 1), every KeyVersionEnvPrefix<n>
// and CurrentKeyEnv: the ring is exactly the versions set. A current
// version without its key (fail fast: core does not start, it never falls
// back to another version), a newer version without a current one, a key
// that is not 32 bytes, version 1 under the _V1 name, or two versions
// holding the same key is an error. Nothing set is an empty ring.
func keyRingFromEnv(getenv func(string) string) (map[byte][]byte, byte, error) {
	keys := map[byte][]byte{}
	if strings.TrimSpace(getenv(KeyVersionEnvPrefix+"1")) != "" {
		return nil, 0, fmt.Errorf("%s1 is not read: version 1 is %s", KeyVersionEnvPrefix, KeyEnv)
	}
	for version := 1; version <= 255; version++ {
		name := keyEnvOf(byte(version))
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			continue
		}
		key, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			return nil, 0, fmt.Errorf("%s must be an unpadded base64url-encoded 32-byte key", name)
		}
		for other, held := range keys {
			if hmac.Equal(held, key) {
				return nil, 0, fmt.Errorf("%s and %s hold the same key; every version needs its own", name, keyEnvOf(other))
			}
		}
		keys[byte(version)] = key
	}
	raw := strings.TrimSpace(getenv(CurrentKeyEnv))
	if raw == "" {
		if len(keys) > 0 && keys[1] == nil {
			return nil, 0, fmt.Errorf("%s must name the version new links are signed with", CurrentKeyEnv)
		}
		return keys, 1, nil
	}
	current, err := strconv.Atoi(raw)
	if err != nil || current < 1 || current > 255 {
		return nil, 0, fmt.Errorf("%s must be a version from 1 to 255", CurrentKeyEnv)
	}
	if keys[byte(current)] == nil {
		return nil, 0, fmt.Errorf("%s is %d but %s is not set", CurrentKeyEnv, current, keyEnvOf(byte(current)))
	}
	return keys, byte(current), nil
}

// keyEnvOf is the variable of a key version.
func keyEnvOf(version byte) string {
	if version == 1 {
		return KeyEnv
	}
	return KeyVersionEnvPrefix + strconv.Itoa(int(version))
}

// KeyVersions are the versions in the ring, for the startup log; never a
// key.
func (c Config) KeyVersions() []int {
	versions := make([]int, 0, len(c.keys))
	for version := range c.keys {
		versions = append(versions, int(version))
	}
	slices.Sort(versions)
	return versions
}

// CurrentKeyVersion is the version new links are signed with; 0 when off.
func (c Config) CurrentKeyVersion() int { return int(c.current) }

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

// AccountCenterLink is the Account center page for the category cat on
// channel ch: AccountCenterURL with ?kategori=<category>&kanal=<channel>,
// so the screen can show that row first (bilet 09). No token travels
// there: the person signs in to the Account center.
func (c Config) AccountCenterLink(ch Channel, cat Category) string {
	link, err := url.Parse(c.AccountCenterURL)
	if err != nil || !cat.Valid() || !ch.valid() {
		return c.AccountCenterURL
	}
	query := link.Query()
	query.Set("kategori", cat.String())
	query.Set("kanal", string(ch))
	link.RawQuery = query.Encode()
	return link.String()
}

// TestConfig is a Config for tests and tools: key, 32 bytes, is version 1
// and current.
func TestConfig(key []byte, origin string) Config {
	return Config{
		Enabled: true, keys: map[byte][]byte{1: key}, current: 1, LinkOrigin: strings.TrimRight(origin, "/"),
		AccountCenterURL: DefaultAccountCenterURL,
	}
}

// derive is the key of one purpose under a ring key.
func derive(key []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}
