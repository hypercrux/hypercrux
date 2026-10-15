#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# The release workflow's plan (task R4): for one engine, 0.x or the Beta,
# whether a run of .github/workflows/release.yml releases a new version,
# builds an existing tag's files again, makes a dry run of the Beta's
# release, or does nothing, with the tag and the commit, and why. The
# workflow's two plan jobs run it, one for each engine, and its outputs
# start that engine's jobs.
#
#   python3 scripts/release-plan.py 0.x
#   python3 scripts/release-plan.py beta
#
# 0.x, as task A2 set it up:
#   - After the test workflow passes for a push to main in this repository
#     (the plan job's `if` checks that), on the commit main is at now:
#     hypercrux.go's Version, X.Y.Z, releases vX.Y.Z, unless that tag
#     exists. A version below the newest vX.Y.Z tag fails the plan. The
#     beta workflow's runs never release 0.x.
#   - A release published by hand, or a run by hand with a tag, builds that
#     tag's files, unless the tag is the Beta's.
# The Beta:
#   - After the test or the beta workflow passes for a push to main in this
#     repository, on the commit main is at now, and once the other of the
#     two has passed for a push of the same commit too:
#     beta/hypercrux/hypercrux.go's Version, X.Y.Z-beta.N, releases
#     vX.Y.Z-beta.N as a pre-release, unless that tag exists. 0.0.0-beta
#     means unreleased and releases nothing, and any other form fails the
#     plan. The version only goes up: above the newest tag of the form
#     vX.Y.Z-beta.N, with its X.Y.Z above the newest 0.x tag, vX.Y.Z, so Go
#     and GitHub order it after every 0.x release. Its X.Y.Z has to be above
#     0.x's Version in hypercrux.go at the same commit too, so that a push
#     that raises both can't release the Beta's ahead of a 0.x release that
#     Go orders after it; a 0.x Version that 0.x's plan refuses is left to
#     that plan. Either workflow's run can be the second to pass, a re-run
#     that turns one green among them.
#   - A release published by hand, or a run by hand with a tag, builds the
#     Beta's files when the tag has -beta. in it, and then it has to be of
#     the form vX.Y.Z-beta.N.
#   - A run by hand with dry_run set to a version, such as 0.3.0-beta.1, is
#     the dry run: the version follows the release's rules, and the Beta's
#     binaries are built and checked from the commit the run is on, with
#     Version set to it, and nothing is tagged or published.
#
# It reads the environment the workflow sets: EVENT (workflow_run, release
# or workflow_dispatch), RUN_WORKFLOW and RUN_SHA (the workflow that
# finished and its commit), RELEASE_TAG (a release published by hand),
# INPUT_TAG and INPUT_DRY_RUN (a run by hand's inputs), SHA (the commit the
# run is on), GITHUB_REPOSITORY and GITHUB_OUTPUT; and the engine's Version
# in the checkout, and for the Beta 0.x's too. It asks git for main's head
# and the tags, and gh for the other workflow's runs, only when it needs
# them.
#
# Its outputs, for the workflow: 0.x's tag, ref and new (true for a new
# release), as the 0.x jobs had them before; the Beta's tag, ref, version
# and mode (release, attach or dry-run). It prints what it decided and why,
# a notice under GitHub Actions, and exits with 0; or why the plan fails,
# an error under GitHub Actions, and exits with 1.
#
# scripts/release-plan-test.py runs its cases.
import json
import os
import re
import subprocess
import sys

ROOT = os.path.realpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
ZEROX, BETA = "0.x", "beta"
VERSION_FILE = {ZEROX: "hypercrux.go", BETA: "beta/hypercrux/hypercrux.go"}
WORKFLOW_FILE = {"test": "test.yml", "beta": "beta.yml"}
UNRELEASED = "0.0.0-beta"

NUM = r"(0|[1-9][0-9]*)"
ZEROX_FORM = re.compile(rf"^{NUM}\.{NUM}\.{NUM}$")
BETA_FORM = re.compile(rf"^{NUM}\.{NUM}\.{NUM}-beta\.([1-9][0-9]*)$")
VERSION_LINE = re.compile(r'^const Version = "(.*)"$', re.M)


class Refusal(Exception):
    """The plan fails, for the reason given."""


class Plan:
    def __init__(self, action, why, tag="", ref="", version=""):
        self.action = action  # release, attach, dry-run or none
        self.why = why
        self.tag = tag
        self.ref = ref
        self.version = version

    def outputs(self, engine):
        if self.action == "none":
            return []
        if engine == ZEROX:
            out = [("tag", self.tag), ("ref", self.ref)]
            if self.action == "release":
                out.append(("new", "true"))
            return out
        return [("tag", self.tag), ("ref", self.ref), ("version", self.version), ("mode", self.action)]


class Event:
    """What the plan decides from. main_head, tags and runs are functions,
    so that nothing is asked of git or GitHub that the plan doesn't need."""

    def __init__(self, event, run_workflow="", run_sha="", release_tag="", input_tag="", dry_run="", sha="",
                 repo="", version=None, zerox_version=None, main_head=None, tags=None, runs=None):
        self.event = event
        self.run_workflow = run_workflow
        self.run_sha = run_sha
        self.release_tag = release_tag
        self.input_tag = input_tag.strip()
        self.dry_run = dry_run.strip()
        self.sha = sha
        self.repo = repo
        self.version = version  # the engine's Version, or None when its file has none
        self.zerox_version = zerox_version  # for the Beta's plan, 0.x's Version at the same commit
        self.main_head = main_head
        self.tags = tags
        self.runs = runs  # runs(workflow) gives that workflow's runs on run_sha


def short(sha):
    return sha[:7] if sha else "(none)"


def beta_parts(version):
    m = BETA_FORM.match(version)
    return tuple(int(g) for g in m.groups()) if m else None


def zerox_parts(version):
    m = ZEROX_FORM.match(version)
    return tuple(int(g) for g in m.groups()) if m else None


def newest(tags, parts):
    found = [p for p in (parts(t[1:]) for t in tags if t.startswith("v")) if p]
    return max(found) if found else None


def dotted(parts):
    return ".".join(str(p) for p in parts[:3]) + (f"-beta.{parts[3]}" if len(parts) == 4 else "")


def check_tag_text(tag, what):
    if not tag or re.search(r"\s", tag):
        raise Refusal(f"{what} {tag!r} isn't a tag")


def check_beta_version(version, what):
    if not beta_parts(version):
        raise Refusal(f"{what}, {version!r}, is neither {UNRELEASED}, which means unreleased, nor of the form "
                      "X.Y.Z-beta.N, such as 0.3.0-beta.1, with numbers that don't start with 0 and N from 1")


def check_beta_order(version, tags, zerox_version):
    """The Beta's versions only go up, and after every 0.x release, the one
    that 0.x's Version at the same commit releases included."""
    v = beta_parts(version)
    top = newest(tags, beta_parts)
    if top and v <= top:
        raise Refusal(f"v{version} isn't above the newest release of the Beta, v{dotted(top)}; "
                      "raise the version above it")
    top = newest(tags, zerox_parts)
    if top and v[:3] <= top:
        raise Refusal(f"v{version}'s X.Y.Z isn't above v{dotted(top)}, the newest release of 0.x, so Go and "
                      "GitHub would order it before that release; raise X.Y.Z above it")
    zerox = zerox_parts(zerox_version or "")
    if zerox and v[:3] <= zerox:
        raise Refusal(f"v{version}'s X.Y.Z isn't above {zerox_version}, 0.x's Version in hypercrux.go at the same "
                      "commit, so once 0.x's release of it is out, Go and GitHub would order the Beta's before it; "
                      "raise X.Y.Z above it")


def passed(runs, sha, repo):
    """Whether a workflow passed for a push of sha to main in repo: its
    latest such run, which a re-run makes again, finished with success."""
    mine = [r for r in runs if r.get("head_sha") == sha and r.get("event") == "push"
            and r.get("head_branch") == "main"
            and (r.get("head_repository") or {}).get("full_name") == repo]
    if not mine:
        return False, f"has no run yet for the push of {short(sha)} to main"
    last = max(mine, key=lambda r: r.get("id", 0))
    if last.get("status") != "completed":
        return False, f"hasn't finished on {short(sha)}: its run {last.get('id')} is {last.get('status')}"
    if last.get("conclusion") != "success":
        return False, f"didn't pass on {short(sha)}: its run {last.get('id')} ended with {last.get('conclusion')}"
    return True, f"passed on {short(sha)} in run {last.get('id')}"


def decide(engine, ev):
    if ev.event == "workflow_run":
        return after_workflow(engine, ev)
    if ev.event == "release":
        return by_tag(engine, ev.release_tag, "a release published by hand")
    if ev.event == "workflow_dispatch":
        if ev.input_tag and ev.dry_run:
            raise Refusal("a run by hand takes a tag to build again or a dry_run version, and this one has both")
        if ev.input_tag:
            return by_tag(engine, ev.input_tag, "a run by hand")
        if ev.dry_run:
            return dry_run(engine, ev)
        raise Refusal("a run by hand needs a tag to build again, such as v0.2.0 or v0.3.0-beta.1, "
                      "or the Beta's version for a dry run, such as dry_run 0.3.0-beta.1")
    return Plan("none", f"nothing to do after a {ev.event or 'missing'} event")


def by_tag(engine, tag, how):
    check_tag_text(tag, "the tag")
    beta_tag = "-beta." in tag
    if engine == ZEROX:
        if beta_tag:
            return Plan("none", f"{tag} is the Beta's tag, which the Beta's plan builds")
        return Plan("attach", f"{how}: building 0.x's files for {tag}", tag=tag, ref=tag)
    if not beta_tag:
        return Plan("none", f"{tag} is 0.x's tag, which 0.x's plan builds")
    if not (tag.startswith("v") and beta_parts(tag[1:])):
        raise Refusal(f"{tag} has -beta. in it, so it's the Beta's, but it isn't of the form vX.Y.Z-beta.N")
    return Plan("attach", f"{how}: building the Beta's files for {tag}", tag=tag, ref=tag, version=tag[1:])


def after_workflow(engine, ev):
    finished = ev.run_workflow
    if finished not in WORKFLOW_FILE:
        return Plan("none", f"the workflow that finished, {finished!r}, is one no release waits for")
    if engine == ZEROX and finished != "test":
        return Plan("none", f"0.x's releases wait for the test workflow alone, and this run follows {finished}")
    head = ev.main_head()
    if head != ev.run_sha:
        return Plan("none", f"main has moved on from {short(ev.run_sha)} to {short(head)}, "
                            "so the run for the newer commit decides")
    if engine == ZEROX:
        if ev.version is None or not zerox_parts(ev.version):
            raise Refusal("hypercrux.go has no Version of the form X.Y.Z")
        tags = ev.tags()
        tag = "v" + ev.version
        if tag in tags:
            return Plan("none", f"{tag} is out already")
        top = newest(tags, zerox_parts)
        if top and zerox_parts(ev.version) < top:
            raise Refusal(f"{tag} is older than the newest release, v{dotted(top)}; raise the version above it")
        return Plan("release", f"releasing {tag} from {short(ev.run_sha)}, which the test workflow passed",
                    tag=tag, ref=ev.run_sha, version=ev.version)
    if ev.version is None:
        raise Refusal(f'{VERSION_FILE[BETA]} has no line const Version = "..."')
    if ev.version == UNRELEASED:
        return Plan("none", f"the Beta's Version is {UNRELEASED}, which means unreleased")
    check_beta_version(ev.version, f"{VERSION_FILE[BETA]}'s Version")
    tags = ev.tags()
    tag = "v" + ev.version
    if tag in tags:
        return Plan("none", f"{tag} is out already")
    check_beta_order(ev.version, tags, ev.zerox_version)
    other = "beta" if finished == "test" else "test"
    ok, why = passed(ev.runs(other), ev.run_sha, ev.repo)
    if not ok:
        return Plan("none", f"{tag} waits for the {other} workflow, which {why}; its run decides")
    return Plan("release", f"releasing {tag} from {short(ev.run_sha)} as a pre-release: the {finished} workflow "
                           f"passed on it, and the {other} workflow {why}",
                tag=tag, ref=ev.run_sha, version=ev.version)


def dry_run(engine, ev):
    if engine == ZEROX:
        return Plan("none", "a dry run is the Beta's")
    check_tag_text(ev.dry_run, "the dry run's version")
    if not ev.sha:
        raise Refusal("the run's commit isn't known, so there's nothing to build")
    version = ev.dry_run[1:] if ev.dry_run.startswith("v") else ev.dry_run
    if version == UNRELEASED:
        raise Refusal(f"{UNRELEASED} means unreleased; give the version to rehearse, such as 0.3.0-beta.1")
    check_beta_version(version, "the dry run's version")
    if ev.version is None:
        raise Refusal(f'{VERSION_FILE[BETA]} has no line const Version = "...", which the dry run sets')
    tags = ev.tags()
    tag = "v" + version
    if tag in tags:
        raise Refusal(f"{tag} is out already, so there's no release of it to rehearse")
    check_beta_order(version, tags, ev.zerox_version)
    how = "as it is" if ev.version == version else f"with Version set to {version} in place of {ev.version}"
    return Plan("dry-run", f"a dry run of {tag} from {short(ev.sha)}, {how}: it builds and checks the Beta's "
                           "binaries and writes the checksums and the notes, and tags and publishes nothing",
                tag=tag, ref=ev.sha, version=version)


# What follows reads what the plan needs, and writes what it decided.


def git(*args):
    try:
        r = subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True)
    except OSError as e:
        raise Refusal(f"git {' '.join(args)} failed: {e}")
    if r.returncode:
        raise Refusal(f"git {' '.join(args)} failed: {(r.stderr or r.stdout).strip()}")
    return r.stdout


def main_head():
    out = git("ls-remote", "origin", "refs/heads/main").split()
    if not out:
        raise Refusal("the repository has no branch main")
    return out[0]


def tags():
    names = []
    for line in git("ls-remote", "--tags", "--refs", "origin").splitlines():
        parts = line.split("\t")
        if len(parts) == 2 and parts[1].startswith("refs/tags/"):
            names.append(parts[1][len("refs/tags/"):])
    return names


def runs_of(repo, sha):
    def runs(workflow):
        path = f"repos/{repo}/actions/workflows/{WORKFLOW_FILE[workflow]}/runs?head_sha={sha}&event=push&per_page=100"
        try:
            r = subprocess.run(["gh", "api", path], capture_output=True, text=True)
        except OSError as e:
            raise Refusal(f"couldn't read the {workflow} workflow's runs: {e}")
        if r.returncode:
            raise Refusal(f"couldn't read the {workflow} workflow's runs: {(r.stderr or r.stdout).strip()}")
        try:
            return json.loads(r.stdout)["workflow_runs"]
        except (ValueError, KeyError) as e:
            raise Refusal(f"couldn't read the {workflow} workflow's runs: {e}")
    return runs


def read_version(engine):
    path = os.path.join(ROOT, VERSION_FILE[engine])
    try:
        with open(path, encoding="utf-8") as f:
            found = VERSION_LINE.findall(f.read())
    except OSError:
        return None
    return found[0] if len(found) == 1 else None


def annotate(level, title, message):
    if os.environ.get("GITHUB_ACTIONS") == "true":
        message = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::{level} title={title}::{message}", flush=True)


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in (ZEROX, BETA):
        sys.exit("usage: python3 scripts/release-plan.py 0.x|beta")
    engine = sys.argv[1]
    env = os.environ
    name = "0.x" if engine == ZEROX else "the Beta"
    title = "0.x release plan" if engine == ZEROX else "Beta release plan"
    repo = env.get("GITHUB_REPOSITORY", "")
    ev = Event(env.get("EVENT", ""), run_workflow=env.get("RUN_WORKFLOW", ""), run_sha=env.get("RUN_SHA", ""),
               release_tag=env.get("RELEASE_TAG", ""), input_tag=env.get("INPUT_TAG", ""),
               dry_run=env.get("INPUT_DRY_RUN", ""), sha=env.get("SHA", ""), repo=repo,
               version=read_version(engine), zerox_version=read_version(ZEROX) if engine == BETA else None,
               main_head=main_head, tags=tags,
               runs=runs_of(repo, env.get("RUN_SHA", "")))
    try:
        plan = decide(engine, ev)
    except Refusal as e:
        print(f"{name}: the plan fails: {e}", file=sys.stderr)
        annotate("error", title, f"{name}: the plan fails: {e}")
        sys.exit(1)
    word = {"release": "release", "attach": "build", "dry-run": "dry run", "none": "no release"}[plan.action]
    print(f"{name}: {word}: {plan.why}")
    annotate("notice", title, f"{name}: {word}: {plan.why}")
    out = env.get("GITHUB_OUTPUT")
    if out:
        with open(out, "a", encoding="utf-8") as f:
            for k, v in plan.outputs(engine):
                f.write(f"{k}={v}\n")
    else:
        for k, v in plan.outputs(engine):
            print(f"  {k}={v}")


if __name__ == "__main__":
    main()
