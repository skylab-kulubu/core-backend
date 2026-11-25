package ticketimage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
)

// fakeEvents holds one Event and the teams whose Events use each Media.
type fakeEvents struct {
	ev    event.Event
	teams map[uuid.UUID][]string
}

func (f *fakeEvents) Get(_ context.Context, id uuid.UUID) (event.Event, error) {
	if id != f.ev.ID {
		return event.Event{}, event.ErrNotFound
	}
	return f.ev, nil
}

func (f *fakeEvents) TeamsUsingMedia(_ context.Context, mediaID, _ uuid.UUID) ([]string, error) {
	return f.teams[mediaID], nil
}

// fakeLinker refuses the Media of a purpose that does not fit and counts
// its checks.
type fakeLinker struct {
	refuse  map[uuid.UUID]bool
	checked []media.Role
}

func (f *fakeLinker) CheckLink(_ context.Context, id uuid.UUID, role media.Role) error {
	f.checked = append(f.checked, role)
	if f.refuse[id] {
		return &media.LinkRefusal{Err: media.ErrPurposeMismatch, MediaID: id, Role: role}
	}
	return nil
}

type fakeMedia map[uuid.UUID]media.Media

func (f fakeMedia) Get(_ context.Context, id uuid.UUID) (media.Media, error) {
	if m, ok := f[id]; ok {
		return m, nil
	}
	return media.Media{}, media.ErrNotFound
}

var (
	leader   = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	stranger = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER"}}
)

func setup() (*ticketimage.Service, *fakeEvents, *fakeLinker, uuid.UUID, uuid.UUID) {
	cover, logo := uuid.New(), uuid.New()
	events := &fakeEvents{ev: event.Event{ID: uuid.New(), Name: "Gecekodu", OwnerTeam: "WEBLAB", CoverImageID: &cover}, teams: map[uuid.UUID][]string{}}
	linker := &fakeLinker{refuse: map[uuid.UUID]bool{}}
	svc := &ticketimage.Service{
		Store: &ticketimage.MemoryStore{}, Events: events, Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Linker: linker,
		Media:   fakeMedia{logo: {ID: logo, Key: "images/logo.png"}},
		Address: func(key string) string { return "https://cdn.example.test/" + key },
	}
	return svc, events, linker, cover, logo
}

// Whoever may edit the Event reads and writes its choice; anyone else is
// refused, and an Event that is not there is not found.
func TestOnlyEditorsChoose(t *testing.T) {
	t.Parallel()
	svc, events, _, _, _ := setup()
	ctx := t.Context()
	view, err := svc.Get(ctx, leader, events.ev.ID)
	if err != nil || view.Source != ticketimage.SourceAuto || view.Band.WidthMM != 182 || view.Band.HeightMM != 24 {
		t.Fatalf("view %+v %v", view, err)
	}
	if _, err := svc.Get(ctx, stranger, events.ev.ID); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("a member read it: %v", err)
	}
	crop := &ticketimage.Crop{X: 0, Y: 0, W: 1, H: 0.2}
	if _, err := svc.Set(ctx, stranger, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceCover, Crop: crop}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("a member wrote it: %v", err)
	}
	if _, err := svc.Get(ctx, leader, uuid.New()); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("another Event: %v", err)
	}
}

// A cover region is drawn on the Event's cover (or the one named); a new
// cover makes it no longer apply. Auto forgets the choice.
func TestCoverRegion(t *testing.T) {
	t.Parallel()
	svc, events, linker, cover, _ := setup()
	ctx := t.Context()
	crop := &ticketimage.Crop{X: 0.1, Y: 0.2, W: 0.8, H: 0.1}
	view, err := svc.Set(ctx, leader, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceCover, Crop: crop})
	if err != nil || view.Source != ticketimage.SourceCover || view.CoverMediaID == nil || *view.CoverMediaID != cover || *view.Crop != *crop || view.CoverChanged {
		t.Fatalf("view %+v %v", view, err)
	}
	if len(linker.checked) != 0 {
		t.Errorf("a cover region checked a link: %v", linker.checked)
	}
	newCover := uuid.New()
	events.ev.CoverImageID = &newCover
	if view, _ := svc.Get(ctx, leader, events.ev.ID); !view.CoverChanged {
		t.Error("a new cover did not show")
	}
	events.ev.CoverImageID = nil
	if _, err := svc.Set(ctx, leader, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceCover, Crop: crop}); !errors.Is(err, ticketimage.ErrInvalid) {
		t.Fatalf("a cover region without a cover: %v", err)
	}
	if view, err := svc.Set(ctx, leader, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceAuto, Crop: crop}); err != nil || view.Source != ticketimage.SourceAuto || view.Crop != nil {
		t.Fatalf("auto %+v %v", view, err)
	}
}

// An image of its own is checked as a new link (purpose, Team media
// library) once; the answer gives its address. A region out of the image
// or an unknown source is refused.
func TestUploadedImage(t *testing.T) {
	t.Parallel()
	svc, events, linker, _, logo := setup()
	ctx := t.Context()
	crop := &ticketimage.Crop{X: 0, Y: 0.4, W: 1, H: 0.13}
	in := ticketimage.Input{Source: ticketimage.SourceUpload, MediaID: &logo, Crop: crop}
	view, err := svc.Set(ctx, leader, events.ev.ID, in)
	if err != nil || view.MediaURL != "https://cdn.example.test/images/logo.png" || view.MediaID == nil || *view.MediaID != logo {
		t.Fatalf("view %+v %v", view, err)
	}
	if len(linker.checked) != 1 || linker.checked[0] != media.RoleEventTicketImage {
		t.Fatalf("checked %v", linker.checked)
	}
	in.Crop = &ticketimage.Crop{X: 0, Y: 0.5, W: 1, H: 0.13}
	if _, err := svc.Set(ctx, leader, events.ev.ID, in); err != nil || len(linker.checked) != 1 {
		t.Fatalf("a new region of the same image checked it again (%v): %v", linker.checked, err)
	}

	other := uuid.New()
	linker.refuse[other] = true
	var refusal *media.LinkRefusal
	if _, err := svc.Set(ctx, leader, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceUpload, MediaID: &other, Crop: crop}); !errors.As(err, &refusal) || !errors.Is(err, media.ErrPurposeMismatch) {
		t.Fatalf("a Media of another purpose: %v", err)
	}
	theirs := uuid.New()
	events.teams[theirs] = []string{"AIRLAB"}
	if _, err := svc.Set(ctx, leader, events.ev.ID, ticketimage.Input{Source: ticketimage.SourceUpload, MediaID: &theirs, Crop: crop}); !errors.Is(err, event.ErrMediaTeamMismatch) {
		t.Fatalf("another team's Media: %v", err)
	}

	for _, bad := range []ticketimage.Input{
		{Source: ticketimage.SourceUpload, Crop: crop},
		{Source: ticketimage.SourceUpload, MediaID: &logo},
		{Source: ticketimage.SourceUpload, MediaID: &logo, Crop: &ticketimage.Crop{X: 0.5, Y: 0, W: 0.6, H: 0.1}},
		{Source: "poster", MediaID: &logo, Crop: crop},
	} {
		if _, err := svc.Set(ctx, leader, events.ev.ID, bad); !errors.Is(err, ticketimage.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// Band names the image and the region the PDF cuts.
func TestSettingBand(t *testing.T) {
	t.Parallel()
	cover, logo, old := uuid.New(), uuid.New(), uuid.New()
	crop := &ticketimage.Crop{X: 0, Y: 0, W: 1, H: 0.1}
	for _, tc := range []struct {
		name  string
		s     ticketimage.Setting
		cover *uuid.UUID
		img   *uuid.UUID
		crop  bool
	}{
		{"auto", ticketimage.Setting{}, &cover, &cover, false},
		{"cover", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &cover, Crop: crop}, &cover, &cover, true},
		{"old cover", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &old, Crop: crop}, &cover, &cover, false},
		{"no cover", ticketimage.Setting{Source: ticketimage.SourceCover, CoverMediaID: &old, Crop: crop}, nil, nil, false},
		{"upload", ticketimage.Setting{Source: ticketimage.SourceUpload, MediaID: &logo, Crop: crop}, nil, &logo, true},
		{"upload gone", ticketimage.Setting{Source: ticketimage.SourceUpload, Crop: crop}, &cover, &cover, false},
	} {
		img, c := tc.s.Band(tc.cover)
		if (img == nil) != (tc.img == nil) || (img != nil && *img != *tc.img) || (c != nil) != tc.crop {
			t.Errorf("%s: %v %v", tc.name, img, c)
		}
	}
}
