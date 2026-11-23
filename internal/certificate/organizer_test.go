package certificate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// A certificate of the Board's (YK), DK's or ADMIN's Event is the club's:
// the PDF's heading, the mail's "Düzenleyen" (OwnerTeam) and the verify
// and mine answers' organizerName say the club's name, and ownerTeam stays
// (Yusuf, 2026-11-23). Another team's says its code (and Turkish name,
// TestCertificateHeadingNamesTheTeam).
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
		heading := "<h1>" + want + "</h1>"
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

// A team's certificate is headed "KOD · Türkçe ad" like every other place
// (organizer.Name), and a template's ownerTeam box that cannot hold it
// prints the code alone (data-short, read by the page's script). Not
// parallel: organizer.UseNames is the process's one setting.
func TestCertificateHeadingNamesTheTeam(t *testing.T) {
	organizer.UseNames(organizer.NewNames(func(context.Context) (map[string]string, error) {
		return map[string]string{"WEBLAB": "Web Geliştirme", "YK": "Yönetim Kurulu"}, nil
	}, time.Hour, nil))
	t.Cleanup(func() { organizer.UseNames(nil) })

	if html := certificate.HTML("WEBLAB", "Ada", "Etkinlik", "https://skyl.app/c/X", nil); !strings.Contains(html, "<h1>WEBLAB · Web Geliştirme</h1>") {
		t.Fatalf("team heading: %s", html)
	}
	if html := certificate.HTML("YK", "Ada", "Etkinlik", "https://skyl.app/c/X", nil); !strings.Contains(html, "<h1>"+organizer.ClubName+"</h1>") {
		t.Fatalf("club heading: %s", html)
	}

	for _, fit := range []string{"", "shrink"} {
		layout := certificate.Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 10, Y: 60, Width: 400, Height: 40, FontSize: 16},
			{ID: "event", Kind: "eventName", X: 10, Y: 110, Width: 400, Height: 40, FontSize: 16},
			{ID: "qr", Kind: "verificationQr", X: 500, Y: 10, Width: 100, Height: 100},
			{ID: "team", Kind: "ownerTeam", X: 10, Y: 10, Width: 80, Height: 20, FontSize: 16, Fit: fit},
		}}
		for team, want := range map[string]string{
			"WEBLAB": ` data-short="WEBLAB" data-font="16" style=`,
			"YK":     `>` + organizer.ClubName + `</div>`,
			"ARTLAB": `>ARTLAB</div>`,
		} {
			html, err := certificate.LayoutHTML(context.Background(), layout, certificate.PreviewData{OwnerTeam: team}, "https://skyl.app/c/X", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(html, want) {
				t.Fatalf("%s fit %q: no %q in %s", team, fit, want, html)
			}
			if team == "WEBLAB" && !strings.Contains(html, ">WEBLAB · Web Geliştirme</div>") {
				t.Fatalf("fit %q: the full name is not printed first: %s", fit, html)
			}
			if team != "WEBLAB" && strings.Contains(html, "data-short=") {
				t.Fatalf("%s fit %q: a short form where the name is already short: %s", team, fit, html)
			}
		}
	}
}

// The code printed in place of a name that does not fit keeps the designed
// font size: the box carries it (data-font) and the page's script puts it
// back before fitting, instead of clearing it to the browser's 16px. Not
// parallel: organizer.UseNames is the process's one setting.
func TestOwnerTeamShortFormKeepsTheDesignedFontSize(t *testing.T) {
	organizer.UseNames(organizer.NewNames(func(context.Context) (map[string]string, error) {
		return map[string]string{"WEBLAB": "Web Geliştirme"}, nil
	}, time.Hour, nil))
	t.Cleanup(func() { organizer.UseNames(nil) })

	for _, fit := range []string{"", "shrink"} {
		layout := certificate.Layout{Width: 800, Height: 600, Orientation: "landscape", Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 10, Y: 60, Width: 400, Height: 40, FontSize: 16},
			{ID: "event", Kind: "eventName", X: 10, Y: 110, Width: 400, Height: 40, FontSize: 16},
			{ID: "qr", Kind: "verificationQr", X: 500, Y: 10, Width: 100, Height: 100},
			{ID: "team", Kind: "ownerTeam", X: 10, Y: 10, Width: 80, Height: 30, FontSize: 26, Fit: fit},
		}}
		html, err := certificate.LayoutHTML(context.Background(), layout, certificate.PreviewData{OwnerTeam: "WEBLAB"}, "https://skyl.app/c/X", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html, ` data-short="WEBLAB" data-font="26" style="`) || !strings.Contains(html, "font-size:26px;") {
			t.Fatalf("fit %q: the box does not carry its designed size: %s", fit, html)
		}
		script := html[strings.Index(html, "<script>"):]
		if strings.Contains(script, "fontSize=''") || strings.Contains(script, `fontSize=""`) {
			t.Fatalf("fit %q: the script clears the designed size: %s", fit, script)
		}
		short := script[strings.Index(script, "[data-short]"):]
		restore, code := strings.Index(short, "el.style.fontSize=el.dataset.font+'px'"), strings.Index(short, "el.textContent=el.dataset.short")
		if restore < 0 || code < 0 || restore > code || !strings.Contains(short[code:], "fit(el)") {
			t.Fatalf("fit %q: the size is not restored before the code is printed and fitted: %s", fit, short)
		}
	}
}
