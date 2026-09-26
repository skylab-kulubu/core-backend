package media

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The image sizes a client can ask for by name. Each purpose's catalogue
// entry picks which of them it stores and how large they are
// (image.sizes); the names are the contract clients rely on.
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

// AddressMode is where an image's sizes are served from.
type AddressMode string

const (
	// AddressStoredSizes serves each size from the copy core stored beside
	// the image (<key>/<size>). The default: it costs only storage.
	AddressStoredSizes AddressMode = "stored"
	// AddressCloudflare serves each size through Cloudflare image
	// transformations of the original (/cdn-cgi/image/…), which must be
	// enabled on the zone of the base. Switching needs no stored content
	// rewritten.
	AddressCloudflare AddressMode = "cloudflare"
)

// ImageAddressModeFromEnv reads MEDIA_IMAGE_ADDRESS_MODE: stored (the
// default) or cloudflare.
func ImageAddressModeFromEnv(getenv func(string) string) (AddressMode, error) {
	switch mode := AddressMode(strings.TrimSpace(getenv("MEDIA_IMAGE_ADDRESS_MODE"))); mode {
	case "", AddressStoredSizes:
		return AddressStoredSizes, nil
	case AddressCloudflare:
		return mode, nil
	}
	return "", fmt.Errorf("MEDIA_IMAGE_ADDRESS_MODE must be %s or %s", AddressStoredSizes, AddressCloudflare)
}

// Addresses builds the public addresses of Media objects from a configured
// base.
type Addresses struct {
	// Base is the public origin objects are served from. Empty is the
	// process's configured base (PublicURL): an address is never a bare key.
	Base string
	// Mode is where image sizes are served from; empty is
	// AddressStoredSizes.
	Mode AddressMode
}

// Object is the public address of an object key. A key that is already an
// absolute address (Media stored before core kept keys) is left alone.
func (a Addresses) Object(key string) string {
	return PublicURL(a.Base, key)
}

// Image is the address of an image Media at the named size, whose longer
// side the purpose sets at px. A size core has not stored (the image is
// smaller than it, or its sizes are not made yet) is the original.
//
// With AddressCloudflare, every size is a Cloudflare transformation of the
// original that fits it in a px square without enlarging it, sized from
// the Media's recorded size when core knows it. An original whose key is
// already an absolute address stays itself.
func (a Addresses) Image(m Media, size string, px int) ImageAddress {
	if a.Mode == AddressCloudflare && !isAbsoluteURL(m.Key) {
		shown := fittedSize(m.imageSize(), px)
		options := fmt.Sprintf("width=%d,height=%d,fit=scale-down", px, px)
		return ImageAddress{URL: a.Object("cdn-cgi/image/" + options + "/" + strings.TrimLeft(m.Key, "/")), Width: shown.Width, Height: shown.Height}
	}
	if object, ok := m.SizeObjects[size]; ok {
		return ImageAddress{URL: a.Object(sizeObjectKey(m.Key, size, object.Type)), Width: object.Width, Height: object.Height}
	}
	return ImageAddress{URL: a.Object(m.Key), Width: m.Width, Height: m.Height}
}

// imageAddresses are the addresses of every size the purpose gives an
// image Media: the SVG itself for an SVG, or nil when the Media has none
// (not a raster image or SVG).
func (a Addresses) imageAddresses(m Media, sizes map[string]int) map[string]ImageAddress {
	if m.Kind != KindImage || len(sizes) == 0 {
		return nil
	}
	if m.Type == svgType {
		// An SVG scales itself: every size is the SVG.
		out := make(map[string]ImageAddress, len(sizes))
		for size := range sizes {
			out[size] = ImageAddress{URL: a.Object(m.Key)}
		}
		return out
	}
	if !isRasterType(m.Type) {
		return nil
	}
	out := make(map[string]ImageAddress, len(sizes))
	for _, size := range slices.Sorted(maps.Keys(sizes)) {
		out[size] = a.Image(m, size, sizes[size])
	}
	return out
}
