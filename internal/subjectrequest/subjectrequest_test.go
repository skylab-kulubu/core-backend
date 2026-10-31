package subjectrequest_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/optional"
	"github.com/skylab-kulubu/core-backend/internal/subjectrequest"
)

// now is 2026-10-27 01:30 in Istanbul, still the 26th in UTC: "today" is
// Turkey's.
var now = time.Date(2026, 10, 26, 22, 30, 0, 0, time.UTC)

var (
	yk     = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	admin  = authz.Principal{ID: uuid.NewString(), Groups: []string{"/ADMIN"}}
	dk     = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/DK"}}
	member = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
)

func newService(t *testing.T) (subjectrequest.Service, *subjectrequest.MemoryStore) {
	t.Helper()
	store := subjectrequest.NewMemoryStore()
	return subjectrequest.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), func() time.Time { return now }), store
}

func valid() subjectrequest.Input {
	return subjectrequest.Input{
		ReceivedOn:       "2026-10-20",
		Channel:          subjectrequest.ChannelEmail,
		RequesterName:    "  Ada Lovelace ",
		RequesterContact: "ada@example.test",
		RequestTypes:     []subjectrequest.Type{subjectrequest.TypeErase, subjectrequest.TypeLearn, subjectrequest.TypeErase},
	}
}

func code(err error) string {
	var invalid *subjectrequest.InvalidError
	if errors.As(err, &invalid) {
		return invalid.Code
	}
	return ""
}

// YK records a request: it is received, due 30 days after it came, answered
// by "YK Başkanı" unless said otherwise, its types in the law's order once
// each, and its trail names who created it and which fields, no value.
func TestCreateRecordsTheRequestAndItsTrail(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	got, err := svc.Create(ctx, yk, valid())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != subjectrequest.StatusReceived || got.DueOn != "2026-11-19" || got.RequesterName != "Ada Lovelace" ||
		got.Responder != subjectrequest.DefaultResponder || got.AnsweredOn != nil || got.CreatedBy == nil || got.CreatedBy.String() != yk.ID ||
		!slices.Equal(got.RequestTypes, []subjectrequest.Type{subjectrequest.TypeLearn, subjectrequest.TypeErase}) {
		t.Fatalf("created %+v", got)
	}
	read, err := svc.Get(ctx, yk, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Events) != 1 || read.Events[0].Action != subjectrequest.ActionCreate || read.Events[0].ActorID == nil ||
		read.Events[0].ActorID.String() != yk.ID || read.Events[0].Status != subjectrequest.StatusReceived ||
		!slices.Contains(read.Events[0].Fields, "requesterName") {
		t.Fatalf("trail %+v", read.Events)
	}
	for _, field := range read.Events[0].Fields {
		if field == "Ada Lovelace" || field == "ada@example.test" {
			t.Fatal("a value reached the trail")
		}
	}
}

// Only ADMIN and YK use the register; DK, a member and a service account
// get ErrForbidden, before anything is read or written.
func TestOnlyTheBoardUsesTheRegister(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, admin, valid())
	if err != nil {
		t.Fatal(err)
	}
	service := authz.Principal{ID: uuid.NewString(), ServiceAccount: true, Roles: []string{authz.RoleSubjectRequestManage}}
	for name, p := range map[string]authz.Principal{"DK": dk, "member": member, "service account": service} {
		if _, err := svc.Create(ctx, p, valid()); !errors.Is(err, subjectrequest.ErrForbidden) {
			t.Errorf("%s create: %v", name, err)
		}
		if _, err := svc.List(ctx, p); !errors.Is(err, subjectrequest.ErrForbidden) {
			t.Errorf("%s list: %v", name, err)
		}
		if _, err := svc.Get(ctx, p, created.ID); !errors.Is(err, subjectrequest.ErrForbidden) {
			t.Errorf("%s get: %v", name, err)
		}
		if _, err := svc.Get(ctx, p, uuid.New()); !errors.Is(err, subjectrequest.ErrForbidden) {
			t.Errorf("%s get of none: %v", name, err)
		}
		if _, err := svc.Update(ctx, p, created.ID, subjectrequest.Patch{}); !errors.Is(err, subjectrequest.ErrForbidden) {
			t.Errorf("%s update: %v", name, err)
		}
	}
	if _, err := svc.Get(ctx, yk, uuid.New()); !errors.Is(err, subjectrequest.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestCreateRefusesWhatThePolicyKeepsOut(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	answered := "2026-10-25"
	early := "2026-10-19"
	tomorrow := "2026-10-28"
	long := func(n int) string { return strings.Repeat("ş", n) }
	cases := map[string]struct {
		edit func(*subjectrequest.Input)
		code string
	}{
		"no date":           {func(in *subjectrequest.Input) { in.ReceivedOn = "" }, "received_on_invalid"},
		"bad date":          {func(in *subjectrequest.Input) { in.ReceivedOn = "20.10.2026" }, "received_on_invalid"},
		"future":            {func(in *subjectrequest.Input) { in.ReceivedOn = "2026-10-28" }, "received_on_invalid"},
		"channel":           {func(in *subjectrequest.Input) { in.Channel = "fax" }, "channel_invalid"},
		"no name":           {func(in *subjectrequest.Input) { in.RequesterName = "  " }, "requester_name_invalid"},
		"long name":         {func(in *subjectrequest.Input) { in.RequesterName = long(201) }, "requester_name_invalid"},
		"no contact":        {func(in *subjectrequest.Input) { in.RequesterContact = "" }, "requester_contact_invalid"},
		"no type":           {func(in *subjectrequest.Input) { in.RequestTypes = nil }, "request_types_invalid"},
		"unknown type":      {func(in *subjectrequest.Input) { in.RequestTypes = []subjectrequest.Type{"forget"} }, "request_types_invalid"},
		"status":            {func(in *subjectrequest.Input) { in.Status = "closed" }, "status_invalid"},
		"answered, no date": {func(in *subjectrequest.Input) { in.Status = subjectrequest.StatusAnswered }, "answered_on_required"},
		"open with a date":  {func(in *subjectrequest.Input) { in.AnsweredOn = &answered }, "answered_on_invalid"},
		"answered before":   {func(in *subjectrequest.Input) { in.Status, in.AnsweredOn = subjectrequest.StatusRejected, &early }, "answered_on_invalid"},
		"answered tomorrow": {func(in *subjectrequest.Input) { in.Status, in.AnsweredOn = subjectrequest.StatusAnswered, &tomorrow }, "answered_on_invalid"},
		"long summary":      {func(in *subjectrequest.Input) { in.AnswerSummary = long(2001) }, "answer_summary_invalid"},
		"long responder":    {func(in *subjectrequest.Input) { in.Responder = long(101) }, "responder_invalid"},
		"year before":       {func(in *subjectrequest.Input) { in.ReceivedOn = "2025-10-26" }, "received_on_invalid"},
		"bad account":       {func(in *subjectrequest.Input) { bad := "ada"; in.UserID = &bad }, "user_invalid"},
		"nil account":       {func(in *subjectrequest.Input) { nilID := uuid.Nil.String(); in.UserID = &nilID }, "user_invalid"},
	}
	for name, tc := range cases {
		in := valid()
		tc.edit(&in)
		if _, err := svc.Create(ctx, yk, in); !errors.Is(err, subjectrequest.ErrInvalid) || code(err) != tc.code {
			t.Errorf("%s: %v (%q)", name, err, code(err))
		}
	}
	// A year back is the earliest day taken.
	in := valid()
	in.ReceivedOn = "2025-10-27"
	if _, err := svc.Create(ctx, yk, in); err != nil {
		t.Fatalf("a year back: %v", err)
	}
	// Today in Istanbul is the 27th, whatever UTC says.
	in = valid()
	in.ReceivedOn = "2026-10-27"
	if _, err := svc.Create(ctx, yk, in); err != nil {
		t.Fatalf("today: %v", err)
	}
}

// Update writes what the patch sends: closing needs the answer's date,
// reopening clears it, and each change adds a trail row naming the fields
// that changed and the status after it. A patch that changes nothing adds
// no row.
func TestUpdateClosesAndReopensWithTheTrail(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, yk, valid())
	if err != nil {
		t.Fatal(err)
	}
	status := func(s subjectrequest.Status) optional.Field[subjectrequest.Status] {
		return optional.Field[subjectrequest.Status]{Set: true, Value: s}
	}
	date := func(d string) optional.Field[*string] { return optional.Field[*string]{Set: true, Value: &d} }

	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{Status: status(subjectrequest.StatusAnswered)}); code(err) != "answered_on_required" {
		t.Fatalf("closing without a date: %v", err)
	}
	waiting, err := svc.Update(ctx, admin, created.ID, subjectrequest.Patch{Status: status(subjectrequest.StatusAwaitingVerification)})
	if err != nil || waiting.Status != subjectrequest.StatusAwaitingVerification || waiting.UpdatedBy.String() != admin.ID {
		t.Fatalf("waiting %+v %v", waiting, err)
	}
	closed, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{
		Status: status(subjectrequest.StatusAnswered), AnsweredOn: date("2026-10-26"),
		AnswerSummary: optional.Field[string]{Set: true, Value: "Hesap silindi, bilgi verildi."},
	})
	if err != nil || closed.AnsweredOn == nil || *closed.AnsweredOn != "2026-10-26" || closed.AnswerSummary != "Hesap silindi, bilgi verildi." ||
		closed.RequesterName != "Ada Lovelace" {
		t.Fatalf("closed %+v %v", closed, err)
	}
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{AnswerSummary: optional.Field[string]{Set: true, Value: "Hesap silindi, bilgi verildi."}}); err != nil {
		t.Fatal(err)
	}
	reopened, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{Status: status(subjectrequest.StatusReceived)})
	if err != nil || reopened.AnsweredOn != nil {
		t.Fatalf("reopened %+v %v", reopened, err)
	}
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{RequesterName: optional.Field[string]{Set: true, Null: true}}); code(err) != "requester_name_invalid" {
		t.Fatalf("null name: %v", err)
	}
	read, err := svc.Get(ctx, yk, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	for _, e := range read.Events {
		trail = append(trail, string(e.Action)+":"+string(e.Status)+":"+join(e.Fields))
	}
	want := []string{
		"create:received:receivedOn,channel,requesterName,requesterContact,requestTypes,status,responder",
		"update:awaiting_verification:status",
		"update:answered:status,answeredOn,answerSummary",
		"update:received:status,answeredOn",
	}
	if !slices.Equal(trail, want) {
		t.Fatalf("trail\n%v\nwant\n%v", trail, want)
	}
	if _, err := svc.Update(ctx, yk, uuid.New(), subjectrequest.Patch{}); !errors.Is(err, subjectrequest.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

// An emptied responder is the default again, on change as on create.
func TestUpdateEmptyResponderIsTheDefault(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	in := valid()
	in.Responder = "YK Başkan Yardımcısı"
	created, err := svc.Create(ctx, yk, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []optional.Field[string]{{Set: true, Value: "  "}, {Set: true, Null: true}} {
		got, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{Responder: f})
		if err != nil || got.Responder != subjectrequest.DefaultResponder {
			t.Fatalf("%+v: %q %v", f, got.Responder, err)
		}
		if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{Responder: optional.Field[string]{Set: true, Value: "X"}}); err != nil {
			t.Fatal(err)
		}
	}
}

// A change that names the version it read is refused once someone else
// changed the request since; without one it is written as before.
func TestUpdateRefusesAStaleVersion(t *testing.T) {
	t.Parallel()
	store := subjectrequest.NewMemoryStore()
	clock := now
	svc := subjectrequest.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), func() time.Time { return clock })
	ctx := context.Background()
	created, err := svc.Create(ctx, yk, valid())
	if err != nil {
		t.Fatal(err)
	}
	read := created.UpdatedAt
	clock = now.Add(time.Minute)
	name := func(n string) optional.Field[string] { return optional.Field[string]{Set: true, Value: n} }
	first, err := svc.Update(ctx, admin, created.ID, subjectrequest.Patch{RequesterName: name("Ada K."), UpdatedAt: &read})
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{RequesterName: name("Ada L."), UpdatedAt: &read}); !errors.Is(err, subjectrequest.ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{RequesterName: name("Ada L."), UpdatedAt: &first.UpdatedAt}); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{RequesterName: name("Ada M.")}); err != nil {
		t.Fatalf("no version: %v", err)
	}
}

// The list puts open requests first, soonest due first, then the closed,
// latest answered first.
func TestListOpenFirstByDueDate(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	add := func(received string, answered string) uuid.UUID {
		t.Helper()
		in := valid()
		in.ReceivedOn = received
		if answered != "" {
			in.Status, in.AnsweredOn = subjectrequest.StatusAnswered, &answered
		}
		r, err := svc.Create(ctx, yk, in)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	closedOld := add("2026-09-01", "2026-09-10")
	openLate := add("2026-10-20", "")
	closedNew := add("2026-09-05", "2026-10-01")
	openEarly := add("2026-10-01", "")
	list, err := svc.List(ctx, yk)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, r := range list {
		ids = append(ids, r.ID)
		if r.Events != nil {
			t.Fatal("the list carries trails")
		}
	}
	if !slices.Equal(ids, []uuid.UUID{openEarly, openLate, closedNew, closedOld}) {
		t.Fatalf("order %v", ids)
	}
}

func join(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += ","
		}
		out += f
	}
	return out
}

// The year's bound is on the day as entered: a request older than a year
// can still be closed, but not moved to another day that old.
func TestAnOldRequestStaysOpenToChange(t *testing.T) {
	t.Parallel()
	clock := now
	svc := subjectrequest.NewService(subjectrequest.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()),
		func() time.Time { return clock })
	ctx := context.Background()
	created, err := svc.Create(ctx, yk, valid())
	if err != nil {
		t.Fatal(err)
	}
	clock = now.AddDate(2, 0, 0)
	answered := "2026-11-01"
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{
		Status:     optional.Field[subjectrequest.Status]{Set: true, Value: subjectrequest.StatusAnswered},
		AnsweredOn: optional.Field[*string]{Set: true, Value: &answered},
	}); err != nil {
		t.Fatalf("closing an old request: %v", err)
	}
	if _, err := svc.Update(ctx, yk, created.ID, subjectrequest.Patch{ReceivedOn: optional.Field[string]{Set: true, Value: "2026-10-19"}}); code(err) != "received_on_invalid" {
		t.Fatalf("moving it to another old day: %v", err)
	}
}
