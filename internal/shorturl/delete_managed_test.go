package shorturl

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// dkMember is a DK member (a Privileged reader) as each mode sees them, with
// url:access for their own links. Where the mode reads Groups, the token
// still carries url:moderator: Keycloak may not have taken it off yet.
func dkMember(mode authz.RoleMode) authz.Principal {
	p := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:access"}}
	if mode != authz.RoleModeRoles {
		p.Groups = []string{"/UYELER/DK"}
		p.Roles = append(p.Roles, "url:moderator")
	}
	if mode != authz.RoleModeGroups {
		p.Roles = append(p.Roles, authz.RoleGlobalRead)
	}
	return p
}

// A form's or an event's link is deleted only by whoever may change someone
// else's link (url.moderateWrite): not its creator, and not a reader who
// reads every link. A reader still deletes their own personal links.
func TestDeleteManagedLinkNeedsModerateWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []authz.RoleMode{authz.RoleModeGroups, authz.RoleModeBoth, authz.RoleModeRoles} {
		policy := authz.DefaultPolicy()
		policy.RoleMode = mode
		svc := NewService(NewMemoryStore(), authz.NewAuthorizer(policy))
		dk := dkMember(mode)
		owner := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:access"}}
		moderator := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:moderator"}}

		formLink := func(by authz.Principal) URL {
			t.Helper()
			formID := uuid.New()
			actor := uuid.MustParse(by.ID)
			u, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), ActorID: &actor})
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			return u
		}

		dkForm := formLink(dk)
		if err := svc.Delete(ctx, dk, dkForm.ID); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: DK deletes their own form link: %v", mode, err)
		}
		if _, err := svc.Lookup(ctx, dkForm.Alias); err != nil {
			t.Errorf("%s: DK's form link stopped redirecting: %v", mode, err)
		}

		ownerForm := formLink(owner)
		if err := svc.Delete(ctx, owner, ownerForm.ID); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: creator deletes their form link: %v", mode, err)
		}
		if err := svc.Delete(ctx, moderator, ownerForm.ID); err != nil {
			t.Errorf("%s: moderator deletes a form link: %v", mode, err)
		}
		if err := svc.Delete(ctx, moderator, dkForm.ID); err != nil {
			t.Errorf("%s: moderator deletes DK's form link: %v", mode, err)
		}

		personal, err := svc.Create(ctx, dk, "https://example.com/dk", "")
		if err != nil {
			t.Fatalf("%s: DK creates a personal link: %v", mode, err)
		}
		if err := svc.Delete(ctx, dk, personal.ID); err != nil {
			t.Errorf("%s: DK deletes their own personal link: %v", mode, err)
		}
	}
}

// Restoring a form's or an event's link a moderator disabled takes the same
// decision as deleting it; renaming or retargeting it is refused to everyone
// here. A reader still restores their own personal link.
func TestRestoreManagedLinkNeedsModerateWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []authz.RoleMode{authz.RoleModeGroups, authz.RoleModeBoth, authz.RoleModeRoles} {
		policy := authz.DefaultPolicy()
		policy.RoleMode = mode
		svc := NewService(NewMemoryStore(), authz.NewAuthorizer(policy))
		dk := dkMember(mode)
		owner := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:access"}}
		moderator := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:moderator"}}

		disabledFormLink := func(by authz.Principal) URL {
			t.Helper()
			formID := uuid.New()
			actor := uuid.MustParse(by.ID)
			u, err := svc.EnsureFormLink(ctx, formsService, formID, FormLinkInput{URL: formTarget(formID), ActorID: &actor})
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			if err := svc.Delete(ctx, moderator, u.ID); err != nil {
				t.Fatalf("%s: moderator disables: %v", mode, err)
			}
			return u
		}

		dkForm := disabledFormLink(dk)
		if _, err := svc.Restore(ctx, dk, dkForm.ID); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: DK restores their own form link: %v", mode, err)
		}
		if _, err := svc.Lookup(ctx, dkForm.Alias); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: DK's disabled form link redirects again: %v", mode, err)
		}
		ownerForm := disabledFormLink(owner)
		if _, err := svc.Restore(ctx, owner, ownerForm.ID); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: creator restores their form link: %v", mode, err)
		}
		if _, err := svc.Restore(ctx, moderator, ownerForm.ID); err != nil {
			t.Errorf("%s: moderator restores a form link: %v", mode, err)
		}

		// Update refuses a rename or a new target of a managed link to
		// its creator, a reader included (unchanged).
		if _, err := svc.Update(ctx, owner, ownerForm.ID, "", "baska-ad"); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: creator renames their form link: %v", mode, err)
		}
		if _, err := svc.Restore(ctx, moderator, dkForm.ID); err != nil {
			t.Fatalf("%s: moderator restores DK's form link: %v", mode, err)
		}
		if _, err := svc.Update(ctx, dk, dkForm.ID, "https://example.com/x", ""); !errors.Is(err, ErrManaged) {
			t.Errorf("%s: DK retargets their form link: %v", mode, err)
		}

		personal, err := svc.Create(ctx, dk, "https://example.com/dk", "")
		if err != nil {
			t.Fatalf("%s: DK creates a personal link: %v", mode, err)
		}
		if err := svc.Delete(ctx, dk, personal.ID); err != nil {
			t.Fatalf("%s: DK deletes their own personal link: %v", mode, err)
		}
		if _, err := svc.Restore(ctx, dk, personal.ID); err != nil {
			t.Errorf("%s: DK restores their own personal link: %v", mode, err)
		}
	}
}
