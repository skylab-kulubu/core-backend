package media

import (
	"maps"
	"slices"
	"strings"
)

// The image sizes a client can ask for by name. Each purpose's catalogue
// entry picks which of them it stores and how large they are
// (image.variants); the names are the contract clients rely on.
const (
	// SizeCard is a small image, such as the picture on an Event card.
	SizeCard = "card"
	// SizePage is a large image, such as the picture across a page.
	SizePage = "page"
)

// imageSizes are the size names, in the order they are made.
var imageSizes = []string{SizeCard, SizePage}

// ImageSize is how large a stored image is, in pixels.
type ImageSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ImageAddress is where one size of an image Media is served, and how large
// it is there when core knows.
type ImageAddress struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// variantKey is the object key of an image's stored size: next to the
// image's own key, so every cleanup that knows the key finds its sizes.
func variantKey(key, size string) string {
	return key + "/" + size
}

// Addresses builds the public addresses of Media objects from a configured
// base.
type Addresses struct {
	// Base is the public origin objects are served from. Empty leaves keys
	// as they are (a development setup without a CDN).
	Base string
}

// Object is the public address of an object key. A key that is already an
// absolute address (Media stored before core kept keys) is left alone.
func (a Addresses) Object(key string) string {
	if strings.TrimSpace(a.Base) == "" {
		return key
	}
	return PublicURL(a.Base, key)
}

// Image is the address of an image Media at the named size, whose longer
// side the purpose sets at px. A size core has not stored (the image is
// smaller than it, or was stored before sizes existed) is the original.
func (a Addresses) Image(m Media, size string, px int) ImageAddress {
	if stored, ok := m.StoredVariants[size]; ok {
		return ImageAddress{URL: a.Object(variantKey(m.Key, size)), Width: stored.Width, Height: stored.Height}
	}
	return ImageAddress{URL: a.Object(m.Key), Width: m.Width, Height: m.Height}
}

// imageAddresses are the addresses of every size the purpose gives an
// image Media, or nil when the Media has none: not a raster image.
func (a Addresses) imageAddresses(m Media, sizes map[string]int) map[string]ImageAddress {
	if m.Kind != KindImage || !isRasterType(m.Type) || len(sizes) == 0 {
		return nil
	}
	out := make(map[string]ImageAddress, len(sizes))
	for _, size := range slices.Sorted(maps.Keys(sizes)) {
		out[size] = a.Image(m, size, sizes[size])
	}
	return out
}
