package media

import (
	"errors"
	"fmt"
)

// The hard ceilings of ADR-0052. No catalogue entry can loosen them: core
// refuses to start with a catalogue that breaks one.
var (
	// ErrCeilingPublicType: a public purpose accepts only raster images, PDF
	// and MP4, the types a browser cannot run script from.
	ErrCeilingPublicType = errors.New("media purpose catalogue: a public purpose accepts only raster images, PDF and MP4")
	// ErrCeilingPublicRaster: a public purpose re-encodes every raster image,
	// so no uploaded image bytes reach the CDN as they came.
	ErrCeilingPublicRaster = errors.New("media purpose catalogue: a public purpose re-encodes raster images")
	// ErrCeilingSVG: SVG is never stored as SVG. A purpose that accepts it
	// rasterizes it to PNG.
	ErrCeilingSVG = errors.New("media purpose catalogue: SVG is rasterized to PNG, never stored as SVG")
	// ErrCeilingSize: a purpose's maximum stays under the global maximum of
	// its transport: MaxUploadBytes through core, MaxDirectUploadBytes by
	// Direct upload.
	ErrCeilingSize = errors.New("media purpose catalogue: maximum size above the global maximum of its transport")
	// ErrCeilingPrivate: a private purpose is always encrypted before it
	// reaches storage (KVKK cloud guidance, §3.4).
	ErrCeilingPrivate = errors.New("media purpose catalogue: a private purpose is encrypted")
	// ErrCeilingImageDimension: a re-encoded image is at most
	// MaxImageDimension pixels on its longer side.
	ErrCeilingImageDimension = errors.New("media purpose catalogue: image dimension above the cap")
)

// MaxImageDimension is the longest side, in pixels, of a re-encoded image.
const MaxImageDimension = 2560

// MaxDirectUploadBytes is the largest Media a Direct upload may carry.
const MaxDirectUploadBytes = 2 << 30

const (
	visibilityPublic  = "public"
	visibilityPrivate = "private"

	mp4Type  = "video/mp4"
	zipType  = "application/zip"
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	transportSingleStep = "single_step"
	transportDirect     = "direct"
)

func checkCeilings(p Purpose) error {
	limit := int64(MaxUploadBytes)
	if p.Transport == transportDirect {
		limit = MaxDirectUploadBytes
	}
	if p.MaxBytes > limit {
		return fmt.Errorf("%s allows %d bytes over %d: %w", p.Name, p.MaxBytes, limit, ErrCeilingSize)
	}
	if p.Image.MaxDimension > MaxImageDimension {
		return fmt.Errorf("%s keeps %d px: %w", p.Name, p.Image.MaxDimension, ErrCeilingImageDimension)
	}
	if p.Visibility == visibilityPrivate && !p.Encrypted {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingPrivate)
	}
	for _, t := range p.Types {
		if t == svgType && !p.Image.RasterizeSVG {
			return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingSVG)
		}
	}
	if p.Visibility == visibilityPublic {
		for _, t := range p.Types {
			if t == svgType {
				continue // rasterized to PNG, checked above
			}
			if !isRasterType(t) && t != pdfType && t != mp4Type {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicType)
			}
			if isRasterType(t) && !p.Image.Reencode {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicRaster)
			}
		}
	}
	return nil
}
