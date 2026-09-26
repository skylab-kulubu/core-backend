package media_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// backfilledCover is a legacy Media an Event used as its cover, given
// event_cover by the purpose backfill, with its blob kept to watch.
func (d mediaDatabase) backfilledCover(t *testing.T) (media.Media, event.Event) {
	t.Helper()
	cover := d.withBlob(t, media.PurposeLegacy)
	created, err := d.events().Create(context.Background(), d.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	d.backfillPurposes(t)
	if got := d.get(t, cover.ID); got.Purpose != media.PurposeEventCover {
		t.Fatalf("backfilled cover purpose %q", got.Purpose)
	}
	return cover, created
}

// dropCover takes the Event's cover away, detaching it.
func (d mediaDatabase) dropCover(t *testing.T, e event.Event) {
	t.Helper()
	e.CoverImageID = nil
	if _, err := d.events().Update(context.Background(), d.organizer, e.ID, e); err != nil {
		t.Fatal(err)
	}
}

// K2: a Media the backfill gave a purpose may still be used where core
// cannot see (CMS content by address). Removed from its last record it is
// detached with no expiry, as it was while legacy, until the hold is
// released after stage 5.
func TestPostgresBackfilledMediaDetachesWithNoExpiryWhileHeld(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover, created := db.backfilledCover(t)

	db.dropCover(t, created)

	if got := db.get(t, cover.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("held cover: status %q expires %v, want detached with no expiry", got.Status, got.ExpiresAt)
	}
	if _, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(31*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if db.purged(t, cover) {
		t.Fatal("held cover purged at day 31")
	}
}

// Linking a held Media again works as for any Media, and the hold stays: the
// next detach sets no expiry either.
func TestPostgresHeldMediaCanBeLinkedAgainAndStaysHeld(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover, created := db.backfilledCover(t)
	db.dropCover(t, created)

	other, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, cover.ID))

	db.dropCover(t, other)
	if got := db.get(t, cover.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("held cover detached again: status %q expires %v, want no expiry", got.Status, got.ExpiresAt)
	}
}

// Only the Media the backfill gave a purpose are held: a Media uploaded with
// a purpose still gets its 30 days when its last record lets it go.
func TestPostgresMediaUploadedWithAPurposeIsNotHeld(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	db.backfilledCover(t)
	cover := db.withBlob(t, "event_cover")
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	db.backfillPurposes(t)

	before := time.Now()
	db.dropCover(t, created)
	detachedWindow(t, db.get(t, cover.ID), before, time.Now())
}

// Restoring an archived Media starts its purpose's pending window, except
// for a held one, which keeps no expiry as a legacy one does.
func TestPostgresRestoredHeldMediaKeepsNoExpiry(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover, created := db.backfilledCover(t)
	db.dropCover(t, created)
	if err := db.svc.Delete(ctx, db.organizer, cover.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.svc.Restore(ctx, db.organizer, cover.ID); err != nil {
		t.Fatal(err)
	}

	if got := db.get(t, cover.ID); got.DeletedAt != nil || got.ExpiresAt != nil {
		t.Fatalf("restored held cover: archived %v expires %v, want current with no expiry", got.DeletedAt, got.ExpiresAt)
	}
}

// Account erasure ignores the hold: a held profile picture is purged at once,
// by the store calls the erasure worker's anonymize_core and
// erase_profile_media steps make.
func TestPostgresAccountErasurePurgesAHeldProfilePicture(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	users := user.NewPostgresStore(db.pool)
	person := uuid.MustParse(db.organizer.ID)
	picture := db.withBlob(t, media.PurposeLegacy)
	if _, err := user.NewService(users).SetProfilePicture(ctx, person, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	db.backfillPurposes(t)
	if got := db.get(t, picture.ID); got.Purpose != media.PurposeProfilePicture {
		t.Fatalf("backfilled picture purpose %q", got.Purpose)
	}

	request, err := users.RequestDeletion(ctx, person, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := users.AnonymizeAccount(ctx, person, at, nil); err != nil {
		t.Fatal(err)
	}
	mediaID, err := users.ProfileMediaForDeletion(ctx, request.ID)
	if err != nil || mediaID == nil || *mediaID != picture.ID {
		t.Fatalf("profile media for deletion %v err %v", mediaID, err)
	}
	if err := media.NewImmediateBlobEraser(db.store, db.blobs).EnsureErased(ctx, *mediaID, at); err != nil {
		t.Fatal(err)
	}

	if !db.purged(t, picture) {
		t.Fatal("held profile picture kept by account erasure")
	}
}

func (d mediaDatabase) releaseHold(t *testing.T, now time.Time, apply bool) media.HoldReleaseReport {
	t.Helper()
	report, err := media.ReleaseDetachExpiryHold(context.Background(), d.store, now, apply, func(err error) {
		t.Errorf("release failure: %v", err)
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// After stage 5 (ticket 18) the hold is released: every held Media follows
// its purpose again, and the ones detached by then get their 30 days from
// the release, not from when they were detached. Without apply it only
// counts; a second release finds nothing.
func TestPostgresReleasingTheHoldStartsThirtyDaysFromTheRelease(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	dropped, droppedEvent := db.backfilledCover(t)
	kept, keptEvent := db.backfilledCover(t)
	db.dropCover(t, droppedEvent)

	if held := db.legacyReport(t).DetachExpiryHeld; held != 2 {
		t.Fatalf("report: %d held, want 2", held)
	}
	dry := db.releaseHold(t, time.Now(), false)
	if dry != (media.HoldReleaseReport{Held: 2, HeldDetached: 1}) {
		t.Fatalf("dry run %+v", dry)
	}
	if got := db.get(t, dropped.ID); got.ExpiresAt != nil {
		t.Fatalf("a dry run set an expiry: %v", got.ExpiresAt)
	}

	releasedAt := time.Now().Add(10 * 24 * time.Hour)
	if report := db.releaseHold(t, releasedAt, true); report != (media.HoldReleaseReport{Held: 2, HeldDetached: 1, Released: 2, WindowsStarted: 1}) {
		t.Fatalf("release %+v", report)
	}
	window := releasedAt.Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	if got := db.get(t, dropped.ID); got.ExpiresAt == nil || !got.ExpiresAt.Equal(window) || got.Purpose != media.PurposeEventCover {
		t.Fatalf("released cover purpose %q expires %v, want event_cover until %v", got.Purpose, got.ExpiresAt, window)
	}
	attached(t, db.get(t, kept.ID))

	for _, day := range []int{29, 31} {
		if _, err := media.PurgeExpired(ctx, db.store, db.blobs, releasedAt.Add(time.Duration(day)*24*time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if purged := db.purged(t, dropped); purged != (day == 31) {
			t.Fatalf("day %d after the release: purged %v", day, purged)
		}
	}

	// Released, the cover still in use detaches like any purposed Media.
	before := time.Now()
	db.dropCover(t, keptEvent)
	detachedWindow(t, db.get(t, kept.ID), before, time.Now())

	if report := db.releaseHold(t, releasedAt.Add(time.Hour), true); report != (media.HoldReleaseReport{}) {
		t.Fatalf("second release %+v", report)
	}
	if held := db.legacyReport(t).DetachExpiryHeld; held != 0 {
		t.Fatalf("report after the release: %d held", held)
	}
}

// A held Media the release cannot update is reported by id and stays held;
// the ones after it are released, and the next run finishes it.
func TestPostgresReleaseWalksPastAMediaItCannotUpdate(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	photos := make([]uuid.UUID, 0, 30)
	for range 30 {
		photos = append(photos, db.storedBeforePurposes(t, "photo.png").ID)
	}
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().AddImages(ctx, db.organizer, created.ID, photos); err != nil {
		t.Fatal(err)
	}
	db.backfillPurposes(t)
	if _, err := db.events().RemoveImages(ctx, db.organizer, created.ID, photos); err != nil {
		t.Fatal(err)
	}
	stuck := photos[3]
	if _, err := db.pool.Exec(ctx, `
		CREATE FUNCTION refuse_stuck_media() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'disk full'; END; $$;
		CREATE TRIGGER stuck_media BEFORE UPDATE ON media FOR EACH ROW
		WHEN (OLD.id = '`+stuck.String()+`') EXECUTE FUNCTION refuse_stuck_media();`); err != nil {
		t.Fatal(err)
	}

	var failures []error
	report, err := media.ReleaseDetachExpiryHold(ctx, db.store, time.Now(), true, func(err error) { failures = append(failures, err) })
	if err != nil {
		t.Fatal(err)
	}
	if report != (media.HoldReleaseReport{Held: 30, HeldDetached: 30, Released: 29, WindowsStarted: 29, Failed: 1}) ||
		len(failures) != 1 || !strings.Contains(failures[0].Error(), stuck.String()) {
		t.Fatalf("report %+v failures %v", report, failures)
	}
	if got := db.get(t, stuck); got.ExpiresAt != nil {
		t.Fatalf("the Media that failed got expiry %v", got.ExpiresAt)
	}

	if _, err := db.pool.Exec(ctx, `DROP TRIGGER stuck_media ON media`); err != nil {
		t.Fatal(err)
	}
	if report := db.releaseHold(t, time.Now(), true); report != (media.HoldReleaseReport{Held: 1, HeldDetached: 1, Released: 1, WindowsStarted: 1}) {
		t.Fatalf("retry %+v", report)
	}
}

// The link rules check a Media's purpose before the write, outside its
// transaction. A link checked while the Media was legacy, written while the
// backfill gives it a purpose, must end refused or fitting, never
// mismatched: the database checks the purpose again, under a lock that waits
// for the backfill's. The backfill here is the real step, held after it
// locked the Media and read its uses.
func TestPostgresALinkRacingTheBackfillEndsRefusedOrFitting(t *testing.T) {
	for name, link := range map[string]struct {
		write   func(db mediaDatabase, m media.Media) error
		blocked string
	}{
		"a CMS page's image": {
			write: func(db mediaDatabase, m media.Media) error {
				_, _, err := db.svc.Attach(context.Background(), cmsService, m.ID, media.AttachRequest{Owner: homePage, Role: media.RoleCMSImage, OnBehalfOf: db.uploader()})
				return err
			},
			blocked: "INSERT INTO media_attachments",
		},
		"an Event's cover": {
			write: func(db mediaDatabase, m media.Media) error {
				_, err := db.events().Create(context.Background(), db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &m.ID})
				return err
			},
			blocked: "INSERT INTO events",
		},
		"a certificate template's background": {
			write: func(db mediaDatabase, m media.Media) error {
				_, err := certificate.NewPostgresStore(db.pool).CreateTemplate(context.Background(), certificate.Template{
					ID: uuid.New(), Name: "Katılım", OwnerTeam: "WEBLAB", SourceKind: "upload",
					DraftLayout: certificate.Layout{Width: 297, Height: 210, Orientation: "landscape", BackgroundMediaID: &m.ID, Elements: []certificate.Element{}},
				})
				return err
			},
			blocked: "INSERT INTO certificate_templates",
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := newMediaDatabase(t)
			ctx := context.Background()
			// A legacy profile picture: the backfill gives it
			// profile_picture, which fits none of the links below.
			cover := db.storedBeforePurposes(t, "me.png")
			if _, err := user.NewService(user.NewPostgresStore(db.pool)).SetProfilePicture(ctx, uuid.MustParse(db.organizer.ID), cover.ID, cover.Key); err != nil {
				t.Fatal(err)
			}
			catalogue, err := media.LoadCatalogue()
			if err != nil {
				t.Fatal(err)
			}
			locked, proceed := make(chan struct{}), make(chan struct{})
			backfilled := make(chan error, 1)
			go func() {
				backfilled <- media.BackfillOneLegacyPurpose(ctx, db.store, catalogue, cover.ID, func() {
					close(locked)
					<-proceed
				})
			}()
			<-locked

			written := make(chan error, 1)
			go func() { written <- link.write(db, cover) }()
			linkErr, done := blockedOrDone(t, db, link.blocked, written)
			close(proceed)
			if err := <-backfilled; err != nil {
				t.Fatal(err)
			}
			if !done {
				linkErr = <-written
			}

			purpose := db.get(t, cover.ID).Purpose
			rows, err := db.pool.Query(ctx, `SELECT owner_service || ' ' || role FROM media_attachments
				WHERE media_id = $1 AND NOT media_purpose_fits_role(owner_service, role, $2)`, cover.ID, purpose)
			if err != nil {
				t.Fatal(err)
			}
			mismatched, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil || len(mismatched) != 0 {
				t.Fatalf("a %s Media has Media attachments %v that do not fit it (link err %v)", purpose, mismatched, linkErr)
			}
			// Refused as a Media that cannot be linked, not a server error.
			var refusal *media.LinkRefusal
			if linkErr != nil && (!errors.As(linkErr, &refusal) || !errors.Is(linkErr, media.ErrNotLinkable) || refusal.MediaID != cover.ID) {
				t.Fatalf("link refused with %v, want media_not_linkable for %s", linkErr, cover.ID)
			}
		})
	}
}

// blockedOrDone waits until a query containing fragment waits for a lock,
// or until done delivers; it reports what done delivered, if it did.
func blockedOrDone(t *testing.T, db mediaDatabase, fragment string, done <-chan error) (error, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return err, true
		default:
		}
		var blocked bool
		if err := db.pool.QueryRow(context.Background(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%')`, fragment).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return nil, false
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the link neither waited for the backfill nor finished")
	return nil, false
}

// A pass stops at a cancelled context with the counts so far; the items it
// did not reach are not counted as failed.
func TestBackfillPassStopsAtACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	items := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	applied := 0
	report, err := media.BackfillPass(ctx, "media",
		func(context.Context, uuid.UUID, int) ([]uuid.UUID, error) { return items, nil },
		func(id uuid.UUID) uuid.UUID { return id },
		func(context.Context, uuid.UUID) error {
			applied++
			if applied == 2 {
				cancel()
			}
			return nil
		}, func(err error) { t.Errorf("failure reported: %v", err) })
	if !errors.Is(err, context.Canceled) || report != (media.BackfillReport{Applied: 2}) {
		t.Fatalf("report %+v err %v", report, err)
	}
}

// The background backfill retries a Media that keeps failing on every pass,
// but reports it once an hour, and reports only the pass that assigned or
// failed something new.
func TestPostgresLegacyPurposeBackfillReportsAMediaThatKeepsFailingOnce(t *testing.T) {
	db := newMediaDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db.backfilledCover(t)
	stuck := db.storedBeforePurposes(t, "stuck.png")
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &stuck.ID}); err != nil {
		t.Fatal(err)
	}
	photo := db.storedBeforePurposes(t, "photo.png")
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Talk", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &photo.ID}); err != nil {
		t.Fatal(err)
	}
	// Each attempt on the stuck Media counts itself before it fails; a
	// sequence is not rolled back with the attempt.
	if _, err := db.pool.Exec(ctx, `
		CREATE SEQUENCE stuck_attempts;
		CREATE FUNCTION refuse_stuck_media() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM nextval('stuck_attempts'); RAISE EXCEPTION 'disk full'; END; $$;
		CREATE TRIGGER stuck_media BEFORE UPDATE ON media FOR EACH ROW
		WHEN (OLD.id = '`+stuck.ID.String()+`') EXECUTE FUNCTION refuse_stuck_media();`); err != nil {
		t.Fatal(err)
	}
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var passes []media.LegacyPurposeReport
	var failures []error
	media.MaintainLegacyPurposeBackfill(ctx, db.store, catalogue, time.Millisecond,
		func(report media.LegacyPurposeReport) { mu.Lock(); passes = append(passes, report); mu.Unlock() },
		func(err error) { mu.Lock(); failures = append(failures, err); mu.Unlock() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		var attempts int64
		if err := db.pool.QueryRow(ctx, `SELECT COALESCE((SELECT last_value FROM stuck_attempts WHERE is_called), 0)`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stuck Media was tried %d times", attempts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), stuck.ID.String()) {
		t.Fatalf("failures reported %v, want the stuck Media once", failures)
	}
	if len(passes) != 1 || passes[0].Assigned != 1 || passes[0].Failed != 1 {
		t.Fatalf("passes reported %+v, want only the first", passes)
	}
	if got := db.get(t, photo.ID); got.Purpose != media.PurposeEventCover {
		t.Fatalf("the Media after the stuck one: purpose %q", got.Purpose)
	}
}

// The release is recorded: purpose-less uploads keep arriving until stage 6,
// and the backfill that gives them a purpose afterwards does not hold them,
// so nothing is held for ever. A dry run records nothing, and a second
// release keeps the first release's time.
func TestPostgresTheBackfillHoldsNothingAfterTheRelease(t *testing.T) {
	db := newMediaDatabase(t)
	if report := db.legacyReport(t); report.HoldReleasedAt != nil {
		t.Fatalf("released at %v before any release", report.HoldReleasedAt)
	}
	db.releaseHold(t, time.Now(), false)
	if report := db.legacyReport(t); report.HoldReleasedAt != nil {
		t.Fatalf("a dry run recorded a release at %v", report.HoldReleasedAt)
	}
	releasedAt := time.Now().Truncate(time.Microsecond)
	db.releaseHold(t, releasedAt, true)
	db.releaseHold(t, releasedAt.Add(time.Hour), true)
	if report := db.legacyReport(t); report.HoldReleasedAt == nil || !report.HoldReleasedAt.Equal(releasedAt) {
		t.Fatalf("released at %v, want %v", report.HoldReleasedAt, releasedAt)
	}

	cover, created := db.backfilledCover(t)
	before := time.Now()
	db.dropCover(t, created)
	detachedWindow(t, db.get(t, cover.ID), before, time.Now())
	if held := db.legacyReport(t).DetachExpiryHeld; held != 0 {
		t.Fatalf("%d held after the release", held)
	}
}

// attachmentsOf lists a Media's Media attachments as "service role".
func (d mediaDatabase) attachmentsOf(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows, err := d.pool.Query(context.Background(), `SELECT owner_service || ' ' || role FROM media_attachments WHERE media_id = $1 ORDER BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	uses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return uses
}

// The hold protects uses core cannot see yet, such as a CMS page showing an
// Event cover by address. When stage 5 attaches that use, the held Media's
// purpose no longer fits all of its uses: it goes back to legacy, as a Media
// with mixed uses does (K1), keeps its hold and gets the CMS attachment.
func TestPostgresAProductAttachingAHeldMediaTurnsItBackToLegacy(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover, created := db.backfilledCover(t)

	a, isNew, err := db.svc.Attach(ctx, cmsService, cover.ID, media.AttachRequest{Owner: homePage, Role: media.RoleCMSImage, OnBehalfOf: db.uploader()})
	if err != nil || !isNew || a.Owner != homePage {
		t.Fatalf("attach %+v created %v err %v", a, isNew, err)
	}

	got := db.get(t, cover.ID)
	if got.Purpose != media.PurposeLegacy {
		t.Fatalf("purpose %q, want legacy", got.Purpose)
	}
	attached(t, got)
	if uses := db.attachmentsOf(t, cover.ID); len(uses) != 2 || uses[0] != "cms image" || uses[1] != "core event_cover" {
		t.Fatalf("Media attachments %v, want the CMS page's and the Event's", uses)
	}
	if held := db.legacyReport(t).DetachExpiryHeld; held != 1 {
		t.Fatalf("%d held, want the hold kept", held)
	}
	// The next backfill keeps it legacy: its uses are mixed now.
	if report := db.backfillPurposes(t); report.KeptMixed != 1 || db.get(t, cover.ID).Purpose != media.PurposeLegacy {
		t.Fatalf("backfill %+v purpose %q", report, db.get(t, cover.ID).Purpose)
	}
	// Legacy again, it gets no expiry from the release either: only the
	// orphan switch gives a legacy Media one.
	if err := db.svc.Detach(ctx, cmsService, cover.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	db.dropCover(t, created)
	if report := db.releaseHold(t, time.Now(), true); report.Released != 1 || report.WindowsStarted != 0 {
		t.Fatalf("release %+v, want the hold cleared and no window", report)
	}
	if got := db.get(t, cover.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("released legacy Media: status %q expires %v", got.Status, got.ExpiresAt)
	}
}

// Only held Media go back: a Media uploaded with a core purpose stays core's
// and is refused as today.
func TestPostgresAProductCannotAttachAMediaUploadedWithACorePurpose(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover := db.withBlob(t, "event_cover")

	_, _, err := db.svc.Attach(ctx, cmsService, cover.ID, media.AttachRequest{Owner: homePage, Role: media.RoleCMSImage, OnBehalfOf: db.uploader()})
	if !errors.Is(err, media.ErrNotLinkable) {
		t.Fatalf("attach err %v, want media_not_linkable", err)
	}
	if got := db.get(t, cover.ID); got.Purpose != media.PurposeEventCover || len(db.attachmentsOf(t, cover.ID)) != 0 {
		t.Fatalf("purpose %q attachments %v", got.Purpose, db.attachmentsOf(t, cover.ID))
	}
}

// Two products' pages attaching the same held Media at once both succeed:
// each takes the Media row's lock before anything else, so they queue and
// cannot deadlock.
func TestPostgresTwoAttachesOfAHeldMediaAtOnceDoNotDeadlock(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover, _ := db.backfilledCover(t)
	// Hold both attaches at the Media row's lock, after their checks.
	gate, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT id FROM media WHERE id = $1 FOR UPDATE`, cover.ID); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	for _, page := range []media.Owner{homePage, aboutPage} {
		go func() {
			_, _, err := db.svc.Attach(ctx, cmsService, cover.ID, media.AttachRequest{Owner: page, Role: media.RoleCMSImage, OnBehalfOf: db.uploader()})
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
			  AND query LIKE '%detach_expiry_held%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d attaches waiting for the Media row, want 2", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	if uses := db.attachmentsOf(t, cover.ID); len(uses) != 3 {
		t.Fatalf("Media attachments %v, want both pages and the Event", uses)
	}
	if got := db.get(t, cover.ID).Purpose; got != media.PurposeLegacy {
		t.Fatalf("purpose %q", got)
	}
}
