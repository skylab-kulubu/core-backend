package httpx_test

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/valyala/fasthttp"
)

// fileForm is a multipart form carrying data as the file field, named name.
func fileForm(t *testing.T, field, name string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, form.FormDataContentType()
}

// pdfForm is a form uploading a PDF of size bytes as its "file".
func pdfForm(t *testing.T, size int) (*bytes.Buffer, string) {
	t.Helper()
	pdf := make([]byte, size)
	copy(pdf, "%PDF-1.7\n")
	return fileForm(t, "file", "cv.pdf", pdf)
}

func uploadPDF(t *testing.T, app *fiber.App, keys *testauth.Bundle, size int) (int, error) {
	t.Helper()
	body, contentType := pdfForm(t, size)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "applicant@example.com", "given_name": "A", "family_name": "B",
	}))
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Logf("status %d body %s", resp.StatusCode, msg)
	}
	return resp.StatusCode, nil
}

// Fiber's default body limit is 4 MiB; a form file between that and the media
// service's own limit has to reach the handler instead of dying with a 413.
func TestMediaUploadAcceptsFilesAboveFiberDefaultBodyLimit(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())

	for _, size := range []int{6 << 20, media.MaxUploadBytes} {
		got, err := uploadPDF(t, app, keys, size)
		if err != nil || got != fiber.StatusCreated {
			t.Fatalf("%d byte PDF upload = %d, %v; want %d", size, got, err, fiber.StatusCreated)
		}
	}

	// app.Test surfaces the server's 413 as the read error that caused it.
	got, err := uploadPDF(t, app, keys, media.MaxUploadBytes+2<<20)
	if !errors.Is(err, fasthttp.ErrBodyTooLarge) && got != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, %v; want the body limit to reject it", got, err)
	}
}
