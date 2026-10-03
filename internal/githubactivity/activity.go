// Package githubactivity gathers the club's GitHub organisation activity for
// the admin dashboard's "Kulüp geliştirmesi" section (docs/github-activity.md).
//
// Core reads GitHub as a read-only GitHub App installed on the organisation:
// it signs a short JWT with the app's private key, trades it for an
// installation access token and asks GitHub's GraphQL and REST APIs. The
// answer is kept ten minutes; when GitHub cannot be reached, the last good
// answer is served marked stale.
//
// Private repositories only ever count in the totals: their names, addresses,
// descriptions, pull request titles and releases never leave this package.
package githubactivity

import "time"

// Activity is the response of GET /v1/dashboard/github-activity. Its shape is
// core-frontend's GithubActivity (src/lib/dashboard/github-activity.ts),
// field by field, plus Stale.
type Activity struct {
	// Org is the organisation's login, such as "skylab-kulubu".
	Org string `json:"org"`
	// GeneratedAt is when the figures were gathered.
	GeneratedAt time.Time `json:"generatedAt"`
	Window      Window    `json:"window"`
	Totals      Totals    `json:"totals"`
	// Repositories are the public repositories with activity in the window,
	// most active first.
	Repositories []Repository `json:"repositories"`
	// Events are the window's merged pull requests and releases of public
	// repositories, newest first.
	Events []Event `json:"events"`
	// Stale is true when GitHub could not be read and this is the last good
	// answer; GeneratedAt says how old it is.
	Stale bool `json:"stale"`
}

// Window is the period the figures cover: Days whole days in Turkish time,
// starting at Since (midnight), today included.
type Window struct {
	Since time.Time `json:"since"`
	Days  int       `json:"days"`
}

// Totals count every repository the figures cover, private ones included.
type Totals struct {
	// Commits are commits on default branches in the window.
	Commits int `json:"commits"`
	// CommitsPrevious are the same for the window before.
	CommitsPrevious    int `json:"commitsPrevious"`
	MergedPullRequests int `json:"mergedPullRequests"`
	// OpenPullRequests are open now, in every covered repository.
	OpenPullRequests int `json:"openPullRequests"`
	// ActiveContributors are the people (bots excluded) with a commit or a
	// merged pull request in the window.
	ActiveContributors  int           `json:"activeContributors"`
	PrivateRepositories PrivateTotals `json:"privateRepositories"`
}

// PrivateTotals are the private repositories with activity in the window and
// their commits. Nothing names them.
type PrivateTotals struct {
	Active  int `json:"active"`
	Commits int `json:"commits"`
}

// Repository is a public repository with activity in the window.
type Repository struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Description *string   `json:"description"`
	Language    *string   `json:"language"`
	PushedAt    time.Time `json:"pushedAt"`
	Commits     int       `json:"commits"`
	// CommitsByDay are the window's commits per day, oldest first; Days long.
	CommitsByDay     []int         `json:"commitsByDay"`
	OpenPullRequests int           `json:"openPullRequests"`
	Contributors     []Contributor `json:"contributors"`
}

// Contributor is a person behind a public repository's commits in the window.
// Logins and avatars of public repositories are public GitHub data.
type Contributor struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
	Commits   int    `json:"commits"`
}

// Event kinds.
const (
	KindPullRequestMerged = "pull_request_merged"
	KindRelease           = "release"
)

// Event is a merged pull request or a release of a public repository.
type Event struct {
	Kind       string    `json:"kind"`
	Repository string    `json:"repository"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	Author     *string   `json:"author"`
	At         time.Time `json:"at"`
}

// stale is a copy of a marked stale.
func (a Activity) stale() Activity {
	a.Stale = true
	return a
}
