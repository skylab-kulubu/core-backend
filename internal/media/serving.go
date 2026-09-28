package media

import (
	"mime"
	"slices"
)

// ServingMetadataFor is the serving policy for a Media of the purpose: how
// the CDN answers for its object. Every write of a Media's object to the
// public bucket takes its metadata from here. It is ServingMetadata but for
// a video: the MP4 of a purpose that plays (videoPurposes, the video purpose)
// keeps its type and is served inline, so a <video> element and the
// browser's player take it (the CDN answers Range requests, and a
// Cloudflare rule adds nosniff). An MP4 of any other purpose, or of none,
// stays an opaque download.
func ServingMetadataFor(purpose, contentType, name string) BlobMetadata {
	if contentType == mp4Type && slices.Contains(videoPurposes, purpose) {
		return BlobMetadata{ContentType: mp4Type}
	}
	return ServingMetadata(contentType, name)
}

// ServingMetadata is the serving policy of an object written without a
// purpose (a certificate's PDF and assets; a Media's object goes through
// ServingMetadataFor). Only types a browser cannot run script from are
// served inline: the raster formats Upload accepts, and PDF. SVG keeps its
// type so `<img>` still renders it, but opening its URL downloads it
// instead of running its script. Everything else is stored as an opaque
// download under its name.
func ServingMetadata(contentType, name string) BlobMetadata {
	switch {
	case isRasterType(contentType), contentType == pdfType:
		return BlobMetadata{ContentType: contentType}
	case contentType == svgType:
		return BlobMetadata{ContentType: contentType, ContentDisposition: downloadDisposition(name)}
	}
	return BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: downloadDisposition(name)}
}

// downloadDisposition encodes the name as a parameter (RFC 2231 when it is
// not plain ASCII), so a name can neither break the header nor add
// parameters to it. An object without a name downloads under its key.
func downloadDisposition(name string) string {
	if name == "" {
		return "attachment"
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
