package identity

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The delegated way of the Group routes (docs/team-delegation.md): a caller
// who is not Privileged for Groups, with team delegation on, reaches a
// Group they manage (authz.Policy.Manages) by its Keycloak id. Off, or
// without a TeamDelegation, every such request is refused as before
// (ErrForbidden), with nothing read.

// delegatedCaller is the caller's subject when p may go the delegated way
// at all: delegation on, a person (no service account, no product), a
// subject that is an id, and a Group named by its id.
func (s *service) delegatedCaller(p authz.Principal, groupRef string) (uuid.UUID, error) {
	if !s.delegation.On() || p.ServiceAccount || p.Product != "" {
		return uuid.Nil, ErrForbidden
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return uuid.Nil, ErrForbidden
	}
	if _, err := uuid.Parse(groupRef); err != nil {
		return uuid.Nil, ErrForbidden
	}
	return id, nil
}

// decisionError is the error of a decision that did not allow.
func decisionError(d authz.ManageDecision) error {
	if d.Refusal != "" {
		return refusedWith(http.StatusForbidden, d.Refusal)
	}
	return ErrForbidden
}

// delegatedRead is the Group groupRef for a read by a Leader who manages it,
// with the caller's Leader groups as kept for reads. A caller who leads no
// Team is refused before the Group is read; an unknown Group is refused as
// one not managed.
func (s *service) delegatedRead(ctx context.Context, p authz.Principal, groupRef string) (authz.TeamManager, authz.ManagedGroup, Group, error) {
	userID, err := s.delegatedCaller(p, groupRef)
	if err != nil {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
	}
	m, err := s.delegation.manager(ctx, userID, false)
	if err != nil {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
	}
	if len(m.LeaderGroups) == 0 {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, ErrForbidden
	}
	target, g, err := s.delegation.target(ctx, groupRef, false)
	if errors.Is(err, ErrNotFound) {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, ErrForbidden
	}
	if err != nil {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
	}
	if d := s.delegation.policy.Manages(m, target); !d.Allowed {
		return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, decisionError(d)
	}
	return m, target, g, nil
}

// delegatedWrite runs write for a delegated write on the Group groupRef.
// The caller must be within their budget of writes a minute and have no
// other delegated write running; the whole write has WriteTimeout. Outside
// the Team code lock, the caller's Leader groups and the Group are read
// fresh and precheck decides: a caller it refuses (most often one who leads
// no Team of the Group) is refused with a denied row without ever waiting
// for the lock or holding a database connection for it (review of #235,
// B2). Then, under the lock (no create, rename or move runs between the
// decision and the write), both are read fresh again and write decides and
// writes. A Group that cannot be read is refused as one not managed; one
// whose ancestors by id and by path disagree is refused with a denied row.
func (s *service) delegatedWrite(ctx context.Context, p authz.Principal, action teamaudit.Action, groupRef string, person uuid.UUID,
	precheck func(p authz.Policy, m authz.TeamManager, target authz.ManagedGroup) authz.ManageDecision,
	write func(ctx context.Context, m authz.TeamManager, target authz.ManagedGroup, g Group) error) error {
	userID, err := s.delegatedCaller(p, groupRef)
	if err != nil {
		return err
	}
	if !s.delegation.allowWrite(userID) {
		return &DelegationError{Status: http.StatusTooManyRequests, Code: CodeRateLimited, RetryAfter: 60}
	}
	release, ok := s.delegation.beginWrite(userID)
	if !ok {
		return &DelegationError{Status: http.StatusTooManyRequests, Code: CodeRateLimited, RetryAfter: 1}
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, s.delegation.options.WriteTimeout)
	defer cancel()
	// No write while core cannot see protection (O3).
	if _, err := s.delegation.Canary(ctx); err != nil {
		return err
	}
	read := func(ctx context.Context) (authz.TeamManager, authz.ManagedGroup, Group, error) {
		m, err := s.delegation.manager(ctx, userID, true)
		if err != nil {
			return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
		}
		target, g, err := s.delegation.target(ctx, groupRef, true)
		var refused *DelegationError
		switch {
		case errors.Is(err, ErrNotFound):
			return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, ErrForbidden
		case errors.As(err, &refused):
			s.deny(ctx, action, target.Group, person, authz.ManageDecision{}, refused.Code)
			return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
		case err != nil:
			return authz.TeamManager{}, authz.ManagedGroup{}, Group{}, err
		}
		return m, target, g, nil
	}
	m, target, _, err := read(ctx)
	if err != nil {
		return err
	}
	// A caller whose own account is no longer active (closed, being
	// erased) writes nothing, whatever their token (review of #235, D3).
	if active, err := s.activeAccount(ctx, userID); err != nil {
		return err
	} else if !active {
		s.deny(ctx, action, target.Group, person, authz.ManageDecision{}, "forbidden")
		return ErrForbidden
	}
	if d := precheck(s.delegation.policy, m, target); !d.Allowed {
		s.deny(ctx, action, target.Group, person, d, d.Refusal)
		return decisionError(d)
	}
	return s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
		m, target, g, err := read(ctx)
		if err != nil {
			return err
		}
		return write(ctx, m, target, g)
	})
}

// activeAccount reports whether core's row of the person is an active
// account (none, or erased or being erased, is not).
func (s *service) activeAccount(ctx context.Context, id uuid.UUID) (bool, error) {
	shadow, err := s.users.Get(ctx, id)
	if errors.Is(err, user.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return shadow.AccountState == user.AccountActive && id != user.DeletedSubject, nil
}

// deny writes the denied row of a delegated write (docs/team-audit.md):
// the caller's basis is the Leader decision's when it found one, none
// otherwise. A row that cannot be written changes nothing (the write is
// refused anyway) and is logged.
func (s *service) deny(ctx context.Context, action teamaudit.Action, g authz.GroupNode, person uuid.UUID, d authz.ManageDecision, code string) {
	if s.audit == nil || g.ID == "" {
		return
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		s.delegation.options.Logf("team delegation: denied %s on group %s not recorded: no actor", action, g.ID)
		return
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisNone, ""
	if d.Basis == authz.ManageBasisLeader && d.ViaTeamID != "" {
		actor.Basis, actor.ViaGroupID = teamaudit.BasisLeader, d.ViaTeamID
	}
	if code == "" {
		code = "forbidden"
	}
	r := teamaudit.Record{Actor: actor, Action: action, GroupID: g.ID, GroupPath: g.Path, TargetUserID: person}
	if err := s.audit.Deny(ctx, r, code); err != nil {
		s.delegation.options.Logf("team delegation: denied %s on group %s not recorded: %v", action, g.ID, err)
	}
}

// asLeader is ctx with its actor's basis the Leader decision d: the audit
// row of the write names the Team that grants it.
func asLeader(ctx context.Context, d authz.ManageDecision) context.Context {
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		return ctx
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisLeader, d.ViaTeamID
	return teamaudit.WithActor(ctx, actor)
}

// delegatedAddMember adds userID to the Group groupRef for a Leader
// (SPEC §4.3): a Team they manage, or the Leader group of a Team they
// manage strictly; never a protected Group. The person is an active
// account (S4), the Groups on the way to the target share no name with
// another Group, and the add keeps the person within the delegated Group
// limit. Each refusal is a denied row.
func (s *service) delegatedAddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	return s.delegatedWrite(ctx, p, teamaudit.ActionMemberAdd, groupRef, userID, authz.Policy.ManagesMembership, func(ctx context.Context, m authz.TeamManager, target authz.ManagedGroup, g Group) error {
		d := s.delegation.policy.ManagesMembership(m, target)
		if !d.Allowed {
			s.deny(ctx, teamaudit.ActionMemberAdd, target.Group, userID, d, d.Refusal)
			return decisionError(d)
		}
		clash, err := s.delegation.nameClash(ctx, target)
		if err != nil {
			return err
		}
		if clash != "" {
			s.deny(ctx, teamaudit.ActionMemberAdd, target.Group, userID, d, CodeTeamCodeNotUnique)
			return refusedWith(http.StatusConflict, CodeTeamCodeNotUnique)
		}
		shadow, err := s.users.Get(ctx, userID)
		if errors.Is(err, user.ErrNotFound) || (err == nil && (shadow.AccountState != user.AccountActive || userID == user.DeletedSubject)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		groups, err := s.dir.GroupsForUser(ctx, userID)
		if err != nil {
			return err
		}
		already := slices.ContainsFunc(groups, func(current Group) bool { return current.ID == target.Group.ID })
		if !already && len(groups)+1 > s.delegation.options.GroupLimit {
			s.deny(ctx, teamaudit.ActionMemberAdd, target.Group, userID, d, CodeGroupLimit)
			return refusedWith(http.StatusConflict, CodeGroupLimit)
		}
		defer s.forgetGroups(userID)
		notify := s.membership != nil && !already && s.membership.Notifies(g)
		if err := s.dir.AddMember(asLeader(ctx, d), target.Group.ID, userID); err != nil {
			return err
		}
		if notify {
			s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: g, Added: true, ActorID: p.ID})
		}
		return nil
	})
}

// delegatedRemoveMember removes userID from the roster of the Group
// groupRef for a Leader who manages it: from the Group the person sits in
// (the Source group), which they must manage too, strictly when it is a
// Leader group (leader_group_needs_parent). Each refusal is a denied row.
func (s *service) delegatedRemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	return s.delegatedWrite(ctx, p, teamaudit.ActionMemberRemove, groupRef, userID, authz.Policy.Manages, func(ctx context.Context, m authz.TeamManager, target authz.ManagedGroup, g Group) error {
		d := s.delegation.policy.Manages(m, target)
		if !d.Allowed {
			s.deny(ctx, teamaudit.ActionMemberRemove, target.Group, userID, d, d.Refusal)
			return decisionError(d)
		}
		hits, err := s.memberHits(ctx, g)
		if err != nil {
			return err
		}
		source, sourceTarget := g, target
		for _, h := range hits {
			if h.person.ID != userID {
				continue
			}
			source = h.source
			if source.ID != g.ID {
				var refused *DelegationError
				sourceTarget, _, err = s.delegation.target(ctx, source.ID, true)
				switch {
				case errors.Is(err, ErrNotFound):
					return ErrForbidden
				case errors.As(err, &refused):
					s.deny(ctx, teamaudit.ActionMemberRemove, sourceTarget.Group, userID, d, refused.Code)
					return err
				case err != nil:
					return err
				}
			}
			break
		}
		ds := s.delegation.policy.ManagesMembership(m, sourceTarget)
		if !ds.Allowed {
			s.deny(ctx, teamaudit.ActionMemberRemove, sourceTarget.Group, userID, ds, ds.Refusal)
			return decisionError(ds)
		}
		defer s.forgetGroups(userID)
		if err := s.dir.RemoveMember(asLeader(ctx, ds), source.ID, userID); err != nil {
			return err
		}
		onRoster := slices.ContainsFunc(hits, func(h memberHit) bool { return h.person.ID == userID })
		if onRoster && s.membership != nil && s.membership.Notifies(source) {
			s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: source, Added: false, ActorID: p.ID})
		}
		return nil
	})
}

// delegatedMembers is the roster of a Group a Leader manages, with the
// fields a Leader sees of a person (SPEC §6.4): id, names, e-mail address,
// profile picture and Source group.
func (s *service) delegatedMembers(ctx context.Context, p authz.Principal, groupRef string) ([]GroupMember, error) {
	_, _, g, err := s.delegatedRead(ctx, p, groupRef)
	if err != nil {
		return nil, err
	}
	hits, err := s.memberHits(ctx, g)
	if err != nil {
		return nil, err
	}
	members := rosterMembers(g, hits)
	for i := range members {
		members[i].Person = projectPeople([]Person{members[i].Person}, false)[0]
	}
	return members, nil
}

// ManagedGroups is the caller's managed tree (GET
// /v1/users/me/managed-groups): every Team for Privileged, the Teams they
// lead for a Leader with delegation on, nothing for anyone else.
func (s *service) ManagedGroups(ctx context.Context, p authz.Principal) ([]ManagedNode, error) {
	if s.delegation == nil || p.ServiceAccount || p.Product != "" {
		return []ManagedNode{}, nil
	}
	userID, err := uuid.Parse(p.ID)
	if err != nil {
		return []ManagedNode{}, nil
	}
	privileged := s.allow(p, authz.TypeGroup, authz.Read) == nil
	return s.delegation.Tree(ctx, userID, privileged)
}

// MemberCandidate is a person a Group's manager may add (GET
// /v1/groups/{id}/member-candidates). Email is masked unless the query was
// the address itself.
type MemberCandidate struct {
	ID                uuid.UUID `json:"id"`
	FirstName         string    `json:"firstName"`
	LastName          string    `json:"lastName"`
	Email             string    `json:"email"`
	ProfilePictureURL string    `json:"profilePictureUrl,omitempty"`
	// AlreadyMember: the person is directly in the Group already.
	AlreadyMember bool `json:"alreadyMember"`
	// YTULinked: the person is YTÜ-linked (users.ytu_linked).
	YTULinked bool `json:"ytuLinked"`
	// Warnings are WarningNotYTUVerified when the Group is Privileged and
	// the person is not YTÜ-linked (grup-yonetimi 07); empty otherwise.
	Warnings []string `json:"warnings"`
}

// Candidate search limits (SPEC §6.2).
const (
	candidateQueryMin = 3
	candidateLimit    = 10
)

// MemberCandidates searches the people the caller may add to the Group
// groupRef: core's active accounts (S4: every active account; nobody erased
// or being erased) whose name holds q, at most ten, with the address
// masked. A query that is a whole address answers only the accounts with
// that address, unmasked; a part of an address matches nothing. Privileged,
// or a Leader who manages the Group within their searches a minute.
func (s *service) MemberCandidates(ctx context.Context, p authz.Principal, groupRef, q string) ([]MemberCandidate, error) {
	q = strings.TrimSpace(q)
	var g Group
	if s.allow(p, authz.TypeGroup, authz.Read) == nil {
		if utf8.RuneCountInString(q) < candidateQueryMin {
			return nil, ErrInvalid
		}
		var err error
		if g, err = s.dir.GetGroup(ctx, groupRef); err != nil {
			return nil, err
		}
	} else {
		userID, err := s.delegatedCaller(p, groupRef)
		if err != nil {
			return nil, err
		}
		if _, _, g, err = s.delegatedRead(ctx, p, groupRef); err != nil {
			return nil, err
		}
		if utf8.RuneCountInString(q) < candidateQueryMin {
			return nil, ErrInvalid
		}
		if !s.delegation.allowSearch(userID) {
			return nil, &DelegationError{Status: http.StatusTooManyRequests, Code: CodeRateLimited, RetryAfter: 60}
		}
	}
	found, err := s.users.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(q)
	matches := make([]user.User, 0, len(found))
	exact := make([]user.User, 0, 1)
	for _, u := range found {
		if u.AccountState != user.AccountActive || u.ID == user.DeletedSubject {
			continue
		}
		if strings.Contains(q, "@") && strings.EqualFold(strings.TrimSpace(u.Email), q) {
			exact = append(exact, u)
			continue
		}
		// Only the names are searched by a part: an address only whole
		// (above), so that a part of one is no oracle for the masked
		// address (review of #235, O2). The other fields the store searches
		// (school address, Sky number, username) are not a Leader's to
		// probe.
		for _, field := range []string{u.FirstName, u.LastName, strings.TrimSpace(u.FirstName + " " + u.LastName)} {
			if strings.Contains(strings.ToLower(field), needle) {
				matches = append(matches, u)
				break
			}
		}
	}
	shown, masked := matches, true
	if len(exact) > 0 {
		shown, masked = exact, false
	}
	slices.SortFunc(shown, func(a, b user.User) int {
		return strings.Compare(strings.ToLower(a.LastName+"\x00"+a.FirstName+"\x00"+a.Email), strings.ToLower(b.LastName+"\x00"+b.FirstName+"\x00"+b.Email))
	})
	if len(shown) > candidateLimit {
		shown = shown[:candidateLimit]
	}
	direct, err := s.dir.Members(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	privileged := s.privilegedPath(g.Path)
	out := make([]MemberCandidate, 0, len(shown))
	for _, u := range shown {
		c := MemberCandidate{ID: u.ID, FirstName: u.FirstName, LastName: u.LastName, Email: u.Email}
		if masked {
			c.Email = MaskEmail(u.Email)
		}
		c.ProfilePictureURL, _ = profilePicture(u)
		c.AlreadyMember = slices.ContainsFunc(direct, func(m Person) bool { return m.ID == u.ID })
		c.YTULinked = u.YTULinked
		c.Warnings = warnings(privileged, c.YTULinked)
		out = append(out, c)
	}
	return out, nil
}

// MaskEmail is address with its local part hidden but for its first
// character: ada@example.com is a***@example.com. Without an "@" it is
// hidden whole.
func MaskEmail(address string) string {
	address = strings.TrimSpace(address)
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return "***"
	}
	first, _ := utf8.DecodeRuneInString(address)
	return string(first) + "***" + address[at:]
}
