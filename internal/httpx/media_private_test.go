package httpx_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// A reviewer's browser opens the read link Skyforms got for them: no core
// token, and the open is logged with the address the edge proxy reports.
func TestPrivateMediaReadLinkOpensWithoutSignInHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Media = media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: deps.ServiceClients.Products(),
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{5}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: store, Now: bao.Clock.Now,
			},
		})
	app := httpx.New(deps)
	respondent := newEditor(t, keys)
	id := uploadAs(t, app, respondent.token, "answer_file")

	reviewer := uuid.NewString()
	person := sendJSON(t, app, respondent.token, fiber.MethodPost, "/v1/media/"+id+"/links", `{"onBehalfOf":"`+reviewer+`"}`)
	if person.status != fiber.StatusForbidden {
		t.Fatalf("a person asked for a link: status %d body %v", person.status, person.body)
	}
	link := sendJSON(t, app, serviceToken(t, keys, "forms", "media:attach"), fiber.MethodPost, "/v1/media/"+id+"/links", `{"onBehalfOf":"`+reviewer+`"}`)
	if link.status != fiber.StatusCreated {
		t.Fatalf("link: status %d body %v", link.status, link.body)
	}
	parsed, err := url.Parse(link.body["url"].(string))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(fiber.MethodGet, parsed.RequestURI(), nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK || !bytes.Equal(raw, pngPicture(t)) {
		t.Fatalf("content: status %d, %d bytes", resp.StatusCode, len(raw))
	}
	log := store.ReadLinkLog(uuid.MustParse(id))
	if len(log) != 1 || len(log[0].Opens) != 1 || log[0].Opens[0].ClientIP != "198.51.100.20" {
		t.Fatalf("access log %+v", log)
	}
}

// unscannedCatalogue is the reviewed catalogue with answer_file's malware
// scan lifted: no scanner exists until ticket 12, so the reviewed
// answer_file cannot be uploaded at all.
func unscannedCatalogue(t testing.TB) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	file["purposes"].(map[string]any)["answer_file"].(map[string]any)["scan"] = false
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := media.ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}
