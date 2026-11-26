package certificate

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Where a new Event template starts (EventTemplateInput.From): the layout
// the Event uses now, an empty one, or else a published team or club
// template's id.
const (
	EventTemplateFromCurrent = "current"
	EventTemplateFromBlank   = "blank"
)

// EventTemplateInput is the body of POST /v1/events/{id}/certificate-template.
// An empty From is EventTemplateFromCurrent.
type EventTemplateInput struct {
	From string `json:"from"`
}

// EventTemplateExistsError is a second Event template for an Event: the
// API answers 409 template_event_exists with the current one's id, which
// the panel opens instead.
type EventTemplateExistsError struct{ TemplateID uuid.UUID }

func (e *EventTemplateExistsError) Error() string {
	return "certificate: " + CodeTemplateEventExists + ": " + e.TemplateID.String()
}

func (e *EventTemplateExistsError) Is(target error) bool { return target == ErrConflict }

// EventTemplateState is GET /v1/events/{id}/certificate-template: the
// template the Event's certificates use now, its Event template if it has
// one, and the team and club templates it may be bound to instead.
type EventTemplateState struct {
	// Resolution is the template a certificate issued now takes (event
	// binding, team binding, club default); nil when none resolves.
	Resolution    *ResolvedTemplate     `json:"resolution"`
	EventTemplate *EventTemplateSummary `json:"eventTemplate"`
	Choices       []TemplateChoice      `json:"choices"`
}

// EventTemplateSummary is an Event template as the Event's certificate
// settings show it; PublishedVersion is nil while it is a draft only.
type EventTemplateSummary struct {
	ID               uuid.UUID `json:"id"`
	Name             string    `json:"name"`
	DraftUpdatedAt   time.Time `json:"draftUpdatedAt"`
	PublishedVersion *int      `json:"publishedVersion,omitempty"`
}

// TemplateChoice is a published team or club template the Event may be
// bound to: its name and latest version, never its layout.
type TemplateChoice struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerTeam string    `json:"ownerTeam"`
	System    bool      `json:"system"`
	Version   int       `json:"version"`
}

// canEditEvent is whether p may edit ev, the one decision every Event
// template rule reads (docs/certificate-event-templates.md): who may edit
// an Event may create, edit, publish and bind its Event template, and bind
// it to a team or club template. Co-organizers (ADR-0092) change who may
// edit an Event; Event templates follow by this decision alone.
func (s *service) canEditEvent(p authz.Principal, ev event.Event) bool {
	return s.authz.Allow(p, eventResource(ev, authz.TypeEvent), authz.Update)
}

// canReadEventTemplates is whether p may read ev's Event template and its
// template choices: who reads its certificate workspace, or edits it.
func (s *service) canReadEventTemplates(p authz.Principal, ev event.Event) bool {
	return s.canReadWorkspace(p, ev) || s.canEditEvent(p, ev)
}

// canWriteEventTemplate is whether p may take action (Create, Update,
// Assign) on ev's Event template or ev's binding: who may edit the Event,
// or whom the template rule of its organizer teams allows.
func (s *service) canWriteEventTemplate(p authz.Principal, ev event.Event, action authz.Action) bool {
	return s.canEditEvent(p, ev) || s.authz.Allow(p, eventResource(ev, authz.TypeCertificateTemplate), action)
}

func templateResource(ownerTeam string) authz.Resource {
	return authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: ownerTeam}
}

// templateEvent is the Event an Event template belongs to, ErrNotFound when
// p may not see it (a draft). An archived Event counts: its template stays.
func (s *service) templateEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) (event.Event, error) {
	ev, err := s.events.GetIncludingArchived(ctx, eventID)
	if err != nil {
		return event.Event{}, mapEventError(err)
	}
	if !event.VisibleTo(s.authz, &p, ev) {
		return event.Event{}, ErrNotFound
	}
	return ev, nil
}

// allowTemplate is nil when p may take action on item: an Event template
// by its Event's rule, any other by the template rule of its Owner team.
func (s *service) allowTemplate(ctx context.Context, p authz.Principal, item Template, action authz.Action) error {
	if item.EventID == nil {
		if !s.authz.Allow(p, templateResource(item.OwnerTeam), action) {
			return ErrForbidden
		}
		return nil
	}
	ev, err := s.templateEvent(ctx, p, *item.EventID)
	if err != nil {
		return err
	}
	allowed := s.canWriteEventTemplate(p, ev, action)
	if action == authz.Read {
		allowed = s.canReadEventTemplates(p, ev)
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// eventTemplateOf is ev's current Event template, ErrNotFound for none.
func (s *service) eventTemplateOf(ctx context.Context, eventID uuid.UUID) (Template, error) {
	items, err := s.templates.ListTemplates(ctx, TemplateFilter{EventID: &eventID})
	if err != nil {
		return Template{}, err
	}
	if len(items) == 0 {
		return Template{}, ErrNotFound
	}
	return items[0], nil
}

// fitsEvent reports whether a team or club template may serve ev: a
// current template of no Owner team or of ev's, the binding rule.
func fitsEvent(item Template, ev event.Event) bool {
	return item.EventID == nil && item.ArchivedAt == nil &&
		(item.OwnerTeam == "" || normalizeTeam(item.OwnerTeam) == normalizeTeam(ev.OwnerTeam))
}

func (s *service) EventTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EventTemplateState, error) {
	if s.templates == nil {
		return EventTemplateState{}, ErrInvalid
	}
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return EventTemplateState{}, err
	}
	if !s.canReadEventTemplates(p, ev) {
		return EventTemplateState{}, ErrForbidden
	}
	state := EventTemplateState{Choices: []TemplateChoice{}}
	resolved, err := s.resolveForEvent(ctx, ev)
	switch {
	case err == nil:
		projected := s.projectResolution(p, ev, resolved)
		state.Resolution = &projected
	case !errors.Is(err, ErrNotFound):
		return EventTemplateState{}, err
	}
	own, err := s.eventTemplateOf(ctx, ev.ID)
	switch {
	case err == nil:
		state.EventTemplate = &EventTemplateSummary{ID: own.ID, Name: own.Name, DraftUpdatedAt: own.UpdatedAt}
		if own.PublishedVersion != nil {
			version := own.PublishedVersion.Version
			state.EventTemplate.PublishedVersion = &version
		}
	case !errors.Is(err, ErrNotFound):
		return EventTemplateState{}, err
	}
	all, err := s.templates.ListTemplates(ctx, TemplateFilter{})
	if err != nil {
		return EventTemplateState{}, err
	}
	for _, item := range all {
		if item.PublishedVersion == nil || !fitsEvent(item, ev) {
			continue
		}
		state.Choices = append(state.Choices, TemplateChoice{
			ID: item.ID, Name: item.Name, OwnerTeam: item.OwnerTeam, System: item.System, Version: item.PublishedVersion.Version,
		})
	}
	return state, nil
}

func (s *service) CreateEventTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID, in EventTemplateInput) (Template, error) {
	if s.templates == nil {
		return Template{}, ErrInvalid
	}
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return Template{}, err
	}
	if !s.canWriteEventTemplate(p, ev, authz.Create) {
		return Template{}, ErrForbidden
	}
	if existing, err := s.eventTemplateOf(ctx, ev.ID); err == nil {
		return Template{}, &EventTemplateExistsError{TemplateID: existing.ID}
	} else if !errors.Is(err, ErrNotFound) {
		return Template{}, err
	}
	start, err := s.eventTemplateStart(ctx, p, ev, in.From)
	if err != nil {
		return Template{}, err
	}
	// The copy links the source's Media as they are: a Media may have
	// several links. Its fonts are the source's: one archived since, or of
	// a team the source's version was published for, stays as in the
	// source (it prints from the copy's own version later).
	if err := s.checkLayoutMedia(ctx, start.DraftLayout, Layout{}, start.DraftLayout, ev.OwnerTeam); err != nil {
		return Template{}, err
	}
	created, err := s.templates.CreateTemplate(ctx, Template{
		ID: uuid.New(), Name: eventTemplateName(ev.Name), OwnerTeam: normalizeTeam(ev.OwnerTeam), EventID: &ev.ID,
		SourceKind: start.SourceKind, SourceRef: start.SourceRef, SourceEditURL: start.SourceEditURL,
		DraftLayout: start.DraftLayout, CreatedBy: principalUUIDPtr(p),
	})
	if errors.Is(err, ErrConflict) {
		// Another request created it first.
		if existing, lookupErr := s.eventTemplateOf(ctx, ev.ID); lookupErr == nil {
			return Template{}, &EventTemplateExistsError{TemplateID: existing.ID}
		}
	}
	return created, err
}

// eventTemplateStart is the source and draft layout a new Event template of
// ev starts with (EventTemplateInput.From), as p may see the source.
func (s *service) eventTemplateStart(ctx context.Context, p authz.Principal, ev event.Event, from string) (Template, error) {
	switch strings.TrimSpace(from) {
	case EventTemplateFromBlank:
		return Template{SourceKind: "sky", DraftLayout: blankLayout()}, nil
	case "", EventTemplateFromCurrent:
		resolved, err := s.resolveForEvent(ctx, ev)
		if errors.Is(err, ErrNotFound) {
			return Template{SourceKind: "sky", DraftLayout: blankLayout()}, nil
		}
		if err != nil {
			return Template{}, err
		}
		return s.copiedStart(p, resolved.Template, resolved.Version.Layout), nil
	}
	id, err := uuid.Parse(strings.TrimSpace(from))
	if err != nil {
		return Template{}, ErrInvalid
	}
	item, err := s.templates.GetTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	// Only a template the Event may be bound to, as its choices list them;
	// any other is answered as if it did not exist.
	if item.PublishedVersion == nil || !fitsEvent(item, ev) {
		return Template{}, ErrNotFound
	}
	return s.copiedStart(p, item, item.PublishedVersion.Layout), nil
}

// copiedStart starts a copy of source with layout. Its reference and edit
// address (a Canva or Figma design) go with it only to who may read the
// source template: a member who edits the Event but not the team template
// gets the layout, not the link to the team's design.
func (s *service) copiedStart(p authz.Principal, source Template, layout Layout) Template {
	kind := source.SourceKind
	if kind == "" {
		kind = "sky"
	}
	start := Template{SourceKind: kind, DraftLayout: layout}
	if s.authz.Allow(p, templateResource(source.OwnerTeam), authz.Read) {
		start.SourceRef, start.SourceEditURL = source.SourceRef, source.SourceEditURL
	}
	return start
}

// eventTemplateName is "<Event name> sertifikası", cut to the 160 bytes a
// template name may take, on a character boundary.
func eventTemplateName(eventName string) string {
	const suffix = " sertifikası"
	name := strings.TrimSpace(eventName)
	if name == "" {
		return "Etkinlik sertifikası"
	}
	limit := 160 - len(suffix)
	if len(name) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimSpace(name[:cut])
	}
	return name + suffix
}

// blankLayout is an empty A4 landscape page with the participant's name,
// the Event's name and the verification QR, as the panel's new template
// starts.
func blankLayout() Layout {
	text := func(id, kind string, x, width, fontSize float64, weight int) Element {
		return Element{
			ID: id, Kind: kind, X: x, Y: 0, Width: width, Height: 64,
			FontFamily: "Arial", FontSize: fontSize, MinFontSize: 12, FontWeight: weight, LineHeight: 1.15,
			Color: "#111111", Align: "center", Fit: "shrink",
		}
	}
	name := text("recipientName-1", "recipientName", 140, 843, 48, 700)
	name.Y = 285
	eventName := text("eventName-1", "eventName", 180, 763, 26, 500)
	eventName.Y = 390
	return Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundColor: "#ffffff",
		Elements: []Element{
			name,
			eventName,
			{ID: "verificationQr-1", Kind: "verificationQr", X: 910, Y: 610, Width: 130, Height: 130},
		},
	}
}

// bindEventTemplate binds item's Event to it, once a version is published:
// a missing binding is set, another template's is switched. Publishing
// again sets it when an earlier publish stored the version but not the
// binding.
func (s *service) bindEventTemplate(ctx context.Context, p authz.Principal, item Template) error {
	key := item.EventID.String()
	current, err := s.templates.GetBinding(ctx, ScopeEvent, key)
	if err == nil && current.TemplateID == item.ID {
		return nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = s.templates.SetBinding(ctx, Binding{
		ID: uuid.New(), Scope: ScopeEvent, ScopeKey: key, TemplateID: item.ID, UpdatedBy: principalUUIDPtr(p),
	})
	return err
}
