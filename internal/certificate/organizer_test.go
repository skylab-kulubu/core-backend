package certificate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// A certificate of the Board's (YK), DK's or ADMIN's Event is the club's:
// the PDF's heading, the mail's "Düzenleyen" (OwnerTeam) and the verify
// and mine answers' organizerName say the club's name, and ownerTeam stays
// (Yusuf, 2026-11-23). Another team's says its code.
func TestCertificateOrganizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := authz.Principal{ID: "admin", Groups: []string{"/ADMIN"}}
	for team, want := range map[string]string{"YK": organizer.ClubName, "DK": organizer.ClubName, "ADMIN": organizer.ClubName, "ARTLAB": "ARTLAB"} {
		events, _, _, ticketSvc, certs, render, mailer, ev, tk, sessions, _ := artlabSetup(t, 0.75)
		ev.OwnerTeam = team
		if _, err := events.Update(ctx, ev); err != nil {
			t.Fatal(err)
		}
		checkInN(t, ticketSvc, admin, tk.ID, sessions, 6)
		issued, err := certs.RecomputeTicket(ctx, tk.ID)
		if err != nil || issued == nil {
			t.Fatalf("%s: %v %+v", team, err, issued)
		}
		if issued.OwnerTeam != team {
			t.Fatalf("%s: stored owner %q", team, issued.OwnerTeam)
		}
		if got := mailer.vars["OwnerTeam"]; got != want {
			t.Fatalf("%s: mail OwnerTeam %q, want %q", team, got, want)
		}
		heading := "<h1>SKY LAB · " + team + "</h1>"
		if want == organizer.ClubName {
			heading = "<h1>" + organizer.ClubName + "</h1>"
		}
		if !strings.Contains(render.html, heading) {
			t.Fatalf("%s: the PDF has no %q", team, heading)
		}
		public, err := certs.Verify(ctx, issued.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if public.OrganizerName != want || public.OwnerTeam != team {
			t.Fatalf("%s: verify organizer %q owner %q", team, public.OrganizerName, public.OwnerTeam)
		}
		mine, err := certs.Mine(ctx, authz.Principal{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").String()})
		if err != nil || len(mine) != 1 {
			t.Fatalf("%s: mine %v %+v", team, err, mine)
		}
		if mine[0].Event.OrganizerName != want || mine[0].Event.OwnerTeam != team {
			t.Fatalf("%s: mine organizer %q owner %q", team, mine[0].Event.OrganizerName, mine[0].Event.OwnerTeam)
		}
	}
}

// A template's ownerTeam element prints the organizer the same way.
func TestTemplateOwnerTeamElementNamesTheClub(t *testing.T) {
	t.Parallel()
	layout := certificate.Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []certificate.Element{
		{ID: "name", Kind: "recipientName", X: 10, Y: 60, Width: 400, Height: 40, FontSize: 16},
		{ID: "event", Kind: "eventName", X: 10, Y: 110, Width: 400, Height: 40, FontSize: 16},
		{ID: "qr", Kind: "verificationQr", X: 500, Y: 10, Width: 100, Height: 100},
		{ID: "team", Kind: "ownerTeam", X: 10, Y: 10, Width: 400, Height: 40, FontSize: 16},
	}}
	for team, want := range map[string]string{"YK": organizer.ClubName, "": organizer.ClubName, "WEBLAB": "WEBLAB"} {
		html, err := certificate.LayoutHTML(context.Background(), layout, certificate.PreviewData{OwnerTeam: team}, "https://skyl.app/c/X", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html, ">"+want+"</div>") {
			t.Fatalf("%q: no %q in %s", team, want, html)
		}
	}
}
