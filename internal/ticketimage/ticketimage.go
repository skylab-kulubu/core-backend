// Package ticketimage is an Event's ticket image (docs/ticket-image.md):
// what the PDF ticket shows in the band across its top. Without a choice
// the band is cut from the middle of the Event's cover. An editor may pick
// the cover's region instead, or an image of its own and its region. The
// region is kept as fractions of the image; the image itself never
// changes, and the band is cut when the ticket is drawn.
package ticketimage

import (
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

// Source is where the band's picture comes from.
type Source string

const (
	// SourceAuto is the cover's middle (no row).
	SourceAuto Source = "auto"
	// SourceCover is a region of the cover.
	SourceCover Source = "cover"
	// SourceUpload is a region of an image of the ticket's own.
	SourceUpload Source = "upload"
)

// The band's shape on the PDF: 182 × 24 mm across the top of the first page.
// The panel's cropper and preview take the same shape.
const (
	BandWidthMM  = 182
	BandHeightMM = 24
)

// BandAspect is the band's width over its height.
const BandAspect = float64(BandWidthMM) / BandHeightMM

// minCropSide bounds a region's side: a region smaller than a hundredth of
// the image is a slip of the hand.
const minCropSide = 0.01

// ErrInvalid is a choice core does not take: an unknown source, a region
// out of the image, a cover region without a cover, an image source without
// an image.
var ErrInvalid = errors.New("ticket image: invalid")

// Crop is a region of an image, in fractions of its width (X, W) and height
// (Y, H).
type Crop struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Valid reports whether the region lies in the image and is not a speck.
func (c Crop) Valid() bool {
	for _, v := range []float64{c.X, c.Y, c.W, c.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	const slack = 1e-6
	return c.X >= 0 && c.Y >= 0 && c.W >= minCropSide && c.H >= minCropSide &&
		c.X+c.W <= 1+slack && c.Y+c.H <= 1+slack
}

// Setting is an Event's choice. The zero Setting and SourceAuto are the
// cover's middle.
type Setting struct {
	Source Source
	// MediaID is the image of SourceUpload; nil once its Media is gone,
	// which draws the cover's middle again.
	MediaID *uuid.UUID
	// CoverMediaID is the cover a SourceCover region was drawn on: another
	// cover draws its middle (the region would show the wrong part).
	CoverMediaID *uuid.UUID
	Crop         *Crop
	UpdatedAt    *time.Time
}

// Band is the image and the region the band is cut from, for an Event
// whose cover is cover: nil image draws no picture (the band takes the
// cover's colour), nil crop the image's middle.
func (s Setting) Band(cover *uuid.UUID) (image *uuid.UUID, crop *Crop) {
	switch s.Source {
	case SourceCover:
		if cover != nil && s.CoverMediaID != nil && *s.CoverMediaID == *cover && s.Crop != nil {
			return cover, s.Crop
		}
	case SourceUpload:
		if s.MediaID != nil && s.Crop != nil {
			return s.MediaID, s.Crop
		}
	}
	return cover, nil
}

// CoverChanged reports whether a cover region no longer applies: the
// Event's cover is not the one it was drawn on.
func (s Setting) CoverChanged(cover *uuid.UUID) bool {
	return s.Source == SourceCover && (cover == nil || s.CoverMediaID == nil || *s.CoverMediaID != *cover)
}
