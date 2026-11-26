package formsfeed

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// The one-time fill of a form mapping's details on the guests an Event
// already has (docs/form-mappings.md#filling-in-earlier-guests): answers
// that came before the mapping named the details' questions. Core reads the
// form's answers from Forms again, the details' questions only, and puts
// each answer's details on the guest Ticket that answer wrote
// (tickets.form_response_id) and on the Unticketed respondent it kept,
// only where the row has none of its own. A run again finds nothing to do.

// ErrDetailsNoMapping is an Event none of whose listed forms has a mapping
// naming a detail's question: the organizer saves one first.
var ErrDetailsNoMapping = errors.New("formsfeed: no form of the Event has a mapping naming a detail")

// ErrDetailsEventNotFound is no such current Event.
var ErrDetailsEventNotFound = errors.New("formsfeed: no such current Event")

// maxDetailsPages bounds one form's read (PageLimit answers a page).
const maxDetailsPages = 500

// DetailsEvents reads the Event (event.PostgresStore).
type DetailsEvents interface {
	Get(ctx context.Context, id uuid.UUID) (event.Event, error)
}

// DetailsLister reads a page of a form's answers (Client.List).
type DetailsLister interface {
	List(ctx context.Context, formID uuid.UUID, q ListQuery) (Page, error)
}

// DetailsBackfill fills the details (FillDetails).
type DetailsBackfill struct {
	Pool   *pgxpool.Pool
	Events DetailsEvents
	Forms  DetailsLister
}

// DetailsReport is what a fill found, in counts only: never a person, an
// answer or a value.
type DetailsReport struct {
	// Forms are the Event's forms whose mapping names a detail; Answers the
	// answers Forms gave for them.
	Forms   int
	Answers int
	// Skipped are the items of Forms' pages that did not read (another
	// form's, an id or a version missing): no Ticket of theirs is filled.
	Skipped int
	// Tickets are the guest Tickets those answers wrote (ticket type GUEST,
	// a guest Ticket linked to its account since included; not cancelled,
	// the guest's identity still kept); TicketsFilled those taking at least
	// one detail (dry run: that would).
	Tickets       int
	TicketsFilled int
	// Expired are the guest Tickets past the details' retention (the
	// Event's end, or without a date the registration, + 2 years): left
	// alone. PhoneExpired are Tickets whose phone stays empty, the Event
	// ended over 90 days ago (the phone's retention).
	Expired      int
	PhoneExpired int
	// Respondents and RespondentsFilled are the same for the Unticketed
	// respondents' guest rows.
	Respondents       int
	RespondentsFilled int
	// Filled counts, per detail, the Tickets taking it; Kept those that
	// have another value of their own and keep it.
	Filled map[string]int
	Kept   map[string]int
	// Dropped counts, per detail, the answers whose value did not read (a
	// phone that is no number, an over-long university or department).
	Dropped map[string]int
	// Grades counts the answers per grade key (GradeOf), "other" among them.
	Grades map[string]int
}

type detailsRow struct {
	id         uuid.UUID
	responseID uuid.UUID
	fields     [4]string
	// expired is a Ticket past the guest details' retention (its Event's
	// end, or without a date its registration, + 2 years): never filled
	// again. noPhone is one past the phone's (the Event's end + 90 days).
	expired bool
	noPhone bool
}

// Retention the fill keeps to (internal/retention: guest_identity,
// linked_guest_details and guest_phone), so that it brings back nothing a
// sweep cleared: the details 2 years after the Event's end (an Event
// without a date: the Ticket's registration), the phone 90 days after it.
const (
	detailsRetentionDays = 2 * 365
	phoneRetentionDays   = 90
)

// detailsEventEndSQL is when the Event of Ticket t ended, as the retention
// sweep reads it.
const detailsEventEndSQL = `(SELECT COALESCE(te.end_date, (SELECT max(rd.end_date) FROM event_days rd WHERE rd.event_id = te.id), te.start_date)
	FROM events te WHERE te.id = t.event_id)`

// detailNames are the details in the order of detailsRow.fields.
var detailNames = [4]string{DetailPhone, DetailUniversity, DetailDepartment, DetailGrade}

func (d Details) fields() [4]string { return [4]string{d.Phone, d.University, d.Department, d.Grade} }

// FillDetails fills the Event's earlier guests' details; with apply false it
// only counts (a dry run). ErrDetailsEventNotFound, ErrDetailsNoMapping, or
// Forms' and the database's errors.
func (b *DetailsBackfill) FillDetails(ctx context.Context, eventID uuid.UUID, apply bool) (DetailsReport, error) {
	report := DetailsReport{Filled: map[string]int{}, Kept: map[string]int{}, Dropped: map[string]int{}, Grades: map[string]int{}}
	ev, err := b.Events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) || (err == nil && ev.ArchivedAt != nil) {
		return report, ErrDetailsEventNotFound
	}
	if err != nil {
		return report, err
	}
	rows, err := b.Pool.Query(ctx, `SELECT `+mappingColumns+` FROM event_form_mappings WHERE event_id = $1 ORDER BY form_id`, eventID)
	if err != nil {
		return report, err
	}
	var mappings []Mapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			rows.Close()
			return report, err
		}
		if ev.ListsForm(m.FormID) && m.HasDetails() {
			mappings = append(mappings, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	if len(mappings) == 0 {
		return report, ErrDetailsNoMapping
	}
	tickets, err := b.detailRows(ctx, `
		SELECT t.id, t.form_response_id, t.guest_phone_number, t.guest_university, t.guest_department, t.guest_grade,
			COALESCE(`+detailsEventEndSQL+`, t.created_at) < now() - make_interval(days => `+fmt.Sprint(detailsRetentionDays)+`),
			COALESCE(`+detailsEventEndSQL+` < now() - make_interval(days => `+fmt.Sprint(phoneRetentionDays)+`), false)
		FROM tickets t
		WHERE t.event_id = $1 AND t.ticket_type = 'GUEST' AND t.form_response_id IS NOT NULL
		  AND t.status IS DISTINCT FROM 'cancelled' AND t.guest_email <> ''`, eventID)
	if err != nil {
		return report, err
	}
	respondents, err := b.detailRows(ctx, `
		SELECT id, form_response_id, phone_number, university, department, grade, false, false
		FROM event_form_respondents
		WHERE event_id = $1 AND user_id IS NULL`, eventID)
	if err != nil {
		return report, err
	}
	report.Forms = len(mappings)
	for _, m := range mappings {
		if err := b.fillForm(ctx, m, tickets, respondents, apply, &report); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (b *DetailsBackfill) detailRows(ctx context.Context, query string, eventID uuid.UUID) (map[uuid.UUID][]detailsRow, error) {
	rows, err := b.Pool.Query(ctx, query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]detailsRow{}
	for rows.Next() {
		var r detailsRow
		if err := rows.Scan(&r.id, &r.responseID, &r.fields[0], &r.fields[1], &r.fields[2], &r.fields[3], &r.expired, &r.noPhone); err != nil {
			return nil, err
		}
		out[r.responseID] = append(out[r.responseID], r)
	}
	return out, rows.Err()
}

// fillForm reads one form's answers, a page at a time, and fills each
// page's rows in one batch.
func (b *DetailsBackfill) fillForm(ctx context.Context, m Mapping, tickets, respondents map[uuid.UUID][]detailsRow, apply bool, report *DetailsReport) error {
	q := ListQuery{AllStatuses: true, Fields: m.DetailFields()}
	for pages := 1; ; pages++ {
		page, err := b.Forms.List(ctx, m.FormID, q)
		if err != nil {
			return err
		}
		report.Skipped += page.Skipped
		batch := &pgx.Batch{}
		for _, item := range page.Items {
			report.Answers++
			d := MappedDetails(m, item.Answers)
			for _, name := range d.Dropped {
				report.Dropped[name]++
			}
			if d.Grade != "" {
				report.Grades[d.Grade]++
			}
			for _, row := range tickets[item.ResponseID] {
				if row.expired {
					report.Expired++
					continue
				}
				report.Tickets++
				if row.noPhone && d.Phone != "" && row.fields[0] == "" {
					report.PhoneExpired++
				}
				if fill := plan(row, d, report.Filled, report.Kept); fill != nil {
					report.TicketsFilled++
					batch.Queue(`
						UPDATE tickets SET
							guest_phone_number = CASE WHEN guest_phone_number = '' THEN $2 ELSE guest_phone_number END,
							guest_university = CASE WHEN guest_university = '' THEN $3 ELSE guest_university END,
							guest_department = CASE WHEN guest_department = '' THEN $4 ELSE guest_department END,
							guest_grade = CASE WHEN guest_grade = '' THEN $5 ELSE guest_grade END,
							updated_at = now()
						WHERE id = $1 AND ticket_type = 'GUEST' AND guest_email <> ''
						  AND ((guest_phone_number = '' AND $2 <> '') OR (guest_university = '' AND $3 <> '')
						    OR (guest_department = '' AND $4 <> '') OR (guest_grade = '' AND $5 <> ''))`,
						row.id, fill[0], fill[1], fill[2], fill[3])
				}
			}
			for _, row := range respondents[item.ResponseID] {
				report.Respondents++
				if fill := plan(row, d, nil, nil); fill != nil {
					report.RespondentsFilled++
					batch.Queue(`
						UPDATE event_form_respondents SET
							phone_number = CASE WHEN phone_number = '' THEN $2 ELSE phone_number END,
							university = CASE WHEN university = '' THEN $3 ELSE university END,
							department = CASE WHEN department = '' THEN $4 ELSE department END,
							grade = CASE WHEN grade = '' THEN $5 ELSE grade END
						WHERE id = $1 AND user_id IS NULL`,
						row.id, fill[0], fill[1], fill[2], fill[3])
				}
			}
		}
		if apply && batch.Len() > 0 {
			if err := b.Pool.SendBatch(ctx, batch).Close(); err != nil {
				return fmt.Errorf("filling the details: %w", err)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if page.NextCursor == q.After {
			return invalid("Forms' cursor did not move")
		}
		if pages >= maxDetailsPages {
			return invalid("Forms' list goes on past %d pages", maxDetailsPages)
		}
		q.After = page.NextCursor
	}
}

// plan is what a row takes of d: each detail it has none of and d has; nil
// for nothing. filled and kept (nil: not counted) count per detail what it
// takes, and what it keeps of its own against another value of d.
func plan(row detailsRow, d Details, filled, kept map[string]int) *[4]string {
	var fill [4]string
	some := false
	values := d.fields()
	if row.noPhone {
		values[0] = ""
	}
	for i, v := range values {
		switch {
		case v == "":
		case row.fields[i] == "":
			fill[i], some = v, true
			if filled != nil {
				filled[detailNames[i]]++
			}
		case row.fields[i] != v && kept != nil:
			kept[detailNames[i]]++
		}
	}
	if !some {
		return nil
	}
	return &fill
}
