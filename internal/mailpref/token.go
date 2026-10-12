package mailpref

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
const UnsubscribePath = "/v1/mail-preferences/unsubscribe"

// MaxLinkCategories is the most categories one link carries: a person may
// enter one send's audience through several teams, and one click turns
// every one of them off.
const MaxLinkCategories = 32

const (
	tokenVersion = 1
	tokenKind    = 'u'
	headerSize   = 1 + 1 + 16 + 1
	macSize      = 16
	// maxTokenSize bounds what verify reads before the MAC.
	maxTokenSize = headerSize + MaxLinkCategories*(1+1+64) + macSize
)

// The tag of a category in a token.
const (
	tagClubAnnouncement byte = 1
	tagClubNews         byte = 2
	tagTeamAnnouncement byte = 3
	tagTeamNews         byte = 4
)

// UnsubscribeURL is the link that turns exactly cats off for person: what a
// News mail's footer links to and its List-Unsubscribe header names (RFC
// 8058). It does not expire: a mail may be opened years later. The token is
// the permission; it carries the person's id and the categories, signed,
// and no address. Never log it.
func (c Config) UnsubscribeURL(person uuid.UUID, cats []Category) (string, error) {
	token, err := c.sign(person, cats)
	if err != nil {
		return "", err
	}
	return c.LinkOrigin + UnsubscribePath + "?t=" + url.QueryEscape(token), nil
}

func (c Config) sign(person uuid.UUID, cats []Category) (string, error) {
	if !c.Enabled {
		return "", ErrDisabled
	}
	cats = normalize(cats)
	if person == uuid.Nil || len(cats) == 0 || len(cats) > MaxLinkCategories {
		return "", ErrInvalid
	}
	payload := make([]byte, 0, maxTokenSize)
	payload = append(payload, tokenVersion, tokenKind)
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
	return base64.RawURLEncoding.EncodeToString(append(payload, c.linkMAC(payload)...)), nil
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

func (c Config) linkMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.derive(linkPurpose))
	mac.Write(payload)
	return mac.Sum(nil)[:macSize]
}

// readToken reads a link token: the person and the categories it turns off.
func (c Config) readToken(token string) (uuid.UUID, []Category, error) {
	if !c.Enabled {
		return uuid.Nil, nil, ErrDisabled
	}
	if token == "" || base64.RawURLEncoding.DecodedLen(len(token)) > maxTokenSize {
		return uuid.Nil, nil, ErrLink
	}
	// Strict: one spelling per token, no stray padding bits.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < headerSize+macSize {
		return uuid.Nil, nil, ErrLink
	}
	payload, sum := raw[:len(raw)-macSize], raw[len(raw)-macSize:]
	if !hmac.Equal(sum, c.linkMAC(payload)) || payload[0] != tokenVersion || payload[1] != tokenKind {
		return uuid.Nil, nil, ErrLink
	}
	person, err := uuid.FromBytes(payload[2:18])
	if err != nil || person == uuid.Nil {
		return uuid.Nil, nil, ErrLink
	}
	n := int(payload[18])
	rest := payload[headerSize:]
	cats := make([]Category, 0, n)
	for range n {
		cat, tail, err := readCategory(rest)
		if err != nil {
			return uuid.Nil, nil, ErrLink
		}
		cats = append(cats, cat)
		rest = tail
	}
	if n == 0 || n > MaxLinkCategories || len(rest) != 0 {
		return uuid.Nil, nil, ErrLink
	}
	return person, cats, nil
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
