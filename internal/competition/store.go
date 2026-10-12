package competition

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

var (
	ErrNotFound  = errors.New("competition: not found")
	ErrForbidden = errors.New("competition: forbidden")
	ErrInvalid   = errors.New("competition: invalid")
	ErrConflict  = errors.New("competition: conflict")
	// ErrStatus is an action the Competition's status does not allow.
	ErrStatus = errors.New("competition: not allowed in this status")
	// ErrReasonRequired is a correction or revocation without a reason.
	ErrReasonRequired = errors.New("competition: reason required")
	// ErrTicketTaken is a Ticket that already is in an Entrant of the
	// Competition.
	ErrTicketTaken = errors.New("competition: ticket already an entrant")
	// ErrScoreChanged is a score write whose caller did not see the live
	// score: another write got in first. Nothing is written.
	ErrScoreChanged = errors.New("competition: the score changed")
	// ErrUnknownEntrant and ErrUnknownStage are a score for an Entrant or
	// Stage that is not an active one of this Competition.
	ErrUnknownEntrant = fmt.Errorf("%w: not an active entrant of this competition", ErrInvalid)
	ErrUnknownStage   = fmt.Errorf("%w: not a stage of this competition", ErrInvalid)
)

// ScoreChangedError is ErrScoreChanged with the live score the write did
// not expect (nil when there is none).
type ScoreChangedError struct {
	Current *Entry
}

func (e *ScoreChangedError) Error() string { return ErrScoreChanged.Error() }

func (e *ScoreChangedError) Is(target error) bool { return target == ErrScoreChanged }

// Store keeps Competitions and their ledger. A ledger row is never updated
// but for its revocation, and never deleted.
type Store interface {
	// Create writes the Competition with its first Stage.
	Create(ctx context.Context, c Competition, first Stage) (Competition, error)
	// Get is a Competition that is not archived, with its Stages in order.
	Get(ctx context.Context, id uuid.UUID) (Competition, error)
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (Competition, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Competition, error)
	// Update writes Name and Description.
	Update(ctx context.Context, c Competition) (Competition, error)
	// Transition moves the Competition to status `to` when its status is
	// one of from (ErrStatus otherwise), with reason for a cancellation.
	Transition(ctx context.Context, id uuid.UUID, from []Status, to Status, reason string) (Competition, error)
	Archive(ctx context.Context, id uuid.UUID, actor *uuid.UUID) error
	Restore(ctx context.Context, id uuid.UUID) error

	// Tickets are the Tickets of ids that exist.
	Tickets(ctx context.Context, ids []uuid.UUID) ([]TicketRef, error)
	// AddIndividuals writes one individual Entrant per Ticket, all or none:
	// ErrTicketTaken when one already is in an Entrant. It returns the new
	// Entrants' ids in the Tickets' order.
	AddIndividuals(ctx context.Context, competitionID uuid.UUID, ticketIDs []uuid.UUID, actor *uuid.UUID) ([]uuid.UUID, error)
	// Entrants are the Competition's Entrants, oldest first, with their
	// members' names as they are now.
	Entrants(ctx context.Context, competitionID uuid.UUID) ([]Entrant, error)

	// SetManualScore writes the Entrant's manual score in the Stage, in one
	// transaction. With in.ReplacesID it revokes that live score with
	// in.Reason (ErrReasonRequired when empty); when the live score is
	// another one, or none, nothing is written (ScoreChangedError). Without
	// it, a live score is revoked only when in.Reason is given; otherwise
	// nothing is written (ScoreChangedError).
	SetManualScore(ctx context.Context, in ManualScore) (Entry, error)
	// Revoke revokes a live ledger row of the Competition (ErrConflict when
	// it already is).
	Revoke(ctx context.Context, competitionID, entryID uuid.UUID, actor *uuid.UUID, reason string) (Entry, error)
	// Entries are the Competition's ledger rows, revoked ones included,
	// oldest first.
	Entries(ctx context.Context, competitionID uuid.UUID) ([]Entry, error)
}

// checkLive is SetManualScore's rule, the same in every store: whether the
// write may replace the live score it found (hasLive; current reads it).
func checkLive(in ManualScore, hasLive bool, current func() *Entry) error {
	changed := func() error {
		if hasLive {
			return &ScoreChangedError{Current: current()}
		}
		return &ScoreChangedError{}
	}
	switch {
	case in.ReplacesID != nil:
		if !hasLive || current().ID != *in.ReplacesID {
			return changed()
		}
		if in.Reason == "" {
			return ErrReasonRequired
		}
	case hasLive && in.Reason == "":
		return changed()
	}
	return nil
}
