// Package competition is the club's competition module (ADR-0068): a
// Competition belongs to one Event, has Stages that each score through one
// Scoring strategy, and Entrants whose members are the Event's Tickets.
// Scores come from an append-only ledger; a correction revokes a row with a
// reason and writes a new one. Contract: docs/competitions.md.
package competition

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Status is where a Competition is in its life (spec §7). Archiving is
// separate: an archived Competition keeps its status and is hidden on
// reads until it is restored.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusOpen      Status = "open"
	StatusRunning   Status = "running"
	StatusClosed    Status = "closed"
	StatusCancelled Status = "cancelled"
)

// Entrant modes. Only individual Entrants exist yet; teams come with
// ticket 11.
const (
	ModeIndividual = "individual"
	ModeTeam       = "team"
	ModeBoth       = "both"
)

// Entrant kinds and statuses.
const (
	KindIndividual = "individual"
	KindTeam       = "team"

	EntrantActive       = "active"
	EntrantWithdrawn    = "withdrawn"
	EntrantDisqualified = "disqualified"
)

type Competition struct {
	ID      uuid.UUID `json:"id"`
	EventID uuid.UUID `json:"eventId"`
	// OwnerTeam is the Event's; the Competition does not store it.
	OwnerTeam    string     `json:"ownerTeam"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	EntrantMode  string     `json:"entrantMode"`
	Status       Status     `json:"status"`
	CancelReason string     `json:"cancelReason,omitempty"`
	Stages       []Stage    `json:"stages"`
	CreatedBy    *uuid.UUID `json:"createdBy,omitempty"`
	ArchivedAt   *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy   *uuid.UUID `json:"archivedBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Stage is a part of a Competition with one Scoring strategy. A new
// Competition has one, which the admin panel does not show.
type Stage struct {
	ID            uuid.UUID       `json:"id"`
	CompetitionID uuid.UUID       `json:"competitionId"`
	Position      int             `json:"position"`
	Name          string          `json:"name"`
	Strategy      string          `json:"strategy"`
	Config        json.RawMessage `json:"config"`
	ConfigVersion int             `json:"configVersion"`
	StartsAt      *time.Time      `json:"startsAt,omitempty"`
	EndsAt        *time.Time      `json:"endsAt,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// Entrant is one row of the standings: a person, or later a team.
type Entrant struct {
	ID            uuid.UUID `json:"id"`
	CompetitionID uuid.UUID `json:"competitionId"`
	Kind          string    `json:"kind"`
	TeamName      string    `json:"teamName,omitempty"`
	Status        string    `json:"status"`
	StatusReason  string    `json:"statusReason,omitempty"`
	// DisplayName is the person's name, or the team's.
	DisplayName string    `json:"displayName"`
	Members     []Member  `json:"members"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Member is a Ticket of the Event in an Entrant.
type Member struct {
	TicketID    uuid.UUID `json:"ticketId"`
	TicketType  string    `json:"ticketType"`
	DisplayName string    `json:"displayName"`
}

// Entry is one ledger row: a score typed in, later a submission or an
// imported result.
type Entry struct {
	ID            uuid.UUID       `json:"id"`
	CompetitionID uuid.UUID       `json:"competitionId"`
	StageID       uuid.UUID       `json:"stageId"`
	EntrantID     uuid.UUID       `json:"entrantId"`
	Source        string          `json:"source"`
	Points        *scoring.Points `json:"points"`
	OccurredAt    time.Time       `json:"occurredAt"`
	RecordedAt    time.Time       `json:"recordedAt"`
	RecordedBy    *uuid.UUID      `json:"recordedBy,omitempty"`
	// ReplacesID is the row this one corrected, revoked in the same write.
	ReplacesID   *uuid.UUID `json:"replacesId,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	RevokedBy    *uuid.UUID `json:"revokedBy,omitempty"`
	RevokeReason string     `json:"revokeReason,omitempty"`
}

// Leaderboard is a Stage's live standings for its organizers.
type Leaderboard struct {
	CompetitionID uuid.UUID        `json:"competitionId"`
	StageID       uuid.UUID        `json:"stageId"`
	Strategy      string           `json:"strategy"`
	Rows          []LeaderboardRow `json:"rows"`
}

// LeaderboardRow is an active Entrant's place. An Entrant without a score
// yet has neither rank nor score and comes after the ranked ones.
type LeaderboardRow struct {
	Rank        *int            `json:"rank"`
	EntrantID   uuid.UUID       `json:"entrantId"`
	DisplayName string          `json:"displayName"`
	Members     []Member        `json:"members"`
	Score       *scoring.Points `json:"score"`
}

// CreateInput is a new Competition of an Event.
type CreateInput struct {
	Name        string
	Description string
	EntrantMode string
}

// UpdateInput changes what is given.
type UpdateInput struct {
	Name        *string
	Description *string
}

// ScoreInput sets an Entrant's manual score in a Stage (the first one when
// StageID is nil). Reason is required when it replaces a score.
type ScoreInput struct {
	EntrantID uuid.UUID
	StageID   *uuid.UUID
	Points    scoring.Points
	Reason    string
}

// ManualScore is what the store writes for a ScoreInput.
type ManualScore struct {
	CompetitionID uuid.UUID
	StageID       uuid.UUID
	EntrantID     uuid.UUID
	Points        scoring.Points
	Reason        string
	Actor         *uuid.UUID
}

// TicketRef is what entrant creation checks of a Ticket.
type TicketRef struct {
	ID      uuid.UUID
	EventID uuid.UUID
}

// memberName is how an Entrant member is shown: a registered Ticket's owner
// by name, "Silinmiş kullanıcı" once the account is being deleted or gone,
// a guest Ticket by the guest's name until it is erased.
func memberName(ticketType string, ownerState user.AccountState, ownerFirst, ownerLast, guestFirst, guestLast string) string {
	var name string
	if ticketType == ticket.Registered {
		if ownerState == user.AccountActive {
			name = ownerFirst + " " + ownerLast
		}
	} else {
		name = guestFirst + " " + guestLast
	}
	if name = strings.TrimSpace(name); name == "" {
		return user.DeletedDisplayName
	}
	return name
}
