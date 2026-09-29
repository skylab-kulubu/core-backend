package media

import (
	"encoding/json"
	"time"
)

// LinkedImage is a Media as a record that links it as an image reads it in
// its own query: an Event's cover or gallery image, a User's profile
// picture. It holds what the image's addresses are built from
// (Addresses.LinkedSizes), so a list of records costs no query per Media.
type LinkedImage struct {
	media Media
}

// LinkedImageOf is m as a record's query reads it (LinkedImageSQL), for a
// store that reads the Media it links another way: the memory stores.
func LinkedImageOf(m Media) *LinkedImage {
	return &LinkedImage{media: m}
}

// LinkedImageSQL is the SQL expression a record's query selects for the
// Media it links, the media row aliased alias (a trusted identifier, never
// input): NULL when the record links none. Scan it into a *LinkedImage
// (a **LinkedImage destination), which a NULL leaves nil.
//
// It maps media columns to Media fields as mediaCols and scanMedia
// (postgres.go) do, as one JSON object rather than positional columns; a
// column those two change here, with linkedImageColumns.
func LinkedImageSQL(alias string) string {
	return `CASE WHEN ` + alias + `.id IS NULL THEN NULL ELSE jsonb_build_object(
		'key', ` + alias + `.file_url,
		'kind', ` + alias + `.kind,
		'type', ` + alias + `.file_type,
		'purpose', ` + alias + `.purpose,
		'visibility', ` + alias + `.visibility,
		'width', ` + alias + `.width,
		'height', ` + alias + `.height,
		'sizeObjects', ` + alias + `.size_objects,
		'status', ` + alias + `.status,
		'blobPurgeStartedAt', ` + alias + `.blob_purge_started_at,
		'blobPurgedAt', ` + alias + `.blob_purged_at) END`
}

// ServedKeySQL is the SQL expression a record's query selects for the object
// key of the Media it links, the media row aliased alias (a trusted
// identifier, never input): NULL for a Media waiting for its malware scan or
// rejected by it, whose key must never become an address (a held file's key
// is known only to core).
func ServedKeySQL(alias string) string {
	return `CASE WHEN ` + alias + `.status IN ('` + string(StatusScanning) + `', '` + string(StatusRejected) + `') THEN NULL ELSE ` + alias + `.file_url END`
}

// ServableSQL is Media.Servable as an SQL condition on the media row aliased
// alias (a trusted identifier, never input): the Media is public, its
// object neither being nor already purged, and it is neither waiting for its
// malware scan nor rejected by it. Records that list Media with their
// addresses (an Event's files and videos) and count them use it, so what
// they show and count is what the Media JSON serves; a test keeps the two
// equal.
func ServableSQL(alias string) string {
	return `(` + alias + `.visibility = '` + string(VisibilityPublic) + `' AND ` + alias + `.blob_purge_started_at IS NULL AND ` +
		alias + `.blob_purged_at IS NULL AND ` + alias + `.status NOT IN ('` + string(StatusScanning) + `', '` + string(StatusRejected) + `'))`
}

// ServableKeySQL is the object key of the Media row aliased alias when it
// can be served (ServableSQL), and NULL otherwise: the key a record answers
// an address for.
func ServableKeySQL(alias string) string {
	return `CASE WHEN ` + ServableSQL(alias) + ` THEN ` + alias + `.file_url END`
}

// linkedImageColumns are the fields LinkedImageSQL builds, named for the
// Media fields scanMedia fills from the same columns.
type linkedImageColumns struct {
	Key                string                `json:"key"`
	Kind               string                `json:"kind"`
	Type               string                `json:"type"`
	Purpose            string                `json:"purpose"`
	Visibility         Visibility            `json:"visibility"`
	Width              int                   `json:"width"`
	Height             int                   `json:"height"`
	SizeObjects        map[string]SizeObject `json:"sizeObjects"`
	Status             Status                `json:"status"`
	BlobPurgeStartedAt *time.Time            `json:"blobPurgeStartedAt"`
	BlobPurgedAt       *time.Time            `json:"blobPurgedAt"`
}

// Key is the linked Media's object key; empty for no Media (a nil
// LinkedImage), and for a Media waiting for its malware scan or rejected by
// it, whose key never becomes an address.
func (l *LinkedImage) Key() string {
	if l == nil || l.media.openable() != nil {
		return ""
	}
	return l.media.Key
}

// UnmarshalJSON reads the value LinkedImageSQL selects.
func (l *LinkedImage) UnmarshalJSON(data []byte) error {
	var columns linkedImageColumns
	if err := json.Unmarshal(data, &columns); err != nil {
		return err
	}
	l.media = Media{
		Key: columns.Key, Kind: columns.Kind, Type: columns.Type, Purpose: columns.Purpose,
		Visibility: columns.Visibility, Width: columns.Width, Height: columns.Height, SizeObjects: columns.SizeObjects,
		Status: columns.Status, BlobPurgeStartedAt: columns.BlobPurgeStartedAt, BlobPurgedAt: columns.BlobPurgedAt,
	}
	return nil
}
