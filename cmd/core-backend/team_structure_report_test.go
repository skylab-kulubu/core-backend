package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// fakeTeamCodes is core's database for the report: the codes its records
// name, the retired codes, and a seed that keeps what it writes.
type fakeTeamCodes struct {
	records map[string][]string
	retired []string
	seeds   int
	fail    error
}

func (f *fakeTeamCodes) RecordCodes(context.Context) (map[string][]string, error) {
	return f.records, f.fail
}

func (f *fakeTeamCodes) RetiredCodes(context.Context) ([]string, error) {
	return slices.Clone(f.retired), f.fail
}

func (f *fakeTeamCodes) SeedRetired(ctx context.Context, codes func(context.Context) ([]string, error)) ([]string, error) {
	f.seeds++
	want, err := codes(ctx)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, code := range want {
		if slices.ContainsFunc(f.retired, func(r string) bool { return strings.EqualFold(r, code) }) {
			continue
		}
		f.retired = append(f.retired, code)
		written = append(written, code)
	}
	return written, nil
}

// teamStructureRealm is a realm with one example of every section of the
// report, and two people whose names and addresses must never be printed.
func teamStructureRealm(t *testing.T) *identity.Memory {
	t.Helper()
	dir := identity.NewMemory()
	put := func(path string) string {
		id := uuid.NewString()
		name := path[strings.LastIndex(path, "/")+1:]
		dir.PutGroup(identity.Group{ID: id, Name: name, Path: path})
		return id
	}
	for _, path := range []string{
		"/ADMIN", "/YK", "/DK", "/UYELER", "/UYELER/ADMIN", "/UYELER/YK", "/UYELER/DK",
		"/UYELER/ARGE", "/UYELER/SOSYAL",
		"/UYELER/ARGE/WEBLAB/LIDERLER",
		"/UYELER/ARGE/WEBLAB/LIDERLER-ESKI",
		"/UYELER/ARGE/WEBLAB/AGC",
		"/UYELER/ARGE/WEBLAB/AGC/KOORDINATORLER",
		"/UYELER/ARGE/WEBLAB/AGC/DEEP",
		"/UYELER/ARGE/WEBLAB/AGC/DEEP/DEEPER",
		"/UYELER/ARGE/LIDERLER",
		"/UYELER/SOSYAL/agc",
		"/UYELER/SOSYAL/agc/LIDERLER",
		"/UYELER/SOSYAL/agc/LIDERLER/ICERDE",
		"/UYELER/SOSYAL/GEZI",
		"/UYELER/SOSYAL/GEZI/LIDERLER",
		"/ROLLER",
	} {
		put(path)
	}
	weblab := put("/UYELER/ARGE/WEBLAB")
	ailab := put("/UYELER/ARGE/AILAB")
	// A Sub-team's Leader group is in its place, however deep.
	put("/UYELER/ARGE/WEBLAB/AGC/DEEP/DEEPER/LIDERLER")
	sosyal, _ := dir.GetGroup(context.Background(), "/UYELER/SOSYAL")
	gezi, _ := dir.GetGroup(context.Background(), "/UYELER/SOSYAL/GEZI")
	agcLeaders, _ := dir.GetGroup(context.Background(), "/UYELER/ARGE/WEBLAB/AGC/KOORDINATORLER")
	// A name with a "/": the token reads it as under /UYELER/YK.
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "YK/BASKAN", Path: "/UYELER/YK/BASKAN"})
	// (a): a YK outside the roots, with two people in it.
	fakeYK := put("/UYELER/ARGE/WEBLAB/YK")
	for _, p := range []identity.Person{
		{ID: uuid.New(), FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", Username: "ada"},
		{ID: uuid.New(), FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com", Username: "grace"},
	} {
		dir.PutUser(p)
		if err := dir.AddMember(context.Background(), fakeYK, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	// (e): an admin role on a top Team, a realm role on an area.
	mustRole(t, dir.AddGroupClientRole(ctx, ailab, identity.ClientRole{ClientID: "frontend-main", Role: authz.RoleCMSAccess}))
	mustRole(t, dir.AddGroupClientRole(ctx, sosyal.ID, identity.ClientRole{Role: "offline_access"}))
	// (j): an app role, and an app role holding an admin role.
	mustRole(t, dir.AddGroupClientRole(ctx, weblab, identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}))
	dir.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}, identity.ClientRole{ClientID: "skymail", Role: "skymail:mails:send"})
	mustRole(t, dir.AddGroupClientRole(ctx, gezi.ID, identity.ClientRole{ClientID: "skymail", Role: "skymail:access"}))
	mustRole(t, dir.AddGroupClientRole(ctx, agcLeaders.ID, identity.ClientRole{ClientID: "core", Role: "users:read"}))
	// The report starts from here: whatever it calls is recorded anew.
	dir.Ops = nil
	return dir
}

func mustRole(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func teamStructureTestConfig(dir *identity.Memory, codes *fakeTeamCodes) teamStructureConfig {
	return teamStructureConfig{
		Groups:    dir.ListGroups,
		Reads:     dir,
		Inventory: codes,
		Roots:     authz.DefaultPrivilegedRoots(),
		Leaders:   authz.DefaultPolicy().LeaderSubgroups,
		Now:       time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	}
}

// sectionOf is the lines of the report's section that starts with title, up
// to the next empty line.
func sectionOf(report, title string) string {
	_, rest, ok := strings.Cut(report, "\n"+title)
	if !ok {
		return ""
	}
	section, _, _ := strings.Cut(rest, "\n\n")
	return title + section
}

func TestTeamStructureReport(t *testing.T) {
	dir := teamStructureRealm(t)
	codes := &fakeTeamCodes{
		records: map[string][]string{
			"GONE":   {teamcode.RecordEvent, teamcode.RecordCertificate},
			"WEBLAB": {teamcode.RecordEvent},
		},
		retired: []string{"oldcode"},
	}
	var out, errOut bytes.Buffer
	options := teamStructureOptions{external: []externalCodes{{source: "cms", codes: []string{"SOSYALTEAM", "weblab", "OLDCODE"}}}}
	if code := teamStructureReportCommand(context.Background(), options, teamStructureTestConfig(dir, codes), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	report := out.String()

	for _, tc := range []struct {
		section string
		want    []string
		not     []string
	}{
		{"(a)", []string{"(a) Groups named like a Privileged root", ": 1\n", "/UYELER/ARGE/WEBLAB/YK  id=", "members=2"}, []string{"/UYELER/YK  id"}},
		{"(b)", []string{": 2\n", "/UYELER/ARGE/LIDERLER  id=", "not right under a Team", "/UYELER/SOSYAL/agc/LIDERLER  id=", "Groups under it: 1"}, []string{"WEBLAB/LIDERLER  id", "DEEPER/LIDERLER"}},
		{"(b')", []string{": 1\n", "/UYELER/ARGE/WEBLAB/LIDERLER-ESKI  id=", `name "LIDERLER-ESKI"`}, nil},
		{"(c)", []string{": 1\n", "AGC: 2 Groups", "/UYELER/ARGE/WEBLAB/AGC  id=", "/UYELER/SOSYAL/agc  id="}, []string{"LIDERLER:", "ADMIN:"}},
		{"(d)", []string{"/UYELER/SOSYAL/agc  id=", "/UYELER/YK/BASKAN  id="}, []string{"WEBLAB  id"}},
		{"(e)", []string{": 3\n", "/UYELER/SOSYAL/GEZI  id=", "composite skymail:skymail:access holds admin role skymail:skymail:mails:send  Teams at or below: 1", "/UYELER/ARGE/AILAB  id=", "admin role frontend-main:cms:access  Teams at or below: 1", "/UYELER/SOSYAL  id=", "realm role offline_access  Teams at or below: 2"}, nil},
		{"(f)", []string{": 1\n", "/UYELER/ARGE/WEBLAB/AGC/DEEP/DEEPER  id=", "3 levels below its top Team"}, nil},
		{"(g)", []string{": 1\n", "/UYELER/ARGE/AILAB  id="}, []string{"WEBLAB"}},
		{"(i)", []string{": 1\n", "/UYELER/YK/BASKAN  id=", "reads as under the Privileged root /UYELER/YK"}, nil},
		{"(j)", []string{
			"  /UYELER/ARGE/WEBLAB  id=", "top team  forms:skyforms:access  app  direct",
			"! /UYELER/ARGE/AILAB  id=", "frontend-main:cms:access  admin  direct",
			"! /UYELER/SOSYAL/GEZI  id=", "skymail:skymail:access  app  direct, composite skymail:skymail:access holds admin role skymail:skymail:mails:send",
			"! /UYELER/ARGE/WEBLAB/AGC/KOORDINATORLER  id=", "sub-team leader group  core:users:read  admin",
		}, []string{"offline_access"}},
		{"(h)", []string{
			"to retire: 2, retired already: 1",
			"core (events, certificates, certificate templates, Owner team bindings): read",
			"cms: 3 codes from a list",
			`"GONE"  to retire  named by core:event, core:certificate`,
			`"OLDCODE"  retired already  named by cms`,
			`"SOSYALTEAM"  to retire  named by cms`,
		}, []string{"WEBLAB  ", "weblab  "}},
	} {
		section := sectionOf(report, tc.section)
		if section == "" {
			t.Errorf("no section %s", tc.section)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(section, want) {
				t.Errorf("section %s lacks %q:\n%s", tc.section, want, section)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(section, not) {
				t.Errorf("section %s holds %q:\n%s", tc.section, not, section)
			}
		}
	}
	for _, personal := range []string{"Ada", "Lovelace", "ada@", "Grace", "Hopper", "grace", "example.com"} {
		if strings.Contains(report, personal) || strings.Contains(errOut.String(), personal) {
			t.Errorf("the report names a person: %q", personal)
		}
	}
	for _, want := range []string{"delegation gate (TEAM_DELEGATION=on): (c) 1, (i) 1", "seed: dry run, nothing written"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if codes.seeds != 0 || !slices.Equal(codes.retired, []string{"oldcode"}) {
		t.Fatalf("a dry run wrote: %v", codes.retired)
	}
	if slices.ContainsFunc(dir.Ops, func(op string) bool {
		return !slices.Contains([]string{"ListGroups", "Members", "GroupProtection", "GroupClientRoles", "RoleProtection"}, op)
	}) {
		t.Fatalf("the report wrote to Keycloak: %v", dir.Ops)
	}
}

// -seed-retired writes the codes to retire, reading the tree again inside
// the lock: a Group made since keeps its code. A second run writes nothing.
func TestTeamStructureReportSeedRetired(t *testing.T) {
	dir := teamStructureRealm(t)
	codes := &fakeTeamCodes{records: map[string][]string{"GONE": {teamcode.RecordEvent}, "LATE": {teamcode.RecordEvent}}, retired: []string{"oldcode"}}
	cfg := teamStructureTestConfig(dir, codes)
	reads := 0
	cfg.Groups = func(ctx context.Context) ([]identity.Group, error) {
		reads++
		if reads == 2 {
			dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "LATE", Path: "/UYELER/SOSYAL/LATE"})
		}
		return dir.ListGroups(ctx)
	}
	options := teamStructureOptions{seed: true, external: []externalCodes{{source: "forms", codes: []string{"FORMTEAM", "OLDCODE"}}}}
	var out, errOut bytes.Buffer
	if code := teamStructureReportCommand(context.Background(), options, cfg, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !slices.Equal(codes.retired, []string{"oldcode", "FORMTEAM", "GONE"}) {
		t.Fatalf("retired = %v", codes.retired)
	}
	if !strings.Contains(out.String(), "seed: wrote 2 codes to retired_team_codes (reason seeded)\n  \"FORMTEAM\"\n  \"GONE\"\n") {
		t.Fatalf("report:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := teamStructureReportCommand(context.Background(), options, cfg, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "seed: wrote 0 codes") || len(codes.retired) != 3 {
		t.Fatalf("second run: %v\n%s", codes.retired, out.String())
	}
}

// Without a -codes list the seed says that only core's own codes went in.
// A failed read writes nothing and answers 1.
func TestTeamStructureReportFailures(t *testing.T) {
	dir := teamStructureRealm(t)
	codes := &fakeTeamCodes{records: map[string][]string{"GONE": {teamcode.RecordEvent}}}
	var out, errOut bytes.Buffer
	if code := teamStructureReportCommand(context.Background(), teamStructureOptions{seed: true}, teamStructureTestConfig(dir, codes), &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "no -codes list was given") {
		t.Fatalf("stderr %q", errOut.String())
	}

	cfg := teamStructureTestConfig(dir, &fakeTeamCodes{})
	cfg.Groups = func(context.Context) ([]identity.Group, error) { return nil, identity.ErrIncompleteGroups }
	out.Reset()
	if code := teamStructureReportCommand(context.Background(), teamStructureOptions{seed: true}, cfg, &out, &errOut); code != 1 {
		t.Fatalf("exit %d on an incomplete tree", code)
	}
	failing := &fakeTeamCodes{fail: errors.New("database down")}
	if code := teamStructureReportCommand(context.Background(), teamStructureOptions{seed: true}, teamStructureTestConfig(dir, failing), &out, &errOut); code != 1 || failing.seeds != 0 {
		t.Fatalf("exit %d, seeds %d on a database failure", code, failing.seeds)
	}
}

func TestTeamStructureReportOptions(t *testing.T) {
	stdin := strings.NewReader("\uFEFFWEBLAB\r\n# CMS teams\n\n  agc  # site\nWeb Lab\n")
	var errOut bytes.Buffer
	options, code := parseTeamStructureOptions([]string{"-seed-retired", "-codes", "cms=-"}, stdin, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !options.seed || len(options.external) != 1 || options.external[0].source != "cms" || !slices.Equal(options.external[0].codes, []string{"WEBLAB", "agc", "Web Lab"}) {
		t.Fatalf("options = %+v", options)
	}
	for _, args := range [][]string{
		{"extra"},
		{"-codes", "cms"},
		{"-codes", "CMS=-"},
		{"-codes", "cms=-", "-codes", "cms=/dev/null"},
		{"-codes", "cms=-", "-codes", "forms=-"},
		{"-codes", "cms=/no/such/file"},
		{"-expect", "2"},
		{"-seed-retired", "-expect", "-1"},
	} {
		errOut.Reset()
		if _, code := parseTeamStructureOptions(args, strings.NewReader(""), &errOut); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// A list line that is no code (a byte order mark left in, a blank in the
// name) is shown as skipped and never written; a mark or a comment around a
// code is dropped.
func TestTeamStructureReportSkipsLinesThatAreNoCode(t *testing.T) {
	dir := teamStructureRealm(t)
	codes := &fakeTeamCodes{}
	external, err := readCodes(strings.NewReader("\uFEFFairlab\r\nweb lab\nSKYSIS # site\nİZCİ\nnewteam\n"))
	if err != nil {
		t.Fatal(err)
	}
	options := teamStructureOptions{seed: true, external: []externalCodes{{source: "cms", codes: external}}}
	var out, errOut bytes.Buffer
	if code := teamStructureReportCommand(context.Background(), options, teamStructureTestConfig(dir, codes), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	section := sectionOf(out.String(), "(h)")
	for _, want := range []string{
		"to retire: 3, retired already: 0",
		`"airlab"  to retire`, `"SKYSIS"  to retire`, `"newteam"  to retire`,
		`"web lab"  skipped, not a code form (never written)  from cms`,
		`"İZCİ"  skipped, not a code form (never written)  from cms`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("(h) lacks %q:\n%s", want, section)
		}
	}
	if !slices.Equal(codes.retired, []string{"airlab", "newteam", "SKYSIS"}) {
		t.Fatalf("retired = %q", codes.retired)
	}
}

// -expect is the dry run's number: any other number, before the lock or
// inside it, writes nothing.
func TestTeamStructureReportExpect(t *testing.T) {
	dir := teamStructureRealm(t)
	records := map[string][]string{"GONE": {teamcode.RecordEvent}, "LATE": {teamcode.RecordEvent}}
	run := func(expect int, cfg func(*teamStructureConfig)) (int, *fakeTeamCodes, string) {
		codes := &fakeTeamCodes{records: records}
		c := teamStructureTestConfig(dir, codes)
		if cfg != nil {
			cfg(&c)
		}
		var out, errOut bytes.Buffer
		code := teamStructureReportCommand(context.Background(), teamStructureOptions{seed: true, expect: expect, expectSet: true}, c, &out, &errOut)
		return code, codes, errOut.String()
	}
	if code, codes, stderr := run(3, nil); code != 1 || codes.seeds != 0 || !strings.Contains(stderr, "nothing written: 2 codes to retire, -expect 3") {
		t.Fatalf("exit %d, seeds %d, stderr %q", code, codes.seeds, stderr)
	}
	reads := 0
	lateGroup := func(c *teamStructureConfig) {
		c.Groups = func(ctx context.Context) ([]identity.Group, error) {
			reads++
			groups, err := dir.ListGroups(ctx)
			if reads == 2 {
				groups = append(groups, identity.Group{ID: uuid.NewString(), Name: "LATE", Path: "/UYELER/SOSYAL/LATE"})
			}
			return groups, err
		}
	}
	if code, codes, stderr := run(2, lateGroup); code != 1 || len(codes.retired) != 0 || !strings.Contains(stderr, "the tree changed: 1 codes to retire, -expect 2") {
		t.Fatalf("exit %d, retired %v, stderr %q", code, codes.retired, stderr)
	}
	if code, codes, stderr := run(2, nil); code != 0 || !slices.Equal(codes.retired, []string{"GONE", "LATE"}) {
		t.Fatalf("exit %d, retired %v, stderr %q", code, codes.retired, stderr)
	}
}
