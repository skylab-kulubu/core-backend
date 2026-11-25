package formsfeed

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Forms statuses an Unticketed respondent may have (forms_status): a
// declined answer's row is deleted.
const (
	RespondentAccepted = StatusAccepted
	RespondentPending  = StatusPending
)

// Sources of an Unticketed respondent (source).
const (
	// SourceCollect is an answer given before the form was linked, found by
	// the gathering (PullCollect).
	SourceCollect = "collect"
	// SourceNew is an answer that came after, with "Otomatik bilet gönder"
	// off, or pending in Forms.
	SourceNew = "new"
)

// RespondentRow is one Unticketed respondent (event_form_respondents): who
// answered an Event's form and holds no Ticket on it. A signed-in answer
// keeps UserID only; a guest's the name and e-mail. Never an answer's text.
type RespondentRow struct {
	ID          uuid.UUID
	EventID     uuid.UUID
	FormID      uuid.UUID
	ResponseID  uuid.UUID
	UserID      *uuid.UUID
	FirstName   string
	LastName    string
	Email       string
	SubmittedAt *time.Time
	FormsStatus string
	Source      string
	CreatedAt   time.Time
}

// RespondentQuery is a page of an Event's Unticketed respondents: the form
// and Forms status ("" for any), a search over the guest's name and e-mail
// (q; a signed-in person's name lives in the directory, not here), the
// cursor after which the page starts, and its size.
type RespondentQuery struct {
	FormID      *uuid.UUID
	FormsStatus string
	Q           string
	After       string
	Limit       int
}

// RespondentPage is a page and the count of every row the query selects,
// with the last gatherings' counts of answers kept as nobody.
type RespondentPage struct {
	Items      []RespondentRow
	NextCursor string
	Total      int
	Incomplete CollectCounts
}

// CollectCounts are a gathering's answers that name nobody usable (K3): an
// e-mail that is no plain address, no first and last name (a one-word full
// name too), or no person at all (no sign-in, no named guest, no mapping).
type CollectCounts struct {
	MissingName  int `json:"missingName"`
	MissingEmail int `json:"missingEmail"`
	NoPerson     int `json:"noPerson"`
}

// RespondentStore keeps the Unticketed respondents (PostgresRespondents).
type RespondentStore interface {
	// Keep writes or updates the row of the answer on the Event (the same
	// answer twice is one row; its source stays the first). When the person
	// holds a Ticket on the Event (the answer's, the account's, or the
	// guest address's) it writes nothing, deletes a row the answer had, and
	// answers false.
	Keep(ctx context.Context, row RespondentRow) (bool, error)
	// DropAnswer deletes the answer's rows, on the Events given (nil:
	// every Event).
	DropAnswer(ctx context.Context, responseID uuid.UUID, eventIDs []uuid.UUID) (int, error)
	// List answers a page of the Event's rows, by submittedAt then id.
	List(ctx context.Context, eventID uuid.UUID, q RespondentQuery) (RespondentPage, error)
	// ByIDs answers the Event's rows among ids.
	ByIDs(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) ([]RespondentRow, error)
	// Next answers up to limit of the Event's rows (of formID when set)
	// after the row after, in id order: the background job's pages.
	Next(ctx context.Context, eventID uuid.UUID, formID *uuid.UUID, after uuid.UUID, limit int) ([]RespondentRow, error)
	// Delete deletes a row.
	Delete(ctx context.Context, id uuid.UUID) error
	// Ticketed reports whether the person of the row holds a Ticket on its
	// Event (as Keep reads it).
	Ticketed(ctx context.Context, row RespondentRow) (bool, error)
	// SaveCollectCounts keeps the last full gathering's counts of the
	// Event's form (event_form_collect_counts), replacing the earlier ones.
	SaveCollectCounts(ctx context.Context, eventID, formID uuid.UUID, c CollectCounts, now time.Time) error
	// Counts is the preview's input over the rows selected (ids, or every
	// row of the Event, of formID when set): accepted rows whose person
	// holds no Ticket, accepted rows whose person holds one, pending rows.
	Counts(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID, all bool, formID *uuid.UUID) (candidates, ticketed, pending int, err error)
}

// PostgresRespondents is RespondentStore on event_form_respondents.
type PostgresRespondents struct{ pool *pgxpool.Pool }

// NewPostgresRespondents builds the store on pool.
func NewPostgresRespondents(pool *pgxpool.Pool) *PostgresRespondents {
	return &PostgresRespondents{pool: pool}
}

const respondentColumns = `r.id, r.event_id, r.form_id, r.form_response_id, r.user_id, COALESCE(r.first_name, ''), COALESCE(r.last_name, ''),
	COALESCE(r.email, ''), r.submitted_at, r.forms_status, r.source, r.created_at`

func scanRespondent(row pgx.Row) (RespondentRow, error) {
	var r RespondentRow
	err := row.Scan(&r.ID, &r.EventID, &r.FormID, &r.ResponseID, &r.UserID, &r.FirstName, &r.LastName, &r.Email,
		&r.SubmittedAt, &r.FormsStatus, &r.Source, &r.CreatedAt)
	return r, err
}

// ticketedSQL: the person of the respondent r holds a Ticket on r's Event:
// the answer's, the account's, or (a guest) the address's. A Ticket Forms'
// own decline cancelled does not count: an acceptance reopens it.
const ticketedSQL = `EXISTS (SELECT 1 FROM tickets t WHERE t.event_id = r.event_id
	AND NOT (t.status = 'cancelled' AND t.cancelled_by = 'form')
	AND (t.form_response_id = r.form_response_id
		OR (r.user_id IS NOT NULL AND t.owner_id = r.user_id)
		OR (r.email IS NOT NULL AND t.owner_id IS NULL AND lower(t.guest_email) = r.email)))`

func (s *PostgresRespondents) Keep(ctx context.Context, row RespondentRow) (bool, error) {
	if row.ID == uuid.Nil {
		row.ID = uuid.New()
	}
	var userID *uuid.UUID
	var first, last, email *string
	if row.UserID != nil {
		userID = row.UserID
	} else {
		f, l, e := strings.TrimSpace(row.FirstName), strings.TrimSpace(row.LastName), strings.ToLower(strings.TrimSpace(row.Email))
		first, last, email = &f, &l, &e
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var ticketed bool
	if err := tx.QueryRow(ctx, `
		SELECT `+ticketedSQL+` FROM (SELECT $1::uuid AS event_id, $2::uuid AS form_response_id, $3::uuid AS user_id, $4::text AS email) r`,
		row.EventID, row.ResponseID, userID, email).Scan(&ticketed); err != nil {
		return false, err
	}
	if ticketed {
		if _, err := tx.Exec(ctx, `DELETE FROM event_form_respondents WHERE event_id = $1 AND form_response_id = $2`, row.EventID, row.ResponseID); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO event_form_respondents AS r
			(id, event_id, form_id, form_response_id, user_id, first_name, last_name, email, submitted_at, forms_status, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (event_id, form_response_id) DO UPDATE SET
			user_id = EXCLUDED.user_id, first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name, email = EXCLUDED.email,
			submitted_at = COALESCE(EXCLUDED.submitted_at, r.submitted_at), forms_status = EXCLUDED.forms_status`,
		row.ID, row.EventID, row.FormID, row.ResponseID, userID, first, last, email, row.SubmittedAt, row.FormsStatus, row.Source); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *PostgresRespondents) DropAnswer(ctx context.Context, responseID uuid.UUID, eventIDs []uuid.UUID) (int, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if eventIDs == nil {
		tag, err = s.pool.Exec(ctx, `DELETE FROM event_form_respondents WHERE form_response_id = $1`, responseID)
	} else {
		tag, err = s.pool.Exec(ctx, `DELETE FROM event_form_respondents WHERE form_response_id = $1 AND event_id = ANY($2::uuid[])`, responseID, eventIDs)
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// respondentCursor is the list's opaque cursor: the last row's submittedAt
// (or none) and id.
func respondentCursor(r RespondentRow) string {
	at := ""
	if r.SubmittedAt != nil {
		at = r.SubmittedAt.UTC().Format(time.RFC3339Nano)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(at + "|" + r.ID.String()))
}

// ErrInvalidCursor is a list cursor core did not give.
var ErrInvalidCursor = errors.New("formsfeed: invalid respondent cursor")

func parseRespondentCursor(raw string) (*time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, uuid.Nil, ErrInvalidCursor
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return nil, uuid.Nil, ErrInvalidCursor
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, uuid.Nil, ErrInvalidCursor
	}
	if at == "" {
		return nil, parsed, nil
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, uuid.Nil, ErrInvalidCursor
	}
	return &t, parsed, nil
}

// respondentFilterSQL selects the Event's rows of the query ($1 Event, $2
// form or NULL, $3 status or ”, $4 search or ”).
const respondentFilterSQL = `r.event_id = $1 AND ($2::uuid IS NULL OR r.form_id = $2)
	AND ($3::text = '' OR r.forms_status = $3)
	AND ($4::text = '' OR position($4 IN lower(COALESCE(r.first_name, '') || ' ' || COALESCE(r.last_name, '') || ' ' || COALESCE(r.email, ''))) > 0)`

func (s *PostgresRespondents) List(ctx context.Context, eventID uuid.UUID, q RespondentQuery) (RespondentPage, error) {
	search := strings.ToLower(strings.TrimSpace(q.Q))
	var page RespondentPage
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM event_form_respondents r WHERE `+respondentFilterSQL,
		eventID, q.FormID, q.FormsStatus, search).Scan(&page.Total); err != nil {
		return RespondentPage{}, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(missing_name), 0), COALESCE(sum(missing_email), 0), COALESCE(sum(no_person), 0)
		FROM event_form_collect_counts WHERE event_id = $1 AND ($2::uuid IS NULL OR form_id = $2)`, eventID, q.FormID).
		Scan(&page.Incomplete.MissingName, &page.Incomplete.MissingEmail, &page.Incomplete.NoPerson); err != nil {
		return RespondentPage{}, err
	}
	// Keyset over (submitted_at NULLS LAST, id): a row without a time
	// comes after every row with one.
	var afterAt *time.Time
	afterID := uuid.Nil
	hasAfter := q.After != ""
	if hasAfter {
		var err error
		if afterAt, afterID, err = parseRespondentCursor(q.After); err != nil {
			return RespondentPage{}, err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+respondentColumns+` FROM event_form_respondents r WHERE `+respondentFilterSQL+`
		AND (NOT $5::boolean
			OR ($6::timestamptz IS NOT NULL AND (r.submitted_at IS NULL OR r.submitted_at > $6 OR (r.submitted_at = $6 AND r.id > $7)))
			OR ($6::timestamptz IS NULL AND r.submitted_at IS NULL AND r.id > $7))
		ORDER BY r.submitted_at ASC NULLS LAST, r.id
		LIMIT $8`, eventID, q.FormID, q.FormsStatus, search, hasAfter, afterAt, afterID, q.Limit+1)
	if err != nil {
		return RespondentPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRespondent(rows)
		if err != nil {
			return RespondentPage{}, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return RespondentPage{}, err
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
		page.NextCursor = respondentCursor(page.Items[len(page.Items)-1])
	}
	return page, nil
}

func (s *PostgresRespondents) collect(ctx context.Context, sql string, args ...any) ([]RespondentRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RespondentRow
	for rows.Next() {
		r, err := scanRespondent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresRespondents) ByIDs(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) ([]RespondentRow, error) {
	return s.collect(ctx, `SELECT `+respondentColumns+` FROM event_form_respondents r WHERE r.event_id = $1 AND r.id = ANY($2::uuid[]) ORDER BY r.id`, eventID, ids)
}

func (s *PostgresRespondents) Next(ctx context.Context, eventID uuid.UUID, formID *uuid.UUID, after uuid.UUID, limit int) ([]RespondentRow, error) {
	return s.collect(ctx, `SELECT `+respondentColumns+` FROM event_form_respondents r
		WHERE r.event_id = $1 AND ($2::uuid IS NULL OR r.form_id = $2) AND r.id > $3 ORDER BY r.id LIMIT $4`, eventID, formID, after, limit)
}

func (s *PostgresRespondents) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM event_form_respondents WHERE id = $1`, id)
	return err
}

func (s *PostgresRespondents) Ticketed(ctx context.Context, row RespondentRow) (bool, error) {
	var email *string
	if row.UserID == nil && row.Email != "" {
		e := strings.ToLower(row.Email)
		email = &e
	}
	var ticketed bool
	err := s.pool.QueryRow(ctx, `
		SELECT `+ticketedSQL+` FROM (SELECT $1::uuid AS event_id, $2::uuid AS form_response_id, $3::uuid AS user_id, $4::text AS email) r`,
		row.EventID, row.ResponseID, row.UserID, email).Scan(&ticketed)
	return ticketed, err
}

func (s *PostgresRespondents) Counts(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID, all bool, formID *uuid.UUID) (int, int, int, error) {
	if ids == nil {
		ids = []uuid.UUID{}
	}
	var candidates, ticketed, pending int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE r.forms_status = 'accepted' AND NOT `+ticketedSQL+`),
			count(*) FILTER (WHERE r.forms_status = 'accepted' AND `+ticketedSQL+`),
			count(*) FILTER (WHERE r.forms_status = 'pending')
		FROM event_form_respondents r
		WHERE r.event_id = $1 AND ($2::boolean OR r.id = ANY($3::uuid[])) AND ($4::uuid IS NULL OR r.form_id = $4)`,
		eventID, all, ids, formID).Scan(&candidates, &ticketed, &pending)
	return candidates, ticketed, pending, err
}

func (s *PostgresRespondents) SaveCollectCounts(ctx context.Context, eventID, formID uuid.UUID, c CollectCounts, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO event_form_collect_counts (event_id, form_id, missing_name, missing_email, no_person, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id, form_id) DO UPDATE SET missing_name = EXCLUDED.missing_name,
			missing_email = EXCLUDED.missing_email, no_person = EXCLUDED.no_person, collected_at = EXCLUDED.collected_at`,
		eventID, formID, c.MissingName, c.MissingEmail, c.NoPerson, now.UTC())
	return err
}
