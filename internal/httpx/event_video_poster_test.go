package httpx_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// uploadImage uploads a gray PNG of w by h pixels for the purpose, as the
// organizer's picker does, and answers its Media JSON: its id, its
// full-size address and its sizes.
func (f *eventFilesEnv) uploadImage(t *testing.T, token, purpose string, w, h int) map[string]any {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, w, h))
	for i := range picture.Pix {
		picture.Pix[i] = 128
	}
	var file bytes.Buffer
	if err := png.Encode(&file, picture); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", purpose); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "kapak.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var created map[string]any
	if resp.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil {
		t.Fatalf("upload %s: status %d body %s", purpose, resp.StatusCode, raw)
	}
	return created
}

// eventWithVideo is an Event of the Owner team whose videos list one video,
// and the video's id.
func (f *eventFilesEnv) eventWithVideo(t *testing.T, token, team string) (string, string) {
	t.Helper()
	eventID := f.createEvent(t, token, team)
	videoID, _ := f.uploaded(t, token, "video", "açılış.mp4", mp4File(3000))
	if added := sendJSON(t, f.app, token, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(videoID)); added.status != fiber.StatusOK {
		t.Fatalf("add video: status %d body %v", added.status, added.body)
	}
	return eventID, videoID
}

func posterPath(eventID, videoID string) string {
	return "/v1/events/" + eventID + "/videos/" + videoID + "/poster"
}

func posterBody(id string) string {
	return `{"posterId":"` + id + `"}`
}

// videoPoster is the poster of the answer's video videoID; nil without one.
func videoPoster(t *testing.T, answer jsonResponse, videoID string) any {
	t.Helper()
	for _, video := range items(t, answer, "videos") {
		if video["id"] == videoID {
			return video["poster"]
		}
	}
	t.Fatalf("video %s is not in %v", videoID, answer.body)
	return nil
}

// asPoster is the poster an Event answers for the image's Media JSON: its
// id, its full-size address and its sizes, as the Media JSON gives them.
func asPoster(uploaded map[string]any) map[string]any {
	return map[string]any{"id": uploaded["id"], "url": uploaded["url"], "sizes": uploaded["sizes"]}
}

// An organizer gives one of their Event's videos a poster: an image uploaded
// for the Event (event_cover), re-encoded with its sizes. The Event's detail
// answers it on the video, to everyone, at its full-size address with its
// card and page sizes, those the Media JSON gives it. Core attaches the
// image to the Event, so it no longer expires.
func TestAnOrganizerGivesAVideoAPosterHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	poster := f.uploadImage(t, organizer, "event_cover", 1600, 1200)
	if card, _ := poster["sizes"].(map[string]any)["card"].(map[string]any); card["width"] != float64(400) {
		t.Fatalf("the poster was stored without a card size: %v", poster)
	}

	set := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, videoID), posterBody(poster["id"].(string)))
	if set.status != fiber.StatusOK {
		t.Fatalf("set poster: status %d body %v", set.status, set.body)
	}
	want := asPoster(poster)
	for who, seen := range map[string]jsonResponse{
		"the answer": set,
		"anyone":     anonymousGet(t, f.app, "/v1/events/"+eventID),
	} {
		if got := videoPoster(t, seen, videoID); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: poster %v, want %v", who, got, want)
		}
	}
	if stored := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/media/"+poster["id"].(string), ""); stored.body["status"] != "attached" || stored.body["expiresAt"] != nil {
		t.Fatalf("the poster is %v, want attached", stored.body)
	}
}
