package httpx_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

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

// requireStatus checks a Media's status and expiry as its uploader reads
// them: attached with no expiry, or detached with its 30 days running.
func (f *eventFilesEnv) requireStatus(t *testing.T, token string, image map[string]any, status string) {
	t.Helper()
	stored := sendJSON(t, f.app, token, fiber.MethodGet, "/v1/media/"+image["id"].(string), "")
	switch status {
	case "attached":
		if stored.body["status"] != "attached" || stored.body["expiresAt"] != nil {
			t.Fatalf("image %s is %v, want attached", image["id"], stored.body)
		}
	case "detached":
		expires, err := time.Parse(time.RFC3339Nano, fmt.Sprint(stored.body["expiresAt"]))
		if stored.body["status"] != "detached" || err != nil ||
			expires.Before(time.Now().Add(29*24*time.Hour)) || expires.After(time.Now().Add(31*24*time.Hour)) {
			t.Fatalf("image %s is %v, want detached with its 30 days", image["id"], stored.body)
		}
	default:
		t.Fatalf("no status %s", status)
	}
}

// Replacing a video's poster detaches the image it had, which is purged 30
// days later unless something attaches it again. Clearing it detaches the
// last one, and the video answers no poster. A photo uploaded for the
// gallery (event_gallery) makes a poster too. Clearing a video without a
// poster changes nothing.
func TestReplacingOrClearingAVideosPosterDetachesTheImageItHadHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	first := f.uploadImage(t, organizer, "event_cover", 800, 600)
	second := f.uploadImage(t, organizer, "event_gallery", 800, 600)
	path := posterPath(eventID, videoID)

	if set := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(first["id"].(string))); set.status != fiber.StatusOK {
		t.Fatalf("set: status %d body %v", set.status, set.body)
	}
	replaced := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(second["id"].(string)))
	if got := videoPoster(t, replaced, videoID); replaced.status != fiber.StatusOK || !reflect.DeepEqual(got, asPoster(second)) {
		t.Fatalf("replace: status %d poster %v, want %v", replaced.status, got, asPoster(second))
	}
	f.requireStatus(t, organizer, first, "detached")
	f.requireStatus(t, organizer, second, "attached")

	cleared := sendJSON(t, f.app, organizer, fiber.MethodDelete, path, "")
	if cleared.status != fiber.StatusOK {
		t.Fatalf("clear: status %d body %v", cleared.status, cleared.body)
	}
	for who, seen := range map[string]jsonResponse{"the answer": cleared, "anyone": anonymousGet(t, f.app, "/v1/events/"+eventID)} {
		video := items(t, seen, "videos")[0]
		if _, carried := video["poster"]; carried || video["id"] != videoID || video["url"] == nil {
			t.Fatalf("%s: the video after clearing is %v, want it without a poster", who, video)
		}
	}
	f.requireStatus(t, organizer, second, "detached")
	if again := sendJSON(t, f.app, organizer, fiber.MethodDelete, path, ""); again.status != fiber.StatusOK {
		t.Fatalf("clear again: status %d body %v", again.status, again.body)
	}

	// Set again within its window, a detached image is attached again.
	if set := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(first["id"].(string))); set.status != fiber.StatusOK {
		t.Fatalf("set again: status %d body %v", set.status, set.body)
	}
	f.requireStatus(t, organizer, first, "attached")
}

// Removing a video from the Event detaches its poster. One image may be
// the poster of two of the Event's videos, and its cover too: it stays
// attached while anything still shows it, and is detached with the last.
func TestRemovingAVideoDetachesItsPosterUnlessSomethingStillShowsItHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, first := f.eventWithVideo(t, organizer, "WEBLAB")
	second, _ := f.uploaded(t, organizer, "video", "kapanış.mp4", mp4File(2000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(second))
	shared := f.uploadImage(t, organizer, "event_cover", 800, 600)
	own := f.uploadImage(t, organizer, "event_gallery", 800, 600)
	for _, video := range []string{first, second} {
		if set := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, video), posterBody(shared["id"].(string))); set.status != fiber.StatusOK {
			t.Fatalf("set: status %d body %v", set.status, set.body)
		}
	}
	detail := anonymousGet(t, f.app, "/v1/events/"+eventID)
	if !reflect.DeepEqual(videoPoster(t, detail, first), asPoster(shared)) || !reflect.DeepEqual(videoPoster(t, detail, second), asPoster(shared)) {
		t.Fatalf("two videos with one poster: %v", detail.body["videos"])
	}

	// The second video still shows it.
	sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, first), posterBody(own["id"].(string)))
	f.requireStatus(t, organizer, shared, "attached")
	// Removing the second video takes its poster's last use.
	removed := sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/events/"+eventID+"/videos", jsonIDs(second))
	if removed.status != fiber.StatusOK || !reflect.DeepEqual(ids(items(t, removed, "videos")), []string{first}) {
		t.Fatalf("remove the second video: status %d body %v", removed.status, removed.body)
	}
	f.requireStatus(t, organizer, shared, "detached")
	f.requireStatus(t, organizer, own, "attached")

	// The first video's poster is also the Event's cover: removing the
	// video leaves it attached as the cover.
	covered := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+eventID,
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB","active":true,"coverImageId":"`+own["id"].(string)+`"}`)
	if covered.status != fiber.StatusOK {
		t.Fatalf("cover: status %d body %v", covered.status, covered.body)
	}
	sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/events/"+eventID+"/videos", jsonIDs(first))
	f.requireStatus(t, organizer, own, "attached")
	// Added again, the video has no poster: its link went with it.
	readded := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(first))
	if got := videoPoster(t, readded, first); got != nil {
		t.Fatalf("a video added again has the poster %v", got)
	}
}
