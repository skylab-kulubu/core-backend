package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/readlinksubject"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// readLinkAccountsHTTP is the read link route on PostgreSQL, where the
// access log only names an active account, with a directory standing in
// for Keycloak: the memory directory unless a test names another.
type readLinkAccountsHTTP struct {
	privateMediaHTTP
	pool  *pgxpool.Pool
	users *user.PostgresStore
	dir   *identity.Memory
	// file is an Answer file of the respondent's that Skyforms may link.
	file media.Media
}

func newReadLinkAccountsHTTP(t *testing.T) readLinkAccountsHTTP {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	// The respondent signed in to core to upload.
	if _, _, err := user.NewService(users).Ensure(ctx, respondentHTTP.ID, user.Profile{Email: "respondent@example.com", Username: "respondent"}); err != nil {
		t.Fatal(err)
	}
	h := readLinkAccountsHTTP{pool: pool, users: users, dir: identity.NewMemory()}
	h.privateMediaHTTP = h.withDirectory(t, h.dir)
	h.file = h.uploadAnswerFile(t, "cv.pdf")
	return h
}

// withDirectory is the media routes on the same database, asking dir about
// a person core has no row for.
func (h readLinkAccountsHTTP) withDirectory(t *testing.T, dir identity.Directory) privateMediaHTTP {
	t.Helper()
	bao := transittest.NewServer(t)
	store := media.NewPostgresStore(h.pool)
	private := media.NewMemoryBlob()
	svc := media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(private, transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{3}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: store, Now: bao.Clock.Now,
				Subjects: readlinksubject.New(dir, h.users),
			},
		})
	return privateMediaHTTP{svc: svc, private: private, bao: bao, logs: &logRecorder{}}
}

// linkFor asks for a read link to the Media as ident, for person.
func (h readLinkAccountsHTTP) linkFor(t *testing.T, ident authn.Identity, id uuid.UUID, person uuid.UUID) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+id.String()+"/links", strings.NewReader(`{"onBehalfOf":"`+person.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h.app(t, ident), req)
}

// coreRow is the person's row in core, if there is one.
func (h readLinkAccountsHTTP) coreRow(t *testing.T, id uuid.UUID) (user.User, bool) {
	t.Helper()
	u, err := h.users.Get(context.Background(), id)
	if errors.Is(err, user.ErrNotFound) {
		return user.User{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return u, true
}

// links counts the access log rows naming the person.
func (h readLinkAccountsHTTP) links(t *testing.T, person uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM media_read_links WHERE on_behalf_of = $1`, person).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A Skyforms reviewer who never signed in to core, enabled in Keycloak,
// gets a core row from Keycloak and the link.
func TestReadLinkEnsuresTheRowOfAnEnabledReviewerHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	reviewer := uuid.New()
	h.dir.PutUser(identity.Person{ID: reviewer, Email: "rita@example.com", FirstName: "Rita", LastName: "Reviewer", Username: "rita"})

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, reviewer)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	row, ok := h.coreRow(t, reviewer)
	if !ok || row.AccountState != user.AccountActive || row.Email != "rita@example.com" || row.FirstName != "Rita" || row.LastName != "Reviewer" {
		t.Fatalf("core row %+v (found %v)", row, ok)
	}
	if n := h.links(t, reviewer); n != 1 {
		t.Fatalf("%d access log rows", n)
	}
}

// requireRefusedWithoutRow checks a link for the person was refused as naming
// no active account, and left no row and no link behind.
func (h readLinkAccountsHTTP) requireRefusedWithoutRow(t *testing.T, resp *http.Response, raw []byte, person uuid.UUID) {
	t.Helper()
	requireProblemCode(t, resp, raw, fiber.StatusUnprocessableEntity, "media_link_subject_inactive")
	if row, ok := h.coreRow(t, person); ok {
		t.Fatalf("a refused link left a core row %+v", row)
	}
	if n := h.links(t, person); n != 0 {
		t.Fatalf("a refused link left %d access log rows", n)
	}
}

// A person disabled in Keycloak gets no row and no link: an admin disabled
// them, or their erasure is between disable_identity and delete_identity.
func TestReadLinkForAReviewerDisabledInKeycloakCreatesNoRowHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	reviewer := uuid.New()
	h.dir.PutDisabledUser(identity.Person{ID: reviewer, Email: "gone@example.com", FirstName: "Gone", Username: "gone"})

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, reviewer)
	h.requireRefusedWithoutRow(t, resp, raw, reviewer)
}

// A person Keycloak does not know (never there, or deleted, as the erasure's
// delete_identity leaves them) gets no row and no link.
func TestReadLinkForAPersonKeycloakDoesNotKnowCreatesNoRowHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	stranger := uuid.New()

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, stranger)
	h.requireRefusedWithoutRow(t, resp, raw, stranger)
}

// unreachableKeycloak is core's Keycloak client for a server that is gone.
func unreachableKeycloak(t *testing.T) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// While Keycloak cannot be reached core cannot tell whether a person without
// a row may have one: it creates none, hands out no link, and the product
// retries.
func TestReadLinkWhileKeycloakIsDownIsARetryable503HTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	h.privateMediaHTTP = h.withDirectory(t, unreachableKeycloak(t))
	reviewer := uuid.New()

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, reviewer)
	requireProblemCode(t, resp, raw, fiber.StatusServiceUnavailable, "media_link_subject_unavailable")
	if resp.Header.Get("Retry-After") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Retry-After %q, Cache-Control %q", resp.Header.Get("Retry-After"), resp.Header.Get("Cache-Control"))
	}
	if row, ok := h.coreRow(t, reviewer); ok {
		t.Fatalf("core row %+v", row)
	}
	if n := h.links(t, reviewer); n != 0 {
		t.Fatalf("%d access log rows", n)
	}
	if !strings.Contains(h.logs.all(), "identity directory") {
		t.Fatalf("logged %q", h.logs.all())
	}
}

// An account core erased, or is erasing, gets no link and no new row, even
// while Keycloak still reports the person enabled: one being erased keeps
// its row, one erased and hard-purged keeps only its deletion marker.
func TestReadLinkForAnErasedAccountCreatesNoRowHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	ctx := context.Background()
	signedIn := func(email string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, _, err := user.NewService(h.users).Ensure(ctx, id, user.Profile{Email: email, Username: email}); err != nil {
			t.Fatal(err)
		}
		h.dir.PutUser(identity.Person{ID: id, Email: email, Username: email})
		return id
	}
	leaving, erased := signedIn("leaving@example.com"), signedIn("erased@example.com")
	for _, id := range []uuid.UUID{leaving, erased} {
		if _, err := h.users.RequestDeletion(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.users.AnonymizeAccount(ctx, erased, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE account_deletion_requests SET status = 'completed', completed_at = now() WHERE subject_id = $1`, erased); err != nil {
		t.Fatal(err)
	}
	if err := h.users.HardPurgeAccount(ctx, erased); err != nil {
		t.Fatal(err)
	}

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, leaving)
	requireProblemCode(t, resp, raw, fiber.StatusUnprocessableEntity, "media_link_subject_inactive")
	if row, _ := h.coreRow(t, leaving); row.AccountState != user.AccountDeletionPending {
		t.Fatalf("being erased: row %+v", row)
	}
	if n := h.links(t, leaving); n != 0 {
		t.Fatalf("being erased: %d access log rows", n)
	}

	resp, raw = h.linkFor(t, formsHTTP, h.file.ID, erased)
	h.requireRefusedWithoutRow(t, resp, raw, erased)
}

// A person core has a row for is linked as before: Keycloak is not asked
// (here it cannot even be reached) and the row is left as it is.
func TestReadLinkForAReviewerCoreKnowsDoesNotAskKeycloakHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	reviewer := uuid.New()
	if _, _, err := user.NewService(h.users).Ensure(context.Background(), reviewer, user.Profile{Email: "rita@example.com", FirstName: "Rita", Username: "rita"}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.coreRow(t, reviewer)
	h.privateMediaHTTP = h.withDirectory(t, unreachableKeycloak(t))

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, reviewer)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if after, _ := h.coreRow(t, reviewer); !after.UpdatedAt.Equal(before.UpdatedAt) || after.Email != before.Email {
		t.Fatalf("row before %+v, after %+v", before, after)
	}
	if n := h.links(t, reviewer); n != 1 {
		t.Fatalf("%d access log rows", n)
	}
}

// Nothing is looked up or ensured before the Media is known to be the
// product's, and nothing for an admin, who acts for themselves.
func TestReadLinkEnsuresNothingForAnotherMediaOrAnAdminHTTP(t *testing.T) {
	h := newReadLinkAccountsHTTP(t)
	reviewer := uuid.New()
	h.dir.PutUser(identity.Person{ID: reviewer, Email: "rita@example.com", Username: "rita"})

	resp, raw := h.linkFor(t, formsHTTP, uuid.New(), reviewer)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("a Media that does not exist: status %d body %s", resp.StatusCode, raw)
	}
	if row, ok := h.coreRow(t, reviewer); ok {
		t.Fatalf("a Media that does not exist: core row %+v", row)
	}

	// An admin with a row uploads a certificate asset; another admin core
	// has no row for (Keycloak knows them) is refused as before.
	uploader := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	if _, _, err := user.NewService(h.users).Ensure(context.Background(), uploader.ID, user.Profile{Email: "yk@example.com", Username: "yk"}); err != nil {
		t.Fatal(err)
	}
	upload := postMedia(t, h.app(t, uploader), "certificate_asset", "background.png", pngDotHTTP())
	if upload.status != fiber.StatusCreated {
		t.Fatalf("upload: status %d body %v", upload.status, upload.body)
	}
	asset := uuid.MustParse(upload.body["id"].(string))
	adminLink := func(ident authn.Identity) (*http.Response, []byte) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+asset.String()+"/links", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		return do(t, h.app(t, ident), req)
	}
	if resp, raw := adminLink(uploader); resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("admin: status %d body %s", resp.StatusCode, raw)
	}
	rowless := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	h.dir.PutUser(identity.Person{ID: rowless.ID, Email: "dk@example.com", Username: "dk"})
	resp, raw = adminLink(rowless)
	h.requireRefusedWithoutRow(t, resp, raw, rowless.ID)

	if slices.Contains(h.dir.Ops, "GetUser") {
		t.Fatalf("the directory was asked: %v", h.dir.Ops)
	}
}
