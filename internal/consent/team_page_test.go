package consent

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The team page consent (s6 row 4 b) is the person's own, on their account,
// and allows publishing on the web, not mails: no product gives it and no
// mail route serves it.
func TestTeamPagePurposeIsTheAccountsOwnAndNotMailed(t *testing.T) {
	purpose, err := ParsePurpose(" team_page ")
	if err != nil || purpose != PurposeTeamPage {
		t.Fatalf("parse %q %v", purpose, err)
	}
	if purpose.Mailed() || purpose.Channel() != ChannelWeb || purpose.Renews() {
		t.Fatalf("mailed %v channel %q renews %v", purpose.Mailed(), purpose.Channel(), purpose.Renews())
	}
	if version, ok := CurrentText(purpose); !ok || version != "ekip-sayfasi-v1" || Text(version) == "" {
		t.Fatalf("text %q %v", version, ok)
	}
	// Account Center's switch label (src/lib/team-page-consent.ts) and the
	// Açık Rıza Metni 5.1, word for word: the pages are the app's only, the
	// site shows none (Yusuf, 2026-10-31).
	const want = "Ekip sayfalarında görüneyim: adım, soyadım, profil fotoğrafım, LinkedIn adresim, üniversitem, fakültem, bölümüm ve ekip liderliğim ekibimin SKY LAB uygulamasındaki sayfasında yayımlansın."
	if got := Text("ekip-sayfasi-v1"); got != want {
		t.Fatalf("ekip-sayfasi-v1 text %q", got)
	}
	for _, mailed := range []Purpose{PurposeEventInvitations, PurposePromotional, PurposeRecruitmentPool} {
		if !mailed.Mailed() || mailed.Channel() != ChannelEmail {
			t.Fatalf("%s: mailed %v channel %q", mailed, mailed.Mailed(), mailed.Channel())
		}
	}
	own := Grant{Purpose: PurposeTeamPage, UserID: uuid.New(), Source: SourceSelf}
	if err := own.Validate(); err != nil || own.TextVersion != "ekip-sayfasi-v1" {
		t.Fatalf("own grant %+v %v", own, err)
	}
	for _, source := range []Source{SourceSignup, SourceForms, SourcePlace, SourceGuessr, SourceGuestApply} {
		g := Grant{Purpose: PurposeTeamPage, Email: "a@example.com", Source: source, ClientID: string(source)}
		if err := g.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", source, err)
		}
	}
	if err := PurposeTeamPage.mailedCheck(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mailed check %v", err)
	}
	if GuestApplyPurposes() != 1 {
		t.Fatalf("guest apply purposes %d", GuestApplyPurposes())
	}
}
