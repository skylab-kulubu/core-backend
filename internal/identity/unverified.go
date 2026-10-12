package identity

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// WarningNotYTUVerified is the warning core gives when a person who is not
// YTÜ-linked (users.ytu_linked; CONTEXT "YTÜ-linked") is about to be added
// to a Privileged root or a Group under one (grup-yonetimi 07, GY9). It is
// a warning, never a refusal: a graduate on the board may have no YTÜ
// account any more.
const WarningNotYTUVerified = "not_ytu_verified"

// MemberCheck is what the panel shows before it adds a person to a Group
// (GET /v1/groups/{id}/member-check/{userId}). Nothing in it stops the add.
type MemberCheck struct {
	// Privileged: the Group is a Privileged root or under one
	// (AUTHZ_PRIVILEGED_ROOTS). The panel learns it here, never from the
	// Group's name.
	Privileged bool `json:"privileged"`
	// YTULinked: the person is YTÜ-linked. A person core has no account of
	// is not.
	YTULinked bool `json:"ytuLinked"`
	// Warnings are WarningNotYTUVerified when the Group is Privileged and
	// the person is not YTÜ-linked; empty otherwise.
	Warnings []string `json:"warnings"`
}

// warnings are a candidate's or a check's warnings: never nil, so the JSON
// is always an array.
func warnings(privileged, ytuLinked bool) []string {
	if privileged && !ytuLinked {
		return []string{WarningNotYTUVerified}
	}
	return []string{}
}

func (s *service) privilegedPath(path string) bool {
	return authz.PrivilegedRoot(s.privilegedRoots, path) != ""
}

// ytuLinked reads whether the person is YTÜ-linked; a person core has no
// account of is not.
func (s *service) ytuLinked(ctx context.Context, userID uuid.UUID) (bool, error) {
	u, err := s.users.Get(ctx, userID)
	if errors.Is(err, user.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u.YTULinked && u.AccountState == user.AccountActive, nil
}

// MemberCheck answers what adding userID to groupRef would warn of, to
// whoever may read the Group: Privileged, or a Leader who manages it.
func (s *service) MemberCheck(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) (MemberCheck, error) {
	var g Group
	if err := s.allow(p, authz.TypeGroup, authz.Read); err == nil {
		if g, err = s.dir.GetGroup(ctx, groupRef); err != nil {
			return MemberCheck{}, err
		}
	} else {
		var err error
		if _, _, g, err = s.delegatedRead(ctx, p, groupRef); err != nil {
			return MemberCheck{}, err
		}
	}
	linked, err := s.ytuLinked(ctx, userID)
	if err != nil {
		return MemberCheck{}, err
	}
	privileged := s.privilegedPath(g.Path)
	return MemberCheck{Privileged: privileged, YTULinked: linked, Warnings: warnings(privileged, linked)}, nil
}

type unverifiedAckKey struct{}

// WithUnverifiedAck carries, for a member add, whether the caller confirmed
// the panel's warning that the person is not YTÜ-linked (the request's
// unverifiedAck). Without it the add counts as unconfirmed.
func WithUnverifiedAck(ctx context.Context, ack bool) context.Context {
	return context.WithValue(ctx, unverifiedAckKey{}, ack)
}

func unverifiedAckFrom(ctx context.Context) bool {
	ack, _ := ctx.Value(unverifiedAckKey{}).(bool)
	return ack
}

type auditAfterKey struct{}

// withAuditAfter gives the audited directory the after of the member add
// that ctx makes.
func withAuditAfter(ctx context.Context, after *teamaudit.State) context.Context {
	return context.WithValue(ctx, auditAfterKey{}, after)
}

func auditAfterFrom(ctx context.Context) *teamaudit.State {
	after, _ := ctx.Value(auditAfterKey{}).(*teamaudit.State)
	return after
}

// markUnverified is ctx with the audit row's mark of an add to a
// Privileged Group of a person who is not YTÜ-linked (unverifiedAck: what
// the caller confirmed). Any other add, or one whose Group or person cannot
// be read, is not marked; the add goes on either way. A Group named by its
// path is not read.
func (s *service) markUnverified(ctx context.Context, groupRef string, userID uuid.UUID) context.Context {
	path := groupRef
	if !strings.HasPrefix(groupRef, "/") {
		readCtx, cancel := context.WithTimeout(ctx, membershipReadTimeout)
		g, err := s.dir.GetGroup(readCtx, groupRef)
		cancel()
		if err != nil {
			return ctx
		}
		path = g.Path
	}
	if !s.privilegedPath(path) {
		return ctx
	}
	linked, err := s.ytuLinked(ctx, userID)
	if err != nil || linked {
		return ctx
	}
	ack := unverifiedAckFrom(ctx)
	return withAuditAfter(ctx, &teamaudit.State{UnverifiedAck: &ack})
}
