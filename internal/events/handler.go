package events

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Handler does what an event asks of core. It runs in the database
// transaction that records the event as consumed: whatever it writes through
// tx commits with that record or not at all, so a repeat of the event (the
// same source and id) never has a second effect. The transaction commits
// when it returns nil; then the delivery is acked.
//
// What it returns decides the delivery:
//   - nil: done, acked.
//   - ErrUnrelated (wrapped or not): an event core has no business with (a
//     form core does not know); nothing is recorded, acked.
//   - Permanent(err): it can never succeed (data that fails validation, an
//     unknown schema version); rejected at once, to the DLQ.
//   - any other error: transient (the database, a peer's 5xx or 429); tried
//     again in the process after 1, 5 and 30 seconds, then to the DLQ.
//
// A handler that calls another service should be idempotent on its own as
// well: a call that went out is not undone by a rollback.
type Handler func(ctx context.Context, tx pgx.Tx, event Event) error

// ErrUnrelated is a handler's answer for an event that is not core's
// business: acked, not recorded.
var ErrUnrelated = errors.New("events: unrelated event")

// permanentError marks an error a retry cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as one a retry cannot fix: the delivery goes to the
// DLQ without another try. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, was marked
// Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
