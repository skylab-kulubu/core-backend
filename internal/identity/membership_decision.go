package identity

import (
	"context"
	"errors"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// MembershipDecision is who may change a Group's membership, and on what
// basis: the decision AddMember and RemoveMember make, without the write
// (the management membership terms' extend and handover,
// docs/management-membership-terms.md).
type MembershipDecision struct {
	Group Group
	// Basis is teamaudit.BasisPrivileged, or BasisLeader with ViaGroupID
	// the Team whose Leader group grants it.
	Basis      teamaudit.Basis
	ViaGroupID string
}

// MembershipWriteDecision is whether p may change the membership of the
// Group groupRef, as a member add would decide: Privileged (groups:manage,
// a writing Privileged root), or with team delegation on a Leader who
// manages it (authz.Policy.ManagesMembership: a Team, or a Leader group of
// a Team they manage strictly). Nothing is written, and a refusal writes
// no denied row: it is a check, not a write. ErrForbidden otherwise; a
// Group a Leader cannot read is refused as one not managed.
func (s *service) MembershipWriteDecision(ctx context.Context, p authz.Principal, groupRef string) (MembershipDecision, error) {
	if s.allow(p, authz.TypeGroup, authz.Update) == nil {
		g, err := s.dir.GetGroup(ctx, groupRef)
		if err != nil {
			return MembershipDecision{}, err
		}
		return MembershipDecision{Group: g, Basis: teamaudit.BasisPrivileged}, nil
	}
	userID, err := s.delegatedCaller(p, groupRef)
	if err != nil {
		return MembershipDecision{}, err
	}
	m, err := s.delegation.manager(ctx, userID, true)
	if err != nil {
		return MembershipDecision{}, err
	}
	target, g, err := s.delegation.target(ctx, groupRef, true)
	if errors.Is(err, ErrNotFound) {
		return MembershipDecision{}, ErrForbidden
	}
	if err != nil {
		return MembershipDecision{}, err
	}
	if active, err := s.activeAccount(ctx, userID); err != nil {
		return MembershipDecision{}, err
	} else if !active {
		return MembershipDecision{}, ErrForbidden
	}
	d := s.delegation.policy.ManagesMembership(m, target)
	if !d.Allowed {
		return MembershipDecision{}, decisionError(d)
	}
	return MembershipDecision{Group: g, Basis: teamaudit.BasisLeader, ViaGroupID: d.ViaTeamID}, nil
}

// JoinMembershipNotifiers is one MembershipNotifier that tells each of
// notifiers (nil ones left out) of the changes it wants: the membership
// mail and the management membership terms both hear of the same writes.
// Nil when none is left.
func JoinMembershipNotifiers(notifiers ...MembershipNotifier) MembershipNotifier {
	var out joinedNotifiers
	for _, n := range notifiers {
		if n != nil {
			out = append(out, n)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

type joinedNotifiers []MembershipNotifier

func (j joinedNotifiers) Notifies(g Group) bool {
	for _, n := range j {
		if n.Notifies(g) {
			return true
		}
	}
	return false
}

func (j joinedNotifiers) MembershipChanged(ctx context.Context, change MembershipChange) {
	for _, n := range j {
		if n.Notifies(change.Group) {
			n.MembershipChanged(ctx, change)
		}
	}
}

func (j joinedNotifiers) PrecheckFailed() {
	for _, n := range j {
		n.PrecheckFailed()
	}
}
