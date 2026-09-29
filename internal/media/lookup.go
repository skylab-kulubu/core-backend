package media

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The address lookup (media redesign ticket 28): Skyforms and CMS content
// keep the CDN addresses of Media uploaded before stage 5. The uuid in such
// an address is the object's key, not the Media's id, so a product asks
// core which Media an address names before it stores the id and attaches
// it.

// AddressMatch is what the lookup answers for one address: the Media it
// names, when the calling product may link that Media, or nothing, the same
// as an address that names no Media.
type AddressMatch struct {
	// Address is the address as the product sent it.
	Address string `json:"address"`
	// MediaID, Purpose and Status are nil when the address names no Media
	// the product may link.
	MediaID *uuid.UUID `json:"mediaId"`
	Purpose *string    `json:"purpose"`
	Status  *Status    `json:"status"`
	// Linkable reports whether the Media may take a new Media attachment
	// now: it is not archived, no purge has started and its expiry has not
	// passed. Always false without a MediaID.
	Linkable bool `json:"linkable"`
}

// KeyMatch is a Media the store found by its lookup key, and whether the
// asking product already holds a Media attachment to it.
type KeyMatch struct {
	LookupKey string
	Media     Media
	Held      bool
}

// MaxLookupAddresses is the most addresses one lookup takes.
const MaxLookupAddresses = 100

// ErrLookupTooMany refuses a lookup of more than MaxLookupAddresses
// addresses.
var ErrLookupTooMany = fmt.Errorf("media: a lookup takes at most %d addresses: %w", MaxLookupAddresses, ErrInvalid)

// LookUp answers, in the order given, which Media each stored address
// names, for the calling product's service account: the one that may
// attach. A Media the product may not link is answered like an address that
// names none.
func (s *service) LookUp(ctx context.Context, p authz.Principal, addresses []string) ([]AddressMatch, error) {
	product, err := s.attachingProduct(p, authz.Create)
	if err != nil {
		return nil, err
	}
	switch {
	case addresses == nil:
		return nil, ErrInvalid
	case len(addresses) > MaxLookupAddresses:
		return nil, ErrLookupTooMany
	}
	// keys[i] is the lookup key addresses[i] names, "" for none.
	keys := make([]string, len(addresses))
	wanted := make([]string, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for i, address := range addresses {
		key, ok := s.addresses.lookupKey(address)
		if !ok {
			continue
		}
		keys[i] = key
		if !seen[key] {
			seen[key] = true
			wanted = append(wanted, key)
		}
	}
	found, err := s.media.LookUpKeys(ctx, wanted, product)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	answers := make(map[string]AddressMatch, len(found))
	for _, match := range found {
		if _, answered := answers[match.LookupKey]; answered {
			continue
		}
		m := match.Media
		// Stage 5 links a stored address's Media for the person who
		// uploaded it (onBehalfOf = uploadedBy), so the rule is asked for
		// that person.
		may, err := s.linkRule(product, m, m.UploadedBy, func() (bool, error) { return match.Held, nil })
		if err != nil {
			return nil, err
		}
		if !may {
			answers[match.LookupKey] = AddressMatch{}
			continue
		}
		purpose, status := m.Purpose, m.Status
		answers[match.LookupKey] = AddressMatch{MediaID: &m.ID, Purpose: &purpose, Status: &status, Linkable: linkable(m, now)}
	}
	out := make([]AddressMatch, len(addresses))
	resolved := 0
	for i, address := range addresses {
		out[i] = answers[keys[i]]
		out[i].Address = address
		if out[i].MediaID != nil {
			resolved++
		}
	}
	// Counts only: an address or a Media id is the product's data.
	log.Printf("media lookup by %s: %d addresses, %d resolved", product, len(addresses), resolved)
	return out, nil
}

// maxLookupAddress is the longest address the lookup reads. A longer one
// names no Media: core never gave one.
const maxLookupAddress = 2048

// lookupKey is the lookup key (lookupKeyOf) of the Media an address names;
// ok is false for anything but a public address core gives a Media:
//
//   - under the configured base or the production CDN's (the base of a core
//     with none configured, such as the sandbox), over https or http, with
//     any query or fragment;
//   - of an image (images/<uuid>, an SVG images/<uuid>.svg), one of its
//     stored sizes (images/<uuid>/card.jpg) or a Cloudflare transformation
//     of it (cdn-cgi/image/<options>/images/<uuid>);
//   - of a file (files/<uuid>);
//   - of a video (videos/<uuid>.mp4) or its faststart copy
//     (videos/<uuid>.fs.<claim>.mp4), both the video's.
//
// A private key (private/…) and a Direct upload's pending one (pending/…)
// are never public, so they name nothing.
func (a Addresses) lookupKey(address string) (string, bool) {
	if len(address) > maxLookupAddress {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawPath != "" {
		return "", false
	}
	for _, base := range []string{a.publicBase(), DefaultPublicBase} {
		if rest, ok := underBase(u, base); ok {
			return objectLookupKey(rest)
		}
	}
	return "", false
}

// publicBase is the base addresses are built from: Base, or the process's
// configured one.
func (a Addresses) publicBase() string {
	if base := strings.TrimSpace(a.Base); base != "" {
		return base
	}
	return configuredPublicBase()
}

// underBase is the path of u under base (its host, any port and path),
// without the slash between them.
func underBase(u *url.URL, base string) (string, bool) {
	b, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || b.Host == "" || !strings.EqualFold(u.Hostname(), b.Hostname()) || u.Port() != b.Port() {
		return "", false
	}
	return strings.CutPrefix(u.Path, b.Path+"/")
}

// cloudflareImagePrefix starts the path of a Cloudflare image
// transformation (AddressCloudflare): cdn-cgi/image/<options>/<key>.
const cloudflareImagePrefix = "cdn-cgi/image/"

// objectLookupKey is the lookup key of the Media whose object, or one of
// whose sizes, is at the path.
func objectLookupKey(path string) (string, bool) {
	if transformed, ok := strings.CutPrefix(path, cloudflareImagePrefix); ok {
		// Only an image's original is transformed.
		options, source, ok := strings.Cut(transformed, "/")
		if !ok || options == "" || !strings.HasPrefix(source, "images/") {
			return "", false
		}
		path = source
	}
	switch {
	case strings.HasPrefix(path, "images/"):
		name := strings.TrimPrefix(path, "images/")
		if image, size, isSize := strings.Cut(name, "/"); isSize {
			if !isKeyUUID(image) || !isSizeObjectName(size) {
				return "", false
			}
			return "images/" + image, true
		}
		return path, isKeyUUID(strings.TrimSuffix(name, svgKeySuffix))
	case strings.HasPrefix(path, "files/"):
		return path, isKeyUUID(strings.TrimPrefix(path, "files/"))
	case strings.HasPrefix(path, videoKeyPrefix):
		key := lookupKeyOf(path)
		name, isVideo := strings.CutSuffix(strings.TrimPrefix(key, videoKeyPrefix), videoKeySuffix)
		return key, isVideo && isKeyUUID(name)
	}
	return "", false
}

// svgKeySuffix ends an SVG's key.
const svgKeySuffix = ".svg"

// isKeyUUID reports whether s is a uuid as core writes one into a key:
// lowercase, with hyphens.
func isKeyUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// isSizeObjectName reports whether name is how a stored size is named
// beside its image (sizeObjectKey): card or page, as a JPEG or a PNG.
func isSizeObjectName(name string) bool {
	size, extension, ok := strings.Cut(name, ".")
	return ok && slices.Contains(imageSizes, size) && (extension == "jpg" || extension == "png")
}

// faststartCopyKeyPattern matches the key of a video's faststart copy as core
// writes it (faststartCopyKey), and names its original's part.
var faststartCopyKeyPattern = regexp.MustCompile(`^(videos/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.fs\.[0-9a-f]{32}\.mp4$`)

// lookupKeyOf is the key a Media stored at key is looked up by: its
// original's, for a video's faststart copy, and key itself for anything
// else. The database's copy is media_lookup_key (migration
// 20260929180000), which the lookup's index is built on; a test keeps the
// two equal.
func lookupKeyOf(key string) string {
	return faststartCopyKeyPattern.ReplaceAllString(key, "$1.mp4")
}
