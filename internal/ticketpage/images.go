package ticketpage

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	_ "image/png" // a stored size may be a PNG
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	xdraw "golang.org/x/image/draw"
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

// The cover band: 182 × 24 mm across the top of the first page, as a
// 1240-pixel JPEG (about 170 dpi), cut from the middle of the cover.
const (
	bandWidth   = 1240
	bandHeight  = 164
	bandQuality = 80
	// bandMaxPixels bounds the cover decoded for it: a page size is at most
	// 1200 pixels wide.
	bandMaxPixels = 4096 * 4096
)

// bandCacheSize and bandTTL bound the bands kept: a ticket's PDF is asked
// for a few times, and many tickets share one Event's cover.
const (
	bandCacheSize = 64
	bandTTL       = 30 * time.Minute
)

type cachedBand struct {
	data []byte
	at   time.Time
}

// bands cuts covers into bands and keeps the last ones, by the cover's
// Media id.
type bands struct {
	mu    sync.Mutex
	cache map[uuid.UUID]cachedBand
}

func (b *bands) get(id uuid.UUID, now time.Time) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hit, ok := b.cache[id]
	if !ok || now.Sub(hit.at) > bandTTL {
		return nil, false
	}
	return hit.data, true
}

func (b *bands) put(id uuid.UUID, data []byte, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cache == nil {
		b.cache = map[uuid.UUID]cachedBand{}
	}
	if len(b.cache) >= bandCacheSize {
		for key, old := range b.cache {
			if now.Sub(old.at) > bandTTL {
				delete(b.cache, key)
			}
		}
		for key := range b.cache {
			if len(b.cache) < bandCacheSize {
				break
			}
			delete(b.cache, key)
		}
	}
	b.cache[id] = cachedBand{data: data, at: now}
}

// CoverBand cuts the middle of a cover image to the band's shape and
// scales it to the band's size, as a JPEG. ok is false for an image it
// cannot decode, or one too large to.
func CoverBand(data []byte) ([]byte, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > bandMaxPixels {
		return nil, false
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	// The widest strip of the band's shape the image holds, centred.
	cropW, cropH := w, w*bandHeight/bandWidth
	if cropH > h {
		cropH, cropW = h, h*bandWidth/bandHeight
	}
	if cropW < 1 || cropH < 1 {
		return nil, false
	}
	x0, y0 := bounds.Min.X+(w-cropW)/2, bounds.Min.Y+(h-cropH)/2
	dst := image.NewRGBA(image.Rect(0, 0, bandWidth, bandHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, image.Rect(x0, y0, x0+cropW, y0+cropH), xdraw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: bandQuality}); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}
