package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const eventFilesCDN = "https://cdn.example.test"

// eventFilesEnv is core with Direct upload (directEnv) that also serves
// Events from the same PostgreSQL: an Event's files and videos are Media
// its organizers sent by Direct upload.
type eventFilesEnv struct {
	*directEnv
	events *event.PostgresStore
}

// newEventFilesEnv serves the catalogue with club_file's malware scan off
// (directCatalogue): these tests run without a scanner.
func newEventFilesEnv(t *testing.T) *eventFilesEnv {
	t.Helper()
	f := &eventFilesEnv{directEnv: newDirectEnv(t, media.DefaultDirectUploadLimits())}
	f.events = event.NewPostgresStore(f.pool)
	f.app = f.appWith(t, directCatalogue(t), nil)
	return f
}

// appWith is f's core with the catalogue and the malware scan (nil: none).
func (f *eventFilesEnv) appWith(t *testing.T, catalogue media.Catalogue, scans media.ScanQueue) *fiber.App {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(f.pool))
	deps.ParseToken = f.keys.Parse()
	deps.MediaUploadLimiter = f.singleStep
	deps.Media = media.NewServiceWithOptions(f.store, f.r2, az, eventFilesCDN, media.ServiceOptions{
		Catalogue:       catalogue,
		ServiceProducts: deps.ServiceClients.Products(),
		Direct:          media.DirectUploadConfig{Storage: f.r2, Limiter: f.limiter, Now: f.clock.Now, Purposes: openDirectPurposes},
		Scans:           scans,
	})
	deps.Events = event.NewServiceWithOptions(f.events, az, event.ServiceOptions{
		PublicBase: eventFilesCDN,
		Media:      media.NewLinker(f.store),
	})
	// The door and tickets read the same Events, as their summary.
	deps.Tickets = ticket.NewService(ticket.NewMemoryStore(), f.events, az)
	return httpx.New(deps)
}

// createEvent creates an Event of the Owner team as the token's holder and
// returns its id.
func (f *eventFilesEnv) createEvent(t *testing.T, token, team string) string {
	t.Helper()
	created := sendJSON(t, f.app, token, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"`+team+`","active":true}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create Event: status %d body %v", created.status, created.body)
	}
	return created.body["id"].(string)
}

// uploaded sends file by Direct upload for the purpose and returns the
// Media's id and address (empty while it waits for its malware scan).
func (f *eventFilesEnv) uploaded(t *testing.T, token, purpose, name string, file []byte) (string, string) {
	t.Helper()
	done := f.upload(t, token, purpose, name, file)
	if done.status != fiber.StatusCreated {
		t.Fatalf("upload %s: status %d body %v", name, done.status, done.body)
	}
	return done.body["id"].(string), done.body["url"].(string)
}

// items reads a list of an Event answer: its files or its videos.
func items(t *testing.T, answer jsonResponse, list string) []map[string]any {
	t.Helper()
	raw, ok := answer.body[list].([]any)
	if !ok {
		t.Fatalf("no %s in %v", list, answer.body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

// getList reads a JSON array answer without a token.
func getList(t *testing.T, app *fiber.App, path string) []map[string]any {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if resp.StatusCode != fiber.StatusOK || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("GET %s: status %d body %s", path, resp.StatusCode, raw)
	}
	return out
}

// mp4File is a file of size bytes that starts like an MP4 (its ftyp box).
func mp4File(size int) []byte {
	file := make([]byte, size)
	copy(file, "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	return file
}

// An organizer adds a club file they sent by Direct upload to their Event.
// The Event's detail lists it with its name, type, size, status and
// address, and core attaches the Media to the Event, so it no longer
// expires.
func TestAnEventListsTheFilesItsOrganizerAddsHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	fileID, fileURL := f.uploaded(t, organizer, "club_file", "etkinlik notları.pdf", pdfFile(5000))

	added := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/files", `["`+fileID+`"]`)
	if added.status != fiber.StatusOK {
		t.Fatalf("add file: status %d body %v", added.status, added.body)
	}
	want := map[string]any{
		"id": fileID, "name": "etkinlik notları.pdf", "type": "application/pdf", "size": float64(5000),
		"status": "attached", "url": fileURL,
	}
	for _, seen := range []jsonResponse{added, anonymousGet(t, f.app, "/v1/events/"+eventID)} {
		files := items(t, seen, "files")
		if len(files) != 1 || !equalItem(files[0], want) {
			t.Fatalf("files %v, want [%v]", files, want)
		}
		if videos := items(t, seen, "videos"); len(videos) != 0 {
			t.Fatalf("videos %v, want none", videos)
		}
	}
	if stored := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/media/"+fileID, ""); stored.body["status"] != "attached" || stored.body["expiresAt"] != nil {
		t.Fatalf("the Event's file is %v", stored.body)
	}
}

// equalItem reports whether a file or video item is exactly want.
func equalItem(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

// Each list takes only its own purpose: a club file is no video and a video
// no file, and neither list takes an Event photo or a Media uploaded without
// a purpose (legacy fits no Event file or video: media_purpose_mismatch).
// One refused Media refuses the whole request.
func TestAnEventsFilesAndVideosTakeOnlyTheirOwnPurposeHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	fileID, _ := f.uploaded(t, organizer, "club_file", "veri seti.zip", zipFile(t, 3000))
	videoID, videoURL := f.uploaded(t, organizer, "video", "açılış konuşması.mp4", mp4File(4000))
	photo := uploadAs(t, f.app, organizer, "event_gallery")
	legacy := uploadAs(t, f.app, organizer, "")

	for _, tc := range []struct {
		list, own, role string
		refused         map[string]string
	}{
		{"files", fileID, "event_file", map[string]string{videoID: "video", photo: "event_gallery", legacy: "legacy"}},
		{"videos", videoID, "event_video", map[string]string{fileID: "club_file", photo: "event_gallery", legacy: "legacy"}},
	} {
		for refused, purpose := range tc.refused {
			resp := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/"+tc.list, `["`+tc.own+`","`+refused+`"]`)
			requireCode(t, resp, fiber.StatusUnprocessableEntity, "media_purpose_mismatch")
			if resp.body["mediaId"] != refused || resp.body["role"] != tc.role || resp.body["purpose"] != purpose {
				t.Fatalf("%s refusing a %s: %v", tc.list, purpose, resp.body)
			}
		}
	}
	detail := anonymousGet(t, f.app, "/v1/events/"+eventID)
	if files, videos := items(t, detail, "files"), items(t, detail, "videos"); len(files) != 0 || len(videos) != 0 {
		t.Fatalf("refused requests added files %v and videos %v", files, videos)
	}

	added := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", `["`+videoID+`"]`)
	if added.status != fiber.StatusOK {
		t.Fatalf("add video: status %d body %v", added.status, added.body)
	}
	want := map[string]any{
		"id": videoID, "name": "açılış konuşması.mp4", "type": "video/mp4", "size": float64(4000),
		"status": "attached", "url": videoURL,
	}
	if videos := items(t, added, "videos"); len(videos) != 1 || !equalItem(videos[0], want) {
		t.Fatalf("videos %v, want [%v]", videos, want)
	}
	if files := items(t, added, "files"); len(files) != 0 {
		t.Fatalf("files %v, want none", files)
	}
}

// teamToken is a token of someone in the groups: a team's leader or member.
func teamToken(t *testing.T, f *eventFilesEnv, groups ...string) string {
	t.Helper()
	return f.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": uuid.NewString() + "@example.com", "given_name": "T", "family_name": "M",
		"groups": groups,
	})
}

// Whoever may edit the Event may add its files and videos, by the same
// decision as editing it: its Owner team's leader may, a member of the team
// (whose Event permissions do not let members edit) and another team's
// leader may not, and nobody without a sign-in.
func TestOnlyWhoMayEditTheEventAddsItsFilesHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	fileID, _ := f.uploaded(t, organizer, "club_file", "sunum.pdf", pdfFile(2000))
	body := `["` + fileID + `"]`

	if anonymous := sendJSON(t, f.app, "", fiber.MethodPost, "/v1/events/"+eventID+"/files", body); anonymous.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: status %d body %v", anonymous.status, anonymous.body)
	}
	for who, token := range map[string]string{
		"a member of the Owner team": teamToken(t, f, "/UYELER/ARGE/WEBLAB"),
		"another team's leader":      teamToken(t, f, "/UYELER/ARGE/GAMELAB/LIDERLER"),
	} {
		for _, list := range []string{"files", "videos"} {
			if refused := sendJSON(t, f.app, token, fiber.MethodPost, "/v1/events/"+eventID+"/"+list, body); refused.status != fiber.StatusForbidden {
				t.Fatalf("%s adding %s: status %d body %v", who, list, refused.status, refused.body)
			}
		}
	}
	if files := items(t, anonymousGet(t, f.app, "/v1/events/"+eventID), "files"); len(files) != 0 {
		t.Fatalf("refused requests added %v", files)
	}

	leader := teamToken(t, f, "/UYELER/ARGE/WEBLAB/LIDERLER")
	added := sendJSON(t, f.app, leader, fiber.MethodPost, "/v1/events/"+eventID+"/files", body)
	if files := items(t, added, "files"); added.status != fiber.StatusOK || len(files) != 1 || files[0]["id"] != fileID {
		t.Fatalf("the Owner team's leader adding a file: status %d body %v", added.status, added.body)
	}
	if missing := sendJSON(t, f.app, leader, fiber.MethodPost, "/v1/events/"+uuid.NewString()+"/files", body); missing.status != fiber.StatusNotFound {
		t.Fatalf("an Event that does not exist: status %d body %v", missing.status, missing.body)
	}
}

// ids are the ids of a list's items, in the answer's order.
func ids(list []map[string]any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item["id"].(string))
	}
	return out
}

// jsonIDs is a JSON array of the ids.
func jsonIDs(ids ...string) string {
	raw, _ := json.Marshal(ids)
	return string(raw)
}

// Organizers order each list and remove items from it; the order is kept.
// An addition goes after what the list holds, in the order given. An order
// names exactly the list's items: one that no longer matches the list (it
// changed meanwhile) is a conflict, a repeated item a bad request. A removed
// file's Media is detached, so its 30 days start.
func TestOrganizersOrderAndRemoveAnEventsFilesHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	path := "/v1/events/" + eventID + "/files"
	a, _ := f.uploaded(t, organizer, "club_file", "a.pdf", pdfFile(1000))
	b, _ := f.uploaded(t, organizer, "club_file", "b.zip", zipFile(t, 1000))
	c, _ := f.uploaded(t, organizer, "club_file", "c.pdf", pdfFile(1000))
	v1, _ := f.uploaded(t, organizer, "video", "1.mp4", mp4File(1000))
	v2, _ := f.uploaded(t, organizer, "video", "2.mp4", mp4File(1000))

	sendJSON(t, f.app, organizer, fiber.MethodPost, path, jsonIDs(a, b))
	added := sendJSON(t, f.app, organizer, fiber.MethodPost, path, jsonIDs(c, a))
	if got := ids(items(t, added, "files")); !slices.Equal(got, []string{a, b, c}) {
		t.Fatalf("after two additions: %v, want %v", got, []string{a, b, c})
	}
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(v1, v2))

	ordered := sendJSON(t, f.app, organizer, fiber.MethodPut, path+"/order", jsonIDs(c, a, b))
	if got := ids(items(t, ordered, "files")); ordered.status != fiber.StatusOK || !slices.Equal(got, []string{c, a, b}) {
		t.Fatalf("order: status %d files %v", ordered.status, got)
	}
	videos := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+eventID+"/videos/order", jsonIDs(v2, v1))
	if got := ids(items(t, videos, "videos")); videos.status != fiber.StatusOK || !slices.Equal(got, []string{v2, v1}) {
		t.Fatalf("order videos: status %d videos %v", videos.status, got)
	}
	for body, status := range map[string]int{
		jsonIDs(c, a):                      fiber.StatusConflict,
		jsonIDs(c, a, b, v1):               fiber.StatusConflict,
		jsonIDs(c, a, b, uuid.NewString()): fiber.StatusConflict,
		jsonIDs(c, a, a, b):                fiber.StatusBadRequest,
		`{"ids":[]}`:                       fiber.StatusBadRequest,
	} {
		if refused := sendJSON(t, f.app, organizer, fiber.MethodPut, path+"/order", body); refused.status != status {
			t.Fatalf("order %s: status %d, want %d", body, refused.status, status)
		}
	}
	detail := anonymousGet(t, f.app, "/v1/events/"+eventID)
	if files, videos := ids(items(t, detail, "files")), ids(items(t, detail, "videos")); !slices.Equal(files, []string{c, a, b}) || !slices.Equal(videos, []string{v2, v1}) {
		t.Fatalf("detail after the orders: files %v, videos %v", files, videos)
	}

	member := teamToken(t, f, "/UYELER/ARGE/WEBLAB")
	if refused := sendJSON(t, f.app, member, fiber.MethodPut, path+"/order", jsonIDs(a, b, c)); refused.status != fiber.StatusForbidden {
		t.Fatalf("a member ordering: status %d", refused.status)
	}
	if refused := sendJSON(t, f.app, member, fiber.MethodDelete, path, jsonIDs(a)); refused.status != fiber.StatusForbidden {
		t.Fatalf("a member removing: status %d", refused.status)
	}

	removed := sendJSON(t, f.app, organizer, fiber.MethodDelete, path, jsonIDs(a))
	if got := ids(items(t, removed, "files")); removed.status != fiber.StatusOK || !slices.Equal(got, []string{c, b}) {
		t.Fatalf("remove: status %d files %v", removed.status, got)
	}
	if stored := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/media/"+a, ""); stored.body["status"] != "detached" || stored.body["expiresAt"] == nil {
		t.Fatalf("the removed file is %v, want detached with its 30 days", stored.body)
	}
	// Removing what the list does not hold removes nothing.
	if missing := sendJSON(t, f.app, organizer, fiber.MethodDelete, path, jsonIDs(b, a)); missing.status != fiber.StatusNotFound {
		t.Fatalf("remove one not in the list: status %d", missing.status)
	}
	if got := ids(items(t, anonymousGet(t, f.app, "/v1/events/"+eventID), "files")); !slices.Equal(got, []string{c, b}) {
		t.Fatalf("after a refused removal: %v", got)
	}
	if stored := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/media/"+b, ""); stored.body["status"] != "attached" {
		t.Fatalf("a file still listed is %v", stored.body)
	}
}

// listed is the Event's entry in the list answer.
func listed(t *testing.T, list []map[string]any, eventID string) map[string]any {
	t.Helper()
	for _, entry := range list {
		if entry["id"] == eventID {
			return entry
		}
	}
	t.Fatalf("Event %s is not in the list %v", eventID, list)
	return nil
}

// A club file can be added while its malware scan runs. The Event's
// organizers see it waiting (scanning, no address); anyone else does not see
// it, and the Event's count leaves it out. Once clean it has its address and
// everyone sees it. A rejected one stays listed for the organizers,
// rejected, with the scan's result and never an address; nobody else sees
// it. A list carries only the count.
func TestAnEventShowsAScannedFileToAnyoneOnlyOnceCleanHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	fake := clamdtest.New(t)
	worker := media.NewScanWorker(media.ScanWorkerConfig{Store: f.store, Scanner: clamd.New(fake.Addr()), Public: f.r2})
	f.app = f.appWith(t, catalogueWith(t, "club_file", func(entry map[string]any) { entry["scan"] = true }), worker)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	clean, _ := f.uploaded(t, organizer, "club_file", "temiz.pdf", pdfFile(3000))
	infected, _ := f.uploaded(t, organizer, "club_file", "virüs.pdf", append(pdfFile(1000), clamd.EICAR()...))
	otherLeader := teamToken(t, f, "/UYELER/ARGE/GAMELAB/LIDERLER")

	added := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/files", jsonIDs(clean, infected))
	waiting := items(t, added, "files")
	if added.status != fiber.StatusOK || len(waiting) != 2 {
		t.Fatalf("add scanning files: status %d body %v", added.status, added.body)
	}
	for _, item := range waiting {
		if item["status"] != "scanning" || item["url"] != nil || item["scanResult"] != nil {
			t.Fatalf("a file waiting for its scan is answered as %v", item)
		}
	}
	for who, seen := range map[string]jsonResponse{
		"anyone":                anonymousGet(t, f.app, "/v1/events/"+eventID),
		"another team's leader": sendJSON(t, f.app, otherLeader, fiber.MethodGet, "/v1/events/"+eventID, ""),
	} {
		if files := items(t, seen, "files"); len(files) != 0 || seen.body["fileCount"] != float64(0) {
			t.Fatalf("%s sees files %v (count %v) while they are scanned", who, files, seen.body["fileCount"])
		}
	}
	if entry := listed(t, getList(t, f.app, "/v1/events"), eventID); entry["fileCount"] != float64(0) {
		t.Fatalf("the list counts %v files while they are scanned", entry["fileCount"])
	}

	if report, err := worker.Pass(context.Background(), func(err error) { t.Error(err) }); err != nil || report.Clean != 1 || report.Rejected != 1 {
		t.Fatalf("scan %+v, err %v", report, err)
	}

	mine := items(t, sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/events/"+eventID, ""), "files")
	wantClean := map[string]any{
		"id": clean, "name": "temiz.pdf", "type": "application/pdf", "size": float64(3000),
		"status": "attached", "url": eventFilesCDN + "/files/" + clean,
	}
	wantRejected := map[string]any{
		"id": infected, "name": "virüs.pdf", "type": "application/pdf", "size": float64(1000 + len(clamd.EICAR())),
		"status": "rejected", "scanResult": "infected",
	}
	if len(mine) != 2 || !equalItem(mine[0], wantClean) || !equalItem(mine[1], wantRejected) {
		t.Fatalf("the organizer sees %v, want [%v %v]", mine, wantClean, wantRejected)
	}
	for who, seen := range map[string]jsonResponse{
		"anyone":                anonymousGet(t, f.app, "/v1/events/"+eventID),
		"another team's leader": sendJSON(t, f.app, otherLeader, fiber.MethodGet, "/v1/events/"+eventID, ""),
	} {
		if files := items(t, seen, "files"); len(files) != 1 || !equalItem(files[0], wantClean) || seen.body["fileCount"] != float64(1) {
			t.Fatalf("%s sees %v (count %v), want only the clean file", who, files, seen.body["fileCount"])
		}
	}
	entry := listed(t, getList(t, f.app, "/v1/events"), eventID)
	if _, carried := entry["files"]; carried || entry["fileCount"] != float64(1) || entry["videoCount"] != float64(0) {
		t.Fatalf("the list answers the Event as %v, want the counts alone", entry)
	}
}

// The Team media library holds for files and videos as for photos: an Event
// may list a file another Event of its own Owner team lists, not one another
// team's Event lists (media_team_mismatch). Moving an Event to another Owner
// team is refused while an Event of the old team lists one of its files; the
// organizer removes it from the Event first.
func TestAnEventsFilesFollowTheTeamMediaLibraryHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	first := f.createEvent(t, organizer, "WEBLAB")
	second := f.createEvent(t, organizer, "WEBLAB")
	other := f.createEvent(t, organizer, "GAMELAB")
	file, _ := f.uploaded(t, organizer, "club_file", "kitapçık.pdf", pdfFile(1000))
	video, _ := f.uploaded(t, organizer, "video", "kayıt.mp4", mp4File(1000))
	for list, id := range map[string]string{"files": file, "videos": video} {
		if added := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+first+"/"+list, jsonIDs(id)); added.status != fiber.StatusOK {
			t.Fatalf("add %s: status %d body %v", list, added.status, added.body)
		}
	}
	if reused := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+second+"/files", jsonIDs(file)); reused.status != fiber.StatusOK {
		t.Fatalf("reuse within the Owner team: status %d body %v", reused.status, reused.body)
	}

	for list, tc := range map[string]struct{ id, role string }{"files": {file, "event_file"}, "videos": {video, "event_video"}} {
		refused := sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+other+"/"+list, jsonIDs(tc.id))
		requireCode(t, refused, fiber.StatusForbidden, "media_team_mismatch")
		if refused.body["mediaId"] != tc.id || refused.body["role"] != tc.role {
			t.Fatalf("problem %v", refused.body)
		}
	}
	moved := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+second, `{"name":"Hack","location":"YTÜ","ownerTeam":"GAMELAB","active":true}`)
	requireCode(t, moved, fiber.StatusForbidden, "media_team_mismatch")
	if moved.body["mediaId"] != file || moved.body["role"] != "event_file" {
		t.Fatalf("problem %v", moved.body)
	}
	sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/events/"+second+"/files", jsonIDs(file))
	if moved := sendJSON(t, f.app, organizer, fiber.MethodPut, "/v1/events/"+second, `{"name":"Hack","location":"YTÜ","ownerTeam":"GAMELAB","active":true}`); moved.status != fiber.StatusOK {
		t.Fatalf("move once the file is removed: status %d body %v", moved.status, moved.body)
	}
}

// A video is stored to play: at a key that ends in .mp4, as video/mp4 and
// inline (no Content-Disposition), so a <video> element and the browser's
// player take it from its address.
func TestAVideoIsStoredToPlayInlineHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	_, videoURL := f.uploaded(t, organizer, "video", "açılış konuşması.mp4", mp4File(3000))
	key := strings.TrimPrefix(videoURL, eventFilesCDN+"/")
	if !regexp.MustCompile(`^videos/[0-9a-f-]{36}\.mp4$`).MatchString(key) {
		t.Fatalf("the video is stored at %q, want videos/<uuid>.mp4", key)
	}
	stored, ok := f.s3.Object("media", key)
	if !ok || stored.ContentType != "video/mp4" || stored.ContentDisposition != "" {
		t.Fatalf("the video is stored %v as %q, %q; want video/mp4 inline", ok, stored.ContentType, stored.ContentDisposition)
	}
	// A club file is still an opaque download under its name.
	_, zipURL := f.uploaded(t, organizer, "club_file", "veri.zip", zipFile(t, 1000))
	if zipped, _ := f.s3.Object("media", strings.TrimPrefix(zipURL, eventFilesCDN+"/")); zipped.ContentType != "application/octet-stream" || !strings.Contains(zipped.ContentDisposition, "veri.zip") {
		t.Fatalf("the club file is stored as %q, %q", zipped.ContentType, zipped.ContentDisposition)
	}
}

// Account erasure keeps an Event's files (media redesign ticket 07: a club
// purpose): an erased organizer's file stays on the Event, attached and
// served at its address, with neither their name as its uploader nor its
// file name, and it no longer downloads under that name.
func TestAnErasedOrganizersEventFileStaysAttachedAndNamelessHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	person, organizer := newOrganizer(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	fileID, fileURL := f.uploaded(t, organizer, "club_file", "Ada_Organizer_notlar.zip", zipFile(t, 2000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/files", jsonIDs(fileID))
	videoID, videoURL := f.uploaded(t, organizer, "video", "Ada_Organizer_konuşma.mp4", mp4File(2000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(videoID))
	key := strings.TrimPrefix(fileURL, eventFilesCDN+"/")
	if stored, _ := f.s3.Object("media", key); !strings.Contains(stored.ContentDisposition, "Ada_Organizer") {
		t.Fatalf("the ZIP downloads as %q before the erasure, want its name", stored.ContentDisposition)
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

	files := items(t, anonymousGet(t, f.app, "/v1/events/"+eventID), "files")
	want := map[string]any{"id": fileID, "name": "", "type": "application/zip", "size": float64(len(zipFile(t, 2000))), "status": "attached", "url": fileURL}
	if len(files) != 1 || !equalItem(files[0], want) {
		t.Fatalf("after the erasure the Event lists %v, want [%v]", files, want)
	}
	kept, err := f.store.Get(ctx, uuid.MustParse(fileID))
	if err != nil || kept.Name != "" || kept.UploadedBy != uuid.Nil || kept.Status != media.StatusAttached || kept.BlobPurgedAt != nil {
		t.Fatalf("the erased organizer's file is %+v (err %v)", kept, err)
	}
	stored, ok := f.s3.Object("media", key)
	if !ok || stored.ContentDisposition != "attachment" || stored.ContentType != "application/octet-stream" {
		t.Fatalf("the kept ZIP is stored %v as %q, %q; want a download without a name", ok, stored.ContentType, stored.ContentDisposition)
	}
	// The video still plays: it named nobody, and its erasure does not make
	// it a download.
	videos := items(t, anonymousGet(t, f.app, "/v1/events/"+eventID), "videos")
	if len(videos) != 1 || videos[0]["name"] != "" || videos[0]["url"] != videoURL || videos[0]["status"] != "attached" {
		t.Fatalf("after the erasure the Event's videos are %v", videos)
	}
	if played, ok := f.s3.Object("media", strings.TrimPrefix(videoURL, eventFilesCDN+"/")); !ok || played.ContentType != "video/mp4" || played.ContentDisposition != "" {
		t.Fatalf("the kept video is stored %v as %q, %q; want video/mp4 inline", ok, played.ContentType, played.ContentDisposition)
	}
}

// An Event's summary (the door's Events, a ticket's or a competitor's
// Event) counts its files and videos anyone can download, and lists none.
func TestAnEventsSummaryCountsItsFilesAndVideosHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	first, _ := f.uploaded(t, organizer, "club_file", "a.pdf", pdfFile(1000))
	second, _ := f.uploaded(t, organizer, "club_file", "b.pdf", pdfFile(1000))
	video, _ := f.uploaded(t, organizer, "video", "c.mp4", mp4File(1000))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/files", jsonIDs(first, second))
	sendJSON(t, f.app, organizer, fiber.MethodPost, "/v1/events/"+eventID+"/videos", jsonIDs(video))

	req := httptest.NewRequest(fiber.MethodGet, "/v1/door/events", nil)
	req.Header.Set("Authorization", "Bearer "+organizer)
	resp, err := f.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var summaries []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&summaries); err != nil || resp.StatusCode != fiber.StatusOK {
		t.Fatalf("door Events: status %d, err %v", resp.StatusCode, err)
	}
	summary := listed(t, summaries, eventID)
	_, files := summary["files"]
	_, videos := summary["videos"]
	if files || videos || summary["fileCount"] != float64(2) || summary["videoCount"] != float64(1) {
		t.Fatalf("the Event's summary is %v, want the counts alone", summary)
	}
}

// A private Media never gets an address, whatever links it: one forced
// into an Event's files (only a writer bypassing core could put it there)
// is shown to the Event's editors without an address, to nobody else, and
// is not counted; the Media JSON has no address for it either.
func TestAPrivateMediaAnEventListsNeverHasAnAddressHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	person, organizer := newOrganizer(t, f.keys)
	eventID := f.createEvent(t, organizer, "WEBLAB")
	ctx := context.Background()
	private, err := f.store.Create(ctx, media.Media{
		Name: "gizli.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "private/files/" + uuid.NewString(), Size: 10,
		UploadedBy: person, Purpose: media.PurposeClubFile, Status: media.StatusAttached, Visibility: media.VisibilityPrivate,
		Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO event_files (event_id, media_id, order_index) VALUES ($1, $2, 1)`, eventID, private.ID); err != nil {
		t.Fatal(err)
	}

	mine := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/events/"+eventID, "")
	if files := items(t, mine, "files"); len(files) != 1 || files[0]["url"] != nil || mine.body["fileCount"] != float64(0) {
		t.Fatalf("the editor sees %v (count %v), want the private file without an address, uncounted", files, mine.body["fileCount"])
	}
	public := anonymousGet(t, f.app, "/v1/events/"+eventID)
	if files := items(t, public, "files"); len(files) != 0 || public.body["fileCount"] != float64(0) {
		t.Fatalf("anyone sees %v (count %v)", files, public.body["fileCount"])
	}
	if entry := listed(t, getList(t, f.app, "/v1/events"), eventID); entry["fileCount"] != float64(0) {
		t.Fatalf("the list counts %v", entry["fileCount"])
	}
	if meta := sendJSON(t, f.app, organizer, fiber.MethodGet, "/v1/media/"+private.ID.String(), ""); meta.status != fiber.StatusOK || meta.body["url"] != "" {
		t.Fatalf("the Media JSON of the private file: status %d body %v", meta.status, meta.body)
	}
}
