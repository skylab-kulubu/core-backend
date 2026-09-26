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

// LinkedImageSQL is the SQL expression a record's query selects for the
// Media it links, the media row aliased alias (a trusted identifier, never
// input): NULL when the record links none. Scan it into a *LinkedImage
// (a **LinkedImage destination), which a NULL leaves nil.
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
		'blobPurgeStartedAt', ` + alias + `.blob_purge_started_at,
		'blobPurgedAt', ` + alias + `.blob_purged_at) END`
}

// linkedImageColumns are the fields LinkedImageSQL builds.
type linkedImageColumns struct {
	Key                string                `json:"key"`
	Kind               string                `json:"kind"`
	Type               string                `json:"type"`
	Purpose            string                `json:"purpose"`
	Visibility         Visibility            `json:"visibility"`
	Width              int                   `json:"width"`
	Height             int                   `json:"height"`
	SizeObjects        map[string]SizeObject `json:"sizeObjects"`
	BlobPurgeStartedAt *time.Time            `json:"blobPurgeStartedAt"`
	BlobPurgedAt       *time.Time            `json:"blobPurgedAt"`
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
		BlobPurgeStartedAt: columns.BlobPurgeStartedAt, BlobPurgedAt: columns.BlobPurgedAt,
	}
	return nil
}
