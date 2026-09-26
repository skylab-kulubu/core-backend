package media_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestAddressesPointAnImageSizeAtItsStoredCopyOrAtCloudflare(t *testing.T) {
	t.Parallel()
	photo := media.Media{
		Key: "images/abc", Kind: media.KindImage, Type: "image/jpeg", Width: 1600, Height: 1200,
		SizeObjects: map[string]media.ImageSize{"card": {Width: 400, Height: 300}},
	}
	small := media.Media{Key: "images/small", Kind: media.KindImage, Type: "image/png", Width: 300, Height: 200, SizeObjects: map[string]media.ImageSize{}}
	stored := media.Addresses{Base: "https://cdn.example.test/"}
	cloudflare := media.Addresses{Base: "https://cdn.example.test", Mode: media.AddressCloudflare}

	for _, tc := range []struct {
		name      string
		addresses media.Addresses
		m         media.Media
		size      string
		px        int
		want      media.ImageAddress
	}{
		{"stored size", stored, photo, "card", 400, media.ImageAddress{URL: "https://cdn.example.test/images/abc/card", Width: 400, Height: 300}},
		{"size not stored is the original", stored, photo, "page", 1200, media.ImageAddress{URL: "https://cdn.example.test/images/abc", Width: 1600, Height: 1200}},
		{"cloudflare card", cloudflare, photo, "card", 400, media.ImageAddress{URL: "https://cdn.example.test/cdn-cgi/image/width=400,height=400,fit=scale-down/images/abc", Width: 400, Height: 300}},
		{"cloudflare page", cloudflare, photo, "page", 1200, media.ImageAddress{URL: "https://cdn.example.test/cdn-cgi/image/width=1200,height=1200,fit=scale-down/images/abc", Width: 1200, Height: 900}},
		{"cloudflare never enlarges", cloudflare, small, "card", 400, media.ImageAddress{URL: "https://cdn.example.test/cdn-cgi/image/width=400,height=400,fit=scale-down/images/small", Width: 300, Height: 200}},
	} {
		if got := tc.addresses.Image(tc.m, tc.size, tc.px); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestImageAddressModeComesFromConfiguration(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]media.AddressMode{
		"":           media.AddressStoredSizes,
		"stored":     media.AddressStoredSizes,
		"cloudflare": media.AddressCloudflare,
	} {
		got, err := media.ImageAddressModeFromEnv(func(name string) string {
			if name == "MEDIA_IMAGE_ADDRESS_MODE" {
				return value
			}
			return ""
		})
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	if _, err := media.ImageAddressModeFromEnv(func(string) string { return "imgix" }); err == nil {
		t.Fatal("an unknown mode is accepted")
	}
}
