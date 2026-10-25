package consent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

// A withdrawal asked by mail to info@ (s6 §6) ends, for the purposes named,
// every open grant of the person holding the address: the grant a form
// recorded for their Personal e-mail, their account's own grant, and a
// grant for another of their addresses; it says staff ended them and keeps
// no address. Another purpose and another person stay. Running it again
// ends none.
func TestStaffWithdrawalEndsThePersonsGrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	member, other := uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO users (id, email, school_email) VALUES ($1, 'm@example.com', 'm@std.yildiz.edu.tr'), ($2, 'o@example.com', '')`, member, other)
	f.svc.SetAddresses(addressSource{member: {"m@example.com", "m@std.yildiz.edu.tr", "personal@example.org"}, other: {"o@example.com"}})
	f.svc.SetPersonalEmailHolders(personalHolders{"personal@example.org": {member}, "m@example.com": nil})
	for _, g := range []consent.Grant{
		{Purpose: consent.PurposePromotional, UserID: member, Source: consent.SourceSignup},
		{Purpose: consent.PurposePromotional, Email: "personal@example.org", Source: consent.SourceForms, ClientID: "forms", EmailVerified: true},
		{Purpose: consent.PurposeEventInvitations, Email: "m@std.yildiz.edu.tr", Source: consent.SourcePlace, ClientID: "place", EmailVerified: true},
		{Purpose: consent.PurposePromotional, UserID: other, Source: consent.SourceSelf},
	} {
		if _, err := f.svc.Grant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: other, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}

	ended, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional, consent.PurposeEventInvitations}, " Personal@Example.ORG ")
	if err != nil || ended[consent.PurposePromotional] != 2 || ended[consent.PurposeEventInvitations] != 1 {
		t.Fatalf("ended %v %v", ended, err)
	}
	if f.count(t, `ended_via = 'staff' AND ended_reason = 'withdrawn' AND email IS NULL`) != 3 {
		t.Fatal("the ended grants do not say staff ended them, or keep an address")
	}
	if f.count(t, `ended_at IS NULL AND user_id = $1`, other) != 2 {
		t.Fatal("another person's grants were ended")
	}
	again, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional}, "personal@example.org")
	if err != nil || again[consent.PurposePromotional] != 0 {
		t.Fatalf("again %v %v", again, err)
	}

	// One purpose only: the other stays.
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposePromotional, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(ctx, consent.Grant{Purpose: consent.PurposeEventInvitations, UserID: member, Source: consent.SourceSelf}); err != nil {
		t.Fatal(err)
	}
	if ended, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposeEventInvitations}, "m@example.com"); err != nil || ended[consent.PurposeEventInvitations] != 1 {
		t.Fatalf("one purpose %v %v", ended, err)
	}
	if f.count(t, `ended_at IS NULL AND user_id = $1 AND purpose = 'promotional'`, member) != 1 {
		t.Fatal("a purpose not named was ended")
	}

	// Keycloak unreachable: nothing ends, the operator runs it again.
	f.svc.SetPersonalEmailHolders(personalHolders{})
	if _, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional}, "personal@example.org"); !errors.Is(err, consent.ErrAddressesUnavailable) {
		t.Fatalf("without Keycloak: %v", err)
	}
	f.svc.SetPersonalEmailHolders(personalHolders{"personal@example.org": {member}, "m@example.com": nil})
	f.svc.SetAddresses(addressSource{})
	if _, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional}, "personal@example.org"); !errors.Is(err, consent.ErrAddressesUnavailable) {
		t.Fatalf("without the account's addresses: %v", err)
	}
	if f.count(t, `ended_at IS NULL AND user_id = $1 AND purpose = 'promotional'`, member) != 1 {
		t.Fatal("a failed staff withdrawal ended a grant")
	}

	for _, purposes := range [][]consent.Purpose{nil, {consent.PurposeRecruitmentPool}, {"newsletter"}} {
		if _, err := f.svc.WithdrawByStaff(ctx, purposes, "m@example.com"); err == nil {
			t.Errorf("purposes %v accepted", purposes)
		}
	}
	if _, err := f.svc.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional}, "not an address"); !errors.Is(err, consent.ErrInvalid) {
		t.Fatalf("bad address: %v", err)
	}
	off := consent.NewService(f.pool, consent.Config{}, nil)
	if _, err := off.WithdrawByStaff(ctx, []consent.Purpose{consent.PurposePromotional}, "m@example.com"); !errors.Is(err, consent.ErrDisabled) {
		t.Fatalf("off: %v", err)
	}
}
