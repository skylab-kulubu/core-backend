package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Draft visibility (ADR 0066, ticket 03, decisions of 2026-10-19): a draft
// exists only for who may edit it and for a Privileged reader (DK), who only
// reads it. These tests walk every read and write path for each kind of
// caller, on a draft and on a published Event.

var (
	draftEditor = weblabLeader()
	draftDK     = authn.Identity{ID: uuid.MustParse("d0000000-0000-0000-0000-00000000000d"), Groups: []string{"/UYELER/DK"}}
	draftMember = authn.Identity{ID: uuid.MustParse("d0000000-0000-0000-0000-00000000000b"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	// draftDoor is the door staff of both Events, in another team.
	draftDoor = authn.Identity{ID: uuid.MustParse("d0000000-0000-0000-0000-00000000000e"), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	// draftOtherLeader leads another team, which has a draft of its own.
	draftOtherLeader = authn.Identity{ID: uuid.MustParse("d0000000-0000-0000-0000-00000000000f"), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	draftAnon        = authn.Identity{}
	draftTarget      = uuid.MustParse("d0000000-0000-0000-0000-0000000000aa")
)

type draftEnv struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
	seasons *season.MemoryStore
	comps   competition.Service
	season  season.Season

	// Each Event has a day and a Session; draftTicket is a Ticket the
	// draft's editor added from the panel.
	draft, published, otherDraft event.Event
	draftDay, publishedDay       event.Day
	draftSession, pubSession     event.Session
	draftTicket                  ticket.Ticket
}

func newDraftEnv(t *testing.T) *draftEnv {
	t.Helper()
	ctx := t.Context()
	e := &draftEnv{
		events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(),
		users: user.NewMemoryStore(), seasons: season.NewMemoryStore(),
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	e.comps = competition.NewService(competition.NewMemoryStore(e.tickets, e.users), e.events, az)
	e.events.ReadTicketsFrom(func(id uuid.UUID) bool {
		listed, _ := e.tickets.ListByEvent(ctx, id)
		return len(listed) > 0
	})
	start := time.Now().Add(time.Hour)
	create := func(in event.Event) event.Event {
		in.Location, in.StartDate = "YTÜ", &start
		in.DoorStaffIDs = []uuid.UUID{draftDoor.ID}
		created, err := e.events.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	e.draft = create(event.Event{Name: "Taslak", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	e.published = create(event.Event{Name: "Yayında", OwnerTeam: "WEBLAB", Status: event.StatusPublished})
	e.otherDraft = create(event.Event{Name: "ArtLab Taslak", OwnerTeam: "ARTLAB", Status: event.StatusDraft})
	var err error
	if e.season, err = e.seasons.Create(ctx, season.Season{Name: "2026"}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{e.draft, e.published, e.otherDraft} {
		if _, err := e.events.SetSeason(ctx, ev.ID, &e.season.ID); err != nil {
			t.Fatal(err)
		}
	}
	day := func(ev event.Event) (event.Day, event.Session) {
		d, err := e.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
		if err != nil {
			t.Fatal(err)
		}
		s, err := e.events.CreateSession(ctx, event.Session{EventDayID: d.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "talk"})
		if err != nil {
			t.Fatal(err)
		}
		return d, s
	}
	e.draftDay, e.draftSession = day(e.draft)
	e.publishedDay, e.pubSession = day(e.published)
	seedTicketUser(t, e.users, draftTarget)
	seedTicketUser(t, e.users, draftMember.ID)
	if e.draftTicket, err = e.tickets.Create(ctx, ticket.Ticket{EventID: e.draft.ID, TicketType: ticket.Registered, OwnerID: &draftMember.ID}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *draftEnv) app(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	evSvc := event.NewService(e.events, az)
	eh := NewEventHandler(evSvc)
	sh := NewSeasonHandler(season.NewService(e.seasons, az), evSvc)
	sch := NewScheduleHandler(evSvc)
	th := NewTicketHandler(ticket.NewService(e.tickets, e.events, az, e.users))
	ch := NewCompetitionHandler(e.comps)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/events", eh.List)
	app.Get("/v1/events/active", eh.ListActive)
	app.Get("/v1/events/:id", eh.Get)
	app.Put("/v1/events/:id", eh.Update)
	app.Post("/v1/events/:id/publish", eh.Publish)
	app.Post("/v1/events/:id/unpublish", eh.Unpublish)
	app.Post("/v1/events/:id/cancel", eh.Cancel)
	app.Post("/v1/events/:id/uncancel", eh.Uncancel)
	app.Get("/v1/seasons/:id/events", sh.ListEvents)
	app.Get("/v1/events/:eventId/days", sch.ListDays)
	app.Get("/v1/event-days/:id", sch.GetDay)
	app.Get("/v1/event-days/:id/sessions", sch.ListSessions)
	app.Get("/v1/event-days/:id/current-session", sch.CurrentSession)
	app.Get("/v1/sessions/:id/qr", sch.SessionQR)
	app.Get("/v1/sessions/:id", sch.GetSession)
	app.Post("/v1/events/:eventId/applications/me", th.Apply)
	app.Post("/v1/events/:eventId/applications/users/:userId", th.ApplyForOther)
	app.Post("/v1/events/:eventId/applications/guest", th.ApplyGuest)
	app.Get("/v1/events/:eventId/tickets", th.ListByEvent)
	app.Get("/v1/events/:eventId/assignable-users", th.ListAssignableUsers)
	app.Get("/v1/events/:eventId/door-attendees", th.SearchDoorAttendees)
	app.Get("/v1/door/events", th.ListDoorEvents)
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", th.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", th.CheckInMe)
	app.Post("/v1/sessions/:sessionId/check-in/guest", th.CheckInGuest)
	app.Post("/v1/sessions/:sessionId/door-qr", th.MintDoorQR)
	app.Post("/v1/sessions/:sessionId/check-in/resolve", th.ResolveAndCheckIn)
	app.Get("/v1/sessions/:sessionId/check-ins", th.DoorActivity)
	app.Get("/v1/events/:eventId/competitions", ch.ListByEvent)
	app.Post("/v1/events/:eventId/competitions", ch.Create)
	app.Get("/v1/competitions/:id", ch.Get)
	return app
}

type draftAnswer struct {
	status int
	raw    []byte
	body   map[string]any
}

func draftCall(t *testing.T, app *fiber.App, method, path, body string) draftAnswer {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := draftAnswer{status: resp.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// listedNames are the names of the Events a list answered.
func listedNames(t *testing.T, a draftAnswer) []string {
	t.Helper()
	if a.status != fiber.StatusOK {
		t.Fatalf("list %d %s", a.status, a.raw)
	}
	var listed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(a.raw, &listed); err != nil {
		t.Fatalf("%v %s", err, a.raw)
	}
	names := make([]string, 0, len(listed))
	for _, e := range listed {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	return names
}

type draftCaller struct {
	name  string
	ident authn.Identity
	// sees is whether the caller may see the WEBLAB draft.
	sees bool
}

var draftCallers = []draftCaller{
	{"editor", draftEditor, true},
	{"DK", draftDK, true},
	{"team member", draftMember, false},
	{"door staff", draftDoor, false},
	{"other team's leader", draftOtherLeader, false},
	{"signed out", draftAnon, false},
}

// Every single read of an Event, its days and Sessions (QR included) is 404
// on a draft to who may not see it, and 200 on the published Event to all.
func TestDraftSingleReads(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	paths := func(ev event.Event, day event.Day, sess event.Session) []string {
		return []string{
			"/v1/events/" + ev.ID.String(),
			"/v1/events/" + ev.ID.String() + "/days",
			"/v1/event-days/" + day.ID.String(),
			"/v1/event-days/" + day.ID.String() + "/sessions",
			"/v1/event-days/" + day.ID.String() + "/current-session",
			"/v1/sessions/" + sess.ID.String(),
			"/v1/sessions/" + sess.ID.String() + "/qr",
		}
	}
	for _, c := range draftCallers {
		app := e.app(t, c.ident)
		for _, path := range paths(e.published, e.publishedDay, e.pubSession) {
			if got := draftCall(t, app, fiber.MethodGet, path, ""); got.status != fiber.StatusOK {
				t.Errorf("%s: published %s = %d %s", c.name, path, got.status, got.raw)
			}
		}
		want := fiber.StatusNotFound
		if c.sees {
			want = fiber.StatusOK
		}
		for _, path := range paths(e.draft, e.draftDay, e.draftSession) {
			if got := draftCall(t, app, fiber.MethodGet, path, ""); got.status != want {
				t.Errorf("%s: draft %s = %d, want %d %s", c.name, path, got.status, want, got.raw)
			}
		}
	}
}

// Lists never carry a draft without ?drafts=include; with it, the editor
// gets their own team's drafts, DK every team's, anyone else none.
// /active, a Season's list and the door's list never carry one.
func TestDraftLists(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	for _, tc := range []struct {
		c          draftCaller
		withDrafts []string
	}{
		{draftCallers[0], []string{"Taslak", "Yayında"}},
		{draftCallers[1], []string{"ArtLab Taslak", "Taslak", "Yayında"}},
		{draftCallers[2], []string{"Yayında"}},
		{draftCallers[3], []string{"Yayında"}},
		{draftCallers[4], []string{"ArtLab Taslak", "Yayında"}},
		{draftCallers[5], []string{"Yayında"}},
	} {
		app := e.app(t, tc.c.ident)
		for _, path := range []string{"/v1/events", "/v1/events?ownerTeam=WEBLAB", "/v1/events/active", "/v1/seasons/" + e.season.ID.String() + "/events"} {
			if got := listedNames(t, draftCall(t, app, fiber.MethodGet, path, "")); !slices.Equal(got, []string{"Yayında"}) {
				t.Errorf("%s: %s = %v", tc.c.name, path, got)
			}
		}
		if got := listedNames(t, draftCall(t, app, fiber.MethodGet, "/v1/events?drafts=include", "")); !slices.Equal(got, tc.withDrafts) {
			t.Errorf("%s: drafts=include = %v, want %v", tc.c.name, got, tc.withDrafts)
		}
		// Another team's draft does not leak through its team's list.
		want := []string{"Yayında"}
		if tc.c.sees {
			want = []string{"Taslak", "Yayında"}
		}
		if got := listedNames(t, draftCall(t, app, fiber.MethodGet, "/v1/events?drafts=include&ownerTeam=WEBLAB", "")); !slices.Equal(got, want) {
			t.Errorf("%s: WEBLAB drafts=include = %v, want %v", tc.c.name, got, want)
		}
		// A draft is never active.
		if got := listedNames(t, draftCall(t, app, fiber.MethodGet, "/v1/events?drafts=include&active=true", "")); !slices.Equal(got, []string{"Yayında"}) {
			t.Errorf("%s: active drafts=include = %v", tc.c.name, got)
		}
		if tc.c.ident.ID == uuid.Nil {
			continue
		}
		var door []string
		for _, name := range listedNames(t, draftCall(t, app, fiber.MethodGet, "/v1/door/events", "")) {
			door = append(door, name)
		}
		if slices.Contains(door, "Taslak") || slices.Contains(door, "ArtLab Taslak") {
			t.Errorf("%s: door events %v", tc.c.name, door)
		}
	}
	if got := draftCall(t, e.app(t, draftEditor), fiber.MethodGet, "/v1/events?drafts=all", ""); got.status != fiber.StatusBadRequest {
		t.Errorf("drafts=all = %d", got.status)
	}
}

func problemCodeOf(a draftAnswer) (string, string) {
	code, _ := a.body["code"].(string)
	reason, _ := a.body["reason"].(string)
	return code, reason
}

// Writes on a draft: self registration is 409 event_not_published to who
// may see it and 404 to anyone else; adding a person from the panel works
// for the editor alone; every door path is 403 session_closed (reason
// event_not_published) to who may see it and 404 to anyone else, door
// staff included.
func TestDraftWrites(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	draft, sess := e.draft.ID.String(), e.draftSession.ID.String()
	guest := `{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}`
	type want struct {
		status       int
		code, reason string
	}
	notFound := want{status: fiber.StatusNotFound}
	registration := want{fiber.StatusConflict, "event_not_published", ""}
	closed := want{fiber.StatusForbidden, "session_closed", "event_not_published"}
	for _, tc := range []struct {
		name, method, path, body string
		// by caller name; a caller left out gets notFound.
		wants map[string]want
	}{
		{"apply me", fiber.MethodPost, "/v1/events/" + draft + "/applications/me", "", map[string]want{
			"editor": registration, "DK": registration, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"apply guest", fiber.MethodPost, "/v1/events/" + draft + "/applications/guest", guest, map[string]want{
			"editor": registration, "DK": registration,
		}},
		{"add a person", fiber.MethodPost, "/v1/events/" + draft + "/applications/users/" + draftTarget.String(), "", map[string]want{
			"editor": {status: fiber.StatusCreated}, "DK": {status: fiber.StatusForbidden}, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"check in me", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/me", "", map[string]want{
			"editor": closed, "DK": closed, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"check in a ticket", fiber.MethodPost, "/v1/tickets/" + e.draftTicket.ID.String() + "/sessions/" + sess + "/check-in", "", map[string]want{
			"editor": closed, "DK": closed, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"guest check-in", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/guest", `{"email":"ada@example.com"}`, map[string]want{}},
		{"door QR", fiber.MethodPost, "/v1/sessions/" + sess + "/door-qr", "", map[string]want{
			"editor": closed, "DK": closed, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"resolve", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/resolve", `{"personId":"` + draftMember.ID.String() + `"}`, map[string]want{
			"editor": closed, "DK": closed, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"door activity", fiber.MethodGet, "/v1/sessions/" + sess + "/check-ins", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusForbidden}, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"applicants", fiber.MethodGet, "/v1/events/" + draft + "/tickets", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusOK}, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"door attendees", fiber.MethodGet, "/v1/events/" + draft + "/door-attendees?q=ada", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusForbidden}, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"competitions", fiber.MethodGet, "/v1/events/" + draft + "/competitions", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusOK}, "signed out": {status: fiber.StatusUnauthorized},
		}},
		{"competition create", fiber.MethodPost, "/v1/events/" + draft + "/competitions", `{"name":"Yarışma"}`, map[string]want{
			"editor": {status: fiber.StatusCreated}, "DK": {status: fiber.StatusForbidden}, "signed out": {status: fiber.StatusUnauthorized},
		}},
	} {
		for _, c := range draftCallers {
			w, ok := tc.wants[c.name]
			if !ok {
				w = notFound
			}
			got := draftCall(t, e.app(t, c.ident), tc.method, tc.path, tc.body)
			code, reason := problemCodeOf(got)
			if got.status != w.status || code != w.code || reason != w.reason {
				t.Errorf("%s by %s: %d %q %q, want %d %q %q %s", tc.name, c.name, got.status, code, reason, w.status, w.code, w.reason, got.raw)
			}
		}
	}
}

// The same paths on the published Event behave as before: nothing of the
// draft rules applies.
func TestPublishedWritesUnchanged(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	pub := e.published.ID.String()
	if got := draftCall(t, e.app(t, draftMember), fiber.MethodPost, "/v1/events/"+pub+"/applications/me", ""); got.status != fiber.StatusCreated {
		t.Fatalf("member apply %d %s", got.status, got.raw)
	}
	if got := draftCall(t, e.app(t, draftAnon), fiber.MethodPost, "/v1/events/"+pub+"/applications/guest",
		`{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}`); got.status != fiber.StatusCreated {
		t.Fatalf("guest apply %d %s", got.status, got.raw)
	}
	if got := draftCall(t, e.app(t, draftMember), fiber.MethodPost, "/v1/sessions/"+e.pubSession.ID.String()+"/check-in/me", ""); got.status != fiber.StatusCreated {
		t.Fatalf("member check-in %d %s", got.status, got.raw)
	}
	if got := draftCall(t, e.app(t, draftMember), fiber.MethodPost, "/v1/events/"+pub+"/applications/users/"+draftTarget.String(), ""); got.status != fiber.StatusForbidden {
		t.Fatalf("member adds a person %d %s", got.status, got.raw)
	}
	if got := draftCall(t, e.app(t, draftDoor), fiber.MethodGet, "/v1/sessions/"+e.pubSession.ID.String()+"/check-ins", ""); got.status != fiber.StatusOK {
		t.Fatalf("door staff activity %d %s", got.status, got.raw)
	}
}

// A competition of a draft is hidden like the draft.
func TestDraftCompetitionReads(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	created := draftCall(t, e.app(t, draftEditor), fiber.MethodPost, "/v1/events/"+e.draft.ID.String()+"/competitions", `{"name":"Yarışma"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create %d %s", created.status, created.raw)
	}
	path := "/v1/competitions/" + created.body["id"].(string)
	for _, c := range draftCallers {
		want := fiber.StatusNotFound
		switch {
		case c.sees:
			want = fiber.StatusOK
		case c.ident.ID == uuid.Nil:
			want = fiber.StatusUnauthorized
		}
		if got := draftCall(t, e.app(t, c.ident), fiber.MethodGet, path, ""); got.status != want {
			t.Errorf("%s: %d, want %d %s", c.name, got.status, want, got.raw)
		}
	}
}

// Publish and unpublish: the editor alone, idempotent; DK sees the draft but
// changes nothing; publishing needs a name, a location and a start date;
// unpublishing an Event with a Ticket is refused.
func TestDraftPublishAndUnpublish(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	draft := "/v1/events/" + e.draft.ID.String()
	for _, c := range draftCallers[1:] {
		want := fiber.StatusNotFound
		switch {
		case c.sees:
			want = fiber.StatusForbidden
		case c.ident.ID == uuid.Nil:
			want = fiber.StatusUnauthorized
		}
		for _, action := range []string{"/publish", "/unpublish"} {
			if got := draftCall(t, e.app(t, c.ident), fiber.MethodPost, draft+action, ""); got.status != want {
				t.Errorf("%s%s by %s: %d, want %d %s", draft, action, c.name, got.status, want, got.raw)
			}
		}
	}
	editor := e.app(t, draftEditor)
	// Unpublishing a draft answers it as it is.
	if got := draftCall(t, editor, fiber.MethodPost, draft+"/unpublish", ""); got.status != fiber.StatusOK || got.body["status"] != "draft" {
		t.Fatalf("unpublish a draft: %d %s", got.status, got.raw)
	}
	var publishedAt any
	for range 2 {
		got := draftCall(t, editor, fiber.MethodPost, draft+"/publish", "")
		if got.status != fiber.StatusOK || got.body["status"] != "published" || got.body["active"] != true || got.body["publishedAt"] == nil {
			t.Fatalf("publish: %d %s", got.status, got.raw)
		}
		if publishedAt != nil && got.body["publishedAt"] != publishedAt {
			t.Fatalf("publishing again moved publishedAt: %v, was %v", got.body["publishedAt"], publishedAt)
		}
		publishedAt = got.body["publishedAt"]
	}
	stored, _ := e.events.Get(t.Context(), e.draft.ID)
	if stored.PublishedBy == nil || *stored.PublishedBy != draftEditor.ID {
		t.Fatalf("publishedBy %v", stored.PublishedBy)
	}
	// Now published, the draft rules are gone: a member sees and applies.
	if got := draftCall(t, e.app(t, draftAnon), fiber.MethodGet, draft, ""); got.status != fiber.StatusOK {
		t.Fatalf("published read %d", got.status)
	}
	// It has a Ticket (added from the panel while a draft).
	got := draftCall(t, editor, fiber.MethodPost, draft+"/unpublish", "")
	if code, _ := problemCodeOf(got); got.status != fiber.StatusConflict || code != "event_has_tickets" {
		t.Fatalf("unpublish with a Ticket: %d %s", got.status, got.raw)
	}
	// The published Event without Tickets goes back, twice.
	pub := "/v1/events/" + e.published.ID.String()
	for range 2 {
		got := draftCall(t, editor, fiber.MethodPost, pub+"/unpublish", "")
		if got.status != fiber.StatusOK || got.body["status"] != "draft" || got.body["active"] != false {
			t.Fatalf("unpublish: %d %s", got.status, got.raw)
		}
	}
	if got := draftCall(t, e.app(t, draftMember), fiber.MethodGet, pub, ""); got.status != fiber.StatusNotFound {
		t.Fatalf("unpublished read by a member %d", got.status)
	}
}

func TestDraftPublishNeedsNameLocationAndStart(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	undated, err := e.events.Create(t.Context(), event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	got := draftCall(t, e.app(t, draftEditor), fiber.MethodPost, "/v1/events/"+undated.ID.String()+"/publish", "")
	missing, _ := got.body["missing"].([]any)
	if code, _ := problemCodeOf(got); got.status != fiber.StatusBadRequest || code != "event_incomplete" || len(missing) != 1 || missing[0] != "startDate" {
		t.Fatalf("publish undated: %d %s", got.status, got.raw)
	}
	if stored, _ := e.events.Get(t.Context(), undated.ID); stored.Status != event.StatusDraft {
		t.Fatalf("status %q", stored.Status)
	}
}
