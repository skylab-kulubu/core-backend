package shorturl

import "errors"

var (
	ErrNotFound  = errors.New("shorturl: not found")
	ErrForbidden = errors.New("shorturl: forbidden")
	ErrInvalid   = errors.New("shorturl: invalid")
	ErrConflict  = errors.New("shorturl: alias exists")
)
