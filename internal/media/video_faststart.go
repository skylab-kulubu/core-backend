package media

import (
	"context"
	"io"
	"strings"
)

// Video faststart (media redesign ticket 13): a video's MP4 whose moov box
// comes after its media data plays only once it has downloaded whole. The
// faststart worker (FaststartWorker) rewrites it with its moov in front
// (internal/faststart, qt-faststart's method in Go: no ffmpeg, no
// re-encode) into a new object beside it, and points the Media there. See
// "Video faststart" in docs/media-lifecycle.md.

// A video's two keys: the one a Direct upload stores it at
// (videos/<uuid>.mp4, directServedKey) and its faststart copy beside it
// (videos/<uuid>.fs.mp4). Each names the other, so every purge of the Media
// deletes both, whichever it points at (purgeMediaObjects).
const (
	videoKeyPrefix     = "videos/"
	videoKeySuffix     = ".mp4"
	faststartKeySuffix = ".fs.mp4"
)

// videoName is the name in a video's key, videos/<name>.mp4 or
// videos/<name>.fs.mp4, and whether the key is its faststart copy's.
func videoName(key string) (name string, faststart, ok bool) {
	rest, ok := strings.CutPrefix(key, videoKeyPrefix)
	if !ok || strings.Contains(rest, "/") {
		return "", false, false
	}
	if name, ok := strings.CutSuffix(rest, faststartKeySuffix); ok {
		// Never an original named "<name>.fs".
		return name, true, name != ""
	}
	if name, ok := strings.CutSuffix(rest, videoKeySuffix); ok && name != "" {
		return name, false, true
	}
	return "", false, false
}

// faststartKeyOf is the key of the faststart copy of the video at key; ok
// is false for a key that is not a video's original.
func faststartKeyOf(key string) (string, bool) {
	name, faststart, ok := videoName(key)
	if !ok || faststart {
		return "", false
	}
	return videoKeyPrefix + name + faststartKeySuffix, true
}

// faststartSourceOf is the key of the original of the faststart copy at
// key; ok is false for a key that is not a faststart copy's.
func faststartSourceOf(key string) (string, bool) {
	name, faststart, ok := videoName(key)
	if !ok || !faststart {
		return "", false
	}
	return videoKeyPrefix + name + videoKeySuffix, true
}

// isFaststartKey reports whether key is a video's faststart copy.
func isFaststartKey(key string) bool {
	_, ok := faststartSourceOf(key)
	return ok
}

// videoPairKey is the other key of a video's pair: the faststart copy of an
// original, the original of a faststart copy.
func videoPairKey(key string) (string, bool) {
	if other, ok := faststartKeyOf(key); ok {
		return other, true
	}
	return faststartSourceOf(key)
}

// FaststartStorage is the public bucket as the faststart rewrite uses it
// (R2): it reads the video by ranges, writes the faststart copy (a small one
// whole, a larger one as a multipart upload whose parts storage copies
// from the video where it can), checks it, and deletes.
type FaststartStorage interface {
	// Size is the stored object's size; ErrNotFound when there is none.
	Size(ctx context.Context, key string) (int64, error)
	// OpenRange streams the n bytes of the object from off (a ranged GET);
	// ErrNotFound when there is none.
	OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
	// CreateMultipartWith opens a multipart upload at key, stored with meta
	// once completed.
	CreateMultipartWith(ctx context.Context, key string, meta BlobMetadata) (string, error)
	// UploadPart sends one part's bytes.
	UploadPart(ctx context.Context, key, uploadID string, number int32, data []byte) (string, error)
	// UploadPartCopy has storage copy n bytes of the object at from, from
	// off, as one part.
	UploadPartCopy(ctx context.Context, key, uploadID string, number int32, from string, off, n int64) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []UploadedPart) error
	// Delete removes the object at key; at a faststart key it also aborts
	// any multipart upload still open there.
	Delete(ctx context.Context, key string) error
}
