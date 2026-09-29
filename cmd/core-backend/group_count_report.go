package main

import (
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

// runGroupCountReport reads every user Keycloak lists and each one's Groups
// through core's service account, and prints the report to out and errors
// to errOut. It fails when a user's Groups could not be read.
func runGroupCountReport(args []string, getenv func(string) string, out, errOut io.Writer) int {
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
	config, missing := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "%s needs %s\n", groupCountReportCommandName, strings.Join(missing, ", "))
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	keycloak := identity.NewKeycloak(config)

	people, err := keycloak.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "keycloak: %v\n", err)
		return 1
	}
	var counts groupCounts
	for _, person := range people {
		groups, err := keycloak.GroupsForUser(ctx, person.ID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			// Deleted since it was listed.
			continue
		case err != nil:
			// Unread, never counted as a user without Groups. The first
			// error says why (it names nobody); the rest are counted.
			counts.unread++
			if counts.unread == 1 {
				fmt.Fprintf(errOut, "keycloak: %v\n", err)
			}
			continue
		}
		paths := make([]string, 0, len(groups))
		for _, group := range groups {
			paths = append(paths, group.Path)
		}
		counts.add(person.ID, paths)
	}
	counts.print(out, time.Now())
	if *listOverage {
		counts.printOverage(out)
	}
	if counts.unread > 0 {
		return 1
	}
	return 0
}

// groupCounts gathers, per user, how many Group paths their groups claim
// carries and how many bytes they take.
type groupCounts struct {
	users   []userGroups
	byPaths map[int]int
	// unread counts the users whose Groups could not be read.
	unread int
	// paths and pathBytes add up every path read and its bytes.
	paths, pathBytes int
}

type userGroups struct {
	id         uuid.UUID
	paths      int
	claimBytes int
}

func (c *groupCounts) add(id uuid.UUID, paths []string) {
	if c.byPaths == nil {
		c.byPaths = map[int]int{}
	}
	c.users = append(c.users, userGroups{id: id, paths: len(paths), claimBytes: groupsClaimBytes(paths)})
	c.byPaths[len(paths)]++
	c.paths += len(paths)
	for _, path := range paths {
		c.pathBytes += len(path)
	}
}

// groupsClaimBytes is the size of `"groups":["…","…"]` in a token's JSON:
// each path's UTF-8 bytes in quotes, commas between them.
func groupsClaimBytes(paths []string) int {
	size := len(`"groups":[]`)
	for i, path := range paths {
		size += len(path) + 2
		if i > 0 {
			size++
		}
	}
	return size
}

// inToken is roughly what a part of the token's JSON takes in the token,
// whose payload is base64url: four characters for every three bytes.
func inToken(jsonBytes float64) int {
	return int(math.Round(jsonBytes * 4 / 3))
}

// print writes counts only: no user, no Group path.
func (c *groupCounts) print(out io.Writer, now time.Time) {
	fmt.Fprintf(out, "SKY LAB core group count report, %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "Direct Group paths per user, as a token's groups claim carries them (full paths). Read from Keycloak; this report changes nothing.")
	fmt.Fprintf(out, "users: %d\n", len(c.users)+c.unread)
	if c.unread > 0 {
		fmt.Fprintf(out, "users whose groups could not be read: %d (the report is incomplete)\n", c.unread)
	}
	fmt.Fprintln(out, "group paths per user (paths: users):")
	counts := make([]int, 0, len(c.byPaths))
	for paths := range c.byPaths {
		counts = append(counts, paths)
	}
	slices.Sort(counts)
	for _, paths := range counts {
		fmt.Fprintf(out, "  %d: %d\n", paths, c.byPaths[paths])
	}
	most, largest, above := 0, 0, 0
	for _, user := range c.users {
		most = max(most, user.paths)
		largest = max(largest, user.claimBytes)
		if user.paths > groupOverageThreshold {
			above++
		}
	}
	fmt.Fprintf(out, "most group paths: %d\n", most)
	fmt.Fprintf(out, "users above %d paths (Group overage): %d\n", groupOverageThreshold, above)
	fmt.Fprintf(out, "largest groups claim: %d bytes of JSON, about %d bytes in a token (base64url)\n", largest, inToken(float64(largest)))
	if c.paths > 0 {
		// A claim of as many paths as the threshold, each of the average
		// length: what the threshold lets a token carry.
		average := float64(c.pathBytes) / float64(c.paths)
		atThreshold := float64(len(`"groups":[]`)) + groupOverageThreshold*(average+2) + groupOverageThreshold - 1
		fmt.Fprintf(out, "average group path: %.1f bytes; %d of them: about %.0f bytes of JSON, about %d bytes in a token\n",
			average, groupOverageThreshold, atThreshold, inToken(atThreshold))
	}
}

// printOverage lists the users above the threshold by sub (their Keycloak
// id), the most paths first.
func (c *groupCounts) printOverage(out io.Writer) {
	above := make([]userGroups, 0)
	for _, user := range c.users {
		if user.paths > groupOverageThreshold {
			above = append(above, user)
		}
	}
	slices.SortFunc(above, func(a, b userGroups) int {
		if a.paths != b.paths {
			return b.paths - a.paths
		}
		return strings.Compare(a.id.String(), b.id.String())
	})
	fmt.Fprintf(out, "users above %d paths, by sub (sub: paths):\n", groupOverageThreshold)
	for _, user := range above {
		fmt.Fprintf(out, "  %s: %d\n", user.id, user.paths)
	}
}
