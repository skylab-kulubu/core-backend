package media

import (
	"errors"
	"fmt"
	"slices"
)

// The hard ceilings of ADR-0052. No catalogue entry can loosen them: core
// refuses to start with a catalogue that breaks one.
var (
	// ErrCeilingPublicType: a public purpose accepts only raster images,
	// SVG (sanitized, served as a download), PDF and MP4.
	ErrCeilingPublicType = errors.New("media purpose catalogue: a public purpose accepts only raster images, SVG, PDF and MP4")
	// ErrCeilingPublicRaster: a public purpose that accepts raster images
	// declares image.reencode, so that no uploaded image bytes reach the CDN
	// as they came: core decodes such an image and stores only its pixels,
	// encoded again (reencodeRaster). Media uploaded without a purpose
	// (legacy) are not a purpose's upload and keep their stripped bytes
	// until they fall to the strict rule.
	ErrCeilingPublicRaster = errors.New("media purpose catalogue: a public purpose declares re-encoding for raster images")
	// ErrCeilingSVG: only a public purpose accepts SVG. Core stores an SVG
	// only after sanitizing it (sanitizeSVG), under a key ending in .svg,
	// and serves it as a download (ServingMetadata): never inline, and
	// never for a purpose that does not list it.
	ErrCeilingSVG = errors.New("media purpose catalogue: only a public purpose accepts SVG")
	// ErrCeilingSize: a purpose's maximum stays under the global maximum of
	// its transport: MaxUploadBytes through core, MaxDirectUploadBytes by
	// Direct upload.
	ErrCeilingSize = errors.New("media purpose catalogue: maximum size above the global maximum of its transport")
	// ErrCeilingPrivate: a private purpose is always encrypted before it
	// reaches storage (KVKK cloud guidance, §3.4).
	ErrCeilingPrivate = errors.New("media purpose catalogue: a private purpose is encrypted")
	// ErrCeilingImageDimension: a re-encoded image is at most
	// MaxImageDimension pixels on its longer side; re-encoding scales a
	// larger one down to the purpose's max_dimension.
	ErrCeilingImageDimension = errors.New("media purpose catalogue: image dimension above the cap")
)

// MaxImageDimension is the longest side, in pixels, of a re-encoded image,
// and of a purpose's image sizes.
const MaxImageDimension = 2560

// MaxDirectUploadBytes is the largest Media a Direct upload may carry.
const MaxDirectUploadBytes = 2 << 30

const (
	mp4Type  = "video/mp4"
	zipType  = "application/zip"
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func checkCeilings(p Purpose) error {
	limit := int64(MaxUploadBytes)
	if p.Transport == TransportDirect {
		limit = MaxDirectUploadBytes
	}
	if p.MaxBytes > limit {
		return fmt.Errorf("%s allows %d bytes over %d: %w", p.Name, p.MaxBytes, limit, ErrCeilingSize)
	}
	if p.Image.MaxDimension > MaxImageDimension {
		return fmt.Errorf("%s keeps %d px: %w", p.Name, p.Image.MaxDimension, ErrCeilingImageDimension)
	}
	if p.Visibility == VisibilityPrivate && !p.Encrypted {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingPrivate)
	}
	if p.Visibility != VisibilityPublic && slices.Contains(p.Types, svgType) {
		return fmt.Errorf("%s names %s: %w", p.Name, svgType, ErrCeilingSVG)
	}
	if p.Visibility == VisibilityPublic {
		for _, t := range p.Types {
			if !isRasterType(t) && t != svgType && t != pdfType && t != mp4Type {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicType)
			}
			if isRasterType(t) && !p.Image.Reencode {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicRaster)
			}
		}
	}
	return nil
}
