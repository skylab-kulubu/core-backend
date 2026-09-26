package media_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// serviceAccount is the service account of a product holding the
// media:attach role on the core client.
func serviceAccount(product authz.Product) authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Product: product, Roles: []string{"media:attach"}}
}

var (
	formsService = serviceAccount(authz.ProductForms)
	cmsService   = serviceAccount(authz.ProductCMS)
)

// attachFixture is the media service over a memory store the test fills
// directly, so any purpose (private ones included) can be linked.
type attachFixture struct {
	svc   media.Service
	store *media.MemoryStore
}

func newAttachFixture(t *testing.T) attachFixture {
	t.Helper()
	store := media.NewMemoryStore()
	return attachFixture{
		svc:   media.NewService(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), ""),
		store: store,
	}
}

// stored is a pending Media of the purpose.
func (f attachFixture) stored(t *testing.T, purpose string) media.Media {
	t.Helper()
	created, err := f.store.Create(context.Background(), media.Media{
		Name: purpose, Type: "application/pdf", Kind: media.KindFile, Key: "files/" + uuid.NewString(),
		UploadedBy: uuid.New(), Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (f attachFixture) attach(p authz.Principal, m media.Media, service, role string) (media.Attachment, bool, error) {
	owner := media.Owner{Service: authz.Product(service), Type: "record", ID: uuid.New()}
	return f.svc.Attach(context.Background(), p, m.ID, owner, media.Role(role))
}

// Each role accepts the purposes its product uploads for it, and legacy
// Media while products still upload without a purpose.
func TestServiceAttach_RoleMustFitThePurpose(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)

	for _, tc := range []struct {
		caller           authz.Principal
		service, role    string
		fits, doesNotFit []string
	}{
		{formsService, "forms", "answer", []string{"answer_file", "answer_file_large", "legacy"}, []string{"cms_image", "cms_file", "event_cover"}},
		{cmsService, "cms", "image", []string{"cms_image", "legacy"}, []string{"cms_file", "event_gallery", "profile_picture"}},
		{cmsService, "cms", "file", []string{"cms_file", "legacy"}, []string{"cms_image", "event_cover"}},
	} {
		for _, purpose := range tc.fits {
			if _, created, err := f.attach(tc.caller, f.stored(t, purpose), tc.service, tc.role); err != nil || !created {
				t.Errorf("%s %s ← %s: created %v, err %v", tc.service, tc.role, purpose, created, err)
			}
		}
		for _, purpose := range tc.doesNotFit {
			_, _, err := f.attach(tc.caller, f.stored(t, purpose), tc.service, tc.role)
			var refusal *media.LinkRefusal
			if !errors.Is(err, media.ErrPurposeMismatch) || !errors.As(err, &refusal) ||
				refusal.Purpose != purpose || refusal.Role != media.Role(tc.role) {
				t.Errorf("%s %s ← %s: err = %v, want %v", tc.service, tc.role, purpose, err, media.ErrPurposeMismatch)
			}
		}
	}
}

// Private Media are attachable only by their owning product: an Answer file
// only by Skyforms, a certificate asset by no product (core links it). The
// refusal comes before the role check and does not name the purpose.
func TestServiceAttach_PrivateMediaOnlyByItsOwningProduct(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)

	for _, tc := range []struct {
		caller                 authz.Principal
		service, role, purpose string
	}{
		{cmsService, "cms", "file", "answer_file"},
		{cmsService, "cms", "image", "answer_file_large"},
		{formsService, "forms", "answer", "certificate_asset"},
		{cmsService, "cms", "image", "certificate_asset"},
	} {
		m := f.stored(t, tc.purpose)
		_, _, err := f.attach(tc.caller, m, tc.service, tc.role)
		var refusal *media.LinkRefusal
		if !errors.Is(err, media.ErrProductMismatch) || !errors.As(err, &refusal) || refusal.Purpose != "" || refusal.MediaID != m.ID {
			t.Errorf("%s attaching %s: err = %v, want %v", tc.service, tc.purpose, err, media.ErrProductMismatch)
		}
		if got, _ := f.store.Get(context.Background(), m.ID); got.Status != media.StatusPending {
			t.Errorf("%s: status %q after a refused attach", tc.purpose, got.Status)
		}
	}
	if _, created, err := f.attach(formsService, f.stored(t, "answer_file"), "forms", "answer"); err != nil || !created {
		t.Fatalf("Skyforms attaching its Answer file: created %v, err %v", created, err)
	}
}

// The link rules of core's own links hold for another product's: no Media
// attachment to a Media that is gone, archived, being purged or expired.
func TestServiceAttach_RefusesMediaThatCannotBeLinked(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	create := func(change func(*media.Media)) media.Media {
		m := media.Media{Name: "logo.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(),
			UploadedBy: uuid.New(), Purpose: "cms_image"}
		change(&m)
		created, err := f.store.Create(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}

	for name, m := range map[string]media.Media{
		"missing":      {ID: uuid.New()},
		"archived":     create(func(m *media.Media) { m.DeletedAt = &past }),
		"being purged": create(func(m *media.Media) { m.BlobPurgeStartedAt = &past }),
		"purged":       create(func(m *media.Media) { m.BlobPurgedAt = &past }),
		"expired":      create(func(m *media.Media) { m.ExpiresAt = &past }),
	} {
		_, _, err := f.attach(cmsService, m, "cms", "image")
		var refusal *media.LinkRefusal
		if !errors.Is(err, media.ErrNotLinkable) || !errors.As(err, &refusal) || refusal.MediaID != m.ID || refusal.Role != media.RoleCMSImage {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrNotLinkable)
		}
	}
}

// A retried attach answers the Media attachment already there, even when the
// Media was archived since: the link exists, only new ones are refused.
func TestServiceAttach_SameLinkAgainAnswersTheExistingAttachment(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	m := f.stored(t, "cms_file")
	owner := media.Owner{Service: "cms", Type: "page", ID: uuid.New()}
	first, created, err := f.svc.Attach(ctx, cmsService, m.ID, owner, media.RoleCMSFile)
	if err != nil || !created {
		t.Fatalf("attach: created %v, err %v", created, err)
	}
	if err := f.store.Archive(ctx, m.ID, nil); err != nil {
		t.Fatal(err)
	}

	again, created, err := f.svc.Attach(ctx, cmsService, m.ID, owner, media.RoleCMSFile)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("attach again: %+v created %v err %v; want %s", again, created, err, first.ID)
	}
}

// A Media stays attached while any Media attachment keeps it. Removing the
// last one detaches it: a purposed Media is purged 30 days later unless
// something attaches it again, a legacy one is kept.
func TestServiceDetach_TheLastAttachmentStartsTheDetachedWindow(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	answer := f.stored(t, "answer_file")
	legacy := f.stored(t, "legacy")
	draft, _, err := f.attach(formsService, answer, "forms", "answer")
	if err != nil {
		t.Fatal(err)
	}
	response, _, err := f.attach(formsService, answer, "forms", "answer")
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := f.attach(formsService, legacy, "forms", "answer")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.svc.Detach(ctx, formsService, answer.ID, draft.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.Get(ctx, answer.ID); got.Status != media.StatusAttached || got.ExpiresAt != nil {
		t.Fatalf("one Media attachment left: status %q expires %v", got.Status, got.ExpiresAt)
	}
	before := time.Now()
	if err := f.svc.Detach(ctx, formsService, answer.ID, response.ID); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	window := 30 * 24 * time.Hour
	if got, _ := f.store.Get(ctx, answer.ID); got.Status != media.StatusDetached || got.ExpiresAt == nil ||
		got.ExpiresAt.Before(before.Add(window)) || got.ExpiresAt.After(after.Add(window)) {
		t.Fatalf("last Media attachment removed: status %q expires %v, want detached for 30 days", got.Status, got.ExpiresAt)
	}
	if err := f.svc.Detach(ctx, formsService, legacy.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.Get(ctx, legacy.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("legacy Media: status %q expires %v, want detached with no expiry", got.Status, got.ExpiresAt)
	}
}

// Removing a Media attachment that is not there (any more) succeeds, so a
// retry does; removing another product's is refused and keeps it.
func TestServiceDetach_IsIdempotentAndOnlyForTheOwningProduct(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	m := f.stored(t, "legacy")
	page, _, err := f.attach(cmsService, m, "cms", "image")
	if err != nil {
		t.Fatal(err)
	}

	err = f.svc.Detach(ctx, formsService, m.ID, page.ID)
	if !errors.Is(err, media.ErrAttachWrongService) {
		t.Fatalf("Skyforms removing a CMS attachment: err = %v, want %v", err, media.ErrAttachWrongService)
	}
	if got, _ := f.store.Get(ctx, m.ID); got.Status != media.StatusAttached {
		t.Fatalf("status %q after a refused detach", got.Status)
	}
	for range 2 {
		if err := f.svc.Detach(ctx, cmsService, m.ID, page.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Detach(ctx, cmsService, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("unknown Media attachment: %v", err)
	}
}

// A product names one of its own roles and a record: a type in lowercase
// snake_case and an id. Anything else is refused before the Media is looked
// at, legacy Media included (they fit every role, but only a real one).
func TestServiceAttach_RefusesUnknownRolesAndMalformedOwners(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	m := f.stored(t, "legacy")

	for name, role := range map[string]media.Role{"no such role": "cover", "another product's role": media.RoleFormsAnswer, "empty": ""} {
		_, _, err := f.svc.Attach(ctx, cmsService, m.ID, media.Owner{Service: "cms", Type: "page", ID: uuid.New()}, role)
		if !errors.Is(err, media.ErrRoleUnknown) || !errors.Is(err, media.ErrInvalid) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrRoleUnknown)
		}
	}
	for name, owner := range map[string]media.Owner{
		"no type":        {Service: "cms", Type: "", ID: uuid.New()},
		"type with caps": {Service: "cms", Type: "Page", ID: uuid.New()},
		"type too long":  {Service: "cms", Type: "p" + strings.Repeat("a", 64), ID: uuid.New()},
		"no id":          {Service: "cms", Type: "page"},
	} {
		_, _, err := f.svc.Attach(ctx, cmsService, m.ID, owner, media.RoleCMSImage)
		if !errors.Is(err, media.ErrInvalid) || errors.Is(err, media.ErrRoleUnknown) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrInvalid)
		}
	}
	if got, _ := f.store.Get(ctx, m.ID); got.Status != media.StatusPending {
		t.Fatalf("status %q after refused attaches", got.Status)
	}
}
