package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The Media purposes core itself uploads as or links. The catalogue must
// define every one of them (requiredPurposes).
const (
	// PurposeLegacy is the Media purpose of Media uploaded without a purpose
	// and of every Media stored before Media purpose existed. It is internal:
	// a client cannot name it.
	PurposeLegacy = "legacy"
	// PurposeProfilePicture is a person's own profile picture.
	PurposeProfilePicture = "profile_picture"
	// PurposeEventCover is an Event's cover image.
	PurposeEventCover = "event_cover"
	// PurposeEventGallery is a photo in an Event's gallery.
	PurposeEventGallery = "event_gallery"
	// PurposeCertificateAsset is a certificate template's background or
	// image.
	PurposeCertificateAsset = "certificate_asset"
)

// requiredPurposes are the purposes core refers to in code. A catalogue that
// lacks one stops core at startup.
var requiredPurposes = []string{
	PurposeLegacy, PurposeProfilePicture, PurposeEventCover, PurposeEventGallery, PurposeCertificateAsset,
}

// ErrCatalogueInvalid is a catalogue file core cannot read as one: a field it
// does not know, a value outside the vocabulary, or a missing purpose that
// core refers to.
var ErrCatalogueInvalid = errors.New("media purpose catalogue: invalid")

// Catalogue is the Media purpose catalogue: every Media purpose core accepts
// and the rules each one fixes. The reviewed file is
// config/media-purposes.json.
type Catalogue struct {
	purposes map[string]Purpose
}

// Visibility is where a purpose's Media are served from.
type Visibility string

const (
	// VisibilityPublic Media are served from the CDN.
	VisibilityPublic Visibility = "public"
	// VisibilityPrivate Media are encrypted and never get a public address.
	VisibilityPrivate Visibility = "private"
)

// Attacher is who attaches the Media of a purpose to the records that use
// them.
type Attacher string

const (
	// AttachCore: a core record links the Media, and the database writes its
	// Media attachment.
	AttachCore Attacher = "core"
	// AttachService: another product attaches the Media through the service
	// attach API.
	AttachService Attacher = "service"
)

// serviceAttachAPI reports whether the service attach API exists. Until it
// does (media redesign ticket 03), nothing can attach a Media whose purpose
// another product attaches, so such a purpose cannot be uploaded.
const serviceAttachAPI = false

// Available reports whether something can attach Media of this purpose
// today. A Media nothing can attach would only wait for its expiry.
func (p Purpose) Available() bool {
	return p.Attach == AttachCore || serviceAttachAPI
}

// Transport is how a purpose's files reach storage.
type Transport string

const (
	// TransportSingleStep files come through core (POST /v1/media).
	TransportSingleStep Transport = "single_step"
	// TransportDirect files go straight to storage by Direct upload.
	TransportDirect Transport = "direct"
)

// Purpose is one Media purpose from the catalogue.
type Purpose struct {
	Name string
	// Uploader is who may upload Media of this purpose.
	Uploader authz.MediaUploader
	// Types are the content types the purpose accepts, detected from the
	// file's content, never from its name or declared type.
	Types      []string
	MaxBytes   int64
	Visibility Visibility
	Encrypted  bool
	Scan       bool
	// PendingTTL is how long a Media with no Media attachment is kept. Zero
	// keeps it: only the legacy purpose may, until Media uploaded without a
	// purpose fall to the strict rule.
	PendingTTL time.Duration
	Transport  Transport
	// Attach is who attaches the purpose's Media.
	Attach Attacher
	Image  ImageHandling
	// LegacyRules makes the purpose accept what Media uploaded without a
	// purpose were accepted as before Media purpose: see
	// docs/media-lifecycle.md.
	LegacyRules bool
}

// ImageHandling is what core does with a raster image of a purpose:
// re-encode it (Reencode) within MaxDimension (MaxImageDimension when 0),
// store its Variants beside it (size name, one of SizeCard and SizePage, to
// its longer side in pixels), and rasterize an SVG to PNG (RasterizeSVG).
// A purpose that does not re-encode keeps the image's own bytes, stripped of
// metadata, and still gets its Variants.
type ImageHandling struct {
	Reencode     bool           `json:"reencode"`
	MaxDimension int            `json:"max_dimension"`
	Sizes        map[string]int `json:"sizes"`
	RasterizeSVG bool           `json:"rasterize_svg"`
}

type catalogueFile struct {
	Purposes map[string]purposeEntry `json:"purposes"`
}

type purposeEntry struct {
	Description string              `json:"description"`
	Upload      authz.MediaUploader `json:"upload"`
	Types       []string            `json:"types"`
	MaxMiB      int64               `json:"max_mib"`
	Visibility  Visibility          `json:"visibility"`
	Encrypted   bool                `json:"encrypted"`
	Scan        bool                `json:"scan"`
	PendingTTL  string              `json:"pending_ttl"`
	Transport   Transport           `json:"transport"`
	Attach      Attacher            `json:"attach"`
	Image       *ImageHandling      `json:"image"`
	LegacyRules bool                `json:"legacy_rules"`
}

var purposeName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// catalogueTypes are the content types a purpose may name: the raster
// formats, from the one table the sanitizer and the serving policy share,
// and the other types core knows.
var catalogueTypes = append(rasterContentTypes(), svgType, pdfType, docxType, zipType, mp4Type)

func rasterContentTypes() []string {
	out := make([]string, 0, len(rasterFormats))
	for _, format := range rasterFormats {
		out = append(out, format.contentType)
	}
	return out
}

// LoadCatalogue reads and validates the reviewed catalogue carried in the
// binary. Core refuses to start when it returns an error.
func LoadCatalogue() (Catalogue, error) {
	return ParseCatalogue(config.MediaPurposes)
}

// ParseCatalogue reads a catalogue file and validates it against the hard
// ceilings. A ceiling violation is one of the ErrCeiling errors; anything
// else wrong with the file is ErrCatalogueInvalid.
func ParseCatalogue(data []byte) (Catalogue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file catalogueFile
	if err := decoder.Decode(&file); err != nil {
		return Catalogue{}, fmt.Errorf("%w: %v", ErrCatalogueInvalid, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Catalogue{}, fmt.Errorf("%w: content after the catalogue", ErrCatalogueInvalid)
	}
	purposes := make(map[string]Purpose, len(file.Purposes))
	for _, name := range slices.Sorted(maps.Keys(file.Purposes)) {
		p, err := file.Purposes[name].purpose(name)
		if err != nil {
			return Catalogue{}, fmt.Errorf("%w: %s: %v", ErrCatalogueInvalid, name, err)
		}
		if err := checkCeilings(p); err != nil {
			return Catalogue{}, err
		}
		purposes[name] = p
	}
	for _, name := range requiredPurposes {
		if _, ok := purposes[name]; !ok {
			return Catalogue{}, fmt.Errorf("%w: no %s purpose, which core refers to", ErrCatalogueInvalid, name)
		}
	}
	return Catalogue{purposes: purposes}, nil
}

func (e purposeEntry) purpose(name string) (Purpose, error) {
	p := Purpose{
		Name: name, Uploader: e.Upload, Types: e.Types, MaxBytes: e.MaxMiB << 20,
		Visibility: e.Visibility, Encrypted: e.Encrypted, Scan: e.Scan,
		Transport: e.Transport, Attach: e.Attach, LegacyRules: e.LegacyRules,
	}
	if e.Image != nil {
		p.Image = *e.Image
	}
	if !purposeName.MatchString(name) {
		return Purpose{}, errors.New("a purpose name is lowercase letters, digits and underscores")
	}
	if !e.Upload.Known() {
		return Purpose{}, fmt.Errorf("unknown upload rule %q", e.Upload)
	}
	switch e.Visibility {
	case VisibilityPublic:
		if e.Encrypted {
			return Purpose{}, errors.New("a public purpose is served from the CDN and cannot be encrypted")
		}
	case VisibilityPrivate:
	default:
		return Purpose{}, fmt.Errorf("unknown visibility %q", e.Visibility)
	}
	if e.Transport != TransportSingleStep && e.Transport != TransportDirect {
		return Purpose{}, fmt.Errorf("unknown transport %q", e.Transport)
	}
	if e.Attach != AttachCore && e.Attach != AttachService {
		return Purpose{}, fmt.Errorf("unknown attach %q", e.Attach)
	}
	if e.PendingTTL == "none" {
		if !e.LegacyRules {
			return Purpose{}, errors.New("only the legacy rules keep a pending Media forever")
		}
	} else {
		ttl, err := time.ParseDuration(e.PendingTTL)
		if err != nil || ttl <= 0 {
			return Purpose{}, fmt.Errorf("pending_ttl %q is not a positive duration", e.PendingTTL)
		}
		p.PendingTTL = ttl
	}
	if e.LegacyRules {
		if name != PurposeLegacy {
			return Purpose{}, fmt.Errorf("only the %s purpose follows the legacy rules", PurposeLegacy)
		}
		if len(e.Types) > 0 || e.MaxMiB != 0 || e.Visibility != VisibilityPublic || e.Transport != TransportSingleStep {
			return Purpose{}, errors.New("the legacy rules fix types and size; the purpose is public and single_step")
		}
	} else {
		if len(e.Types) == 0 {
			return Purpose{}, errors.New("no types")
		}
		for i, t := range e.Types {
			if !slices.Contains(catalogueTypes, t) {
				return Purpose{}, fmt.Errorf("unknown type %q", t)
			}
			if slices.Contains(e.Types[:i], t) {
				return Purpose{}, fmt.Errorf("type %q named twice", t)
			}
		}
		if e.MaxMiB <= 0 {
			return Purpose{}, errors.New("max_mib is not positive")
		}
	}
	if p.Image.MaxDimension < 0 {
		return Purpose{}, errors.New("max_dimension is negative")
	}
	largest := p.Image.MaxDimension
	if largest == 0 {
		largest = MaxImageDimension
	}
	for name, size := range p.Image.Sizes {
		if !slices.Contains(imageSizes, name) {
			return Purpose{}, fmt.Errorf("size %q is not one clients can ask for (%v)", name, imageSizes)
		}
		if size <= 0 || size > largest {
			return Purpose{}, fmt.Errorf("size %q is %d px, outside 1..%d", name, size, largest)
		}
	}
	return p, nil
}

// Lookup returns the Media purpose with this name.
func (c Catalogue) Lookup(name string) (Purpose, bool) {
	p, ok := c.purposes[name]
	return p, ok
}
