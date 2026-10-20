package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Copying an Event (ADR 0066, ticket 06; spec §3).

var (
	copyLeader = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	copyYK     = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	copyMember = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	copyDK     = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/DK"}}
	copyOther  = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
)

func stamp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// copySource is an Event with every field the copy decides on set, a
// current day with a cancelled Session, and an archived day and Session.
func copySource(t *testing.T, store *event.MemoryStore) event.Event {
	t.Helper()
	ctx := context.Background()
	ratio := 0.75
	cover, mailList, season, staff := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	source, err := store.Create(ctx, event.Event{
		Name: "Gecekodu 2026", Description: "Açıklama", Location: "YTÜ Davutpaşa", OwnerTeam: "WEBLAB",
		FormURL: "https://forms.yildizskylab.com/" + uuid.NewString(), FormAlias: "gecekodu2026",
		ExtraFormURLs: []event.EventFormLink{{Label: "CTF", URL: "https://forms.yildizskylab.com/" + uuid.NewString(), Alias: "ctf2026"}},
		Capacity:      120, StartDate: stamp("2026-03-14T19:00:00+03:00"), EndDate: stamp("2026-03-15T03:00:00+03:00"),
		Linkedin: "https://linkedin.com/x", Status: event.StatusPublished, Ranked: true, PrizeInfo: "Ödül",
		RegistrationClosed: true, RegistrationClosesAt: stamp("2026-03-13T23:59:00+03:00"),
		SeasonID: &season, CoverImageID: &cover, AttendanceRule: "ratio", AttendanceRatio: &ratio,
		Images: []event.GalleryImage{{ID: uuid.New()}}, DoorStaffIDs: []uuid.UUID{staff}, MailListID: &mailList,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(ctx, event.Day{EventID: source.ID, Name: "Cumartesi", StartDate: stamp("2026-03-14T19:00:00+03:00"), EndDate: stamp("2026-03-15T03:00:00+03:00")})
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range []event.Session{
		{Title: "Açılış", SpeakerName: "Ada", SpeakerLinkedin: "https://linkedin.com/ada", Description: "Merhaba", StartTime: stamp("2026-03-14T19:30:00+03:00"), EndTime: stamp("2026-03-14T20:00:00+03:00"), OrderIndex: 1, SessionType: "talk", Cancelled: true},
		{Title: "Silinen", SpeakerName: "Can", SessionType: "talk"},
	} {
		sess.EventDayID = day.ID
		created, err := store.CreateSession(ctx, sess)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Title == "Silinen" {
			if err := store.ArchiveSession(ctx, created.ID, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	gone, err := store.CreateDay(ctx, event.Day{EventID: source.ID, Name: "Silinen gün"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveDay(ctx, gone.ID, nil); err != nil {
		t.Fatal(err)
	}
	return source
}

func copyIn(name, start string) event.CopyInput {
	return event.CopyInput{ID: uuid.New(), Name: name, StartDate: stamp(start)}
}

// What is copied and what is not: spec §3.1, one row a case.
func TestCopyTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source := copySource(t, store)
	in := copyIn("  Gecekodu 2027  ", "2027-03-13T19:00:00+03:00")
	in.IncludeDoorStaff = true
	got, fresh, err := svc.Copy(ctx, copyYK, source.ID, in)
	if err != nil || !fresh {
		t.Fatalf("copy: %v fresh=%v", err, fresh)
	}
	c := got.Event
	days, err := store.ListDays(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sessions []event.Session
	for _, d := range days {
		listed, err := store.ListSessions(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, listed...)
	}
	for _, row := range []struct {
		name string
		ok   bool
	}{
		{"id is the client's", c.ID == in.ID},
		{"name is the new one, trimmed", c.Name == "Gecekodu 2027"},
		{"description", c.Description == source.Description},
		{"location", c.Location == source.Location},
		{"Owner team, the source's", c.OwnerTeam == "WEBLAB"},
		{"start is the new one", c.StartDate.Equal(*stamp("2027-03-13T19:00:00+03:00"))},
		{"end moves with it", c.EndDate.Equal(*stamp("2027-03-14T03:00:00+03:00"))},
		{"capacity", c.Capacity == 120},
		{"ranked", c.Ranked},
		{"prize", c.PrizeInfo == "Ödül"},
		{"LinkedIn is not copied", c.Linkedin == ""},
		{"Season is not copied blindly (none covers the start here)", c.SeasonID == nil},
		{"cover", c.CoverImageID != nil && *c.CoverImageID == *source.CoverImageID},
		{"gallery is not copied", len(c.Images) == 0},
		{"certificate rule", c.AttendanceRule == "ratio" && c.AttendanceRatio != nil && *c.AttendanceRatio == 0.75},
		{"one day, the archived one left out", len(days) == 1 && days[0].Name == "Cumartesi"},
		{"day moves", len(days) == 1 && days[0].StartDate.Equal(*stamp("2027-03-13T19:00:00+03:00")) && days[0].EndDate.Equal(*stamp("2027-03-14T03:00:00+03:00"))},
		{"one Session, the archived one left out", len(sessions) == 1 && sessions[0].Title == "Açılış"},
		{"Session keeps its content", len(sessions) == 1 && sessions[0].SpeakerName == "Ada" && sessions[0].SpeakerLinkedin != "" && sessions[0].Description == "Merhaba" && sessions[0].OrderIndex == 1 && sessions[0].SessionType == "talk"},
		{"Session moves", len(sessions) == 1 && sessions[0].StartTime.Equal(*stamp("2027-03-13T19:30:00+03:00")) && sessions[0].EndTime.Equal(*stamp("2027-03-13T20:00:00+03:00"))},
		{"Session cancellation is reset", len(sessions) == 1 && !sessions[0].Cancelled},
		{"door staff, asked by who may assign them", slices.Equal(c.DoorStaffIDs, source.DoorStaffIDs)},
		{"application form is not copied", c.FormURL == "" && c.FormAlias == ""},
		{"extra forms are not copied", len(c.ExtraFormURLs) == 0},
		{"mail list is not copied", c.MailListID == nil},
		{"always a draft", c.Status == event.StatusDraft && !c.Active && c.PublishedAt == nil},
		{"copied from the source", c.CopiedFromEventID != nil && *c.CopiedFromEventID == source.ID},
		{"applying is open again", !c.RegistrationClosed && c.RegistrationClosedAt == nil},
		{"the deadline moves", c.RegistrationClosesAt != nil && c.RegistrationClosesAt.Equal(*stamp("2027-03-12T23:59:00+03:00"))},
		{"nothing skipped", len(got.Skipped) == 0 && got.Skipped != nil},
	} {
		if !row.ok {
			t.Errorf("%s: %+v", row.name, c)
		}
	}
	// The source is untouched.
	after, err := store.Get(ctx, source.ID)
	if err != nil || after.Status != event.StatusPublished || after.FormURL != source.FormURL || len(after.DoorStaffIDs) != 1 {
		t.Fatalf("source after copy: %+v %v", after, err)
	}
}

// The anchor is the start, else the first day's start, else the first
// Session's start; with none nothing moves.
func TestCopyShiftAnchor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name                string
		dayStart, sessStart *time.Time
		wantDay, wantSess   *time.Time
	}{
		{"first day", stamp("2026-05-01T10:00:00+03:00"), stamp("2026-05-01T11:00:00+03:00"), stamp("2026-06-01T10:00:00+03:00"), stamp("2026-06-01T11:00:00+03:00")},
		{"first Session", nil, stamp("2026-05-01T11:00:00+03:00"), nil, stamp("2026-06-01T10:00:00+03:00")},
		{"none", nil, nil, nil, nil},
	} {
		store := event.NewMemoryStore()
		svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
		source, err := store.Create(ctx, event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "WEBLAB"})
		if err != nil {
			t.Fatal(err)
		}
		day, err := store.CreateDay(ctx, event.Day{EventID: source.ID, Name: "Gün", StartDate: tc.dayStart})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SpeakerName: "Ada", SessionType: "talk", StartTime: tc.sessStart}); err != nil {
			t.Fatal(err)
		}
		start := "2026-06-01T10:00:00+03:00"
		got, _, err := svc.Copy(ctx, copyLeader, source.ID, copyIn("Kopya", start))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Event.StartDate.Equal(*stamp(start)) {
			t.Errorf("%s: start %v", tc.name, got.Event.StartDate)
		}
		days, _ := store.ListDays(ctx, got.Event.ID)
		sessions, _ := store.ListSessions(ctx, days[0].ID)
		if !sameTime(days[0].StartDate, tc.wantDay) || !sameTime(sessions[0].StartTime, tc.wantSess) {
			t.Errorf("%s: day %v session %v", tc.name, days[0].StartDate, sessions[0].StartTime)
		}
	}
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// The move is on the Europe/Istanbul wall clock and keeps durations.
func TestShiftKeepsWallClockAndDurations(t *testing.T) {
	t.Parallel()
	shift := event.NewShift(stamp("2026-03-14T19:00:00+03:00"), *stamp("2027-03-13T16:00:00Z"))
	if got := shift.Time(*stamp("2026-03-15T02:30:00+03:00")); !got.Equal(*stamp("2027-03-14T02:30:00+03:00")) || got.Location().String() != "Europe/Istanbul" {
		t.Fatalf("moved %v", got)
	}
	if event.NewShift(nil, time.Now()).At(stamp("2026-01-01T00:00:00Z")).Equal(*stamp("2026-01-01T00:00:00Z")) == false {
		t.Fatal("no anchor moves")
	}
}

func TestCopyRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source := copySource(t, store)
	draft, err := store.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.Create(ctx, event.Event{Name: "Silinmiş", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	start := "2027-01-01T10:00:00+03:00"
	for _, tc := range []struct {
		name   string
		p      authz.Principal
		source uuid.UUID
		in     event.CopyInput
		want   error
	}{
		{"no id", copyLeader, source.ID, event.CopyInput{Name: "x", StartDate: stamp(start)}, event.ErrInvalid},
		{"no name", copyLeader, source.ID, copyIn("  ", start), event.ErrInvalid},
		{"no start", copyLeader, source.ID, event.CopyInput{ID: uuid.New(), Name: "x"}, event.ErrInvalid},
		{"team member sees, may not create", copyMember, source.ID, copyIn("x", start), event.ErrForbidden},
		{"DK reads a draft, may not copy it", copyDK, draft.ID, copyIn("x", start), event.ErrForbidden},
		{"another team's leader", copyOther, source.ID, copyIn("x", start), event.ErrForbidden},
		{"another team's draft does not exist", copyOther, draft.ID, copyIn("x", start), event.ErrNotFound},
		{"archived source", copyLeader, archived.ID, copyIn("x", start), event.ErrNotFound},
		{"unknown source", copyLeader, uuid.New(), copyIn("x", start), event.ErrNotFound},
		{"the id is another Event's", copyLeader, source.ID, event.CopyInput{ID: draft.ID, Name: "x", StartDate: stamp(start)}, event.ErrConflict},
	} {
		if _, _, err := svc.Copy(ctx, tc.p, tc.source, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A draft, a cancelled and an ended Event are copied too.
func TestCopyAnyStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	past := time.Now().Add(-72 * time.Hour)
	future := time.Now().Add(72 * time.Hour)
	for _, in := range []event.Event{
		{Name: "Taslak", Status: event.StatusDraft},
		{Name: "Bitti", StartDate: &past},
		{Name: "İptal", StartDate: &future},
	} {
		in.Location, in.OwnerTeam = "YTÜ", "WEBLAB"
		source, err := store.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if in.Name == "İptal" {
			if _, err := svc.Cancel(ctx, copyLeader, source.ID, "", false); err != nil {
				t.Fatal(err)
			}
		}
		got, fresh, err := svc.Copy(ctx, copyLeader, source.ID, copyIn(in.Name+" (kopya)", "2027-01-01T10:00:00+03:00"))
		if err != nil || !fresh || got.Event.Status != event.StatusDraft || got.Event.CancelledAt != nil {
			t.Errorf("%s: %+v %v", in.Name, got.Event, err)
		}
	}
}

// A retried request answers the copy it made; nothing is copied twice.
func TestCopyIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source := copySource(t, store)
	other, err := store.Create(ctx, event.Event{Name: "Başka", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	in := copyIn("Kopya", "2027-01-01T10:00:00+03:00")
	first, fresh, err := svc.Copy(ctx, copyLeader, source.ID, in)
	if err != nil || !fresh {
		t.Fatalf("first: %v %v", fresh, err)
	}
	again, fresh, err := svc.Copy(ctx, copyLeader, source.ID, in)
	if err != nil || fresh || again.Event.ID != first.Event.ID || again.Skipped == nil {
		t.Fatalf("again: %+v %v %v", again, fresh, err)
	}
	if _, _, err := svc.Copy(ctx, copyLeader, other.ID, in); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("same id, another source: %v", err)
	}
	all, err := store.ListIncludingDrafts(ctx, "WEBLAB")
	if err != nil || len(all) != 3 {
		t.Fatalf("events %d %v", len(all), err)
	}
}

// A part the copier may not write, or a rule refuses, is left out and
// named; the copy is still made.
func TestCopySkipsWhatItMayNotWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source := copySource(t, store)
	// The cover is another team's Event's cover now: the Team media library
	// refuses it.
	if _, err := store.Create(ctx, event.Event{Name: "ArtLab", Location: "YTÜ", OwnerTeam: "ARTLAB", CoverImageID: source.CoverImageID}); err != nil {
		t.Fatal(err)
	}
	in := copyIn("Kopya", "2027-01-01T10:00:00+03:00")
	in.IncludeDoorStaff = true
	got, _, err := svc.Copy(ctx, copyLeader, source.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Skipped, []string{event.SkippedDoorStaff, event.SkippedCover}) || got.Event.CoverImageID != nil || len(got.Event.DoorStaffIDs) != 0 {
		t.Fatalf("skipped %v cover %v staff %v", got.Skipped, got.Event.CoverImageID, got.Event.DoorStaffIDs)
	}
	// Not asked for, door staff are not named.
	got, _, err = svc.Copy(ctx, copyLeader, source.ID, copyIn("Kopya 2", "2027-01-01T10:00:00+03:00"))
	if err != nil || !slices.Equal(got.Skipped, []string{event.SkippedCover}) {
		t.Fatalf("not asked: %v %v", got.Skipped, err)
	}
}

// Into another Owner team (ticket 16): who may edit the source and create
// Events in the target; each part is decided for the target.
func TestCopyIntoAnotherTeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source := copySource(t, store)
	both := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/ARTLAB/LIDERLER"}}
	readsSource := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/ARTLAB/LIDERLER"}}
	start := "2027-01-01T10:00:00+03:00"
	into := func(team string) event.CopyInput {
		in := copyIn("Kopya", start)
		in.OwnerTeam = team
		return in
	}
	for _, tc := range []struct {
		name string
		p    authz.Principal
		in   event.CopyInput
	}{
		{"source read only", readsSource, into("ARTLAB")},
		{"no right in the target", copyLeader, into("ARTLAB")},
		{"DK", copyDK, into("ARTLAB")},
	} {
		if _, _, err := svc.Copy(ctx, tc.p, source.ID, tc.in); !errors.Is(err, event.ErrForbidden) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The source's own team named is the same as none.
	if got, _, err := svc.Copy(ctx, copyLeader, source.ID, into("WEBLAB")); err != nil || got.Event.OwnerTeam != "WEBLAB" {
		t.Fatalf("own team: %+v %v", got.Event.OwnerTeam, err)
	}
	in := into(" ARTLAB ")
	in.IncludeDoorStaff = true
	got, fresh, err := svc.Copy(ctx, both, source.ID, in)
	if err != nil || !fresh {
		t.Fatalf("both: %v %v", fresh, err)
	}
	// The cover is WEBLAB's Event's: the Team media library keeps it out of
	// ARTLAB; door staff need event:manage in ARTLAB.
	if got.Event.OwnerTeam != "ARTLAB" || got.Event.Status != event.StatusDraft || got.Event.CoverImageID != nil ||
		!slices.Equal(got.Skipped, []string{event.SkippedDoorStaff, event.SkippedCover}) {
		t.Fatalf("into ARTLAB: %s %v %v", got.Event.OwnerTeam, got.Event.CoverImageID, got.Skipped)
	}
	// Retried into the same team: the same copy; into another: a conflict.
	if again, fresh, err := svc.Copy(ctx, both, source.ID, in); err != nil || fresh || again.Event.ID != got.Event.ID {
		t.Fatalf("again: %v %v", fresh, err)
	}
	other := in
	other.OwnerTeam = "WEBLAB"
	if _, _, err := svc.Copy(ctx, both, source.ID, other); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("same id, another team: %v", err)
	}
	// YK copies into any team, door staff too.
	yk := into("GECEKODU")
	yk.IncludeDoorStaff = true
	if got, _, err := svc.Copy(ctx, copyYK, source.ID, yk); err != nil || got.Event.OwnerTeam != "GECEKODU" || len(got.Event.DoorStaffIDs) != 1 {
		t.Fatalf("yk: %+v %v", got, err)
	}
}
