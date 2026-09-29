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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The address lookup (media redesign ticket 28): Skyforms and CMS content
// keep the CDN addresses of Media uploaded before stage 5. The uuid in such
// an address is the object's key, not the Media's id, so a product asks
// core which Media an address names before it stores the id and attaches
// it.

// AddressMatch is what the lookup answers for one address: the Media it
// names, when the calling product may link that Media for the person it
// acts for, or nothing, the same as an address that names no Media.
type AddressMatch struct {
	// Address is the address as the product sent it.
	Address string `json:"address"`
	// MediaID, Purpose and Status are nil when the address names no Media
	// the product may link for the person.
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

// LookupRequest is what a product asks the address lookup: the addresses a
// record of its keeps, for the person it acts for.
type LookupRequest struct {
	// OnBehalfOf is the person the product acts for, as when it attaches:
	// the respondent whose Skyforms answer it is, the editor saving the CMS
	// page.
	OnBehalfOf uuid.UUID
	Addresses  []string
}

// LookUp answers, in the order given, which Media each stored address
// names, for the calling product's service account (the one that may
// attach), acting for req.OnBehalfOf. An address names a Media only when
// the service attach API would let the product link that Media for that
// person (linkRule); any other is answered like an address that names none.
//
// The caller is authorized before read is called: a person's request is
// never read.
func (s *service) LookUp(ctx context.Context, p authz.Principal, read func() (LookupRequest, error)) ([]AddressMatch, error) {
	product, err := s.attachingProduct(p, authz.Create)
	if err != nil {
		return nil, err
	}
	req, err := read()
	if err != nil {
		return nil, err
	}
	switch {
	case req.OnBehalfOf == uuid.Nil || req.Addresses == nil:
		return nil, ErrInvalid
	case len(req.Addresses) > MaxLookupAddresses:
		return nil, ErrLookupTooMany
	}
	catalogue := s.addresses.catalogue()
	found, err := resolveAddresses(ctx, s.media, s.addresses, product, req.Addresses, func(m Media, held bool) bool {
		may, _ := linkRule(catalogue, product, m, req.OnBehalfOf, func() (bool, error) { return held, nil })
		return may
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]AddressMatch, len(req.Addresses))
	resolved := 0
	for i, address := range req.Addresses {
		out[i].Address = address
		if m := found[i].media; m != nil {
			purpose, status := m.Purpose, m.Status
			out[i].MediaID, out[i].Purpose, out[i].Status = &m.ID, &purpose, &status
			out[i].Linkable = linkable(*m, now)
			resolved++
		}
	}
	// Counts only: an address, a Media id or a person is the product's
	// data.
	log.Printf("media lookup by %s: %d addresses, %d resolved", product, len(req.Addresses), resolved)
	return out, nil
}

// OperatorMatch is one row of the operator's address lookup
// (core-backend media-lookup): what an address names, without the file's
// name or who uploaded it.
type OperatorMatch struct {
	Address string
	// Recognised is false for an address that is none of core's public
	// Media addresses.
	Recognised bool
	// MediaID is nil when the address names no Media (for the product).
	MediaID *uuid.UUID
	Purpose string
	Status  Status
	// HasUploader is whether the Media has an uploader a product can
	// attach it for (onBehalfOf); an erased account leaves none.
	HasUploader bool
}

// LookUpForOperator answers, in order, which Media each address names
// under base (the process's configured base when empty), for an operator
// running a migration that does not know the uploader: every Media, or,
// with a product, only those it may link for their uploader or already
// holds, as stage 5 attaches them (onBehalfOf = uploadedBy). Nothing is
// logged.
func LookUpForOperator(ctx context.Context, store Store, base string, product authz.Product, addresses []string) ([]OperatorMatch, error) {
	a := Addresses{Base: base}
	catalogue := a.catalogue()
	may := func(Media, bool) bool { return true }
	if product != "" {
		may = func(m Media, held bool) bool {
			allowed, _ := linkRule(catalogue, product, m, m.UploadedBy, func() (bool, error) { return held, nil })
			return allowed
		}
	}
	found, err := resolveAddresses(ctx, store, a, product, addresses, may)
	if err != nil {
		return nil, err
	}
	out := make([]OperatorMatch, len(addresses))
	for i, address := range addresses {
		out[i] = OperatorMatch{Address: address, Recognised: found[i].recognised}
		if m := found[i].media; m != nil {
			out[i].MediaID, out[i].Purpose, out[i].Status, out[i].HasUploader = &m.ID, m.Purpose, m.Status, m.UploadedBy != uuid.Nil
		}
	}
	return out, nil
}

// resolution is what one address names: whether it is one of core's
// public Media addresses at all, and the Media it names that may allowed
// (nil for none).
type resolution struct {
	recognised bool
	media      *Media
}

// resolveAddresses answers, in order, the Media each address names that
// may allows, given whether the product holds a Media attachment to it.
// The store is read once for every MaxLookupAddresses keys. Were two Media
// to share a key, only the first the store answers (the current and
// newest) is asked about.
func resolveAddresses(ctx context.Context, store Store, a Addresses, product authz.Product, addresses []string, may func(m Media, held bool) bool) ([]resolution, error) {
	// keys[i] is the lookup key addresses[i] names, "" for none.
	keys := make([]string, len(addresses))
	wanted := make([]string, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for i, address := range addresses {
		key, ok := a.lookupKey(address)
		if !ok {
			continue
		}
		keys[i] = key
		if !seen[key] {
			seen[key] = true
			wanted = append(wanted, key)
		}
	}
	named := make(map[string]*Media, len(wanted))
	answered := make(map[string]bool, len(wanted))
	for batch := range slices.Chunk(wanted, MaxLookupAddresses) {
		found, err := store.LookUpKeys(ctx, batch, product)
		if err != nil {
			return nil, err
		}
		for _, match := range found {
			if answered[match.LookupKey] {
				continue
			}
			answered[match.LookupKey] = true
			if may(match.Media, match.Held) {
				m := match.Media
				named[match.LookupKey] = &m
			}
		}
	}
	out := make([]resolution, len(addresses))
	for i, key := range keys {
		out[i] = resolution{recognised: key != "", media: named[key]}
	}
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
//     of a raster original (cdn-cgi/image/<options>/images/<uuid>);
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
	if err != nil || b.Host == "" || !sameASCIIHost(u.Hostname(), b.Hostname()) || u.Port() != b.Port() {
		return "", false
	}
	return strings.CutPrefix(u.Path, b.Path+"/")
}

// sameASCIIHost reports whether two host names are the same, ignoring the
// case of ASCII letters only. A host with any other byte is no host core
// serves from: strings.EqualFold would take the long s (U+017F) for s and
// the Kelvin sign (U+212A) for k.
func sameASCIIHost(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= utf8.RuneSelf || y >= utf8.RuneSelf {
			return false
		}
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// cloudflareImagePrefix starts the path of a Cloudflare image
// transformation (AddressCloudflare): cdn-cgi/image/<options>/<key>.
const cloudflareImagePrefix = "cdn-cgi/image/"

// objectLookupKey is the lookup key of the Media whose object, or one of
// whose sizes, is at the path.
func objectLookupKey(path string) (string, bool) {
	if transformed, ok := strings.CutPrefix(path, cloudflareImagePrefix); ok {
		// Only a raster image's original is transformed (Addresses.Image):
		// never one of its stored sizes, never an SVG, which is its own
		// size.
		options, source, ok := strings.Cut(transformed, "/")
		name, isImage := strings.CutPrefix(source, "images/")
		if !ok || options == "" || !isImage || !isKeyUUID(name) {
			return "", false
		}
		return source, true
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
