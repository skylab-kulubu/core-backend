package commpref

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Open and click tracking of News mail (hub ticket iletisim-servisi 16,
// docs/communication-preferences.md "Tracking"): SkyMail records whether a
// person opened a News mail and which link they clicked only for the people
// core allows. The basis is legitimate interest with an opt-out (decision
// T1): the person turns it off on the Account center, the only way (T8: no
// mail carries a "stop tracking" link).

// ScopeTracking is the tracking preference's row: one per person, on the
// e-mail channel, with no group and no kind. It is no News category: it
// never appears in a link, an audience or the items of a PUT.
const ScopeTracking Scope = "tracking"

// Tracking is the tracking preference's category.
func Tracking() Category { return Category{Scope: ScopeTracking} }

// TrackingMode is what a person without a row gets.
type TrackingMode string

const (
	// TrackingOptOut tracks a Member until they turn it off (legitimate
	// interest, decision T1). The default.
	TrackingOptOut TrackingMode = "opt_out"
	// TrackingOptIn tracks only who turned it on: the brake and the way
	// back should explicit consent be required.
	TrackingOptIn TrackingMode = "opt_in"
)

// TrackingModeEnv sets the mode; unset is TrackingOptOut, any other value
// stops core at startup.
const TrackingModeEnv = "COMMUNICATION_TRACKING_MODE"

// TrackingModeFromEnv reads TrackingModeEnv.
func TrackingModeFromEnv(getenv func(string) string) (TrackingMode, error) {
	switch mode := TrackingMode(strings.TrimSpace(getenv(TrackingModeEnv))); mode {
	case "":
		return TrackingOptOut, nil
	case TrackingOptOut, TrackingOptIn:
		return mode, nil
	}
	return "", fmt.Errorf("%s must be %s or %s", TrackingModeEnv, TrackingOptOut, TrackingOptIn)
}

// fallback is the value of a person without a row.
func (m TrackingMode) fallback() bool { return m != TrackingOptIn }

// TrackingState is the Account center's tracking switch.
type TrackingState struct {
	// Enabled is the person's choice, or the mode's default without one.
	Enabled bool `json:"enabled"`
	// Effective is whether core lets SkyMail track the person now: enabled
	// and a Member (the signed-in person is active).
	Effective bool         `json:"effective"`
	Mode      TrackingMode `json:"mode"`
}

// trackingChoice is the person's tracking row, nil without one.
func trackingChoice(settings []Setting) *bool {
	for _, s := range settings {
		if s.Channel == ChannelEmail && s.Category.Scope == ScopeTracking {
			on := s.Enabled
			return &on
		}
	}
	return nil
}

func trackingState(settings []Setting, mode TrackingMode, member bool) TrackingState {
	enabled := mode.fallback()
	if choice := trackingChoice(settings); choice != nil {
		enabled = *choice
	}
	return TrackingState{Enabled: enabled, Effective: enabled && member, Mode: mode}
}

// SetTrackingMode sets the mode (TrackingModeFromEnv); a service never set
// is TrackingOptOut.
func (s *Service) SetTrackingMode(mode TrackingMode) { s.trackingMode = mode }

// TrackingMode is the service's mode.
func (s *Service) TrackingMode() TrackingMode {
	if s == nil || s.trackingMode == "" {
		return TrackingOptOut
	}
	return s.trackingMode
}

// OnTrackingWithdrawn is called after a write that turned tracking off and
// queued its withdrawal (Withdrawals.Wake).
func (s *Service) OnTrackingWithdrawn(wake func()) { s.onWithdrawn = wake }

// TrackingResolveRole is core's client role of the service account that may
// ask whom a send may track: SkyMail's (hub ticket 17). A person's token
// never may, whatever roles it carries. It follows audience:read: core's
// client roles carry no "core:" prefix.
const TrackingResolveRole = "communication-preferences:read"

// MaxTrackingSubjects is the most people one question names (SkyMail asks
// in batches of 1000).
const MaxTrackingSubjects = 1000

// TrackingReason is why a person may not be tracked.
type TrackingReason string

const (
	// TrackingPreferenceOff: turned off on the Account center, or no row
	// under TrackingOptIn.
	TrackingPreferenceOff TrackingReason = "preference_off"
	// TrackingNotMember: in no /UYELER Group (a graduate, an honorary
	// member, a guest, a service account, someone Keycloak does not have).
	TrackingNotMember TrackingReason = "not_member"
	// TrackingInactive: disabled in Keycloak, or being erased or erased.
	TrackingInactive TrackingReason = "inactive"
)

// TrackingDecision is one person's answer.
type TrackingDecision struct {
	Subject uuid.UUID
	Allowed bool
	// Reason is empty when Allowed.
	Reason TrackingReason
}

// TrackingAvailable reports whether ResolveTracking can answer. It needs no
// link key: the decision is the tree, the accounts and the rows.
func (a *Audience) TrackingAvailable() bool {
	return a != nil && a.prefs != nil && a.dir != nil && a.accounts != nil
}

// ResolveTracking answers, for each subject in order, whether SkyMail may
// track News mail to them: an active account, a Member (in the /UYELER
// tree, the audience's Member), and tracking on by their row or, without
// one, by the mode. The first rule that fails is the reason, in that
// order. It reads the audience's snapshot of the tree (AudienceSnapshotTTL).
func (a *Audience) ResolveTracking(ctx context.Context, subjects []uuid.UUID) ([]TrackingDecision, error) {
	if !a.TrackingAvailable() {
		return nil, ErrDisabled
	}
	if len(subjects) > MaxTrackingSubjects {
		return nil, ErrInvalid
	}
	seen := make(map[uuid.UUID]bool, len(subjects))
	for _, id := range subjects {
		if id == uuid.Nil || seen[id] {
			return nil, ErrInvalid
		}
		seen[id] = true
	}
	out := make([]TrackingDecision, 0, len(subjects))
	if len(subjects) == 0 {
		return out, nil
	}
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	ids := slices.Clone(subjects)
	accounts, err := a.accounts.Accounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	settings, err := a.prefs.store.ListPeople(ctx, ids)
	if err != nil {
		return nil, err
	}
	mode := a.prefs.TrackingMode()
	for _, id := range subjects {
		decision := TrackingDecision{Subject: id}
		person, inTree := snap.people[id]
		switch {
		case id == user.DeletedSubject || accounts[id].Blocked || (inTree && !person.Enabled):
			decision.Reason = TrackingInactive
		case !inTree || person.ServiceAccount || !snap.tree.Membership(snap.paths[id]).Member:
			decision.Reason = TrackingNotMember
		case !trackingState(settings[id], mode, true).Enabled:
			decision.Reason = TrackingPreferenceOff
		default:
			decision.Allowed = true
		}
		out = append(out, decision)
	}
	return out, nil
}

// TrackingMode is the preferences' mode.
func (a *Audience) TrackingMode() TrackingMode {
	if a == nil {
		return TrackingOptOut
	}
	return a.prefs.TrackingMode()
}
