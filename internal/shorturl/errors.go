package shorturl

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound  = errors.New("shorturl: not found")
	ErrForbidden = errors.New("shorturl: forbidden")
	ErrInvalid   = errors.New("shorturl: invalid")
	ErrConflict  = errors.New("shorturl: alias exists")
	// ErrManaged rejects a change through the generic URL endpoints to a link
	// that a form or an event owns.
	ErrManaged = errors.New("shorturl: managed by a form or an event")
	// ErrEventManaged rejects a form-side rename of a link an event owns; that
	// alias only changes when the event is saved.
	ErrEventManaged = errors.New("shorturl: managed by an event")
	// ErrGone is a hop to an alias that once named a link which is now
	// disabled. It is an ErrNotFound: the JSON answer stays a 404, and only a
	// browser is told the link is gone (410, docs/short-link-miss-page.md).
	ErrGone = fmt.Errorf("%w: link disabled", ErrNotFound)
)
