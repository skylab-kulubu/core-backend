package event

import "github.com/skylab-kulubu/core-backend/internal/media"

func withPublicMedia(e Event, base string) Event {
	e.CoverImageURL = media.PublicURL(base, e.CoverImageURL)
	if n := len(e.Images); n > 0 {
		images := make([]GalleryImage, n)
		copy(images, e.Images)
		for i := range images {
			images[i].URL = media.PublicURL(base, images[i].URL)
		}
		e.Images = images
	}
	if n := len(e.ImageURLs); n > 0 {
		urls := make([]string, 0, n)
		for _, u := range e.ImageURLs {
			if abs := media.PublicURL(base, u); abs != "" {
				urls = append(urls, abs)
			}
		}
		e.ImageURLs = urls
	}
	return emptyGallery(e)
}

func withPublicMediaAll(events []Event, base string) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = withPublicMedia(e, base)
	}
	return out
}
