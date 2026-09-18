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
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func eventApp(t *testing.T, ident authn.Identity, store event.Store) *fiber.App {
	t.Helper()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewEventHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/events", h.List)
	app.Get("/v1/events/active", h.ListActive)
	app.Get("/v1/events/:id", h.Get)
	app.Post("/v1/events", h.Create)
	app.Put("/v1/events/:id", h.Update)
	app.Patch("/v1/events/:id", h.Update)
	app.Delete("/v1/events/:id", h.Delete)
	app.Post("/v1/events/:id/images", h.AddImages)
	app.Delete("/v1/events/:id/images", h.RemoveImages)
	return app
}

func weblabLeader() authn.Identity {
	return authn.Identity{
		ID:      uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Profile: user.Profile{Email: "lead@example.com"},
		Groups:  []string{"/UYELER/ARGE/WEBLAB/LIDERLER"},
	}
}

func yk() authn.Identity {
	return authn.Identity{
		ID:      uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Profile: user.Profile{Email: "yk@example.com"},
		Groups:  []string{"/UYELER/YK"},
	}
}

func TestEventCreateReadablePublic(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "Hack" || got.OwnerTeam != "WEBLAB" {
		t.Fatalf("got %+v", got)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status %d", resp.StatusCode)
	}
}

func TestEventListActiveOnly(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	activeReq := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Live","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
	))
	activeReq.Header.Set("Content-Type", "application/json")
	if resp, err := app.Test(activeReq); err != nil || resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("active create %v", err)
	}
	inactiveReq := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Old","location":"YTÜ","ownerTeam":"WEBLAB","active":false}`,
	))
	inactiveReq.Header.Set("Content-Type", "application/json")
	if resp, err := app.Test(inactiveReq); err != nil || resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("inactive create %v", err)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err := public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events?active=true", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var listed []event.Event
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "Live" || !listed[0].Active {
		t.Fatalf("listed %+v", listed)
	}
}

func TestEventGalleryAttachHTTP(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %d %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	img := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	attach := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+created.ID.String()+"/images", strings.NewReader(`["`+img.String()+`"]`))
	attach.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(attach)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("attach %d %s", resp.StatusCode, body)
	}
	var attached event.Event
	if err := json.NewDecoder(resp.Body).Decode(&attached); err != nil {
		t.Fatal(err)
	}
	if len(attached.Images) != 1 || attached.Images[0].ID != img {
		t.Fatalf("images %+v", attached.Images)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 1 || got.Images[0].ID != img {
		t.Fatalf("public images %+v", got.Images)
	}

	member := eventApp(t, authn.Identity{
		ID:     uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}, store)
	detach := httptest.NewRequest(fiber.MethodDelete, "/v1/events/"+created.ID.String()+"/images", strings.NewReader(`["`+img.String()+`"]`))
	detach.Header.Set("Content-Type", "application/json")
	resp, err = member.Test(detach)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member detach %d", resp.StatusCode)
	}

	detach = httptest.NewRequest(fiber.MethodDelete, "/v1/events/"+created.ID.String()+"/images", strings.NewReader(`["`+img.String()+`"]`))
	detach.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(detach)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("detach %d %s", resp.StatusCode, body)
	}
	var after event.Event
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if len(after.Images) != 0 {
		t.Fatalf("after %+v", after.Images)
	}
}

func TestEventWrongTeamProblemJSON(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"CTF","location":"YTÜ","ownerTeam":"SKYSEC"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type %s", ct)
	}
	var problem map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem["type"] != "about:blank" || problem["title"] != "Forbidden" || problem["detail"] != "Forbidden" {
		t.Fatalf("problem %+v", problem)
	}
	if status, ok := problem["status"].(float64); !ok || int(status) != 403 {
		t.Fatalf("status %+v", problem["status"])
	}
	instance, _ := problem["instance"].(string)
	if instance != "/v1/events" {
		t.Fatalf("instance %q", instance)
	}
}

func TestEventCreateUnauthorized(t *testing.T) {
	t.Parallel()
	app := eventApp(t, authn.Identity{}, event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEventCreateCoverImage(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	cover := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","coverImageId":"`+cover.String()+`"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.CoverImageID == nil || *created.CoverImageID != cover {
		t.Fatalf("cover %+v", created.CoverImageID)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CoverImageID == nil || *got.CoverImageID != cover {
		t.Fatalf("public cover %+v", got.CoverImageID)
	}
}

func TestEventCreateEmptyOwnerPrivileged(t *testing.T) {
	t.Parallel()
	app := eventApp(t, yk(), event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Seminer","location":"YTÜ"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.OwnerTeam != "" {
		t.Fatalf("owner %q", created.OwnerTeam)
	}
}

func TestEventAnonymousListHidesInactive(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	for _, body := range []string{
		`{"name":"Live","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
		`{"name":"Old","location":"YTÜ","ownerTeam":"WEBLAB","active":false}`,
	} {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := app.Test(req); err != nil || resp.StatusCode != fiber.StatusCreated {
			t.Fatalf("create %v", err)
		}
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err := public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("anon list %d", resp.StatusCode)
	}
	var listed []event.Event
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "Live" || !listed[0].Active {
		t.Fatalf("anon listed %+v", listed)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/active", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("active route %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "Live" {
		t.Fatalf("active listed %+v", listed)
	}

	staff := eventApp(t, weblabLeader(), store)
	resp, err = staff.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("staff listed %+v", listed)
	}
}

func TestEventPatchSameAuthzAndBodyAsPut(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, weblabLeader(), store)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %d %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	patch := httptest.NewRequest(fiber.MethodPatch, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Hack 2","location":"Davutpaşa","ownerTeam":"WEBLAB","active":true}`,
	))
	patch.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(patch)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("leader patch %d %s", resp.StatusCode, body)
	}
	var updated event.Event
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Hack 2" || updated.Location != "Davutpaşa" {
		t.Fatalf("patched %+v", updated)
	}

	member := eventApp(t, authn.Identity{
		ID:     uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}, store)
	memberPatch := httptest.NewRequest(fiber.MethodPatch, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Nope","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	memberPatch.Header.Set("Content-Type", "application/json")
	resp, err = member.Test(memberPatch)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member patch %d", resp.StatusCode)
	}

	public := eventApp(t, authn.Identity{}, store)
	anon := httptest.NewRequest(fiber.MethodPatch, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Nope","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	anon.Header.Set("Content-Type", "application/json")
	resp, err = public.Test(anon)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon patch %d", resp.StatusCode)
	}
}

func TestEventCreateEmptyOwnerLeaderForbidden(t *testing.T) {
	t.Parallel()
	app := eventApp(t, weblabLeader(), event.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Seminer","location":"YTÜ"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestEventListFiltersByOwnerTeamNotTypeName(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, yk(), store)
	for _, body := range []string{
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":true}`,
		`{"name":"CTF","location":"YTÜ","ownerTeam":"SKYSEC","active":true}`,
	} {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := app.Test(req); err != nil || resp.StatusCode != fiber.StatusCreated {
			t.Fatalf("create %v", err)
		}
	}

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events?ownerTeam=WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("ownerTeam list %d", resp.StatusCode)
	}
	var listed []event.Event
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "Hack" || listed[0].OwnerTeam != "WEBLAB" {
		t.Fatalf("ownerTeam listed %+v", listed)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events?typeName=WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("typeName must not filter %+v", listed)
	}
}

func TestEventDoorStaffAssignHTTP(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	staff := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	app := eventApp(t, yk(), store)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","doorStaffIds":["`+staff+`"]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if len(created.DoorStaffIDs) != 1 || created.DoorStaffIDs[0].String() != staff {
		t.Fatalf("created %+v", created.DoorStaffIDs)
	}

	req = httptest.NewRequest(fiber.MethodPut, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Hack","location":"Davutpaşa","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("omit put %d body %s", resp.StatusCode, body)
	}
	var omitted event.Event
	if err := json.NewDecoder(resp.Body).Decode(&omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.Location != "Davutpaşa" || len(omitted.DoorStaffIDs) != 1 || omitted.DoorStaffIDs[0].String() != staff {
		t.Fatalf("omitted field wiped %+v", omitted)
	}

	leaderApp := eventApp(t, weblabLeader(), store)
	req = httptest.NewRequest(fiber.MethodPut, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","doorStaffIds":["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = leaderApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("leader put %d body %s", resp.StatusCode, body)
	}
	var afterLeader event.Event
	if err := json.NewDecoder(resp.Body).Decode(&afterLeader); err != nil {
		t.Fatal(err)
	}
	if len(afterLeader.DoorStaffIDs) != 1 || afterLeader.DoorStaffIDs[0].String() != staff {
		t.Fatalf("leader overwrote %+v", afterLeader.DoorStaffIDs)
	}

	req = httptest.NewRequest(fiber.MethodPut, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","doorStaffIds":[]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("clear put %d body %s", resp.StatusCode, body)
	}
	var cleared event.Event
	if err := json.NewDecoder(resp.Body).Decode(&cleared); err != nil {
		t.Fatal(err)
	}
	if len(cleared.DoorStaffIDs) != 0 {
		t.Fatalf("cleared %+v", cleared.DoorStaffIDs)
	}
}

func TestEventExtraFormURLsRoundTrip(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	app := eventApp(t, yk(), store)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Skydays","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"https://apply.example.test","formAlias":"skydays2026","extraFormUrls":[{"label":"CTF","url":"https://ctf.example.test","alias":"skydays-ctf2026"}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.FormURL != "https://apply.example.test" || created.FormAlias != "skydays2026" {
		t.Fatalf("apply %+v", created)
	}
	if len(created.ExtraFormURLs) != 1 || created.ExtraFormURLs[0].Label != "CTF" || created.ExtraFormURLs[0].URL != "https://ctf.example.test" {
		t.Fatalf("extra %+v", created.ExtraFormURLs)
	}

	req = httptest.NewRequest(fiber.MethodPut, "/v1/events/"+created.ID.String(), strings.NewReader(
		`{"name":"Skydays","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"https://apply.example.test"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("put %d body %s", resp.StatusCode, body)
	}
	var kept event.Event
	if err := json.NewDecoder(resp.Body).Decode(&kept); err != nil {
		t.Fatal(err)
	}
	if kept.FormAlias != "skydays2026" || len(kept.ExtraFormURLs) != 1 || kept.ExtraFormURLs[0].Label != "CTF" {
		t.Fatalf("kept %+v", kept)
	}
}

func TestEventGetJSONPrefixesCDN(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	cover := "images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa"
	gallery := "/images/7f863471-c2b6-45f4-912d-80f97daf25f4"
	created, err := store.Create(t.Context(), event.Event{
		Name:          "Hack",
		Location:      "YTÜ",
		OwnerTeam:     "WEBLAB",
		Active:        true,
		CoverImageURL: cover,
		Images:        []event.GalleryImage{{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), URL: gallery}},
		ImageURLs:     []string{gallery},
	})
	if err != nil {
		t.Fatal(err)
	}

	public := eventApp(t, authn.Identity{}, store)
	resp, err := public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got event.Event
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CoverImageURL != "https://cdn.yildizskylab.com/images/b0db8eb9-5914-4db7-a39a-cabd6bf47faa" {
		t.Fatalf("cover %s", got.CoverImageURL)
	}
	if len(got.Images) != 1 || got.Images[0].URL != "https://cdn.yildizskylab.com/images/7f863471-c2b6-45f4-912d-80f97daf25f4" {
		t.Fatalf("images %+v", got.Images)
	}
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != got.Images[0].URL {
		t.Fatalf("imageUrls %+v", got.ImageURLs)
	}

	stored, err := store.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CoverImageURL != cover || stored.Images[0].URL != gallery {
		t.Fatalf("store mutated %+v", stored)
	}
}
