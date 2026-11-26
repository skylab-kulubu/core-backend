package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// formDetailsBackfillCommandName runs the one-time fill of a form mapping's
// details on an Event's earlier guests instead of the server
// (docs/form-mappings.md#filling-in-earlier-guests):
// `core-backend backfill-form-guest-details -event <id> [-apply]`. It reads
// the same environment as the server, so it runs inside the core container.
const formDetailsBackfillCommandName = "backfill-form-guest-details"

// formDetailsFiller is formsfeed.DetailsBackfill.
type formDetailsFiller interface {
	FillDetails(ctx context.Context, eventID uuid.UUID, apply bool) (formsfeed.DetailsReport, error)
}

// runFormDetailsBackfill wires the fill to the database and to Forms'
// internal reads with core's service account. It prints counts only, never
// a person, an answer or a configuration value.
func runFormDetailsBackfill(args []string, getenv func(string) string, out io.Writer) int {
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(out, "%s needs DATABASE_URL\n", formDetailsBackfillCommandName)
		return 2
	}
	base, realm := keycloakRealmFromEnv(getenv)
	client, _, err := newFormsClient(getenv, base, realm)
	if err != nil {
		fmt.Fprintf(out, "%s: %v\n", formDetailsBackfillCommandName, err)
		return 2
	}
	if client == nil {
		fmt.Fprintf(out, "%s needs Forms' internal address (%s or ACCOUNT_ERASURE_FORMS_URL), KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET\n",
			formDetailsBackfillCommandName, formsfeed.URLEnv)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(out, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	return formDetailsBackfillCommand(ctx, args, out, &formsfeed.DetailsBackfill{Pool: pool, Events: event.NewPostgresStore(pool), Forms: client})
}

func formDetailsBackfillCommand(ctx context.Context, args []string, out io.Writer, filler formDetailsFiller) int {
	flags := flag.NewFlagSet(formDetailsBackfillCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	eventFlag := flags.String("event", "", "the Event's id")
	apply := flags.Bool("apply", false, "write the details; without it the command only counts them")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	eventID, err := uuid.Parse(strings.TrimSpace(*eventFlag))
	if err != nil || eventID == uuid.Nil || flags.NArg() > 0 {
		fmt.Fprintf(out, "usage: %s -event <id> [-apply]\n", formDetailsBackfillCommandName)
		return 2
	}
	report, err := filler.FillDetails(ctx, eventID, *apply)
	switch {
	case errors.Is(err, formsfeed.ErrDetailsNoMapping):
		// The organizer saves the details' questions first (panel: Soru eşleme).
		fmt.Fprintln(out, "no_details_mapping: 1")
		return 3
	case errors.Is(err, formsfeed.ErrDetailsEventNotFound):
		fmt.Fprintln(out, "event: not found")
		return 1
	case err != nil:
		// Forms' and the database's errors name no person and no value.
		fmt.Fprintf(out, "failed: %v\n", err)
		return 1
	}
	if *apply {
		fmt.Fprintln(out, "form guest details backfill")
	} else {
		fmt.Fprintln(out, "form guest details backfill, dry run (add -apply to write)")
	}
	line := func(key string, n int) { fmt.Fprintf(out, "%s: %d\n", key, n) }
	line("forms", report.Forms)
	line("answers", report.Answers)
	line("skipped", report.Skipped)
	line("tickets", report.Tickets)
	line("tickets_fill", report.TicketsFilled)
	line("respondents", report.Respondents)
	line("respondents_fill", report.RespondentsFilled)
	for _, name := range []string{formsfeed.DetailPhone, formsfeed.DetailUniversity, formsfeed.DetailDepartment, formsfeed.DetailGrade} {
		line("fill_"+name, report.Filled[name])
		line("kept_"+name, report.Kept[name])
	}
	for _, name := range []string{formsfeed.DetailPhone, formsfeed.DetailUniversity, formsfeed.DetailDepartment} {
		line("dropped_"+name, report.Dropped[name])
	}
	for _, key := range ticket.Grades {
		line("grade_"+key, report.Grades[key])
	}
	return 0
}
