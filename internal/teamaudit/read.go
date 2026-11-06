package teamaudit

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxPage is the most rows one read of the record answers
// (GET /v1/team-audit-records, ticket 10).
const MaxPage = 100

// Query is a read of the record: every set field narrows it. Rows come
// newest first (occurred_at, then id, both descending).
type Query struct {
	// GroupIDs are the target Groups: a row matches when its group_id is
	// one of them. Empty, with no GroupPath, filters no Group.
	GroupIDs []string
	// GroupPath, with a subtree read, is the Group's path: a row whose
	// path at the time is it or under it matches too, so the rows of a
	// Sub-team deleted since stay in its Team's history.
	GroupPath string
	ActorID   uuid.UUID
	// TargetUserID is the person of a member, leader, term or app row.
	TargetUserID uuid.UUID
	Actions      []Action
	Outcome      Outcome
	// From is inclusive, To exclusive; zero is open.
	From, To time.Time
	// After is the cursor: only rows older than it.
	After Cursor
	// Limit is the page size, 1 to MaxPage.
	Limit int
}

// Cursor is the place of the last row of a page: its time and id.
type Cursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
}

// IsZero reports whether c is no cursor: the first page.
func (c Cursor) IsZero() bool { return c.ID == uuid.Nil }

// CursorOf is the cursor that continues after r.
func CursorOf(r Row) Cursor { return Cursor{OccurredAt: r.OccurredAt, ID: r.ID} }

// String is c as the next page's opaque token.
func (c Cursor) String() string {
	if c.IsZero() {
		return ""
	}
	raw := strconv.FormatInt(c.OccurredAt.UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ErrInvalidCursor is a cursor token no page gave out.
var ErrInvalidCursor = errors.New("teamaudit: invalid cursor")

// ParseCursor reads a token String gave out.
func ParseCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	micros, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	at, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{OccurredAt: time.UnixMicro(at).UTC(), ID: parsed}, nil
}

// Reader reads the record (GET /v1/team-audit-records).
type Reader interface {
	// Records are the rows q matches, newest first, at most q.Limit, and
	// the cursor of the next page (zero on the last).
	Records(ctx context.Context, q Query) ([]Row, Cursor, error)
}

// before reports whether row a comes before row b in the read order:
// newer, or as new with the greater id.
func before(aAt time.Time, aID uuid.UUID, bAt time.Time, bID uuid.UUID) bool {
	if !aAt.Equal(bAt) {
		return aAt.After(bAt)
	}
	return strings.Compare(aID.String(), bID.String()) > 0
}

// matches reports whether r is one of q's rows, as the Postgres query
// decides it (the memory reader).
func (q Query) matches(r Row) bool {
	if len(q.GroupIDs) > 0 || q.GroupPath != "" {
		in := slices.Contains(q.GroupIDs, r.GroupID)
		if !in && q.GroupPath != "" {
			in = r.GroupPath == q.GroupPath || strings.HasPrefix(r.GroupPath, q.GroupPath+"/")
		}
		if !in {
			return false
		}
	}
	switch {
	case q.ActorID != uuid.Nil && r.Actor.ID != q.ActorID,
		q.TargetUserID != uuid.Nil && r.TargetUserID != q.TargetUserID,
		len(q.Actions) > 0 && !slices.Contains(q.Actions, r.Action),
		q.Outcome != "" && r.Outcome != q.Outcome,
		!q.From.IsZero() && r.OccurredAt.Before(q.From),
		!q.To.IsZero() && !r.OccurredAt.Before(q.To):
		return false
	}
	return q.After.IsZero() || before(q.After.OccurredAt, q.After.ID, r.OccurredAt, r.ID)
}

func (m *Memory) Records(_ context.Context, q Query) ([]Row, Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, Cursor{}, unavailable("down")
	}
	found := []Row{}
	for _, r := range m.rows {
		// Postgres keeps microseconds: so does the memory reader.
		r.OccurredAt = r.OccurredAt.Truncate(time.Microsecond)
		if q.matches(r) {
			found = append(found, r)
		}
	}
	slices.SortFunc(found, func(a, b Row) int {
		if before(a.OccurredAt, a.ID, b.OccurredAt, b.ID) {
			return -1
		}
		return 1
	})
	return page(found, q.Limit)
}

// page is the first limit rows and, when more follow, the cursor after them.
func page(rows []Row, limit int) ([]Row, Cursor, error) {
	if limit < 1 || limit > MaxPage {
		return nil, Cursor{}, errors.New("teamaudit: invalid page size")
	}
	if len(rows) <= limit {
		return rows, Cursor{}, nil
	}
	return rows[:limit], CursorOf(rows[limit-1]), nil
}

func (p *Postgres) Records(ctx context.Context, q Query) ([]Row, Cursor, error) {
	if q.Limit < 1 || q.Limit > MaxPage {
		return nil, Cursor{}, errors.New("teamaudit: invalid page size")
	}
	where, args := []string{}, []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(q.GroupIDs) > 0 || q.GroupPath != "" {
		ids := []uuid.UUID{}
		for _, raw := range q.GroupIDs {
			if id, err := uuid.Parse(raw); err == nil {
				ids = append(ids, id)
			}
		}
		group := "group_id = ANY(" + arg(ids) + "::uuid[])"
		if q.GroupPath != "" {
			path := arg(q.GroupPath)
			group += " OR group_path = " + path + " OR starts_with(group_path, " + path + " || '/')"
		}
		where = append(where, "("+group+")")
	}
	if q.ActorID != uuid.Nil {
		where = append(where, "actor_id = "+arg(q.ActorID))
	}
	if q.TargetUserID != uuid.Nil {
		where = append(where, "target_user_id = "+arg(q.TargetUserID))
	}
	if len(q.Actions) > 0 {
		actions := make([]string, len(q.Actions))
		for i, a := range q.Actions {
			actions[i] = string(a)
		}
		where = append(where, "action = ANY("+arg(actions)+"::text[])")
	}
	if q.Outcome != "" {
		where = append(where, "outcome = "+arg(string(q.Outcome)))
	}
	if !q.From.IsZero() {
		where = append(where, "occurred_at >= "+arg(q.From.UTC()))
	}
	if !q.To.IsZero() {
		where = append(where, "occurred_at < "+arg(q.To.UTC()))
	}
	if !q.After.IsZero() {
		where = append(where, "(occurred_at, id) < ("+arg(q.After.OccurredAt.UTC())+", "+arg(q.After.ID)+")")
	}
	query := "SELECT " + appColumns + " FROM team_audit_records"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY occurred_at DESC, id DESC LIMIT " + arg(q.Limit+1)
	rows, err := p.appRows(ctx, query, args...)
	if err != nil {
		return nil, Cursor{}, err
	}
	return page(rows, q.Limit)
}
