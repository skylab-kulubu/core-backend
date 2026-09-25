package event

import (
	"context"
	"log"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// FormLink is a form an event points at and the alias the event gave it.
type FormLink struct {
	FormID uuid.UUID
	URL    string
	Alias  string
	Label  string
}

// FormLinkSync keeps the short links of an event's forms in step with the
// event. The short-link service implements it.
type FormLinkSync interface {
	SyncEventForms(ctx context.Context, eventID uuid.UUID, links []FormLink) error
}

type ServiceOptions struct {
	PublicBase string
	FormLinks  FormLinkSync
	// Media checks each Media an Event is about to link as its cover or in
	// its gallery. Nil leaves the Media's own rules (purpose, state) to the
	// database's guards; the Team media library rule holds either way.
	Media media.Linker
}

func NewServiceWithOptions(store Store, az authz.Authorizer, options ServiceOptions) Service {
	return &service{store: store, authz: az, publicBase: options.PublicBase, formLinks: options.FormLinks, media: options.Media}
}

func formLinksOf(e Event) []FormLink {
	links := make([]FormLink, 0, len(e.ExtraFormURLs)+1)
	if id, ok := FormIDFromURL(e.FormURL); ok {
		links = append(links, FormLink{FormID: id, URL: e.FormURL, Alias: e.FormAlias, Label: e.Name})
	}
	for _, extra := range e.ExtraFormURLs {
		id, ok := FormIDFromURL(extra.URL)
		if !ok {
			continue
		}
		label := e.Name
		if name := strings.TrimSpace(extra.Label); name != "" {
			label = e.Name + " · " + name
		}
		links = append(links, FormLink{FormID: id, URL: extra.URL, Alias: extra.Alias, Label: label})
	}
	return links
}

// FormIDFromURL finds the form in a forms address: the first path segment
// that parses as a UUID.
func FormIDFromURL(raw string) (uuid.UUID, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, false
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if id, err := uuid.Parse(part); err == nil && id != uuid.Nil {
			return id, true
		}
	}
	return uuid.Nil, false
}

// syncFormLinks never fails the event write: a link that cannot be bound (an
// alias another page owns, say) leaves the form managing its own link.
func (s *service) syncFormLinks(ctx context.Context, eventID uuid.UUID, links []FormLink) {
	if s.formLinks == nil {
		return
	}
	if err := s.formLinks.SyncEventForms(ctx, eventID, links); err != nil {
		log.Printf("event %s: sync form links: %v", eventID, err)
	}
}
