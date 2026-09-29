package event

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

var (
	ErrNotFound  = errors.New("event: not found")
	ErrForbidden = errors.New("event: forbidden")
	ErrInvalid   = errors.New("event: invalid")
	ErrConflict  = errors.New("event: conflict")
	// ErrMediaTeamMismatch refuses a Media another Owner team's Event uses
	// (Team media library). It comes in a *media.LinkRefusal.
	ErrMediaTeamMismatch = fmt.Errorf("event: the Media is used on another Owner team's Event: %w", ErrForbidden)
)

type Store interface {
	List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error)
	ListLifecycle(ctx context.Context, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error)
	Get(ctx context.Context, id uuid.UUID) (Event, error)
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (Event, error)
	Create(ctx context.Context, e Event) (Event, error)
	Update(ctx context.Context, e Event) (Event, error)
	Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	Restore(ctx context.Context, id uuid.UUID) error
	AddImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error)
	RemoveImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error)
	// ListFiles reads the Event's files and videos, each list in its
	// organizers' order (empty lists, never nil). Get leaves them out: it
	// answers their counts alone.
	ListFiles(ctx context.Context, eventID uuid.UUID) (files, videos []MediaItem, err error)
	// AddFiles appends the Media to the Event's list, in the order given,
	// leaving out those already in it.
	AddFiles(ctx context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error)
	// RemoveFiles removes the Media from the Event's list: all of them, or,
	// when one is not in it, none (ErrNotFound).
	RemoveFiles(ctx context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error)
	// OrderFiles puts the Event's list in the order of ids, which name each
	// of its items once: ErrConflict when they are not the list's items.
	OrderFiles(ctx context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error)
	// SetVideoPoster links the poster Media to the Event's video, in place
	// of the one it had, or clears it (nil): ErrNotFound when the Event
	// does not list the video.
	SetVideoPoster(ctx context.Context, eventID, videoID uuid.UUID, posterID *uuid.UUID) (Event, error)
	// TeamsUsingMedia returns the Owner teams of the Events, archived ones
	// included, that use the Media as their cover, in their gallery, among
	// their files or videos, or as a video's poster, leaving out the Event
	// except.
	TeamsUsingMedia(ctx context.Context, mediaID, except uuid.UUID) ([]string, error)
	GetDay(ctx context.Context, id uuid.UUID) (Day, error)
	GetDayIncludingArchived(ctx context.Context, id uuid.UUID) (Day, error)
	CreateDay(ctx context.Context, d Day) (Day, error)
	ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error)
	ListDaysLifecycle(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error)
	UpdateDay(ctx context.Context, d Day) (Day, error)
	ArchiveDay(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	RestoreDay(ctx context.Context, id uuid.UUID) error
	ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error)
	SetSeason(ctx context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error)
	SetMailListID(ctx context.Context, eventID, listID uuid.UUID) (Event, error)
	GetSession(ctx context.Context, id uuid.UUID) (Session, error)
	GetSessionIncludingArchived(ctx context.Context, id uuid.UUID) (Session, error)
	ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error)
	ListSessionsLifecycle(ctx context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error)
	CreateSession(ctx context.Context, s Session) (Session, error)
	UpdateSession(ctx context.Context, s Session) (Session, error)
	ArchiveSession(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	RestoreSession(ctx context.Context, id uuid.UUID) error
}
