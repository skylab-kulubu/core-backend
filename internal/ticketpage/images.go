package ticketpage

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // an original may be a GIF (its first frame)
	"image/jpeg"
	_ "image/png" // a stored size may be a PNG
	"math"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // an original may be a WebP
)

// Images reads the pictures a PDF ticket shows from core's media: a public,
// current image, by its Media's id, in the stored size asked (media.SizePage
// for the cover). ok is false when there is none, it is private or being
// purged, or it cannot be read: the ticket is drawn without it.
type Images interface {
	Image(ctx context.Context, id uuid.UUID, size string) (data []byte, contentType string, ok bool)
}

// maxImageBytes bounds an image put in, or decoded for, a PDF.
const maxImageBytes = 2 << 20

// MediaImages reads images from the media store and its public bucket.
type MediaImages struct {
	Media media.Store
	Blobs media.BlobStore
}

func (m *MediaImages) Image(ctx context.Context, id uuid.UUID, size string) ([]byte, string, bool) {
	if m == nil || m.Media == nil || m.Blobs == nil {
		return nil, "", false
	}
	item, err := m.Media.Get(ctx, id)
	if err != nil || item.DeletedAt != nil || !item.Servable() {
		return nil, "", false
	}
	key, contentType := item.Key, item.Type
	if size != "" {
		var ok bool
		if key, contentType, ok = item.SizeObjectKey(size); !ok {
			return nil, "", false
		}
	} else if item.Size > maxImageBytes {
		return nil, "", false
	}
	if contentType != "image/jpeg" && contentType != "image/png" {
		return nil, "", false
	}
	data, err := m.Blobs.Read(ctx, key)
	if err != nil || len(data) == 0 || len(data) > maxImageBytes {
		return nil, "", false
	}
	return data, contentType, true
}

// BandSources reads the copy of an image a band is cut from: the largest
// one core can decode (MediaImages: the original, else its page size). An
// Images without it gives the page size.
type BandSources interface {
	BandSource(ctx context.Context, id uuid.UUID) (data []byte, ok bool)
}

// maxBandSourceBytes bounds an original read for a band: an Event image is
// at most 10 MiB (config/media-purposes.json).
const maxBandSourceBytes = 10 << 20

// bandSourceTypes are the originals a band is cut from; any other (an SVG)
// gives its page size, if it has one.
var bandSourceTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif"}

// BandSource is the image's original when core can decode it, else its
// page size: a region of a poster needs the original's pixels to stay
// sharp at print size.
func (m *MediaImages) BandSource(ctx context.Context, id uuid.UUID) ([]byte, bool) {
	if m == nil || m.Media == nil || m.Blobs == nil {
		return nil, false
	}
	item, err := m.Media.Get(ctx, id)
	if err != nil || item.DeletedAt != nil || !item.Servable() {
		return nil, false
	}
	if slices.Contains(bandSourceTypes, item.Type) && item.Size > 0 && item.Size <= maxBandSourceBytes {
		if data, err := m.Blobs.Read(ctx, item.Key); err == nil && len(data) > 0 && len(data) <= maxBandSourceBytes {
			return data, true
		}
	}
	data, _, ok := m.Image(ctx, id, media.SizePage)
	return data, ok
}

// The band: 182 × 24 mm across the top of the first page
// (ticketimage.BandWidthMM, BandHeightMM). It is cut at 300 dpi, 2150
// pixels wide, or at the region's own width when that is less (never
// enlarged: the PDF's viewer scales it), as a JPEG of at most
// bandMaxBytes, narrower when that is the only way to fit.
const (
	bandWidth = 2150
	// bandMinWidth is the narrowest a band shrinks to fit bandMaxBytes
	// (about 85 dpi).
	bandMinWidth = 600
	// bandQualities are tried in turn until the JPEG fits bandMaxBytes.
	bandMaxBytes = 64 << 10
	// bandMaxPixels bounds the image decoded for it: an Event image is at
	// most 2560 pixels on a side.
	bandMaxPixels = 4096 * 4096
)

var bandQualities = []int{80, 70, 60, 50}

// bandCacheSize and bandTTL bound the bands kept: a ticket's PDF is asked
// for a few times, and many tickets share one Event's band.
const (
	bandCacheSize = 64
	bandTTL       = 30 * time.Minute
)

type cachedBand struct {
	data []byte
	at   time.Time
}

// bands keeps the last bands cut, by image and region (bandKey).
type bands struct {
	mu    sync.Mutex
	cache map[string]cachedBand
}

// bandKey names an image's band of a region.
func bandKey(id uuid.UUID, crop *ticketimage.Crop) string {
	if crop == nil {
		return id.String()
	}
	return fmt.Sprintf("%s/%.6f/%.6f/%.6f/%.6f", id, crop.X, crop.Y, crop.W, crop.H)
}

func (b *bands) get(key string, now time.Time) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hit, ok := b.cache[key]
	if !ok || now.Sub(hit.at) > bandTTL {
		return nil, false
	}
	return hit.data, true
}

func (b *bands) put(key string, data []byte, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cache == nil {
		b.cache = map[string]cachedBand{}
	}
	if len(b.cache) >= bandCacheSize {
		for k, old := range b.cache {
			if now.Sub(old.at) > bandTTL {
				delete(b.cache, k)
			}
		}
		for k := range b.cache {
			if len(b.cache) < bandCacheSize {
				break
			}
			delete(b.cache, k)
		}
	}
	b.cache[key] = cachedBand{data: data, at: now}
}

// CoverBand cuts the middle of a cover image to the band's shape: Band
// without a region.
func CoverBand(data []byte) ([]byte, bool) {
	return Band(data, nil)
}

// Band cuts the region of an image (nil: the widest strip of the band's
// shape, centred) to the band's shape and size, as a JPEG. A region of
// another shape is narrowed about its centre to the band's. ok is false for
// an image it cannot decode, or one too large to.
func Band(data []byte, crop *ticketimage.Crop) ([]byte, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > bandMaxPixels {
		return nil, false
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	region, ok := bandRegion(src.Bounds(), crop)
	if !ok {
		return nil, false
	}
	// The JPEG shrinks in quality first, then in width, until it fits
	// bandMaxBytes (a photo fits at the first try; noise needs the rest).
	var out []byte
	for outW := min(bandWidth, region.Dx()); ; outW = outW * 3 / 4 {
		outH := max(1, int(math.Round(float64(outW)/ticketimage.BandAspect)))
		dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, region, xdraw.Src, nil)
		for _, quality := range bandQualities {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
				return nil, false
			}
			out = buf.Bytes()
			if len(out) <= bandMaxBytes {
				return out, true
			}
		}
		if outW <= bandMinWidth {
			break
		}
	}
	return out, true
}

// bandRegion is the pixels of bounds the band shows: the region of crop
// (or the whole image) narrowed about its centre to the band's shape.
func bandRegion(bounds image.Rectangle, crop *ticketimage.Crop) (image.Rectangle, bool) {
	w, h := float64(bounds.Dx()), float64(bounds.Dy())
	x0, y0, rw, rh := 0.0, 0.0, w, h
	if crop != nil {
		if !crop.Valid() {
			return image.Rectangle{}, false
		}
		x0, y0 = crop.X*w, crop.Y*h
		rw, rh = min(crop.W*w, w-x0), min(crop.H*h, h-y0)
	}
	if rw/rh > ticketimage.BandAspect {
		narrower := rh * ticketimage.BandAspect
		x0 += (rw - narrower) / 2
		rw = narrower
	} else {
		lower := rw / ticketimage.BandAspect
		y0 += (rh - lower) / 2
		rh = lower
	}
	r := image.Rect(
		bounds.Min.X+int(math.Round(x0)), bounds.Min.Y+int(math.Round(y0)),
		bounds.Min.X+int(math.Round(x0+rw)), bounds.Min.Y+int(math.Round(y0+rh)),
	).Intersect(bounds)
	if r.Dx() < 1 || r.Dy() < 1 {
		return image.Rectangle{}, false
	}
	return r, true
}
