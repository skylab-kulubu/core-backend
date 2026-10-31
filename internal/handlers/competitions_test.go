package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type competitionEnv struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	svc     competition.Service
}

func newCompetitionEnv() competitionEnv {
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	store := competition.NewMemoryStore(tickets, user.NewMemoryStore())
	return competitionEnv{events: events, tickets: tickets,
		svc: competition.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()))}
}

func (e competitionEnv) app(ident *authn.Identity) *fiber.App {
	h := NewCompetitionHandler(e.svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident != nil {
			c.Locals(authn.LocalsIdentity, *ident)
		}
		return c.Next()
	})
	app.Get("/v1/events/:eventId/competitions", h.ListByEvent)
	app.Post("/v1/events/:eventId/competitions", h.Create)
	app.Get("/v1/competitions/:id", h.Get)
	app.Patch("/v1/competitions/:id", h.Update)
	for _, action := range []string{"open", "start", "close", "reopen", "cancel"} {
		app.Post("/v1/competitions/:id/"+action, h.Transition(action))
	}
	app.Post("/v1/competitions/:id/archive", h.Archive)
	app.Post("/v1/competitions/:id/restore", h.Restore)
	app.Get("/v1/competitions/:id/entrants", h.Entrants)
	app.Post("/v1/competitions/:id/entrants", h.AddEntrants)
	app.Get("/v1/competitions/:id/entrant-candidates", h.EntrantCandidates)
	app.Post("/v1/competitions/:id/scores", h.SetScore)
	app.Get("/v1/competitions/:id/entries", h.Entries)
	app.Post("/v1/competitions/:id/entries/:entryId/revoke", h.RevokeEntry)
	app.Get("/v1/competitions/:id/leaderboard", h.Leaderboard)
	return app
}

type competitionAnswer struct {
	status int
	raw    string
	body   map[string]any
	list   []any
}

func competitionCall(t *testing.T, app *fiber.App, method, path, body string) competitionAnswer {
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
	raw, _ := io.ReadAll(resp.Body)
	a := competitionAnswer{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &a.body)
	_ = json.Unmarshal(raw, &a.list)
	return a
}

func TestCompetitionHTTPFlow(t *testing.T) {
	t.Parallel()
	env := newCompetitionEnv()
	ev, err := env.events.Create(t.Context(), event.Event{Name: "AGC Hafta 3", Location: "YTÜ", OwnerTeam: "AGC"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := env.tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	leader := &authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}}
	member := &authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC"}}
	app := env.app(leader)

	if got := competitionCall(t, env.app(nil), fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/competitions", `{"name":"x"}`); got.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous create %d", got.status)
	}
	if got := competitionCall(t, env.app(member), fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/competitions", `{"name":"x"}`); got.status != fiber.StatusForbidden {
		t.Fatalf("member create %d", got.status)
	}
	if got := competitionCall(t, app, fiber.MethodPost, "/v1/events/"+uuid.NewString()+"/competitions", `{"name":"x"}`); got.status != fiber.StatusNotFound {
		t.Fatalf("create without an event %d", got.status)
	}
	if got := competitionCall(t, app, fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/competitions", `{"name":" "}`); got.status != fiber.StatusBadRequest {
		t.Fatalf("empty name %d", got.status)
	}
	created := competitionCall(t, app, fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/competitions", `{"name":"AGC Hafta 3"}`)
	if created.status != fiber.StatusCreated || created.body["status"] != "draft" || created.body["ownerTeam"] != "AGC" {
		t.Fatalf("create %d %s", created.status, created.raw)
	}
	id := created.body["id"].(string)
	base := "/v1/competitions/" + id

	added := competitionCall(t, app, fiber.MethodPost, base+"/entrants", `{"ticketIds":["`+guest.ID.String()+`"]}`)
	if added.status != fiber.StatusCreated || len(added.list) != 1 {
		t.Fatalf("add %d %s", added.status, added.raw)
	}
	entrant := added.list[0].(map[string]any)
	if entrant["displayName"] != "Ada Lovelace" {
		t.Fatalf("entrant %v", entrant)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/entrants", `{"ticketIds":["`+guest.ID.String()+`"]}`); got.status != fiber.StatusConflict || got.body["code"] != "ticket_taken" {
		t.Fatalf("again %d %s", got.status, got.raw)
	}
	entrantID := entrant["id"].(string)

	for bad, code := range map[string]string{
		`"points":"30"`:                      "invalid_points",
		`"points":1.2345`:                    "invalid_points",
		`"points":1e3`:                       "invalid_points",
		`"points":null`:                      "missing_field",
		`"points":1000000001`:                "invalid_points",
		`"points":30,"stageId":"not-a-uuid"`: "invalid_body",
		`"points":30,"stageId":"` + id + `"`: "unknown_stage",
	} {
		got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`",`+bad+`}`)
		if got.status != fiber.StatusBadRequest || got.body["code"] != code {
			t.Errorf("%s: %d %s, want 400 %s", bad, got.status, got.raw, code)
		}
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+uuid.NewString()+`","points":30}`); got.status != fiber.StatusBadRequest || got.body["code"] != "unknown_entrant" {
		t.Errorf("unknown entrant: %d %s", got.status, got.raw)
	}
	first := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`","points":30.5}`)
	if first.status != fiber.StatusCreated || first.body["points"] != 30.5 || first.body["source"] != "manual" {
		t.Fatalf("score %d %s", first.status, first.raw)
	}
	firstID := first.body["id"].(string)
	// Writing as if there were no score: 409 with the live one.
	got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`","points":50}`)
	if current, _ := got.body["current"].(map[string]any); got.status != fiber.StatusConflict || got.body["code"] != "score_changed" || current["id"] != firstID {
		t.Fatalf("write over an unseen score %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`","points":50,"replacesId":"`+firstID+`"}`); got.status != fiber.StatusBadRequest || got.body["code"] != "reason_required" {
		t.Fatalf("correction without reason %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`","points":50,"replacesId":"`+firstID+`","reason":"Yanlış girildi"}`); got.status != fiber.StatusCreated || got.body["replacesId"] != firstID {
		t.Fatalf("correction %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/close", ""); got.status != fiber.StatusOK || got.body["status"] != "closed" {
		t.Fatalf("close %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/open", ""); got.status != fiber.StatusConflict || got.body["code"] != "competition_status" {
		t.Fatalf("open a closed one %d %s", got.status, got.raw)
	}
	board := competitionCall(t, app, fiber.MethodGet, base+"/leaderboard", "")
	rows, _ := board.body["rows"].([]any)
	if board.status != fiber.StatusOK || len(rows) != 1 {
		t.Fatalf("leaderboard %d %s", board.status, board.raw)
	}
	if row := rows[0].(map[string]any); row["rank"] != 1.0 || row["score"] != 50.0 || row["displayName"] != "Ada Lovelace" || row["entryId"] == nil {
		t.Fatalf("row %v", row)
	}
	entries := competitionCall(t, app, fiber.MethodGet, base+"/entries", "")
	if entries.status != fiber.StatusOK || len(entries.list) != 2 {
		t.Fatalf("entries %d %s", entries.status, entries.raw)
	}
	live := entries.list[1].(map[string]any)["id"].(string)
	if got := competitionCall(t, app, fiber.MethodPost, base+"/entries/"+live+"/revoke", `{}`); got.status != fiber.StatusBadRequest || got.body["code"] != "reason_required" {
		t.Fatalf("revoke without reason %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/entries/"+live+"/revoke", `{"reason":"Katılmadı"}`); got.status != fiber.StatusOK || got.body["revokeReason"] != "Katılmadı" {
		t.Fatalf("revoke %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, env.app(member), fiber.MethodGet, base+"/leaderboard", ""); got.status != fiber.StatusForbidden {
		t.Fatalf("member leaderboard %d", got.status)
	}
	if got := competitionCall(t, app, fiber.MethodPatch, base, `{"description":"Haftalık AGC"}`); got.status != fiber.StatusOK || got.body["description"] != "Haftalık AGC" || got.body["name"] != "AGC Hafta 3" {
		t.Fatalf("patch %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/cancel", `{}`); got.status != fiber.StatusBadRequest {
		t.Fatalf("cancel without reason %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/archive", ""); got.status != fiber.StatusNoContent {
		t.Fatalf("archive %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodGet, base, ""); got.status != fiber.StatusNotFound {
		t.Fatalf("archived get %d", got.status)
	}
	if got := competitionCall(t, app, fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitions?lifecycle=inactive", ""); got.status != fiber.StatusOK || len(got.list) != 1 {
		t.Fatalf("archived list %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodPost, base+"/restore", ""); got.status != fiber.StatusOK {
		t.Fatalf("restore %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodGet, "/v1/competitions/not-a-uuid", ""); got.status != fiber.StatusBadRequest {
		t.Fatalf("bad id %d", got.status)
	}
}

// A score of an Entrant the writer is in is 403 own_entrant; the candidates
// answer names without contact details, and checkedIn takes true or false.
func TestCompetitionHTTPOwnEntrantAndCandidates(t *testing.T) {
	t.Parallel()
	env := newCompetitionEnv()
	ev, err := env.events.Create(t.Context(), event.Event{Name: "AGC Hafta 4", Location: "YTÜ", OwnerTeam: "AGC"})
	if err != nil {
		t.Fatal(err)
	}
	leader := &authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}}
	own, err := env.tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tickets.Create(t.Context(), ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.test"}); err != nil {
		t.Fatal(err)
	}
	app := env.app(leader)
	created := competitionCall(t, app, fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/competitions", `{"name":"AGC Hafta 4"}`)
	base := "/v1/competitions/" + created.body["id"].(string)
	added := competitionCall(t, app, fiber.MethodPost, base+"/entrants", `{"ticketIds":["`+own.ID.String()+`"]}`)
	if added.status != fiber.StatusCreated {
		t.Fatalf("add %d %s", added.status, added.raw)
	}
	entrantID := added.list[0].(map[string]any)["id"].(string)
	if got := competitionCall(t, app, fiber.MethodPost, base+"/scores", `{"entrantId":"`+entrantID+`","points":30}`); got.status != fiber.StatusForbidden || got.body["code"] != "own_entrant" {
		t.Errorf("own score %d %s", got.status, got.raw)
	}
	got := competitionCall(t, app, fiber.MethodGet, base+"/entrant-candidates?q=ada&checkedIn=false", "")
	if got.status != fiber.StatusOK || len(got.list) != 1 || strings.Contains(got.raw, "example.test") || strings.Contains(got.raw, "mail") {
		t.Errorf("candidates %d %s", got.status, got.raw)
	}
	if got := competitionCall(t, app, fiber.MethodGet, base+"/entrant-candidates?checkedIn=1", ""); got.status != fiber.StatusBadRequest {
		t.Errorf("checkedIn=1: %d", got.status)
	}
}
