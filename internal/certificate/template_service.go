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
)

func (s *service) ListTemplates(ctx context.Context, p authz.Principal) ([]Template, error) {
	if s.templates == nil {
		return nil, ErrInvalid
	}
	all, err := s.templates.ListTemplates(ctx)
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
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: item.OwnerTeam}, authz.Read) {
		return Template{}, ErrForbidden
	}
	return item, nil
}

func (s *service) CreateTemplate(ctx context.Context, p authz.Principal, in TemplateDraft) (Template, error) {
	if s.templates == nil || !validTemplateDraft(in) {
		return Template{}, ErrInvalid
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: in.OwnerTeam}, authz.Create) {
		return Template{}, ErrForbidden
	}
	item := Template{
		ID: uuid.New(), Name: strings.TrimSpace(in.Name), OwnerTeam: normalizeTeam(in.OwnerTeam),
		SourceKind: in.SourceKind, SourceRef: strings.TrimSpace(in.SourceRef), SourceEditURL: strings.TrimSpace(in.SourceEditURL),
		DraftLayout: in.Layout, CreatedBy: principalUUIDPtr(p),
	}
	return s.templates.CreateTemplate(ctx, item)
}

func (s *service) UpdateTemplate(ctx context.Context, p authz.Principal, id uuid.UUID, in TemplateDraft) (Template, error) {
	if s.templates == nil || !validTemplateDraft(in) {
		return Template{}, ErrInvalid
	}
	existing, err := s.templates.GetTemplate(ctx, id)
	if err != nil {
		return Template{}, err
	}
	oldResource := authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: existing.OwnerTeam}
	newResource := authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: in.OwnerTeam}
	if !s.authz.Allow(p, oldResource, authz.Update) || !s.authz.Allow(p, newResource, authz.Update) {
		return Template{}, ErrForbidden
	}
	existing.Name = strings.TrimSpace(in.Name)
	existing.OwnerTeam = normalizeTeam(in.OwnerTeam)
	existing.SourceKind = in.SourceKind
	existing.SourceRef = strings.TrimSpace(in.SourceRef)
	existing.SourceEditURL = strings.TrimSpace(in.SourceEditURL)
	existing.DraftLayout = in.Layout
	return s.templates.UpdateTemplate(ctx, existing)
}

func (s *service) PublishTemplate(ctx context.Context, p authz.Principal, id uuid.UUID) (TemplateVersion, error) {
	item, err := s.GetTemplate(ctx, p, id)
	if err != nil {
		return TemplateVersion{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: item.OwnerTeam}, authz.Update) {
		return TemplateVersion{}, ErrForbidden
	}
	if err := ValidateLayout(item.DraftLayout); err != nil {
		return TemplateVersion{}, err
	}
	if s.render == nil {
		return TemplateVersion{}, ErrInvalid
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
	version := TemplateVersion{ID: versionID, TemplateID: item.ID, Layout: item.DraftLayout, AssetManifest: manifest}
	html, err := LayoutHTML(ctx, item.DraftLayout, sample, s.verifyURL(sample.Serial), s.assetsForVersion(version))
	if err != nil {
		return TemplateVersion{}, err
	}
	if preview, err := s.render.PDF(ctx, html); err != nil || len(preview) == 0 {
		if err != nil {
			return TemplateVersion{}, err
		}
		return TemplateVersion{}, ErrInvalid
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
		return nil, ErrInvalid
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
	html, err := LayoutHTML(ctx, item.DraftLayout, sample, s.verifyURL(sample.Serial), s.assets)
	if err != nil {
		return nil, err
	}
	return s.render.PDF(ctx, html)
}

func (s *service) PreviewEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]byte, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return nil, mapEventError(err)
	}
	if !s.canReadWorkspace(p, ev.OwnerTeam) {
		return nil, ErrForbidden
	}
	if s.render == nil {
		return nil, ErrInvalid
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
	html, err := LayoutHTML(ctx, resolved.Version.Layout, sample, s.verifyURL(sample.Serial), s.assetsForVersion(resolved.Version))
	if err != nil {
		return nil, err
	}
	return s.render.PDF(ctx, html)
}

func (s *service) ResolveTemplate(ctx context.Context, p authz.Principal, eventID uuid.UUID) (ResolvedTemplate, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return ResolvedTemplate{}, mapEventError(err)
	}
	if !s.canReadWorkspace(p, ev.OwnerTeam) {
		return ResolvedTemplate{}, ErrForbidden
	}
	resolved, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return ResolvedTemplate{}, err
	}
	return s.projectResolution(p, resolved), nil
}

func (s *service) projectResolution(p authz.Principal, resolved ResolvedTemplate) ResolvedTemplate {
	resource := authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: resolved.Template.OwnerTeam}
	if s.authz.Allow(p, resource, authz.Read) {
		return resolved
	}
	resolved.Template = Template{
		ID:         resolved.Template.ID,
		Name:       resolved.Template.Name,
		OwnerTeam:  resolved.Template.OwnerTeam,
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
		owner, _, err := s.bindingOwner(ctx, binding.Scope, binding.ScopeKey)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: owner}, authz.Assign) {
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
	owner, key, err := s.bindingOwner(ctx, in.Scope, in.ScopeKey)
	if err != nil {
		return Binding{}, err
	}
	if item.OwnerTeam != "" && normalizeTeam(item.OwnerTeam) != normalizeTeam(owner) {
		return Binding{}, ErrInvalid
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: owner}, authz.Assign) {
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
	owner, key, err := s.bindingOwner(ctx, scope, scopeKey)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: owner}, authz.Assign) {
		return ErrForbidden
	}
	return s.templates.DeleteBinding(ctx, scope, key)
}

func (s *service) bindingOwner(ctx context.Context, scope, key string) (string, string, error) {
	switch scope {
	case ScopeClub:
		return "", "SKY_LAB", nil
	case ScopeOwnerTeam:
		team := normalizeTeam(key)
		if team == "" {
			return "", "", ErrInvalid
		}
		return team, team, nil
	case ScopeEvent:
		id, err := uuid.Parse(key)
		if err != nil {
			return "", "", ErrInvalid
		}
		ev, err := s.events.Get(ctx, id)
		if err != nil {
			return "", "", mapEventError(err)
		}
		return ev.OwnerTeam, id.String(), nil
	default:
		return "", "", ErrInvalid
	}
}

func validTemplateDraft(in TemplateDraft) bool {
	if strings.TrimSpace(in.Name) == "" || len(strings.TrimSpace(in.Name)) > 160 {
		return false
	}
	switch in.SourceKind {
	case "upload", "canva", "figma", "sky":
	default:
		return false
	}
	if len(in.SourceRef) > 500 || len(in.SourceEditURL) > 2000 {
		return false
	}
	if strings.TrimSpace(in.SourceEditURL) != "" {
		parsed, err := url.ParseRequestURI(strings.TrimSpace(in.SourceEditURL))
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return false
		}
	}
	return ValidateLayout(in.Layout) == nil
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
