package githubactivity_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// appKey is one RSA key for the package's tests: generating one is slow under
// -race.
func appKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
	})
	return testKey
}

// appKeyPEM is the key the way GitHub hands it out (PKCS #1).
func appKeyPEM(t *testing.T) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey(t))}))
}

// clock is the tests' time, shared by the service and the fake GitHub.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeCommit struct {
	Login string // "" is a commit GitHub links to no account
	Bot   bool
	At    time.Time
}

type fakePull struct {
	Title     string
	Login     string
	MergedAt  time.Time // zero: closed without merging
	UpdatedAt time.Time
}

type fakeRelease struct {
	Name, Tag string
	Draft     bool
	At        time.Time
}

type fakeRepo struct {
	Name        string
	Description string
	Language    string
	Private     bool
	Visibility  string // "" follows Private
	Archived    bool
	Fork        bool
	PushedAt    time.Time
	Branch      string // "" is "main"
	OpenPRs     int
	Empty       bool // commits answer 409
	Commits     []fakeCommit
	Pulls       []fakePull
	Releases    []fakeRelease
}

// fakeGitHub is GitHub's API as far as core uses it: the app's installation
// and token, the GraphQL repository list and the REST commits, pulls and
// releases, each list paged pageSize at a time.
type fakeGitHub struct {
	t            *testing.T
	srv          *httptest.Server
	clock        *clock
	appID        int64
	org          string
	installation int64
	pageSize     int

	mu          sync.Mutex
	repos       []fakeRepo
	down        bool
	limitReset  time.Time // while in the future, every API call is a 403 rate limit
	tokens      []string
	tokenTTL    time.Duration
	calls       map[string]int
	paths       []string
	permissions []map[string]string
	gate        chan struct{} // non-nil: the GraphQL list waits on it
	nextLink    string        // non-empty: the first commits page names this as the next page
}

func newFakeGitHub(t *testing.T, c *clock, repos ...fakeRepo) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t: t, clock: c, appID: 4242, org: "skylab-kulubu", installation: 777, pageSize: 2,
		repos: repos, tokenTTL: time.Hour, calls: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind]
}

func (f *fakeGitHub) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.calls {
		n += v
	}
	return n
}

func (f *fakeGitHub) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

func (f *fakeGitHub) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	down, limited := f.down, f.clock.Now().Before(f.limitReset)
	reset := f.limitReset
	gate := f.gate
	f.mu.Unlock()
	if down {
		http.Error(w, "unicorn", http.StatusBadGateway)
		return
	}
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("X-RateLimit-Remaining", "4000")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/"+f.org+"/installation":
		f.mustAppJWT(w, r, "installation", func() { writeJSON(w, map[string]any{"id": f.installation}) })
	case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", f.installation):
		f.mustAppJWT(w, r, "token", func() {
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			token := fmt.Sprintf("ghs_fake_%d", len(f.tokens)+1)
			f.tokens = append(f.tokens, token)
			f.permissions = append(f.permissions, body.Permissions)
			expires := f.clock.Now().Add(f.tokenTTL)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"token": token, "expires_at": expires.UTC().Format(time.RFC3339)})
		})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		// An installation that is gone (the app was reinstalled).
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		if !f.installationToken(w, r, "graphql") {
			return
		}
		if gate != nil {
			<-gate
		}
		f.graphql(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"+f.org+"/"):
		rest := strings.TrimPrefix(r.URL.Path, "/repos/"+f.org+"/")
		name, kind, _ := strings.Cut(rest, "/")
		if !f.installationToken(w, r, kind) {
			return
		}
		repo, ok := f.repo(name)
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		switch kind {
		case "commits":
			f.commits(w, r, repo)
		case "pulls":
			f.pulls(w, r, repo)
		case "releases":
			f.releases(w, repo)
		default:
			http.NotFound(w, r)
		}
	default:
		f.t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) mustAppJWT(w http.ResponseWriter, r *http.Request, kind string, next func()) {
	f.mu.Lock()
	f.calls[kind]++
	f.mu.Unlock()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, `{"message":"no JWT"}`, http.StatusUnauthorized)
		return
	}
	token, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return &appKey(f.t).PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(f.clock.Now), jwt.WithIssuedAt())
	if err != nil {
		f.t.Errorf("fake GitHub: app JWT refused: %v", err)
		http.Error(w, `{"message":"bad JWT"}`, http.StatusUnauthorized)
		return
	}
	claims := token.Claims.(jwt.MapClaims)
	if claims["iss"] != strconv.FormatInt(f.appID, 10) {
		f.t.Errorf("fake GitHub: JWT iss %v", claims["iss"])
	}
	exp, _ := claims.GetExpirationTime()
	iat, _ := claims.GetIssuedAt()
	if exp == nil || iat == nil || exp.Sub(iat.Time) > 10*time.Minute {
		f.t.Errorf("fake GitHub: JWT lifetime over ten minutes")
	}
	next()
}

func (f *fakeGitHub) installationToken(w http.ResponseWriter, r *http.Request, kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[kind]++
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(f.tokens) == 0 || raw != f.tokens[len(f.tokens)-1] {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeGitHub) repo(name string) (fakeRepo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.repos {
		if r.Name == name {
			return r, true
		}
	}
	return fakeRepo{}, false
}

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string `json:"query"`
		Variables struct {
			Org    string  `json:"org"`
			Cursor *string `json:"cursor"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Variables.Org != f.org {
		f.t.Errorf("fake GitHub: GraphQL body %v org %q", err, body.Variables.Org)
	}
	start := 0
	if body.Variables.Cursor != nil {
		start, _ = strconv.Atoi(*body.Variables.Cursor)
	}
	f.mu.Lock()
	repos := slices.Clone(f.repos)
	f.mu.Unlock()
	end := min(start+f.pageSize, len(repos))
	nodes := []map[string]any{}
	for _, repo := range repos[start:end] {
		visibility := repo.Visibility
		if visibility == "" {
			visibility = "PUBLIC"
			if repo.Private {
				visibility = "PRIVATE"
			}
		}
		node := map[string]any{
			"name": repo.Name, "url": "https://github.com/" + f.org + "/" + repo.Name,
			"description": nil, "isArchived": repo.Archived, "isFork": repo.Fork, "isPrivate": repo.Private,
			"visibility": visibility, "pushedAt": repo.PushedAt.UTC().Format(time.RFC3339),
			"primaryLanguage": nil, "defaultBranchRef": map[string]any{"name": cmpOr(repo.Branch, "main")},
			"pullRequests": map[string]any{"totalCount": repo.OpenPRs},
		}
		if repo.Description != "" {
			node["description"] = repo.Description
		}
		if repo.Language != "" {
			node["primaryLanguage"] = map[string]any{"name": repo.Language}
		}
		nodes = append(nodes, node)
	}
	writeJSON(w, map[string]any{"data": map[string]any{"organization": map[string]any{"repositories": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(repos), "endCursor": strconv.Itoa(end)},
		"nodes":    nodes,
	}}}})
}

func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, n int) (int, int) {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	start := min((page-1)*f.pageSize, n)
	end := min(start+f.pageSize, n)
	if end < n {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		next := f.srv.URL + r.URL.Path + "?" + q.Encode()
		f.mu.Lock()
		if f.nextLink != "" {
			next = f.nextLink
		}
		f.mu.Unlock()
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, f.srv.URL+r.URL.Path))
	}
	return start, end
}

func (f *fakeGitHub) commits(w http.ResponseWriter, r *http.Request, repo fakeRepo) {
	if repo.Empty {
		http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
		return
	}
	if got := r.URL.Query().Get("sha"); got != cmpOr(repo.Branch, "main") {
		f.t.Errorf("fake GitHub: commits of %s read from %q, not the default branch", repo.Name, got)
	}
	since, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	until, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("until"))
	if err1 != nil || err2 != nil {
		f.t.Errorf("fake GitHub: commits without since/until")
	}
	var list []fakeCommit
	for _, c := range repo.Commits {
		if !c.At.Before(since) && !c.At.After(until) {
			list = append(list, c)
		}
	}
	slices.SortStableFunc(list, func(a, b fakeCommit) int { return b.At.Compare(a.At) })
	start, end := f.page(w, r, len(list))
	out := []map[string]any{}
	for _, c := range list[start:end] {
		item := map[string]any{"commit": map[string]any{
			"author":    map[string]any{"date": c.At.UTC().Format(time.RFC3339)},
			"committer": map[string]any{"date": c.At.UTC().Format(time.RFC3339)},
		}, "author": nil}
		if c.Login != "" {
			kind := "User"
			if c.Bot {
				kind = "Bot"
			}
			item["author"] = map[string]any{"login": c.Login, "avatar_url": "https://avatars.example/" + c.Login, "type": kind}
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}

func (f *fakeGitHub) pulls(w http.ResponseWriter, r *http.Request, repo fakeRepo) {
	q := r.URL.Query()
	if q.Get("state") != "closed" || q.Get("sort") != "updated" || q.Get("direction") != "desc" {
		f.t.Errorf("fake GitHub: pulls asked as %s", r.URL.RawQuery)
	}
	list := slices.Clone(repo.Pulls)
	slices.SortStableFunc(list, func(a, b fakePull) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	start, end := f.page(w, r, len(list))
	out := []map[string]any{}
	for i, p := range list[start:end] {
		item := map[string]any{
			"title": p.Title, "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", f.org, repo.Name, start+i+1),
			"user": map[string]any{"login": p.Login, "type": "User"}, "merged_at": nil,
			"updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if !p.MergedAt.IsZero() {
			item["merged_at"] = p.MergedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}

func (f *fakeGitHub) releases(w http.ResponseWriter, repo fakeRepo) {
	out := []map[string]any{}
	for _, rel := range repo.Releases {
		out = append(out, map[string]any{
			"name": rel.Name, "tag_name": rel.Tag, "draft": rel.Draft,
			"html_url":     "https://github.com/" + f.org + "/" + repo.Name + "/releases/tag/" + url.PathEscape(rel.Tag),
			"author":       map[string]any{"login": "releaser", "type": "User"},
			"published_at": rel.At.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
