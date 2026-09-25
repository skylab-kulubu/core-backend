package media

import "mime"

// servingMetadata is the serving policy: how the CDN answers for an object of
// this type. Only types a browser cannot run script from are served inline.
// SVG keeps its type so `<img>` still renders it, but opening its URL
// downloads it instead of running its script. Everything else is stored as
// an opaque download under its uploaded name.
func servingMetadata(contentType, name string) BlobMetadata {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp", "image/gif", "application/pdf":
		return BlobMetadata{ContentType: contentType}
	case "image/svg+xml":
		return BlobMetadata{ContentType: contentType, ContentDisposition: attachment(name)}
	}
	return BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: attachment(name)}
}

// attachment encodes the name as a parameter (RFC 2231 when it is not plain
// ASCII), so a name can neither break the header nor add parameters to it.
func attachment(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
