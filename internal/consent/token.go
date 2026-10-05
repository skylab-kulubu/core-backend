package consent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// linkKind is what a signed link may do.
type linkKind byte

const (
	// linkWithdraw ends the open grant of the subject of a grant. It does
	// not expire: an invitation may be opened years after it was sent.
	linkWithdraw linkKind = 'w'
	// linkConfirm confirms a pending grant or renews an open one. It
	// expires.
	linkConfirm linkKind = 'c'
)

const (
	tokenVersion = 1
	payloadSize  = 1 + 1 + 16 + 8
	macSize      = 16
)

// Paths of the public pages the links open (docs/contact-consents.md).
const (
	WithdrawPath = "/v1/consents/withdraw"
	ConfirmPath  = "/v1/consents/confirm"
)

// sign makes a link token: the grant id, what it may do and when it stops
// working (zero: never), with a truncated HMAC-SHA256. It carries no address
// and no person; the grant id names a row only core can read.
func (c Config) sign(kind linkKind, id uuid.UUID, expires time.Time) string {
	payload := make([]byte, payloadSize, payloadSize+macSize)
	payload[0] = tokenVersion
	payload[1] = byte(kind)
	copy(payload[2:18], id[:])
	if !expires.IsZero() {
		binary.BigEndian.PutUint64(payload[18:], uint64(expires.Unix()))
	}
	return base64.RawURLEncoding.EncodeToString(append(payload, c.linkMAC(payload)...))
}

func (c Config) linkMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.derive("contact-consent link v1"))
	mac.Write(payload)
	return mac.Sum(nil)[:macSize]
}

// verify reads a link token of kind at now.
func (c Config) verify(token string, kind linkKind, now time.Time) (uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != payloadSize+macSize {
		return uuid.Nil, ErrLink
	}
	payload, sum := raw[:payloadSize], raw[payloadSize:]
	if !hmac.Equal(sum, c.linkMAC(payload)) || payload[0] != tokenVersion || linkKind(payload[1]) != kind {
		return uuid.Nil, ErrLink
	}
	var id uuid.UUID
	copy(id[:], payload[2:18])
	if exp := binary.BigEndian.Uint64(payload[18:]); exp != 0 && now.Unix() > int64(exp) {
		return id, ErrLinkExpired
	}
	return id, nil
}

// WithdrawURL is the withdraw page of a grant's subject: what an invitation's
// footer links to and its List-Unsubscribe header names (RFC 8058).
func (c Config) WithdrawURL(id uuid.UUID) string {
	return c.LinkOrigin + WithdrawPath + "?token=" + url.QueryEscape(c.sign(linkWithdraw, id, time.Time{}))
}

// confirmURL is the confirm page of a grant, working until expires.
func (c Config) confirmURL(id uuid.UUID, expires time.Time) string {
	return c.LinkOrigin + ConfirmPath + "?token=" + url.QueryEscape(c.sign(linkConfirm, id, expires))
}
