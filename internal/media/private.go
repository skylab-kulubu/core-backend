package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/envelope"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// privateKeyPrefix starts the object key of every private Media. The key
// alone tells which bucket holds the object (Buckets).
const privateKeyPrefix = "private/"

// PrivateObjectKey is the key of an object core keeps in the private
// bucket under the given name.
func PrivateObjectKey(name string) string {
	return privateKeyPrefix + name
}

// isPrivateKey reports whether the object lives in the private bucket.
func isPrivateKey(key string) bool {
	return strings.HasPrefix(key, privateKeyPrefix)
}

var (
	// ErrPrivateUnavailable is private Media storage that cannot be reached
	// now: OpenBao is down, sealed, or refuses core's identity. Only
	// private uploads and reads fail; retrying later may work.
	ErrPrivateUnavailable = errors.New("media: private Media storage is unavailable")
	// ErrPrivateIntegrity is a private Media whose stored object or wrapped
	// key is not what core wrote: it is never served.
	ErrPrivateIntegrity = errors.New("media: private Media failed its integrity check")
)

// KeyWrapper wraps and unwraps data keys with the Transit key
// (transit.Client). A wrapped key names its key version.
type KeyWrapper interface {
	WrapKey(ctx context.Context, dataKey []byte) (wrapped string, version int, err error)
	UnwrapKey(ctx context.Context, wrapped string) ([]byte, error)
}

// SealedBlobStore is the private bucket. It holds only ciphertext.
type SealedBlobStore interface {
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
	// Open streams a stored object; ErrNotFound when there is none.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Encryption is how a private object is encrypted: the format, and its data
// key as Transit wrapped it, with the key version that did.
type Encryption struct {
	Algorithm  string `json:"algorithm"`
	WrappedKey string `json:"wrappedKey"`
	KeyVersion int    `json:"keyVersion"`
}

// SealedObject is an object in the private bucket and what opens it. The
// ciphertext is bound to its key: copied to another key, it does not open.
type SealedObject struct {
	Key        string
	Encryption Encryption
}

// Sealed is the Media's private object; false for a public Media.
func (m Media) Sealed() (SealedObject, bool) {
	if m.Visibility != VisibilityPrivate || m.Encryption == nil {
		return SealedObject{}, false
	}
	return SealedObject{Key: m.Key, Encryption: *m.Encryption}, true
}

// PrivateObjects stores and reads private objects (PrivateStorage).
type PrivateObjects interface {
	Seal(ctx context.Context, key string, plaintext []byte) (SealedObject, error)
	Read(ctx context.Context, obj SealedObject) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// PrivateStorage stores private Media: each object is encrypted with its
// own random 256-bit data key (envelope), bound to its object key, the data
// key is wrapped by the Transit key, and only ciphertext reaches the private
// bucket.
type PrivateStorage struct {
	blobs SealedBlobStore
	keys  KeyWrapper
}

func NewPrivateStorage(blobs SealedBlobStore, keys KeyWrapper) *PrivateStorage {
	return &PrivateStorage{blobs: blobs, keys: keys}
}

// sealedMetadata is what every private object is stored with: opaque bytes.
var sealedMetadata = BlobMetadata{ContentType: "application/octet-stream"}

// Seal encrypts plaintext under a new data key and stores it at key in the
// private bucket.
func (p *PrivateStorage) Seal(ctx context.Context, key string, plaintext []byte) (SealedObject, error) {
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return SealedObject{}, err
	}
	defer clear(dataKey)
	wrapped, version, err := p.keys.WrapKey(ctx, dataKey)
	if err != nil {
		return SealedObject{}, keyError(err)
	}
	var sealed bytes.Buffer
	sealed.Grow(len(plaintext) + envelope.HeaderSize + (len(plaintext)/envelope.SegmentSize+1)*16)
	w, err := envelope.NewWriter(&sealed, dataKey, []byte(key))
	if err != nil {
		return SealedObject{}, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return SealedObject{}, err
	}
	if err := w.Close(); err != nil {
		return SealedObject{}, err
	}
	if err := p.blobs.Put(ctx, key, sealed.Bytes(), sealedMetadata); err != nil {
		return SealedObject{}, err
	}
	return SealedObject{Key: key, Encryption: Encryption{Algorithm: envelope.Algorithm, WrappedKey: wrapped, KeyVersion: version}}, nil
}

// Open streams the plaintext of a private object. The first segment is
// checked before Open returns, so a wrong key or a changed start is an error
// here; a later segment that fails its check ends the stream with
// ErrPrivateIntegrity, naming the segment, and nothing of it is released.
func (p *PrivateStorage) Open(ctx context.Context, obj SealedObject) (io.ReadCloser, error) {
	enc := obj.Encryption
	if enc.Algorithm != envelope.Algorithm {
		return nil, fmt.Errorf("%w: unknown algorithm", ErrPrivateIntegrity)
	}
	if version, err := transit.KeyVersion(enc.WrappedKey); err != nil || version != enc.KeyVersion {
		return nil, fmt.Errorf("%w: the wrapped key is not of the recorded key version", ErrPrivateIntegrity)
	}
	dataKey, err := p.keys.UnwrapKey(ctx, enc.WrappedKey)
	if err != nil {
		return nil, keyError(err)
	}
	defer clear(dataKey)
	body, err := p.blobs.Open(ctx, obj.Key)
	if err != nil {
		return nil, err
	}
	plain, err := envelope.NewReader(body, dataKey, []byte(obj.Key))
	if err != nil {
		body.Close()
		return nil, integrityError(err)
	}
	first := make([]byte, 1)
	n, err := io.ReadFull(plain, first)
	if err != nil && !errors.Is(err, io.EOF) {
		body.Close()
		return nil, integrityError(err)
	}
	return &openedObject{Reader: io.MultiReader(bytes.NewReader(first[:n]), integrityReader{plain}), body: body}, nil
}

// Read returns the whole plaintext of a private object.
func (p *PrivateStorage) Read(ctx context.Context, obj SealedObject) ([]byte, error) {
	opened, err := p.Open(ctx, obj)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	return io.ReadAll(opened)
}

// Delete removes the object at key from the private bucket.
func (p *PrivateStorage) Delete(ctx context.Context, key string) error {
	return p.blobs.Delete(ctx, key)
}

type openedObject struct {
	io.Reader
	body io.Closer
}

func (o *openedObject) Close() error { return o.body.Close() }

type integrityReader struct{ r io.Reader }

func (r integrityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = integrityError(err)
	}
	return n, err
}

func integrityError(err error) error {
	if errors.Is(err, envelope.ErrCorrupt) {
		return fmt.Errorf("%w: %v", ErrPrivateIntegrity, err)
	}
	return err
}

// keyError sorts a Transit failure: an OpenBao that cannot help now (down,
// refusing core's identity, or without the mount or key core is configured
// with) is ErrPrivateUnavailable; a wrapped key it rejects is
// ErrPrivateIntegrity.
func keyError(err error) error {
	switch {
	case errors.Is(err, transit.ErrUnavailable), errors.Is(err, transit.ErrDenied), errors.Is(err, transit.ErrMisconfigured):
		return fmt.Errorf("%w: %v", ErrPrivateUnavailable, err)
	case errors.Is(err, transit.ErrRejected):
		return fmt.Errorf("%w: %v", ErrPrivateIntegrity, err)
	default:
		return err
	}
}
