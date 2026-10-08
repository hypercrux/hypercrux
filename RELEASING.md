# Releasing HyperCrux

Releases go out on their own. Nobody clicks Publish. This file is the record of
how that works, for whoever (person or Claude session) works on this repo next.

## The short version

1. Raise `const Version = "X.Y.Z"` in `hypercrux.go`.
2. Write what changed into the commit message. The release notes list the files; the site's posts carry the story.
3. Push the commit to `main`.

That's it. When the `test` workflow passes on that commit, the `release`
workflow builds the binaries, smoke-tests every one, then tags the commit
`vX.Y.Z` and publishes the release "HyperCrux vX.Y.Z" with the archives and
SHA256SUMS attached. If any build or check fails, nothing is tagged and nothing
goes public. Fix it, push again, and the next green run picks it up.

Pushing without raising the version is fine. The release workflow sees that the
version already has a tag and stops quietly.

## What it does, step by step

The `release` workflow ([.github/workflows/release.yml](.github/workflows/release.yml))
is started by a finished `test` run, not by the push itself.

- **plan.** Goes on only if the `test` run passed, came from a push to `main`
  in this repo, and `main` still points at that commit (if `main` moved on, the
  run for the newer commit decides). Reads the version from `hypercrux.go`.
  Stops quietly if `vX.Y.Z` is already a tag. Fails loudly if the version is
  lower than the newest tag.
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
  publish twice.

Notes on the release come from the table in the workflow's "Checksums and notes" step.

## Files that go out

| System | File |
|---|---|
| Linux, x86-64 (static) | hypercrux_linux_amd64.tar.gz |
| Linux, ARM64 (static) | hypercrux_linux_arm64.tar.gz |
| macOS, Apple silicon | hypercrux_darwin_arm64.tar.gz |
| macOS, Intel | hypercrux_darwin_amd64.tar.gz |
| Windows, x86-64 | hypercrux_windows_amd64.zip |
| Checksums | SHA256SUMS |

Archive names carry no version on purpose. Links like
`https://github.com/hypercrux/hypercrux/releases/latest/download/hypercrux_linux_amd64.tar.gz`
keep working from one release to the next, so the site's download page never
needs editing for a release.

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

If the release run failed: read its log
(`gh api repos/hypercrux/hypercrux/actions/runs/<id>/jobs`), fix the cause on `main`,
and push. Don't change the version unless the fix needs it; the tag was never
made, so the same version goes out on the next green run.

## Other ways in

- **A release published by hand** on GitHub still works. The workflow builds
  the binaries for that tag and attaches them.
- **Rebuilding an existing tag:** run the `release` workflow by hand
  (Actions, release, Run workflow) with the tag, such as `v0.1.0`. It rebuilds
  and replaces the files on that release.
- Releases made by the workflow don't start the workflow again, because
  events from `GITHUB_TOKEN` don't trigger other runs. No loop.

## Rules that don't change

- The owner doesn't publish releases, interim or final. Never ask him to. If
  something blocks a release, fix it in the repo.
- Every release is a full release ("HyperCrux vX.Y.Z"), not a pre-release.
- Versions only go up. The plan step refuses a version below the newest tag.
- The CI workflow must keep the name `test`, because `release.yml` listens for
  `workflows: [test]`. Renaming one means renaming the other.
- `workflow_run` only fires for workflow files on the default branch, so
  changes to either workflow take effect once they're on `main`.
- The Beta engine in `beta/` carries its own version (`0.0.0-beta`) and isn't
  released by this workflow. Only the version in `hypercrux.go` counts.

## Bringing this to another project

`release.yml` here is the template. Copy it to the new repo with `test.yml`,
then change:

- the binary name and `./cmd/<name>` path in the build steps,
- the file the plan step reads the version from (the `sed` line), and that
  file's `const Version = "X.Y.Z"`,
- the smoke test the builds run (`scripts/smoke.sh` here) and the Windows check,
- the release title and the file table in the notes,
- the archive list in `gh release create` and `gh release upload`.

Then push to `main`, watch the first run, and check the release as above.
