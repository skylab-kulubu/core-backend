package teamaudit

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Row is a written row as the memory writer keeps it.
type Row struct {
	Record
	ID         uuid.UUID
	OccurredAt time.Time
	Outcome    Outcome
	ErrorCode  string
}

// Memory is a Writer that keeps its rows in memory, for tests. It checks a
// row as the table does.
type Memory struct {
	mu   sync.Mutex
	rows []Row
	down bool
	now  func() time.Time
}

// NewMemory is an empty memory writer.
func NewMemory() *Memory { return &Memory{} }

// Rows are the rows written, oldest first.
func (m *Memory) Rows() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows)
}

// SetClock replaces the writer's clock (tests); nil is time.Now.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// SetUnavailable makes every Begin and Deny fail (true), as a database
// that is down does, or work again (false).
func (m *Memory) SetUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

func (m *Memory) Begin(_ context.Context, r Record) (uuid.UUID, error) {
	return m.insert(r, OutcomePending, "")
}

func (m *Memory) Deny(_ context.Context, r Record, errorCode string) error {
	if !ValidErrorCode(errorCode) {
		return unavailable("invalid_error_code")
	}
	_, err := m.insert(r, OutcomeDenied, errorCode)
	return err
}

func (m *Memory) insert(r Record, outcome Outcome, errorCode string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return uuid.Nil, unavailable("down")
	}
	if bad := r.checkOutcome(outcome); bad != "" {
		return uuid.Nil, unavailable(bad)
	}
	r, bad := r.normalize()
	if bad != "" {
		return uuid.Nil, unavailable(bad)
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	row := Row{Record: r, ID: uuid.New(), OccurredAt: now().UTC(), Outcome: outcome, ErrorCode: errorCode}
	m.rows = append(m.rows, row)
	return row.ID, nil
}

func (m *Memory) AppRows(_ context.Context, client string, limit int) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, unavailable("down")
	}
	out := []Row{}
	for i := len(m.rows) - 1; i >= 0 && len(out) < limit; i-- {
		if row := m.rows[i]; row.Action.IsApp() && row.After != nil && row.After.Client == client {
			out = append(out, row)
		}
	}
	return out, nil
}

func (m *Memory) LatestAppRows(_ context.Context, client string) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, unavailable("down")
	}
	seen := map[string]bool{}
	out := []Row{}
	for i := len(m.rows) - 1; i >= 0; i-- {
		row := m.rows[i]
		if !row.Action.IsApp() || row.Outcome != OutcomeOK || row.After == nil || row.After.Client != client || seen[row.AppKey()] {
			continue
		}
		seen[row.AppKey()] = true
		out = append(out, row)
	}
	return out, nil
}

// ErrNotPending is a Finish of a row that is not pending (or not there).
var ErrNotPending = errors.New("teamaudit: no pending row")

func (m *Memory) Finish(_ context.Context, id uuid.UUID, outcome Outcome, errorCode string, after *State) error {
	if err := checkFinish(outcome, errorCode); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		row := &m.rows[i]
		if row.ID != id || row.Outcome != OutcomePending {
			continue
		}
		row.Outcome, row.ErrorCode = outcome, errorCode
		if after != nil {
			row.After = after.normalize()
			if row.GroupID == "" {
				if _, err := uuid.Parse(after.ID); err == nil {
					row.GroupID = after.ID
				}
			}
		}
		return nil
	}
	return ErrNotPending
}

// checkFinish refuses an outcome a Finish cannot write: ok without a code,
// failed with one.
func checkFinish(outcome Outcome, errorCode string) error {
	switch {
	case outcome == OutcomeOK && errorCode == "":
		return nil
	case outcome == OutcomeFailed && ValidErrorCode(errorCode):
		return nil
	}
	return errors.New("teamaudit: invalid outcome")
}
