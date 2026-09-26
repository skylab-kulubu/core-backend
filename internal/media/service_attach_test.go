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

// stored is a pending Media of the purpose that person uploaded.
func (f attachFixture) stored(t *testing.T, purpose string, person uuid.UUID) media.Media {
	t.Helper()
	created, err := f.store.Create(context.Background(), media.Media{
		Name: purpose, Type: "application/pdf", Kind: media.KindFile, Key: "files/" + uuid.NewString(),
		UploadedBy: person, Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// request asks for a link to a new record of the product, for person.
func request(product authz.Product, role media.Role, person uuid.UUID) media.AttachRequest {
	return media.AttachRequest{
		Owner:      media.Owner{Service: product, Type: "record", ID: uuid.NewString()},
		Role:       role,
		OnBehalfOf: person,
	}
}

func (f attachFixture) attach(p authz.Principal, m media.Media, req media.AttachRequest) (media.Attachment, bool, error) {
	return f.svc.Attach(context.Background(), p, m.ID, req)
}

// requireNotLinkable checks a refusal that tells the caller nothing about the
// Media: the same as for a Media that does not exist, without its purpose.
func requireNotLinkable(t *testing.T, err error, m media.Media, role media.Role, what string) {
	t.Helper()
	var refusal *media.LinkRefusal
	if !errors.Is(err, media.ErrNotLinkable) || !errors.As(err, &refusal) ||
		refusal.MediaID != m.ID || refusal.Role != role || refusal.Purpose != "" {
		t.Errorf("%s: err = %v, want %v with no purpose", what, err, media.ErrNotLinkable)
	}
}

// A product links Media of its own purposes, and legacy Media. Within its
// own purposes the role must fit, and the refusal names the purpose.
func TestServiceAttach_RoleMustFitThePurpose(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	person := uuid.New()

	for _, tc := range []struct {
		caller authz.Principal
		role   media.Role
		fits   []string
	}{
		{formsService, media.RoleFormsAnswer, []string{"answer_file", "answer_file_large", "legacy"}},
		{cmsService, media.RoleCMSImage, []string{"cms_image", "legacy"}},
		{cmsService, media.RoleCMSFile, []string{"cms_file", "legacy"}},
	} {
		for _, purpose := range tc.fits {
			_, created, err := f.attach(tc.caller, f.stored(t, purpose, person), request(tc.caller.Product, tc.role, person))
			if err != nil || !created {
				t.Errorf("%s %s ← %s: created %v, err %v", tc.caller.Product, tc.role, purpose, created, err)
			}
		}
	}
	for role, purpose := range map[media.Role]string{media.RoleCMSImage: "cms_file", media.RoleCMSFile: "cms_image"} {
		m := f.stored(t, purpose, person)
		_, _, err := f.attach(cmsService, m, request(authz.ProductCMS, role, person))
		var refusal *media.LinkRefusal
		if !errors.Is(err, media.ErrPurposeMismatch) || !errors.As(err, &refusal) || refusal.Purpose != purpose || refusal.Role != role {
			t.Errorf("cms %s ← %s: err = %v, want %v", role, purpose, err, media.ErrPurposeMismatch)
		}
	}
}

// A Media of another product's purpose (private or not) or of core's is
// refused exactly like a Media that does not exist: the caller learns
// neither that it exists nor what it is.
func TestServiceAttach_MediaOfAnotherProductLooksMissing(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	person := uuid.New()

	for _, tc := range []struct {
		caller  authz.Principal
		role    media.Role
		purpose string
	}{
		{cmsService, media.RoleCMSFile, "answer_file"},
		{cmsService, media.RoleCMSImage, "answer_file_large"},
		{cmsService, media.RoleCMSImage, "event_cover"},
		{cmsService, media.RoleCMSImage, "profile_picture"},
		{formsService, media.RoleFormsAnswer, "certificate_asset"},
		{formsService, media.RoleFormsAnswer, "cms_file"},
		{formsService, media.RoleFormsAnswer, "event_gallery"},
	} {
		m := f.stored(t, tc.purpose, person)
		_, _, err := f.attach(tc.caller, m, request(tc.caller.Product, tc.role, person))
		requireNotLinkable(t, err, m, tc.role, string(tc.caller.Product)+" attaching "+tc.purpose)
		if got, _ := f.store.Get(context.Background(), m.ID); got.Status != media.StatusPending {
			t.Errorf("%s: status %q after a refused attach", tc.purpose, got.Status)
		}
	}
	missing := media.Media{ID: uuid.New()}
	_, _, err := f.attach(formsService, missing, request(authz.ProductForms, media.RoleFormsAnswer, person))
	requireNotLinkable(t, err, missing, media.RoleFormsAnswer, "missing")
}

// An Answer file is the respondent's own: Skyforms links it only for the
// person who uploaded it.
func TestServiceAttach_PersonalMediaOnlyForItsUploader(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	respondent, someoneElse := uuid.New(), uuid.New()

	for _, purpose := range []string{"answer_file", "answer_file_large"} {
		m := f.stored(t, purpose, respondent)
		_, _, err := f.attach(formsService, m, request(authz.ProductForms, media.RoleFormsAnswer, someoneElse))
		requireNotLinkable(t, err, m, media.RoleFormsAnswer, purpose+" for someone else")
		if _, created, err := f.attach(formsService, m, request(authz.ProductForms, media.RoleFormsAnswer, respondent)); err != nil || !created {
			t.Errorf("%s for its uploader: created %v, err %v", purpose, created, err)
		}
	}
}

// A legacy Media, uploaded before purposes, may belong to anyone and anything:
// a product links one for the person who uploaded it, or once it already
// holds a Media attachment to it (a CMS editor reusing a library image). No
// product can pin another product's or core's legacy Media.
func TestServiceAttach_LegacyMediaForItsUploaderOrOnceTheProductHoldsIt(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	uploader, editor := uuid.New(), uuid.New()
	image := f.stored(t, "legacy", uploader)

	_, _, err := f.attach(cmsService, image, request(authz.ProductCMS, media.RoleCMSImage, editor))
	requireNotLinkable(t, err, image, media.RoleCMSImage, "legacy Media for another person")

	if _, _, err := f.attach(cmsService, image, request(authz.ProductCMS, media.RoleCMSImage, uploader)); err != nil {
		t.Fatalf("legacy Media for its uploader: %v", err)
	}
	if _, created, err := f.attach(cmsService, image, request(authz.ProductCMS, media.RoleCMSImage, editor)); err != nil || !created {
		t.Fatalf("legacy Media the CMS already holds, for another editor: created %v, err %v", created, err)
	}
	// The CMS holding it does not let Skyforms pin it.
	_, _, err = f.attach(formsService, image, request(authz.ProductForms, media.RoleFormsAnswer, editor))
	requireNotLinkable(t, err, image, media.RoleFormsAnswer, "legacy Media another product holds")
}

// The state rules of core's own links hold for another product's: no Media
// attachment to a Media that is archived, being purged or expired.
func TestServiceAttach_RefusesMediaThatCannotBeLinked(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	person := uuid.New()
	past := time.Now().Add(-time.Minute)
	create := func(change func(*media.Media)) media.Media {
		m := media.Media{Name: "logo.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(),
			UploadedBy: person, Purpose: "cms_image"}
		change(&m)
		created, err := f.store.Create(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}

	for name, m := range map[string]media.Media{
		"archived":     create(func(m *media.Media) { m.DeletedAt = &past }),
		"being purged": create(func(m *media.Media) { m.BlobPurgeStartedAt = &past }),
		"purged":       create(func(m *media.Media) { m.BlobPurgedAt = &past }),
		"expired":      create(func(m *media.Media) { m.ExpiresAt = &past }),
	} {
		_, _, err := f.attach(cmsService, m, request(authz.ProductCMS, media.RoleCMSImage, person))
		requireNotLinkable(t, err, m, media.RoleCMSImage, name)
	}
}

// A retried attach answers the Media attachment already there, even when the
// Media was archived since: the link exists, only new ones are refused.
func TestServiceAttach_SameLinkAgainAnswersTheExistingAttachment(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	person := uuid.New()
	m := f.stored(t, "cms_file", person)
	req := request(authz.ProductCMS, media.RoleCMSFile, person)
	first, created, err := f.attach(cmsService, m, req)
	if err != nil || !created {
		t.Fatalf("attach: created %v, err %v", created, err)
	}
	if err := f.store.Archive(ctx, m.ID, nil); err != nil {
		t.Fatal(err)
	}

	again, created, err := f.attach(cmsService, m, req)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("attach again: %+v created %v err %v; want %s", again, created, err, first.ID)
	}
}

// A product names one of its own roles, a record and the person it acts
// for. Owner ids are the product's own: a Skyforms response id, a CMS block
// id, or a CMS page as clientId:slug. Anything else is refused before the
// Media is looked at, legacy Media included.
func TestServiceAttach_RefusesUnknownRolesAndMalformedRequests(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	person := uuid.New()
	m := f.stored(t, "legacy", person)
	valid := func(change func(*media.AttachRequest)) media.AttachRequest {
		req := media.AttachRequest{Owner: media.Owner{Service: "cms", Type: "page", ID: "skylab-site:hakkimizda"}, Role: media.RoleCMSImage, OnBehalfOf: person}
		change(&req)
		return req
	}

	for name, role := range map[string]media.Role{"no such role": "cover", "another product's role": media.RoleFormsAnswer, "core's role": media.RoleEventCover, "empty": ""} {
		_, _, err := f.svc.Attach(ctx, cmsService, m.ID, valid(func(r *media.AttachRequest) { r.Role = role }))
		var refusal *media.RoleRefusal
		if !errors.Is(err, media.ErrRoleUnknown) || !errors.Is(err, media.ErrInvalid) || !errors.As(err, &refusal) || refusal.Role != role {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrRoleUnknown)
		}
	}
	for name, req := range map[string]media.AttachRequest{
		"no type":          valid(func(r *media.AttachRequest) { r.Owner.Type = "" }),
		"type with caps":   valid(func(r *media.AttachRequest) { r.Owner.Type = "Page" }),
		"type too long":    valid(func(r *media.AttachRequest) { r.Owner.Type = "p" + strings.Repeat("a", 64) }),
		"no id":            valid(func(r *media.AttachRequest) { r.Owner.ID = "" }),
		"id with a space":  valid(func(r *media.AttachRequest) { r.Owner.ID = "skylab-site:hakkımızda sayfası" }),
		"id with a query":  valid(func(r *media.AttachRequest) { r.Owner.ID = "home?draft=1" }),
		"id too long":      valid(func(r *media.AttachRequest) { r.Owner.ID = strings.Repeat("a", 201) }),
		"no acting person": valid(func(r *media.AttachRequest) { r.OnBehalfOf = uuid.Nil }),
	} {
		_, _, err := f.svc.Attach(ctx, cmsService, m.ID, req)
		if !errors.Is(err, media.ErrInvalid) || errors.Is(err, media.ErrRoleUnknown) || errors.Is(err, media.ErrNotLinkable) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrInvalid)
		}
	}
	if _, _, err := f.svc.Attach(ctx, cmsService, uuid.Nil, valid(func(*media.AttachRequest) {})); !errors.Is(err, media.ErrInvalid) {
		t.Errorf("no Media id: err = %v, want %v", err, media.ErrInvalid)
	}
	for _, id := range []string{"skylab-site:hakkimizda", "3f2b8c1e-0a4d-4a5e-9c7b-1d2e3f4a5b6c", "arge:projeler/yapay-zeka", strings.Repeat("a", 200)} {
		if _, _, err := f.svc.Attach(ctx, cmsService, m.ID, valid(func(r *media.AttachRequest) { r.Owner.ID = id })); err != nil {
			t.Errorf("owner id %q: %v", id, err)
		}
	}
}

// Only a product's service account manages Media attachments, and it is
// asked before anything in the request is looked at: a person is refused
// even with a request that makes no sense.
func TestServiceAttach_AuthorizesTheCallerFirst(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	person := authz.Principal{ID: uuid.NewString(), Roles: []string{"media:attach"}}

	for name, req := range map[string]media.AttachRequest{
		"empty request": {},
		"valid request": request(authz.ProductCMS, media.RoleCMSImage, uuid.New()),
	} {
		if _, _, err := f.svc.Attach(context.Background(), person, uuid.Nil, req); !errors.Is(err, media.ErrAttachForbidden) {
			t.Errorf("person, %s: err = %v, want %v", name, err, media.ErrAttachForbidden)
		}
	}
	if err := f.svc.Detach(context.Background(), person, uuid.Nil, uuid.Nil); !errors.Is(err, media.ErrAttachForbidden) {
		t.Errorf("person detaching: err = %v, want %v", err, media.ErrAttachForbidden)
	}
	_, _, err := f.svc.Attach(context.Background(), formsService, uuid.New(), request(authz.ProductCMS, media.RoleCMSImage, uuid.New()))
	if !errors.Is(err, media.ErrAttachWrongService) {
		t.Errorf("Skyforms attaching for the CMS: err = %v, want %v", err, media.ErrAttachWrongService)
	}
}

// A Media stays attached while any Media attachment keeps it. Removing the
// last one detaches it: a purposed Media is purged 30 days later unless
// something attaches it again, a legacy one is kept.
func TestServiceDetach_TheLastAttachmentStartsTheDetachedWindow(t *testing.T) {
	t.Parallel()
	f := newAttachFixture(t)
	ctx := context.Background()
	person := uuid.New()
	answer := f.stored(t, "answer_file", person)
	legacy := f.stored(t, "legacy", person)
	draft, _, err := f.attach(formsService, answer, request(authz.ProductForms, media.RoleFormsAnswer, person))
	if err != nil {
		t.Fatal(err)
	}
	response, _, err := f.attach(formsService, answer, request(authz.ProductForms, media.RoleFormsAnswer, person))
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := f.attach(formsService, legacy, request(authz.ProductForms, media.RoleFormsAnswer, person))
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
	person := uuid.New()
	m := f.stored(t, "legacy", person)
	page, _, err := f.attach(cmsService, m, request(authz.ProductCMS, media.RoleCMSImage, person))
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
