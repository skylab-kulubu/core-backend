package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

// frameService is a fake frame service: it reads the video by the
// presigned address core hands it, as the real one does, and answers a
// 1280x720 JPEG.
func frameService(t *testing.T) *mediaframe.Client {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	for i := range img.Pix {
		img.Pix[i] = 90
	}
	var frame bytes.Buffer
	if err := jpeg.Encode(&frame, img, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mediaframe.Request
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		resp, err := http.Get(req.URL)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frame.Bytes())
	}))
	t.Cleanup(server.Close)
	client, err := mediaframe.NewClient(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// takeFrames settles the videos' faststart as the faststart worker would
// (their moov first already) and makes a pass of the frame worker.
func (f *eventFilesEnv) takeFrames(t *testing.T, frames *mediaframe.Client) media.FrameReport {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE media SET video_faststart = 'not_needed' WHERE purpose = 'video' AND video_faststart IS NULL`); err != nil {
		t.Fatal(err)
	}
	w, err := media.NewFrameWorker(media.FrameWorkerConfig{Store: f.store, Storage: f.r2, Frames: frames, Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.Pass(ctx, func(err error) { t.Errorf("frame: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// mediaJSON is a Media's JSON as a privileged person (YK) reads it.
func (f *eventFilesEnv) mediaJSON(t *testing.T, id string) map[string]any {
	t.Helper()
	got := sendJSON(t, f.app, organizerToken(t, f.keys), fiber.MethodGet, "/v1/media/"+id, "")
	if got.status != fiber.StatusOK {
		t.Fatalf("GET media %s: status %d body %v", id, got.status, got.body)
	}
	return got.body
}

// A video its organizers gave no poster shows the frame core took of it,
// to everyone, in the poster's shape with source "frame": a re-encoded JPEG
// at its address with its card and page sizes, those the frame's own Media
// JSON gives. An uploaded poster wins (source "uploaded"); the frame stays
// on the video, attached, and clearing the uploaded poster falls back to
// it. Removing the video detaches the frame.
func TestAVideoWithoutAnUploadedPosterShowsItsFrameHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	organizer := organizerToken(t, f.keys)
	eventID, videoID := f.eventWithVideo(t, organizer, "WEBLAB")
	if got := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID); got != nil {
		t.Fatalf("a video before its frame answered the poster %v", got)
	}

	if report := f.takeFrames(t, frameService(t)); report.Made != 1 {
		t.Fatalf("frame pass %+v", report)
	}
	shown, ok := videoPoster(t, anonymousGet(t, f.app, "/v1/events/"+eventID), videoID).(map[string]any)
	if !ok || shown["source"] != "frame" {
		t.Fatalf("the frame answered %v", shown)
	}
	frameID := shown["id"].(string)
	frame := f.mediaJSON(t, frameID)
	want := map[string]any{"id": frameID, "type": "image/jpeg", "url": frame["url"], "sizes": frame["sizes"], "source": "frame"}
	if !reflect.DeepEqual(shown, want) || frame["purpose"] != "video_frame" || frame["status"] != "attached" {
		t.Fatalf("the frame answered %v, want %v (Media %v)", shown, want, frame)
	}
	if card, _ := frame["sizes"].(map[string]any)["card"].(map[string]any); card["width"] != float64(400) || card["height"] != float64(225) {
		t.Fatalf("the frame's card size %v", card)
	}
	if frame["uploadedBy"] != uuid.Nil.String() || frame["name"] != "" {
		t.Fatalf("the frame has an uploader or a name: %v", frame)
	}

	poster := f.uploadImage(t, organizer, "event_cover", 1600, 1200)
	set := sendJSON(t, f.app, organizer, fiber.MethodPut, posterPath(eventID, videoID), posterBody(poster["id"].(string)))
	if set.status != fiber.StatusOK || !reflect.DeepEqual(videoPoster(t, set, videoID), asPoster(poster)) {
		t.Fatalf("set poster: status %d poster %v", set.status, videoPoster(t, set, videoID))
	}
	if f.mediaJSON(t, frameID)["status"] != "attached" {
		t.Fatal("the frame was detached under an uploaded poster")
	}
	cleared := sendJSON(t, f.app, organizer, fiber.MethodDelete, posterPath(eventID, videoID), "")
	if cleared.status != fiber.StatusOK || !reflect.DeepEqual(videoPoster(t, cleared, videoID), want) {
		t.Fatalf("after clearing: status %d poster %v, want the frame %v", cleared.status, videoPoster(t, cleared, videoID), want)
	}

	removed := sendJSON(t, f.app, organizer, fiber.MethodDelete, "/v1/events/"+eventID+"/videos", jsonIDs(videoID))
	if removed.status != fiber.StatusOK {
		t.Fatalf("remove video: status %d body %v", removed.status, removed.body)
	}
	if status := f.mediaJSON(t, frameID)["status"]; status != "detached" {
		t.Fatalf("the removed video's frame is %v, want detached", status)
	}
}

// No person may upload a video frame, however privileged: it is core's own
// image.
func TestNoOneUploadsAVideoFrameHTTP(t *testing.T) {
	f := newEventFilesEnv(t)
	var file bytes.Buffer
	if err := jpeg.Encode(&file, image.NewGray(image.Rect(0, 0, 32, 18)), nil); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("purpose", "video_frame")
	part, _ := form.CreateFormFile("file", "frame.jpg")
	part.Write(file.Bytes())
	form.Close()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+organizerToken(t, f.keys))
	resp, err := f.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var problem map[string]any
	if resp.StatusCode != fiber.StatusForbidden || json.Unmarshal(raw, &problem) != nil || problem["code"] != "purpose_forbidden" {
		t.Fatalf("a privileged person uploading a video frame: status %d body %s", resp.StatusCode, raw)
	}
}
