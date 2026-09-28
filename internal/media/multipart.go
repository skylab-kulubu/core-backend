package media

import (
	"context"
	"errors"
	"strings"
	"time"
)

// pendingKeyPrefix starts the object key of a Direct upload until core
// completes it: the browser writes its parts under pending/<uuid>, core
// copies the finished object to its final key and deletes this one. The R2
// lifecycle rule (ops wizard media-direct-upload-r2-wizard.sh) deletes what
// is left under the prefix, and aborts multipart uploads left open, after
// two days.
const pendingKeyPrefix = "pending/"

// pendingMetadata is what a pending object is stored with: an opaque
// download, whatever the browser sent.
var pendingMetadata = BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment"}

// isPendingKey reports whether the key is a Direct upload's pending object,
// in the public bucket (pending/…) or the private one (private/pending/…).
func isPendingKey(key string) bool {
	return strings.HasPrefix(strings.TrimPrefix(key, privateKeyPrefix), pendingKeyPrefix)
}

var (
	// ErrMultipartGone is a multipart upload storage no longer has: it was
	// completed or aborted.
	ErrMultipartGone = errors.New("media: the multipart upload is gone")
	// ErrMultipartPartsMismatch is a completion whose parts are not the
	// parts storage holds: a part missing, out of order, or another ETag.
	ErrMultipartPartsMismatch = errors.New("media: the parts do not match the multipart upload")
)

// UploadedPart is one part of a multipart upload: its number from 1, its
// ETag as storage gave it, and its size in bytes (read back from storage;
// not needed to complete).
type UploadedPart struct {
	Number int32  `json:"partNumber"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size,omitempty"`
}

// MultipartStore is the bucket side of Direct upload: an S3 multipart upload
// the browser fills through presigned part addresses, which core then
// completes, checks, and copies to its final key. R2 is one.
type MultipartStore interface {
	// CreateMultipart opens a multipart upload at key, whose object is
	// stored as an opaque download (pendingMetadata).
	CreateMultipart(ctx context.Context, key string) (uploadID string, err error)
	// PresignPart is the address a browser PUTs one part to: signed for
	// exactly size bytes, valid for ttl.
	PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, ttl time.Duration) (string, error)
	// ListParts is the parts storage holds, by number; ErrMultipartGone once
	// the upload is completed or aborted.
	ListParts(ctx context.Context, key, uploadID string) ([]UploadedPart, error)
	// CompleteMultipart joins the parts into the object at key;
	// ErrMultipartPartsMismatch when they are not the stored parts,
	// ErrMultipartGone when the upload is.
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []UploadedPart) error
	// Size is the stored object's size; ErrNotFound when there is none.
	Size(ctx context.Context, key string) (int64, error)
	// ReadStart reads at most n bytes from the start of the object;
	// ErrNotFound when there is none.
	ReadStart(ctx context.Context, key string, n int) ([]byte, error)
	// Copy copies the object at from to the key to, stored with meta;
	// ErrNotFound when there is none.
	Copy(ctx context.Context, from, to string, meta BlobMetadata) error
	// Delete removes the object at key. At a pending key it also aborts
	// any multipart upload still open there.
	Delete(ctx context.Context, key string) error
}
