package shorturl

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Store interface {
	Create(ctx context.Context, u URL) (URL, error)
	Get(ctx context.Context, id uuid.UUID) (URL, error)
	GetIncludingDisabled(ctx context.Context, id uuid.UUID) (URL, error)
	GetByAlias(ctx context.Context, alias string) (URL, error)
	// GetByRetiredAlias finds the active link an alias was renamed away from.
	GetByRetiredAlias(ctx context.Context, alias string) (URL, error)
	GetByForm(ctx context.Context, formID uuid.UUID) (URL, error)
	ListByCreator(ctx context.Context, userID uuid.UUID) ([]URL, error)
	ListByCreatorLifecycle(ctx context.Context, userID uuid.UUID, visibility lifecycle.Visibility) ([]URL, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]URL, error)
	ListAll(ctx context.Context) ([]URL, error)
	ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]URL, error)
	// Update saves u. A changed alias retires the previous one in the same
	// step: it keeps redirecting to u and can never be handed to another link.
	Update(ctx context.Context, u URL) (URL, error)
	// BindForm makes id the form's link, releasing any other link the form
	// had; eventID is nil for a link the form manages itself.
	BindForm(ctx context.Context, id, formID uuid.UUID, eventID *uuid.UUID, label string) (URL, error)
	// ReleaseForm drops the form and event binding of id; the link keeps
	// redirecting as an ordinary one.
	ReleaseForm(ctx context.Context, id uuid.UUID) (URL, error)
	// AliasTaken reports whether any link other than except uses or retired
	// alias, ignoring case and counting disabled links.
	AliasTaken(ctx context.Context, alias string, except uuid.UUID) (bool, error)
	Disable(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error
	Restore(ctx context.Context, id uuid.UUID) error
	RecordHit(ctx context.Context, id uuid.UUID, hit Hit) (URL, error)
	ListHits(ctx context.Context, id uuid.UUID, since time.Time) ([]Hit, error)
	// FormSources counts the hits since the given time on every link that
	// points at formID, bound or not, grouped by utm_source.
	FormSources(ctx context.Context, formID uuid.UUID, since time.Time) ([]SourceCount, error)
}
