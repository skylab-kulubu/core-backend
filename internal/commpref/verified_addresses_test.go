package commpref

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The guest-account-link read of the directory (docs/guest-account-links.md)
// matches exactly the recipient question's addresses: a verified Primary
// e-mail, a School e-mail of a YTÜ-linked account, a stamped Personal
// e-mail; never an unverified, unlinked or unstamped one, never a service
// account. An address two accounts hold is nobody's.
func TestVerifiedAddressesMatchTheRecipientRule(t *testing.T) {
	f := newRecipientsFixture(t)
	ctx := context.Background()
	addrs, err := ReadVerifiedAddresses(ctx, f.book, testYTUAlias)
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]struct {
		name string
		kind AddressKind
	}{
		"member@example.test":        {"member", AddressPrimary},
		"okul.adi@std.yildiz.edu.tr": {"ytu", AddressSchool},
		"kisisel@example.org":        {"personal", AddressPersonal},
	} {
		v, ok := addrs[address]
		owner, single := v.Owner()
		if !ok || !single || owner != f.p[want.name] || v.Kind != want.kind {
			t.Fatalf("%s: %+v %t", address, v, ok)
		}
	}
	for what, address := range map[string]string{
		"unverified primary": "unverified@example.test",
		"unlinked school":    "unlinked@std.yildiz.edu.tr",
		"unstamped personal": "damgasiz@example.org",
		"unreadable stamp":   "bozuk@example.org",
		"service account":    "service@example.test",
	} {
		if v, ok := addrs[address]; ok {
			t.Fatalf("%s matched: %+v", what, v)
		}
	}
	twin := addrs["ortak@example.test"]
	if _, ok := twin.Owner(); ok || len(twin.Holders) != 2 {
		t.Fatalf("an address two accounts hold has an owner: %+v", twin)
	}
	disabled := addrs["disabled@example.test"]
	if _, ok := disabled.Owner(); ok || len(disabled.Holders) != 1 || disabled.Enabled {
		t.Fatalf("a disabled account owns its address: %+v", disabled)
	}
}

// An account holding its address twice (Primary and Personal) stays its
// one holder, counted by the stronger kind.
func TestVerifiedAddressHeldTwiceIsOneHolder(t *testing.T) {
	f := newRecipientsFixture(t)
	f.edit("member", func(h *identity.AddressHolder) {
		h.PersonalEmail, h.PersonalEmailVerifiedAt = "MEMBER@example.test", "2026-09-21T14:13:20Z"
	})
	addrs, err := ReadVerifiedAddresses(context.Background(), f.book, testYTUAlias)
	if err != nil {
		t.Fatal(err)
	}
	v := addrs["member@example.test"]
	if owner, ok := v.Owner(); !ok || owner != f.p["member"] || v.Kind != AddressPrimary {
		t.Fatalf("%+v", v)
	}
}

// The cached lookup answers from the directory as the recipient question
// keeps it, and a fresh read replaces it at once.
func TestVerifiedAddressOfAndFreshRead(t *testing.T) {
	f := newRecipientsFixture(t)
	ctx := context.Background()
	v, ok, err := f.recipients.VerifiedAddressOf(ctx, "  Kisisel@Example.ORG ")
	if err != nil || !ok || v.Holders[0] != f.p["personal"] {
		t.Fatalf("%+v %t %v", v, ok, err)
	}
	late := f.add("verifiedNow", nil)
	if _, ok, _ := f.recipients.VerifiedAddressOf(ctx, "verifiednow@example.test"); ok {
		t.Fatal("a fresh directory read before it was asked for")
	}
	addrs, err := f.recipients.FreshVerifiedAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := addrs["verifiednow@example.test"].Owner(); !ok || owner != late {
		t.Fatalf("fresh read: %+v", addrs["verifiednow@example.test"])
	}
	if v, ok, err := f.recipients.VerifiedAddressOf(ctx, "verifiednow@example.test"); err != nil || !ok || v.Holders[0] != late {
		t.Fatalf("the fresh read was not kept: %+v %t %v", v, ok, err)
	}
	var none *Recipients
	if _, _, err := none.VerifiedAddressOf(ctx, "a@b.c"); err == nil {
		t.Fatal("no directory answered")
	}
	if _, err := none.FreshVerifiedAddresses(ctx); err == nil {
		t.Fatal("no directory answered")
	}
}
