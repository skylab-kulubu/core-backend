package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The group count report measures how many Group paths each user carries
// in a token, for the Group overage threshold (ADR-0059). It runs instead of
// the server, inside the running core container, which already has core's
// Keycloak service account in its environment; it only reads Keycloak. By
// default it prints counts only; -list-overage adds the users above the
// threshold by sub (docs/keycloak-admin-permissions.md, Group count report).
//
//	core-backend group-count-report [-list-overage]
const groupCountReportCommandName = "group-count-report"

// groupOverageThreshold is the most Group paths a token carries before it
// carries the Group overage marker instead (ADR-0059).
const groupOverageThreshold = 30

// runGroupCountReport wires the report to Keycloak through core's service
// account (read-only).
func runGroupCountReport(args []string, getenv func(string) string, out, errOut io.Writer) int {
	config, missing := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "%s needs %s\n", groupCountReportCommandName, strings.Join(missing, ", "))
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	keycloak := identity.NewKeycloak(config)
	return groupCountReportCommand(ctx, args, out, errOut, time.Now(), keycloak.ListUsers,
		func(ctx context.Context, id uuid.UUID) ([]string, error) {
			groups, err := keycloak.GroupsForUser(ctx, id)
			return identity.GroupPaths(groups), err
		})
}

// groupCountReportCommand reads the Group paths of every enabled user and
// writes the report to out, errors to errOut. A disabled user gets no token
// and is left out. It fails when a user's Groups could not be read.
func groupCountReportCommand(ctx context.Context, args []string, out, errOut io.Writer, now time.Time, users func(context.Context) ([]identity.Person, error), groupPaths func(context.Context, uuid.UUID) ([]string, error)) int {
	flags := flag.NewFlagSet(groupCountReportCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	listOverage := flags.Bool("list-overage", false, fmt.Sprintf("also list the users above %d paths by sub (personal data)", groupOverageThreshold))
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s takes no arguments, only flags\n", groupCountReportCommandName)
		return 2
	}
	people, err := users(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "keycloak: %v\n", err)
		return 1
	}
	var report groupCountReport
	for _, person := range people {
		if !person.Enabled {
			report.disabled++
			continue
		}
		paths, err := groupPaths(ctx, person.ID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			// Deleted since it was listed.
			continue
		case err != nil:
			// Unread, never counted as a user without Groups. The first
			// error says why (it names nobody); the rest are counted.
			report.unread++
			if report.unread == 1 {
				fmt.Fprintf(errOut, "keycloak: %v\n", err)
			}
			continue
		}
		report.add(person.ID, paths)
	}
	report.print(out, now)
	if *listOverage {
		report.printOverage(out)
	}
	if report.unread > 0 {
		return 1
	}
	return 0
}

// groupCountReport gathers each user's groups claim: how many Group paths
// it carries and how many bytes they take.
type groupCountReport struct {
	claims           []groupsClaim
	usersByPathCount map[int]int
	totalPaths       int
	totalPathBytes   int
	// unread counts the users whose Groups could not be read; disabled
	// the users left out.
	unread, disabled int
}

// groupsClaim is one user's groups claim.
type groupsClaim struct {
	sub       uuid.UUID
	pathCount int
	bytes     int
}

func (r *groupCountReport) add(sub uuid.UUID, paths []string) {
	pathBytes := 0
	for _, path := range paths {
		pathBytes += len(path)
	}
	if r.usersByPathCount == nil {
		r.usersByPathCount = map[int]int{}
	}
	r.claims = append(r.claims, groupsClaim{sub: sub, pathCount: len(paths), bytes: int(groupsClaimBytes(len(paths), float64(pathBytes)))})
	r.usersByPathCount[len(paths)]++
	r.totalPaths += len(paths)
	r.totalPathBytes += pathBytes
}

// groupsClaimBytes is the size of `"groups":["…","…"]` in a token's JSON
// for pathCount paths of pathBytes UTF-8 bytes in all: each path in
// quotes, commas between them.
func groupsClaimBytes(pathCount int, pathBytes float64) float64 {
	return float64(len(`"groups":[]`)) + pathBytes + float64(2*pathCount+max(pathCount-1, 0))
}

// inToken is roughly what a part of the token's JSON takes in the token,
// whose payload is base64url: four characters for every three bytes.
func inToken(jsonBytes float64) int {
	return int(math.Round(jsonBytes * 4 / 3))
}

// print writes counts only: no user, no Group path.
func (r *groupCountReport) print(out io.Writer, now time.Time) {
	fmt.Fprintf(out, "SKY LAB core group count report, %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "Direct Group paths per enabled user, as a token's groups claim carries them (full paths). Read from Keycloak; this report changes nothing.")
	fmt.Fprintf(out, "users: %d\n", len(r.claims)+r.unread)
	if r.unread > 0 {
		fmt.Fprintf(out, "users whose Groups could not be read: %d (the report is incomplete)\n", r.unread)
	}
	fmt.Fprintf(out, "disabled users (left out: they get no token): %d\n", r.disabled)
	fmt.Fprintln(out, "Group paths per user (paths: users):")
	pathCounts := make([]int, 0, len(r.usersByPathCount))
	for pathCount := range r.usersByPathCount {
		pathCounts = append(pathCounts, pathCount)
	}
	slices.Sort(pathCounts)
	for _, pathCount := range pathCounts {
		fmt.Fprintf(out, "  %d: %d\n", pathCount, r.usersByPathCount[pathCount])
	}
	most, largest, above := 0, 0, 0
	for _, claim := range r.claims {
		most = max(most, claim.pathCount)
		largest = max(largest, claim.bytes)
		if claim.pathCount > groupOverageThreshold {
			above++
		}
	}
	fmt.Fprintf(out, "most Group paths: %d\n", most)
	fmt.Fprintf(out, "users above %d paths (Group overage): %d\n", groupOverageThreshold, above)
	fmt.Fprintf(out, "largest groups claim: %d bytes of JSON, about %d bytes in a token (base64url)\n", largest, inToken(float64(largest)))
	if r.totalPaths > 0 {
		// A claim of as many paths as the threshold, each of the average
		// length: what the threshold lets a token carry.
		average := float64(r.totalPathBytes) / float64(r.totalPaths)
		atThreshold := groupsClaimBytes(groupOverageThreshold, groupOverageThreshold*average)
		fmt.Fprintf(out, "average Group path: %.1f bytes; %d of them: about %.0f bytes of JSON, about %d bytes in a token\n",
			average, groupOverageThreshold, atThreshold, inToken(atThreshold))
	}
}

// printOverage lists the users above the threshold by sub (their Keycloak
// id), the most paths first.
func (r *groupCountReport) printOverage(out io.Writer) {
	above := make([]groupsClaim, 0)
	for _, claim := range r.claims {
		if claim.pathCount > groupOverageThreshold {
			above = append(above, claim)
		}
	}
	slices.SortFunc(above, func(a, b groupsClaim) int {
		if a.pathCount != b.pathCount {
			return b.pathCount - a.pathCount
		}
		return strings.Compare(a.sub.String(), b.sub.String())
	})
	fmt.Fprintf(out, "users above %d paths, by sub (sub: paths):\n", groupOverageThreshold)
	for _, claim := range above {
		fmt.Fprintf(out, "  %s: %d\n", claim.sub, claim.pathCount)
	}
}
