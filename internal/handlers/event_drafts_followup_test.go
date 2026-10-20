package handlers

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Draft visibility on the Competitor, certificate, mail list and Ticket
// paths (ADR 0066, SPEC §2.4, ticket 03 follow-up): a draft does not exist
// for who may not see it, and a published Event with Competitors does not
// become a draft again.

// draftIssuer is a WEBLAB member who issues certificates but may not edit
// the team's Events: they read the team's Tickets, not its drafts.
var draftIssuer = authn.Identity{
	ID:     uuid.MustParse("d0000000-0000-0000-0000-0000000000c1"),
	Groups: []string{"/UYELER/ARGE/WEBLAB"},
	Roles:  []string{"certificate:issue"},
}

// draftFollowupCallers are draftCallers with the certificate issuer.
var draftFollowupCallers = append(slices.Clone(draftCallers), draftCaller{"certificate issuer", draftIssuer, false})

func (e *draftEnv) followupApp(t *testing.T, ident authn.Identity, comps competitor.Store) *fiber.App {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	app := e.app(t, ident)
	cp := NewCompetitorHandler(competitor.NewService(comps, e.events, az))
	app.Get("/v1/competitors/me", cp.Mine)
	app.Get("/v1/competitors/leaderboard/team/:ownerTeam", cp.LeaderboardByTeam)
	app.Get("/v1/competitors/user/:userId", cp.ListByUser)
	app.Get("/v1/competitors/team/:ownerTeam", cp.ListByOwnerTeam)
	app.Get("/v1/competitors/:id", cp.Get)
	app.Get("/v1/events/:eventId/competitors", cp.ListByEvent)
	app.Get("/v1/events/:eventId/competitors/winner", cp.Winner)
	certs := NewCertificateHandler(certificate.NewService(certificate.NewMemoryStore(), e.tickets, e.events, e.users, az, staticPDF("%PDF-1.4"), &recCertMail{}, "https://api.example.test"))
	app.Get("/v1/events/:eventId/certificates", certs.ListByEvent)
	app.Get("/v1/events/:eventId/certificates/summary", certs.Summary)
	app.Get("/v1/events/:eventId/certificates/template", certs.Resolve)
	app.Post("/v1/events/:eventId/certificates/preview", certs.PreviewEvent)
	app.Get("/v1/events/:eventId/certificate-batches", certs.ListBatches)
	app.Post("/v1/events/:eventId/certificates/issue", certs.Issue)
	app.Post("/v1/events/:eventId/certificates/finalize", certs.Finalize)
	app.Post("/v1/events/:eventId/certificates/recompute", certs.Recompute)
	mailList := NewEventMailHandler(eventmail.New(e.events, e.tickets, e.users, newHandlerLists(), az, eventmail.NewMemorySnapshotStore()))
	app.Post("/v1/events/:eventId/mail-list", mailList.Sync)
	th := NewTicketHandler(ticket.NewService(e.tickets, e.events, az, e.users))
	app.Get("/v1/tickets/:id", th.Get)
	app.Get("/v1/tickets", th.List)
	return app
}

// competitorEvents are the names of the Events a Competitor list answered.
func competitorEvents(t *testing.T, a draftAnswer) []string {
	t.Helper()
	if a.status != fiber.StatusOK {
		t.Fatalf("competitor list %d %s", a.status, a.raw)
	}
	var listed []struct {
		Event struct {
			Name string `json:"name"`
		} `json:"event"`
	}
	if err := json.Unmarshal(a.raw, &listed); err != nil {
		t.Fatalf("%v %s", err, a.raw)
	}
	names := make([]string, 0, len(listed))
	for _, c := range listed {
		names = append(names, c.Event.Name)
	}
	slices.Sort(names)
	return names
}

// A published Event with a current Competitor is not made a draft again
// (409 event_has_competitors); once the Competitor is withdrawn it is.
func TestDraftUnpublishRefusesCompetitors(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	ctx := t.Context()
	comps := competitor.NewMemoryStore(e.events)
	e.events.ReadCompetitorsFrom(func(id uuid.UUID) bool {
		listed, _ := comps.ListByEvent(ctx, id)
		return len(listed) > 0
	})
	created, err := comps.Create(ctx, competitor.Competitor{UserID: draftMember.ID, EventID: e.published.ID})
	if err != nil {
		t.Fatal(err)
	}
	editor := e.followupApp(t, draftEditor, comps)
	path := "/v1/events/" + e.published.ID.String() + "/unpublish"
	got := draftCall(t, editor, fiber.MethodPost, path, "")
	if code, _ := problemCodeOf(got); got.status != fiber.StatusConflict || code != "event_has_competitors" {
		t.Fatalf("unpublish with a Competitor: %d %s", got.status, got.raw)
	}
	if stored, _ := e.events.Get(ctx, e.published.ID); stored.Status != event.StatusPublished {
		t.Fatalf("status %q", stored.Status)
	}
	if err := comps.Withdraw(ctx, created.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, editor, fiber.MethodPost, path, ""); got.status != fiber.StatusOK || got.body["status"] != "draft" {
		t.Fatalf("unpublish after the withdrawal: %d %s", got.status, got.raw)
	}
}

// A draft's Competitors (one left from before the unpublish guard, or
// written straight to the store) exist only for who may see the draft:
// the single reads are 404 to anyone else, and the lists leave them out,
// the person's own list included.
func TestDraftCompetitorPaths(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	ctx := t.Context()
	comps := competitor.NewMemoryStore(e.events)
	onDraft, err := comps.Create(ctx, competitor.Competitor{UserID: draftMember.ID, EventID: e.draft.ID, IsWinner: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comps.Create(ctx, competitor.Competitor{UserID: draftMember.ID, EventID: e.published.ID, IsWinner: true}); err != nil {
		t.Fatal(err)
	}
	draft := e.draft.ID.String()
	for _, c := range draftFollowupCallers {
		app := e.followupApp(t, c.ident, comps)
		want := fiber.StatusNotFound
		switch {
		case c.sees:
			want = fiber.StatusOK
		case c.ident.ID == uuid.Nil:
			want = fiber.StatusUnauthorized
		}
		for _, path := range []string{
			"/v1/competitors/" + onDraft.ID.String(),
			"/v1/events/" + draft + "/competitors",
			"/v1/events/" + draft + "/competitors/winner",
		} {
			if got := draftCall(t, app, fiber.MethodGet, path, ""); got.status != want {
				t.Errorf("%s: %s = %d, want %d %s", c.name, path, got.status, want, got.raw)
			}
		}
	}
	// The person's own lists and the team's list leave the draft out for
	// who may not see it.
	member := e.followupApp(t, draftMember, comps)
	for _, path := range []string{
		"/v1/competitors/me",
		"/v1/competitors/user/" + draftMember.ID.String(),
		"/v1/competitors/team/WEBLAB",
	} {
		if got := competitorEvents(t, draftCall(t, member, fiber.MethodGet, path, "")); !slices.Equal(got, []string{"Yayında"}) {
			t.Errorf("member %s = %v", path, got)
		}
	}
	got := draftCall(t, member, fiber.MethodGet, "/v1/competitors/leaderboard/team/WEBLAB", "")
	var board []struct {
		EventCount int `json:"eventCount"`
	}
	if err := json.Unmarshal(got.raw, &board); err != nil || len(board) != 1 || board[0].EventCount != 1 {
		t.Errorf("member leaderboard %d %s", got.status, got.raw)
	}
	// DK sees the draft and both.
	dk := e.followupApp(t, draftDK, comps)
	if got := competitorEvents(t, draftCall(t, dk, fiber.MethodGet, "/v1/competitors/user/"+draftMember.ID.String(), "")); !slices.Equal(got, []string{"Taslak", "Yayında"}) {
		t.Errorf("DK user list = %v", got)
	}
}

// Certificates and the mail list on a draft (SPEC §2.4): 404 to who may not
// see it; who may see it reads and syncs, but no certificate is issued (409
// event_not_published).
func TestDraftCertificatesAndMailList(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	comps := competitor.NewMemoryStore(e.events)
	draft := "/v1/events/" + e.draft.ID.String()
	issue := `{"ticketId":"` + e.draftTicket.ID.String() + `"}`
	type want struct {
		status int
		code   string
	}
	notPublished := want{fiber.StatusConflict, "event_not_published"}
	for _, tc := range []struct {
		name, method, path, body string
		// by caller name; a visible caller left out is not checked, anyone
		// else left out gets 404 (401 signed out).
		wants map[string]want
	}{
		{"certificates", fiber.MethodGet, draft + "/certificates", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusOK},
		}},
		{"summary", fiber.MethodGet, draft + "/certificates/summary", "", nil},
		{"template", fiber.MethodGet, draft + "/certificates/template", "", nil},
		{"preview", fiber.MethodPost, draft + "/certificates/preview", "", nil},
		{"batches", fiber.MethodGet, draft + "/certificate-batches", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusOK},
		}},
		{"issue", fiber.MethodPost, draft + "/certificates/issue", issue, map[string]want{
			"editor": notPublished, "DK": {status: fiber.StatusForbidden},
		}},
		{"finalize", fiber.MethodPost, draft + "/certificates/finalize", "", map[string]want{
			"editor": notPublished, "DK": {status: fiber.StatusForbidden},
		}},
		{"recompute", fiber.MethodPost, draft + "/certificates/recompute", "", map[string]want{
			"editor": notPublished, "DK": {status: fiber.StatusForbidden},
		}},
		{"mail list", fiber.MethodPost, draft + "/mail-list", "", map[string]want{
			"editor": {status: fiber.StatusOK}, "DK": {status: fiber.StatusForbidden},
		}},
	} {
		for _, c := range draftFollowupCallers {
			w, ok := tc.wants[c.name]
			switch {
			case ok:
			case c.sees:
				// No template is bound here: what a visible caller gets
				// past the draft check is not this test's.
				continue
			case c.ident.ID == uuid.Nil:
				w = want{status: fiber.StatusUnauthorized}
			default:
				w = want{status: fiber.StatusNotFound}
			}
			got := draftCall(t, e.followupApp(t, c.ident, comps), tc.method, tc.path, tc.body)
			if code, _ := problemCodeOf(got); got.status != w.status || code != w.code {
				t.Errorf("%s by %s: %d %q, want %d %q %s", tc.name, c.name, got.status, code, w.status, w.code, got.raw)
			}
		}
	}
}

// The certificate issuer reads the team's Tickets, but not a draft's: its
// Ticket is 404 and the person's Ticket list leaves it out. The Ticket's
// holder still reads their own (decision of 2026-10-19).
func TestDraftTicketReads(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	comps := competitor.NewMemoryStore(e.events)
	onPublished, err := e.tickets.Create(t.Context(), ticket.Ticket{EventID: e.published.ID, TicketType: ticket.Registered, OwnerID: &draftMember.ID})
	if err != nil {
		t.Fatal(err)
	}
	draftTicket := "/v1/tickets/" + e.draftTicket.ID.String()
	for _, tc := range []struct {
		ident authn.Identity
		want  int
	}{
		{draftIssuer, fiber.StatusNotFound},
		{draftMember, fiber.StatusOK},
		{draftEditor, fiber.StatusOK},
		{draftDK, fiber.StatusOK},
	} {
		if got := draftCall(t, e.followupApp(t, tc.ident, comps), fiber.MethodGet, draftTicket, ""); got.status != tc.want {
			t.Errorf("%v: draft Ticket %d, want %d %s", tc.ident.Groups, got.status, tc.want, got.raw)
		}
	}
	issuer := e.followupApp(t, draftIssuer, comps)
	if got := draftCall(t, issuer, fiber.MethodGet, "/v1/tickets/"+onPublished.ID.String(), ""); got.status != fiber.StatusOK {
		t.Fatalf("issuer reads a published Event's Ticket: %d %s", got.status, got.raw)
	}
	got := draftCall(t, issuer, fiber.MethodGet, "/v1/tickets?userId="+draftMember.ID.String(), "")
	var listed []struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(got.raw, &listed); err != nil || got.status != fiber.StatusOK || len(listed) != 1 || listed[0].ID != onPublished.ID {
		t.Fatalf("issuer lists the person's Tickets: %d %s", got.status, got.raw)
	}
}

// lifecycle=all is the current list and the archive together: a current
// draft only with drafts=include, an archived one always (the archive is
// where a deleted draft comes back from).
func TestDraftLifecycleAll(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	archived, err := e.events.Create(t.Context(), event.Event{Name: "Silinen Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.events.Archive(t.Context(), archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	editor := e.app(t, draftEditor)
	for path, want := range map[string][]string{
		"/v1/events?lifecycle=all&ownerTeam=WEBLAB":                {"Silinen Taslak", "Yayında"},
		"/v1/events?lifecycle=all&ownerTeam=WEBLAB&drafts=include": {"Silinen Taslak", "Taslak", "Yayında"},
		"/v1/events?lifecycle=inactive&ownerTeam=WEBLAB":           {"Silinen Taslak"},
	} {
		if got := listedNames(t, draftCall(t, editor, fiber.MethodGet, path, "")); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
}
