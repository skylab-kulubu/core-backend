package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// TeamPageConsents answers which people hold an open team page consent
// (consent.Service.TeamPageAccounts; aydınlatma s6 row 4 b): only they are
// shown on their teams' public pages in full.
type TeamPageConsents interface {
	TeamPageAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// TeamPageVisibility is who the public rosters (GET /v1/teams/{team}/members
// and /leaders, read by the site and SkyApp without a sign-in) show
// (docs/team-page-consent.md):
//
//   - a person with an open team page consent, in full;
//   - a Board (YK) member without one, by first and last name and the leader
//     flag only: their name and role are public on the club's legitimate
//     interest (s6 row 4 a, KVKK art. 5/2-f); their picture, LinkedIn and
//     school follow the consent like everyone's;
//   - nobody else.
//
// Until GraceUntil the rosters still show everyone in full, as before the
// consent (the transition Yusuf chose on 2026-10-27: the members listed then
// stay listed for 14 days while they are told, then those who did not
// consent are hidden).
type TeamPageVisibility struct {
	Consents TeamPageConsents
	// GraceUntil is the end of the transition; zero: none, the rule
	// holds from the start.
	GraceUntil time.Time
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// TeamPageGraceUntilEnv is the end of the transition, an RFC 3339 time
// (2026-11-10T21:00:00Z). Unset: no transition, only consenting members
// (and the Board's names) are shown from the start.
const TeamPageGraceUntilEnv = "TEAM_PAGE_CONSENT_GRACE_UNTIL"

// TeamPageGraceUntilFromEnv reads TeamPageGraceUntilEnv: zero when unset,
// an error naming the variable when it is not an RFC 3339 time.
func TeamPageGraceUntilFromEnv(getenv func(string) string) (time.Time, error) {
	raw := strings.TrimSpace(getenv(TeamPageGraceUntilEnv))
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 time (2026-11-10T21:00:00Z)", TeamPageGraceUntilEnv)
	}
	return at, nil
}

// errNoTeamPageConsents is a TeamPageVisibility without a consent source:
// a configuration error, never an unfiltered roster.
var errNoTeamPageConsents = errors.New("identity: the team page rule has no consent source")

// shown is how a public roster shows a person.
type shown int

const (
	shownNot shown = iota
	shownName
	shownFull
)

// teamPageShown decides how each of ids is shown on a public roster. Without
// a TeamPageVisibility (tests and tools built with NewService) everyone is
// shown in full, as before the rule; production always sets one.
func (s *service) teamPageShown(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]shown, error) {
	out := make(map[uuid.UUID]shown, len(ids))
	rule := s.teamPage
	if rule == nil || (!rule.GraceUntil.IsZero() && rule.now().Before(rule.GraceUntil)) {
		for _, id := range ids {
			out[id] = shownFull
		}
		return out, nil
	}
	if rule.Consents == nil {
		return nil, errNoTeamPageConsents
	}
	consented, err := rule.Consents.TeamPageAccounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	var rest []uuid.UUID
	for _, id := range ids {
		if consented[id] {
			out[id] = shownFull
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return out, nil
	}
	board, err := s.boardMembers(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range rest {
		if board[id] {
			out[id] = shownName
		}
	}
	return out, nil
}

func (r *TeamPageVisibility) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// boardMembers is everyone in a YK Privileged root (/YK, /UYELER/YK) or in
// a Group under one.
func (s *service) boardMembers(ctx context.Context) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, root := range s.privilegedRoots {
		if authz.RootLabel(root) != "YK" {
			continue
		}
		g, err := s.dir.GetGroup(ctx, root)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.Path != root {
			// A search found a Group of that name elsewhere in the tree.
			continue
		}
		hits, err := s.memberHits(ctx, g)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			out[h.person.ID] = true
		}
	}
	return out, nil
}
