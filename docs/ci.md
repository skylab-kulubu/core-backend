# CI and the release line

Two workflows: **Checks** (`.github/workflows/checks.yml`) tests, **GHCR**
(`.github/workflows/ghcr.yml`) builds the images and deploys.

## Checks

| Run | Size | What runs |
|---|---|---|
| Pull request into `main` | fast | `lint` unless only Markdown changed; the tests of the packages the change can affect |
| Pull request into `main` labelled `full-ci` | full | everything |
| Push to `main`, nightly 02:17 UTC, `gh workflow run checks.yml --ref <branch>` | full | everything |
| Pull request into `production`, push to `production` | full, or promoted | nothing again when a full run already tested the tree, else everything |

- **`scope (full|fast)`** decides and plans. Affected packages
  (`go run ./scripts/ci/testplan affected`): a changed file marks its
  package and the packages above it, then every package whose test binary
  imports one of them (`go list -test -deps`). `go.mod`, `go.sum` and any
  file outside every package except Markdown (the workflows, the Dockerfiles,
  `deploy/`) select every package. The packages whose tests read files above
  their directory (`internal/account` and `internal/retention` read `docs/`
  and every migration) run on any change.
- **`lint`**: `gofmt`, `go vet`, the two binaries the Dockerfiles build.
- **`test (N)`**: the shards. `testplan plan` splits the packages by the
  last runs' timings (the `test-timings-*` Actions cache; without it, a
  second a test); a package longer than a shard's share is split by its
  test names into parts (`go test -run '^(A|B)$'`), and its last part runs
  every test the other parts do not (`-skip`), so a new test still runs.
  `testplan run` runs four units at a time, each `go test -race -count=1`,
  prints the output of what failed and fails on any skipped test (a test that
  cannot reach Docker or ffmpeg skips itself; none skips on purpose).
- The shards' ffmpeg and ffprobe (for `internal/mediaframe` and
  `internal/faststart`) are the static builds in `FFMPEG_IMAGE`, pinned by
  digest and copied out of it while Go builds; 6.1, as the frame service's
  Alpine ffmpeg. Not apt: `apt-get update` hung often enough to fail shards.
- Each shard runs one PostgreSQL 18 server; with `CORE_TEST_POSTGRES_URL` set,
  `testpostgres.Start` gives each test a database of its own on it (created,
  then dropped with its connections), instead of a container of its own.
  Locally, without the variable, every test still gets its own container.
- **`gate`** is the one check to require. It fails when a job fails or is
  cancelled, and in a full run that tested when any job was skipped.
- Pushes, the schedule and manual runs are never cancelled; a newer push to
  a pull request cancels its older run.
- Caches (Actions): the module cache by `go.sum` and the build cache (with
  the `-race` test builds) are saved by `lint` on `main`'s pushes and the
  nightly run, and every job restores them. The test cache is not used
  (`-count=1`): the database and Docker tests depend on what Go does not see.

## Promotion and the release line

A tree is **tested** when a push, schedule or manual run of Checks whose
`scope (full)` and every other job passed tested exactly that tree
(`scripts/ci/tested-tree.sh`; pull request runs test GitHub's merge commit,
so they do not count). `production` is a squash of `main`, so the tree is
compared, not the commit.

1. Merge into `main`: Checks runs full; GHCR builds `:latest`, `:sandbox`,
   `:<sha>` and `:tree-<tree>` and deploys sandbox.
2. Release PR into `production` (the same tree): `scope` finds `main`'s
   green full run and promotes; `gate` passes without testing again.
3. Push to `production`: GHCR's `tested` job waits until the tree is tested,
   then `image` copies `main`'s `:tree-<tree>` image to `:production` and
   `:<sha>` (`docker buildx imagetools create`: same digest, no build) and
   calls the Dokploy production hook as before.

A hotfix (a tree `main` never had): the PR into `production` tests in full;
the push to `production` runs Checks in full again (a push run is the
evidence) and GHCR waits for it, then builds. If that run fails: fix, or
rerun Checks, then rerun GHCR. A failed or cancelled `main` run of the
released tree means the release PR tests in full, and so does the push.
