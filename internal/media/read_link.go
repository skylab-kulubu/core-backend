package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

// ReadLinkTTL is how long a read link opens its Media.
const ReadLinkTTL = 5 * time.Minute

var (
	// ErrLinkForbidden refuses a read link to a caller that is neither a
	// product's service account with the media:attach role nor a privileged
	// admin.
	ErrLinkForbidden = fmt.Errorf("media: only a product's service account or an admin may ask for read links: %w", ErrForbidden)
	// ErrLinkSubjectInactive refuses a read link for a person who is not an
	// active account: unknown to core, or being erased.
	ErrLinkSubjectInactive = fmt.Errorf("media: the read link names no active account: %w", ErrInvalid)
	// ErrLinkInvalid is a read link token core did not sign for this Media.
	ErrLinkInvalid = errors.New("media: the read link is not valid")
	// ErrLinkExpired is a read link past its five minutes.
	ErrLinkExpired = errors.New("media: the read link has expired")
)

// ReadLinkRequest is a read link request as its caller sent it.
type ReadLinkRequest struct {
	// OnBehalfOf is the person the link is for, as sent; nil when the
	// request names no one.
	OnBehalfOf *string
	// Malformed is a request that could not be read: not JSON, or an
	// onBehalfOf that is not a string.
	Malformed bool
}

// ReadLink is a link to a private Media's content that opens it for
// ReadLinkTTL.
type ReadLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Content is a private Media's decrypted content as a read link opens it.
// The caller closes Body.
type Content struct {
	Name string
	Type string
	Size int64
	Body io.ReadCloser
}

// ReadLinkRecord is one read link in the access log: which Media, for which
// product and the person it acted for, when it was issued and until when it
// opens, and every time it was opened.
type ReadLinkRecord struct {
	ID         uuid.UUID
	MediaID    uuid.UUID
	Product    authz.Product
	OnBehalfOf uuid.UUID
	IssuedAt   time.Time
	ExpiresAt  time.Time
	Opens      []ReadLinkOpen
}

// ReadLinkOpen is one successful open of a read link: when, and the
// opener's address as the trusted proxies report it.
type ReadLinkOpen struct {
	LinkID   uuid.UUID
	OpenedAt time.Time
	ClientIP string
}

// AccessLog is the access log of private Media. A link is only handed out
// once its row is written, and content only once its open is.
type AccessLog interface {
	// RecordReadLink is ErrLinkSubjectInactive when the link names a person
	// who is not an active account.
	RecordReadLink(ctx context.Context, link ReadLinkRecord) error
	RecordReadLinkOpen(ctx context.Context, open ReadLinkOpen) error
}

// IssueReadLink gives a read link to a private Media:
//   - to the owning product's service account, for one of the product's
//     purposes, for the person it acts for (the reviewer): the product has
//     decided that person may open it;
//   - to a privileged admin, for a core purpose (a certificate asset), the
//     admin being the person the link is for.
//
// Core records every link before it hands it out.
func (s *service) IssueReadLink(ctx context.Context, p authz.Principal, id uuid.UUID, req ReadLinkRequest) (ReadLink, error) {
	if s.private == nil {
		return ReadLink{}, ErrPrivateMediaDisabled
	}
	switch {
	case s.authz.Allow(p, authz.Resource{Type: authz.TypeMediaReadLink}, authz.Create):
		if id == uuid.Nil || req.Malformed || req.OnBehalfOf == nil {
			return ReadLink{}, ErrInvalid
		}
		person, err := uuid.Parse(*req.OnBehalfOf)
		if err != nil || person == uuid.Nil {
			return ReadLink{}, ErrInvalid
		}
		return s.issueReadLink(ctx, id, p.Product, person)
	case s.authz.Allow(p, authz.Resource{Type: authz.TypeMediaReadLink}, authz.Read):
		// The admin is the person the link is for; naming anyone, even
		// no one ("" or null), is a malformed request.
		admin := lifecycle.ActorID(p.ID)
		if id == uuid.Nil || admin == nil || req.Malformed || req.OnBehalfOf != nil {
			return ReadLink{}, ErrInvalid
		}
		return s.issueReadLink(ctx, id, authz.ProductCore, *admin)
	default:
		return ReadLink{}, ErrLinkForbidden
	}
}

// linkableMedia is a current private Media of a purpose the product owns.
// Any other Media is refused as one that does not exist.
func (s *service) linkableMedia(ctx context.Context, id uuid.UUID, product authz.Product) (Media, error) {
	m, err := s.media.Get(ctx, id)
	if err != nil {
		return Media{}, err
	}
	purpose, known := s.catalogue.Lookup(m.Purpose)
	if m.Visibility != VisibilityPrivate || m.Encryption == nil || !known || purpose.OwningProduct() != product {
		return Media{}, ErrNotFound
	}
	return m, nil
}

func (s *service) issueReadLink(ctx context.Context, id uuid.UUID, product authz.Product, onBehalfOf uuid.UUID) (ReadLink, error) {
	m, err := s.linkableMedia(ctx, id, product)
	if err != nil {
		return ReadLink{}, err
	}
	// Whole seconds, as the token carries the expiry.
	issued := s.private.now().UTC().Truncate(time.Second)
	record := ReadLinkRecord{
		ID: uuid.New(), MediaID: m.ID, Product: product, OnBehalfOf: onBehalfOf,
		IssuedAt: issued, ExpiresAt: issued.Add(ReadLinkTTL),
	}
	if err := s.private.AccessLog.RecordReadLink(ctx, record); err != nil {
		return ReadLink{}, err
	}
	token := signReadLink(s.private.LinkKey, readLinkClaims{linkID: record.ID, mediaID: m.ID, expires: record.ExpiresAt})
	return ReadLink{
		URL:       strings.TrimRight(s.private.LinkOrigin, "/") + "/v1/media/" + m.ID.String() + "/content?token=" + token,
		ExpiresAt: record.ExpiresAt,
	}, nil
}

// OpenContent checks a read link's token and opens the private Media's
// decrypted content. The open is written to the access log with the
// opener's address before any byte is returned.
func (s *service) OpenContent(ctx context.Context, id uuid.UUID, token, clientIP string) (Content, error) {
	if s.private == nil {
		return Content{}, ErrPrivateMediaDisabled
	}
	now := s.private.now().UTC()
	claims, err := verifyReadLink(s.private.LinkKey, token)
	if err != nil {
		return Content{}, err
	}
	if claims.mediaID != id {
		return Content{}, ErrLinkInvalid
	}
	if !now.Before(claims.expires) {
		return Content{}, ErrLinkExpired
	}
	m, err := s.media.Get(ctx, id)
	if err != nil {
		return Content{}, err
	}
	sealed, ok := m.Sealed()
	if !ok {
		return Content{}, ErrNotFound
	}
	body, err := s.private.Storage.Open(ctx, sealed)
	if err != nil {
		return Content{}, err
	}
	if err := s.private.AccessLog.RecordReadLinkOpen(ctx, ReadLinkOpen{LinkID: claims.linkID, OpenedAt: now, ClientIP: clientIP}); err != nil {
		body.Close()
		return Content{}, err
	}
	return Content{Name: m.Name, Type: m.Type, Size: m.Size, Body: body}, nil
}

// The read link token is two unpadded base64url parts joined by a dot:
//
//	claims     format version 0x01 | disposition 0x01 (attachment) | link id (16) | Media id (16) | expiry, Unix seconds, int64 big endian
//	signature  HMAC-SHA256 under MEDIA_LINK_SIGNING_KEY of readLinkDomain followed by the claims
//
// The domain keeps the key from signing anything but a read link, and the
// link id ties every open to the access log row of its link.
const (
	readLinkDomain      = "skylab-core-media-read-link:v1\x00"
	readLinkVersion     = 1
	dispositionDownload = 1
	readLinkClaimsSize  = 2 + 16 + 16 + 8
)

type readLinkClaims struct {
	linkID  uuid.UUID
	mediaID uuid.UUID
	expires time.Time
}

func signReadLink(key []byte, c readLinkClaims) string {
	claims := make([]byte, 0, readLinkClaimsSize)
	claims = append(claims, readLinkVersion, dispositionDownload)
	claims = append(claims, c.linkID[:]...)
	claims = append(claims, c.mediaID[:]...)
	claims = binary.BigEndian.AppendUint64(claims, uint64(c.expires.Unix()))
	return base64.RawURLEncoding.EncodeToString(claims) + "." + base64.RawURLEncoding.EncodeToString(readLinkMAC(key, claims))
}

func readLinkMAC(key, claims []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(readLinkDomain))
	mac.Write(claims)
	return mac.Sum(nil)
}

func verifyReadLink(key []byte, token string) (readLinkClaims, error) {
	encodedClaims, encodedMAC, ok := strings.Cut(token, ".")
	if !ok {
		return readLinkClaims{}, ErrLinkInvalid
	}
	claims, err := base64.RawURLEncoding.DecodeString(encodedClaims)
	if err != nil || len(claims) != readLinkClaimsSize {
		return readLinkClaims{}, ErrLinkInvalid
	}
	mac, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil || !hmac.Equal(mac, readLinkMAC(key, claims)) {
		return readLinkClaims{}, ErrLinkInvalid
	}
	if claims[0] != readLinkVersion || claims[1] != dispositionDownload {
		return readLinkClaims{}, ErrLinkInvalid
	}
	var c readLinkClaims
	copy(c.linkID[:], claims[2:18])
	copy(c.mediaID[:], claims[18:34])
	c.expires = time.Unix(int64(binary.BigEndian.Uint64(claims[34:])), 0).UTC()
	return c, nil
}

// now is the clock read links are issued and checked by.
func (p *PrivateMedia) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
