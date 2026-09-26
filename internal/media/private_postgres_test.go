package media_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// privateDatabase is mediaDatabase with private Media on: a fake OpenBao
// and a private bucket.
type privateDatabase struct {
	mediaDatabase
	private *media.MemoryBlob
	bao     *transittest.Server
}

func newPrivateDatabase(t *testing.T) privateDatabase {
	t.Helper()
	db := newMediaDatabase(t)
	// The reviewer links are issued for is an active account.
	if _, _, err := user.NewService(user.NewPostgresStore(db.pool)).Ensure(context.Background(), reviewer, user.Profile{
		Email: "reviewer@example.com", FirstName: "Rita", LastName: "Reviewer", Username: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	bao := transittest.NewServer(t)
	private := media.NewMemoryBlob()
	db.svc = media.NewServiceWithOptions(db.store, db.blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: authz.ServiceProducts,
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(private, transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{9}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: db.store, Now: bao.Clock.Now,
			},
		})
	return privateDatabase{mediaDatabase: db, private: private, bao: bao}
}

func TestPostgresPrivateMediaKeepsItsEncryption(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()

	created, err := db.svc.UploadForPurpose(ctx, db.organizer, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	got := db.get(t, created.ID)
	if got.Visibility != media.VisibilityPrivate || got.Encryption == nil || *got.Encryption != *created.Encryption {
		t.Fatalf("stored %+v with encryption %+v, created with %+v", got, got.Encryption, created.Encryption)
	}
	if got.Encryption.KeyVersion != 1 {
		t.Fatalf("key version %d", got.Encryption.KeyVersion)
	}

	picture := db.upload(t, "event_cover")
	if got := db.get(t, picture.ID); got.Visibility != media.VisibilityPublic || got.Encryption != nil {
		t.Fatalf("public Media stored %+v with encryption %+v", got, got.Encryption)
	}
}

func TestPostgresPrivateMediaCannotLoseItsEncryption(t *testing.T) {
	db := newPrivateDatabase(t)
	uploader := uuid.MustParse(db.organizer.ID)

	for name, m := range map[string]media.Media{
		"private without encryption": {Name: "cv.pdf", Type: "application/pdf", Key: "private/files/x", UploadedBy: uploader, Kind: media.KindFile, Visibility: media.VisibilityPrivate},
		"public with encryption": {Name: "cv.pdf", Type: "application/pdf", Key: "files/x", UploadedBy: uploader, Kind: media.KindFile, Visibility: media.VisibilityPublic,
			Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1}},
	} {
		if _, err := db.store.Create(context.Background(), m); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
}

func TestPostgresAccessLogKeepsEveryLinkAndOpen(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()
	created, err := db.svc.UploadForPurpose(ctx, db.organizer, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := db.bao.Clock.Now()

	link, err := db.svc.IssueReadLink(ctx, formsService, created.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"203.0.113.9", "2001:db8::7"} {
		db.bao.Clock.Advance(time.Minute)
		content, err := db.svc.OpenContent(ctx, created.ID, tokenOf(t, link), ip)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, content); !bytes.Equal(got, pdfFile()) {
			t.Fatalf("opened %q", got)
		}
	}

	var linkID, mediaID, onBehalfOf uuid.UUID
	var product string
	var issued, expires time.Time
	if err := db.pool.QueryRow(ctx, `SELECT id, media_id, product, on_behalf_of, issued_at, expires_at FROM media_read_links`).
		Scan(&linkID, &mediaID, &product, &onBehalfOf, &issued, &expires); err != nil {
		t.Fatal(err)
	}
	if mediaID != created.ID || product != "forms" || onBehalfOf != reviewer || !issued.Equal(issuedAt) || !expires.Equal(issuedAt.Add(5*time.Minute)) {
		t.Fatalf("link row %s %s %s %s %s", mediaID, product, onBehalfOf, issued, expires)
	}
	rows, err := db.pool.Query(ctx, `SELECT opened_at, client_ip FROM media_read_link_opens WHERE link_id = $1 ORDER BY opened_at`, linkID)
	if err != nil {
		t.Fatal(err)
	}
	var opens []string
	for rows.Next() {
		var at time.Time
		var ip string
		if err := rows.Scan(&at, &ip); err != nil {
			t.Fatal(err)
		}
		opens = append(opens, at.Sub(issuedAt).String()+" "+ip)
	}
	if want := []string{"1m0s 203.0.113.9", "2m0s 2001:db8::7"}; !slices.Equal(opens, want) {
		t.Fatalf("opens %v, want %v", opens, want)
	}
}

// A read link names an active account: not a person core does not know, and
// not one being erased.
func TestPostgresReadLinkNeedsAnActiveAccount(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()
	created, err := db.svc.UploadForPurpose(ctx, db.organizer, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	leaving := uuid.New()
	users := user.NewPostgresStore(db.pool)
	if _, _, err := user.NewService(users).Ensure(ctx, leaving, user.Profile{Email: "leaving@example.com", Username: "leaving"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, leaving, nil); err != nil {
		t.Fatal(err)
	}

	for name, person := range map[string]uuid.UUID{"unknown to core": uuid.New(), "being erased": leaving} {
		if _, err := db.svc.IssueReadLink(ctx, formsService, created.ID, person.String()); !errors.Is(err, media.ErrLinkSubjectInactive) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrLinkSubjectInactive)
		}
	}
}

// The access log keeps a link and its opens for a year: a link issued 364
// days ago stays, one issued 366 days ago goes with its opens, and a second
// run changes nothing.
func TestPostgresAccessLogIsKeptForAYear(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()
	created, err := db.svc.UploadForPurpose(ctx, db.organizer, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := func(age time.Duration) uuid.UUID {
		t.Helper()
		issued := now.Add(-age)
		link := media.ReadLinkRecord{ID: uuid.New(), MediaID: created.ID, Product: authz.ProductForms, OnBehalfOf: reviewer,
			IssuedAt: issued, ExpiresAt: issued.Add(media.ReadLinkTTL)}
		if err := db.store.RecordReadLink(ctx, link); err != nil {
			t.Fatal(err)
		}
		if err := db.store.RecordReadLinkOpen(ctx, media.ReadLinkOpen{LinkID: link.ID, OpenedAt: issued.Add(time.Minute), ClientIP: "203.0.113.9"}); err != nil {
			t.Fatal(err)
		}
		return link.ID
	}
	kept, gone := record(364*24*time.Hour), record(366*24*time.Hour)

	deleted, err := db.store.PruneReadLinks(ctx, now.Add(-media.ReadLinkRetention))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted %d, err %v", deleted, err)
	}
	count := func(query string, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := db.pool.QueryRow(ctx, query, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for id, want := range map[uuid.UUID]int{kept: 1, gone: 0} {
		if links := count(`SELECT count(*) FROM media_read_links WHERE id = $1`, id); links != want {
			t.Errorf("link %s: %d rows, want %d", id, links, want)
		}
		if opens := count(`SELECT count(*) FROM media_read_link_opens WHERE link_id = $1`, id); opens != want {
			t.Errorf("opens of %s: %d rows, want %d", id, opens, want)
		}
	}
	if deleted, err := db.store.PruneReadLinks(ctx, now.Add(-media.ReadLinkRetention)); err != nil || deleted != 0 {
		t.Fatalf("second run deleted %d, err %v", deleted, err)
	}
}

func TestPostgresAccessLogRefusesAnOpenOfNoIssuedLink(t *testing.T) {
	db := newPrivateDatabase(t)
	err := db.store.RecordReadLinkOpen(context.Background(), media.ReadLinkOpen{LinkID: uuid.New(), OpenedAt: time.Now(), ClientIP: "203.0.113.9"})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// Private Media are linked through the database's purpose check like any
// other: a private certificate asset on a certificate template draft, and a
// private Answer file on a Skyforms answer.
func TestPostgresPrivateMediaPassTheAttachmentPurposeCheck(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()

	background, err := db.svc.UploadForPurpose(ctx, db.organizer, "certificate_asset", uploaded("background.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	layout := certificate.Layout{Width: 297, Height: 210, Orientation: "landscape", BackgroundMediaID: &background.ID, Elements: []certificate.Element{}}
	if _, err := certificate.NewPostgresStore(db.pool).CreateTemplate(ctx, certificate.Template{
		ID: uuid.New(), Name: "PRIVATE", OwnerTeam: "WEBLAB", SourceKind: "upload", DraftLayout: layout,
	}); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, background.ID))

	answer, err := db.svc.UploadForPurpose(ctx, db.organizer, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := db.svc.Attach(ctx, formsService, answer.ID, media.AttachRequest{
		Owner: media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()},
		Role:  media.RoleFormsAnswer, OnBehalfOf: uuid.MustParse(db.organizer.ID),
	}); err != nil || !created {
		t.Fatalf("Skyforms answer: created %v, err %v", created, err)
	}
	attached(t, db.get(t, answer.ID))
}
