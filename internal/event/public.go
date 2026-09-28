package event

import "github.com/skylab-kulubu/core-backend/internal/media"

// withPublicMedia answers the Event's images at their addresses, full size
// and sizes, and its files and videos at theirs, under addresses.
func withPublicMedia(e Event, addresses media.Addresses) Event {
	e.CoverImageURL = addresses.Object(e.CoverImageURL)
	e.CoverImageSizes = addresses.LinkedSizes(e.coverImage)
	if n := len(e.Images); n > 0 {
		images := make([]GalleryImage, n)
		copy(images, e.Images)
		for i := range images {
			images[i].URL = addresses.Object(images[i].URL)
			images[i].Sizes = addresses.LinkedSizes(images[i].image)
		}
		e.Images = images
	}
	e.Files = withFileAddresses(e.Files, addresses)
	e.Videos = withFileAddresses(e.Videos, addresses)
	if n := len(e.ImageURLs); n > 0 {
		urls := make([]string, 0, n)
		for _, u := range e.ImageURLs {
			if abs := addresses.Object(u); abs != "" {
				urls = append(urls, abs)
			}
		}
		e.ImageURLs = urls
	}
	return emptyGallery(e)
}

func withPublicMediaAll(events []Event, addresses media.Addresses) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = withPublicMedia(e, addresses)
	}
	return out
}
