package apppref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// The operations, as the metrics name them.
const (
	OpGet     = "get"
	OpPut     = "put"
	OpDelete  = "delete"
	OpReset   = "reset"
	OpSummary = "summary"
)

// Service decides who may reach which namespace and keeps the limits.
type Service struct {
	config  Config
	store   Store
	now     func() time.Time
	metrics *Metrics
	budget  *writeBudget
}

// NewService serves config over store. An empty client map leaves it off.
func NewService(config Config, store Store) *Service {
	return &Service{config: config, store: store, now: func() time.Time { return time.Now().UTC() }, metrics: NewMetrics(config), budget: newWriteBudget()}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Enabled says whether the routes are served.
func (s *Service) Enabled() bool { return s != nil && s.config.Enabled() }

// Metrics are the routes' counters, served on /v1/metrics.
func (s *Service) Metrics() *Metrics {
	if s == nil {
		return nil
	}
	return s.metrics
}

// authorize lets caller do op on app (ADR 0081 karar 2 and 3): a person's
// token only; the app must have a client; the token's client must be the
// app's, except that Account center reads the summary and resets any app.
func (s *Service) authorize(caller Caller, app, op string) error {
	if !s.Enabled() {
		return ErrOff
	}
	if caller.ServiceAccount {
		return ErrPersonOnly
	}
	own := s.config.AppOf(caller.Client)
	if op == OpSummary {
		if own != AccountApp {
			return ErrWrongClient
		}
		return nil
	}
	if !s.config.Known(app) {
		return ErrUnknownApp
	}
	if own == app || (op == OpReset && own == AccountApp) {
		return nil
	}
	return ErrWrongClient
}

// Get is every preference of the caller in app.
func (s *Service) Get(ctx context.Context, caller Caller, app string) (items []Item, err error) {
	defer func() { s.metrics.Record(app, OpGet, OutcomeOf(err)) }()
	if err := s.authorize(caller, app, OpGet); err != nil {
		return nil, err
	}
	return s.store.List(ctx, caller.Person, app, "")
}

// Put stores one value. value is the request's raw JSON; schemaVersion 0
// means 1.
func (s *Service) Put(ctx context.Context, caller Caller, app, key string, value json.RawMessage, schemaVersion int, match *Match) (item Item, err error) {
	defer func() {
		s.metrics.Record(app, OpPut, OutcomeOf(err))
		if err == nil {
			s.metrics.ObserveValue(app, len(item.Value))
		}
	}()
	if err := s.authorize(caller, app, OpPut); err != nil {
		return Item{}, err
	}
	if err := s.budget.take(caller.Person, s.now()); err != nil {
		return Item{}, err
	}
	w, err := validWrite(key, value, schemaVersion, match)
	if err != nil {
		return Item{}, err
	}
	return s.store.Put(ctx, caller.Person, app, key, w, caller.Client, s.now())
}

// DeleteKey deletes one key: the app's default is back.
func (s *Service) DeleteKey(ctx context.Context, caller Caller, app, key string) (err error) {
	defer func() { s.metrics.Record(app, OpDelete, OutcomeOf(err)) }()
	if err := s.authorize(caller, app, OpDelete); err != nil {
		return err
	}
	if err := s.budget.take(caller.Person, s.now()); err != nil {
		return err
	}
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	return s.store.DeleteKey(ctx, caller.Person, app, key)
}

// Reset deletes every key of the caller in app.
func (s *Service) Reset(ctx context.Context, caller Caller, app string) (err error) {
	defer func() { s.metrics.Record(app, OpReset, OutcomeOf(err)) }()
	if err := s.authorize(caller, app, OpReset); err != nil {
		return err
	}
	if err := s.budget.take(caller.Person, s.now()); err != nil {
		return err
	}
	_, err = s.store.DeleteApp(ctx, caller.Person, app)
	return err
}

// Summaries are the caller's configured apps that hold a preference, with
// the key count and last change; never a value.
func (s *Service) Summaries(ctx context.Context, caller Caller) (out []Summary, err error) {
	defer func() { s.metrics.Record("", OpSummary, OutcomeOf(err)) }()
	if err := s.authorize(caller, "", OpSummary); err != nil {
		return nil, err
	}
	all, err := s.store.Summaries(ctx, caller.Person)
	if err != nil {
		return nil, err
	}
	out = []Summary{}
	for _, sum := range all {
		// An app taken out of the map keeps its rows until a migration
		// deletes them, but nobody reaches them.
		if s.config.Known(sum.App) {
			out = append(out, sum)
		}
	}
	return out, nil
}

// EraseSubject deletes every preference of person, in every app, whether
// App preferences are on or off: the account erasure step
// erase_app_preferences.
func (s *Service) EraseSubject(ctx context.Context, person uuid.UUID) (int64, error) {
	return s.store.EraseSubject(ctx, person)
}

// Subjects are the people who hold a preference in any app, whether App
// preferences are on or off: the daily deleted people cleanup reads them
// (commpref.Service.AlsoRemoveDeletedPeopleFrom).
func (s *Service) Subjects(ctx context.Context) ([]uuid.UUID, error) {
	return s.store.Subjects(ctx)
}

// validWrite checks a write's key and value and makes the value compact.
func validWrite(key string, value json.RawMessage, schemaVersion int, match *Match) (Write, error) {
	if !ValidKey(key) {
		return Write{}, ErrInvalidKey
	}
	if schemaVersion == 0 {
		schemaVersion = 1
	}
	if schemaVersion < 1 || schemaVersion > MaxSchemaVersion {
		return Write{}, ErrInvalidValue
	}
	var compacted bytes.Buffer
	if len(bytes.TrimSpace(value)) == 0 || json.Compact(&compacted, value) != nil || compacted.String() == "null" {
		return Write{}, ErrInvalidValue
	}
	if compacted.Len() > MaxValueBytes {
		return Write{}, ErrValueTooLarge
	}
	return Write{Value: json.RawMessage(compacted.Bytes()), SchemaVersion: schemaVersion, Match: match}, nil
}

// OutcomeOf names err for the metrics.
func OutcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrOff), errors.Is(err, ErrUnknownApp):
		return "unknown_app"
	case errors.Is(err, ErrPersonOnly):
		return "person_only"
	case errors.Is(err, ErrWrongClient):
		return "wrong_client"
	case errors.Is(err, ErrInvalidKey), errors.Is(err, ErrInvalidValue):
		return "invalid"
	case errors.Is(err, ErrValueTooLarge):
		return "too_large"
	case errors.Is(err, ErrQuota):
		return "quota"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrSubjectInactive):
		return "inactive"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	}
	return "error"
}
