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

// TeamPageConsents answers what people decided about the team page
// consent (consent.Service.TeamPageDecisions; aydınlatma s6 row 4 b): true
// for an open grant, false for one who withdrew it, absent for one who never
// gave it.
type TeamPageConsents interface {
	TeamPageDecisions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// TeamPageVisibility is who the public rosters (GET /v1/teams/{team}/members
// and /leaders, read by SkyApp without a sign-in; the site reads neither) show
// (docs/team-page-consent.md):
//
//   - a person with an open team page consent, in full;
//   - on the Board's own roster (a team in or under a YK Privileged root),
//     a Board member without one by first and last name only: the Board's
//     names and roles are public on the club's legitimate interest (s6 row
//     4 a, KVKK art. 5/2-f; Yusuf, 2026-10-27). Their picture, LinkedIn,
//     school and leader flag follow the consent like everyone's, and on
//     every other team's roster they are shown only with it;
//   - nobody else.
//
// Until GraceUntil the rosters still show in full everyone who has not
// decided, as before the consent (the transition Yusuf chose on 2026-10-27:
// the members listed then stay listed for 14 days while they are told, then
// those who did not consent are hidden). Who withdrew is hidden at once,
// during the transition too.
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
	grace := !rule.GraceUntil.IsZero() && rule.now().Before(rule.GraceUntil)
	decisions, err := rule.Consents.TeamPageDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	board := s.boardRoster(team)
	for _, id := range ids {
		open, decided := decisions[id]
		switch {
		case open, grace && !decided:
			out[id] = shownFull
		case board:
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

// boardRoster reports whether team is the Board's own: a YK Privileged root
// (/YK, /UYELER/YK) or a Group under one.
func (s *service) boardRoster(team Group) bool {
	root := authz.PrivilegedRoot(s.privilegedRoots, team.Path)
	return root != "" && authz.RootLabel(root) == "YK"
}
