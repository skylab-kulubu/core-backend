package consent

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The team page objection (s6 row 4 b) is the person's own, on their
// account, and is about publishing on the web, not mails: no product
// records it and no mail route serves it. The team_page consent before it
// is no longer taken; its text stays for the rows kept as proof.
func TestTeamPageObjectionIsTheAccountsOwnAndNotMailed(t *testing.T) {
	purpose, err := ParsePurpose(" team_page_objection ")
	if err != nil || purpose != PurposeTeamPageObjection {
		t.Fatalf("parse %q %v", purpose, err)
	}
	if purpose.Mailed() || purpose.Channel() != ChannelWeb || purpose.Renews() {
		t.Fatalf("mailed %v channel %q renews %v", purpose.Mailed(), purpose.Channel(), purpose.Renews())
	}
	if version, ok := CurrentText(purpose); !ok || version != "ekip-sayfasi-itiraz-v1" {
		t.Fatalf("text %q %v", version, ok)
	}
	// Account Center's switch, label and description word for word
	// (src/lib/team-page-consent.ts): what the person turned off.
	const want = "Ekip sayfalarında görün. Adın, soyadın, profil fotoğrafın, LinkedIn adresin, üniversiten, fakülten, bölümün ve ekip liderliğin ekibinin SKY LAB uygulamasındaki sayfasında yer alır."
	if got := Text("ekip-sayfasi-itiraz-v1"); got != want {
		t.Fatalf("ekip-sayfasi-itiraz-v1 text %q", got)
	}
	own := Grant{Purpose: PurposeTeamPageObjection, UserID: uuid.New(), Source: SourceSelf}
	if err := own.Validate(); err != nil || own.TextVersion != "ekip-sayfasi-itiraz-v1" {
		t.Fatalf("own objection %+v %v", own, err)
	}
	for _, source := range []Source{SourceSignup, SourceForms, SourcePlace, SourceGuessr, SourceGuestApply} {
		g := Grant{Purpose: PurposeTeamPageObjection, Email: "a@example.com", Source: source, ClientID: string(source)}
		if err := g.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", source, err)
		}
	}
	if err := PurposeTeamPageObjection.mailedCheck(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mailed check %v", err)
	}

	// The consent of 2026-10-27: known, no longer taken, its text kept.
	if _, err := ParsePurpose("team_page"); !errors.Is(err, ErrPurposeNotEnabled) {
		t.Fatalf("team_page %v", err)
	}
	old := Grant{Purpose: PurposeTeamPage, UserID: uuid.New(), Source: SourceSelf}
	if err := old.Validate(); !errors.Is(err, ErrPurposeNotEnabled) {
		t.Fatalf("team_page grant %v", err)
	}
	const v1 = "Ekip sayfalarında görüneyim: adım, soyadım, profil fotoğrafım, LinkedIn adresim, üniversitem, fakültem, bölümüm ve ekip liderliğim ekibimin SKY LAB uygulamasındaki sayfasında yayımlansın."
	if got := Text("ekip-sayfasi-v1"); got != v1 {
		t.Fatalf("ekip-sayfasi-v1 text %q", got)
	}

	for _, mailed := range []Purpose{PurposeEventInvitations, PurposePromotional, PurposeRecruitmentPool} {
		if !mailed.Mailed() || mailed.Channel() != ChannelEmail {
			t.Fatalf("%s: mailed %v channel %q", mailed, mailed.Mailed(), mailed.Channel())
		}
	}
	if GuestApplyPurposes() != 1 {
		t.Fatalf("guest apply purposes %d", GuestApplyPurposes())
	}
}
