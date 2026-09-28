package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// readLinkSubjectsHTTP is the read link route on PostgreSQL, where the
// access log only names an active account, with a directory standing in
// for Keycloak: the memory directory unless a test names another.
type readLinkSubjectsHTTP struct {
	privateMediaHTTP
	pool  *pgxpool.Pool
	users *user.PostgresStore
	dir   *skyNumberLog
	// file is an Answer file of the respondent's that Skyforms may link.
	file media.Media
}

// skyNumberLog is the memory directory keeping every Sky number written to
// it and counting the Sky number reads.
type skyNumberLog struct {
	*identity.Memory
	mu       sync.Mutex
	written  []string
	skyReads int
}

func (d *skyNumberLog) WriteSkyNumber(ctx context.Context, id uuid.UUID, skyNumber string) error {
	d.mu.Lock()
	d.written = append(d.written, skyNumber)
	d.mu.Unlock()
	return d.Memory.WriteSkyNumber(ctx, id, skyNumber)
}

func (d *skyNumberLog) ReadSkyNumber(ctx context.Context, id uuid.UUID) (string, error) {
	d.mu.Lock()
	d.skyReads++
	d.mu.Unlock()
	return d.Memory.ReadSkyNumber(ctx, id)
}

// writes are the Sky numbers written so far, and the Sky number reads.
func (d *skyNumberLog) writes() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.written), d.skyReads
}

// directoryOps counts the directory's operations named op.
func (d *skyNumberLog) directoryOps(op string) int {
	n := 0
	for _, done := range d.Ops {
		if done == op {
			n++
		}
	}
	return n
}

func newReadLinkSubjectsHTTP(t *testing.T) readLinkSubjectsHTTP {
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
	h := readLinkSubjectsHTTP{pool: pool, users: users, dir: &skyNumberLog{Memory: identity.NewMemory()}}
	h.privateMediaHTTP = h.with(t, h.dir, users)
	h.file = h.uploadAnswerFile(t, "cv.pdf")
	return h
}

// with is the media routes on the same database, asking dir about a person
// users has no row for.
func (h readLinkSubjectsHTTP) with(t *testing.T, dir identity.Directory, users user.Store) privateMediaHTTP {
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
				Subjects: readlinksubject.New(dir, users),
			},
		})
	return privateMediaHTTP{svc: svc, private: private, bao: bao, logs: &logRecorder{}}
}

// linkFor asks for a read link to the Media as ident, for person.
func (h readLinkSubjectsHTTP) linkFor(t *testing.T, ident authn.Identity, id uuid.UUID, person uuid.UUID) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+id.String()+"/links", strings.NewReader(`{"onBehalfOf":"`+person.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h.app(t, ident), req)
}

// coreRow is the person's row in core, if there is one.
func (h readLinkSubjectsHTTP) coreRow(t *testing.T, id uuid.UUID) (user.User, bool) {
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
func (h readLinkSubjectsHTTP) links(t *testing.T, person uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM media_read_links WHERE on_behalf_of = $1`, person).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A Skyforms reviewer who never signed in to core, enabled in Keycloak,
// gets a core row from Keycloak and the link. Keycloak is read once, and
// learns the Sky number core gave them, as at a first sign-in.
func TestReadLinkEnsuresTheRowOfAnEnabledReviewerHTTP(t *testing.T) {
	h := newReadLinkSubjectsHTTP(t)
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

	written, skyReads := h.dir.writes()
	if row.SkyNumber == "" || h.dir.directoryOps("WriteSkyNumber") != 1 || !slices.Equal(written, []string{row.SkyNumber}) {
		t.Fatalf("row Sky number %q, written to Keycloak %q", row.SkyNumber, written)
	}
	if inKeycloak, err := h.dir.Memory.ReadSkyNumber(context.Background(), reviewer); err != nil || inKeycloak != row.SkyNumber {
		t.Fatalf("Keycloak holds %q (%v), core %q", inKeycloak, err, row.SkyNumber)
	}
	if lookups := h.dir.directoryOps("GetUser"); lookups != 1 || skyReads != 0 {
		t.Fatalf("Keycloak read %d times and its Sky number %d times; want once, and no second read", lookups, skyReads)
	}
}

// requireRefusedWithoutRow checks a link for the person was refused as naming
// no active account, and left no row and no link behind.
func (h readLinkSubjectsHTTP) requireRefusedWithoutRow(t *testing.T, resp *http.Response, raw []byte, person uuid.UUID) {
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
	h := newReadLinkSubjectsHTTP(t)
	reviewer := uuid.New()
	h.dir.PutUser(identity.Person{ID: reviewer, Email: "gone@example.com", FirstName: "Gone", Username: "gone"})
	if err := h.dir.DisableUser(context.Background(), reviewer); err != nil {
		t.Fatal(err)
	}

	resp, raw := h.linkFor(t, formsHTTP, h.file.ID, reviewer)
	h.requireRefusedWithoutRow(t, resp, raw, reviewer)
}

// A person Keycloak does not know gets no row and no link: one who was never
// there, and one who was and has been deleted (as the erasure's
// delete_identity leaves them).
func TestReadLinkForAPersonKeycloakDoesNotKnowCreatesNoRowHTTP(t *testing.T) {
	h := newReadLinkSubjectsHTTP(t)
	stranger, deleted := uuid.New(), uuid.New()
	h.dir.PutUser(identity.Person{ID: deleted, Email: "deleted@example.com", FirstName: "Deleted", Username: "deleted"})
	if err := h.dir.DeleteUser(context.Background(), deleted); err != nil {
		t.Fatal(err)
	}

	for _, person := range []uuid.UUID{stranger, deleted} {
		resp, raw := h.linkFor(t, formsHTTP, h.file.ID, person)
		h.requireRefusedWithoutRow(t, resp, raw, person)
	}
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
	h := newReadLinkSubjectsHTTP(t)
	h.privateMediaHTTP = h.with(t, unreachableKeycloak(t), h.users)
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
	if logged := h.logs.all(); !strings.Contains(logged, "Keycloak could not be reached") || strings.Contains(logged, reviewer.String()) {
		t.Fatalf("logged %q", h.logs.all())
	}
}

// An account core erased, or is erasing, gets no link and no new row, even
// while Keycloak still reports the person enabled: one being erased keeps
// its row, one erased and hard-purged keeps only its deletion marker.
func TestReadLinkForAnErasedAccountCreatesNoRowHTTP(t *testing.T) {
	h := newReadLinkSubjectsHTTP(t)
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
	h := newReadLinkSubjectsHTTP(t)
	reviewer := uuid.New()
	if _, _, err := user.NewService(h.users).Ensure(context.Background(), reviewer, user.Profile{Email: "rita@example.com", FirstName: "Rita", Username: "rita"}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.coreRow(t, reviewer)
	h.privateMediaHTTP = h.with(t, unreachableKeycloak(t), h.users)

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
	h := newReadLinkSubjectsHTTP(t)
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

	if h.dir.directoryOps("GetUser") != 0 {
		t.Fatalf("the directory was asked: %v", h.dir.Ops)
	}
}

// Skyforms links the files of one answer at once. Concurrent first links for
// a reviewer core has no row for make one row with one Sky number, the next
// one (none is skipped), and every write back to Keycloak carries it.
func TestConcurrentFirstReadLinksGiveTheReviewerOneSkyNumberHTTP(t *testing.T) {
	h := newReadLinkSubjectsHTTP(t)
	const rounds, links = 5, 8
	for round := range rounds {
		reviewer := uuid.New()
		h.dir.PutUser(identity.Person{ID: reviewer, Email: fmt.Sprintf("reviewer%d@example.com", round), Username: fmt.Sprintf("reviewer%d", round)})
		writtenBefore, _ := h.dir.writes()
		apps := make([]*fiber.App, links)
		for i := range apps {
			apps[i] = h.app(t, formsHTTP)
		}
		statuses := make([]int, links)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, app := range apps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+h.file.ID.String()+"/links", strings.NewReader(`{"onBehalfOf":"`+reviewer.String()+`"}`))
				req.Header.Set("Content-Type", "application/json")
				<-start
				resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
				if err != nil {
					statuses[i] = -1
					return
				}
				resp.Body.Close()
				statuses[i] = resp.StatusCode
			}()
		}
		close(start)
		wg.Wait()

		for i, status := range statuses {
			if status != fiber.StatusCreated {
				t.Fatalf("round %d link %d: status %d", round, i, status)
			}
		}
		row, ok := h.coreRow(t, reviewer)
		want, err := user.FormatSkyNumber(round + 2) // the respondent holds the first
		if err != nil {
			t.Fatal(err)
		}
		if !ok || row.SkyNumber != want {
			t.Fatalf("round %d: row %+v (found %v), want Sky number %s", round, row, ok, want)
		}
		written, _ := h.dir.writes()
		written = written[len(writtenBefore):]
		if len(written) == 0 || slices.ContainsFunc(written, func(n string) bool { return n != row.SkyNumber }) {
			t.Fatalf("round %d: core holds %s, Keycloak was written %q", round, row.SkyNumber, written)
		}
		if n := h.links(t, reviewer); n != links {
			t.Fatalf("round %d: %d access log rows", round, n)
		}
	}
}

// answeringKeycloak is core's Keycloak client for a server that hands out
// tokens and answers every user lookup with status and body.
func answeringKeycloak(t *testing.T, status int, body string) *identity.Keycloak {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// takenSkyNumbers is core's users where every next Sky number is one
// another person already holds: assignment stays contended.
type takenSkyNumbers struct {
	*user.PostgresStore
	taken string
}

func (s takenSkyNumbers) NextSkyNumber(context.Context) (string, error) { return s.taken, nil }

// Only a failure that may pass is a 503 to retry: Keycloak unreachable or
// answering 5xx, or Sky numbers contended. Keycloak refusing core (a
// permission it lacks) or answering something that is not a user is a
// misconfiguration: a 500, logged, never offered as worth a retry. The log
// names Keycloak's status, never the person.
func TestReadLinkFailuresToEnsureTheSubjectHTTP(t *testing.T) {
	h := newReadLinkSubjectsHTTP(t)
	respondent, ok := h.coreRow(t, respondentHTTP.ID)
	if !ok || respondent.SkyNumber == "" {
		t.Fatalf("respondent %+v", respondent)
	}
	for name, tc := range map[string]struct {
		dir        identity.Directory
		users      user.Store
		status     int
		code       string
		retryAfter bool
		logged     string
		// keycloak marks a failure before any row is made.
		keycloak bool
	}{
		"Keycloak answers 503": {answeringKeycloak(t, http.StatusServiceUnavailable, `{"error":"unavailable"}`), h.users,
			fiber.StatusServiceUnavailable, "media_link_subject_unavailable", true, "Keycloak answered 503", true},
		"Keycloak cannot be reached": {unreachableKeycloak(t), h.users,
			fiber.StatusServiceUnavailable, "media_link_subject_unavailable", true, "Keycloak could not be reached", true},
		"Sky numbers stay contended": {h.dir, takenSkyNumbers{PostgresStore: h.users, taken: respondent.SkyNumber},
			fiber.StatusServiceUnavailable, "media_link_subject_unavailable", true, "Sky number", false},
		"Keycloak refuses core": {answeringKeycloak(t, http.StatusForbidden, `{"error":"HTTP 403 Forbidden"}`), h.users,
			fiber.StatusInternalServerError, "media_link_subject_lookup_failed", false, "Keycloak answered 403", true},
		"Keycloak answers no user": {answeringKeycloak(t, http.StatusOK, `{"id":"not-a-uuid","enabled":true}`), h.users,
			fiber.StatusInternalServerError, "media_link_subject_lookup_failed", false, "not a user", true},
	} {
		reviewer := uuid.New()
		h.dir.PutUser(identity.Person{ID: reviewer, Email: reviewer.String()[:8] + "@example.com", Username: reviewer.String()[:8]})
		routes := h
		routes.privateMediaHTTP = h.with(t, tc.dir, tc.users)

		resp, raw := routes.linkFor(t, formsHTTP, h.file.ID, reviewer)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if resp.StatusCode != tc.status || body["code"] != tc.code {
			t.Errorf("%s: status %d body %s, want %d %s", name, resp.StatusCode, raw, tc.status, tc.code)
			continue
		}
		if got := resp.Header.Get("Retry-After") != ""; got != tc.retryAfter {
			t.Errorf("%s: Retry-After %q", name, resp.Header.Get("Retry-After"))
		}
		if n := h.links(t, reviewer); n != 0 {
			t.Errorf("%s: %d access log rows", name, n)
		}
		if row, ok := h.coreRow(t, reviewer); ok && tc.keycloak {
			t.Errorf("%s: core row %+v", name, row)
		}
		logged := routes.logs.all()
		if !strings.Contains(logged, tc.logged) || strings.Contains(logged, reviewer.String()) {
			t.Errorf("%s: logged %q", name, logged)
		}
	}
}
