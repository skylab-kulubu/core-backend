package githubactivity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
)

// 15:00 in Türkiye. With a seven-day window the window starts on 27 September
// at midnight Turkish time (26 September 21:00 UTC), the previous one on 20
// September.
var testNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

var (
	yk     = authz.Principal{ID: "u-yk", Groups: []string{"/UYELER/YK"}}
	member = authz.Principal{ID: "u-member", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// clubRepos is an organisation with every kind of repository core meets.
func clubRepos(t *testing.T) []fakeRepo {
	return []fakeRepo{
		{
			Name: "site", Description: "Kulüp sitesi", Language: "Go", PushedAt: at(t, "2026-10-02T11:00:00Z"), OpenPRs: 2,
			Commits: []fakeCommit{
				{Login: "alice", At: at(t, "2026-09-28T10:00:00Z")},
				{Login: "alice", At: at(t, "2026-10-01T10:00:00Z")},
				{Login: "alice", At: at(t, "2026-10-02T10:00:00Z")},
				{Login: "bob", At: at(t, "2026-10-02T11:00:00Z")},
				{Login: "dependabot[bot]", Bot: true, At: at(t, "2026-10-01T09:00:00Z")},
				// 27 September 01:00 in Türkiye: the window's first day.
				{Login: "", At: at(t, "2026-09-26T22:00:00Z")},
				// The previous window: 22 September and 26 September 23:00 in Türkiye.
				{Login: "alice", At: at(t, "2026-09-22T10:00:00Z")},
				{Login: "alice", At: at(t, "2026-09-26T20:00:00Z")},
				{Login: "alice", At: at(t, "2026-09-10T10:00:00Z")},
			},
			Pulls: []fakePull{
				{Title: "Add footer", Login: "carol", MergedAt: at(t, "2026-10-01T15:00:00Z"), UpdatedAt: at(t, "2026-10-01T15:00:00Z")},
				{Title: "Abandoned", Login: "bob", UpdatedAt: at(t, "2026-09-30T08:00:00Z")},
				{Title: "Typo", Login: "alice", MergedAt: at(t, "2026-09-26T12:00:00Z"), UpdatedAt: at(t, "2026-09-29T08:00:00Z")},
				{Title: "Ancient", Login: "alice", MergedAt: at(t, "2026-09-10T08:00:00Z"), UpdatedAt: at(t, "2026-09-10T08:00:00Z")},
				{Title: "Older", Login: "alice", MergedAt: at(t, "2026-09-01T08:00:00Z"), UpdatedAt: at(t, "2026-09-01T08:00:00Z")},
			},
			Releases: []fakeRelease{
				{Name: "Sürüm 1.2", Tag: "v1.2", At: at(t, "2026-10-02T09:00:00Z")},
				{Name: "next", Tag: "v1.3", Draft: true, At: at(t, "2026-10-02T10:00:00Z")},
				{Name: "", Tag: "v1.0", At: at(t, "2026-09-01T09:00:00Z")},
			},
		},
		{
			Name: "api", PushedAt: at(t, "2026-10-01T08:00:00Z"),
			Commits: []fakeCommit{
				{Login: "frank", At: at(t, "2026-09-30T08:00:00Z")},
				{Login: "frank", At: at(t, "2026-10-01T08:00:00Z")},
			},
		},
		{
			Name: "secret-infra", Description: "Top secret infra", Language: "HCL", Private: true,
			PushedAt: at(t, "2026-10-02T12:00:00Z"), OpenPRs: 1,
			Commits: []fakeCommit{
				{Login: "dave", At: at(t, "2026-09-28T08:00:00Z")},
				{Login: "dave", At: at(t, "2026-09-29T08:00:00Z")},
				{Login: "dave", At: at(t, "2026-10-01T08:00:00Z")},
				{Login: "dave", At: at(t, "2026-10-02T08:00:00Z")},
			},
			Pulls:    []fakePull{{Title: "Rotate prod creds", Login: "dave", MergedAt: at(t, "2026-10-02T08:00:00Z"), UpdatedAt: at(t, "2026-10-02T08:00:00Z")}},
			Releases: []fakeRelease{{Name: "secret release", Tag: "s1", At: at(t, "2026-10-02T08:00:00Z")}},
		},
		{
			Name: "internal-tool", Visibility: "INTERNAL", PushedAt: at(t, "2026-10-01T08:00:00Z"),
			Commits: []fakeCommit{{Login: "erin", At: at(t, "2026-10-01T08:00:00Z")}},
		},
		{
			Name: "old-archive", Archived: true, PushedAt: at(t, "2026-10-02T08:00:00Z"), OpenPRs: 5,
			Commits: []fakeCommit{{Login: "alice", At: at(t, "2026-10-02T08:00:00Z")}},
		},
		{
			Name: "forked-lib", Fork: true, PushedAt: at(t, "2026-10-02T08:00:00Z"), OpenPRs: 3,
			Commits: []fakeCommit{{Login: "mallory", At: at(t, "2026-10-02T08:00:00Z")}},
		},
		{Name: "quiet", PushedAt: at(t, "2026-08-01T08:00:00Z"), OpenPRs: 1},
		{Name: "empty", PushedAt: at(t, "2026-10-01T08:00:00Z"), Empty: true},
	}
}

type harness struct {
	f       *fakeGitHub
	clock   *clock
	service *githubactivity.Service
	logs    *logs
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newHarness(t *testing.T, edit ...func(*githubactivity.Config)) *harness {
	t.Helper()
	c := newClock(testNow)
	f := newFakeGitHub(t, c, clubRepos(t)...)
	config := githubactivity.Config{Org: f.org, AppID: f.appID, InstallationID: f.installation, PrivateKey: appKey(t), WindowDays: 7}
	for _, e := range edit {
		e(&config)
	}
	l := &logs{}
	s := githubactivity.New(config, authz.NewAuthorizer(authz.DefaultPolicy()), githubactivity.Options{
		APIURL: f.srv.URL, HTTP: f.srv.Client(), Now: c.Now, Logf: l.Logf,
	})
	return &harness{f: f, clock: c, service: s, logs: l}
}

func (h *harness) get(t *testing.T) githubactivity.Activity {
	t.Helper()
	a, err := h.service.Get(context.Background(), yk)
	if err != nil {
		t.Fatalf("Get: %v (logs: %s)", err, h.logs.all())
	}
	return a
}

func strp(s string) *string { return &s }

func TestActivityOfTheOrganisation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got := h.get(t)

	if got.Org != "skylab-kulubu" || !got.GeneratedAt.Equal(testNow) || got.Stale {
		t.Fatalf("org %q generatedAt %s stale %v", got.Org, got.GeneratedAt, got.Stale)
	}
	if want := at(t, "2026-09-27T00:00:00+03:00"); !got.Window.Since.Equal(want) || got.Window.Days != 7 {
		t.Fatalf("window %+v, want since %s days 7", got.Window, want)
	}
	wantTotals := githubactivity.Totals{
		Commits: 13, CommitsPrevious: 2, MergedPullRequests: 2, OpenPullRequests: 4, ActiveContributors: 6,
		PrivateRepositories: githubactivity.PrivateTotals{Active: 2, Commits: 5},
	}
	if got.Totals != wantTotals {
		t.Fatalf("totals\n got %+v\nwant %+v", got.Totals, wantTotals)
	}
	wantRepos := []githubactivity.Repository{
		{
			Name: "site", URL: "https://github.com/skylab-kulubu/site", Description: strp("Kulüp sitesi"), Language: strp("Go"),
			PushedAt: at(t, "2026-10-02T11:00:00Z"), Commits: 6, CommitsByDay: []int{1, 1, 0, 0, 2, 2, 0}, OpenPullRequests: 2,
			Contributors: []githubactivity.Contributor{
				{Login: "alice", AvatarURL: "https://avatars.example/alice", Commits: 3},
				{Login: "bob", AvatarURL: "https://avatars.example/bob", Commits: 1},
			},
		},
		{
			Name: "api", URL: "https://github.com/skylab-kulubu/api", PushedAt: at(t, "2026-10-01T08:00:00Z"),
			Commits: 2, CommitsByDay: []int{0, 0, 0, 1, 1, 0, 0}, OpenPullRequests: 0,
			Contributors: []githubactivity.Contributor{{Login: "frank", AvatarURL: "https://avatars.example/frank", Commits: 2}},
		},
	}
	if !reflect.DeepEqual(got.Repositories, wantRepos) {
		gotJSON, _ := json.MarshalIndent(got.Repositories, "", " ")
		t.Fatalf("repositories\n%s", gotJSON)
	}
	wantEvents := []githubactivity.Event{
		{Kind: githubactivity.KindRelease, Repository: "site", Title: "Sürüm 1.2", URL: "https://github.com/skylab-kulubu/site/releases/tag/v1.2", Author: strp("releaser"), At: at(t, "2026-10-02T09:00:00Z")},
		{Kind: githubactivity.KindPullRequestMerged, Repository: "site", Title: "Add footer", URL: "https://github.com/skylab-kulubu/site/pull/1", Author: strp("carol"), At: at(t, "2026-10-01T15:00:00Z")},
	}
	if !reflect.DeepEqual(got.Events, wantEvents) {
		gotJSON, _ := json.MarshalIndent(got.Events, "", " ")
		t.Fatalf("events\n%s", gotJSON)
	}
}

// The response matches core-frontend's GithubActivity field for field.
func TestActivityJSONShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body, err := json.Marshal(h.get(t))
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatal(err)
	}
	keys := func(v any) []string {
		var out []string
		for k := range v.(map[string]any) {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got, want := keys(shape), []string{"events", "generatedAt", "org", "repositories", "stale", "totals", "window"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level keys %v", got)
	}
	if got, want := keys(shape["window"]), []string{"days", "since"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("window keys %v", got)
	}
	totals := shape["totals"].(map[string]any)
	if got, want := keys(totals), []string{"activeContributors", "commits", "commitsPrevious", "mergedPullRequests", "openPullRequests", "privateRepositories"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("totals keys %v", got)
	}
	if got, want := keys(totals["privateRepositories"]), []string{"active", "commits"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("privateRepositories keys %v", got)
	}
	repo := shape["repositories"].([]any)[0]
	if got, want := keys(repo), []string{"commits", "commitsByDay", "contributors", "description", "language", "name", "openPullRequests", "pushedAt", "url"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repository keys %v", got)
	}
	if got, want := keys(repo.(map[string]any)["contributors"].([]any)[0]), []string{"avatarUrl", "commits", "login"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contributor keys %v", got)
	}
	if got, want := keys(shape["events"].([]any)[0]), []string{"at", "author", "kind", "repository", "title", "url"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event keys %v", got)
	}
	if !strings.Contains(string(body), `"since":"2026-09-27T00:00:00+03:00"`) || !strings.Contains(string(body), `"generatedAt":"2026-10-03T12:00:00Z"`) {
		t.Fatalf("times are not ISO 8601 as expected: %s", body)
	}
	if !strings.Contains(string(body), `"description":null,"language":null`) {
		t.Fatalf("a repository without description or language should say null: %s", body)
	}
}

func TestPrivateRepositoriesOnlyCountInTotals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body, err := json.Marshal(h.get(t))
	if err != nil {
		t.Fatal(err)
	}
	// Names, descriptions, pull request titles, releases and the people only
	// seen in private (or internal) repositories.
	for _, secret := range []string{"secret-infra", "Top secret", "HCL", "Rotate prod creds", "secret release", "internal-tool", "dave", "erin"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the response names %q from a private repository: %s", secret, body)
		}
	}
	for _, path := range h.f.requested() {
		if strings.HasPrefix(path, "/repos/skylab-kulubu/secret-infra/releases") {
			t.Errorf("a private repository's releases were read: %s", path)
		}
	}
}

func TestArchivedForkedAndQuietRepositoriesAreLeftOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got := h.get(t)
	for _, r := range got.Repositories {
		if r.Name == "old-archive" || r.Name == "forked-lib" {
			t.Errorf("%s is listed", r.Name)
		}
	}
	// Their open pull requests (5 and 3) and commits are not counted either.
	if got.Totals.OpenPullRequests != 4 || got.Totals.Commits != 13 {
		t.Fatalf("totals %+v", got.Totals)
	}
	for _, path := range h.f.requested() {
		for _, name := range []string{"old-archive", "forked-lib", "quiet"} {
			if strings.HasPrefix(path, "/repos/skylab-kulubu/"+name+"/") {
				t.Errorf("%s was read: %s", name, path)
			}
		}
	}
}

func TestEveryPageIsRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got := h.get(t)
	// Eight repositories two to a page: four GraphQL pages.
	if n := h.f.count("graphql"); n != 4 {
		t.Fatalf("GraphQL pages read: %d, want 4", n)
	}
	// site's eight commits in the two windows come two to a page; all are
	// counted.
	if got.Repositories[0].Commits != 6 || got.Totals.CommitsPrevious != 2 {
		t.Fatalf("commits %d, previous %d", got.Repositories[0].Commits, got.Totals.CommitsPrevious)
	}
	// site's closed pull requests: the second page holds one last updated
	// before the window, so a third is not read. The four other recently
	// pushed repositories (api, secret-infra, internal-tool, empty) read one
	// page each.
	if n := h.f.count("pulls"); n != 2+4 {
		t.Fatalf("pull request pages read: %d, want 6", n)
	}
}

func TestCachedForTenMinutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.get(t)
	h.clock.Advance(9 * time.Minute)
	h.get(t)
	if n := h.f.count("graphql"); n != 4 {
		t.Fatalf("GitHub read again within ten minutes (%d GraphQL calls)", n)
	}
	h.clock.Advance(2 * time.Minute)
	got := h.get(t)
	if n := h.f.count("graphql"); n != 8 {
		t.Fatalf("GitHub not read again after ten minutes (%d GraphQL calls)", n)
	}
	if !got.GeneratedAt.Equal(testNow.Add(11*time.Minute)) || got.Stale {
		t.Fatalf("generatedAt %s stale %v", got.GeneratedAt, got.Stale)
	}
}

func TestGitHubDownServesTheLastGoodAnswerStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := h.get(t)
	h.clock.Advance(11 * time.Minute)
	h.f.setDown(true)
	got := h.get(t)
	if !got.Stale || !got.GeneratedAt.Equal(first.GeneratedAt) || got.Totals != first.Totals {
		t.Fatalf("stale answer %+v", got)
	}
	if !strings.Contains(h.logs.all(), "github activity: refresh failed") || !strings.Contains(h.logs.all(), "502") {
		t.Fatalf("the failure is not logged: %q", h.logs.all())
	}
	// GitHub is left alone for a minute after a failure.
	before := h.f.total()
	h.clock.Advance(30 * time.Second)
	if again := h.get(t); !again.Stale {
		t.Fatal("not stale")
	}
	if h.f.total() != before {
		t.Fatal("GitHub was asked again within a minute of a failure")
	}
	h.f.setDown(false)
	h.clock.Advance(31 * time.Second)
	fresh := h.get(t)
	if fresh.Stale || !fresh.GeneratedAt.Equal(h.clock.Now()) {
		t.Fatalf("not fresh after GitHub came back: stale %v generatedAt %s", fresh.Stale, fresh.GeneratedAt)
	}
	// The stale copy did not mark the cached answer.
	if cached := h.get(t); cached.Stale {
		t.Fatal("the cached answer turned stale")
	}
}

func TestGitHubDownWithNothingReadIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.setDown(true)
	if _, err := h.service.Get(context.Background(), yk); !errors.Is(err, githubactivity.ErrUnavailable) {
		t.Fatalf("err %v, want ErrUnavailable", err)
	}
}

func TestOneReadOfGitHubAtATime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	gate := make(chan struct{})
	h.f.mu.Lock()
	h.f.gate = gate
	h.f.mu.Unlock()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.service.Get(context.Background(), yk)
			errs <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.f.count("graphql") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.f.count("graphql"); n != 4 {
		t.Fatalf("%d GraphQL calls for eight callers, want one read (4)", n)
	}
	if n := h.f.count("token"); n != 1 {
		t.Fatalf("%d installation tokens, want 1", n)
	}
}

// A caller who gives up gets the last good answer; the read goes on for the
// others.
func TestACallerWhoGivesUpGetsTheLastAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := h.get(t)
	h.clock.Advance(11 * time.Minute)
	gate := make(chan struct{})
	h.f.mu.Lock()
	h.f.gate = gate
	h.f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := h.service.Get(ctx, yk)
	if err != nil || !got.Stale || !got.GeneratedAt.Equal(first.GeneratedAt) {
		t.Fatalf("got stale=%v err=%v", got.Stale, err)
	}
	close(gate)
	fresh := h.get(t)
	if fresh.Stale {
		t.Fatal("the read did not finish for the next caller")
	}
}

func TestInstallationTokenKeptUntilShortlyBeforeItExpires(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.get(t)
	h.clock.Advance(11 * time.Minute)
	h.get(t)
	h.clock.Advance(11 * time.Minute)
	h.get(t)
	if n := h.f.count("token"); n != 1 {
		t.Fatalf("%d tokens within 22 minutes of an hour-long one", n)
	}
	// 56 minutes in: under five minutes left, so a new one.
	h.clock.Advance(34 * time.Minute)
	h.get(t)
	if n := h.f.count("token"); n != 2 {
		t.Fatalf("%d tokens, want a second one near the first's expiry", n)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	want := map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read"}
	for _, p := range h.f.permissions {
		if !reflect.DeepEqual(p, want) {
			t.Fatalf("token asked for %v, want only %v", p, want)
		}
	}
}

func TestInstallationFoundFromTheOrganisation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *githubactivity.Config) { c.InstallationID = 0 })
	h.get(t)
	h.clock.Advance(56 * time.Minute)
	h.get(t)
	if n := h.f.count("installation"); n != 1 {
		t.Fatalf("installation looked up %d times, want once", n)
	}
	if n := h.f.count("token"); n != 2 {
		t.Fatalf("%d tokens", n)
	}
	known := newHarness(t)
	known.get(t)
	if n := known.f.count("installation"); n != 0 {
		t.Fatalf("installation looked up %d times although configured", n)
	}
}

func TestARefusedTokenIsReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.get(t)
	// GitHub revokes the token (the app's key rotated, say).
	h.f.mu.Lock()
	h.f.tokens = append(h.f.tokens, "revoked-elsewhere")
	h.f.mu.Unlock()
	h.clock.Advance(11 * time.Minute)
	if got := h.get(t); !got.Stale {
		t.Fatal("want the last answer while the token is refused")
	}
	h.clock.Advance(61 * time.Second)
	if got := h.get(t); got.Stale {
		t.Fatal("a new token was not fetched")
	}
	if n := h.f.count("token"); n != 2 {
		t.Fatalf("%d tokens, want 2", n)
	}
}

func TestRateLimitHoldsRequestsUntilItResets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.get(t)
	h.clock.Advance(11 * time.Minute)
	h.f.mu.Lock()
	h.f.limitReset = h.clock.Now().Add(30 * time.Minute)
	h.f.mu.Unlock()
	if got := h.get(t); !got.Stale {
		t.Fatal("want the last answer while rate limited")
	}
	before := h.f.total()
	// Past the minute's pause after a failure but before GitHub's reset: core
	// asks nothing.
	h.clock.Advance(5 * time.Minute)
	if got := h.get(t); !got.Stale {
		t.Fatal("want the last answer while rate limited")
	}
	if h.f.total() != before {
		t.Fatalf("GitHub asked %d more times before its reset", h.f.total()-before)
	}
	h.clock.Advance(26 * time.Minute)
	if got := h.get(t); got.Stale {
		t.Fatal("not read again after the reset")
	}
}

func TestANextPageElsewhereIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.mu.Lock()
	h.f.nextLink = "https://attacker.example/steal?page=2"
	h.f.mu.Unlock()
	if _, err := h.service.Get(context.Background(), yk); !errors.Is(err, githubactivity.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(h.logs.all(), "outside the GitHub API") {
		t.Fatalf("logs %q", h.logs.all())
	}
}

func TestOnlyPrivilegedPeopleReadIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	service := authz.Principal{ID: "svc", Groups: []string{"/ADMIN"}, Product: authz.ProductForms}
	for _, p := range []authz.Principal{member, service, {}} {
		if _, err := h.service.Get(context.Background(), p); !errors.Is(err, githubactivity.ErrForbidden) {
			t.Fatalf("%+v: err %v, want ErrForbidden", p, err)
		}
	}
	if h.f.total() != 0 {
		t.Fatal("GitHub was read for a forbidden caller")
	}
	for _, p := range []authz.Principal{yk, {ID: "a", Groups: []string{"/ADMIN"}}, {ID: "d", Groups: []string{"/UYELER/DK"}}} {
		if _, err := h.service.Get(context.Background(), p); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}
}

func TestUnavailableServiceChecksTheCallerFirst(t *testing.T) {
	t.Parallel()
	s := githubactivity.Unavailable(authz.NewAuthorizer(authz.DefaultPolicy()), errors.New("GITHUB_ACTIVITY_APP_ID is required"))
	if _, err := s.Get(context.Background(), member); !errors.Is(err, githubactivity.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	if _, err := s.Get(context.Background(), yk); !errors.Is(err, githubactivity.ErrUnavailable) {
		t.Fatalf("yk: %v", err)
	}
}

// No log line carries a token or the key.
func TestLogsCarryNoToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.setDown(true)
	_, _ = h.service.Get(context.Background(), yk)
	h.f.setDown(false)
	h.clock.Advance(2 * time.Minute)
	h.get(t)
	h.f.mu.Lock()
	h.f.tokens = append(h.f.tokens, "revoked")
	h.f.mu.Unlock()
	h.clock.Advance(11 * time.Minute)
	h.get(t)
	text := h.logs.all()
	if text == "" {
		t.Fatal("expected failure logs")
	}
	for _, leak := range []string{"ghs_", "Bearer", "BEGIN", "eyJ"} {
		if strings.Contains(text, leak) {
			t.Fatalf("log carries %q: %s", leak, text)
		}
	}
}

// An app uninstalled and installed again has a new installation: core finds
// it when the old one is gone, if it found the first one itself.
func TestAReinstalledAppIsFoundAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *githubactivity.Config) { c.InstallationID = 0 })
	h.get(t)
	h.f.mu.Lock()
	h.f.installation = 778
	h.f.mu.Unlock()
	h.clock.Advance(56 * time.Minute)
	if got := h.get(t); !got.Stale {
		t.Fatal("want the last answer while the old installation is gone")
	}
	h.clock.Advance(61 * time.Second)
	if got := h.get(t); got.Stale {
		t.Fatalf("the new installation was not found: %s", h.logs.all())
	}
	if n := h.f.count("installation"); n != 2 {
		t.Fatalf("installation looked up %d times, want 2", n)
	}
}
