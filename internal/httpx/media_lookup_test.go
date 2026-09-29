package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// lookupBase is the public base the lookup tests' core is configured with.
const lookupBase = "https://cdn.example.test"

// lookupEnv is the assembled app over real PostgreSQL, for the address
// lookup (POST /v1/media/lookup).
type lookupEnv struct {
	app  *fiber.App
	pool *pgxpool.Pool
	keys *testauth.Bundle
}

func newLookupEnv(t *testing.T) *lookupEnv {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	e := &lookupEnv{pool: pool, keys: testauth.New(t)}
	e.app = e.appWithLookupLimit(0)
	return e
}

// appWithLookupLimit is a core over the same database whose products may
// look up perMinute times a minute (0: the default).
func (e *lookupEnv) appWithLookupLimit(perMinute int) *fiber.App {
	deps := memoryDeps()
	deps.Users = user.NewService(user.NewPostgresStore(e.pool))
	deps.ParseToken = e.keys.Parse()
	deps.Media = media.NewServiceWithOptions(media.NewPostgresStore(e.pool), media.NewMemoryBlob(),
		authz.NewAuthorizer(authz.DefaultPolicy()), lookupBase,
		media.ServiceOptions{ServiceProducts: deps.ServiceClients.Products()})
	deps.MediaLookupsPerMinute = perMinute
	return httpx.New(deps)
}

// upload uploads a PNG with the person's token, for the purpose ("" for
// none: a legacy Media), and answers the Media JSON.
func (e *lookupEnv) upload(t *testing.T, token, purpose string) map[string]any {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if purpose != "" {
		if err := form.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(pngPicture(t)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var created map[string]any
	if resp.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil {
		t.Fatalf("upload %q: status %d body %s", purpose, resp.StatusCode, raw)
	}
	return created
}

// insert stores a Media record at the key with SQL, for the uploader (a
// new person when uploader is uuid.Nil), and answers its id: the shapes an
// upload through HTTP cannot make here (a video, a faststart copy, core's
// own purposes).
func (e *lookupEnv) insert(t *testing.T, key, purpose string, uploader uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	if uploader == uuid.Nil {
		uploader = uuid.New()
	}
	if _, _, err := user.NewService(user.NewPostgresStore(e.pool)).Ensure(ctx, uploader, user.Profile{Email: uploader.String() + "@example.test"}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, status)
		VALUES ($1, 'a', 'image/png', $2, 10, $3, 'IMAGE', $4, 'pending')`, id, key, uploader, purpose); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// requireNotFound checks that a result names no Media: no id, purpose or
// status, and not linkable.
func requireNotFound(t *testing.T, got map[string]any) {
	t.Helper()
	for _, field := range []string{"mediaId", "purpose", "status"} {
		if value, present := got[field]; !present || value != nil {
			t.Fatalf("result %v; want %s null", got, field)
		}
	}
	if got["linkable"] != false {
		t.Fatalf("result %v; want linkable false", got)
	}
}

// lookUp sends the addresses to POST /v1/media/lookup with the token, for
// the person the product acts for.
func (e *lookupEnv) lookUp(t *testing.T, token, onBehalfOf string, addresses ...string) jsonResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"onBehalfOf": onBehalfOf, "addresses": addresses})
	if err != nil {
		t.Fatal(err)
	}
	return sendJSON(t, e.app, token, fiber.MethodPost, "/v1/media/lookup", string(body))
}

// results are the lookup's results, in order.
func results(t *testing.T, resp jsonResponse) []map[string]any {
	t.Helper()
	if resp.status != fiber.StatusOK {
		t.Fatalf("lookup: status %d body %v", resp.status, resp.body)
	}
	list, ok := resp.body["results"].([]any)
	if !ok {
		t.Fatalf("lookup: no results in %v", resp.body)
	}
	out := make([]map[string]any, len(list))
	for i, r := range list {
		out[i] = r.(map[string]any)
	}
	return out
}

// Products store CDN addresses for old uploads; the lookup turns them into
// Media ids for stage 5 (media redesign ticket 28). One PostgreSQL for
// every slice: the machine runs the whole suite at once.
func TestMediaAddressLookupHTTP(t *testing.T) {
	e := newLookupEnv(t)
	cms := serviceToken(t, e.keys, cmsClient, "media:attach")
	forms := serviceToken(t, e.keys, "forms", "media:attach")

	t.Run("a product's own Media by its address", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		created := e.upload(t, ed.token, "cms_image")
		// Another editor may save a page with it: a CMS image is the CMS's.
		got := results(t, e.lookUp(t, cms, uuid.NewString(), created["url"].(string)))
		if len(got) != 1 {
			t.Fatalf("results %v", got)
		}
		want := map[string]any{"address": created["url"], "mediaId": created["id"], "purpose": "cms_image", "status": "pending", "linkable": true}
		for field, value := range want {
			if got[0][field] != value {
				t.Fatalf("result %v; want %s = %v", got[0], field, value)
			}
		}
	})

	t.Run("only a product's service account that may attach", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		address := e.upload(t, ed.token, "cms_image")["url"].(string)
		for name, token := range map[string]string{
			"person with the role":              personToken(t, e.keys, uuid.NewString(), cmsClient, "media:attach"),
			"service without the role":          serviceToken(t, e.keys, cmsClient, "users:read"),
			"service of an unknown client":      serviceToken(t, e.keys, "frontend-main", "media:attach"),
			"service of an unconfigured client": serviceToken(t, e.keys, "skyforms", "media:attach"),
		} {
			resp := e.lookUp(t, token, ed.id, address)
			if resp.status != fiber.StatusForbidden || resp.body["code"] != "media_attach_forbidden" {
				t.Errorf("%s: status %d body %v", name, resp.status, resp.body)
			}
		}
		// A person is refused before the body is read: what they send does
		// not matter, however malformed or large.
		person := personToken(t, e.keys, uuid.NewString(), cmsClient, "media:attach")
		for _, body := range []string{`addresses=`, `{"addresses":[`, `{"addresses":["` + strings.Repeat("a", 300<<10) + `"]}`} {
			requireCode(t, sendJSON(t, e.app, person, fiber.MethodPost, "/v1/media/lookup", body),
				fiber.StatusForbidden, "media_attach_forbidden")
		}
	})

	t.Run("a body larger than a lookup needs is not read", func(t *testing.T) {
		person := uuid.NewString()
		address := lookupBase + "/images/" + uuid.NewString() + "?q="
		address += strings.Repeat("a", 2048-len(address))
		batch := make([]string, 100)
		for i := range batch {
			batch[i] = address
		}
		// The longest lookup the limits allow fits: 100 addresses of the
		// longest length read.
		if got := results(t, e.lookUp(t, cms, person, batch...)); len(got) != 100 {
			t.Fatalf("%d results", len(got))
		}
		resp := sendJSON(t, e.app, cms, fiber.MethodPost, "/v1/media/lookup",
			`{"onBehalfOf":"`+person+`","addresses":["`+strings.Repeat("a", 256<<10)+`"]}`)
		requireCode(t, resp, fiber.StatusRequestEntityTooLarge, "media_lookup_too_large")
		if resp.body["maxBytes"] != float64(256<<10) {
			t.Fatalf("refusal %v", resp.body)
		}
	})

	t.Run("another product's or core's Media is not found", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		cmsImage := e.upload(t, ed.token, "cms_image")["url"].(string)
		cover := lookupBase + "/images/" + uuid.NewString()
		e.insert(t, strings.TrimPrefix(cover, lookupBase+"/"), "event_cover", uuid.MustParse(ed.id))
		nowhere := lookupBase + "/images/" + uuid.NewString()

		// Not even for the person who uploaded them.
		for _, got := range results(t, e.lookUp(t, forms, ed.id, cmsImage, cover, nowhere)) {
			requireNotFound(t, got)
		}
		got := results(t, e.lookUp(t, cms, ed.id, cover, nowhere))
		requireNotFound(t, got[0])
		requireNotFound(t, got[1])
	})

	t.Run("a legacy Media for its uploader, or once the product holds it", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		someoneElse := uuid.NewString()
		legacy := e.upload(t, ed.token, "")
		// A legacy Media core's uses gave a purpose, held until stage 5,
		// counts as legacy.
		heldAddress := lookupBase + "/images/" + uuid.NewString()
		held := e.insert(t, strings.TrimPrefix(heldAddress, lookupBase+"/"), "event_cover", uuid.MustParse(ed.id))
		if _, err := e.pool.Exec(context.Background(), `UPDATE media SET detach_expiry_held = true WHERE id = $1`, held); err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{cms, forms} {
			got := results(t, e.lookUp(t, token, ed.id, legacy["url"].(string), heldAddress))
			if got[0]["mediaId"] != legacy["id"] || got[0]["purpose"] != "legacy" || got[0]["linkable"] != true {
				t.Fatalf("legacy for its uploader: %v", got[0])
			}
			if got[1]["mediaId"] != held || got[1]["purpose"] != "event_cover" {
				t.Fatalf("held for its uploader: %v", got[1])
			}
			// For anyone else, another person's legacy Media (a still public
			// Answer file, an Event cover) is not found.
			for _, got := range results(t, e.lookUp(t, token, someoneElse, legacy["url"].(string), heldAddress)) {
				requireNotFound(t, got)
			}
		}

		// Once the CMS holds a legacy Media, any of its editors may reuse
		// it; Skyforms still may not.
		resp := sendJSON(t, e.app, cms, fiber.MethodPost, "/v1/media/"+legacy["id"].(string)+"/attachments",
			attachBody("cms", "page", aboutPage, "image", ed.id))
		if resp.status != fiber.StatusCreated {
			t.Fatalf("attach: %d %v", resp.status, resp.body)
		}
		if got := results(t, e.lookUp(t, cms, someoneElse, legacy["url"].(string))); got[0]["mediaId"] != legacy["id"] || got[0]["status"] != "attached" {
			t.Fatalf("held by the CMS: %v", got[0])
		}
		requireNotFound(t, results(t, e.lookUp(t, forms, someoneElse, legacy["url"].(string)))[0])

		// An uploader whose account was erased leaves the Media without one:
		// nobody can be named for it, so only a product that holds it may
		// link it.
		orphan := e.upload(t, ed.token, "")
		if _, err := e.pool.Exec(context.Background(), `UPDATE media SET uploaded_by = NULL WHERE id = ANY($1::uuid[])`,
			[]string{orphan["id"].(string), legacy["id"].(string)}); err != nil {
			t.Fatal(err)
		}
		got := results(t, e.lookUp(t, cms, ed.id, legacy["url"].(string), orphan["url"].(string)))
		if got[0]["mediaId"] != legacy["id"] {
			t.Fatalf("held orphan: %v", got[0])
		}
		requireNotFound(t, got[1])
		for _, got := range results(t, e.lookUp(t, forms, ed.id, legacy["url"].(string), orphan["url"].(string))) {
			requireNotFound(t, got)
		}
	})

	t.Run("every public address core gives a Media", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		uploader := uuid.MustParse(ed.id)
		image := e.upload(t, ed.token, "cms_image")
		address, id := image["url"].(string), image["id"].(string)
		key := strings.TrimPrefix(address, lookupBase+"/")
		name := strings.TrimPrefix(key, "images/")
		svg := "images/" + uuid.NewString() + ".svg"
		file := "files/" + uuid.NewString()
		video := "videos/" + uuid.NewString() + ".mp4"
		// A video moved to its faststart copy: the Media's key is the copy's,
		// and content may still hold the original's address.
		moved := uuid.NewString()
		movedCopy := "videos/" + moved + ".fs." + strings.ReplaceAll(uuid.NewString(), "-", "") + ".mp4"
		private := "private/" + uuid.NewString()
		pending := "pending/scan/" + uuid.NewString()
		ids := map[string]string{}
		for _, k := range []string{svg, file, video, movedCopy, private, pending} {
			ids[k] = e.insert(t, k, "legacy", uploader)
		}

		for want, addresses := range map[string][]string{
			id: {
				address,
				address + "?v=2#top",
				strings.Replace(address, "https://", "http://", 1),
				strings.Replace(address, "cdn.example.test", "CDN.Example.Test", 1),
				// The production CDN, the base of a core that has none
				// configured (the sandbox).
				media.DefaultPublicBase + "/" + key,
				address + "/card.jpg",
				address + "/page.png",
				lookupBase + "/cdn-cgi/image/width=400,height=400,fit=scale-down/" + key,
			},
			ids[svg]:       {lookupBase + "/" + svg},
			ids[file]:      {lookupBase + "/" + file},
			ids[video]:     {lookupBase + "/" + video},
			ids[movedCopy]: {lookupBase + "/videos/" + moved + ".mp4", lookupBase + "/" + movedCopy},
			"": {
				lookupBase + "/" + private,
				lookupBase + "/" + pending,
				address + "/huge.jpg",
				address + "/card.gif",
				address + "/",
				"https://elsewhere.example/" + key,
				// Hosts are compared in ASCII only: a Unicode letter that
				// folds to s or k (U+017F, the Kelvin sign U+212A) is
				// another host.
				"https://cdn.yildiz\u017fkylab.com/" + key,
				"https://cdn.yildizs\u212aylab.com/" + key,
				"https://cdn.example.te\u017ft/" + key,
				"https://user@cdn.example.test/" + key,
				"https://cdn.example.test:8443/" + key,
				key,
				"/" + key,
				lookupBase + "/images/" + strings.ToUpper(name),
				lookupBase + "/cdn-cgi/image/width=400/" + address,
				lookupBase + "/cdn-cgi/image/width=400/" + file,
				// Only a raster original is transformed: never a stored size,
				// never an SVG.
				lookupBase + "/cdn-cgi/image/width=400/" + key + "/card.jpg",
				lookupBase + "/cdn-cgi/image/width=400/" + svg,
				address + "?q=" + strings.Repeat("a", 3000),
				"not an address",
				"",
			},
		} {
			for i, got := range results(t, e.lookUp(t, cms, ed.id, addresses...)) {
				if got["address"] != addresses[i] {
					t.Fatalf("result %d is %v, for %q", i, got, addresses[i])
				}
				if want == "" {
					requireNotFound(t, got)
				} else if got["mediaId"] != want {
					t.Errorf("%q: %v; want %s", addresses[i], got, want)
				}
			}
		}
	})

	t.Run("results in the order sent, an address as often as sent", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		first, second := e.upload(t, ed.token, "cms_image"), e.upload(t, ed.token, "cms_image")
		got := results(t, e.lookUp(t, cms, ed.id, second["url"].(string), "junk", first["url"].(string), second["url"].(string)))
		if len(got) != 4 || got[0]["mediaId"] != second["id"] || got[1]["mediaId"] != nil ||
			got[2]["mediaId"] != first["id"] || got[3]["mediaId"] != second["id"] {
			t.Fatalf("results %v", got)
		}
	})

	t.Run("a Media that cannot take a Media attachment now is not linkable", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		states := map[string]string{
			"archived": `deleted_at = now()`,
			"expired":  `expires_at = now() - interval '1 minute'`,
			"purging":  `blob_purge_started_at = now()`,
		}
		addresses, ids := []string{}, []string{}
		for _, set := range states {
			created := e.upload(t, ed.token, "cms_image")
			if _, err := e.pool.Exec(context.Background(), `UPDATE media SET `+set+` WHERE id = $1`, created["id"]); err != nil {
				t.Fatal(err)
			}
			addresses, ids = append(addresses, created["url"].(string)), append(ids, created["id"].(string))
		}
		for i, got := range results(t, e.lookUp(t, cms, ed.id, addresses...)) {
			if got["mediaId"] != ids[i] || got["purpose"] != "cms_image" || got["linkable"] != false {
				t.Fatalf("result %v", got)
			}
		}
	})

	t.Run("at most 100 addresses a batch, for a named person", func(t *testing.T) {
		person := uuid.NewString()
		batch := make([]string, 101)
		for i := range batch {
			batch[i] = lookupBase + "/images/" + uuid.NewString()
		}
		resp := e.lookUp(t, cms, person, batch...)
		requireCode(t, resp, fiber.StatusBadRequest, "media_lookup_too_many")
		if resp.body["maxAddresses"] != float64(100) {
			t.Fatalf("refusal %v", resp.body)
		}
		if got := results(t, e.lookUp(t, cms, person, batch[:100]...)); len(got) != 100 {
			t.Fatalf("%d results", len(got))
		}
		if got := results(t, sendJSON(t, e.app, cms, fiber.MethodPost, "/v1/media/lookup", `{"onBehalfOf":"`+person+`","addresses":[]}`)); len(got) != 0 {
			t.Fatalf("results %v", got)
		}
		for _, body := range []string{
			`{}`, `{"onBehalfOf":"` + person + `"}`, `{"onBehalfOf":"` + person + `","addresses":"x"}`,
			`{"onBehalfOf":"` + person + `","addresses":[1]}`, `addresses=x`,
			// The person the product acts for is required, as a UUID.
			`{"addresses":[]}`, `{"onBehalfOf":"ed","addresses":[]}`, `{"onBehalfOf":"` + uuid.Nil.String() + `","addresses":[]}`,
		} {
			resp := sendJSON(t, e.app, cms, fiber.MethodPost, "/v1/media/lookup", body)
			if resp.status != fiber.StatusBadRequest {
				t.Errorf("%s: status %d body %v", body, resp.status, resp.body)
			}
		}
	})

	t.Run("rate-limited per product", func(t *testing.T) {
		app := e.appWithLookupLimit(2)
		body := `{"onBehalfOf":"` + uuid.NewString() + `","addresses":["` + lookupBase + `/images/` + uuid.NewString() + `"]}`
		for range 2 {
			results(t, sendJSON(t, app, cms, fiber.MethodPost, "/v1/media/lookup", body))
		}
		req := httptest.NewRequest(fiber.MethodPost, "/v1/media/lookup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cms)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var refusal map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusTooManyRequests || refusal["code"] != "media_lookup_rate_limited" ||
			resp.Header.Get(fiber.HeaderRetryAfter) == "" || refusal["retryAfterSeconds"] == nil || refusal["maxLookups"] != float64(2) {
			t.Fatalf("status %d Retry-After %q body %v", resp.StatusCode, resp.Header.Get(fiber.HeaderRetryAfter), refusal)
		}
		// Each product has its own budget, and a person is still refused
		// for who they are.
		results(t, sendJSON(t, app, forms, fiber.MethodPost, "/v1/media/lookup", body))
		requireCode(t, sendJSON(t, app, personToken(t, e.keys, uuid.NewString(), cmsClient, "media:attach"), fiber.MethodPost, "/v1/media/lookup", body),
			fiber.StatusForbidden, "media_attach_forbidden")
	})

	// Not parallel: it reads the process-wide logger, and this test's
	// parent runs before the package's parallel tests start.
	t.Run("logged by count only", func(t *testing.T) {
		ed := newEditor(t, e.keys)
		created := e.upload(t, ed.token, "cms_image")
		address, id := created["url"].(string), created["id"].(string)
		missing := lookupBase + "/files/" + uuid.NewString()
		var captured bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&captured)
		defer log.SetOutput(previous)

		results(t, e.lookUp(t, cms, ed.id, address, missing, "junk"))
		e.lookUp(t, cms, ed.id, make([]string, 101)...)
		line := captured.String()
		if !strings.Contains(line, "media lookup by cms: 3 addresses, 1 resolved") {
			t.Fatalf("log %q", line)
		}
		for _, secret := range []string{address, strings.TrimPrefix(address, lookupBase+"/"), missing, id, ed.id, "junk"} {
			if strings.Contains(line, secret) {
				t.Fatalf("log %q names %q", line, secret)
			}
		}
	})
}
