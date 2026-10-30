package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// The team structure report (docs/team-structure-report.md; ekip-yetki-devri
// ticket 03) reads the realm's Group tree and core's own records before team
// delegation is switched on, and lists what delegation would get wrong. It
// runs inside the core container, beside the server (the binary is
// /app/core-backend):
//
//	./core-backend team-structure-report                       # reads, writes nothing
//	./core-backend team-structure-report -codes cms=- < cms.txt
//	./core-backend team-structure-report -seed-retired         # also writes (h)
//
// It prints Group paths, Group ids, Team codes, role names and counts, never
// a person. Without -seed-retired it writes nothing; with it, it writes the
// codes of section (h) to retired_team_codes (reason seeded), once: a second
// run writes nothing new.
const teamStructureReportCommandName = "team-structure-report"

// teamCodeInventory is what the report reads and writes in core's database
// (teamcode.PostgresLedger).
type teamCodeInventory interface {
	RecordCodes(ctx context.Context) (map[string][]string, error)
	RetiredCodes(ctx context.Context) ([]string, error)
	SeedRetired(ctx context.Context, codes func(ctx context.Context) ([]string, error)) ([]string, error)
}

// teamStructureReads are the per-Group Keycloak reads of the report
// (identity.Keycloak, identity.Memory): all reads, no write.
type teamStructureReads interface {
	Members(ctx context.Context, groupID string) ([]identity.Person, error)
	GroupProtection(ctx context.Context, groupID string) (identity.Protection, error)
	GroupClientRoles(ctx context.Context, groupID string) ([]identity.ClientRole, error)
	RoleProtection(ctx context.Context, role identity.ClientRole) (identity.Protection, error)
}

// teamStructureConfig is what the report runs on.
type teamStructureConfig struct {
	// Groups reads the realm's whole tree; it fails rather than answer a
	// tree missing a branch (identity.Keycloak.ListGroupsComplete), since a
	// code is seeded for a Group it does not find.
	Groups    func(ctx context.Context) ([]identity.Group, error)
	Reads     teamStructureReads
	Inventory teamCodeInventory
	Roots     []string
	Leaders   []string
	// Depth and PathLen are the Team structure limits
	// (TEAM_SUBTEAM_DEPTH_LIMIT, TEAM_PATH_LENGTH_LIMIT).
	Depth, PathLen int
	Now            time.Time
}

func runTeamStructureReport(args []string, getenv func(string) string, stdin io.Reader, out, errOut io.Writer) int {
	options, code := parseTeamStructureOptions(args, stdin, errOut)
	if code != 0 {
		return code
	}
	roots, err := authz.PrivilegedRootsFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamStructureReportCommandName, err)
		return 2
	}
	limits, err := identity.TeamDelegationOptionsFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamStructureReportCommandName, err)
		return 2
	}
	keycloak, missing, err := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "%s needs %s\n", teamStructureReportCommandName, strings.Join(missing, ", "))
		return 2
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", teamStructureReportCommandName, err)
		return 2
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(errOut, "%s needs DATABASE_URL: core's Owner team records and retired codes are there\n", teamStructureReportCommandName)
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(errOut, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	dir := identity.NewKeycloak(keycloak)
	cfg := teamStructureConfig{
		Groups:    dir.ListGroupsComplete,
		Reads:     dir,
		Inventory: teamcode.NewPostgresLedger(pool),
		Roots:     roots,
		Leaders:   authz.DefaultPolicy().LeaderSubgroups,
		Depth:     limits.SubTeamDepth,
		PathLen:   limits.TeamPathLen,
		Now:       time.Now(),
	}
	return teamStructureReportCommand(ctx, options, cfg, out, errOut)
}

// teamStructureOptions are the command's flags.
type teamStructureOptions struct {
	seed bool
	// external are the code lists another system's codes came in, each
	// under its source's label (-codes cms=path).
	external []externalCodes
}

// externalCodes are the Team codes one other system names (the CMS's Teams,
// Forms' templates, a site's TEAM_META), read from a list: core reads none
// of those systems itself.
type externalCodes struct {
	source string
	codes  []string
}

var codeSourceLabel = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// maxRetiredCode is retired_team_codes.code's longest value.
const maxRetiredCode = 255

func parseTeamStructureOptions(args []string, stdin io.Reader, errOut io.Writer) (teamStructureOptions, int) {
	var options teamStructureOptions
	flags := flag.NewFlagSet(teamStructureReportCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.BoolVar(&options.seed, "seed-retired", false, "write the codes of section (h) to retired_team_codes; without it nothing is written")
	stdinUsed := false
	flags.Func("codes", "source=path: the Team codes another system names, one per line (# comments); path - is the standard input. Repeatable, one source each", func(value string) error {
		source, path, ok := strings.Cut(value, "=")
		switch {
		case !ok || path == "":
			return errors.New("want source=path")
		case !codeSourceLabel.MatchString(source):
			return fmt.Errorf("source %q: a lower-case label (cms, forms, skylab-site, arge)", source)
		case slices.ContainsFunc(options.external, func(e externalCodes) bool { return e.source == source }):
			return fmt.Errorf("source %q is given twice", source)
		}
		var in io.Reader
		if path == "-" {
			if stdinUsed {
				return errors.New("only one source may be read from the standard input")
			}
			stdinUsed = true
			in = stdin
		} else {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			in = file
		}
		codes, err := readCodes(in)
		if err != nil {
			return fmt.Errorf("source %q: %w", source, err)
		}
		options.external = append(options.external, externalCodes{source: source, codes: codes})
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return options, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s takes no arguments, only flags\n", teamStructureReportCommandName)
		return options, 2
	}
	return options, 0
}

// readCodes reads one code per line: blanks around it trimmed, empty lines
// and lines starting with # skipped.
func readCodes(in io.Reader) ([]string, error) {
	var codes []string
	scanner := bufio.NewScanner(in)
	line := 0
	for scanner.Scan() {
		line++
		code := strings.TrimSpace(scanner.Text())
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		if len(code) > maxRetiredCode {
			return nil, fmt.Errorf("line %d is longer than %d bytes", line, maxRetiredCode)
		}
		codes = append(codes, code)
	}
	return codes, scanner.Err()
}

// teamStructureReportCommand reads the tree and core's records, prints the
// report and, with -seed-retired, seeds the retired codes. It answers 1
// when a read or the write failed, 0 otherwise, whatever the report found.
func teamStructureReportCommand(ctx context.Context, options teamStructureOptions, cfg teamStructureConfig, out, errOut io.Writer) int {
	if cfg.Depth <= 0 {
		cfg.Depth = identity.DefaultSubTeamDepth
	}
	if cfg.PathLen <= 0 {
		cfg.PathLen = identity.DefaultTeamPathLen
	}
	groups, err := cfg.Groups(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "keycloak: %v\n", err)
		return 1
	}
	records, err := cfg.Inventory.RecordCodes(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "database: %v\n", err)
		return 1
	}
	retired, err := cfg.Inventory.RetiredCodes(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "database: %v\n", err)
		return 1
	}
	tree := newTeamStructure(groups, cfg.Roots, cfg.Leaders)
	report := &teamStructureReport{out: out}
	report.header(cfg, tree)
	failed := false
	for _, section := range []func() error{
		func() error { return report.privilegedNames(ctx, tree, cfg.Reads) },
		func() error { report.leaderGroups(tree); return nil },
		func() error { report.duplicates(tree); return nil },
		func() error { report.invalidNames(tree); return nil },
		func() error { return report.protectedTeams(ctx, tree, cfg.Reads) },
		func() error { report.limits(tree, cfg.Depth, cfg.PathLen); return nil },
		func() error { report.leaderless(tree); return nil },
		func() error { report.slashNames(tree); return nil },
		func() error { return report.roleMappings(ctx, tree, cfg.Reads) },
	} {
		if err := section(); err != nil {
			fmt.Fprintf(errOut, "keycloak: %v\n", err)
			failed = true
		}
	}
	codes := unmatchedCodes(groups, records, options.external, retired)
	report.codes(codes, options.external)
	report.gate()
	if failed {
		fmt.Fprintln(errOut, "a section could not be read; the report is incomplete")
	}
	if !options.seed {
		fmt.Fprintln(out, "seed: dry run, nothing written (-seed-retired writes the codes to retire above to retired_team_codes)")
		if failed {
			return 1
		}
		return 0
	}
	if len(options.external) == 0 {
		fmt.Fprintln(errOut, "no -codes list was given: only the codes core's own records name are seeded; the CMS's, Forms' and the sites' are not")
	}
	written, err := cfg.Inventory.SeedRetired(ctx, func(ctx context.Context) ([]string, error) {
		// Read again inside the lock: a Group made since the report
		// keeps its code.
		groups, err := cfg.Groups(ctx)
		if err != nil {
			return nil, err
		}
		var due []string
		for _, code := range unmatchedCodes(groups, records, options.external, nil) {
			due = append(due, code.code)
		}
		return due, nil
	})
	if err != nil {
		fmt.Fprintf(errOut, "seed: nothing written: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "seed: wrote %d codes to retired_team_codes (reason %s)\n", len(written), teamcode.ReasonSeeded)
	for _, code := range written {
		fmt.Fprintf(out, "  %s\n", code)
	}
	if failed {
		return 1
	}
	return 0
}

// teamStructure is the realm's tree as the report reads it: every Group by
// path, and what core's rules make of each.
type teamStructure struct {
	groups  []identity.Group
	roots   []string
	leaders []string
	// paths count the Groups at each path; children are by parent path.
	paths    map[string]int
	children map[string][]identity.Group
}

func newTeamStructure(groups []identity.Group, roots, leaders []string) teamStructure {
	sorted := slices.Clone(groups)
	slices.SortFunc(sorted, func(a, b identity.Group) int { return strings.Compare(a.Path, b.Path) })
	tree := teamStructure{groups: sorted, roots: roots, leaders: leaders, paths: map[string]int{}, children: map[string][]identity.Group{}}
	for _, g := range sorted {
		tree.paths[g.Path]++
		tree.children[parentPath(g.Path)] = append(tree.children[parentPath(g.Path)], g)
	}
	return tree
}

func parentPath(path string) string {
	return path[:max(strings.LastIndex(path, "/"), 0)]
}

// kind is what core's rules make of the Group at path: the members' tree,
// an area, a top Team, a Sub-team, a Leader group (of a top Team or of a
// Sub-team), or "" for anything else (a Privileged root, a Group outside
// /UYELER).
func (t teamStructure) kind(path string) string {
	if path == authz.MembersRoot {
		return "members"
	}
	if authz.PrivilegedRoot(t.roots, path) != "" {
		return ""
	}
	if strings.HasPrefix(path, authz.MembersRoot+"/") && strings.Count(path, "/") == 2 {
		return "area"
	}
	team, role, ok := authz.TeamOf(path, t.roots, t.leaders)
	switch {
	case !ok:
		return ""
	case role != "" && strings.Count(team, "/") == teamTopDepth:
		return "top team leader group"
	case role != "":
		return "sub-team leader group"
	case strings.Count(path, "/") == teamTopDepth:
		return "top team"
	}
	return "sub-team"
}

// teamTopDepth is how many Groups deep a top Team is: /UYELER/<area>/<team>.
const teamTopDepth = 3

func (t teamStructure) isTeam(path string) bool {
	k := t.kind(path)
	return k == "top team" || k == "sub-team"
}

func (t teamStructure) isRoot(path string) bool {
	return slices.Contains(t.roots, path)
}

func (t teamStructure) isLeaderName(name string) bool {
	return slices.Contains(t.leaders, name)
}

// teamStructureReport prints the report.
type teamStructureReport struct {
	out io.Writer
	// duplicateCodes and slashNames feed the delegation gate line.
	duplicateCodes, slashCount int
}

func (r *teamStructureReport) printf(format string, args ...any) {
	fmt.Fprintf(r.out, format, args...)
}

func (r *teamStructureReport) section(title string, count int) {
	r.printf("\n%s: %d\n", title, count)
}

func (r *teamStructureReport) header(cfg teamStructureConfig, tree teamStructure) {
	r.printf("SKY LAB core team structure report, %s\n", cfg.Now.UTC().Format(time.RFC3339))
	r.printf("Read from Keycloak and core's database. Group paths, Group ids, Team codes, role names and counts only, never a person.\n")
	teams, top := 0, 0
	for _, g := range tree.groups {
		switch tree.kind(g.Path) {
		case "top team":
			teams++
			top++
		case "sub-team":
			teams++
		}
	}
	r.printf("groups: %d, Teams: %d (top Teams %d)\n", len(tree.groups), teams, top)
	var missing []string
	for _, root := range cfg.Roots {
		if tree.paths[root] == 0 {
			missing = append(missing, root)
		}
	}
	r.printf("Privileged roots: %s", strings.Join(cfg.Roots, ", "))
	if len(missing) > 0 {
		r.printf(" (missing in Keycloak: %s)", strings.Join(missing, ", "))
	}
	r.printf("\nlimits: %d levels of Sub-teams below a top Team, paths of at most %d characters\n", cfg.Depth, cfg.PathLen)
}

// privilegedNames is section (a): Groups named like a Privileged root
// outside the roots, with their direct member counts.
func (r *teamStructureReport) privilegedNames(ctx context.Context, tree teamStructure, reads teamStructureReads) error {
	labels := map[string]bool{}
	for _, root := range tree.roots {
		labels[strings.ToUpper(authz.RootLabel(root))] = true
	}
	var found []identity.Group
	for _, g := range tree.groups {
		if labels[strings.ToUpper(strings.TrimSpace(g.Name))] && !tree.isRoot(g.Path) {
			found = append(found, g)
		}
	}
	r.section("(a) Groups named like a Privileged root (ADMIN, YK, DK) outside the roots; their members are not Privileged", len(found))
	var failed error
	for _, g := range found {
		members, err := reads.Members(ctx, g.ID)
		if err != nil {
			failed = errors.Join(failed, err)
			r.printf("  %s  id=%s  members=unread\n", g.Path, g.ID)
			continue
		}
		r.printf("  %s  id=%s  members=%d\n", g.Path, g.ID, len(members))
	}
	return failed
}

// leaderGroups is section (b) and (b'): leader subgroups that are not right
// under a Team or have a Group under them, and names that only look like one.
func (r *teamStructureReport) leaderGroups(tree teamStructure) {
	type row struct {
		group identity.Group
		why   string
	}
	var misplaced, lookalike []row
	for _, g := range tree.groups {
		folded := strings.ToUpper(strings.TrimSpace(g.Name))
		switch {
		case tree.isLeaderName(g.Name):
			var why []string
			if _, role, ok := authz.TeamOf(g.Path, tree.roots, tree.leaders); !ok || role == "" {
				why = append(why, "not right under a Team")
			}
			if n := len(tree.children[g.Path]); n > 0 {
				why = append(why, fmt.Sprintf("Groups under it: %d", n))
			}
			if len(why) > 0 {
				misplaced = append(misplaced, row{g, strings.Join(why, ", ")})
			}
		case slices.ContainsFunc(tree.leaders, func(leader string) bool { return strings.HasPrefix(folded, strings.ToUpper(leader)) }):
			lookalike = append(lookalike, row{g, fmt.Sprintf("name %q", g.Name)})
		}
	}
	r.section("(b) Leader groups (LIDERLER, KOORDINATORLER) in the wrong place", len(misplaced))
	for _, x := range misplaced {
		r.printf("  %s  id=%s  %s\n", x.group.Path, x.group.ID, x.why)
	}
	r.section("(b') names that look like a Leader group's but are not one (only Privileged manages them and what is under them)", len(lookalike))
	for _, x := range lookalike {
		r.printf("  %s  id=%s  %s\n", x.group.Path, x.group.ID, x.why)
	}
}

// duplicates is section (c): codes more than one Group holds, compared
// without case and blanks; Leader groups and the Privileged roots aside,
// which share their names by design.
func (r *teamStructureReport) duplicates(tree teamStructure) {
	byCode := map[string][]identity.Group{}
	var order []string
	for _, g := range tree.groups {
		if tree.isLeaderName(g.Name) || tree.isRoot(g.Path) {
			continue
		}
		code := strings.ToLower(strings.TrimSpace(g.Name))
		if byCode[code] == nil {
			order = append(order, code)
		}
		byCode[code] = append(byCode[code], g)
	}
	var dup []string
	for _, code := range order {
		if len(byCode[code]) > 1 {
			dup = append(dup, code)
		}
	}
	slices.Sort(dup)
	r.duplicateCodes = len(dup)
	r.section("(c) codes more than one Group holds (case and blanks aside)", len(dup))
	for _, code := range dup {
		r.printf("  %s: %d Groups\n", strings.ToUpper(code), len(byCode[code]))
		for _, g := range byCode[code] {
			r.printf("    %s  id=%s\n", g.Path, g.ID)
		}
	}
}

// invalidNames is section (d): names outside the code form. They stay as
// they are (no rename is forced); a rename or a new Group follows the form.
func (r *teamStructureReport) invalidNames(tree teamStructure) {
	var found []identity.Group
	for _, g := range tree.groups {
		if !teamcode.Valid(g.Name) {
			found = append(found, g)
		}
	}
	r.section("(d) names outside the code form ^[A-Z][A-Z0-9-]{1,23}$ (they stay; a rename follows the form)", len(found))
	for _, g := range found {
		r.printf("  %s  id=%s\n", g.Path, g.ID)
	}
}

// protectedTeams is section (e) and (e'): the members' tree, every area and
// every Team whose own role mappings make it Protected, with how many Teams
// that leaves to Privileged alone.
func (r *teamStructureReport) protectedTeams(ctx context.Context, tree teamStructure, reads teamStructureReads) error {
	type row struct {
		group  identity.Group
		reason string
		teams  int
	}
	var found []row
	var failed error
	unread := 0
	for _, g := range tree.groups {
		switch tree.kind(g.Path) {
		case "members", "area", "top team", "sub-team":
		default:
			continue
		}
		protection, err := reads.GroupProtection(ctx, g.ID)
		if err != nil {
			failed = errors.Join(failed, err)
			unread++
			continue
		}
		if !protection.Protected {
			continue
		}
		teams := 0
		for _, below := range tree.groups {
			if (below.Path == g.Path || strings.HasPrefix(below.Path, g.Path+"/")) && tree.isTeam(below.Path) {
				teams++
			}
		}
		found = append(found, row{g, protection.Reason, teams})
	}
	r.section("(e) the members' tree, areas and Teams whose own role mappings make them Protected (no delegation there or below)", len(found))
	for _, x := range found {
		r.printf("  %s  id=%s  %s  Teams at or below: %d\n", x.group.Path, x.group.ID, x.reason, x.teams)
	}
	if unread > 0 {
		r.printf("  unread: %d Groups\n", unread)
	}
	return failed
}

// limits is section (f): Teams beyond the structure limits.
func (r *teamStructureReport) limits(tree teamStructure, depth, pathLen int) {
	type row struct {
		group identity.Group
		why   string
	}
	var found []row
	for _, g := range tree.groups {
		if !tree.isTeam(g.Path) {
			continue
		}
		var why []string
		if levels := strings.Count(g.Path, "/") - teamTopDepth; levels > depth {
			why = append(why, fmt.Sprintf("%d levels below its top Team", levels))
		}
		if len(g.Path) > pathLen {
			why = append(why, fmt.Sprintf("path of %d characters", len(g.Path)))
		}
		if len(why) > 0 {
			found = append(found, row{g, strings.Join(why, ", ")})
		}
	}
	r.section("(f) Teams beyond the structure limits", len(found))
	for _, x := range found {
		r.printf("  %s  id=%s  %s\n", x.group.Path, x.group.ID, x.why)
	}
}

// leaderless is section (g): top Teams without a Leader group. Delegation
// gives nobody their management; a Sub-team without one is managed by the
// Leaders above it.
func (r *teamStructureReport) leaderless(tree teamStructure) {
	var found []identity.Group
	for _, g := range tree.groups {
		if tree.kind(g.Path) != "top team" {
			continue
		}
		if !slices.ContainsFunc(tree.children[g.Path], func(child identity.Group) bool { return tree.isLeaderName(child.Name) }) {
			found = append(found, g)
		}
	}
	r.section("(g) top Teams without a Leader group (nobody manages them through delegation)", len(found))
	for _, g := range found {
		r.printf("  %s  id=%s\n", g.Path, g.ID)
	}
}

// slashNames is section (i): names with a "/". The token carries the path
// unescaped, so such a Group reads as another path (/UYELER with a Group
// YK/BASKAN reads as /UYELER/YK/BASKAN).
func (r *teamStructureReport) slashNames(tree teamStructure) {
	var found []identity.Group
	for _, g := range tree.groups {
		if strings.Contains(g.Name, "/") {
			found = append(found, g)
		}
	}
	r.slashCount = len(found)
	r.section("(i) names with a \"/\" (a token path that reads as another path)", len(found))
	for _, g := range found {
		// Keycloak answers the path with the name's "/" escaped as "~/"
		// or not at all; the token carries it unescaped.
		read := strings.ReplaceAll(g.Path, "~/", "/")
		parent := read
		for _, own := range []string{"/" + g.Name, "/" + strings.ReplaceAll(g.Name, "/", "~/")} {
			if strings.HasSuffix(g.Path, own) {
				parent = strings.ReplaceAll(strings.TrimSuffix(g.Path, own), "~/", "/")
				break
			}
		}
		var marks []string
		if root := authz.PrivilegedRoot(tree.roots, read); root != "" && authz.PrivilegedRoot(tree.roots, parent) == "" {
			marks = append(marks, "reads as under the Privileged root "+root)
		}
		if tree.paths[read] > 1 || (read != g.Path && tree.paths[read] > 0) {
			marks = append(marks, "reads as another Group's path")
		}
		r.printf("  %s  id=%s", g.Path, g.ID)
		if len(marks) > 0 {
			r.printf("  %s", strings.Join(marks, ", "))
		}
		r.printf("\n")
	}
}

// roleMappings is section (j): the client role mappings of the members'
// tree, every area, every Team and every Sub-team's Leader group (a top
// Team's Leader group aside), with each role's class. An app role that
// holds a protecting role as a composite says so.
func (r *teamStructureReport) roleMappings(ctx context.Context, tree teamStructure, reads teamStructureReads) error {
	type row struct {
		group identity.Group
		kind  string
		role  identity.ClientRole
		class authz.RoleClass
		via   string
	}
	var rows []row
	var failed error
	unread := 0
	for _, g := range tree.groups {
		kind := tree.kind(g.Path)
		switch kind {
		case "members", "area", "top team", "sub-team", "sub-team leader group":
		default:
			continue
		}
		roles, err := reads.GroupClientRoles(ctx, g.ID)
		if err != nil {
			failed = errors.Join(failed, err)
			unread++
			continue
		}
		slices.SortFunc(roles, func(a, b identity.ClientRole) int {
			return strings.Compare(a.ClientID+":"+a.Role, b.ClientID+":"+b.Role)
		})
		for _, role := range roles {
			if role.ClientID == "" {
				// A realm role: (e) shows it.
				continue
			}
			x := row{group: g, kind: kind, role: role, class: authz.ClassifyRole(role.ClientID, role.Role)}
			if x.class == authz.RoleClassApp {
				protection, err := reads.RoleProtection(ctx, role)
				switch {
				case err != nil:
					failed = errors.Join(failed, err)
					x.via = "composite unread"
				case protection.Protected:
					x.via = protection.Reason
				}
			}
			rows = append(rows, x)
		}
	}
	flagged := 0
	for _, x := range rows {
		if x.class != authz.RoleClassApp || x.via != "" {
			flagged++
		}
	}
	r.section(fmt.Sprintf("(j) client role mappings of the members' tree, areas, Teams and Sub-teams' Leader groups (! = admin, unclassified or a composite holding one: %d)", flagged), len(rows))
	for _, x := range rows {
		mark := " "
		if x.class != authz.RoleClassApp || x.via != "" {
			mark = "!"
		}
		r.printf(" %s %s  id=%s  %s  %s:%s  %s  direct", mark, x.group.Path, x.group.ID, x.kind, x.role.ClientID, x.role.Role, x.class)
		if x.via != "" {
			r.printf(", %s", x.via)
		}
		r.printf("\n")
	}
	r.printf("  realm roles are not listed here; any one makes its Group Protected and shows in (e)\n")
	if unread > 0 {
		r.printf("  unread: %d Groups\n", unread)
	}
	return failed
}

// unmatchedCode is a code core's records or another system name that no
// Group holds.
type unmatchedCode struct {
	code    string
	sources []string
	retired bool
}

// unmatchedCodes are the codes of records and external that no Group holds
// (compared without case and blanks), each once (its first spelling) with
// every source that names it, sorted; retired marks those retired already.
func unmatchedCodes(groups []identity.Group, records map[string][]string, external []externalCodes, retired []string) []unmatchedCode {
	held := map[string]bool{}
	for _, g := range groups {
		held[strings.ToLower(strings.TrimSpace(g.Name))] = true
	}
	gone := map[string]bool{}
	for _, code := range retired {
		gone[strings.ToLower(code)] = true
	}
	byKey := map[string]*unmatchedCode{}
	var keys []string
	add := func(code, source string) {
		code = strings.TrimSpace(code)
		key := strings.ToLower(code)
		if code == "" || held[key] {
			return
		}
		c, ok := byKey[key]
		if !ok {
			c = &unmatchedCode{code: code, retired: gone[key]}
			byKey[key] = c
			keys = append(keys, key)
		}
		if !slices.Contains(c.sources, source) {
			c.sources = append(c.sources, source)
		}
	}
	recordCodes := make([]string, 0, len(records))
	for code := range records {
		recordCodes = append(recordCodes, code)
	}
	slices.Sort(recordCodes)
	for _, code := range recordCodes {
		for _, kind := range records[code] {
			add(code, "core:"+kind)
		}
	}
	for _, list := range external {
		for _, code := range list.codes {
			add(code, list.source)
		}
	}
	slices.Sort(keys)
	out := make([]unmatchedCode, 0, len(keys))
	for _, key := range keys {
		out = append(out, *byKey[key])
	}
	return out
}

// codes is section (h): the sources read and the codes no Group holds.
func (r *teamStructureReport) codes(codes []unmatchedCode, external []externalCodes) {
	due := 0
	for _, c := range codes {
		if !c.retired {
			due++
		}
	}
	r.section(fmt.Sprintf("(h) Team codes named without a Group (to retire: %d, retired already: %d)", due, len(codes)-due), len(codes))
	r.printf("  sources: core (events, certificates, certificate templates, Owner team bindings): read\n")
	for _, list := range external {
		r.printf("  sources: %s: %d codes from a list (-codes)\n", list.source, len(list.codes))
	}
	if len(external) == 0 {
		r.printf("  sources: the CMS's Teams, Forms' templates and the sites' TEAM_META: not given (-codes source=path)\n")
	}
	for _, c := range codes {
		state := "to retire"
		if c.retired {
			state = "retired already"
		}
		r.printf("  %s  %s  named by %s\n", c.code, state, strings.Join(c.sources, ", "))
	}
}

// gate is the line team delegation's switch waits on (ticket 03, review of
// #227 Y3): (c) and (i) empty or decided.
func (r *teamStructureReport) gate() {
	r.printf("\ndelegation gate (TEAM_DELEGATION=on): (c) %d, (i) %d; both 0, or every row decided\n", r.duplicateCodes, r.slashCount)
}
