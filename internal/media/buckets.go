package media

import (
	"context"
	"errors"
)

// errPrivateObject refuses to treat an object of the private bucket as a
// public one.
var errPrivateObject = errors.New("media: a private object is only sealed and opened through PrivateStorage")

// Buckets is the public and the private bucket as one BlobStore, told apart
// by the object key (PrivateObjectKey). The work that removes objects by key
// alone (the blob purge, the expiry cleanup, the staged upload sweeper,
// account erasure) goes through it, so a private object is deleted from the
// private bucket. Only Delete reaches the private bucket: a private object is
// never read, written or given serving metadata as a public one.
type Buckets struct {
	Public BlobStore
	// Private is nil while private Media is off; deleting a private object
	// then fails, so a purge retries instead of recording one that did not
	// happen.
	Private *PrivateStorage
}

func (b Buckets) Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error {
	if isPrivateKey(key) {
		return errPrivateObject
	}
	return b.Public.Put(ctx, key, data, meta)
}

func (b Buckets) SetMetadata(ctx context.Context, key string, meta BlobMetadata) error {
	if isPrivateKey(key) {
		return errPrivateObject
	}
	return b.Public.SetMetadata(ctx, key, meta)
}

func (b Buckets) Read(ctx context.Context, key string) ([]byte, error) {
	if isPrivateKey(key) {
		return nil, errPrivateObject
	}
	return b.Public.Read(ctx, key)
}

func (b Buckets) Delete(ctx context.Context, key string) error {
	if !isPrivateKey(key) {
		return b.Public.Delete(ctx, key)
	}
	if b.Private == nil {
		return ErrPrivateMediaDisabled
	}
	return b.Private.Delete(ctx, key)
}
