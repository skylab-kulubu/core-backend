package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// eventMediaFixture serves the Event routes to a privileged organizer, with
// every Media link checked against one Media store.
type eventMediaFixture struct {
	app    *fiber.App
	events *event.MemoryStore
	media  media.Service
	store  *media.MemoryStore
	who    authz.Principal
}

func newEventMediaFixture(t *testing.T) eventMediaFixture {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	ident := yk()
	events := event.NewMemoryStore()
	store := media.NewMemoryStore()
	svc := event.NewServiceWithOptions(events, az, event.ServiceOptions{Media: media.NewLinker(store)})
	return eventMediaFixture{
		app:    eventServiceApp(t, ident, svc),
		events: events,
		media:  media.NewService(store, media.NewMemoryBlob(), az, ""),
		store:  store,
		who:    authz.Principal{ID: ident.ID.String(), Groups: ident.Groups},
	}
}

func (f eventMediaFixture) upload(t *testing.T, purpose, name string, data []byte) media.Media {
	t.Helper()
	file := media.UploadedFile{Name: name, Data: data}
	var created media.Media
	var err error
	if purpose == media.PurposeLegacy {
		created, err = f.media.Upload(t.Context(), f.who, name, "", data)
	} else {
		created, err = f.media.UploadForPurpose(t.Context(), f.who, purpose, file)
	}
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// send sends a JSON body and reads the JSON answer.
func (f eventMediaFixture) send(t *testing.T, method, path, body string) uploadResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
	}
	return uploadResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}
}

func (f eventMediaFixture) createEvent(t *testing.T, team, coverID string) uploadResponse {
	t.Helper()
	cover := ""
	if coverID != "" {
		cover = `,"coverImageId":"` + coverID + `"`
	}
	return f.send(t, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"`+team+`"`+cover+`}`)
}

func TestEventCoverRefusesMediaOfAnotherPurposeHTTP(t *testing.T) {
	t.Parallel()
	f := newEventMediaFixture(t)
	bylaws := f.upload(t, "cms_file", "bylaws.pdf", []byte("%PDF-1.7\n"))

	resp := f.createEvent(t, "WEBLAB", bylaws.ID.String())
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "media_purpose_mismatch")
	if resp.body["mediaId"] != bylaws.ID.String() || resp.body["role"] != "event_cover" || resp.body["purpose"] != "cms_file" {
		t.Fatalf("problem %v", resp.body)
	}
	if events, _ := f.events.List(t.Context(), "", false); len(events) != 0 {
		t.Fatalf("refused Event was stored: %+v", events)
	}
}

// Transition rule: a Media uploaded without a purpose stays linkable where
// any Media was linkable before Media purpose, until superadmin, Skyforms and
// CMS send purposes.
func TestEventCoverStillAcceptsLegacyMediaHTTP(t *testing.T) {
	t.Parallel()
	f := newEventMediaFixture(t)
	legacy := f.upload(t, media.PurposeLegacy, "poster.pdf", []byte("%PDF-1.7\n"))

	if resp := f.createEvent(t, "WEBLAB", legacy.ID.String()); resp.status != fiber.StatusCreated {
		t.Fatalf("status %d body %v", resp.status, resp.body)
	}
}

func TestEventGalleryRefusesAnotherTeamsPhotoHTTP(t *testing.T) {
	t.Parallel()
	f := newEventMediaFixture(t)
	photo := f.upload(t, "event_gallery", "photo.png", pngDotHTTP())
	weblab := f.createEvent(t, "WEBLAB", "")
	images := `["` + photo.ID.String() + `"]`
	if resp := f.send(t, fiber.MethodPost, "/v1/events/"+weblab.body["id"].(string)+"/images", images); resp.status != fiber.StatusOK {
		t.Fatalf("first gallery status %d body %v", resp.status, resp.body)
	}

	// The Team media library: another WEBLAB Event may reuse the photo...
	again := f.createEvent(t, "WEBLAB", "")
	if resp := f.send(t, fiber.MethodPost, "/v1/events/"+again.body["id"].(string)+"/images", images); resp.status != fiber.StatusOK {
		t.Fatalf("same team reuse status %d body %v", resp.status, resp.body)
	}
	// ...another team's Event may not.
	gamelab := f.createEvent(t, "GAMELAB", "")
	resp := f.send(t, fiber.MethodPost, "/v1/events/"+gamelab.body["id"].(string)+"/images", images)
	requireProblem(t, resp, fiber.StatusForbidden, "media_team_mismatch")
	if resp.body["mediaId"] != photo.ID.String() || resp.body["role"] != "event_gallery" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestEventCoverRefusesMediaThatCannotBeLinkedHTTP(t *testing.T) {
	t.Parallel()
	f := newEventMediaFixture(t)
	archived := f.upload(t, "event_cover", "archived.png", pngDotHTTP())
	if err := f.media.Delete(t.Context(), f.who, archived.ID); err != nil {
		t.Fatal(err)
	}
	expiredAt := time.Now().Add(-time.Minute)
	expired, err := f.store.Create(t.Context(), media.Media{
		Name: "abandoned.png", Type: "image/png", Kind: media.KindImage, Key: "images/abandoned",
		UploadedBy: uuid.New(), Purpose: "event_cover", ExpiresAt: &expiredAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, id := range map[string]uuid.UUID{"archived": archived.ID, "expired": expired.ID, "unknown": uuid.New()} {
		resp := f.createEvent(t, "WEBLAB", id.String())
		if resp.status != fiber.StatusUnprocessableEntity || resp.body["code"] != "media_not_linkable" || resp.body["mediaId"] != id.String() {
			t.Errorf("%s media: status %d body %v", name, resp.status, resp.body)
		}
	}
}
