package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// The reading route of the Team audit record (docs/team-audit.md,
// ekip-yetki-devri ticket 10): GET /v1/team-audit-records. Only the
// audit:read role reads it, in every role mode (authz.TypeTeamAudit): no
// Privileged Group stands for it, global:read neither, and no Leader reads
// even their own Teams' history (S7). Rows hold subjects only; names are
// read from core's users as the page is built, and nothing else of a
// person: no address, no number.

// AuditQuery is the route's filters, as the caller sent them.
type AuditQuery struct {
	// GroupID is a Group's Keycloak id; with Subtree, its Sub-teams' rows
	// too.
	GroupID string
	Subtree bool
	ActorID uuid.UUID
	// UserID is the person a row is about (the target).
	UserID  uuid.UUID
	Actions []string
	Outcome string
	// From is inclusive, To exclusive; zero is open.
	From, To time.Time
	// Cursor is the token of the page before, "" for the first.
	Cursor string
	// Limit is the page size; zero is teamaudit.MaxPage.
	Limit int
	// Malformed is a filter the route could not read: 400 once the
	// caller may read the record.
	Malformed bool
}

// AuditPerson is a person of a row: their subject and the name core holds
// now, user.DeletedDisplayName for an erased or blocked account, "" when
// core has no row of them (or for core's own job).
type AuditPerson struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"displayName"`
}

// AuditActor is who made the change and on what basis.
type AuditActor struct {
	AuditPerson
	// Basis is privileged, leader, none (a denied row) or system.
	Basis string `json:"basis"`
	// ViaGroupID is, for a Leader, the Team that grants the change.
	ViaGroupID string `json:"viaGroupId,omitempty"`
	// Client is the token's azp, or core's job (system:term-expiry).
	Client string `json:"client,omitempty"`
}

// AuditGroup is a row's Group: its path at the time and, while the Group
// still exists, its path now (null once it is gone).
type AuditGroup struct {
	ID          string  `json:"id,omitempty"`
	Path        string  `json:"path"`
	CurrentPath *string `json:"currentPath"`
}

// AuditEntry is one row as the route answers it.
type AuditEntry struct {
	ID         uuid.UUID  `json:"id"`
	OccurredAt time.Time  `json:"occurredAt"`
	Action     string     `json:"action"`
	Actor      AuditActor `json:"actor"`
	// Target is the person of a member, leader, term or app row.
	Target *AuditPerson `json:"target"`
	// Successor is who a membership.term.handover handed the term to
	// (after.successorId, named); nil on every other row.
	Successor *AuditPerson `json:"successor"`
	// Group is nil for an app row about a person.
	Group         *AuditGroup      `json:"group"`
	Before        *teamaudit.State `json:"before"`
	After         *teamaudit.State `json:"after"`
	Outcome       string           `json:"outcome"`
	ErrorCode     string           `json:"errorCode,omitempty"`
	CorrelationID string           `json:"correlationId,omitempty"`
}

// AuditPage is a page and the token of the next one ("" on the last).
type AuditPage struct {
	Items []AuditEntry
	Next  string
}

func (s *service) TeamAuditRecords(ctx context.Context, p authz.Principal, in AuditQuery) (AuditPage, error) {
	if err := s.allow(p, authz.TypeTeamAudit, authz.Read); err != nil {
		return AuditPage{}, err
	}
	q, err := auditQuery(in)
	if err != nil {
		return AuditPage{}, err
	}
	page := AuditPage{Items: []AuditEntry{}}
	if s.auditRecords == nil {
		return page, nil
	}
	// all is the realm's Groups once a read has listed them: the subtree's
	// list gives the page's current paths too.
	var all []Group
	if in.Subtree {
		if q.GroupIDs, q.GroupPath, all, err = s.auditSubtree(ctx, in.GroupID); err != nil {
			return AuditPage{}, err
		}
	}
	rows, next, err := s.auditRecords.Records(ctx, q)
	if err != nil {
		return AuditPage{}, err
	}
	people, groups := []uuid.UUID{}, []string{}
	for _, r := range rows {
		if r.Actor.ID != uuid.Nil {
			people = append(people, r.Actor.ID)
		}
		if r.TargetUserID != uuid.Nil {
			people = append(people, r.TargetUserID)
		}
		if id, ok := successorOf(r); ok {
			people = append(people, id)
		}
		if r.GroupID != "" {
			groups = append(groups, r.GroupID)
		}
	}
	slices.SortFunc(people, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	names, err := s.accountNames(ctx, slices.Compact(people))
	if err != nil {
		return AuditPage{}, err
	}
	slices.Sort(groups)
	paths, err := s.currentPaths(ctx, slices.Compact(groups), all)
	if err != nil {
		return AuditPage{}, err
	}
	for _, r := range rows {
		e := AuditEntry{
			ID: r.ID, OccurredAt: r.OccurredAt, Action: string(r.Action), Outcome: string(r.Outcome),
			ErrorCode: r.ErrorCode, CorrelationID: r.Actor.CorrelationID, Before: r.Before, After: r.After,
			Actor: AuditActor{
				AuditPerson: AuditPerson{ID: r.Actor.ID, DisplayName: names[r.Actor.ID]},
				Basis:       string(r.Actor.Basis), ViaGroupID: r.Actor.ViaGroupID, Client: r.Actor.Client,
			},
		}
		if r.TargetUserID != uuid.Nil {
			e.Target = &AuditPerson{ID: r.TargetUserID, DisplayName: names[r.TargetUserID]}
		}
		if id, ok := successorOf(r); ok {
			e.Successor = &AuditPerson{ID: id, DisplayName: names[id]}
		}
		if r.GroupID != "" || r.GroupPath != "" {
			e.Group = &AuditGroup{ID: r.GroupID, Path: r.GroupPath}
			if now, ok := paths[r.GroupID]; ok {
				e.Group.CurrentPath = &now
			}
		}
		page.Items = append(page.Items, e)
	}
	page.Next = next.String()
	return page, nil
}

// successorOf is the person a membership.term.handover row handed the term
// to.
func successorOf(r teamaudit.Row) (uuid.UUID, bool) {
	if r.Action != teamaudit.ActionTermHandover || r.After == nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(r.After.SuccessorID)
	return id, err == nil && id != uuid.Nil
}

// auditQuery checks the caller's filters: anything it does not know is
// ErrInvalid (400).
func auditQuery(in AuditQuery) (teamaudit.Query, error) {
	q := teamaudit.Query{ActorID: in.ActorID, TargetUserID: in.UserID, From: in.From, To: in.To, Limit: in.Limit}
	if in.Malformed {
		return q, ErrInvalid
	}
	if in.GroupID != "" {
		if _, err := uuid.Parse(in.GroupID); err != nil {
			return q, ErrInvalid
		}
		q.GroupIDs = []string{in.GroupID}
	} else if in.Subtree {
		return q, ErrInvalid
	}
	for _, raw := range in.Actions {
		a := teamaudit.Action(raw)
		if !slices.Contains(teamaudit.Actions, a) {
			return q, ErrInvalid
		}
		if !slices.Contains(q.Actions, a) {
			q.Actions = append(q.Actions, a)
		}
	}
	switch o := teamaudit.Outcome(in.Outcome); o {
	case "", teamaudit.OutcomePending, teamaudit.OutcomeOK, teamaudit.OutcomeFailed, teamaudit.OutcomeDenied:
		q.Outcome = o
	default:
		return q, ErrInvalid
	}
	if !in.From.IsZero() && !in.To.IsZero() && !in.From.Before(in.To) {
		return q, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = teamaudit.MaxPage
	}
	if q.Limit < 1 || q.Limit > teamaudit.MaxPage {
		return q, ErrInvalid
	}
	if in.Cursor != "" {
		after, err := teamaudit.ParseCursor(in.Cursor)
		if err != nil {
			return q, ErrInvalid
		}
		q.After = after
	}
	return q, nil
}

// auditSubtree is the Group id's subtree as a read of the record filters
// it: the Group and every Group under it now, and its path now, so a row
// of a Sub-team deleted since (its path at the time under the Group's)
// stays in the history. A Group that is gone is its own rows and the rows
// under the path of its newest row. The realm's Groups, as it read them,
// come back too.
func (s *service) auditSubtree(ctx context.Context, id string) ([]string, string, []Group, error) {
	all, err := s.dir.ListGroups(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	path := ""
	for _, g := range all {
		if g.ID == id {
			path = g.Path
			break
		}
	}
	ids := []string{id}
	if path == "" {
		newest, _, err := s.auditRecords.Records(ctx, teamaudit.Query{GroupIDs: ids, Limit: 1})
		if err != nil {
			return nil, "", nil, err
		}
		if len(newest) == 1 {
			path = newest[0].GroupPath
		}
		return ids, path, all, nil
	}
	for _, g := range all {
		if strings.HasPrefix(g.Path, path+"/") {
			ids = append(ids, g.ID)
		}
	}
	return ids, path, all, nil
}

// currentPaths are the paths now of the Groups ids that still exist, read
// from all, the realm's Groups, when the caller has listed them. Otherwise
// one Group is read by its id, and more in one list of the realm: a page
// holds up to a hundred Groups, and Keycloak answers one list faster than
// that many reads (review of #333, T2).
func (s *service) currentPaths(ctx context.Context, ids []string, all []Group) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	if all == nil && len(ids) == 1 {
		g, err := s.dir.GetGroup(ctx, ids[0])
		switch {
		case errors.Is(err, ErrNotFound):
			return out, nil
		case err != nil:
			return nil, err
		}
		out[ids[0]] = g.Path
		return out, nil
	}
	if all == nil {
		var err error
		if all, err = s.dir.ListGroups(ctx); err != nil {
			return nil, err
		}
	}
	for _, g := range all {
		if slices.Contains(ids, g.ID) {
			out[g.ID] = g.Path
		}
	}
	return out, nil
}
