package media

import (
	"errors"
	"fmt"
)

// Upload refusals by a Media purpose. Each except ErrPrivateMediaDisabled and
// ErrPurposeNotAvailable also matches ErrInvalid or ErrForbidden, so a caller
// that only knows those keeps working.
var (
	ErrPurposeUnknown   = fmt.Errorf("media: unknown purpose: %w", ErrInvalid)
	ErrPurposeForbidden = fmt.Errorf("media: this uploader may not upload for the purpose: %w", ErrForbidden)
	ErrTypeNotAllowed   = fmt.Errorf("media: type not allowed for its purpose: %w", ErrInvalid)
	ErrTooLarge         = fmt.Errorf("media: too large for its purpose: %w", ErrInvalid)
	// ErrImageTooLarge refuses an image whose header says decoding it would
	// take more than core allows (PurposeRefusal.MaxPixels).
	ErrImageTooLarge    = fmt.Errorf("media: image too large to decode: %w", ErrInvalid)
	ErrDirectUploadOnly = fmt.Errorf("media: the purpose uploads by Direct upload: %w", ErrInvalid)
	// ErrPrivateMediaDisabled refuses a private purpose while core has no
	// private Media storage. A private purpose is never stored publicly
	// instead.
	ErrPrivateMediaDisabled = errors.New("media: private Media is not enabled")
	// ErrPurposeNotAvailable refuses a service purpose whose product has no
	// service client configured, or that names no product: nothing could
	// attach the Media before it expires.
	ErrPurposeNotAvailable = errors.New("media: nothing can attach Media of this purpose yet")
)

// PurposeRefusal is an upload its Media purpose refuses. errors.Is matches
// its Err.
type PurposeRefusal struct {
	Err     error
	Purpose string
	// AllowedTypes are the purpose's types, with ErrTypeNotAllowed.
	AllowedTypes []string
	// MaxBytes is the purpose's maximum size, with ErrTooLarge.
	MaxBytes int64
	// MaxPixels is the most pixels core decodes in an image like this one,
	// with ErrImageTooLarge.
	MaxPixels int64
}

func (r *PurposeRefusal) Error() string {
	return fmt.Sprintf("%v (purpose %q)", r.Err, r.Purpose)
}

func (r *PurposeRefusal) Unwrap() error { return r.Err }
