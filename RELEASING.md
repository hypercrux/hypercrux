# Releasing HyperCrux

Releases go out on their own. Nobody clicks Publish. This file is the record of
how that works, for whoever (person or Claude session) works on this repo next.

One workflow releases both engines: 0.x, at the repository's root, and the
Beta, the engine in `beta/` that [BETA.md](BETA.md) plans. Each has its own
version, files and tags, and neither waits for the other.

## The short version

1. Raise the version: `const Version = "X.Y.Z"` in `hypercrux.go` for 0.x,
   or `const Version = "X.Y.Z-beta.N"` in `beta/hypercrux/hypercrux.go` for
   the Beta.
2. Write what changed into the commit message. The release notes list the files; the site's posts carry the story.
3. Push the commit to `main`.

That's it. For 0.x, when the `test` workflow passes on that commit, the
`release` workflow builds the binaries, smoke-tests every one, then tags the
commit `vX.Y.Z` and publishes the release "HyperCrux vX.Y.Z" with the
archives and SHA256SUMS attached. For the Beta it waits until the `test` and
`beta` workflows have both passed on the commit, builds and checks its two
Linux binaries, then tags the commit `vX.Y.Z-beta.N` and publishes "HyperCrux
vX.Y.Z-beta.N" as a pre-release. If any build or check fails, nothing is
tagged and nothing goes public. Fix it, push again, and the next green run
picks it up.

Pushing without raising a version is fine. The release workflow sees that
the version already has a tag, or for the Beta that it's `0.0.0-beta`, and
stops quietly.

## What it does, step by step

The `release` workflow ([.github/workflows/release.yml](.github/workflows/release.yml))
starts when a `test` or `beta` run finishes, and the push alone doesn't
start it. Two plan jobs decide, one for each engine, both with
[scripts/release-plan.py](scripts/release-plan.py), whose opening comment
has every rule and whose cases are
[scripts/release-plan-test.py](scripts/release-plan-test.py). A late or
repeated `test` or `beta` run of a commit from before that script came in
releases nothing, as `main` has moved on from that commit: each plan says
so in a notice, and the run stays green.

0.x's jobs are as they were before the Beta's release:

- **plan.** Goes on only if the `test` run passed, came from a push to `main`
  in this repo, and `main` still points at that commit (if `main` moved on, the
  run for the newer commit decides). A `beta` run never releases 0.x. Reads
  the version from `hypercrux.go`. Stops quietly if `vX.Y.Z` is already a
  tag. Fails loudly if the version is lower than the newest `vX.Y.Z` tag.
- **linux, macos, windows.** Builds the binaries from that exact commit. Linux
  builds are static (musl), on x86-64 and ARM runners. Each Linux binary and
  the Apple silicon one run `scripts/smoke.sh`. The Windows binary is built on
  Linux with mingw.
- **windows-check.** Runs the Windows binary on a Windows runner: put, link,
  export and import, nearest, walk, delete and check.
- **release.** Runs only after every build and check passed. Creates the tag
  and the release in one step with `gh release create --target <commit>`,
  on the repo's own `GITHUB_TOKEN` (`contents: write` on this job only). A
  `concurrency` group per tag means two runs for the same version can't
  publish twice. It takes 0.x's archives alone, since the Beta's can be in
  the same run.

The Beta's jobs (task R4):

- **beta-plan.** Goes on after a passing `test` or `beta` run for a push to
  `main` in this repo, on the commit `main` points at now, once the other of
  the two workflows has passed for a push of the same commit too, so it reads
  that workflow's runs (`actions: read` on this job only). Whichever finishes
  second releases. A run that failed and then passes when it's run again
  releases as a first run would. Reads the version from
  `beta/hypercrux/hypercrux.go`: `0.0.0-beta` means unreleased and releases
  nothing, `X.Y.Z-beta.N` releases `vX.Y.Z-beta.N` unless that tag exists, and
  any other form fails the plan, saying why. So does a version that isn't
  above the newest `vX.Y.Z-beta.N` tag, or whose X.Y.Z isn't above the newest
  0.x tag, `vX.Y.Z`, since Go and GitHub have to order it after every 0.x
  release, or isn't above 0.x's `Version` in `hypercrux.go` at the same
  commit, so a push that raises both can't release the Beta's ahead of a 0.x
  release that Go orders after it.
- **beta-prerelease.** Runs only for a release published by hand, or one
  built again by hand. Straight after the plan, whatever the builds then do,
  it makes that release a pre-release if it isn't one, with `gh release edit
  --prerelease --latest=false` (`contents: write`, as `beta-release` has),
  and says which in a notice. A full release of the Beta could stay the
  latest, and the site's links to the latest release would lose 0.x's files.
- **beta-linux.** Builds `./beta/cmd/hypercrux` with
  `CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`, linux/amd64 on
  `ubuntu-latest` and linux/arm64 on `ubuntu-24.04-arm`, and checks each
  binary on the machine that built it with
  [scripts/beta-release.sh](scripts/beta-release.sh): `file` has to say it's
  statically linked and built for that architecture, `hypercrux version` has
  to name the version being released, and `scripts/smoke.sh` has to pass.
  Then it packages the archive. Each result is a notice.
- **beta-release.** Runs only after both builds and checks passed. Writes
  SHA256SUMS and the notes with `scripts/beta-release.sh`, then creates the
  tag and the release in one step with `gh release create --target <commit>
  --prerelease --latest=false`, with `contents: write`, which only it and
  `beta-prerelease` have among the Beta's jobs, and a `concurrency` group per
  tag, as 0.x's has. For a release published by hand or built again, it
  attaches the files to that release, and writes the notes if the release
  has none.
- **beta-dry-run.** A dry run's last job, in place of beta-release: below.

0.x's notes come from the table in its release job's "Checksums and notes"
step, and the Beta's from `notes` in `scripts/beta-release.sh`.

## Files that go out

0.x's, in every 0.x release:

| System | File |
|---|---|
| Linux, x86-64 (static) | hypercrux_linux_amd64.tar.gz |
| Linux, ARM64 (static) | hypercrux_linux_arm64.tar.gz |
| macOS, Apple silicon | hypercrux_darwin_arm64.tar.gz |
| macOS, Intel | hypercrux_darwin_amd64.tar.gz |
| Windows, x86-64 | hypercrux_windows_amd64.zip |
| Checksums | SHA256SUMS |

The Beta's, in every release of the Beta:

| System | File |
|---|---|
| Linux, x86-64 (static) | hypercrux-beta_linux_amd64.tar.gz |
| Linux, ARM64 (static) | hypercrux-beta_linux_arm64.tar.gz |
| Checksums | SHA256SUMS |

Each of the Beta's archives holds the `hypercrux` binary, LICENSE, NOTICE,
THIRD_PARTY_LICENSES.md and `beta/README.md` as README.md. The Beta's binary
has no SQLite and no go-sqlite3 in it, only the Go runtime and standard
library, so its NOTICE and THIRD_PARTY_LICENSES.md are the root's with those
two left out, made by the script, which refuses a binary that holds any
other module.

Archive names carry no version on purpose. Links like
`https://github.com/hypercrux/hypercrux/releases/latest/download/hypercrux_linux_amd64.tar.gz`
keep working from one release to the next, so the site's download page never
needs editing for a release.

## The Beta's releases are pre-releases

A pre-release never becomes the repository's latest release, so the latest
stays 0.x's, and the site's links to `releases/latest/download/...` keep
giving 0.x's files, which the Beta's archives don't replace. Go versions the
whole module by its tags, so the Beta's tag is a version of the module, and
`go get github.com/hypercrux/hypercrux` keeps a program on 0.x, since `@latest`
leaves out pre-release versions while a release exists. A program gets the
Beta by naming its version:
`go get github.com/hypercrux/hypercrux/beta/hypercrux@v0.3.0-beta.1`.

The Beta's first release is 0.3.0-beta.1, and later ones go up as
0.3.0-beta.2 and on. BETA.md's "Decisions" has the reasons for 0.3.

## The dry run

A dry run builds and checks the Beta's binaries, writes SHA256SUMS and the
notes as a release would, keeps them as the run's artifacts, and tags and
publishes nothing. It's a run by hand with `dry_run` set to the version to
rehearse. From a cloud session, through the REST API:

```sh
gh api -X POST repos/hypercrux/hypercrux/actions/workflows/release.yml/dispatches \
  -f ref=main -f 'inputs[dry_run]=0.3.0-beta.1'
```

Or from the Actions tab: release, Run workflow, with the version under
`dry_run`. The version keeps a release's rules: of the form X.Y.Z-beta.N, no
tag yet, above the newest tags. Each build sets `Version` to it in its own
checkout before building, unless the code says that already, so the binaries,
the notes, the title and the tag are the release's own, from the branch's
commit as it is otherwise. The dry run's last job has the workflow's
`contents: read` alone, so it can't tag or publish anything.

A cloud session can read neither a job's log nor its artifacts, so every
result is an annotation. Reading them:

```sh
# the run
gh api "repos/hypercrux/hypercrux/actions/workflows/release.yml/runs?event=workflow_dispatch&per_page=1" \
  --jq '.workflow_runs[0] | "\(.id) \(.status) \(.conclusion) \(.head_sha[0:7])"'
# its jobs, with their ids
gh api "repos/hypercrux/hypercrux/actions/runs/<run id>/jobs" \
  --jq '.jobs[] | "\(.name)\t\(.conclusion)\t\(.id)"'
# a job's notices
gh api repos/hypercrux/hypercrux/check-runs/<job id>/annotations \
  --jq '.[] | "\(.annotation_level) \(.title): \(.message)"'
```

The dry run passes when the jobs that run are green: `plan`, `beta-plan`,
both `beta-linux` jobs and `beta-dry-run`. 0.x's jobs, `beta-prerelease` and
`beta-release` are skipped. `plan` says 0.x has nothing to do, and
`beta-plan` that it's a dry run of the tag from the commit. Each
`beta-linux` job says the version it set, its binary's `file` line, which has
to say "statically linked" and x86-64 or ARM aarch64, its `hypercrux version`
line, which names the version, that `scripts/smoke.sh` passed, and what its
archive holds. `beta-dry-run` gives SHA256SUMS, the title and the tag the
release would have, and the notes. A check that fails is an error annotation
saying what failed.

## After pushing: checking it went out

Claude's cloud sessions can't create tags or releases themselves, and the
`gh` GraphQL calls (`gh release list`, `gh run list`) are blocked there. The
REST API works:

```sh
# the test run, then the release run, for the commit just pushed
gh api "repos/hypercrux/hypercrux/actions/workflows/test.yml/runs?per_page=1" \
  --jq '.workflow_runs[0] | "\(.status) \(.conclusion) \(.head_sha[0:7])"'
gh api "repos/hypercrux/hypercrux/actions/workflows/release.yml/runs?per_page=1" \
  --jq '.workflow_runs[0] | "\(.status) \(.conclusion) \(.head_sha[0:7])"'

# the release and its files
gh api repos/hypercrux/hypercrux/releases/latest \
  --jq '.tag_name, .name, (.assets[].name)'
```

Then download one archive through the `latest/download` link, check it
against SHA256SUMS, unpack it and run `hypercrux version`. It should print the new
version. Only then is the release done.

For the Beta, a push starts two release runs, one when `test` finishes and
one when `beta` does. The run after the later of the two releases, and the
other's `beta-plan` notice says it waits for the other workflow. Then:

```sh
# the release: a pre-release, with the two archives and SHA256SUMS
gh api repos/hypercrux/hypercrux/releases/tags/v0.3.0-beta.1 \
  --jq '.tag_name, .name, .prerelease, (.assets[].name)'
# the latest release, still 0.x's
gh api repos/hypercrux/hypercrux/releases/latest --jq .tag_name
```

Then download `https://github.com/hypercrux/hypercrux/releases/download/v0.3.0-beta.1/hypercrux-beta_linux_amd64.tar.gz`
and SHA256SUMS beside it, check the archive with `sha256sum -c
--ignore-missing SHA256SUMS`, unpack it, and run `hypercrux version`, which
should print `hypercrux 0.3.0-beta.1, file format 1`, and
`sh scripts/smoke.sh ./hypercrux`. The latest release's links should still
give 0.x's files. Only then is the release done.

If the release run failed: read its log
(`gh api repos/hypercrux/hypercrux/actions/runs/<id>/jobs`), fix the cause on `main`,
and push. Don't change the version unless the fix needs it; the tag was never
made, so the same version goes out on the next green run. A run that failed
for a reason outside the repository, such as a call to GitHub's API, can run
again whole, with
`gh api -X POST repos/hypercrux/hypercrux/actions/runs/<id>/rerun`. That runs
the plans again, which check again that `main` is still at the commit and
that the other workflow passed. Running the failed jobs alone again
(`rerun-failed-jobs`) keeps the plans' first answer.

## Other ways in

- **A release published by hand** on GitHub still works. The workflow builds
  the binaries for that tag and attaches them: the Beta's for a tag with
  `-beta.` in it, which has to be `vX.Y.Z-beta.N`, and 0.x's for any other.
  A release of the Beta published by hand as a full release becomes a
  pre-release straight after the plan, by `beta-prerelease`, whatever its
  builds then do, so it keeps the latest release from 0.x only until that
  job has run.
- **Rebuilding an existing tag:** run the `release` workflow by hand
  (Actions, release, Run workflow) with the tag, such as `v0.1.0` or
  `v0.3.0-beta.1`. It rebuilds that tag's engine and replaces the files on
  that release. The Beta's builds run the tag's own scripts, and
  `beta-prerelease` makes sure a Beta release is a pre-release.
- **A dry run of the Beta's release,** above.
- Releases made by the workflow don't start the workflow again, because
  events from `GITHUB_TOKEN` don't trigger other runs. No loop.

## Rules that don't change

- The owner doesn't publish releases, interim or final. Never ask him to. If
  something blocks a release, fix it in the repo.
- Every release of 0.x is a full release, "HyperCrux vX.Y.Z". Every
  release of the Beta is a pre-release, "HyperCrux vX.Y.Z-beta.N", so the
  latest release stays 0.x's.
- Versions only go up. The plan refuses a version below the newest tag of its
  engine, and a Beta version whose X.Y.Z isn't above the newest 0.x tag, or
  above 0.x's `Version` at the same commit.
- The CI workflows keep the names `test` and `beta`, because `release.yml`
  listens for `workflows: [test, beta]`. Renaming one means renaming it there.
- `workflow_run` only fires for workflow files on the default branch, so
  changes to either workflow take effect once they're on `main`.

## Bringing this to another project

`release.yml` here is the template. Copy it to the new repo with `test.yml`,
`scripts/release-plan.py` and its test, then change:

- the binary name and `./cmd/<name>` path in the build steps,
- the file the plan reads the version from (`VERSION_FILE` in
  `scripts/release-plan.py`), and that file's `const Version = "X.Y.Z"`,
- the smoke test the builds run (`scripts/smoke.sh` here) and the Windows check,
- the release title and the file table in the notes,
- the archive list in `gh release create` and `gh release upload`.

A project with one engine leaves out the Beta's jobs and its workflow name in
`workflows:`. Then push to `main`, watch the first run, and check the release
as above.
