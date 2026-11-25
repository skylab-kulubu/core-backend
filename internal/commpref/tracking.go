package commpref

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Open and click tracking (hub ticket iletisim-servisi 16,
// docs/communication-preferences.md "Tracking"): SkyNotify records whether a
// person opened a mail and which link they clicked. Since ADR-0074's
// 2026-11-24 addendum (B9) every mail is tracked: a person's objection, a
// non-Member and a recipient without an account no longer stop it. Only an
// inactive account (disabled, being erased or erased) and the operator's
// brake (COMMUNICATION_TRACKING_MODE=opt_in) do.

// ScopeTracking is the old tracking preference's row: one per person, on the
// e-mail channel, with no group and no kind. Since B9 no write makes one and
// no decision reads one; the rows written before are kept. It is no News
// category: it never appears in a link, an audience or the items of a PUT.
const ScopeTracking Scope = "tracking"

// Tracking is the tracking preference's category.
func Tracking() Category { return Category{Scope: ScopeTracking} }

// TrackingMode is the operator's switch for tracking. It is no personal
// preference: it holds for everyone alike. The names are the ones the
// variable took before B9, kept so a deployment's value still reads.
type TrackingMode string

const (
	// TrackingOptOut tracks every active account. The default.
	TrackingOptOut TrackingMode = "opt_out"
	// TrackingOptIn is the brake: core lets SkyNotify track no one
	// (tracking_off), whatever a person ever chose.
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

// TrackingStartupLine is the startup log line of tracking: the mode.
func TrackingStartupLine(mode TrackingMode) string {
	if !mode.on() {
		return fmt.Sprintf("WARNING communication tracking: mode %s (%s): the brake is on, SkyNotify may track no one", mode, TrackingModeEnv)
	}
	return fmt.Sprintf("communication tracking: mode %s (%s)", mode, TrackingModeEnv)
}

// on reports whether the mode lets anyone be tracked.
func (m TrackingMode) on() bool { return m != TrackingOptIn }

// TrackingState is the screen's tracking field. Since B9 the Account center
// has no switch: the field stays for one release so an older Account center
// still reads the screen, and it tells the truth about the signed-in person
// (an active account): tracked exactly when the mode is on, whatever their
// old row says.
type TrackingState struct {
	// Enabled is whether the mode tracks anyone.
	Enabled bool `json:"enabled"`
	// Effective is whether core lets SkyNotify track the person now; for the
	// signed-in person, the same as Enabled.
	Effective bool         `json:"effective"`
	Mode      TrackingMode `json:"mode"`
}

func trackingState(mode TrackingMode) TrackingState {
	return TrackingState{Enabled: mode.on(), Effective: mode.on(), Mode: mode}
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

// TrackingResolveRole is core's client role of the service account that may
// ask whom a send may track: SkyNotify's (hub ticket 17). A person's token
// never may, whatever roles it carries. It follows audience:read: core's
// client roles carry no "core:" prefix.
const TrackingResolveRole = "communication-preferences:read"

// MaxTrackingSubjects is the most people one question names (SkyNotify asks
// in batches of 1000).
const MaxTrackingSubjects = 1000

// TrackingReason is why a person may not be tracked. Before B9 core also
// said preference_off (an objection) and not_member; it no longer does.
type TrackingReason string

const (
	// TrackingInactive: disabled in Keycloak, or being erased or erased.
	TrackingInactive TrackingReason = "inactive"
	// TrackingOff: the operator's brake (TrackingOptIn) is on.
	TrackingOff TrackingReason = "tracking_off"
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

// ResolveTracking answers, for each subject in order, whether SkyNotify may
// track mail to them (B9): an active account, and the mode on. A Member or
// not, with an objection on record or not, makes no difference. Inactive is
// the erased subject, an account core holds as being erased or erased, or a
// club member Keycloak has disabled (the snapshot holds the club's members
// only: a disabled non-Member is not seen and is allowed). The brake
// (TrackingOptIn) comes first and reads nothing: everyone is tracking_off
// even while Keycloak does not answer. Otherwise it reads the audience's
// snapshot of the tree (AudienceSnapshotTTL).
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
	// The brake answers without Keycloak or the accounts: it must hold
	// while they do not answer, when SkyNotify would otherwise track everyone.
	if mode := a.prefs.TrackingMode(); !mode.on() {
		for _, id := range subjects {
			out = append(out, TrackingDecision{Subject: id, Reason: TrackingOff})
		}
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
	for _, id := range subjects {
		decision := TrackingDecision{Subject: id}
		person, inTree := snap.people[id]
		if id == user.DeletedSubject || accounts[id].Blocked || (inTree && !person.Enabled) {
			decision.Reason = TrackingInactive
		} else {
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
