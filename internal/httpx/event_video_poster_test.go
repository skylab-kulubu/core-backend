package httpx_test

import (
	"bytes"
	"context"
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
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// grayPNG is a gray PNG of w by h pixels.
func grayPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, w, h))
	for i := range picture.Pix {
		picture.Pix[i] = 128
	}
	var file bytes.Buffer
	if err := png.Encode(&file, picture); err != nil {
		t.Fatal(err)
	}
	return file.Bytes()
}

// uploadImage uploads a gray PNG of w by h pixels for the purpose, as the
// organizer's picker does, and answers its Media JSON: its id, its
// full-size address and its sizes.
func (f *eventFilesEnv) uploadImage(t *testing.T, token, purpose string, w, h int) map[string]any {
	t.Helper()
	return f.uploadFile(t, token, purpose, "kapak.png", grayPNG(t, w, h))
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
	// No deadline: re-encoding a large image and making its sizes takes
	// longer than app.Test's default second under -race and a busy suite.
	resp, err := f.app.Test(req, fiber.TestConfig{Timeout: 0})
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
// id, its type, its full-size address and its sizes, as the Media JSON
// gives them.
func asPoster(uploaded map[string]any) map[string]any {
	return map[string]any{"id": uploaded["id"], "type": uploaded["type"], "url": uploaded["url"], "sizes": uploaded["sizes"]}
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
	if card, _ := poster["sizes"].(map[string]any)["card"].(map[string]any); card["width"] != float64(400) || poster["type"] != "image/png" {
		t.Fatalf("the poster was stored without a card size, or not as a PNG: %v", poster)
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
	// Its type tells a client that cannot draw an SVG (Flutter's
	// Image.network) to use an SVG renderer or skip it; its sizes carry no
	// width or height.
	want := map[string]any{"id": svg["id"], "type": "image/svg+xml", "url": svgURL, "sizes": map[string]any{
		"card": map[string]any{"url": svgURL}, "page": map[string]any{"url": svgURL},
	}}
	if got := videoPoster(t, set, videoID); set.status != fiber.StatusOK || !strings.HasSuffix(svgURL, ".svg") || !reflect.DeepEqual(got, want) {
		t.Fatalf("an SVG poster: status %d poster %v, want %v", set.status, got, want)
	}
}

// Whoever may edit the Event may set, replace and clear a video's poster,
// by the same decision as editing it: the Owner team's leader may; a member
// of the team (whose Event permissions do not let members edit), another
// team's leader and nobody without a sign-in may not. A video the Event
// does not list (a file, a video not added, one whose Media was archived)
// is not found, and neither is an Event that does not exist. A body that
// names no poster is a bad request.
func TestOnlyWhoMayEditTheEventSetsAVideosPosterHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	poster := f.uploadImage(t, organizer, "event_cover", 800, 600)
	path, body := posterPath(eventID, videoID), posterBody(poster["id"].(string))

	for _, method := range []string{fiber.MethodPut, fiber.MethodDelete} {
		if anonymous := sendJSON(t, f.app, "", method, path, body); anonymous.status != fiber.StatusUnauthorized {
			t.Fatalf("anonymous %s: status %d body %v", method, anonymous.status, anonymous.body)
		}
		for who, token := range map[string]string{
			"a member of the Owner team": teamToken(t, f, "/UYELER/ARGE/WEBLAB"),
			"another team's leader":      teamToken(t, f, "/UYELER/ARGE/GAMELAB/LIDERLER"),
		} {
			if refused := sendJSON(t, f.app, token, method, path, body); refused.status != fiber.StatusForbidden {
				t.Fatalf("%s %s: status %d body %v", who, method, refused.status, refused.body)
			}
		}
	}
	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); got != nil {
		t.Fatalf("refused requests set the poster %v", got)
	}
	leader := teamToken(t, f, "/UYELER/ARGE/WEBLAB/LIDERLER")
	if set := sendJSON(t, f.app, leader, fiber.MethodPut, path, body); set.status != fiber.StatusOK || !reflect.DeepEqual(videoPoster(t, set, videoID), asPoster(poster)) {
		t.Fatalf("the Owner team's leader: status %d body %v", set.status, set.body)
	}

	fileID, _ := f.uploaded(t, organizer, "club_file", "sunum.pdf", pdfFile(1000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/files", jsonIDs(fileID))
	notAdded, _ := f.uploaded(t, organizer, "video", "eklenmedi.mp4", mp4File(1000))
	archived, _ := f.uploaded(t, organizer, "video", "arşiv.mp4", mp4File(1000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(archived))
	if gone := sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/media/"+archived, ""); gone.status != fiber.StatusNoContent {
		t.Fatalf("archive the video: status %d body %v", gone.status, gone.body)
	}
	for name, missing := range map[string]string{
		"a file":                     posterPath(eventID, fileID),
		"a video not added":          posterPath(eventID, notAdded),
		"an archived video":          posterPath(eventID, archived),
		"an Event that is not there": posterPath(uuid.NewString(), videoID),
	} {
		for _, method := range []string{fiber.MethodPut, fiber.MethodDelete} {
			if resp := sendJSON(t, f.app, organizer, method, missing, body); resp.status != fiber.StatusNotFound {
				t.Fatalf("%s, %s: status %d body %v", name, method, resp.status, resp.body)
			}
		}
	}
	for _, bad := range []string{`{}`, `{"posterId":"kapak"}`, `{"posterId":"00000000-0000-0000-0000-000000000000"}`, jsonIDs(poster["id"].(string))} {
		if resp := sendJSON(t, f.app, organizer, fiber.MethodPut, path, bad); resp.status != fiber.StatusBadRequest {
			t.Fatalf("body %s: status %d body %v", bad, resp.status, resp.body)
		}
	}
	if resp := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, "video"), body); resp.status != fiber.StatusBadRequest {
		t.Fatalf("a video id that is not one: status %d", resp.status)
	}
	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); !reflect.DeepEqual(got, asPoster(poster)) {
		t.Fatalf("after the refusals the poster is %v", got)
	}
}

// The Team media library holds for posters as for photos: a video may show
// a photo another Event of its own Owner team uses, not one another team's
// Event uses, as its cover or a video's poster (media_team_mismatch); and
// another team's Event cannot take a poster as its cover. Moving an Event
// to another Owner team is refused while an Event of the old team shows
// one of its posters; clearing the poster first lets it move.
func TestAVideosPosterFollowsTheTeamMediaLibraryHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	first, firstVideo := f.eventWithVideo(t, organizer, "WEBLAB")
	second, secondVideo := f.eventWithVideo(t, organizer, "WEBLAB")
	other, otherVideo := f.eventWithVideo(t, organizer, "GAMELAB")
	poster := f.uploadImage(t, organizer, "event_cover", 800, 600)
	for eventID, videoID := range map[string]string{first: firstVideo, second: secondVideo} {
		if set := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, videoID), posterBody(poster["id"].(string))); set.status != fiber.StatusOK {
			t.Fatalf("a poster within the Owner team: status %d body %v", set.status, set.body)
		}
	}

	refused := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(other, otherVideo), posterBody(poster["id"].(string)))
	requireCode(t, refused, fiber.StatusForbidden, "media_team_mismatch")
	if refused.body["mediaId"] != poster["id"] || refused.body["role"] != "event_video_poster" {
		t.Fatalf("problem %v", refused.body)
	}
	covered := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+other,
		`{"name":"Hack","location":"YTÜ","ownerTeam":"GAMELAB","active":true,"coverImageId":"`+poster["id"].(string)+`"}`)
	requireCode(t, covered, fiber.StatusForbidden, "media_team_mismatch")
	if covered.body["role"] != "event_cover" {
		t.Fatalf("problem %v", covered.body)
	}
	otherCover := f.uploadImage(t, organizer, "event_cover", 800, 600)
	if set := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+other,
		`{"name":"Hack","location":"YTÜ","ownerTeam":"GAMELAB","active":true,"coverImageId":"`+otherCover["id"].(string)+`"}`); set.status != fiber.StatusOK {
		t.Fatalf("cover: status %d body %v", set.status, set.body)
	}
	requireCode(t, sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(first, firstVideo), posterBody(otherCover["id"].(string))),
		fiber.StatusForbidden, "media_team_mismatch")

	move := `{"name":"Hack","location":"YTÜ","ownerTeam":"GAMELAB","active":true}`
	moved := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+second, move)
	requireCode(t, moved, fiber.StatusForbidden, "media_team_mismatch")
	if moved.body["mediaId"] != poster["id"] || moved.body["role"] != "event_video_poster" {
		t.Fatalf("problem %v", moved.body)
	}
	sendJSON(t, f.app, organizer, fiber.MethodDelete, posterPath(second, secondVideo), "")
	if moved := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+second, move); moved.status != fiber.StatusOK {
		t.Fatalf("move once the poster is cleared: status %d body %v", moved.status, moved.body)
	}
}

// A video answers its poster only while the image can be served (the rule
// of media.ServableSQL): not once the image is archived (restored, it is
// back), nor while its object is being purged; the video itself is answered
// as before. Who sees the poster follows the video: a video nobody but its
// editors may see (a private one, which only a writer bypassing core could
// list) shows its poster to its editors alone.
func TestAVideoAnswersItsPosterOnlyWhileTheImageCanBeServedHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	person, organizer := newOrganizer(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	poster := f.uploadImage(t, organizer, "event_cover", 800, 600)
	sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, videoID), posterBody(poster["id"].(string)))
	posterID := poster["id"].(string)
	withoutPoster := func(when string) {
		t.Helper()
		for who, seen := range map[string]jsonResponse{
			"the organizer": sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/events/"+eventID, ""),
			"anyone":        anonymousGet(t, f.app, "/v1/events/"+eventID),
		} {
			video := items(t, seen, "videos")[0]
			if _, carried := video["poster"]; carried || video["url"] == nil {
				t.Fatalf("%s, %s sees the video as %v, want it without its poster", when, who, video)
			}
		}
	}

	if gone := sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/media/"+posterID, ""); gone.status != fiber.StatusNoContent {
		t.Fatalf("archive: status %d body %v", gone.status, gone.body)
	}
	withoutPoster("archived")
	if back := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/media/"+posterID+"/restore", ""); back.status != fiber.StatusOK {
		t.Fatalf("restore: status %d body %v", back.status, back.body)
	}
	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); !reflect.DeepEqual(got, asPoster(poster)) {
		t.Fatalf("restored, the poster is %v", got)
	}
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE media SET blob_purge_started_at = now() WHERE id = $1`, posterID); err != nil {
		t.Fatal(err)
	}
	withoutPoster("being purged")
	if _, err := f.pool.Exec(ctx, `UPDATE media SET blob_purge_started_at = NULL WHERE id = $1`, posterID); err != nil {
		t.Fatal(err)
	}

	private, err := f.store.Create(ctx, media.Media{
		Name: "gizli.mp4", Type: "video/mp4", Kind: media.KindFile, Key: "private/files/" + uuid.NewString(), Size: 10,
		UploadedBy: person, Purpose: media.PurposeVideo, Status: media.StatusAttached, Visibility: media.VisibilityPrivate,
		Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index, poster_media_id) VALUES ($1, $2, 2, $3)`,
		eventID, private.ID, posterID); err != nil {
		t.Fatal(err)
	}
	mine := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/events/"+eventID, "")
	if got := videoPoster(t, mine, private.ID.String()); !reflect.DeepEqual(got, asPoster(poster)) {
		t.Fatalf("the editor sees the private video's poster as %v", got)
	}
	if videos := ids(items(t, anonymousGet(t, f.app, "/v1/events/"+eventID), "videos")); !reflect.DeepEqual(videos, []string{videoID}) {
		t.Fatalf("anyone sees the videos %v, want only the one they can play", videos)
	}
}

// Account erasure keeps a video's poster (media redesign ticket 07: an
// Event photo is a club purpose): an erased organizer's poster stays on the
// video, attached and served at its address with its sizes, with neither
// their name as its uploader nor its file name.
func TestAnErasedOrganizersPosterStaysOnTheVideoNamelessHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	person, organizer := newOrganizer(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	poster := f.uploadFile(t, organizer, "event_cover", "Ada_Organizer_kapak.png", grayPNG(t, 800, 600))
	sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, videoID), posterBody(poster["id"].(string)))
	key := strings.TrimPrefix(poster["url"].(string), eventFilesCDN+"/")
	before, ok := f.s3.Object("media", key)
	if !ok {
		t.Fatalf("the poster is not stored at %s", key)
	}

	ctx := context.Background()
	users := user.NewPostgresStore(f.pool)
	request, err := users.RequestDeletion(ctx, person, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := users.AnonymizeAccount(ctx, person, now, nil); err != nil {
		t.Fatal(err)
	}
	recorded, err := users.MediaForDeletion(ctx, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	eraser := media.NewImmediateBlobEraser(f.store, media.Buckets{Public: f.r2})
	for _, id := range recorded {
		if err := eraser.EnsureErased(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}

	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); !reflect.DeepEqual(got, asPoster(poster)) {
		t.Fatalf("after the erasure the video's poster is %v, want %v", got, asPoster(poster))
	}
	kept, err := f.store.Get(ctx, uuid.MustParse(poster["id"].(string)))
	if err != nil || kept.Name != "" || kept.UploadedBy != uuid.Nil || kept.Status != media.StatusAttached || kept.BlobPurgedAt != nil {
		t.Fatalf("the erased organizer's poster is %+v (err %v)", kept, err)
	}
	after, ok := f.s3.Object("media", key)
	if !ok || after.ContentType != before.ContentType || after.ContentDisposition != before.ContentDisposition {
		t.Fatalf("the kept poster is stored %v as %q, %q; before the erasure %q, %q", ok, after.ContentType, after.ContentDisposition, before.ContentType, before.ContentDisposition)
	}
}
