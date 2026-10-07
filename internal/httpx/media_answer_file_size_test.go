package httpx_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// answerFileLimit is the largest Answer file sent through core, signed in
// (answer_file) or as a guest (answer_file_guest): 50 MiB, as Yusuf and
// Fatih agreed for form file uploads on 2026-10-07.
const answerFileLimit = 50 << 20

// Answer files of both kinds take up to 50 MiB through the whole
// single-step path: the server's body limit, the upload budget, the form,
// the purpose's maximum and private storage. A file one byte larger is
// refused with 413 media_too_large, naming the purpose and its maximum.
//
// Not parallel: the server holds a few copies of each 50 MiB body.
func TestAnswerFilesTakeUpTo50MiBHTTP(t *testing.T) {
	keys := testauth.New(t)
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Media = media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: deps.ServiceClients.Products(),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: store, Now: bao.Clock.Now,
			},
			Scans: idleScans{},
		})
	app := httpx.New(deps)

	for _, tc := range []struct {
		purpose string
		token   string
	}{
		{media.PurposeAnswerFile, newEditor(t, keys).token},
		{media.PurposeAnswerFileGuest, serviceToken(t, keys, "forms", "media:attach")},
	} {
		created := postLargeAnswerFile(t, app, tc.token, tc.purpose, answerFileLimit)
		if created.status != fiber.StatusCreated || created.body["purpose"] != tc.purpose ||
			created.body["size"] != float64(answerFileLimit) || created.body["status"] != "scanning" {
			t.Fatalf("%s of 50 MiB: status %d body %v", tc.purpose, created.status, created.body)
		}

		tooLarge := postLargeAnswerFile(t, app, tc.token, tc.purpose, answerFileLimit+1)
		if tooLarge.status != fiber.StatusRequestEntityTooLarge || tooLarge.body["code"] != "media_too_large" ||
			tooLarge.body["purpose"] != tc.purpose || tooLarge.body["maxBytes"] != float64(answerFileLimit) {
			t.Fatalf("%s of 50 MiB and a byte: status %d body %v", tc.purpose, tooLarge.status, tooLarge.body)
		}
	}
	if stored, _ := store.List(t.Context()); len(stored) != 2 {
		t.Fatalf("%d Media stored, want the two 50 MiB Answer files", len(stored))
	}
}

// postLargeAnswerFile uploads a PDF of size bytes for the purpose with the
// token. The body is streamed (answerFileForm): the test never builds the
// file itself.
func postLargeAnswerFile(t *testing.T, app *fiber.App, token, purpose string, size int64) jsonResponse {
	t.Helper()
	body, length, contentType := answerFileForm(t, purpose, size)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
	}
	return jsonResponse{status: resp.StatusCode, body: got}
}

// answerFileForm is a multipart form, as a stream of length bytes, carrying
// the purpose and a PDF of size bytes as its "file": "%PDF-1.7\n" and then
// zeros, which core takes as a PDF by its content.
func answerFileForm(t *testing.T, purpose string, size int64) (io.Reader, int64, string) {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("purpose", purpose); err != nil {
		t.Fatal(err)
	}
	if _, err := form.CreateFormFile("file", "cv.pdf"); err != nil {
		t.Fatal(err)
	}
	head := bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	tail := bytes.Clone(buf.Bytes())
	const magic = "%PDF-1.7\n"
	body := io.MultiReader(bytes.NewReader(head), strings.NewReader(magic),
		io.LimitReader(zeros{}, size-int64(len(magic))), bytes.NewReader(tail))
	return body, int64(len(head)) + size + int64(len(tail)), form.FormDataContentType()
}

// zeros reads zero bytes without end.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
