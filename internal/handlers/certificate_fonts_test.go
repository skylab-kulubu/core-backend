package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// fontService answers the font routes; the rest of certificate.Service is
// not called.
type fontService struct {
	certificate.Service
	existing uuid.UUID
	input    certificate.FontInput
	team     string
	template *uuid.UUID
}

func (s *fontService) ListFonts(_ context.Context, _ authz.Principal, team string, template *uuid.UUID) ([]certificate.Font, error) {
	s.team = team
	s.template = template
	return []certificate.Font{{ID: s.existing, OwnerTeam: "ARTLAB", Family: "Brandon Grotesque", Weight: 700, Style: "normal", MissingTurkish: "ğı"}}, nil
}

func (s *fontService) CreateFont(_ context.Context, _ authz.Principal, in certificate.FontInput) (certificate.Font, error) {
	s.input = in
	switch in.OwnerTeam {
	case "ARTLAB":
		return certificate.Font{}, &certificate.FontExistsError{FontID: s.existing}
	case "VARIABLE":
		return certificate.Font{}, &certificate.InvalidError{Code: certificate.CodeFontUnsupported}
	case "OTHER":
		return certificate.Font{}, certificate.ErrForbidden
	}
	return certificate.Font{ID: uuid.New(), OwnerTeam: in.OwnerTeam, Family: "Brandon Grotesque", Weight: 400, Style: "normal"}, nil
}

func (s *fontService) FontFile(_ context.Context, _ authz.Principal, id uuid.UUID) (certificate.FontFile, error) {
	if id != s.existing {
		return certificate.FontFile{}, certificate.ErrNotFound
	}
	return certificate.FontFile{Data: []byte("wOF2 font")}, nil
}

func (s *fontService) ArchiveFont(_ context.Context, _ authz.Principal, id uuid.UUID) error {
	if id != s.existing {
		return certificate.ErrNotFound
	}
	return nil
}

// The font routes: a list, 201 with the font, 409 font_exists with the
// font's id, a font code as 400, someone else's font 404; the file as
// font/woff2, not sniffed, kept a day by the caller's browser only.
func TestCertificateFontsHTTP(t *testing.T) {
	t.Parallel()
	svc := &fontService{existing: uuid.New()}
	ch := NewCertificateHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New()})
		return c.Next()
	})
	app.Get("/v1/certificate-fonts", ch.ListFonts)
	app.Post("/v1/certificate-fonts", ch.CreateFont)
	app.Get("/v1/certificate-fonts/:id/file", ch.FontFile)
	app.Delete("/v1/certificate-fonts/:id", ch.ArchiveFont)
	call := func(method, path, body string) (int, fiber.Map, map[string]string, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out fiber.Map
		_ = json.Unmarshal(raw, &out)
		headers := map[string]string{}
		for _, name := range []string{fiber.HeaderContentType, fiber.HeaderCacheControl, fiber.HeaderXContentTypeOptions} {
			headers[name] = resp.Header.Get(name)
		}
		return resp.StatusCode, out, headers, raw
	}

	status, _, _, raw := call(fiber.MethodGet, "/v1/certificate-fonts?team=artlab", "")
	var listed []certificate.Font
	if status != fiber.StatusOK || json.Unmarshal(raw, &listed) != nil || len(listed) != 1 || listed[0].MissingTurkish != "ğı" || svc.team != "artlab" {
		t.Fatalf("list: %d %s", status, raw)
	}
	template := uuid.New()
	if status, _, _, _ := call(fiber.MethodGet, "/v1/certificate-fonts?template="+template.String(), ""); status != fiber.StatusOK || svc.template == nil || *svc.template != template {
		t.Fatalf("list by template: %d %v", status, svc.template)
	}
	if status, _, _, _ := call(fiber.MethodGet, "/v1/certificate-fonts?template=nope", ""); status != fiber.StatusBadRequest {
		t.Fatalf("bad template: %d", status)
	}
	media := uuid.New()
	status, created, _, _ := call(fiber.MethodPost, "/v1/certificate-fonts", `{"mediaId":"`+media.String()+`","ownerTeam":"WEBLAB"}`)
	if status != fiber.StatusCreated || created["ownerTeam"] != "WEBLAB" || svc.input.MediaID != media {
		t.Fatalf("create: %d %v", status, created)
	}
	status, problem, headers, _ := call(fiber.MethodPost, "/v1/certificate-fonts", `{"mediaId":"`+media.String()+`","ownerTeam":"ARTLAB"}`)
	if status != fiber.StatusConflict || problem["code"] != certificate.CodeFontExists || problem["fontId"] != svc.existing.String() ||
		headers[fiber.HeaderContentType] != "application/problem+json" {
		t.Fatalf("exists: %d %v", status, problem)
	}
	if status, problem, _, _ := call(fiber.MethodPost, "/v1/certificate-fonts", `{"mediaId":"`+media.String()+`","ownerTeam":"VARIABLE"}`); status != fiber.StatusBadRequest || problem["code"] != certificate.CodeFontUnsupported {
		t.Fatalf("variable: %d %v", status, problem)
	}
	if status, _, _, _ := call(fiber.MethodPost, "/v1/certificate-fonts", `{"mediaId":"`+media.String()+`","ownerTeam":"OTHER"}`); status != fiber.StatusForbidden {
		t.Fatalf("other team: %d", status)
	}
	status, _, headers, raw = call(fiber.MethodGet, "/v1/certificate-fonts/"+svc.existing.String()+"/file", "")
	if status != fiber.StatusOK || !bytes.Equal(raw, []byte("wOF2 font")) || headers[fiber.HeaderContentType] != "font/woff2" ||
		headers[fiber.HeaderCacheControl] != "private, max-age=86400" || headers[fiber.HeaderXContentTypeOptions] != "nosniff" {
		t.Fatalf("file: %d %v", status, headers)
	}
	if status, _, _, _ := call(fiber.MethodGet, "/v1/certificate-fonts/"+uuid.NewString()+"/file", ""); status != fiber.StatusNotFound {
		t.Fatalf("someone else's file: %d", status)
	}
	if status, _, _, _ := call(fiber.MethodDelete, "/v1/certificate-fonts/"+svc.existing.String(), ""); status != fiber.StatusNoContent {
		t.Fatalf("archive: %d", status)
	}
	if status, _, _, _ := call(fiber.MethodDelete, "/v1/certificate-fonts/not-an-id", ""); status != fiber.StatusBadRequest {
		t.Fatalf("bad id: %d", status)
	}
}
