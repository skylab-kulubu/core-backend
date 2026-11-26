package certificate

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// ListTemplates lists the team and club templates p may read; with
// filter.EventID, that Event's Event template alone (an Event template is
// never among the others: docs/certificate-event-templates.md).
func (s *service) ListTemplates(ctx context.Context, p authz.Principal, filter TemplateFilter) ([]Template, error) {
	if s.templates == nil {
		return nil, ErrInvalid
	}
	if filter.EventID != nil {
		ev, err := s.templateEvent(ctx, p, *filter.EventID)
		if err != nil {
			return nil, err
		}
		if !s.canReadEventTemplates(p, ev) {
			return nil, ErrForbidden
		}
		return s.templates.ListTemplates(ctx, TemplateFilter{EventID: &ev.ID})
	}
	all, err := s.templates.ListTemplates(ctx, TemplateFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(all))
	for _, item := range all {
		if s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: item.OwnerTeam}, authz.Read) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *service) GetTemplate(ctx context.Context, p authz.Principal, id uuid.UUID) (Template, error) {
	if s.templates == nil {
		return Template{}, ErrInvalid
	}
	item, err := s.templates.GetTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	if err := s.allowTemplate(ctx, p, item, authz.Read); err != nil {
		return Template{}, err
	}
	return item, nil
}

func (s *service) CreateTemplate(ctx context.Context, p authz.Principal, in TemplateDraft) (Template, error) {
	if s.templates == nil {
		return Template{}, ErrInvalid
	}
	if err := checkTemplateDraft(in); err != nil {
		return Template{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: in.OwnerTeam}, authz.Create) {
		return Template{}, ErrForbidden
	}
	if err := s.checkLayoutMedia(ctx, in.Layout, Layout{}); err != nil {
		return Template{}, err
	}
	item := Template{
		ID: uuid.New(), Name: strings.TrimSpace(in.Name), OwnerTeam: normalizeTeam(in.OwnerTeam),
		SourceKind: in.SourceKind, SourceRef: strings.TrimSpace(in.SourceRef), SourceEditURL: strings.TrimSpace(in.SourceEditURL),
		DraftLayout: in.Layout, CreatedBy: principalUUIDPtr(p),
	}
	return s.templates.CreateTemplate(ctx, item)
}

func (s *service) UpdateTemplate(ctx context.Context, p authz.Principal, id uuid.UUID, in TemplateDraft) (Template, error) {
	if s.templates == nil {
		return Template{}, ErrInvalid
	}
	if err := checkTemplateDraft(in); err != nil {
		return Template{}, err
	}
	existing, err := s.templates.GetTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	if err := s.allowTemplate(ctx, p, existing, authz.Update); err != nil {
		return Template{}, err
	}
	// An Event template's Owner team is its Event's: the body's is ignored.
	if existing.EventID == nil {
		if !s.authz.Allow(p, templateResource(in.OwnerTeam), authz.Update) {
			return Template{}, ErrForbidden
		}
		existing.OwnerTeam = normalizeTeam(in.OwnerTeam)
	}
	if err := s.checkLayoutMedia(ctx, in.Layout, existing.DraftLayout); err != nil {
		return Template{}, err
	}
	existing.Name = strings.TrimSpace(in.Name)
	existing.SourceKind = in.SourceKind
	existing.SourceRef = strings.TrimSpace(in.SourceRef)
	existing.SourceEditURL = strings.TrimSpace(in.SourceEditURL)
	existing.DraftLayout = in.Layout
	return s.templates.UpdateTemplate(ctx, existing)
}

// checkLayoutMedia checks each Media the layout links that the previous
// draft did not: a certificate asset, or a legacy Media.
func (s *service) checkLayoutMedia(ctx context.Context, layout, previous Layout) error {
	if s.media == nil {
		return nil
	}
	linked := make(map[uuid.UUID]bool)
	for _, id := range layoutAssetIDs(previous) {
		linked[id] = true
	}
	for _, id := range layoutAssetIDs(layout) {
		if linked[id] {
			continue
		}
		if err := s.media.CheckLink(ctx, id, media.RoleCertificateAsset); err != nil {
			return err
		}
	}
	return nil
}

func (s *service) PublishTemplate(ctx context.Context, p authz.Principal, id uuid.UUID) (TemplateVersion, error) {
	item, err := s.GetTemplate(ctx, p, id)
	if err != nil {
		return TemplateVersion{}, err
	}
	if err := s.allowTemplate(ctx, p, item, authz.Update); err != nil {
		return TemplateVersion{}, err
	}
	if err := ValidateLayout(item.DraftLayout); err != nil {
		return TemplateVersion{}, err
	}
	if s.render == nil {
		return TemplateVersion{}, rendererUnavailable(nil)
	}
	sample := PreviewData{
		RecipientName: "Örnek Katılımcı",
		EventName:     "SKY LAB Etkinliği",
		OwnerTeam:     item.OwnerTeam,
		Serial:        "ÖRNEK-2026",
		EventDates:    "19.09.2026 – 20.09.2026",
		IssueDate:     time.Now().Format("02.01.2006"),
	}
	versionID := uuid.New()
	manifest, err := s.snapshotVersionAssets(ctx, versionID, item.DraftLayout)
	if err != nil {
		return TemplateVersion{}, err
	}
	published, err := s.createVersion(ctx, p, item, sample, TemplateVersion{
		ID: versionID, TemplateID: item.ID, Layout: item.DraftLayout, AssetManifest: manifest, AssetServingPolicyApplied: true,
	})
	if err != nil && s.versionNotStored(ctx, versionID) {
		// The private copies are this attempt's own; nothing refers to them.
		s.discardPrivateCopies(ctx, privateCopyKeys(manifest))
	}
	if err != nil || item.EventID == nil {
		return published, err
	}
	// An Event template serves its Event once published. A binding that
	// fails leaves the version; publishing again binds it.
	if err := s.bindEventTemplate(ctx, p, item); err != nil {
		return TemplateVersion{}, err
	}
	return published, nil
}

// versionNotStored reports whether the version is provably absent. A store
// that answered an error may still have committed it (the answer was lost):
// its copies are then in use, and only an ErrNotFound lets them go.
func (s *service) versionNotStored(ctx context.Context, versionID uuid.UUID) bool {
	_, err := s.templates.GetVersion(context.WithoutCancel(ctx), versionID)
	return errors.Is(err, ErrNotFound)
}

func (s *service) createVersion(ctx context.Context, p authz.Principal, item Template, sample PreviewData, version TemplateVersion) (TemplateVersion, error) {
	if preview, err := s.renderLayoutPDF(ctx, item.DraftLayout, sample, s.verifyURL(sample.Serial), s.assetsForVersion(version)); err != nil || len(preview) == 0 {
		if err != nil {
			return TemplateVersion{}, err
		}
		return TemplateVersion{}, invalid(CodeRenderFailed)
	}
	checksum, err := LayoutChecksum(item.DraftLayout)
	if err != nil {
		return TemplateVersion{}, err
	}
	version.Checksum = checksum
	version.PublishedBy = principalUUIDPtr(p)
	return s.templates.CreateVersion(ctx, version)
}

func (s *service) PreviewTemplate(ctx context.Context, p authz.Principal, id uuid.UUID, sample PreviewData) ([]byte, error) {
	item, err := s.GetTemplate(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if s.render == nil {
		return nil, rendererUnavailable(nil)
	}
	if sample.RecipientName == "" {
		sample.RecipientName = "Ada Lovelace"
	}
	if sample.EventName == "" {
		sample.EventName = "SKY LAB Etkinliği"
	}
	if sample.OwnerTeam == "" {
		sample.OwnerTeam = item.OwnerTeam
	}
	if sample.Serial == "" {
		sample.Serial = "ÖRNEK-2026"
	}
	if sample.IssueDate == "" {
		sample.IssueDate = time.Now().Format("02.01.2006")
	}
	return s.renderLayoutPDF(ctx, item.DraftLayout, sample, s.verifyURL(sample.Serial), s.decrypted(s.assets))
}

func (s *service) PreviewEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]byte, error) {
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return nil, err
	}
	if !s.canReadEventTemplates(p, ev) {
		return nil, ErrForbidden
	}
	if s.render == nil {
		return nil, rendererUnavailable(nil)
	}
	resolved, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return nil, err
	}
	sample := PreviewData{
		RecipientName: "Örnek Katılımcı",
		EventName:     ev.Name,
		EventDates:    eventDates(ev),
		IssueDate:     time.Now().Format("02.01.2006"),
		OwnerTeam:     ev.OwnerTeam,
		Serial:        "ÖRNEK-2026",
	}
	return s.renderLayoutPDF(ctx, resolved.Version.Layout, sample, s.verifyURL(sample.Serial), s.assetsForVersion(resolved.Version))
}

func (s *service) ResolveTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID) (ResolvedTemplate, error) {
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return ResolvedTemplate{}, err
	}
	if !s.canReadEventTemplates(p, ev) {
		return ResolvedTemplate{}, ErrForbidden
	}
	resolved, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return ResolvedTemplate{}, err
	}
	return s.projectResolution(p, ev, resolved), nil
}

// projectResolution is resolved in full to who may read its template (an
// Event template: who may read ev's), else its names and version only.
func (s *service) projectResolution(p authz.Principal, ev event.Event, resolved ResolvedTemplate) ResolvedTemplate {
	full := s.authz.Allow(p, templateResource(resolved.Template.OwnerTeam), authz.Read)
	if resolved.Template.EventID != nil && *resolved.Template.EventID == ev.ID {
		full = s.canReadEventTemplates(p, ev)
	}
	if full {
		return resolved
	}
	resolved.Template = Template{
		ID:         resolved.Template.ID,
		Name:       resolved.Template.Name,
		OwnerTeam:  resolved.Template.OwnerTeam,
		EventID:    resolved.Template.EventID,
		SourceKind: resolved.Template.SourceKind,
		System:     resolved.Template.System,
	}
	resolved.Version = TemplateVersion{
		ID:          resolved.Version.ID,
		TemplateID:  resolved.Version.TemplateID,
		Version:     resolved.Version.Version,
		Checksum:    resolved.Version.Checksum,
		PublishedAt: resolved.Version.PublishedAt,
	}
	return resolved
}

func (s *service) ListBindings(ctx context.Context, p authz.Principal) ([]Binding, error) {
	if s.templates == nil {
		return nil, ErrInvalid
	}
	bindings, err := s.templates.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		owner, _, ev, err := s.bindingScope(ctx, p, binding.Scope, binding.ScopeKey)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.canAssign(p, owner, ev) {
			out = append(out, binding)
		}
	}
	return out, nil
}

func (s *service) resolveForEvent(ctx context.Context, ev event.Event) (ResolvedTemplate, error) {
	if s.templates == nil {
		return ResolvedTemplate{}, ErrInvalid
	}
	candidates := []struct{ scope, key, source string }{
		{ScopeEvent, ev.ID.String(), SourceEvent},
		{ScopeOwnerTeam, normalizeTeam(ev.OwnerTeam), SourceOwnerTeam},
		{ScopeClub, "SKY_LAB", SourceClubDefault},
	}
	for _, candidate := range candidates {
		if candidate.scope == ScopeOwnerTeam && candidate.key == "" {
			continue
		}
		binding, err := s.templates.GetBinding(ctx, candidate.scope, candidate.key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return ResolvedTemplate{}, err
		}
		item, err := s.templates.GetTemplate(ctx, binding.TemplateID)
		if err != nil {
			return ResolvedTemplate{}, err
		}
		version, err := s.templates.LatestVersion(ctx, item.ID)
		if err != nil {
			return ResolvedTemplate{}, err
		}
		return ResolvedTemplate{Template: item, Version: version, Source: candidate.source}, nil
	}
	return ResolvedTemplate{}, ErrNotFound
}

func (s *service) SetBinding(ctx context.Context, p authz.Principal, in Binding) (Binding, error) {
	if s.templates == nil || in.TemplateID == uuid.Nil || !validScope(in.Scope) {
		return Binding{}, ErrInvalid
	}
	item, err := s.templates.GetTemplate(ctx, in.TemplateID)
	if err != nil {
		return Binding{}, err
	}
	owner, key, ev, err := s.bindingScope(ctx, p, in.Scope, in.ScopeKey)
	if err != nil {
		return Binding{}, err
	}
	switch {
	case item.EventID != nil:
		// An Event template serves its own Event only.
		if in.Scope != ScopeEvent || key != item.EventID.String() {
			return Binding{}, invalid(CodeTemplateEventScoped)
		}
	case item.OwnerTeam != "" && normalizeTeam(item.OwnerTeam) != normalizeTeam(owner):
		// An Event's binding may also take a template of one of its
		// Co-organizer teams shown outside the panel (co_organizer, never a
		// helper; docs/co-organizers.md), for who may read that team's
		// templates; any other team's stays refused as before.
		if ev == nil || !coOrganizerTeamOf(*ev, item.OwnerTeam) {
			return Binding{}, ErrInvalid
		}
		if !s.authz.Allow(p, templateResource(item.OwnerTeam), authz.Read) {
			return Binding{}, ErrForbidden
		}
	}
	if !s.canAssign(p, owner, ev) {
		return Binding{}, ErrForbidden
	}
	if _, err := s.templates.LatestVersion(ctx, item.ID); err != nil {
		return Binding{}, err
	}
	in.ID = uuid.New()
	in.ScopeKey = key
	in.UpdatedBy = principalUUIDPtr(p)
	return s.templates.SetBinding(ctx, in)
}

func (s *service) ClearBinding(ctx context.Context, p authz.Principal, scope, scopeKey string) error {
	if s.templates == nil || !validScope(scope) || scope == ScopeClub {
		return ErrInvalid
	}
	owner, key, ev, err := s.bindingScope(ctx, p, scope, scopeKey)
	if err != nil {
		return err
	}
	if !s.canAssign(p, owner, ev) {
		return ErrForbidden
	}
	return s.templates.DeleteBinding(ctx, scope, key)
}

// canAssign is whether p may set or clear a binding of owner's scope: the
// template rule's Assign, and for an Event's binding (ev) who may edit the
// Event too.
func (s *service) canAssign(p authz.Principal, owner string, ev *event.Event) bool {
	if ev != nil {
		return s.canEditEvent(p, *ev) || s.authz.Allow(p, eventResource(*ev, authz.TypeCertificateTemplate), authz.Assign)
	}
	return s.authz.Allow(p, templateResource(owner), authz.Assign)
}

// bindingScope is the Owner team and the stored key of a binding's scope,
// and for an Event scope its Event. An Event scope names a current Event p
// may see: a draft p may not see is ErrNotFound, like its other certificate
// paths.
func (s *service) bindingScope(ctx context.Context, p authz.Principal, scope, key string) (string, string, *event.Event, error) {
	switch scope {
	case ScopeClub:
		return "", "SKY_LAB", nil, nil
	case ScopeOwnerTeam:
		team := normalizeTeam(key)
		if team == "" {
			return "", "", nil, ErrInvalid
		}
		return team, team, nil, nil
	case ScopeEvent:
		id, err := uuid.Parse(key)
		if err != nil {
			return "", "", nil, ErrInvalid
		}
		ev, err := s.visibleEvent(ctx, p, id)
		if err != nil {
			return "", "", nil, err
		}
		return ev.OwnerTeam, id.String(), &ev, nil
	default:
		return "", "", nil, ErrInvalid
	}
}

// checkTemplateDraft is nil for a draft that may be saved, else an
// InvalidError that says what to change.
func checkTemplateDraft(in TemplateDraft) error {
	if strings.TrimSpace(in.Name) == "" || len(strings.TrimSpace(in.Name)) > 160 {
		return invalid(CodeTemplateNameInvalid)
	}
	switch in.SourceKind {
	case "upload", "canva", "figma", "sky":
	default:
		return invalid(CodeTemplateSourceInvalid)
	}
	if len(in.SourceRef) > 500 || len(in.SourceEditURL) > 2000 {
		return invalid(CodeTemplateSourceInvalid)
	}
	if strings.TrimSpace(in.SourceEditURL) != "" {
		parsed, err := url.ParseRequestURI(strings.TrimSpace(in.SourceEditURL))
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return invalid(CodeTemplateSourceInvalid)
		}
	}
	return ValidateLayout(in.Layout)
}

func validScope(scope string) bool {
	return scope == ScopeClub || scope == ScopeOwnerTeam || scope == ScopeEvent
}

func principalUUID(p authz.Principal) uuid.UUID {
	id, _ := uuid.Parse(p.ID)
	return id
}

func principalUUIDPtr(p authz.Principal) *uuid.UUID {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil
	}
	return &id
}

func normalizeTeam(team string) string {
	return strings.ToUpper(strings.TrimSpace(team))
}
