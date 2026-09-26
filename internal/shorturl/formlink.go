package shorturl

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

const (
	maxLabelRunes      = 200
	maxAliasRunes      = 64
	maxSuggestionTries = 9
)

// FormLinkInput describes the link a form asks for. Alias is the readable name
// the form suggests (its title and year, ADR 0033). ActorID is the person the
// forms service acts for; it becomes the creator when core knows that account.
type FormLinkInput struct {
	URL     string
	Label   string
	Alias   string
	ActorID *uuid.UUID
}

// EventForm is a form an event links to, under the alias the event chose.
type EventForm struct {
	FormID uuid.UUID
	URL    string
	Alias  string
	Label  string
}

func (s *service) formsAllowed(p authz.Principal, action authz.Action) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeFormLink}, action)
}

func (s *service) Availability(ctx context.Context, p authz.Principal, alias string) (Availability, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Create) && !s.formsAllowed(p, authz.Read) {
		return Availability{}, ErrForbidden
	}
	alias = strings.TrimSpace(alias)
	out := Availability{Alias: alias}
	switch {
	case !aliasPattern.MatchString(alias):
		out.Reason = ReasonInvalid
	case reservedAlias(alias):
		out.Reason = ReasonReserved
	default:
		taken, err := s.store.AliasTaken(ctx, alias, uuid.Nil)
		if err != nil {
			return Availability{}, err
		}
		if taken {
			out.Reason = ReasonTaken
		} else {
			out.Available = true
		}
	}
	return out, nil
}

func (s *service) FormLink(ctx context.Context, p authz.Principal, formID uuid.UUID) (URL, error) {
	if !s.formsAllowed(p, authz.Read) {
		return URL{}, ErrForbidden
	}
	return s.store.GetByForm(ctx, formID)
}

func (s *service) EnsureFormLink(ctx context.Context, p authz.Principal, formID uuid.UUID, in FormLinkInput) (URL, error) {
	if !s.formsAllowed(p, authz.Create) {
		return URL{}, ErrForbidden
	}
	label := cleanLabel(in.Label)
	existing, err := s.store.GetByForm(ctx, formID)
	if err == nil {
		if existing.EventID == nil && label != "" && label != existing.Label {
			existing.Label = label
			return s.store.Update(ctx, existing)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return URL{}, err
	}
	target, err := normalizeTarget(in.URL)
	if err != nil {
		return URL{}, err
	}
	if !strings.Contains(strings.ToLower(target), formID.String()) {
		return URL{}, ErrInvalid
	}
	alias, err := s.readableAlias(ctx, in.Alias, uuid.Nil)
	if err != nil {
		return URL{}, err
	}
	form := formID
	link := URL{Alias: alias, URL: target, FormID: &form, Label: label, CreatedBy: in.ActorID}
	if link.CreatedBy == nil {
		link.CreatedBy = principalID(p)
	}
	created, err := s.store.Create(ctx, link)
	if errors.Is(err, ErrForbidden) && in.ActorID != nil {
		// The actor has never signed in to core, so the account guard on
		// created_by rejects them; the forms service stands in as creator.
		link.CreatedBy = principalID(p)
		created, err = s.store.Create(ctx, link)
	}
	if errors.Is(err, ErrConflict) {
		if bound, getErr := s.store.GetByForm(ctx, formID); getErr == nil {
			return bound, nil
		}
	}
	return created, err
}

// RenameFormLink gives the form's link the alias asked for. An empty alias
// asks for the readable default instead, built from suggestion. An unusable
// suggestion is refused: falling back to a random alias would retire the
// current one for good for a name nobody asked for.
func (s *service) RenameFormLink(ctx context.Context, p authz.Principal, formID uuid.UUID, alias, suggestion string) (URL, error) {
	if !s.formsAllowed(p, authz.Update) {
		return URL{}, ErrForbidden
	}
	existing, err := s.store.GetByForm(ctx, formID)
	if err != nil {
		return URL{}, err
	}
	if existing.EventID != nil {
		return URL{}, ErrEventManaged
	}
	alias = strings.TrimSpace(alias)
	if alias == existing.Alias {
		return existing, nil
	}
	if alias == "" {
		if err := validateAlias(readableBase(suggestion)); err != nil {
			return URL{}, err
		}
		next, err := s.readableAlias(ctx, suggestion, existing.ID)
		if err != nil {
			return URL{}, err
		}
		if next == existing.Alias {
			return existing, nil
		}
		existing.Alias = next
		return s.store.Update(ctx, existing)
	}
	if err := validateAlias(alias); err != nil {
		return URL{}, err
	}
	taken, err := s.store.AliasTaken(ctx, alias, existing.ID)
	if err != nil {
		return URL{}, err
	}
	if taken {
		return URL{}, ErrConflict
	}
	existing.Alias = alias
	return s.store.Update(ctx, existing)
}

func (s *service) FormStats(ctx context.Context, p authz.Principal, formID uuid.UUID) (Stats, error) {
	if !s.formsAllowed(p, authz.Read) {
		return Stats{}, ErrForbidden
	}
	since := time.Now().UTC().Add(-HitRetention)
	sources, err := s.store.FormSources(ctx, formID, since)
	if err != nil {
		return Stats{}, err
	}
	total := 0
	for _, c := range sources {
		total += c.Count
	}
	return Stats{Since: since, Total: total, Sources: sources}, nil
}

// SyncEventForms makes the links of an event match the forms it names: each
// named alias becomes its form's link and is managed by the event, and a form
// the event no longer names goes back to managing its own link. A nil list
// releases everything, which is what archiving an event does.
func (s *service) SyncEventForms(ctx context.Context, eventID uuid.UUID, forms []EventForm) error {
	wanted := make(map[uuid.UUID]EventForm, len(forms))
	for _, f := range forms {
		if f.FormID == uuid.Nil || strings.TrimSpace(f.Alias) == "" {
			continue
		}
		if _, dup := wanted[f.FormID]; !dup {
			wanted[f.FormID] = f
		}
	}
	current, err := s.store.ListByEvent(ctx, eventID)
	if err != nil {
		return err
	}
	var errs []error
	for _, link := range current {
		if link.FormID == nil {
			if _, err := s.store.ReleaseForm(ctx, link.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if _, keep := wanted[*link.FormID]; keep {
			continue
		}
		if _, err := s.store.BindForm(ctx, link.ID, *link.FormID, nil, link.Label); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range wanted {
		if err := s.bindEventForm(ctx, eventID, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// bindEventForm prefers an existing link over a new one: the event panel
// creates its link before saving the alias, and a link people already use
// must keep working when the event takes the form over.
func (s *service) bindEventForm(ctx context.Context, eventID uuid.UUID, f EventForm) error {
	alias := strings.TrimSpace(f.Alias)
	label := cleanLabel(f.Label)
	event := eventID
	bound, err := s.store.GetByForm(ctx, f.FormID)
	switch {
	case err == nil && bound.Alias == alias:
		if bound.EventID != nil && *bound.EventID == eventID && bound.Label == label {
			return nil
		}
		_, err = s.store.BindForm(ctx, bound.ID, f.FormID, &event, label)
		return err
	case err != nil && !errors.Is(err, ErrNotFound):
		return err
	}
	named, err := s.store.GetByAlias(ctx, alias)
	if err == nil {
		if !strings.Contains(strings.ToLower(named.URL), f.FormID.String()) {
			return ErrConflict
		}
		_, err = s.store.BindForm(ctx, named.ID, f.FormID, &event, label)
		return err
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := validateAlias(alias); err != nil {
		return err
	}
	taken, err := s.store.AliasTaken(ctx, alias, uuid.Nil)
	if err != nil {
		return err
	}
	if taken {
		return ErrConflict
	}
	target, err := normalizeTarget(f.URL)
	if err != nil {
		return err
	}
	created, err := s.store.Create(ctx, URL{Alias: alias, URL: target, Label: label})
	if err != nil {
		return err
	}
	_, err = s.store.BindForm(ctx, created.ID, f.FormID, &event, label)
	return err
}

// readableAlias follows ADR 0033: a link gets the readable name it is offered,
// numbered (-2, -3, ...) when that name is taken, and a random alias only when
// no numbered variant is free. except lets a link take back its own name.
func (s *service) readableAlias(ctx context.Context, suggestion string, except uuid.UUID) (string, error) {
	base := readableBase(suggestion)
	if validateAlias(base) == nil {
		for n := 1; n <= maxSuggestionTries; n++ {
			candidate := numberedAlias(base, n)
			if validateAlias(candidate) != nil {
				continue
			}
			taken, err := s.store.AliasTaken(ctx, candidate, except)
			if err != nil {
				return "", err
			}
			if !taken {
				return candidate, nil
			}
		}
	}
	return s.ensureAlias(ctx, "")
}

// readableBase is the alias a suggestion asks for, before any numbering.
func readableBase(suggestion string) string {
	return strings.ToLower(strings.TrimSpace(suggestion))
}

func numberedAlias(base string, n int) string {
	if n == 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	if len(base)+len(suffix) > maxAliasRunes {
		base = strings.TrimRight(base[:maxAliasRunes-len(suffix)], "-_")
	}
	return base + suffix
}

func principalID(p authz.Principal) *uuid.UUID {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil
	}
	return &id
}

func cleanLabel(raw string) string {
	v := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, ""))
	v = strings.TrimSpace(v)
	if r := []rune(v); len(r) > maxLabelRunes {
		v = strings.TrimSpace(string(r[:maxLabelRunes]))
	}
	return v
}
