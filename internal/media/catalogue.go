package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

const (
	// PurposeLegacy is the Media purpose of a purpose-less upload and of
	// every Media stored before Media purpose existed.
	PurposeLegacy = "legacy"
	// PurposeProfilePicture is a person's own profile picture.
	PurposeProfilePicture = "profile_picture"
)

// ErrCatalogueInvalid is a catalogue file core cannot read as one: a field it
// does not know, a value outside the vocabulary, or a missing legacy purpose.
var ErrCatalogueInvalid = errors.New("media purpose catalogue: invalid")

// Catalogue is the Media purpose catalogue: every Media purpose core accepts
// and the rules each one fixes. The reviewed file is
// config/media-purposes.json.
type Catalogue struct {
	purposes map[string]Purpose
}

// Purpose is one Media purpose from the catalogue.
type Purpose struct {
	Name string
	// Uploader is who may upload Media of this purpose.
	Uploader authz.MediaUploader
	// Types are the content types the purpose accepts, detected from the
	// file's content, never from its name or declared type.
	Types    []string
	MaxBytes int64
	// Visibility is "public" (served from the CDN) or "private".
	Visibility string
	Encrypted  bool
	Scan       bool
	// PendingTTL is how long a Media with no Media attachment is kept. Zero
	// keeps it: only the legacy purpose may, until purpose-less uploads fall
	// to the strict rule.
	PendingTTL time.Duration
	// Transport is "single_step" (through core) or "direct" (Direct upload).
	Transport string
	Image     ImageHandling
	// LegacyRules makes the purpose accept what purpose-less uploads accepted
	// before Media purpose: see docs/media-lifecycle.md.
	LegacyRules bool
}

// ImageHandling is what core does with a raster image of a purpose.
type ImageHandling struct {
	Reencode     bool           `json:"reencode"`
	MaxDimension int            `json:"max_dimension"`
	Variants     map[string]int `json:"variants"`
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
	Visibility  string              `json:"visibility"`
	Encrypted   bool                `json:"encrypted"`
	Scan        bool                `json:"scan"`
	PendingTTL  string              `json:"pending_ttl"`
	Transport   string              `json:"transport"`
	Image       *ImageHandling      `json:"image"`
	LegacyRules bool                `json:"legacy_rules"`
}

var purposeName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// catalogueTypes are the content types a purpose may name.
var catalogueTypes = []string{
	"image/jpeg", "image/png", "image/webp", "image/gif",
	svgType, pdfType, docxType, zipType, mp4Type,
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
	if _, ok := purposes[PurposeLegacy]; !ok {
		return Catalogue{}, fmt.Errorf("%w: no %s purpose for purpose-less uploads", ErrCatalogueInvalid, PurposeLegacy)
	}
	return Catalogue{purposes: purposes}, nil
}

func (e purposeEntry) purpose(name string) (Purpose, error) {
	p := Purpose{
		Name: name, Uploader: e.Upload, Types: e.Types, MaxBytes: e.MaxMiB << 20,
		Visibility: e.Visibility, Encrypted: e.Encrypted, Scan: e.Scan,
		Transport: e.Transport, LegacyRules: e.LegacyRules,
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
	case visibilityPublic:
		if e.Encrypted {
			return Purpose{}, errors.New("a public purpose is served from the CDN and cannot be encrypted")
		}
	case visibilityPrivate:
	default:
		return Purpose{}, fmt.Errorf("unknown visibility %q", e.Visibility)
	}
	if e.Transport != transportSingleStep && e.Transport != transportDirect {
		return Purpose{}, fmt.Errorf("unknown transport %q", e.Transport)
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
		if len(e.Types) > 0 || e.MaxMiB != 0 || e.Visibility != visibilityPublic || e.Transport != transportSingleStep {
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
	for variant, size := range p.Image.Variants {
		if size <= 0 || size > largest {
			return Purpose{}, fmt.Errorf("variant %q is %d px, outside 1..%d", variant, size, largest)
		}
	}
	return p, nil
}

// Lookup returns the Media purpose with this name.
func (c Catalogue) Lookup(name string) (Purpose, bool) {
	p, ok := c.purposes[name]
	return p, ok
}
