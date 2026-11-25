package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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
// scan routes' service calls. Checked with CheckScannedAt first. It is kept
// to the microsecond, as the database keeps it, so the same scan sent again
// carries the same moment as its record.
func WithScannedAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, scannedAtKey{}, at.UTC().Truncate(time.Microsecond))
}

// DuplicateScanError is a scan sent later whose Ticket is already in: that
// venue day's entry (Kind entry) or that Session's Check-in (Kind
// check_in). It carries the record that is there (its ScannedAt nil for a
// scan core took as it came) and whether this is the same scan sent again
// (Resent: the record's scannedAt is this one's) or another door's.
// Nothing was written. It is the conflict it came from (ErrConflict, and
// an *EntryConflictError or ErrAlreadyCheckedIn).
type DuplicateScanError struct {
	Kind      string
	ID        uuid.UUID
	Name      string
	ScannedAt *time.Time
	CreatedAt time.Time
	Resent    bool
	err       error
}

func (e *DuplicateScanError) Error() string { return "ticket: duplicate scan: " + e.err.Error() }

// Unwrap is the conflict it came from.
func (e *DuplicateScanError) Unwrap() error { return e.err }

// ScanRecords read the record a duplicate scan met (MemoryStore,
// PostgresStore). A store without them answers duplicates without it.
type ScanRecords interface {
	// VenueEntryOn is the Ticket's Venue entry of the venue day date.
	VenueEntryOn(ctx context.Context, ticketID uuid.UUID, date string) (VenueEntry, error)
	// CheckInOf is the Ticket's Check-in at the Session.
	CheckInOf(ctx context.Context, ticketID, sessionID uuid.UUID) (CheckIn, error)
}

// duplicate turns err, a conflict of a scan sent later (ctx carries its
// scannedAt) with the record already there, into a *DuplicateScanError
// with that record, counting it; any other err, or a scan core took as it
// came, is err.
func (s *service) duplicate(ctx context.Context, err error, ticketID uuid.UUID, date string, sessionID uuid.UUID) error {
	at, offline := ScannedAtFrom(ctx)
	if !offline || err == nil {
		return err
	}
	var entered *EntryConflictError
	dup := &DuplicateScanError{err: err}
	reads, _ := s.tickets.(ScanRecords)
	switch {
	case errors.As(err, &entered):
		dup.Kind, dup.Name = KindEntry, entered.Name
		if reads != nil {
			if e, rerr := reads.VenueEntryOn(ctx, ticketID, date); rerr == nil {
				dup.ID, dup.ScannedAt, dup.CreatedAt = e.ID, e.ScannedAt, e.CreatedAt
			}
		}
	case errors.Is(err, ErrAlreadyCheckedIn):
		dup.Kind = KindCheckIn
		if reads != nil {
			if c, rerr := reads.CheckInOf(ctx, ticketID, sessionID); rerr == nil {
				dup.ID, dup.ScannedAt, dup.CreatedAt = c.ID, c.ScannedAt, c.CreatedAt
			}
		}
	default:
		return err
	}
	dup.Resent = dup.ScannedAt != nil && dup.ScannedAt.Equal(at)
	s.scans.recordOffline(dup)
	return dup
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
