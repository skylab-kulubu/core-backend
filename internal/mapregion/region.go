// Package mapregion keeps the club's map regions (docs/event-map.md,
// "Regions"): extracts of OpenStreetMap (a Protomaps basemap) in the public
// bucket that Event maps are drawn from. An ADMIN asks for a region by its
// name and box; a worker extracts it from the latest Protomaps build, as
// `pmtiles extract` does, and uploads it; a point's map is drawn from the
// smallest ready region that holds it.
package mapregion

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
)

// Status is where a region is.
type Status string

const (
	StatusRequested  Status = "requested"
	StatusExtracting Status = "extracting"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusRetired    Status = "retired"
)

// Why a region failed (Region.ErrorCode).
const (
	CodeSourceUnavailable = "source_unavailable"
	CodeSchemaMismatch    = "schema_mismatch"
	CodeTooLarge          = "too_large"
	CodeEmpty             = "empty"
	CodeUploadFailed      = "upload_failed"
	CodeFailed            = "failed"
)

var (
	ErrNotFound  = errors.New("mapregion: not found")
	ErrForbidden = errors.New("mapregion: forbidden")
	ErrInvalid   = errors.New("mapregion: invalid")
	// ErrExists is a box a live region (requested, extracting or ready)
	// already has.
	ErrExists = errors.New("mapregion: a live region has this box")
	// ErrState is a change the region's status does not allow.
	ErrState = errors.New("mapregion: not in a status that allows it")
)

// NameMax bounds a region's name, in characters.
const NameMax = 80

// MaxZoom is the deepest zoom a region holds: the basemap's.
const MaxZoom = 15

// MaxArea bounds a region's box, in square degrees: four times Türkiye's.
const MaxArea = 4 * 19.17 * 6.3

// Region is one of the club's map regions.
type Region struct {
	ID            uuid.UUID     `json:"id"`
	Name          string        `json:"name"`
	Box           mapimage.BBox `json:"-"`
	BBox          [4]float64    `json:"bbox"`
	MaxZoom       int           `json:"maxZoom"`
	Status        Status        `json:"status"`
	SizeBytes     *int64        `json:"sizeBytes,omitempty"`
	ProgressBytes int64         `json:"progressBytes"`
	SourceBuild   string        `json:"sourceBuild,omitempty"`
	// URL is the extract's public address, once ready.
	URL       string     `json:"url,omitempty"`
	ErrorCode string     `json:"errorCode,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ReadyAt   *time.Time `json:"readyAt,omitempty"`
	// ObjectKey is the extract's key in the public bucket.
	ObjectKey string `json:"-"`
	Attempts  int    `json:"-"`
}

func (r Region) withBox() Region {
	r.BBox = [4]float64{r.Box.MinLon, r.Box.MinLat, r.Box.MaxLon, r.Box.MaxLat}
	return r
}

// validRequest checks a request's name, box and zoom.
func validRequest(name string, box mapimage.BBox, maxZoom int) bool {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= NameMax && box.Valid() && box.Area() <= MaxArea && maxZoom >= 0 && maxZoom <= MaxZoom
}

// slug is the region's name for its object key: ASCII, lower case, hyphens.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.NewReplacer("İ", "i", "I", "ı").Replace(name)) {
		switch r {
		case 'ç':
			r = 'c'
		case 'ğ':
			r = 'g'
		case 'ı':
			r = 'i'
		case 'ö':
			r = 'o'
		case 'ş':
			r = 's'
		case 'ü':
			r = 'u'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "bolge"
}

// ObjectKeyFor is where a region's extract of a build goes: a new key per
// region and build, so a file is never overwritten in place (core keeps an
// archive's directory while it runs).
func ObjectKeyFor(r Region, build string) string {
	return "maps/" + slug(r.Name) + "-" + build + "-" + r.ID.String()[:8] + ".pmtiles"
}
