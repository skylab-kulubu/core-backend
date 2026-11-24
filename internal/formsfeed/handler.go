package formsfeed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Queue is the queue of Forms' events for core (ADR-0071 §2.4).
const Queue = "core.forms"

// Subscription is the core.forms subscription (CloudEvents, the default
// decoder). An event of another source, or of a data schema version other
// than SchemaVersion, goes to the DLQ: core cannot read it. A response
// created or whose status changed goes through Recorder.Response, an
// updated form through Recorder.FormState; any other type (Forms' later
// events, forms.workflow_run.* and forms.attempt.*, until core reads them)
// is unrelated, and so is an answer to a form core does not know.
func Subscription(r *Recorder) events.Subscription {
	return events.Subscription{
		Queue:  Queue,
		Handle: func(ctx context.Context, tx pgx.Tx, e events.Event) error { return r.handle(ctx, tx, e) },
	}
}

func (r *Recorder) handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	if e.Source != Source {
		return events.Permanent(fmt.Errorf("formsfeed: source is not %s", Source))
	}
	switch e.Type {
	case TypeResponseCreated, TypeResponseStatusChanged, TypeFormUpdated:
	default:
		return events.ErrUnrelated
	}
	if e.DataSchema != "" {
		if v, ok := e.SchemaVersion(); !ok || v != SchemaVersion {
			return events.Permanent(fmt.Errorf("formsfeed: %s data schema is not version %d", e.Type, SchemaVersion))
		}
	}
	origin := Origin{Via: ViaEvent, EventID: e.ID}
	if e.Type == TypeFormUpdated {
		st, err := DecodeFormState(e.Data)
		if err != nil {
			return events.Permanent(err)
		}
		return r.FormState(ctx, tx, origin, st)
	}
	in, err := DecodeResponse(e.Data)
	if err != nil {
		return events.Permanent(err)
	}
	outcome, err := r.Response(ctx, tx, origin, in, e.Type == TypeResponseCreated)
	if err != nil {
		return err
	}
	if outcome == string(ticket.FormResponseNotListed) {
		return events.ErrUnrelated
	}
	return nil
}
