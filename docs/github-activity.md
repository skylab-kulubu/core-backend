# GitHub activity

**`GET /v1/dashboard/github-activity` is the club's GitHub organisation
activity for the admin dashboard's "Kulüp geliştirmesi" section.** Core reads
GitHub as a read-only GitHub App installed on the organisation and keeps the
answer ten minutes. Private repositories only count in the totals.

The response is core-frontend's `GithubActivity`
(`src/lib/dashboard/github-activity.ts`) field by field, with three
differences: `stale` (always there), `truncated` (on the answer and on a
repository, only when true) and `totals.privateRepositories`, which is left
out while fewer than two private repositories are active.

## Who may read it

A person in a privileged Group (`ADMIN`, `YK` or `DK`, the same check as the
other club-wide admin data: `authz.TypeGithubActivity`). Everyone else gets
403, a request without a bearer 401, and a product's service account 403 even
in such a Group.

Why not every signed-in person: the figures are internal, and the private
repositories' totals (how many are active, how many commits) are not public.
Team leaders see the dashboard but get 403 here; the dashboard hides the
section on any failed answer.

## Answers

| Status | When |
|---|---|
| 200 | The activity. `Cache-Control: private, max-age=60`. |
| 200, `"stale": true` | GitHub could not be read; this is the last good answer and `generatedAt` says how old it is. Served until it is twelve hours old; after that 503. |
| 401 / 403 | No bearer / not a privileged person. |
| 404 | The settings are unset: the route is not served. |
| 503 `github_activity_unavailable` | The settings are set but wrong (the startup line says which), or GitHub could not be read and nothing was read before. `Retry-After: 60`, `no-store`. |

## What it counts

- **Repositories:** every repository the installation sees, from GitHub's
  GraphQL API. Archived repositories and forks are left out of everything.
  A repository that is not public (private or internal) counts in the totals
  only: its name, address, description, language, pull request titles,
  releases and the people who worked on it never appear. The list is read by
  name (a push between two pages would reorder a list by push date); a
  repository met twice counts once, and as private if either copy says so.
- **Private split:** `privateRepositories` (how many private repositories were
  active, their commits) only when at least two were: one alone would be its
  own figures. The totals include them either way.
- **Window:** `GITHUB_ACTIVITY_WINDOW_DAYS` whole days (default 30) in Turkish
  time (UTC+3), today included; `window.since` is the first day's midnight.
  `commitsPrevious` is the same number of days before it.
- **Commits:** on each repository's default branch, by committer date (the
  date GitHub's `since`/`until` filter on). Only repositories pushed to since
  the previous window began are read.
- **Merged pull requests:** merged in the window, into any branch.
- **Open pull requests:** open now, in every covered repository.
- **Active contributors:** distinct GitHub accounts with a commit or a merged
  pull request in the window, private repositories included (as a count).
  Bots (`[bot]` accounts) are left out; their commits still count.
- **Repositories list:** public repositories with a commit, merged pull
  request or release in the window, most commits first, at most 20. Each
  lists at most five contributors (login, avatar, commits).
- **Events:** merged pull requests and published releases of public
  repositories in the window, newest first, at most 20.

Contributor logins and avatars are public GitHub data of public repositories.
The dashboard shows them as avatars; the per-person commit count is only used
for the order.

## How it reads GitHub

1. A JWT (RS256, nine minutes) signed with the app's private key.
2. The installation on the organisation: `GITHUB_ACTIVITY_INSTALLATION_ID`,
   or `GET /orgs/{org}/installation` once when it is unset.
3. An installation access token asked for with only `metadata`, `contents`
   and `pull_requests` read, whatever the app holds. It is kept until five
   minutes before it expires (GitHub's last an hour) and dropped when GitHub
   refuses it.
4. One GraphQL query per 100 repositories (with each one's open pull request
   count), then for each recently pushed repository its commits, closed pull
   requests (newest first, until one older than the window) and, for public
   ones, its releases. `GITHUB_ACTIVITY_WORKERS` repositories (4) are read at
   once; every list follows its `Link: rel="next"` pages, never to another
   host, up to `GITHUB_ACTIVITY_MAX_PAGES` pages (10, that is 1,000 commits
   or pull requests). A longer list stops there and marks the repository and
   the answer `truncated`: its counts are a lower bound, and the rest of the
   read goes on. A panic while reading one repository fails the read, not
   the process.

Each request gives up after 15 seconds and a whole read after
`GITHUB_ACTIVITY_REFRESH_TIMEOUT` (45s). One read runs at a time whoever
asks. An answer is fresh for ten minutes; after that the cached answer is
still served at once and GitHub is read in the background, so only the first
caller after a start (or after the answer passed twelve hours) waits. A first
caller who stops waiting gets 503 while the read goes on for the next. After a
failed read GitHub is left alone for a minute. When GitHub's budget is nearly
spent (`X-RateLimit-Remaining` 20 or under) or it answers 403/429 for its rate
limit, core asks nothing until `Retry-After` or `X-RateLimit-Reset`, an hour at
most. A read is about two GraphQL calls and three or four REST calls per
active repository, every ten minutes at most: far below the installation's
5,000 an hour.

Failures are logged as `github activity: refresh failed: …` with the path and
status, never a token. A repository in a path is named by the first eight hex
digits of the SHA-256 of `owner/name` (lower case), `repo-1a2b3c4d`, since it
may be private: `printf '%s' skylab-kulubu/site | shasum -a 256 | cut -c1-8`
finds it.

## Settings

| Variable | |
|---|---|
| `GITHUB_ACTIVITY_ORG` | The organisation's login, such as `skylab-kulubu`. |
| `GITHUB_ACTIVITY_APP_ID` | The GitHub App's ID (its settings page). Not a secret. |
| `GITHUB_ACTIVITY_INSTALLATION_ID` | Optional: the installation's ID (the number at the end of the installation's settings address). Unset, core asks GitHub. |
| `GITHUB_ACTIVITY_APP_PRIVATE_KEY` | The app's private key: an OpenBao reference (ADR-0049), `${{vault.bao-<side>.<core appName>/GITHUB_ACTIVITY_APP_PRIVATE_KEY:value}}`. The value is the downloaded `.pem` base64-encoded on one line (a plain PEM, or one with `\n` for its line breaks, also works). |
| `GITHUB_ACTIVITY_WINDOW_DAYS` | Optional, 1 to 90, default 30. |
| `GITHUB_ACTIVITY_WORKERS` | Optional, 1 to 16, default 4. |
| `GITHUB_ACTIVITY_MAX_PAGES` | Optional, 1 to 50, default 10. |
| `GITHUB_ACTIVITY_REFRESH_TIMEOUT` | Optional, a duration from `10s` to `5m`, default `45s`. |

The optional budget settings alone switch nothing on. All of the first four
unset: off, startup says `github activity: off (…)` and
the route is not served. Set: `github activity: on (org …, app …,
installation …, window … days)`. Set but wrong, or partly set: `github
activity: settings are wrong, … answers 503: <which variable>`; core still
starts. No line carries the key.

### The GitHub App

Organisation settings → Developer settings → GitHub Apps → New GitHub App:
no webhook; repository permissions **Metadata**, **Contents** and **Pull
requests**, all read-only; nothing else; installable only on this account.
Install it on all repositories. Generate one private key per side (sandbox,
production; an app holds several) so that a sandbox leak never reaches
production and each side's key is revoked on its own.
`ops/wizards/github-activity-app-wizard.sh` in the hub walks through it and
puts each key into its side's OpenBao path.

### Rollback

Remove the four `GITHUB_ACTIVITY_*` lines (and the comment above them) from
core's Dokploy environment and redeploy: the route is gone (404) and the
dashboard hides the section. The OpenBao entries can stay or be deleted; to
cut GitHub access, delete the keys (or the app) in GitHub.
