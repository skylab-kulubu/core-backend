package ticketimage

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Events reads what the choice is decided on: the Event (its editors, its
// cover, its Owner team) and the teams whose Events use a Media (the Team
// media library). event.PostgresStore is one.
type Events interface {
	Get(ctx context.Context, id uuid.UUID) (event.Event, error)
	TeamsUsingMedia(ctx context.Context, mediaID, except uuid.UUID) ([]string, error)
}

// MediaReader reads an image's Media, for its address in the answer.
type MediaReader interface {
	Get(ctx context.Context, id uuid.UUID) (media.Media, error)
}

// Service answers GET and PUT /v1/events/{id}/ticket-image: whoever may
// edit the Event reads and writes its choice.
type Service struct {
	Store  Store
	Events Events
	Authz  authz.Authorizer
	// Linker checks a new image (purpose, current); nil checks only the
	// Team media library.
	Linker media.Linker
	// Media and Address give the image's address; nil leaves it out.
	Media   MediaReader
	Address func(key string) string
	// Now is the clock the Co-organizer people's rights are read at; nil
	// is time.Now.
	Now func() time.Time
}

// Band is the band's shape, for the panel's cropper.
type Band struct {
	WidthMM  int `json:"widthMm"`
	HeightMM int `json:"heightMm"`
}

// View is the answer: the choice, the image's address, and whether a
// cover region no longer applies (another cover: the band shows the new
// cover's middle).
type View struct {
	Source       Source     `json:"source"`
	MediaID      *uuid.UUID `json:"mediaId,omitempty"`
	MediaURL     string     `json:"mediaUrl,omitempty"`
	CoverMediaID *uuid.UUID `json:"coverMediaId,omitempty"`
	Crop         *Crop      `json:"crop,omitempty"`
	CoverChanged bool       `json:"coverChanged"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
	Band         Band       `json:"band"`
}

// Input is a new choice. CoverMediaID, when sent, must be the Event's
// cover; left out, it is.
type Input struct {
	Source       Source     `json:"source"`
	MediaID      *uuid.UUID `json:"mediaId"`
	CoverMediaID *uuid.UUID `json:"coverMediaId"`
	Crop         *Crop      `json:"crop"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// editable is the Event p may edit.
func (s *Service) editable(ctx context.Context, p authz.Principal, id uuid.UUID) (event.Event, error) {
	ev, err := s.Events.Get(ctx, id)
	if err != nil {
		return event.Event{}, err
	}
	if !s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeEvent, s.now()), authz.Update) {
		return event.Event{}, event.ErrForbidden
	}
	return ev, nil
}

// Get is the Event's choice.
func (s *Service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (View, error) {
	ev, err := s.editable(ctx, p, id)
	if err != nil {
		return View{}, err
	}
	setting, err := s.Store.Get(ctx, id)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, ev, setting), nil
}

// Set writes the Event's choice: SourceAuto forgets it, SourceCover takes a
// region of the cover, SourceUpload an image (of an Event purpose, the
// Team's) and its region.
func (s *Service) Set(ctx context.Context, p authz.Principal, id uuid.UUID, in Input) (View, error) {
	ev, err := s.editable(ctx, p, id)
	if err != nil {
		return View{}, err
	}
	next := Setting{Source: in.Source}
	switch in.Source {
	case SourceAuto:
	case SourceCover:
		// The region is drawn on the Event's saved cover: another one (a
		// cover not saved yet) is refused.
		cover := ev.CoverImageID
		if cover == nil || in.Crop == nil || !in.Crop.Valid() ||
			(in.CoverMediaID != nil && *in.CoverMediaID != *cover) {
			return View{}, ErrInvalid
		}
		next.CoverMediaID, next.Crop = cover, in.Crop
	case SourceUpload:
		if in.MediaID == nil || *in.MediaID == uuid.Nil || in.Crop == nil || !in.Crop.Valid() {
			return View{}, ErrInvalid
		}
		was, err := s.Store.Get(ctx, id)
		if err != nil {
			return View{}, err
		}
		// Only a new link is checked, as for the cover.
		if was.MediaID == nil || *was.MediaID != *in.MediaID {
			if err := s.checkMedia(ctx, ev, *in.MediaID); err != nil {
				return View{}, err
			}
		}
		next.MediaID, next.Crop = in.MediaID, in.Crop
	default:
		return View{}, ErrInvalid
	}
	saved, err := s.Store.Put(ctx, id, next)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, ev, saved), nil
}

// checkMedia is the cover's rule for a new image: the Media's own (purpose,
// current), then the Team media library's.
func (s *Service) checkMedia(ctx context.Context, ev event.Event, mediaID uuid.UUID) error {
	if s.Linker != nil {
		if err := s.Linker.CheckLink(ctx, mediaID, media.RoleEventTicketImage); err != nil {
			return err
		}
	}
	teams, err := s.Events.TeamsUsingMedia(ctx, mediaID, ev.ID)
	if err != nil {
		return err
	}
	for _, team := range teams {
		if team != ev.OwnerTeam {
			return &media.LinkRefusal{Err: event.ErrMediaTeamMismatch, MediaID: mediaID, Role: media.RoleEventTicketImage}
		}
	}
	return nil
}

func (s *Service) view(ctx context.Context, ev event.Event, setting Setting) View {
	if setting.Source == "" {
		setting.Source = SourceAuto
	}
	v := View{
		Source: setting.Source, MediaID: setting.MediaID, CoverMediaID: setting.CoverMediaID, Crop: setting.Crop,
		CoverChanged: setting.CoverChanged(ev.CoverImageID), UpdatedAt: setting.UpdatedAt,
		Band: Band{WidthMM: BandWidthMM, HeightMM: BandHeightMM},
	}
	if setting.MediaID != nil && s.Media != nil && s.Address != nil {
		if item, err := s.Media.Get(ctx, *setting.MediaID); err == nil && item.DeletedAt == nil && item.Key != "" {
			v.MediaURL = s.Address(item.Key)
		}
	}
	return v
}
