package teammail

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The privilege notice (docs/privilege-change-mail.md; grup-yonetimi 05,
// Yusuf 2026-10-12: "UYELER'e mail gitmemeli; sektördeki gibi"):
//
//   - A Group's role mappings changed (any Group, /UYELER too): ADMIN alone
//     is told, never the Group's members (rule 1, as Microsoft PIM).
//   - A person added to or removed from a Privileged Group (a root of
//     AUTHZ_PRIVILEGED_ROOTS or a Group under one): ADMIN and every member
//     of that root's tree are told (rule 2). The person changed gets the
//     role membership mail instead (club.role-membership).
//
// Whoever made the change is left out; so is the person changed. Each
// recipient gets one mail per change, however many roots they sit in.

// NoticeEvent is what a privilege notice tells of.
type NoticeEvent string

const (
	NoticeMemberAdded   NoticeEvent = "member_added"
	NoticeMemberRemoved NoticeEvent = "member_removed"
	NoticeRolesChanged  NoticeEvent = "roles_changed"
)

// noticeChange is the template's Change variable of each event.
var noticeChange = map[NoticeEvent]string{
	NoticeMemberAdded:   "member-added",
	NoticeMemberRemoved: "member-removed",
	NoticeRolesChanged:  "roles-changed",
}

// Notice is one queued privilege notice: a change waiting to be read for
// its recipients (RecipientID nil), or its mail to one recipient. Ids, a
// Group path and role names only.
type Notice struct {
	ID       uuid.UUID
	ChangeID uuid.UUID
	// RecipientID is who the mail goes to; nil until the change is read for
	// its recipients (Expand).
	RecipientID *uuid.UUID
	Event       NoticeEvent
	GroupPath   string
	// SubjectID is the person added or removed (member events).
	SubjectID *uuid.UUID
	ActorID   *uuid.UUID
	// RolesAdded and RolesRemoved are "client:role", sorted (roles_changed).
	RolesAdded   []string
	RolesRemoved []string
	// MemberCount is how many people are in the Group's tree when the
	// change was read for its recipients (roles_changed).
	MemberCount   *int
	OccurredAt    time.Time
	NextAttemptAt time.Time
	Attempts      int
	ClaimedAt     time.Time
}

// NoticeQueue keeps the privilege notices waiting (PostgresNoticeQueue keeps
// them in privilege_notice_mails). Claim, Complete, Retry, DropBefore and
// Backlog work as the membership Queue's.
type NoticeQueue interface {
	// Enqueue queues a change, without a recipient, due at once.
	Enqueue(ctx context.Context, change Notice) error
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Notice, error)
	// Expand replaces the claimed change by one notice per recipient, each
	// carrying memberCount and due at at. A claim another core has taken
	// since changes nothing (0).
	Expand(ctx context.Context, claimed Notice, recipients []uuid.UUID, memberCount *int, at time.Time) (int, error)
	Complete(ctx context.Context, claimed Notice) error
	Retry(ctx context.Context, claimed Notice, at time.Time) error
	DropBefore(ctx context.Context, occurredBefore time.Time) (int, error)
	Backlog(ctx context.Context, now time.Time) (int, time.Duration, error)
}

// NoticeMailer sends the privilege notice (mail.SkyMail).
type NoticeMailer interface {
	// PrivilegeNoticeConfigured reports whether the notice has a template;
	// without one it is off.
	PrivilegeNoticeConfigured() bool
	PrivilegeNotice(ctx context.Context, recipient, fullName string, vars map[string]string) error
}

// Rosters reads who sits in a Group's tree.
type Rosters interface {
	// MemberIDs are the people in the Group at path or in any Group under
	// it, each once. A Group that does not exist has nobody.
	MemberIDs(ctx context.Context, path string) ([]uuid.UUID, error)
}

// NoticeConfig turns the privilege notice on (Config.Notice).
type NoticeConfig struct {
	Queue   NoticeQueue
	Rosters Rosters
	Mail    NoticeMailer
}

// RosterDirectory is what DirectoryRosters reads (identity.Directory).
type RosterDirectory interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
	Subgroups(ctx context.Context, groupID string) ([]identity.Group, error)
	Members(ctx context.Context, groupID string) ([]identity.Person, error)
}

// DirectoryRosters reads rosters from the directory (Keycloak): the Group,
// every Group under it, and each one's direct members.
type DirectoryRosters struct {
	Directory RosterDirectory
}

func (r DirectoryRosters) MemberIDs(ctx context.Context, path string) ([]uuid.UUID, error) {
	root, err := r.Directory.GetGroup(ctx, path)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	pending := []identity.Group{root}
	for len(pending) > 0 {
		g := pending[0]
		pending = pending[1:]
		members, err := r.Directory.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m.ID)
			}
		}
		children, err := r.Directory.Subgroups(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		pending = append(pending, children...)
	}
	return out, nil
}

// roleNames are roles as "client:role", sorted, each once.
func roleNames(roles []identity.ClientRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.ClientID+":"+r.Role)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// adminRole reports whether a changed role is a management role by authz's
// rule (authz.ProtectedRole: core's permission roles, audit:read,
// content:write, any client:admin, realm-management). A role that holds one as a composite is
// not seen here: the mail says "yes" only when it is sure.
func adminRole(names ...[]string) bool {
	for _, list := range names {
		for _, name := range list {
			client, role, ok := strings.Cut(name, ":")
			if ok && authz.ProtectedRole(client, role) {
				return true
			}
		}
	}
	return false
}

// adminRoots are the Privileged roots named ADMIN: whoever sits in their
// trees is told of every privilege change.
func adminRoots(roots []string) []string {
	var out []string
	for _, root := range roots {
		if authz.RootLabel(root) == "ADMIN" {
			out = append(out, root)
		}
	}
	return out
}

// noticeRecipients are who the change n goes to (rule 1 or 2 above), each
// once, without its actor and its subject; and, for a role mapping, how
// many people sit in the Group's tree.
func (s *Service) noticeRecipients(ctx context.Context, n Notice) ([]uuid.UUID, *int, error) {
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	if n.ActorID != nil {
		seen[*n.ActorID] = true
	}
	if n.SubjectID != nil {
		seen[*n.SubjectID] = true
	}
	add := func(path string) (int, error) {
		ids, err := s.notice.Rosters.MemberIDs(ctx, path)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return len(ids), nil
	}
	for _, root := range adminRoots(s.roots) {
		if _, err := add(root); err != nil {
			return nil, nil, err
		}
	}
	switch n.Event {
	case NoticeRolesChanged:
		// The Group's members are counted, never told. The count is best
		// effort: a tree that cannot be read (a large /UYELER, a slow
		// Keycloak) leaves it out, and ADMIN is told all the same.
		countCtx, cancel := context.WithTimeout(ctx, countTimeout)
		ids, err := s.notice.Rosters.MemberIDs(countCtx, n.GroupPath)
		cancel()
		if err != nil {
			s.count(func() { s.notices.countErrors++ })
			s.logger.Printf("privilege notice: a Group's members could not be counted; the notice goes without the count")
			return out, nil, nil
		}
		count := len(ids)
		return out, &count, nil
	default:
		if root := authz.PrivilegedRoot(s.roots, n.GroupPath); root != "" {
			if _, err := add(root); err != nil {
				return nil, nil, err
			}
		}
		return out, nil, nil
	}
}

// countTimeout bounds the count of a Group's tree, so that a slow one
// leaves the rest of the send's time (sendTimeout) to the mails.
const countTimeout = 20 * time.Second

// noticeVars are the template's variables of a notice to one recipient.
// Every key is always there, empty when it does not apply.
func (s *Service) noticeVars(ctx context.Context, n Notice) map[string]string {
	group := s.group(ctx, n.GroupPath)
	vars := map[string]string{
		"Change":       noticeChange[n.Event],
		"GroupName":    groupName(group),
		"GroupId":      group.ID,
		"Root":         "",
		"SubjectName":  "",
		"ActorName":    "",
		"OccurredAt":   n.OccurredAt.In(s.location).Format(dateTimeLayout),
		"RolesAdded":   strings.Join(n.RolesAdded, ", "),
		"RolesRemoved": strings.Join(n.RolesRemoved, ", "),
		"AdminRole":    "no",
		"MemberCount":  "",
	}
	if n.ActorID != nil {
		vars["ActorName"] = s.people.DisplayName(ctx, *n.ActorID)
	}
	switch n.Event {
	case NoticeRolesChanged:
		if adminRole(n.RolesAdded, n.RolesRemoved) {
			vars["AdminRole"] = "yes"
		}
		if n.MemberCount != nil {
			vars["MemberCount"] = strconv.Itoa(*n.MemberCount)
		}
	default:
		vars["Root"] = privilegedRole(n.GroupPath, s.roots)
		if n.SubjectID != nil {
			vars["SubjectName"] = s.people.DisplayName(ctx, *n.SubjectID)
		}
	}
	return vars
}

// dateTimeLayout is when a notice's change happened, in Europe/Istanbul.
const dateTimeLayout = "02.01.2006 15:04"

// MemoryNoticeQueue is a NoticeQueue in memory, for tests.
type MemoryNoticeQueue struct {
	mu      sync.Mutex
	notices []Notice
}

func NewMemoryNoticeQueue() *MemoryNoticeQueue { return &MemoryNoticeQueue{} }

// Snapshot is a copy of the queued notices, in queue order.
func (q *MemoryNoticeQueue) Snapshot() []Notice {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.notices)
}

func (q *MemoryNoticeQueue) Enqueue(_ context.Context, change Notice) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	change.RecipientID = nil
	change.NextAttemptAt = change.OccurredAt
	change.ClaimedAt = time.Time{}
	q.notices = append(q.notices, change)
	return nil
}

func (q *MemoryNoticeQueue) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]Notice, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	due := make([]int, 0)
	for i, n := range q.notices {
		if !n.NextAttemptAt.After(now) {
			due = append(due, i)
		}
	}
	slices.SortStableFunc(due, func(a, b int) int { return q.notices[a].NextAttemptAt.Compare(q.notices[b].NextAttemptAt) })
	out := make([]Notice, 0, min(limit, len(due)))
	for _, i := range due[:min(limit, len(due))] {
		q.notices[i].NextAttemptAt = now.Add(lease)
		q.notices[i].ClaimedAt = now
		out = append(out, q.notices[i])
	}
	return out, nil
}

func (q *MemoryNoticeQueue) Expand(_ context.Context, claimed Notice, recipients []uuid.UUID, memberCount *int, at time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := slices.IndexFunc(q.notices, func(n Notice) bool {
		return n.ID == claimed.ID && n.RecipientID == nil && n.ClaimedAt.Equal(claimed.ClaimedAt)
	})
	if i < 0 {
		return 0, nil
	}
	change := q.notices[i]
	q.notices = slices.Delete(q.notices, i, i+1)
	added := 0
	for _, r := range recipients {
		if slices.ContainsFunc(q.notices, func(n Notice) bool {
			return n.ChangeID == change.ChangeID && n.RecipientID != nil && *n.RecipientID == r
		}) {
			continue
		}
		n := change
		n.ID = uuid.New()
		recipient := r
		n.RecipientID = &recipient
		n.MemberCount = memberCount
		n.NextAttemptAt = at
		n.Attempts = 0
		n.ClaimedAt = time.Time{}
		q.notices = append(q.notices, n)
		added++
	}
	return added, nil
}

func (q *MemoryNoticeQueue) Complete(_ context.Context, claimed Notice) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.notices = slices.DeleteFunc(q.notices, func(n Notice) bool { return n.ID == claimed.ID && n.ClaimedAt.Equal(claimed.ClaimedAt) })
	return nil
}

func (q *MemoryNoticeQueue) Retry(_ context.Context, claimed Notice, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.notices {
		if q.notices[i].ID == claimed.ID && q.notices[i].ClaimedAt.Equal(claimed.ClaimedAt) {
			q.notices[i].Attempts++
			q.notices[i].NextAttemptAt = at
			q.notices[i].ClaimedAt = time.Time{}
		}
	}
	return nil
}

func (q *MemoryNoticeQueue) DropBefore(_ context.Context, occurredBefore time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	before := len(q.notices)
	q.notices = slices.DeleteFunc(q.notices, func(n Notice) bool { return n.OccurredAt.Before(occurredBefore) })
	return before - len(q.notices), nil
}

func (q *MemoryNoticeQueue) Backlog(_ context.Context, now time.Time) (int, time.Duration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.notices) == 0 {
		return 0, 0, nil
	}
	oldest := q.notices[0].OccurredAt
	for _, n := range q.notices {
		if n.OccurredAt.Before(oldest) {
			oldest = n.OccurredAt
		}
	}
	return len(q.notices), max(now.Sub(oldest), 0), nil
}

var _ NoticeQueue = (*MemoryNoticeQueue)(nil)
