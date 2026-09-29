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
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
	return f.uploadFile(t, token, purpose, "kapak.png", file.Bytes())
}

// uploadFile uploads the file for the purpose (none: "") by POST
// /v1/media and answers its Media JSON.
func (f *eventFilesEnv) uploadFile(t *testing.T, token, purpose, name string, file []byte) map[string]any {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if purpose != "" {
		if err := form.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file); err != nil {
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
		t.Fatalf("upload %s for %q: status %d body %s", name, purpose, resp.StatusCode, raw)
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

// A poster takes what an Event cover takes: a photo uploaded for the cover
// or the gallery, an SVG among them (whose every size is the SVG itself).
// Anything else is refused with media_purpose_mismatch in the poster's
// role: a video, a club file, a profile picture, a CMS image, and a Media
// uploaded without a purpose (legacy fits no poster, as it fits no Event
// file or video). A Media that is missing or archived is not linkable. A
// refused poster leaves the one the video had.
func TestAVideosPosterTakesWhatAnEventCoverTakesHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	path := posterPath(eventID, videoID)
	kept := f.uploadImage(t, organizer, "event_gallery", 800, 600)
	if set := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(kept["id"].(string))); set.status != fiber.StatusOK {
		t.Fatalf("set: status %d body %v", set.status, set.body)
	}

	otherVideo, _ := f.uploaded(t, organizer, "video", "başka.mp4", mp4File(1000))
	clubFile, _ := f.uploaded(t, organizer, "club_file", "sunum.pdf", pdfFile(1000))
	refused := map[string]string{
		otherVideo: "video",
		clubFile:   "club_file",
		f.uploadImage(t, organizer, "profile_picture", 200, 200)["id"].(string):  "profile_picture",
		f.uploadImage(t, organizer, "cms_image", 200, 200)["id"].(string):        "cms_image",
		f.uploadFile(t, organizer, "", "eski.png", pngPicture(t))["id"].(string): "legacy",
	}
	for id, purpose := range refused {
		resp := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(id))
		requireCode(t, resp, fiber.StatusUnprocessableEntity, "media_purpose_mismatch")
		if resp.body["mediaId"] != id || resp.body["role"] != "event_video_poster" || resp.body["purpose"] != purpose {
			t.Fatalf("a %s as a poster: %v", purpose, resp.body)
		}
	}
	archived := f.uploadImage(t, organizer, "event_cover", 200, 200)
	if gone := sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/media/"+archived["id"].(string), ""); gone.status != fiber.StatusNoContent {
		t.Fatalf("archive: status %d body %v", gone.status, gone.body)
	}
	for _, id := range []string{archived["id"].(string), uuid.NewString()} {
		resp := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(id))
		requireCode(t, resp, fiber.StatusUnprocessableEntity, "media_not_linkable")
		if resp.body["mediaId"] != id || resp.body["role"] != "event_video_poster" {
			t.Fatalf("problem %v", resp.body)
		}
	}
	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); !reflect.DeepEqual(got, asPoster(kept)) {
		t.Fatalf("after the refusals the poster is %v, want %v", got, asPoster(kept))
	}

	svg := f.uploadFile(t, organizer, "event_cover", "logo.svg",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="30"><rect width="40" height="30" fill="#123456"/></svg>`))
	set := sendJSON(t, f.app, organizer, fiber.MethodPut, path, posterBody(svg["id"].(string)))
	svgURL, _ := svg["url"].(string)
	want := map[string]any{"id": svg["id"], "url": svgURL, "sizes": map[string]any{
		"card": map[string]any{"url": svgURL}, "page": map[string]any{"url": svgURL},
	}}
	if got := videoPoster(t, set, videoID); set.status != fiber.StatusOK || !strings.HasSuffix(svgURL, ".svg") || !reflect.DeepEqual(got, want) {
		t.Fatalf("an SVG poster: status %d poster %v, want %v", set.status, got, want)
	}
}
