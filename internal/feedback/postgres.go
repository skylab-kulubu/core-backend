package feedback

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const inviteCols = `id, ticket_id, event_id, session_id, form_id, code, sent_at, reminded_at, answered_at, reminder_cancelled_at`

func scanInvite(row pgx.Row) (Invite, error) {
	var inv Invite
	err := row.Scan(&inv.ID, &inv.TicketID, &inv.EventID, &inv.SessionID, &inv.FormID, &inv.Code,
		&inv.SentAt, &inv.RemindedAt, &inv.AnsweredAt, &inv.ReminderCancelledAt)
	return inv, err
}

// Create writes an invite, with a new code when it has none: ErrExists when
// the Ticket has one for the target already.
func (s *PostgresStore) Create(ctx context.Context, inv Invite) (Invite, error) {
	if inv.TicketID == uuid.Nil || inv.EventID == uuid.Nil || inv.FormID == uuid.Nil {
		return Invite{}, ErrInvalid
	}
	if inv.ID == uuid.Nil {
		inv.ID = uuid.New()
	}
	if inv.Code == "" {
		code, err := NewCode()
		if err != nil {
			return Invite{}, err
		}
		inv.Code = code
	}
	if !ValidCode(inv.Code) {
		return Invite{}, ErrInvalid
	}
	created, err := scanInvite(s.pool.QueryRow(ctx, `
		INSERT INTO feedback_invites (id, ticket_id, event_id, session_id, form_id, code)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+inviteCols, inv.ID, inv.TicketID, inv.EventID, inv.SessionID, inv.FormID, inv.Code))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "feedback_invites_target_key" {
		return Invite{}, ErrExists
	}
	return created, err
}

// MarkAnswered marks who answered the form: the invite the code names, when
// it is the form's; else, for an answer given signed in, the form's invites
// of the person's Tickets. An invite answered already stays as it was. False
// when nothing matched: an unknown code, another form's, or nobody invited.
func (s *PostgresStore) MarkAnswered(ctx context.Context, formID uuid.UUID, code string, userID *uuid.UUID) (bool, error) {
	if ValidCode(code) {
		tag, err := s.pool.Exec(ctx, `
			UPDATE feedback_invites SET answered_at = COALESCE(answered_at, now())
			WHERE form_id = $1 AND code = $2`, formID, code)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() > 0 {
			return true, nil
		}
	}
	if userID == nil {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE feedback_invites fi SET answered_at = COALESCE(fi.answered_at, now())
		FROM tickets t
		WHERE t.id = fi.ticket_id AND fi.form_id = $1 AND t.owner_id = $2`, formID, *userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Counts reads an Event's counts.
func (s *PostgresStore) Counts(ctx context.Context, eventID uuid.UUID) (Counts, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, sent, answered FROM feedback_invite_counts
		WHERE event_id = $1 ORDER BY session_id NULLS FIRST`, eventID)
	if err != nil {
		return Counts{}, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Count, error) {
		var c Count
		err := row.Scan(&c.SessionID, &c.Sent, &c.Answered)
		return c, err
	})
	if err != nil {
		return Counts{}, err
	}
	out := Counts{Sessions: []Count{}}
	for _, c := range all {
		if c.SessionID == nil {
			out.Event = c
			continue
		}
		out.Sessions = append(out.Sessions, c)
	}
	return out, nil
}

// Get reads an invite by its id.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Invite, error) {
	return scanInvite(s.pool.QueryRow(ctx, `SELECT `+inviteCols+` FROM feedback_invites WHERE id = $1`, id))
}
