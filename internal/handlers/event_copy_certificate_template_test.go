package handlers

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// copyRecorder answers Copy as the store does for a source with an Event
// template: copied unless asked to leave it out, then named skipped.
type copyRecorder struct {
	event.Service
	got event.CopyInput
}

func (r *copyRecorder) Copy(_ context.Context, _ authz.Principal, _ uuid.UUID, in event.CopyInput) (event.Copied, bool, error) {
	r.got = in
	skipped := []string{}
	if in.SkipCertificateTemplate {
		skipped = append(skipped, event.SkippedCertificateTemplate)
	}
	return event.Copied{Event: event.Event{ID: in.ID, Name: in.Name}, Skipped: skipped}, true, nil
}

func (r *copyRecorder) ProjectFor(_ *authz.Principal, in event.Event) event.Event { return in }

// includeCertificateTemplate (docs/event-copy.md): left out or true copies
// the certificate template, false leaves it out and the answer names it.
func TestEventCopyIncludeCertificateTemplate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, extra string
		skip        bool
	}{
		{"left out", ``, false},
		{"true", `,"includeCertificateTemplate":true`, false},
		{"false", `,"includeCertificateTemplate":false`, true},
	} {
		rec := &copyRecorder{}
		h := NewEventHandler(rec)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
			return c.Next()
		})
		app.Post("/v1/events/:id/copy", h.Copy)
		body := `{"id":"` + uuid.NewString() + `","name":"Kopya","startDate":"2027-03-14T19:00:00+03:00"` + tc.extra + `}`
		req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+uuid.NewString()+"/copy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		named := strings.Contains(string(raw), `"skipped":["certificateTemplate"]`)
		if resp.StatusCode != fiber.StatusCreated || rec.got.SkipCertificateTemplate != tc.skip || named != tc.skip {
			t.Errorf("%s: %d skip %v named %v %s", tc.name, resp.StatusCode, rec.got.SkipCertificateTemplate, named, raw)
		}
	}
}
