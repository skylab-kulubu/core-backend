package handlers

import (
	"bytes"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

var guestPDF = []byte("%PDF-1.7\n%%EOF\n")

// POST /v1/media with purpose=answer_file_guest: Skyforms' service account
// uploads it (201, private, no uploader); a person, whatever roles they hold,
// and another product's service account get 403 purpose_forbidden.
func TestGuestAnswerFileUploadIsSkyformsAloneHTTP(t *testing.T) {
	t.Parallel()
	h := newPrivateMediaHTTP(t)

	resp := postMedia(t, h.app(t, formsHTTP), media.PurposeAnswerFileGuest, "cv.pdf", guestPDF)
	if resp.status != fiber.StatusCreated || resp.body["visibility"] != "private" || resp.body["url"] != "" ||
		resp.body["uploadedBy"] != uuid.Nil.String() || resp.body["purpose"] != media.PurposeAnswerFileGuest {
		t.Fatalf("Skyforms: status %d body %v", resp.status, resp.body)
	}

	for name, ident := range map[string]authn.Identity{
		"a person":                   respondentHTTP,
		"an admin with media:attach": {ID: uuid.New(), Groups: []string{"/UYELER/ADMIN"}, Roles: []string{"media:attach"}},
		"the CMS service account":    {ID: uuid.New(), ServiceAccount: true, Product: authz.ProductCMS, Roles: []string{"media:attach"}},
	} {
		resp := postMedia(t, h.app(t, ident), media.PurposeAnswerFileGuest, "cv.pdf", guestPDF)
		requireProblem(t, resp, fiber.StatusForbidden, "purpose_forbidden")
		if resp.body["purpose"] != media.PurposeAnswerFileGuest {
			t.Errorf("%s: problem %v", name, resp.body)
		}
	}
}

// Until this side has private Media and a malware scanner, Skyforms' upload
// is refused with 422 and nothing is stored: merging the purpose changes
// nothing where they are off.
func TestGuestAnswerFileNeedsPrivateMediaAndAScannerHTTP(t *testing.T) {
	t.Parallel()
	off := media.NewMemoryStore()
	app := mediaServiceApp(t, formsHTTP, media.NewServiceWithOptions(off, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{ServiceProducts: []authz.Product{authz.ProductForms}}))
	requireProblem(t, postMedia(t, app, media.PurposeAnswerFileGuest, "cv.pdf", guestPDF), fiber.StatusUnprocessableEntity, "private_media_disabled")

	bao := transittest.NewServer(t)
	noScanner := media.NewMemoryStore()
	app = mediaServiceApp(t, formsHTTP, media.NewServiceWithOptions(noScanner, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{3}, 32), LinkOrigin: "https://api.example.test", AccessLog: noScanner, Now: bao.Clock.Now,
			},
		}))
	requireProblem(t, postMedia(t, app, media.PurposeAnswerFileGuest, "cv.pdf", guestPDF), fiber.StatusUnprocessableEntity, "purpose_not_available")

	for name, store := range map[string]*media.MemoryStore{"private Media off": off, "no scanner": noScanner} {
		if stored, _ := store.List(t.Context()); len(stored) != 0 {
			t.Errorf("%s: %d Media stored", name, len(stored))
		}
	}
}
