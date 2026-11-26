package event

import (
	"context"
	"log"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
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
	// ImageAddressMode is where the sizes of the Event's images point
	// (MEDIA_IMAGE_ADDRESS_MODE), as for the Media JSON; empty is
	// media.AddressStoredSizes.
	ImageAddressMode media.AddressMode
	FormLinks        FormLinkSync
	// Media checks each Media an Event is about to link as its cover, in
	// its gallery, or among its files or videos. Nil leaves the Media's own
	// rules (purpose, state) to the database's guards; the Team media
	// library rule holds either way.
	Media media.Linker
	// Policy is the authorization policy az decides with: the Owner teams'
	// rules and the Privileged roots the Co-organizer checks read. Without
	// EventPermissions it is authz.DefaultPolicy().
	Policy authz.Policy
	// Teams finds a Co-organizer team's Group (docs/co-organizers.md); nil
	// finds none, so no team can be added.
	Teams authz.TeamTree
	// CoOrganizersWrite lets PUT /v1/events/{id}/organizers write
	// (EVENT_CO_ORGANIZERS_WRITE); off, it is refused
	// (ErrCoOrganizersDisabled) and GET still answers.
	CoOrganizersWrite bool
	// Now is the clock a Co-organizer person's end is read against; nil is
	// time.Now.
	Now func() time.Time
	// Audit records each change of an Event's Co-organizer list in the Team
	// audit record (docs/co-organizers.md); nil records none. Groups finds
	// the Event's Owner team's Group, the row's target; Audit needs it.
	Audit  teamaudit.Writer
	Groups TeamGroups
	// FormsLinked hears of the application and extra forms a create or an
	// update newly linked (formsfeed: the Unticketed respondents'
	// gathering, docs/form-respondents.md); nil hears nothing.
	FormsLinked FormsLinkedHook
}

// FormsLinkedHook is told, after the write committed, which forms an Event
// newly lists as its application or extra forms. It never fails the write.
type FormsLinkedHook interface {
	FormsLinked(ctx context.Context, e Event, formIDs []uuid.UUID)
}

func NewServiceWithOptions(store Store, az authz.Authorizer, options ServiceOptions) Service {
	policy := options.Policy
	if policy.EventPermissions == nil {
		policy = authz.DefaultPolicy()
	}
	return &service{
		store: store, authz: az, formLinks: options.FormLinks, media: options.Media,
		addresses: media.Addresses{Base: options.PublicBase, Mode: options.ImageAddressMode},
		policy:    policy, teams: options.Teams, coOrganizersWrite: options.CoOrganizersWrite, now: options.Now,
		formsLinked: options.FormsLinked,
		audit:       options.Audit, groups: options.Groups,
	}
}

// formLinksOf are the forms whose short links the Event manages: its
// application and extra forms and its feedback form.
func formLinksOf(e Event) []FormLink {
	links := applicationLinksOf(e)
	if feedback, ok := feedbackLink(e); ok {
		links = append(links, feedback)
	}
	return links
}

// applicationLinksOf are the Event's application and extra forms: the forms
// whose answers write Tickets.
func applicationLinksOf(e Event) []FormLink {
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

// ListsForm reports whether the Event's application form or one of its
// extra forms is formID, with or without an alias. Its feedback form is not
// listed: its answers write no Ticket.
func (e Event) ListsForm(formID uuid.UUID) bool {
	for _, link := range applicationLinksOf(e) {
		if link.FormID == formID {
			return true
		}
	}
	return false
}

// ApplicationFormIDs are the forms the Event lists (ListsForm), each once;
// never nil.
func ApplicationFormIDs(e Event) []uuid.UUID {
	out := []uuid.UUID{}
	for _, link := range applicationLinksOf(e) {
		if !slices.Contains(out, link.FormID) {
			out = append(out, link.FormID)
		}
	}
	return out
}

// linkedForms tells formsLinked which of e's forms were not in before (nil:
// every one, a new Event).
func (s *service) linkedForms(ctx context.Context, e Event, before map[uuid.UUID]bool) {
	if s.formsLinked == nil {
		return
	}
	var added []uuid.UUID
	for _, id := range ApplicationFormIDs(e) {
		if !before[id] {
			added = append(added, id)
		}
	}
	if len(added) > 0 {
		s.formsLinked.FormsLinked(ctx, e, added)
	}
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
