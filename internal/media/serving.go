package media

import "mime"

// servingMetadata is the serving policy: how the CDN answers for an object of
// this type. Only types a browser cannot run script from are served inline:
// the raster formats Upload accepts, and PDF. SVG keeps its type so `<img>`
// still renders it, but opening its URL downloads it instead of running its
// script. Everything else is stored as an opaque download under its name.
func servingMetadata(contentType, name string) BlobMetadata {
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
// parameters to it.
func downloadDisposition(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
