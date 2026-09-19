package certificate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func versionedLayout(label string) certificate.Layout {
	return certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundColor: "#ffffff",
		Elements: []certificate.Element{
			{ID: "label", Kind: "staticText", Text: label, X: 10, Y: 10, Width: 400, Height: 40, FontSize: 20, FontWeight: 500, Color: "#111111", Align: "left", Opacity: 1},
			{ID: "name", Kind: "recipientName", X: 100, Y: 200, Width: 800, Height: 80, FontSize: 48, FontWeight: 700, Color: "#111111", Align: "center", Opacity: 1},
			{ID: "event", Kind: "eventName", X: 100, Y: 320, Width: 800, Height: 60, FontSize: 28, FontWeight: 500, Color: "#111111", Align: "center", Opacity: 1},
			{ID: "qr", Kind: "verificationQr", X: 900, Y: 600, Width: 130, Height: 130, Opacity: 1},
		},
	}
}

func TestQueuedIssuancePinsVersionAndKeepsPublicDTOPrivate(t *testing.T) {
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	store := certificate.NewMemoryStore()
	authorizer := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketService := ticket.NewService(tickets, events, authorizer, users)
	render := &recRender{}
	service := certificate.NewServiceWithOptions(store, tickets, events, users, authorizer, render, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})

	ev, err := events.Create(ctx, event.Event{Name: "Gelecek Zirvesi", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	session, _ := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	ownerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "private@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	item, err := ticketService.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	if _, err := ticketService.CheckIn(ctx, leader, item.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := service.RecomputeTicket(ctx, item.ID); err != nil || got != nil {
		t.Fatalf("check-in must not issue: %v %+v", err, got)
	}

	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "figma", DraftLayout: versionedLayout("VERSION ONE")})
	if err != nil {
		t.Fatal(err)
	}
	versionOne, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finalize(ctx, leader, ev.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("active event finalization: %v", err)
	}
	ev.Active = false
	ev, err = events.Update(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}

	batch, err := service.Finalize(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if batch.TotalCount != 1 || batch.TemplateVersionID != versionOne.ID || render.html != "" {
		t.Fatalf("queued batch %+v render=%q", batch, render.html)
	}

	template.DraftLayout = versionedLayout("VERSION TWO")
	if _, err := store.UpdateTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "two"}); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ProcessNext(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	if !strings.Contains(render.html, "VERSION ONE") || strings.Contains(render.html, "VERSION TWO") {
		t.Fatalf("batch did not pin version: %s", render.html)
	}

	listed, err := service.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed: %v %+v", err, listed)
	}
	issued := listed[0]
	if len(issued.Serial) != 32 || issued.VerifyURL != "https://skyl.app/c/"+issued.Serial {
		t.Fatalf("serial/verify %q %q", issued.Serial, issued.VerifyURL)
	}
	public, err := service.Verify(ctx, issued.Serial)
	if err != nil || public.Status != "valid" {
		t.Fatalf("verify: %v %+v", err, public)
	}
	raw, _ := json.Marshal(public)
	for _, forbidden := range []string{"private@example.com", "ticketId", "ownerId", "templateVersionId"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public response leaks %q: %s", forbidden, raw)
		}
	}
	reissue, err := service.Reissue(ctx, leader, issued.Serial)
	if err != nil || reissue.Reason != "reissue" {
		t.Fatalf("queue reissue: %v %+v", err, reissue)
	}
	if processed, err := service.ProcessNext(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("process reissue: processed=%d err=%v", processed, err)
	}
	listed, err = service.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("reissued list: %v %+v", err, listed)
	}
	activeCount := 0
	for _, candidate := range listed {
		if candidate.RevokedAt == nil {
			activeCount++
			if candidate.Serial == issued.Serial {
				t.Fatal("reissue must create a new serial")
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("want exactly one active certificate, got %d", activeCount)
	}
	if _, err := service.Finalize(ctx, leader, ev.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("second finalization: %v", err)
	}
}

func TestLayoutValidationRejectsUnknownOrMissingRequiredElements(t *testing.T) {
	layout := versionedLayout("ok")
	layout.Elements[0].Kind = "rawHtml"
	if !errors.Is(certificate.ValidateLayout(layout), certificate.ErrInvalid) {
		t.Fatal("raw html must be rejected")
	}
	layout = versionedLayout("ok")
	layout.Elements = layout.Elements[:3]
	if !errors.Is(certificate.ValidateLayout(layout), certificate.ErrInvalid) {
		t.Fatal("verification QR must be required")
	}
}
