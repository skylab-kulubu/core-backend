package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"golang.org/x/sync/errgroup"
)

type Service interface {
	ListGroups(ctx context.Context, p authz.Principal) ([]Group, error)
	GetGroup(ctx context.Context, p authz.Principal, groupRef string) (Group, error)
	CreateGroup(ctx context.Context, p authz.Principal, parentRef, name string) (Group, error)
	UpdateGroup(ctx context.Context, p authz.Principal, groupRef, name string, attrs map[string]string) (Group, error)
	DeleteGroup(ctx context.Context, p authz.Principal, groupID string) error
	Members(ctx context.Context, p authz.Principal, groupRef string) ([]GroupMember, error)
	AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	ListUsers(ctx context.Context, p authz.Principal, q string, seat ...ClientRole) ([]Person, error)
	ListClientRoles(ctx context.Context, p authz.Principal) ([]ClientRole, error)
	GetUser(ctx context.Context, p authz.Principal, id uuid.UUID) (UserCard, error)
	PatchUser(ctx context.Context, p authz.Principal, id uuid.UUID, in user.ProfilePatch) (UserCard, error)
	CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error)
	DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error
	GroupClientRoles(ctx context.Context, p authz.Principal, groupRef string) ([]ClientRole, error)
	SetGroupClientRoles(ctx context.Context, p authz.Principal, groupRef string, roles []ClientRole) error
	AddUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error
	RemoveUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error
	LogoutAllSessions(ctx context.Context, p authz.Principal, id uuid.UUID) error
	ListPublicTeams(ctx context.Context) ([]PublicTeam, error)
	PublicMembers(ctx context.Context, team string) (Roster, error)
	PublicLeaders(ctx context.Context, team string) (Roster, error)
	// ManagedGroups is the caller's managed tree (docs/team-delegation.md).
	ManagedGroups(ctx context.Context, p authz.Principal) ([]ManagedNode, error)
	// MemberCandidates searches the people the caller may add to a Group.
	MemberCandidates(ctx context.Context, p authz.Principal, groupRef, q string) ([]MemberCandidate, error)
	// MemberCheck is what adding a person to a Group would warn of
	// (grup-yonetimi 07); it never refuses the add.
	MemberCheck(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) (MemberCheck, error)
}

type service struct {
	dir                   Directory
	users                 user.Store
	assign                user.Service
	authz                 authz.Authorizer
	mail                  mail.Mailer
	accountErasureEnabled bool
	accessProjector       DeletionProjector
	groupCache            GroupCache
	membership            MembershipNotifier
	groupRoles            GroupRolesNotifier
	privilegedRoots       []string
	names                 teamcode.Rules
	codes                 teamcode.Ledger
	delegation            *TeamDelegation
	audit                 teamaudit.Writer
	directUserRoles       bool
	adminGroups           KeycloakAdminGroups
}

func NewService(dir Directory, users user.Store, az authz.Authorizer, mailers ...mail.Mailer) Service {
	return NewServiceWithOptions(dir, users, az, Options{}, mailers...)
}

type Options struct {
	AccountErasureEnabled bool
	AccessProjector       DeletionProjector
	// GroupCache is what core remembers of people's Groups for Group
	// overage tokens (OverageGroups). The service's membership writes and
	// Group renames drop it at once. Nil remembers nothing.
	GroupCache GroupCache
	// Membership hears of every person added to or removed from a Group it
	// Notifies of (the team membership mail, internal/teammail). Nil hears
	// nothing, and the membership writes read nothing for it.
	Membership MembershipNotifier
	// GroupRoles hears of every change to a Group's role mappings made
	// through SetGroupClientRoles (the privilege notice,
	// docs/privilege-change-mail.md). Nil hears nothing, and the write
	// reads nothing for it.
	GroupRoles GroupRolesNotifier
	// PrivilegedRoots are the Privileged roots (AUTHZ_PRIVILEGED_ROOTS):
	// adding someone who is not YTÜ-linked to one, or under one, is flagged
	// (docs/team-audit.md, "Unverified account"). Nil is
	// authz.DefaultPrivilegedRoots.
	PrivilegedRoots []string
	// GroupNames is the Team code rule every Group create and rename passes
	// (docs/team-codes.md). Nil is the default policy's.
	GroupNames *teamcode.Rules
	// TeamCodes are the codes core's records hold and the lock a create or
	// rename holds from its check to its write. Nil is a ledger of this
	// process only, with no records.
	TeamCodes teamcode.Ledger
	// Delegation decides and reads for team delegation
	// (docs/team-delegation.md): the Group routes' delegated way, the
	// managed tree. Nil, or switched off, leaves the Group routes
	// Privileged's alone, as before.
	Delegation *TeamDelegation
	// TeamAudit receives the denied rows of delegated writes (the changes
	// themselves are recorded by AuditedDirectory). Nil records none.
	TeamAudit teamaudit.Writer
	// DirectUserRoles lets a client role be given to a person directly
	// (DIRECT_USER_ROLES=on). Off, the default (rol-temizligi K1), a role
	// reaches a person only through a Group; a client's service account
	// still takes one. Taking a role off a person is always allowed.
	DirectUserRoles bool
	// AdminGroups reads whether a Group holds a Keycloak administration
	// role (realm-management): core changes no membership there. Nil reads
	// nothing and refuses nothing (memory setups).
	AdminGroups KeycloakAdminGroups
}

// MembershipNotifier hears of membership changes made through the service.
type MembershipNotifier interface {
	// Notifies reports whether changes to g are wanted. It is asked before
	// the change, so a Group nobody wants costs no extra read.
	Notifies(g Group) bool
	// MembershipChanged is told of one change that took place: after the
	// directory accepted it, and only when the person's membership of the
	// Group did change. It must not fail or hold up the request.
	MembershipChanged(ctx context.Context, change MembershipChange)
	// PrecheckFailed is told that the reads that tell a change from a
	// repeat failed: the write went on, and no change is reported for it.
	PrecheckFailed()
}

// GroupRolesNotifier hears of role mapping changes made through the service.
type GroupRolesNotifier interface {
	// GroupRolesChanged is told of one write that changed a Group's role
	// mappings: after the directory took it, with the roles it added and
	// removed. It must not fail or hold up the request.
	GroupRolesChanged(ctx context.Context, change GroupRolesChange)
	// RolesPrecheckFailed is told that the Group's role mappings could not
	// be read before a write: the write went on, and no change is reported
	// for it.
	RolesPrecheckFailed()
}

// GroupRolesChange is one write of a Group's role mappings that changed
// them.
type GroupRolesChange struct {
	Group   Group
	Added   []ClientRole
	Removed []ClientRole
	// ActorID is the caller's subject (authz.Principal.ID).
	ActorID string
}

// membershipReadTimeout bounds the reads an add makes for the notifier, so
// a slow Keycloak delays the write by at most this much.
const membershipReadTimeout = 3 * time.Second

// MembershipChange is one person added to, or removed from, one Group. The
// Group is the one the person's membership changed in: on a removal through
// a parent roster, the subgroup they sat in.
type MembershipChange struct {
	UserID uuid.UUID
	Group  Group
	Added  bool
	// ActorID is the caller's subject (authz.Principal.ID), as the token
	// carried it.
	ActorID string
}

type DeletionProjector interface {
	Project(context.Context, user.DeletionRequest) error
}

func NewServiceWithOptions(dir Directory, users user.Store, az authz.Authorizer, options Options, mailers ...mail.Mailer) Service {
	s := &service{
		dir: dir, users: users, assign: user.NewService(users, dir), authz: az,
		accountErasureEnabled: options.AccountErasureEnabled,
		accessProjector:       options.AccessProjector,
		groupCache:            options.GroupCache,
		membership:            options.Membership,
		groupRoles:            options.GroupRoles,
		privilegedRoots:       options.PrivilegedRoots,
		codes:                 options.TeamCodes,
		delegation:            options.Delegation,
		audit:                 options.TeamAudit,
		directUserRoles:       options.DirectUserRoles,
		adminGroups:           options.AdminGroups,
	}
	if options.GroupNames != nil {
		s.names = *options.GroupNames
	} else {
		s.names = teamcode.NewRules(authz.DefaultPolicy())
	}
	if s.codes == nil {
		s.codes = teamcode.NewMemoryLedger()
	}
	if s.privilegedRoots == nil {
		s.privilegedRoots = authz.DefaultPrivilegedRoots()
	}
	if len(mailers) > 0 {
		s.mail = mailers[0]
	}
	return s
}

func (s *service) allow(p authz.Principal, t authz.Type, a authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: t}, a) {
		return ErrForbidden
	}
	return nil
}

func (s *service) allowUserRead(p authz.Principal) error {
	if s.allow(p, authz.TypeUser, authz.Read) == nil {
		return nil
	}
	for _, role := range p.Roles {
		if role == "users:read" {
			return nil
		}
	}
	return ErrForbidden
}

func (s *service) ListGroups(ctx context.Context, p authz.Principal) ([]Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.ListGroups(ctx)
}

func (s *service) GetGroup(ctx context.Context, p authz.Principal, groupRef string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		_, _, g, err := s.delegatedRead(ctx, p, groupRef)
		return g, err
	}
	return s.dir.GetGroup(ctx, groupRef)
}

// CreateGroup creates name under parentRef, or at the realm's top. The name
// passes the Team code rule (docs/team-codes.md), checked and written under
// the Team code lock.
func (s *service) CreateGroup(ctx context.Context, p authz.Principal, parentRef, name string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Create); err != nil {
		return Group{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, ErrInvalid
	}
	parentPath := ""
	if parentRef != "" {
		parent, err := s.dir.GetGroup(ctx, parentRef)
		if err != nil {
			return Group{}, err
		}
		// The Group the rule was checked under is the one written under.
		parentRef, parentPath = parent.ID, parent.Path
	}
	if err := roleGroupStaysLeaf(parentPath); err != nil {
		return Group{}, err
	}
	scope, err := s.names.Create(parentPath, name)
	if err != nil {
		return Group{}, err
	}
	var created Group
	err = s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		if err := s.nameFree(ctx, records, scope, parentPath+"/"+name, name, ""); err != nil {
			return err
		}
		var err error
		created, err = s.dir.CreateGroup(ctx, parentRef, name)
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return created, nil
}

// UpdateGroup writes the Group's attributes, and its name when name is a new
// one. A new name passes the Team code rule as a create's does, and the old
// one is retired with the write: no new Group takes it again. The same name,
// blanks aside, is no rename, whatever rule the name predates.
func (s *service) UpdateGroup(ctx context.Context, p authz.Principal, groupRef, name string, attrs map[string]string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return Group{}, err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return Group{}, err
	}
	if attrs != nil {
		g.Attributes = attrs
	}
	name = strings.TrimSpace(name)
	if name == "" || name == strings.TrimSpace(g.Name) {
		return s.dir.UpdateGroup(ctx, g)
	}
	subgroups, err := s.dir.Subgroups(ctx, g.ID)
	if err != nil {
		return Group{}, err
	}
	scope, err := s.names.Rename(g.Path, name, len(subgroups) == 0)
	if err != nil {
		return Group{}, err
	}
	if err := s.renameKeepsPlacement(ctx, g, name, len(subgroups) > 0); err != nil {
		return Group{}, err
	}
	// A new name is a new path for the Group and everything under it;
	// forgotten even when the write fails: it may have reached Keycloak.
	defer s.forgetAllGroups()
	old := g.Name
	g.Name = name
	var updated Group
	err = s.codes.Locked(ctx, func(ctx context.Context, records teamcode.Records) error {
		// A change of case keeps the code: the records that hold it are
		// this Group's own, and nothing is retired.
		sameCode := strings.EqualFold(strings.TrimSpace(old), name)
		checked := scope
		if sameCode {
			checked = teamcode.AtPath
		}
		path := g.Path[:strings.LastIndex(g.Path, "/")+1] + name
		if err := s.nameFree(ctx, records, checked, path, name, g.ID); err != nil {
			return err
		}
		if !sameCode && !s.names.Reserved(old) {
			if err := records.Retire(ctx, old, g.ID, teamcode.ReasonRenamed); err != nil {
				return err
			}
		}
		var err error
		updated, err = s.dir.UpdateGroup(ctx, g)
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return updated, nil
}

// nameFree checks that no Group of the realm but except sits at path and,
// for a name that must be free Everywhere, that no other Group, Owner team
// record or retired code holds name. Names compare without case.
func (s *service) nameFree(ctx context.Context, records teamcode.Records, scope teamcode.Scope, path, name, except string) error {
	groups, err := s.dir.ListGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.ID == except {
			continue
		}
		if strings.EqualFold(g.Path, path) || (scope == teamcode.Everywhere && strings.EqualFold(strings.TrimSpace(g.Name), name)) {
			return teamcode.Taken(name)
		}
	}
	if scope != teamcode.Everywhere {
		return nil
	}
	held, err := records.Held(ctx, name)
	if err != nil {
		return err
	}
	if held {
		return teamcode.Taken(name)
	}
	return nil
}

func (s *service) Members(ctx context.Context, p authz.Principal, groupRef string) ([]GroupMember, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return s.delegatedMembers(ctx, p, groupRef)
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return nil, err
	}
	hits, err := s.memberHits(ctx, g)
	if err != nil {
		return nil, err
	}
	return rosterMembers(g, hits), nil
}

func (s *service) AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return s.delegatedAddMember(ctx, p, groupRef, userID)
	}
	return s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
		return s.addMember(ctx, p, groupRef, userID)
	})
}

func (s *service) addMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.notKeycloakAdminGroup(ctx, groupRef); err != nil {
		return err
	}
	// Forgotten even when the write fails: it may have reached Keycloak.
	defer s.forgetGroups(userID)
	ctx = s.markUnverified(ctx, groupRef, userID)
	g, notify := s.addNotification(ctx, groupRef, userID)
	if err := s.dir.AddMember(ctx, groupRef, userID); err != nil {
		return err
	}
	if notify {
		s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: g, Added: true, ActorID: p.ID})
	}
	return nil
}

// addNotification is the Group an add is about to change and whether the
// notifier hears of it: a Group it wants, that the person is not yet
// directly in. Keycloak takes the add of a member again without a word, so
// this is the only place that can tell a change from a repeat. A read that
// fails tells nothing; the add itself goes on as before.
func (s *service) addNotification(ctx context.Context, groupRef string, userID uuid.UUID) (Group, bool) {
	if s.membership == nil {
		return Group{}, false
	}
	// A Group named by its path is asked about before anything is read.
	if strings.HasPrefix(groupRef, "/") && !s.membership.Notifies(Group{Path: groupRef}) {
		return Group{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, membershipReadTimeout)
	defer cancel()
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		// An unknown Group fails the add itself, which says so.
		if !errors.Is(err, ErrNotFound) {
			s.membership.PrecheckFailed()
		}
		return Group{}, false
	}
	if !s.membership.Notifies(g) {
		return Group{}, false
	}
	groups, err := s.dir.GroupsForUser(ctx, userID)
	if err != nil {
		s.membership.PrecheckFailed()
		return Group{}, false
	}
	for _, current := range groups {
		if current.ID == g.ID {
			return Group{}, false
		}
	}
	return g, true
}

func (s *service) RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return s.delegatedRemoveMember(ctx, p, groupRef, userID)
	}
	defer s.forgetGroups(userID)
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return err
	}
	hits, err := s.memberHits(ctx, g)
	if err != nil {
		return err
	}
	for _, h := range hits {
		if h.person.ID == userID {
			if err := s.notKeycloakAdminGroup(ctx, h.source.ID); err != nil {
				return err
			}
			if err := s.dir.RemoveMember(ctx, h.source.ID, userID); err != nil {
				return err
			}
			if s.membership != nil && s.membership.Notifies(h.source) {
				s.membership.MembershipChanged(ctx, MembershipChange{UserID: userID, Group: h.source, Added: false, ActorID: p.ID})
			}
			return nil
		}
	}
	// Not on the roster: nothing changes, and nobody is told.
	if err := s.notKeycloakAdminGroup(ctx, g.ID); err != nil {
		return err
	}
	return s.dir.RemoveMember(ctx, g.ID, userID)
}

// forgetGroups drops what core remembers of userID's Groups: for Group
// overage tokens and for team delegation.
func (s *service) forgetGroups(userID uuid.UUID) {
	if s.groupCache != nil {
		s.groupCache.Forget(userID)
	}
	s.delegation.Forget(userID)
}

func (s *service) forgetAllGroups() {
	if s.groupCache != nil {
		s.groupCache.ForgetAll()
	}
	s.delegation.ForgetAll()
}

func (s *service) ListClientRoles(ctx context.Context, p authz.Principal) ([]ClientRole, error) {
	if err := s.allow(p, authz.TypeUser, authz.Read); err != nil {
		return nil, err
	}
	roles, err := s.dir.ListClientRoles(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(roles, func(a, b ClientRole) int {
		if a.ClientID != b.ClientID {
			return strings.Compare(a.ClientID, b.ClientID)
		}
		return strings.Compare(a.Role, b.Role)
	})
	return roles, nil
}

func (s *service) ListUsers(ctx context.Context, p authz.Principal, q string, seat ...ClientRole) ([]Person, error) {
	if err := s.allow(p, authz.TypeUser, authz.Read); err != nil {
		return nil, err
	}
	full := s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.ReadFull)
	q = strings.TrimSpace(q)
	if len(seat) > 0 && strings.TrimSpace(seat[0].Role) != "" {
		clientID := strings.TrimSpace(seat[0].ClientID)
		if clientID == "" {
			clientID = "forms"
		}
		role := strings.TrimSpace(seat[0].Role)
		people, err := s.dir.UsersWithClientRole(ctx, clientID, role)
		if err != nil {
			return nil, err
		}
		if q == "" {
			// Without a query the list stays the role's direct holders and
			// the members of the Groups it is mapped to; finding everyone
			// who holds it in effect would read the whole realm.
			people, err = s.overlayShadow(ctx, people)
			if err != nil {
				return nil, err
			}
			return projectPeople(people, full), nil
		}
		people, err = s.seatSearch(ctx, clientID, role, q, full, people)
		if err != nil {
			return nil, err
		}
		return projectPeople(people, full), nil
	}
	people, err := s.dir.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	if q != "" {
		if full {
			people, err = s.mergeUserSearch(ctx, people, q)
		} else {
			people, err = s.overlayShadow(ctx, people)
			if err == nil {
				people = safePeopleSearch(people, q)
			}
		}
		return projectPeople(people, full), err
	}
	if !full {
		people, err = s.overlayShadow(ctx, people)
		return projectPeople(people, false), err
	}
	out := make([]Person, 0, len(people))
	for _, person := range people {
		// The list reads core's row and ensures only a person whose row
		// Ensure would change: it is opened on every visit to several panel
		// pages, and Ensure's write stamps updated_at even when nothing
		// changed.
		shadow, err := s.users.Get(ctx, person.ID)
		if err != nil && !errors.Is(err, user.ErrNotFound) {
			return nil, err
		}
		if err == nil && shadow.AccountState != user.AccountActive {
			continue
		}
		if err != nil || listChanges(person, shadow) {
			shadow, _, err = s.assign.Ensure(ctx, person.ID, user.Profile{
				Email:       person.Email,
				FirstName:   person.FirstName,
				LastName:    person.LastName,
				Username:    person.Username,
				SchoolEmail: person.SchoolEmail,
				SkyNumber:   person.SkyNumber,
			})
		}
		if err != nil {
			if errors.Is(err, user.ErrAccountBlocked) {
				continue
			}
			if errors.Is(err, user.ErrConflict) {
				out = append(out, person)
				continue
			}
			return nil, err
		}
		if shadow.ID == person.ID {
			person.FirstName = shadow.FirstName
			person.LastName = shadow.LastName
			if shadow.SchoolEmail != "" {
				person.SchoolEmail = shadow.SchoolEmail
			}
			if shadow.SkyNumber != "" {
				person.SkyNumber = shadow.SkyNumber
			}
			person = withProfilePicture(person, shadow)
		}
		out = append(out, person)
	}
	return projectPeople(out, full), nil
}

// listChanges reports whether Ensure would change the active row shadow of
// a person the full list read from the directory: what user's Upsert takes
// from the directory (a non-empty e-mail, username or school e-mail that
// differs; never the names, and a Sky number only where the row has none),
// and a missing Sky number, which Ensure assigns.
func listChanges(person Person, shadow user.User) bool {
	differs := func(directory, stored string) bool { return directory != "" && directory != stored }
	return shadow.SkyNumber == "" ||
		differs(person.Email, shadow.Email) ||
		differs(person.Username, shadow.Username) ||
		differs(person.SchoolEmail, shadow.SchoolEmail)
}

func projectPeople(people []Person, full bool) []Person {
	out := make([]Person, 0, len(people))
	for _, person := range people {
		if !full {
			person = Person{
				ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
				ProfilePictureURL: person.ProfilePictureURL, ProfilePictureSizes: person.ProfilePictureSizes,
			}
		}
		out = append(out, activePerson(person))
	}
	return out
}

func safePeopleSearch(people []Person, query string) []Person {
	out := make([]Person, 0, len(people))
	for _, person := range people {
		if safePersonMatches(person, query) {
			out = append(out, person)
		}
	}
	return out
}

func safePersonMatches(person Person, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	for _, field := range []string{person.Email, person.FirstName, person.LastName, strings.TrimSpace(person.FirstName + " " + person.LastName)} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// GetUser answers a person who is erased, being erased, or the placeholder
// subject with erasedPerson, to every caller allowed to read users, whatever
// Keycloak still holds: core's row (or, after the hard purge, its deletion
// marker) decides. A caller who may not read users is refused first, so it
// learns nothing of the id.
func (s *service) GetUser(ctx context.Context, p authz.Principal, id uuid.UUID) (UserCard, error) {
	if err := s.allowUserRead(p); err != nil {
		return UserCard{}, err
	}
	if id == user.DeletedSubject {
		return UserCard{Person: erasedPerson(id, user.ReadStatusDeleted)}, nil
	}
	shadow, shadowErr := s.users.Get(ctx, id)
	switch {
	case shadowErr == nil:
		if shadow.AccountState != user.AccountActive {
			return UserCard{Person: erasedPerson(id, shadow.AccountState.ReadStatus())}, nil
		}
	case errors.Is(shadowErr, user.ErrNotFound):
		// No row: a deletion marker means the person was hard-purged.
		state, err := s.users.AttributionState(ctx, id)
		if err != nil {
			return UserCard{}, err
		}
		if state == user.AttributionBlocked {
			return UserCard{Person: erasedPerson(id, user.ReadStatusDeleted)}, nil
		}
	default:
		// Without core's row the answer cannot say whether the person is
		// erased; it is not guessed from Keycloak.
		return UserCard{}, shadowErr
	}
	person, err := s.dir.GetUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.ReadFull) {
		if shadowErr == nil {
			person.FirstName = shadow.FirstName
			person.LastName = shadow.LastName
		}
		return nameOnlyCard(UserCard{Person: withProfilePicture(Person{
			ID:        person.ID,
			Email:     person.Email,
			FirstName: person.FirstName,
			LastName:  person.LastName,
		}, shadow)}), nil
	}
	if shadowErr == nil {
		person = overlayPerson(person, shadow)
	}
	groups, err := s.dir.GroupsForUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	inherited := make([]ClientRole, 0)
	seen := map[string]struct{}{}
	for _, g := range groups {
		roles, err := s.dir.GroupClientRoles(ctx, g.ID)
		if err != nil {
			return UserCard{}, err
		}
		for _, r := range roles {
			key := r.ClientID + ":" + r.Role
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			inherited = append(inherited, r)
		}
	}
	extra, err := s.dir.UserExtraRoles(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if groups == nil {
		groups = []Group{}
	}
	if extra == nil {
		extra = []ClientRole{}
	}
	return userCard(person, groups, inherited, extra, shadow), nil
}

func nameOnlyCard(card UserCard) UserCard {
	return UserCard{
		Person: activePerson(Person{
			ID:                  card.ID,
			Email:               card.Email,
			FirstName:           card.FirstName,
			LastName:            card.LastName,
			ProfilePictureURL:   card.ProfilePictureURL,
			ProfilePictureSizes: card.ProfilePictureSizes,
		}),
	}
}

// activePerson marks a person whose account is active.
func activePerson(person Person) Person {
	person.Status = user.ReadStatusActive
	person.DisplayName = strings.TrimSpace(person.FirstName + " " + person.LastName)
	return person
}

// erasedPerson is the answer for a person who is erased or being erased
// (docs/account-erasure-command.md §8): the id, the status, and the fixed
// name in displayName and firstName, so a client that joins firstName and
// lastName shows "Silinmiş kullanıcı" too. Nothing else of the person.
func erasedPerson(id uuid.UUID, status user.ReadStatus) Person {
	return Person{ID: id, FirstName: user.DeletedDisplayName, Status: status, DisplayName: user.DeletedDisplayName}
}

func overlayPerson(person Person, shadow user.User) Person {
	if shadow.SchoolEmail != "" {
		person.SchoolEmail = shadow.SchoolEmail
	}
	if shadow.SkyNumber != "" {
		person.SkyNumber = shadow.SkyNumber
	}
	person.FirstName = shadow.FirstName
	person.LastName = shadow.LastName
	return person
}

func userCard(person Person, groups []Group, inherited, extra []ClientRole, shadow user.User) UserCard {
	return UserCard{
		Person:         activePerson(withProfilePicture(person, shadow)),
		Linkedin:       shadow.Linkedin,
		University:     shadow.University,
		Faculty:        shadow.Faculty,
		Department:     shadow.Department,
		YTULinked:      shadow.YTULinked,
		Phone:          shadow.Phone,
		StudentCardUid: shadow.StudentCardUID,
		Groups:         groups,
		InheritedRoles: inherited,
		ExtraRoles:     extra,
	}
}

func (s *service) PatchUser(ctx context.Context, p authz.Principal, id uuid.UUID, in user.ProfilePatch) (UserCard, error) {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return UserCard{}, err
	}
	person, err := s.dir.GetUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if _, err := s.users.Get(ctx, id); errors.Is(err, user.ErrNotFound) {
		if _, _, err := s.assign.Ensure(ctx, id, user.Profile{
			Email:       person.Email,
			FirstName:   person.FirstName,
			LastName:    person.LastName,
			Username:    person.Username,
			SchoolEmail: person.SchoolEmail,
			SkyNumber:   person.SkyNumber,
		}); err != nil {
			return UserCard{}, err
		}
	} else if err != nil {
		return UserCard{}, err
	}
	if _, err := s.assign.Patch(ctx, id, in); err != nil {
		return UserCard{}, err
	}
	return s.GetUser(ctx, p, id)
}

func (s *service) GroupClientRoles(ctx context.Context, p authz.Principal, groupRef string) ([]ClientRole, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return nil, err
	}
	return s.dir.GroupClientRoles(ctx, g.ID)
}

func (s *service) SetGroupClientRoles(ctx context.Context, p authz.Principal, groupRef string, roles []ClientRole) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	return s.codes.Locked(ctx, func(ctx context.Context, _ teamcode.Records) error {
		return s.setGroupClientRoles(ctx, p, groupRef, roles)
	})
}

func (s *service) setGroupClientRoles(ctx context.Context, p authz.Principal, groupRef string, roles []ClientRole) error {
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return err
	}
	added := s.addedRoles(ctx, g, roles)
	if err := s.noKeycloakAdminRole(ctx, added); err != nil {
		return err
	}
	if err := s.placementAllows(ctx, g, added); err != nil {
		return err
	}
	// A Group's role mappings decide whether it is protected: forgotten
	// even when the write fails, since it may have reached Keycloak.
	defer s.delegation.ForgetAll()
	before, known := s.rolesBefore(ctx, g)
	err = s.dir.SetGroupClientRoles(ctx, g.ID, roles)
	if known {
		s.reportRoles(ctx, p, g, before, roles, err)
	}
	return err
}

// rolesBefore reads the Group's role mappings before a write, for the
// notifier: nothing without one. A read that fails is counted and reports
// nothing; the write goes on.
func (s *service) rolesBefore(ctx context.Context, g Group) ([]ClientRole, bool) {
	if s.groupRoles == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, membershipReadTimeout)
	defer cancel()
	before, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		s.groupRoles.RolesPrecheckFailed()
		return nil, false
	}
	return before, true
}

// reportRoles tells the notifier what a role mapping write changed. A write
// that failed may still have reached Keycloak in part (it writes client by
// client), so the mappings are read again and whatever did change is told.
func (s *service) reportRoles(ctx context.Context, p authz.Principal, g Group, before, want []ClientRole, writeErr error) {
	after := want
	if writeErr != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), membershipReadTimeout)
		defer cancel()
		var err error
		if after, err = s.dir.GroupClientRoles(readCtx, g.ID); err != nil {
			s.groupRoles.RolesPrecheckFailed()
			return
		}
	}
	added, removed := roleDiff(before, after)
	if len(added)+len(removed) == 0 {
		return
	}
	s.groupRoles.GroupRolesChanged(ctx, GroupRolesChange{Group: g, Added: added, Removed: removed, ActorID: p.ID})
}

// roleDiff is what going from before to after adds and removes, each role
// once, in after's and before's order.
func roleDiff(before, after []ClientRole) (added, removed []ClientRole) {
	key := func(r ClientRole) string { return r.ClientID + "\x00" + r.Role }
	had := map[string]bool{}
	for _, r := range before {
		had[key(r)] = true
	}
	has := map[string]bool{}
	for _, r := range after {
		if !has[key(r)] && !had[key(r)] {
			added = append(added, r)
		}
		has[key(r)] = true
	}
	seen := map[string]bool{}
	for _, r := range before {
		if !has[key(r)] && !seen[key(r)] {
			removed = append(removed, r)
		}
		seen[key(r)] = true
	}
	return added, removed
}

func (s *service) AddUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	if role.ClientID == "" || role.Role == "" {
		return ErrInvalid
	}
	person, err := s.dir.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !s.directUserRoles && !person.ServiceAccount {
		return directRoleRefused()
	}
	if err := s.noKeycloakAdminRole(ctx, []ClientRole{role}); err != nil {
		return err
	}
	return s.dir.AddUserExtraRole(ctx, id, role)
}

func (s *service) RemoveUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	return s.dir.RemoveUserExtraRole(ctx, id, role)
}

func (s *service) CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error) {
	if err := s.allow(p, authz.TypeUser, authz.Create); err != nil {
		return Person{}, err
	}
	if in.Email == "" {
		return Person{}, ErrInvalid
	}
	created, err := s.dir.CreateUser(ctx, in)
	if err != nil {
		return Person{}, err
	}
	shadow, _, err := s.assign.Ensure(ctx, created.ID, user.Profile{
		Email:       created.Email,
		FirstName:   created.FirstName,
		LastName:    created.LastName,
		SchoolEmail: created.SchoolEmail,
		SkyNumber:   created.SkyNumber,
	})
	if err != nil {
		_ = s.dir.DeleteUser(ctx, created.ID)
		return Person{}, err
	}
	created.SkyNumber = shadow.SkyNumber
	if s.mail != nil {
		s.mail.Welcome(ctx, shadow)
	}
	return created, nil
}

func (s *service) LogoutAllSessions(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	if _, err := s.dir.GetUser(ctx, id); err != nil {
		return err
	}
	return s.dir.LogoutAllSessions(ctx, id)
}

func (s *service) DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if err := s.allow(p, authz.TypeUser, authz.Delete); err != nil {
		return err
	}
	if !s.accountErasureEnabled {
		return ErrAccountErasureDisabled
	}
	var requestedBy *uuid.UUID
	if actorID, err := uuid.Parse(p.ID); err == nil {
		requestedBy = &actorID
	}
	request, err := s.users.RequestDeletion(ctx, id, requestedBy)
	if errors.Is(err, user.ErrNotFound) {
		person, directoryErr := s.dir.GetUser(ctx, id)
		if directoryErr != nil {
			return directoryErr
		}
		if _, _, ensureErr := s.assign.Ensure(ctx, id, user.Profile{
			Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
			Username: person.Username, SchoolEmail: person.SchoolEmail, SkyNumber: person.SkyNumber,
		}); ensureErr != nil {
			return ensureErr
		}
		request, err = s.users.RequestDeletion(ctx, id, requestedBy)
	}
	if err != nil {
		return err
	}
	if s.accessProjector == nil {
		return ErrAccountAccessUnavailable
	}
	if err := s.accessProjector.Project(ctx, request); err != nil {
		return fmt.Errorf("%w: %v", ErrAccountAccessUnavailable, err)
	}
	return nil
}

func (s *service) ListPublicTeams(ctx context.Context) ([]PublicTeam, error) {
	if err := s.allow(authz.Principal{}, authz.TypeTeam, authz.Read); err != nil {
		return nil, err
	}
	groups, err := s.dir.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PublicTeam, 0)
	for _, g := range groups {
		if !attrTrue(g.Attributes, "public_listing") {
			continue
		}
		out = append(out, PublicTeam{
			Team:        g.Name,
			Path:        g.Path,
			DisplayName: displayName(g),
		})
	}
	return out, nil
}

func (s *service) PublicMembers(ctx context.Context, team string) (Roster, error) {
	g, err := s.publicGroup(ctx, team, false)
	if err != nil {
		return Roster{}, err
	}
	people, err := s.collectMembers(ctx, g, true)
	if err != nil {
		return Roster{}, err
	}
	leaders, err := s.leaderIDs(ctx, g)
	if err != nil {
		return Roster{}, err
	}
	return s.buildRoster(ctx, g, people, leaders)
}

func (s *service) PublicLeaders(ctx context.Context, team string) (Roster, error) {
	g, err := s.publicGroup(ctx, team, true)
	if err != nil {
		return Roster{}, err
	}
	order, err := s.leaders(ctx, g)
	if err != nil {
		return Roster{}, err
	}
	people := make([]Person, 0, len(order))
	for _, id := range order {
		p, err := s.dir.GetUser(ctx, id)
		if err != nil {
			continue
		}
		people = append(people, p)
	}
	return s.buildRoster(ctx, g, people, idSet(order))
}

func (s *service) publicGroup(ctx context.Context, team string, leaders bool) (Group, error) {
	if err := s.allow(authz.Principal{}, authz.TypeTeam, authz.Read); err != nil {
		return Group{}, err
	}
	g, err := s.dir.GetGroup(ctx, team)
	if err != nil {
		return Group{}, ErrNotFound
	}
	if leaders {
		if !attrTrue(g.Attributes, "public_listing") && !attrTrue(g.Attributes, "public_leaders") {
			return Group{}, ErrNotFound
		}
		return g, nil
	}
	if !attrTrue(g.Attributes, "public_listing") {
		return Group{}, ErrNotFound
	}
	return g, nil
}

type memberHit struct {
	person Person
	source Group
}

func (s *service) memberHits(ctx context.Context, root Group) ([]memberHit, error) {
	groups := []Group{root}
	desc, err := s.descendants(ctx, root)
	if err != nil {
		return nil, err
	}
	groups = append(groups, desc...)
	slices.SortFunc(groups, func(a, b Group) int {
		da, db := strings.Count(a.Path, "/"), strings.Count(b.Path, "/")
		if da != db {
			return da - db
		}
		return strings.Compare(a.Path, b.Path)
	})
	best := make(map[uuid.UUID]memberHit)
	order := make([]uuid.UUID, 0)
	for _, g := range groups {
		members, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range members {
			if _, ok := best[p.ID]; ok {
				continue
			}
			best[p.ID] = memberHit{person: p, source: g}
			order = append(order, p.ID)
		}
	}
	out := make([]memberHit, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out, nil
}

func rosterMembers(root Group, hits []memberHit) []GroupMember {
	out := make([]GroupMember, 0, len(hits))
	for _, h := range hits {
		m := GroupMember{Person: h.person}
		if h.source.ID != root.ID {
			m.SourceGroupID = h.source.ID
			m.SourceGroupPath = h.source.Path
		}
		out = append(out, m)
	}
	return out
}

func (s *service) collectMembers(ctx context.Context, root Group, recursive bool) ([]Person, error) {
	if !recursive {
		return s.dir.Members(ctx, root.ID)
	}
	hits, err := s.memberHits(ctx, root)
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.person)
	}
	return out, nil
}

func (s *service) descendants(ctx context.Context, root Group) ([]Group, error) {
	children, err := s.dir.Subgroups(ctx, root.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(children))
	for _, child := range children {
		out = append(out, child)
		nested, err := s.descendants(ctx, child)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func (s *service) leaderIDs(ctx context.Context, team Group) (map[uuid.UUID]struct{}, error) {
	order, err := s.leaders(ctx, team)
	if err != nil {
		return nil, err
	}
	return idSet(order), nil
}

// leaders are a team's leaders, each once, in one order on every read:
// the leader subgroups in the policy's order (LIDERLER, then
// KOORDINATORLER), and in each by username, then by ID, the order /members
// lists a group in (Keycloak answers a group's members by username).
// Someone in two leader subgroups stands where first seen.
func (s *service) leaders(ctx context.Context, team Group) ([]uuid.UUID, error) {
	children, err := s.dir.Subgroups(ctx, team.ID)
	if err != nil {
		return nil, err
	}
	seen := make(map[uuid.UUID]struct{})
	out := make([]uuid.UUID, 0)
	for _, sub := range authz.DefaultPolicy().LeaderSubgroups {
		for _, child := range children {
			if child.Name != sub {
				continue
			}
			members, err := s.dir.Members(ctx, child.ID)
			if err != nil {
				return nil, err
			}
			slices.SortFunc(members, byUsername)
			for _, p := range members {
				if _, ok := seen[p.ID]; ok {
					continue
				}
				seen[p.ID] = struct{}{}
				out = append(out, p.ID)
			}
		}
	}
	return out, nil
}

// byUsername orders people by username, then by ID for people without one.
func byUsername(a, b Person) int {
	if c := strings.Compare(a.Username, b.Username); c != 0 {
		return c
	}
	return bytes.Compare(a.ID[:], b.ID[:])
}

func idSet(ids []uuid.UUID) map[uuid.UUID]struct{} {
	set := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func attrTrue(attrs map[string]string, key string) bool {
	if attrs == nil {
		return false
	}
	return strings.EqualFold(attrs[key], "true")
}

func displayName(g Group) LocalizedText {
	tr := g.Name
	en := ""
	if g.Attributes != nil {
		if v := g.Attributes["display_name_tr"]; v != "" {
			tr = v
		}
		en = g.Attributes["display_name_en"]
	}
	return LocalizedText{TR: tr, EN: en}
}

func description(g Group) *LocalizedText {
	if g.Attributes == nil {
		return nil
	}
	tr := g.Attributes["description_tr"]
	en := g.Attributes["description_en"]
	if tr == "" && en == "" {
		return nil
	}
	return &LocalizedText{TR: tr, EN: en}
}

// buildRoster is the public list of a team's people, read with core's
// profiles in one query (Store.Accounts). Someone core may no longer show
// (erased or being erased) is no member of it, nor is the placeholder
// subject, while Keycloak still holds them in the Group. Without core's
// rows the list cannot say who that is, and is not answered.
func (s *service) buildRoster(ctx context.Context, g Group, people []Person, leaders map[uuid.UUID]struct{}) (Roster, error) {
	ids := make([]uuid.UUID, 0, len(people))
	for _, p := range people {
		ids = append(ids, p.ID)
	}
	accounts, err := s.users.Accounts(ctx, ids)
	if err != nil {
		return Roster{}, err
	}
	members := make([]PublicMember, 0, len(people))
	for _, p := range people {
		account := accounts[p.ID]
		if p.ID == user.DeletedSubject || account.Blocked {
			continue
		}
		_, leader := leaders[p.ID]
		m := PublicMember{
			FirstName: p.FirstName,
			LastName:  p.LastName,
			Leader:    leader,
		}
		if account.Stored {
			shadow := account.User
			m.FirstName = shadow.FirstName
			m.LastName = shadow.LastName
			m.Linkedin = shadow.Linkedin
			m.University = shadow.University
			m.Faculty = shadow.Faculty
			m.Department = shadow.Department
			pictured := withProfilePicture(Person{ID: p.ID}, shadow)
			m.ProfilePictureURL, m.ProfilePictureSizes = pictured.ProfilePictureURL, pictured.ProfilePictureSizes
		}
		members = append(members, m)
	}
	return Roster{
		Team:        g.Name,
		DisplayName: displayName(g),
		Description: description(g),
		Count:       len(members),
		Members:     members,
	}, nil
}

// overlayShadow puts core's profile over the directory's people and leaves
// out the people who are erased or being erased: a list is for finding
// people, and the full list (ListUsers) never had them either.
func (s *service) overlayShadow(ctx context.Context, people []Person) ([]Person, error) {
	out := make([]Person, 0, len(people))
	for _, person := range people {
		shadow, err := s.users.Get(ctx, person.ID)
		if err != nil {
			out = append(out, person)
			continue
		}
		if shadow.AccountState != user.AccountActive {
			continue
		}
		person.FirstName = shadow.FirstName
		person.LastName = shadow.LastName
		if shadow.SchoolEmail != "" {
			person.SchoolEmail = shadow.SchoolEmail
		}
		if shadow.SkyNumber != "" {
			person.SkyNumber = shadow.SkyNumber
		}
		out = append(out, withProfilePicture(person, shadow))
	}
	return out, nil
}

func (s *service) mergeUserSearch(ctx context.Context, people []Person, q string) ([]Person, error) {
	people, err := s.overlayShadow(ctx, people)
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0)
	seen := map[uuid.UUID]int{}
	for _, person := range people {
		if !personMatches(person, q) {
			continue
		}
		seen[person.ID] = len(out)
		out = append(out, person)
	}
	found, err := s.users.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, u := range found {
		person := personFromUser(u)
		if i, ok := seen[u.ID]; ok {
			out[i].FirstName = person.FirstName
			out[i].LastName = person.LastName
			if person.SchoolEmail != "" {
				out[i].SchoolEmail = person.SchoolEmail
			}
			if person.SkyNumber != "" {
				out[i].SkyNumber = person.SkyNumber
			}
			out[i].ProfilePictureURL, out[i].ProfilePictureSizes = person.ProfilePictureURL, person.ProfilePictureSizes
			continue
		}
		seen[u.ID] = len(out)
		out = append(out, person)
	}
	return out, nil
}

// seatSearchLimit is how many people a seat search takes from Keycloak's
// search; each of them not already known as a holder costs one read of
// their effective roles.
const seatSearchLimit = 50

// seatSearchConcurrency bounds the effective role reads one search runs at
// once.
const seatSearchConcurrency = 8

// seatSearch answers the people matching q who hold clientID's role in
// effect, as Keycloak puts it in their token: directly, through a Group or
// a Group above theirs, through the default roles or a composite role at any
// depth. A skyforms: role is also held through skyforms:*. Keycloak's
// role/users and role/groups list only direct mappings, so the candidates
// are the direct holders (holders) plus Keycloak's search for q (and, for a
// full caller, core's own profile search), and each candidate who is not a
// direct holder is kept only if Keycloak's effective roles hold the role.
// People who are erased or being erased are left out. A failed read is an
// error, never a shorter list.
func (s *service) seatSearch(ctx context.Context, clientID, role, q string, full bool, holders []Person) ([]Person, error) {
	matches := personMatches
	if !full {
		matches = safePersonMatches
	}
	direct := make(map[uuid.UUID]struct{}, len(holders))
	for _, person := range holders {
		direct[person.ID] = struct{}{}
	}
	candidates := append([]Person(nil), holders...)
	seen := make(map[uuid.UUID]struct{}, len(holders))
	for _, person := range holders {
		seen[person.ID] = struct{}{}
	}
	add := func(person Person) {
		if person.ID == uuid.Nil {
			return
		}
		if _, ok := seen[person.ID]; ok {
			return
		}
		seen[person.ID] = struct{}{}
		candidates = append(candidates, person)
	}
	found, err := s.dir.SearchUsers(ctx, keycloakInfix(q), seatSearchLimit)
	if err != nil {
		return nil, err
	}
	for _, person := range found {
		add(person)
	}
	if full {
		shadows, err := s.users.Search(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, u := range shadows {
			add(personFromUser(u))
		}
	}
	candidates, err = s.overlayShadow(ctx, candidates)
	if err != nil {
		return nil, err
	}
	matched := make([]Person, 0, len(candidates))
	for _, person := range candidates {
		if matches(person, q) {
			matched = append(matched, person)
		}
	}
	keep := make([]bool, len(matched))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(seatSearchConcurrency)
	for i, person := range matched {
		if _, ok := direct[person.ID]; ok {
			keep[i] = true
			continue
		}
		g.Go(func() error {
			names, err := s.dir.EffectiveClientRoles(gctx, person.ID, clientID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			keep[i] = holdsSeat(names, role)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(matched))
	for i, person := range matched {
		if keep[i] {
			out = append(out, person)
		}
	}
	return out, nil
}

// holdsSeat reports whether the role names include role, or skyforms:* for
// a skyforms: role.
func holdsSeat(names []string, role string) bool {
	for _, name := range names {
		if name == role || (strings.HasPrefix(role, "skyforms:") && name == "skyforms:*") {
			return true
		}
	}
	return false
}

// keycloakInfix is the longest word of q as Keycloak's infix search
// (*word*): Keycloak's plain search is a prefix search of one string, so
// "ada love" or "love" would miss Ada Lovelace. The caller matches the whole
// query afterwards.
func keycloakInfix(q string) string {
	longest := ""
	for _, word := range strings.Fields(q) {
		word = strings.Trim(word, "*")
		if len([]rune(word)) > len([]rune(longest)) {
			longest = word
		}
	}
	return "*" + longest + "*"
}

func personMatches(p Person, q string) bool {
	n := strings.ToLower(q)
	for _, f := range []string{
		p.Email, p.FirstName, p.LastName, p.Username, p.SchoolEmail, p.SkyNumber,
		strings.TrimSpace(p.FirstName + " " + p.LastName),
	} {
		if strings.Contains(strings.ToLower(f), n) {
			return true
		}
	}
	return false
}

func personFromUser(u user.User) Person {
	return withProfilePicture(Person{
		ID:          u.ID,
		Email:       u.Email,
		FirstName:   u.FirstName,
		LastName:    u.LastName,
		Username:    u.Username,
		SchoolEmail: u.SchoolEmail,
		SkyNumber:   u.SkyNumber,
	}, u)
}

// profilePicture is the profile picture core stores for a person as every
// people read answers it (the public team list, the user reads): its
// address and its card and page addresses, from the base and address mode
// core is configured with (identity has no media dependency to carry its
// own). Both are empty for a profile without a picture.
func profilePicture(shadow user.User) (string, map[string]media.ImageAddress) {
	addresses := media.ConfiguredAddresses()
	return addresses.Object(shadow.ProfilePictureURL), addresses.LinkedSizes(shadow.ProfilePicture)
}

// withProfilePicture puts on person the picture of their profile, shadow,
// read with it: none when shadow is another person's (an e-mail conflict),
// or a profile core no longer shows (erased or being erased), or for the
// placeholder subject.
func withProfilePicture(person Person, shadow user.User) Person {
	if shadow.ID != person.ID || person.ID == user.DeletedSubject || shadow.AccountState != user.AccountActive {
		return person
	}
	person.ProfilePictureURL, person.ProfilePictureSizes = profilePicture(shadow)
	return person
}
