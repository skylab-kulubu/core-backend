package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

type fakeDetailsFiller struct {
	report formsfeed.DetailsReport
	err    error
	event  uuid.UUID
	apply  bool
	calls  int
}

func (f *fakeDetailsFiller) FillDetails(_ context.Context, eventID uuid.UUID, apply bool) (formsfeed.DetailsReport, error) {
	f.calls++
	f.event, f.apply = eventID, apply
	return f.report, f.err
}

func TestFormDetailsBackfillCommand(t *testing.T) {
	ev := uuid.New()
	filler := &fakeDetailsFiller{report: formsfeed.DetailsReport{
		Forms: 1, Answers: 530, Tickets: 514, TicketsFilled: 500, Respondents: 10, RespondentsFilled: 9,
		Filled: map[string]int{formsfeed.DetailPhone: 480, formsfeed.DetailGrade: 500}, Kept: map[string]int{formsfeed.DetailPhone: 3},
		Dropped: map[string]int{formsfeed.DetailPhone: 7}, Grades: map[string]int{"3": 120, "other": 4},
	}}
	var out bytes.Buffer
	if code := formDetailsBackfillCommand(context.Background(), []string{"-event", ev.String()}, &out, filler); code != 0 || filler.apply || filler.event != ev {
		t.Fatalf("dry run: %d %+v\n%s", code, filler, out.String())
	}
	for _, want := range []string{"dry run", "\ntickets: 514\n", "\ntickets_fill: 500\n", "\nrespondents_fill: 9\n", "\nfill_phone: 480\n",
		"\nfill_university: 0\n", "\nkept_phone: 3\n", "\ndropped_phone: 7\n", "\ngrade_3: 120\n", "\ngrade_other: 4\n", "\ngrade_prep: 0\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := formDetailsBackfillCommand(context.Background(), []string{"-event", ev.String(), "-apply"}, &out, filler); code != 0 || !filler.apply ||
		strings.Contains(out.String(), "dry run") {
		t.Fatalf("apply: %d\n%s", code, out.String())
	}

	filler.err = formsfeed.ErrDetailsNoMapping
	out.Reset()
	if code := formDetailsBackfillCommand(context.Background(), []string{"-event", ev.String()}, &out, filler); code != 3 || out.String() != "no_details_mapping: 1\n" {
		t.Fatalf("no mapping: %d %q", code, out.String())
	}
	filler.err = errors.New("formsfeed: Forms answered 503")
	out.Reset()
	if code := formDetailsBackfillCommand(context.Background(), []string{"-event", ev.String()}, &out, filler); code != 1 || !strings.HasPrefix(out.String(), "failed: ") {
		t.Fatalf("failed: %d %q", code, out.String())
	}
	calls := filler.calls
	for _, args := range [][]string{nil, {"-event", "x"}, {"-event", uuid.Nil.String()}, {"-event", ev.String(), "extra"}, {"-nope"}} {
		out.Reset()
		if code := formDetailsBackfillCommand(context.Background(), args, &out, filler); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	if filler.calls != calls {
		t.Fatal("a usage error ran the fill")
	}
}

func TestFormDetailsBackfillNeedsItsEnvironment(t *testing.T) {
	var out bytes.Buffer
	if code := runFormDetailsBackfill([]string{"-event", uuid.NewString()}, func(string) string { return "" }, &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("%d %q", code, out.String())
	}
	env := map[string]string{"DATABASE_URL": "postgres://x"}
	out.Reset()
	if code := runFormDetailsBackfill([]string{"-event", uuid.NewString()}, func(k string) string { return env[k] }, &out); code != 2 || !strings.Contains(out.String(), "FORMS_INTERNAL_URL") {
		t.Fatalf("%d %q", code, out.String())
	}
}
