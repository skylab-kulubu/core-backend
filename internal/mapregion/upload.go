package mapregion

import (
	"context"
	"errors"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Uploader is the public bucket's multipart upload (media.R2).
type Uploader interface {
	CreateMultipartWith(ctx context.Context, key string, meta media.BlobMetadata) (string, error)
	UploadPart(ctx context.Context, key, uploadID string, number int32, data []byte) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []media.UploadedPart) error
	AbortMultipartUploads(ctx context.Context, key string) error
}

// partSize is every part's size but the last's: R2 wants them equal.
const partSize = 16 << 20

// parts writes an object as a multipart upload, a part of partSize at a
// time; after tells it the bytes uploaded so far.
type parts struct {
	ctx      context.Context
	uploader Uploader
	key, id  string
	buf      []byte
	done     []media.UploadedPart
	sent     int64
	after    func(sent int64) error
}

func (p *parts) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		room := partSize - len(p.buf)
		take := min(room, len(b))
		p.buf = append(p.buf, b[:take]...)
		b = b[take:]
		if len(p.buf) == partSize {
			if err := p.flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (p *parts) flush() error {
	if len(p.buf) == 0 {
		return nil
	}
	number := int32(len(p.done) + 1)
	etag, err := p.uploader.UploadPart(p.ctx, p.key, p.id, number, p.buf)
	if err != nil {
		return err
	}
	p.done = append(p.done, media.UploadedPart{Number: number, ETag: etag, Size: int64(len(p.buf))})
	p.sent += int64(len(p.buf))
	p.buf = p.buf[:0]
	if p.after != nil {
		return p.after(p.sent)
	}
	return nil
}

// close sends the last part and completes the upload.
func (p *parts) close() error {
	if err := p.flush(); err != nil {
		return err
	}
	if len(p.done) == 0 {
		return errors.New("mapregion: an empty extract")
	}
	return p.uploader.CompleteMultipart(p.ctx, p.key, p.id, p.done)
}
