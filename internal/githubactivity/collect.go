package githubactivity

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// turkey is the club's clock: Türkiye keeps UTC+3 all year (since 2016), so a
// fixed zone needs no time zone database in the image.
var turkey = time.FixedZone("TRT", 3*60*60)

const (
	// maxRepositories and maxEvents bound the answer's lists; the dashboard
	// shows six and eight.
	maxRepositories = 20
	maxEvents       = 20
	// maxContributors is how many people a repository lists.
	maxContributors = 5
	// workers is how many repositories are read at once.
	workers = 4
	// maxPages bounds the pages read of one list, against a runaway.
	maxPages = 50
)

const repositoriesQuery = `query($org: String!, $cursor: String) {
  organization(login: $org) {
    repositories(first: 100, after: $cursor, orderBy: {field: PUSHED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        url
        description
        isArchived
        isFork
        isPrivate
        visibility
        pushedAt
        primaryLanguage { name }
        defaultBranchRef { name }
        pullRequests(states: OPEN) { totalCount }
      }
    }
  }
}`

// repo is a repository as core reads it. For a private one, only the counts
// below ever leave this package.
type repo struct {
	name          string
	url           string
	description   *string
	language      *string
	pushedAt      time.Time
	private       bool
	defaultBranch string
	openPRs       int

	// What the window holds, filled for repositories pushed to since the
	// previous window began.
	commits         int
	commitsPrevious int
	commitsByDay    []int
	contributors    []Contributor
	people          []string
	mergedPRs       []Event
	releases        []Event
}

// collector gathers the activity from GitHub.
type collector struct {
	gh   *client
	org  string
	days int
	now  func() time.Time
}

func (c *collector) collect(ctx context.Context) (Activity, error) {
	now := c.now()
	today := time.Date(now.In(turkey).Year(), now.In(turkey).Month(), now.In(turkey).Day(), 0, 0, 0, 0, turkey)
	since := today.AddDate(0, 0, -(c.days - 1))
	previous := since.AddDate(0, 0, -c.days)

	covered, err := c.repositories(ctx)
	if err != nil {
		return Activity{}, err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(workers)
	for _, r := range covered {
		// A commit is pushed after it is made, so a repository last pushed
		// before the previous window has nothing in either.
		if r.pushedAt.Before(previous) || r.defaultBranch == "" {
			continue
		}
		group.Go(func() error { return c.fill(groupCtx, r, previous, since, now) })
	}
	if err := group.Wait(); err != nil {
		return Activity{}, err
	}

	activity := Activity{
		Org:          c.org,
		GeneratedAt:  now.UTC(),
		Window:       Window{Since: since, Days: c.days},
		Repositories: []Repository{},
		Events:       []Event{},
	}
	people := map[string]bool{}
	for _, r := range covered {
		activity.Totals.Commits += r.commits
		activity.Totals.CommitsPrevious += r.commitsPrevious
		activity.Totals.MergedPullRequests += len(r.mergedPRs)
		activity.Totals.OpenPullRequests += r.openPRs
		for _, login := range r.people {
			people[login] = true
		}
		active := r.commits > 0 || len(r.mergedPRs) > 0 || len(r.releases) > 0
		if !active {
			continue
		}
		if r.private {
			activity.Totals.PrivateRepositories.Active++
			activity.Totals.PrivateRepositories.Commits += r.commits
			continue
		}
		activity.Repositories = append(activity.Repositories, Repository{
			Name: r.name, URL: r.url, Description: r.description, Language: r.language, PushedAt: r.pushedAt,
			Commits: r.commits, CommitsByDay: r.commitsByDay, OpenPullRequests: r.openPRs, Contributors: r.contributors,
		})
		activity.Events = append(activity.Events, r.mergedPRs...)
		activity.Events = append(activity.Events, r.releases...)
	}
	activity.Totals.ActiveContributors = len(people)
	slices.SortStableFunc(activity.Repositories, func(a, b Repository) int {
		if a.Commits != b.Commits {
			return b.Commits - a.Commits
		}
		if !a.PushedAt.Equal(b.PushedAt) {
			return b.PushedAt.Compare(a.PushedAt)
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(activity.Repositories) > maxRepositories {
		activity.Repositories = activity.Repositories[:maxRepositories]
	}
	slices.SortStableFunc(activity.Events, func(a, b Event) int {
		if !a.At.Equal(b.At) {
			return b.At.Compare(a.At)
		}
		return strings.Compare(a.URL, b.URL)
	})
	if len(activity.Events) > maxEvents {
		activity.Events = activity.Events[:maxEvents]
	}
	return activity, nil
}

// repositories lists the organisation's repositories the installation sees,
// archived ones and forks left out.
func (c *collector) repositories(ctx context.Context) ([]*repo, error) {
	var out []*repo
	var cursor *string
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, errors.New("githubactivity: too many pages of repositories")
		}
		var data struct {
			Organization *struct {
				Repositories struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Name            string     `json:"name"`
						URL             string     `json:"url"`
						Description     *string    `json:"description"`
						IsArchived      bool       `json:"isArchived"`
						IsFork          bool       `json:"isFork"`
						IsPrivate       bool       `json:"isPrivate"`
						Visibility      string     `json:"visibility"`
						PushedAt        *time.Time `json:"pushedAt"`
						PrimaryLanguage *struct {
							Name string `json:"name"`
						} `json:"primaryLanguage"`
						DefaultBranchRef *struct {
							Name string `json:"name"`
						} `json:"defaultBranchRef"`
						PullRequests struct {
							TotalCount int `json:"totalCount"`
						} `json:"pullRequests"`
					} `json:"nodes"`
				} `json:"repositories"`
			} `json:"organization"`
		}
		variables := map[string]any{"org": c.org, "cursor": cursor}
		if err := c.gh.graphql(ctx, repositoriesQuery, variables, &data); err != nil {
			return nil, err
		}
		if data.Organization == nil {
			return nil, errors.New("githubactivity: GitHub does not show the organisation to the app")
		}
		for _, node := range data.Organization.Repositories.Nodes {
			if node.IsArchived || node.IsFork {
				continue
			}
			r := &repo{
				name: node.Name, url: node.URL, description: nonEmpty(node.Description), openPRs: node.PullRequests.TotalCount,
				// Anything but public (private, internal) counts only in totals.
				private: node.IsPrivate || !strings.EqualFold(node.Visibility, "PUBLIC"),
			}
			if node.PushedAt != nil {
				r.pushedAt = node.PushedAt.UTC()
			}
			if node.PrimaryLanguage != nil && node.PrimaryLanguage.Name != "" {
				language := node.PrimaryLanguage.Name
				r.language = &language
			}
			if node.DefaultBranchRef != nil {
				r.defaultBranch = node.DefaultBranchRef.Name
			}
			out = append(out, r)
		}
		info := data.Organization.Repositories.PageInfo
		if !info.HasNextPage || info.EndCursor == "" {
			return out, nil
		}
		next := info.EndCursor
		cursor = &next
	}
}

// fill reads a repository's commits, merged pull requests and (public ones)
// releases from previous on.
func (c *collector) fill(ctx context.Context, r *repo, previous, since, now time.Time) error {
	if err := c.fillCommits(ctx, r, previous, since, now); err != nil {
		return err
	}
	if err := c.fillPullRequests(ctx, r, since); err != nil {
		return err
	}
	if !r.private {
		return c.fillReleases(ctx, r, since)
	}
	return nil
}

func (c *collector) repoPath(r *repo) string {
	return c.gh.base + "/repos/" + url.PathEscape(c.org) + "/" + url.PathEscape(r.name)
}

type githubUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Type      string `json:"type"`
}

func (u *githubUser) person() bool {
	return u != nil && u.Login != "" && u.Type != "Bot" && !strings.HasSuffix(u.Login, "[bot]")
}

// fillCommits counts the default branch's commits by their committer date:
// the date GitHub's since and until filter on.
func (c *collector) fillCommits(ctx context.Context, r *repo, previous, since, now time.Time) error {
	r.commitsByDay = make([]int, c.days)
	query := url.Values{}
	query.Set("sha", r.defaultBranch)
	query.Set("since", previous.UTC().Format(time.RFC3339))
	query.Set("until", now.UTC().Format(time.RFC3339))
	query.Set("per_page", "100")
	address := c.repoPath(r) + "/commits?" + query.Encode()
	byLogin := map[string]*Contributor{}
	for page := 0; address != ""; page++ {
		if page == maxPages {
			return errors.New("githubactivity: too many pages of commits")
		}
		var commits []struct {
			Commit struct {
				Committer struct {
					Date time.Time `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
			Author *githubUser `json:"author"`
		}
		next, err := c.gh.get(ctx, address, &commits)
		var status *statusError
		if errors.As(err, &status) && status.Status == http.StatusConflict {
			// An empty repository.
			return nil
		}
		if err != nil {
			return err
		}
		for _, commit := range commits {
			at := commit.Commit.Committer.Date
			switch {
			case at.Before(previous) || at.After(now):
				continue
			case at.Before(since):
				r.commitsPrevious++
				continue
			}
			r.commits++
			day := int(at.Sub(since) / (24 * time.Hour))
			if day >= 0 && day < c.days {
				r.commitsByDay[day]++
			}
			if !commit.Author.person() {
				continue
			}
			key := strings.ToLower(commit.Author.Login)
			if byLogin[key] == nil {
				byLogin[key] = &Contributor{Login: commit.Author.Login, AvatarURL: commit.Author.AvatarURL}
				r.people = append(r.people, key)
			}
			byLogin[key].Commits++
		}
		address = next
	}
	for _, person := range byLogin {
		r.contributors = append(r.contributors, *person)
	}
	slices.SortFunc(r.contributors, func(a, b Contributor) int {
		if a.Commits != b.Commits {
			return b.Commits - a.Commits
		}
		return strings.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login))
	})
	if len(r.contributors) > maxContributors {
		r.contributors = r.contributors[:maxContributors]
	}
	if r.contributors == nil {
		r.contributors = []Contributor{}
	}
	return nil
}

// fillPullRequests finds the pull requests merged in the window: closed ones,
// most recently updated first, until one was last updated before the window.
func (c *collector) fillPullRequests(ctx context.Context, r *repo, since time.Time) error {
	query := url.Values{}
	query.Set("state", "closed")
	query.Set("sort", "updated")
	query.Set("direction", "desc")
	query.Set("per_page", "100")
	address := c.repoPath(r) + "/pulls?" + query.Encode()
	for page := 0; address != ""; page++ {
		if page == maxPages {
			return errors.New("githubactivity: too many pages of pull requests")
		}
		var pulls []struct {
			Title     string      `json:"title"`
			HTMLURL   string      `json:"html_url"`
			User      *githubUser `json:"user"`
			MergedAt  *time.Time  `json:"merged_at"`
			UpdatedAt time.Time   `json:"updated_at"`
		}
		next, err := c.gh.get(ctx, address, &pulls)
		if err != nil {
			return err
		}
		older := false
		for _, pull := range pulls {
			if pull.UpdatedAt.Before(since) {
				older = true
				continue
			}
			if pull.MergedAt == nil || pull.MergedAt.Before(since) {
				continue
			}
			event := Event{Kind: KindPullRequestMerged, Repository: r.name, Title: pull.Title, URL: pull.HTMLURL, At: pull.MergedAt.UTC()}
			if pull.User != nil && pull.User.Login != "" {
				author := pull.User.Login
				event.Author = &author
			}
			if pull.User.person() {
				key := strings.ToLower(pull.User.Login)
				if !slices.Contains(r.people, key) {
					r.people = append(r.people, key)
				}
			}
			r.mergedPRs = append(r.mergedPRs, event)
		}
		if older {
			return nil
		}
		address = next
	}
	return nil
}

// fillReleases takes the window's published releases from the newest page.
func (c *collector) fillReleases(ctx context.Context, r *repo, since time.Time) error {
	var releases []struct {
		Name        string      `json:"name"`
		TagName     string      `json:"tag_name"`
		HTMLURL     string      `json:"html_url"`
		Draft       bool        `json:"draft"`
		Author      *githubUser `json:"author"`
		PublishedAt *time.Time  `json:"published_at"`
	}
	if _, err := c.gh.get(ctx, c.repoPath(r)+"/releases?per_page="+strconv.Itoa(20), &releases); err != nil {
		return err
	}
	for _, release := range releases {
		if release.Draft || release.PublishedAt == nil || release.PublishedAt.Before(since) {
			continue
		}
		title := strings.TrimSpace(release.Name)
		if title == "" {
			title = release.TagName
		}
		event := Event{Kind: KindRelease, Repository: r.name, Title: title, URL: release.HTMLURL, At: release.PublishedAt.UTC()}
		if release.Author != nil && release.Author.Login != "" {
			author := release.Author.Login
			event.Author = &author
		}
		r.releases = append(r.releases, event)
	}
	return nil
}

func nonEmpty(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}
