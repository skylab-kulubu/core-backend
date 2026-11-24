package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The offline door (docs/offline-door.md): a door app that lost the
// network keeps scanning Ticket QRs (verified on the device with
// /v1/door/jwks) and sends them later, each with the moment it was taken on
// the device, scannedAt. Core decides that scan as of scannedAt: the Ticket
// QR's expiry, the Event's door window and the venue day are read at it.
// The record keeps both: created_at is when core wrote it, scanned_at when
// the device took it. Everything else (the Event's status, the Ticket's
// status, who may scan) is read now.

const (
	// ScanFutureSkew is how far ahead of core's clock a scannedAt may be: a
	// phone's clock a little fast.
	ScanFutureSkew = 5 * time.Minute
	// ScanMaxAge is how old a scannedAt may be when it reaches core: a door
	// offline for a weekend's Event still syncs; anything older is refused.
	ScanMaxAge = 72 * time.Hour
	// ScanMailLate is how old a scan may be and still send the door's mails
	// (docs/attendance-mail.md): a welcome or a receipt that would arrive an
	// hour or more after the person came in is not sent.
	ScanMailLate = time.Hour
)

var (
	// ErrScannedAtFuture is a scannedAt more than ScanFutureSkew ahead.
	ErrScannedAtFuture = errors.New("ticket: scanned at is in the future")
	// ErrScannedAtTooOld is a scannedAt more than ScanMaxAge ago.
	ErrScannedAtTooOld = errors.New("ticket: scanned at is too old")
	// ErrAlreadyCheckedIn is a second Check-in of a Ticket at a Session. It
	// is ErrConflict.
	ErrAlreadyCheckedIn = fmt.Errorf("%w: already checked in", ErrConflict)
)

// CheckScannedAt refuses a scannedAt too far ahead of now
// (ErrScannedAtFuture) or too old (ErrScannedAtTooOld).
func CheckScannedAt(at, now time.Time) error {
	switch {
	case at.After(now.Add(ScanFutureSkew)):
		return ErrScannedAtFuture
	case at.Before(now.Add(-ScanMaxAge)):
		return ErrScannedAtTooOld
	}
	return nil
}

type scannedAtKey struct{}

// WithScannedAt is ctx carrying the moment the door took the scan, for the
// scan routes' service calls. Checked with CheckScannedAt first.
func WithScannedAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, scannedAtKey{}, at.UTC())
}

// ScannedAtFrom is the moment ctx says the scan was taken.
func ScannedAtFrom(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(scannedAtKey{}).(time.Time)
	return at, ok
}

// scanTime is the moment the door decides a scan at: its scannedAt, else
// now.
func (s *service) scanTime(ctx context.Context) time.Time {
	if at, ok := ScannedAtFrom(ctx); ok {
		return at
	}
	return s.now()
}

// scanned is the scannedAt a write keeps, and whether the scan is too late
// for the door's mails.
func (s *service) scanned(ctx context.Context) (*time.Time, bool) {
	at, ok := ScannedAtFrom(ctx)
	if !ok {
		return nil, false
	}
	return &at, at.Before(s.now().Add(-ScanMailLate))
}
