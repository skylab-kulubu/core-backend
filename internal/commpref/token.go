package commpref

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// UnsubscribePath is the public page of the link in every News mail.
const UnsubscribePath = "/v1/communication-preferences/unsubscribe"

// MaxLinkCategories is the most categories one link carries: a person may
// enter one send's audience through several teams, and one click turns
// every one of them off.
const MaxLinkCategories = 32

const (
	// formatOne is a token of key version 1: no key id follows. Every link
	// mailed before the key ring is one, and version 1 still signs them.
	formatOne = 1
	// formatTwo is a token of a newer key version: its key id follows.
	formatTwo = 2
	tokenKind = 'u'
	// fieldsSize is what follows the format (and key id): the kind, the
	// channel, the person and the number of categories.
	fieldsSize = 1 + 1 + 16 + 1
	macSize    = 16
	// maxTokenSize bounds what verify reads before the MAC.
	maxTokenSize = 2 + fieldsSize + MaxLinkCategories*(1+1+64) + macSize
)

// The tag of a category in a token.
const (
	tagClubAnnouncement byte = 1
	tagClubNews         byte = 2
	tagTeamAnnouncement byte = 3
	tagTeamNews         byte = 4
)

// channelTags are the channels' bytes in a token. A tag is never reused.
var channelTags = map[Channel]byte{ChannelEmail: 1}

// UnsubscribeURL is the link that turns exactly cats off on channel ch for
// person: what a News mail's footer links to and its List-Unsubscribe
// header names (RFC 8058). It does not expire: a mail may be opened years
// later. The token is the permission; it carries the person's id, the
// channel and the categories, signed with the current key version (and
// that version, from version 2), and no address. Never log it.
func (c Config) UnsubscribeURL(person uuid.UUID, ch Channel, cats []Category) (string, error) {
	token, err := c.sign(person, ch, cats)
	if err != nil {
		return "", err
	}
	return c.LinkOrigin + UnsubscribePath + "?t=" + url.QueryEscape(token), nil
}

func (c Config) sign(person uuid.UUID, ch Channel, cats []Category) (string, error) {
	if !c.Enabled {
		return "", ErrDisabled
	}
	cats = normalize(cats)
	chTag, ok := channelTags[ch]
	if person == uuid.Nil || !ok || len(cats) == 0 || len(cats) > MaxLinkCategories {
		return "", ErrInvalid
	}
	payload := make([]byte, 0, maxTokenSize)
	if c.current == 1 {
		payload = append(payload, formatOne)
	} else {
		payload = append(payload, formatTwo, c.current)
	}
	payload = append(payload, tokenKind, chTag)
	payload = append(payload, person[:]...)
	payload = append(payload, byte(len(cats)))
	for _, cat := range cats {
		if !cat.Valid() || cat.Scope == ScopeAll {
			return "", ErrInvalid
		}
		payload = append(payload, tag(cat))
		if cat.Scope == ScopeTeam {
			payload = append(payload, byte(len(cat.GroupID)))
			payload = append(payload, cat.GroupID...)
		}
	}
	return base64.RawURLEncoding.EncodeToString(append(payload, linkMAC(c.keys[c.current], payload)...)), nil
}

// normalize sorts cats and drops repeats.
func normalize(cats []Category) []Category {
	out := slices.Clone(cats)
	slices.SortFunc(out, func(a, b Category) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}

func tag(c Category) byte {
	switch {
	case c.Scope == ScopeClub && c.Kind == KindAnnouncement:
		return tagClubAnnouncement
	case c.Scope == ScopeClub:
		return tagClubNews
	case c.Kind == KindAnnouncement:
		return tagTeamAnnouncement
	}
	return tagTeamNews
}

// linkMAC is the MAC of payload under one ring key. The key is the
// version's own, so the purpose stays the same in every version; the MAC
// covers the format and the key id.
func linkMAC(key []byte, payload []byte) []byte {
	mac := hmac.New(sha256.New, derive(key, linkPurpose))
	mac.Write(payload)
	return mac.Sum(nil)[:macSize]
}

// readToken reads a link token: the person, the channel and the
// categories it turns off.
func (c Config) readToken(token string) (uuid.UUID, Channel, []Category, error) {
	if !c.Enabled {
		return uuid.Nil, "", nil, ErrDisabled
	}
	if token == "" || base64.RawURLEncoding.DecodedLen(len(token)) > maxTokenSize {
		return uuid.Nil, "", nil, ErrLink
	}
	// Strict: one spelling per token, no stray padding bits.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < 1+fieldsSize+macSize {
		return uuid.Nil, "", nil, ErrLink
	}
	// The format and the key id pick the ring key; the MAC then covers
	// both. Format 2 never names version 1: a token has one spelling.
	var key []byte
	start := 1
	switch raw[0] {
	case formatOne:
		key = c.keys[1]
	case formatTwo:
		if raw[1] > 1 {
			key = c.keys[raw[1]]
		}
		start = 2
	}
	if key == nil || len(raw) < start+fieldsSize+macSize {
		return uuid.Nil, "", nil, ErrLink
	}
	payload, sum := raw[:len(raw)-macSize], raw[len(raw)-macSize:]
	if !hmac.Equal(sum, linkMAC(key, payload)) || payload[start] != tokenKind {
		return uuid.Nil, "", nil, ErrLink
	}
	fields := payload[start:]
	var ch Channel
	for channel, tag := range channelTags {
		if tag == fields[1] {
			ch = channel
		}
	}
	person, err := uuid.FromBytes(fields[2:18])
	if ch == "" || err != nil || person == uuid.Nil {
		return uuid.Nil, "", nil, ErrLink
	}
	n := int(fields[18])
	rest := fields[fieldsSize:]
	cats := make([]Category, 0, n)
	for range n {
		cat, tail, err := readCategory(rest)
		if err != nil {
			return uuid.Nil, "", nil, ErrLink
		}
		cats = append(cats, cat)
		rest = tail
	}
	if n == 0 || n > MaxLinkCategories || len(rest) != 0 {
		return uuid.Nil, "", nil, ErrLink
	}
	return person, ch, cats, nil
}

var errShort = errors.New("short")

func readCategory(b []byte) (Category, []byte, error) {
	if len(b) == 0 {
		return Category{}, nil, errShort
	}
	var cat Category
	switch b[0] {
	case tagClubAnnouncement:
		return Club(KindAnnouncement), b[1:], nil
	case tagClubNews:
		return Club(KindNews), b[1:], nil
	case tagTeamAnnouncement:
		cat.Kind = KindAnnouncement
	case tagTeamNews:
		cat.Kind = KindNews
	default:
		return Category{}, nil, ErrLink
	}
	if len(b) < 2 || len(b) < 2+int(b[1]) {
		return Category{}, nil, errShort
	}
	cat.Scope, cat.GroupID = ScopeTeam, string(b[2:2+int(b[1])])
	if !cat.Valid() {
		return Category{}, nil, ErrLink
	}
	return cat, b[2+int(b[1]):], nil
}
