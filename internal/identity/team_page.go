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

// TeamPageConsents answers who objects to being shown on the team pages
// (consent.Service.TeamPageDecisions; aydınlatma s6 row 4 b): false for a
// person with an open objection, absent (or true) for everyone else.
type TeamPageConsents interface {
	TeamPageDecisions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// TeamPageVisibility is who the public rosters (GET /v1/teams/{team}/members
// and /leaders, read by SkyApp without a sign-in; the site reads neither) show
// (docs/team-page-consent.md):
//
//   - every member in full, on the club's legitimate interest (Yusuf,
//     2026-10-31, lawyer approved), unless they object;
//   - a member who objects (Account Center's switch turned off) nowhere, at
//     once; on the Board's own roster (a team in or under a YK Privileged
//     root) a Board member who objects stays by first and last name only:
//     the Board's names and roles are public on the club's legitimate
//     interest (s6 row 4 a, KVKK art. 5/2-f; Yusuf, 2026-10-27).
type TeamPageVisibility struct {
	Consents TeamPageConsents
}

// TeamPageGraceUntilEnv was the end of the transition of the consent rule
// (2026-10-27 to 2026-10-31): the rosters no longer read it, so it may stay
// set or go. Nothing reads it: the team page notice command
// (team-page-notice) refuses every run without reading the environment.
const TeamPageGraceUntilEnv = "TEAM_PAGE_CONSENT_GRACE_UNTIL"

// TeamPageGraceUntilFromEnv reads TeamPageGraceUntilEnv (no command calls
// it since the notice refuses every run): zero when unset, an error naming the variable when it is not an
// RFC 3339 time.
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

// teamPageShown decides how each of ids is shown on team's public roster.
// Without a TeamPageVisibility (tests and tools built with NewService)
// everyone is shown in full, as before the rule; production always sets one.
func (s *service) teamPageShown(ctx context.Context, team Group, ids []uuid.UUID) (map[uuid.UUID]shown, error) {
	out := make(map[uuid.UUID]shown, len(ids))
	rule := s.teamPage
	if rule == nil {
		for _, id := range ids {
			out[id] = shownFull
		}
		return out, nil
	}
	if rule.Consents == nil {
		return nil, errNoTeamPageConsents
	}
	decisions, err := rule.Consents.TeamPageDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	board := s.boardRoster(team)
	for _, id := range ids {
		visible, objected := decisions[id]
		switch {
		case !objected || visible:
			out[id] = shownFull
		case board:
			out[id] = shownName
		}
	}
	return out, nil
}

// boardRoster reports whether team is the Board's own: a YK Privileged root
// (/YK, /UYELER/YK) or a Group under one.
func (s *service) boardRoster(team Group) bool {
	root := authz.PrivilegedRoot(s.privilegedRoots, team.Path)
	return root != "" && authz.RootLabel(root) == "YK"
}
