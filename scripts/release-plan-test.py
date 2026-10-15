#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# The cases of the release workflow's plan, scripts/release-plan.py (task
# R4), for 0.x and for the Beta: a new version, one out already, one below
# the newest tag, a malformed one, 0.0.0-beta, a commit main has moved on
# from, the other workflow not passed yet, a push that raises both
# versions, the paths by hand and the dry run. The last cases run the
# script itself, as the workflow does, with git and gh made up on PATH, and
# read what it writes to GITHUB_OUTPUT.
#
#   python3 scripts/release-plan-test.py      # add -v to name each case
import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True  # no __pycache__ in scripts/
HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "release-plan.py")
spec = importlib.util.spec_from_file_location("release_plan", SCRIPT)
plan = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plan)

REPO = "hypercrux/hypercrux"
SHA = "9096d53" + "1" * 33
NEWER = "45f01a5" + "2" * 33
TAGS = ["v0.1.0", "v0.2.0"]


def run(id, status="completed", conclusion="success", sha=SHA, event="push", branch="main", repo=REPO):
    return {"id": id, "status": status, "conclusion": conclusion, "head_sha": sha, "event": event,
            "head_branch": branch, "head_repository": {"full_name": repo}}


class Asked:
    """The event, with a note of what the plan asked git and GitHub."""

    def __init__(self, event, head=SHA, tags=TAGS, runs=None, **kw):
        self.asked = []
        runs = runs or {}

        def main_head():
            self.asked.append("main")
            return head

        def tag_list():
            self.asked.append("tags")
            return list(tags)

        def runs_of(workflow):
            self.asked.append("runs of " + workflow)
            return runs.get(workflow, [])

        kw.setdefault("repo", REPO)
        self.ev = plan.Event(event, main_head=main_head, tags=tag_list, runs=runs_of, **kw)


def after(workflow, version, **kw):
    return Asked("workflow_run", run_workflow=workflow, run_sha=SHA, version=version, **kw)


class Case(unittest.TestCase):
    def decides(self, engine, asked, action, tag="", ref="", version="", why=""):
        p = plan.decide(engine, asked.ev)
        self.assertEqual((p.action, p.tag, p.ref, p.version), (action, tag, ref, version), p.why)
        self.assertIn(why, p.why)
        return p

    def refuses(self, engine, asked, why):
        with self.assertRaises(plan.Refusal) as e:
            plan.decide(engine, asked.ev)
        self.assertIn(why, str(e.exception))


class ZeroxAfterTheTests(Case):
    def test_a_new_version_releases(self):
        a = after("test", "0.2.1", tags=TAGS + ["v0.3.0-beta.1"])
        p = self.decides("0.x", a, "release", "v0.2.1", SHA, "0.2.1", "releasing v0.2.1 from 9096d53")
        self.assertEqual(p.outputs("0.x"), [("tag", "v0.2.1"), ("ref", SHA), ("new", "true")])
        self.assertEqual(a.asked, ["main", "tags"])  # never the beta workflow

    def test_one_out_already_releases_nothing(self):
        self.decides("0.x", after("test", "0.2.0"), "none", why="v0.2.0 is out already")

    def test_one_below_the_newest_tag_fails(self):
        self.refuses("0.x", after("test", "0.1.5"),
                     "v0.1.5 is older than the newest release, v0.2.0; raise the version above it")

    def test_a_beta_tag_above_it_doesnt_count(self):
        self.decides("0.x", after("test", "0.2.1", tags=TAGS + ["v0.9.0-beta.1"]), "release", "v0.2.1", SHA, "0.2.1")

    def test_versions_compare_as_numbers(self):
        self.decides("0.x", after("test", "0.10.0", tags=["v0.9.0"]), "release", "v0.10.0", SHA, "0.10.0")
        self.refuses("0.x", after("test", "0.9.1", tags=["v0.10.0"]), "older than the newest release, v0.10.0")

    def test_a_malformed_one_fails(self):
        for v in ("0.2", "0.2.0.1", "v0.2.1", "0.02.1", "0.2.1-rc1", "0.0.0-beta", ""):
            with self.subTest(version=v):
                self.refuses("0.x", after("test", v), "hypercrux.go has no Version of the form X.Y.Z")
        self.refuses("0.x", after("test", None), "hypercrux.go has no Version of the form X.Y.Z")

    def test_a_commit_main_has_moved_on_from_releases_nothing(self):
        a = after("test", "0.2.1", head=NEWER)
        self.decides("0.x", a, "none", why="main has moved on from 9096d53 to 45f01a5")
        self.assertEqual(a.asked, ["main"])

    def test_the_beta_workflow_never_releases_it(self):
        a = after("beta", "0.2.1")
        self.decides("0.x", a, "none", why="0.x's releases wait for the test workflow alone")
        self.assertEqual(a.asked, [])

    def test_it_doesnt_wait_for_the_beta_workflow(self):
        a = after("test", "0.2.1", runs={"beta": [run(7, status="in_progress", conclusion=None)]})
        self.decides("0.x", a, "release", "v0.2.1", SHA, "0.2.1")
        self.assertNotIn("runs of beta", a.asked)


class ZeroxByHand(Case):
    def test_a_release_published_by_hand_gets_its_files(self):
        a = Asked("release", release_tag="v0.2.0", version="0.2.0")
        p = self.decides("0.x", a, "attach", "v0.2.0", "v0.2.0")
        self.assertEqual(p.outputs("0.x"), [("tag", "v0.2.0"), ("ref", "v0.2.0")])
        self.assertEqual(a.asked, [])

    def test_a_run_by_hand_builds_a_tag_again(self):
        self.decides("0.x", Asked("workflow_dispatch", input_tag="v0.1.0"), "attach", "v0.1.0", "v0.1.0")

    def test_the_betas_tags_are_left_to_the_betas_plan(self):
        self.decides("0.x", Asked("release", release_tag="v0.3.0-beta.1"), "none", why="the Beta's tag")
        self.decides("0.x", Asked("workflow_dispatch", input_tag="v0.3.0-beta.1"), "none", why="the Beta's tag")

    def test_a_dry_run_is_the_betas(self):
        self.decides("0.x", Asked("workflow_dispatch", dry_run="0.3.0-beta.1", sha=SHA), "none",
                     why="a dry run is the Beta's")


class BetaAfterTheTests(Case):
    passed_beta = {"beta": [run(41)]}
    passed_test = {"test": [run(40)]}

    def test_a_new_version_releases_once_both_have_passed(self):
        a = after("test", "0.3.0-beta.1", runs=self.passed_beta)
        p = self.decides("beta", a, "release", "v0.3.0-beta.1", SHA, "0.3.0-beta.1",
                         "the test workflow passed on it, and the beta workflow passed on 9096d53 in run 41")
        self.assertEqual(p.outputs("beta"), [("tag", "v0.3.0-beta.1"), ("ref", SHA), ("version", "0.3.0-beta.1"),
                                             ("mode", "release")])
        self.assertEqual(a.asked, ["main", "tags", "runs of beta"])

    def test_either_workflow_can_be_the_second_to_pass(self):
        a = after("beta", "0.3.0-beta.1", runs=self.passed_test)
        self.decides("beta", a, "release", "v0.3.0-beta.1", SHA, "0.3.0-beta.1", "the beta workflow passed on it")
        self.assertEqual(a.asked, ["main", "tags", "runs of test"])

    def test_one_out_already_releases_nothing(self):
        a = after("test", "0.3.0-beta.1", tags=TAGS + ["v0.3.0-beta.1"], runs=self.passed_beta)
        self.decides("beta", a, "none", why="v0.3.0-beta.1 is out already")
        self.assertNotIn("runs of beta", a.asked)

    def test_the_next_one_releases(self):
        a = after("test", "0.3.0-beta.2", tags=TAGS + ["v0.3.0-beta.1"], runs=self.passed_beta)
        self.decides("beta", a, "release", "v0.3.0-beta.2", SHA, "0.3.0-beta.2")
        a = after("test", "0.3.0-beta.10", tags=TAGS + ["v0.3.0-beta.9"], runs=self.passed_beta)
        self.decides("beta", a, "release", "v0.3.0-beta.10", SHA, "0.3.0-beta.10")

    def test_one_below_the_newest_tag_fails(self):
        a = after("test", "0.3.0-beta.1", tags=TAGS + ["v0.3.0-beta.2"], runs=self.passed_beta)
        self.refuses("beta", a, "v0.3.0-beta.1 isn't above the newest release of the Beta, v0.3.0-beta.2")
        a = after("test", "0.3.0-beta.3", tags=TAGS + ["v0.4.0-beta.1"], runs=self.passed_beta)
        self.refuses("beta", a, "isn't above the newest release of the Beta, v0.4.0-beta.1")

    def test_one_not_after_every_0x_release_fails(self):
        for v in ("0.2.0-beta.1", "0.1.9-beta.4"):
            with self.subTest(version=v):
                self.refuses("beta", after("test", v, runs=self.passed_beta),
                             "X.Y.Z isn't above v0.2.0, the newest release of 0.x")
        a = after("test", "0.3.0-beta.1", tags=TAGS + ["v0.3.0"], runs=self.passed_beta)
        self.refuses("beta", a, "X.Y.Z isn't above v0.3.0, the newest release of 0.x")

    def test_one_not_above_0xs_version_at_the_same_commit_fails(self):
        # A push that raises both: 0.x's 0.3.0 goes out with it, and Go
        # orders 0.3.0-beta.2 below it, whichever workflow finishes first.
        for zerox in ("0.3.0", "0.4.0"):
            with self.subTest(zerox=zerox):
                a = after("beta", "0.3.0-beta.2", zerox_version=zerox, tags=TAGS + ["v0.3.0-beta.1"],
                          runs=self.passed_test)
                self.refuses("beta", a, f"v0.3.0-beta.2's X.Y.Z isn't above {zerox}, 0.x's Version in "
                             "hypercrux.go at the same commit")
        # Below it, out already, or one that 0.x's plan refuses: no bar.
        for zerox in ("0.2.1", "0.2.0", "0.3", None):
            with self.subTest(zerox=zerox):
                a = after("beta", "0.3.0-beta.2", zerox_version=zerox, tags=TAGS + ["v0.3.0-beta.1"],
                          runs=self.passed_test)
                self.decides("beta", a, "release", "v0.3.0-beta.2", SHA, "0.3.0-beta.2")

    def test_a_malformed_one_fails(self):
        for v in ("0.3.0-beta", "0.3.0-beta.0", "0.3.0-beta.01", "0.3-beta.1", "v0.3.0-beta.1", "0.3.0-rc.1",
                  "0.3.0", "0.03.0-beta.1", "0.3.0-beta.1 ", ""):
            with self.subTest(version=v):
                self.refuses("beta", after("test", v, runs=self.passed_beta), "is neither 0.0.0-beta")
        self.refuses("beta", after("test", None), 'has no line const Version = "..."')

    def test_0_0_0_beta_releases_nothing(self):
        for workflow in ("test", "beta"):
            a = after(workflow, "0.0.0-beta", runs=self.passed_beta)
            self.decides("beta", a, "none", why="0.0.0-beta, which means unreleased")
            self.assertEqual(a.asked, ["main"])

    def test_a_commit_main_has_moved_on_from_releases_nothing(self):
        a = after("beta", "0.3.0-beta.1", head=NEWER, runs=self.passed_test)
        self.decides("beta", a, "none", why="main has moved on from 9096d53 to 45f01a5")
        self.assertEqual(a.asked, ["main"])

    def test_the_other_workflow_not_passed_yet_releases_nothing(self):
        cases = {
            "no run yet": ([], "has no run yet for the push of 9096d53 to main"),
            "still running": ([run(41, status="in_progress", conclusion=None)],
                              "hasn't finished on 9096d53: its run 41 is in_progress"),
            "queued": ([run(41, status="queued", conclusion=None)], "its run 41 is queued"),
            "failed": ([run(41, conclusion="failure")], "didn't pass on 9096d53: its run 41 ended with failure"),
            "cancelled": ([run(41, conclusion="cancelled")], "ended with cancelled"),
            "another commit's": ([run(41, sha=NEWER)], "has no run yet"),
            "a pull request's": ([run(41, event="pull_request", branch="beta-r4")], "has no run yet"),
            "a push to another branch": ([run(41, branch="beta-r4")], "has no run yet"),
            "an event other than a push": ([run(41, event="workflow_dispatch")], "has no run yet"),
            "another repository's": ([run(41, repo="someone/hypercrux")], "has no run yet"),
            "a newer run still going": ([run(41), run(52, status="in_progress", conclusion=None)],
                                        "its run 52 is in_progress"),
        }
        for name, (runs, why) in cases.items():
            with self.subTest(name):
                a = after("test", "0.3.0-beta.1", runs={"beta": runs})
                p = self.decides("beta", a, "none", why="v0.3.0-beta.1 waits for the beta workflow, which ")
                self.assertIn(why, p.why)
                self.assertTrue(p.why.endswith("; its run decides"), p.why)

    def test_a_rerun_that_turns_green_releases(self):
        # A run that failed and then passed when it ran again keeps its id;
        # a newer run after a failed one counts by itself.
        a = after("beta", "0.3.0-beta.1", runs={"test": [run(40)]})
        self.decides("beta", a, "release", "v0.3.0-beta.1", SHA, "0.3.0-beta.1")
        a = after("test", "0.3.0-beta.1", runs={"beta": [run(41, conclusion="failure"), run(57)]})
        self.decides("beta", a, "release", "v0.3.0-beta.1", SHA, "0.3.0-beta.1", "passed on 9096d53 in run 57")


class BetaByHand(Case):
    def test_a_release_published_by_hand_gets_the_betas_files(self):
        a = Asked("release", release_tag="v0.3.0-beta.1")
        p = self.decides("beta", a, "attach", "v0.3.0-beta.1", "v0.3.0-beta.1", "0.3.0-beta.1")
        self.assertEqual(p.outputs("beta")[-1], ("mode", "attach"))
        self.assertEqual(a.asked, [])

    def test_a_run_by_hand_builds_a_tag_again(self):
        self.decides("beta", Asked("workflow_dispatch", input_tag="v0.3.0-beta.1"), "attach", "v0.3.0-beta.1",
                     "v0.3.0-beta.1", "0.3.0-beta.1")

    def test_0xs_tags_are_left_to_0xs_plan(self):
        self.decides("beta", Asked("release", release_tag="v0.2.0"), "none", why="0.x's tag")
        self.decides("beta", Asked("workflow_dispatch", input_tag="v0.1.0"), "none", why="0.x's tag")

    def test_a_malformed_beta_tag_fails(self):
        for tag in ("v0.3.0-beta.x", "0.3.0-beta.1", "v0.3-beta.1"):
            with self.subTest(tag=tag):
                self.refuses("beta", Asked("release", release_tag=tag), "isn't of the form vX.Y.Z-beta.N")


class DryRun(Case):
    def test_a_dry_run_rehearses_the_release(self):
        a = Asked("workflow_dispatch", dry_run="0.3.0-beta.1", sha=SHA, version="0.0.0-beta")
        p = self.decides("beta", a, "dry-run", "v0.3.0-beta.1", SHA, "0.3.0-beta.1",
                         "with Version set to 0.3.0-beta.1 in place of 0.0.0-beta")
        self.assertEqual(p.outputs("beta")[-1], ("mode", "dry-run"))
        self.assertEqual(a.asked, ["tags"])

    def test_a_dry_run_of_the_version_in_the_code_builds_it_as_it_is(self):
        a = Asked("workflow_dispatch", dry_run="v0.3.0-beta.1", sha=SHA, version="0.3.0-beta.1")
        self.decides("beta", a, "dry-run", "v0.3.0-beta.1", SHA, "0.3.0-beta.1", "as it is")

    def test_a_dry_run_keeps_the_releases_rules(self):
        def dry(v, tags=TAGS, zerox="0.2.0"):
            return Asked("workflow_dispatch", dry_run=v, sha=SHA, version="0.0.0-beta", zerox_version=zerox,
                         tags=tags)
        self.refuses("beta", dry("0.3.0-beta.1", TAGS + ["v0.3.0-beta.1"]), "v0.3.0-beta.1 is out already")
        self.refuses("beta", dry("0.3.0-beta.1", TAGS + ["v0.3.0-beta.2"]), "isn't above the newest release")
        self.refuses("beta", dry("0.2.0-beta.1"), "isn't above v0.2.0, the newest release of 0.x")
        self.refuses("beta", dry("0.3.0"), "the dry run's version, '0.3.0', is neither")
        self.refuses("beta", dry("0.0.0-beta"), "0.0.0-beta means unreleased")
        self.refuses("beta", dry("0.3.0-beta.1\nmode=release"), "isn't a tag")
        self.refuses("beta", dry("0.3.0-beta.1", zerox="0.3.0"), "isn't above 0.3.0, 0.x's Version in hypercrux.go")

    def test_a_run_by_hand_takes_one_of_the_two(self):
        both = Asked("workflow_dispatch", input_tag="v0.2.0", dry_run="0.3.0-beta.1", sha=SHA)
        neither = Asked("workflow_dispatch", sha=SHA)
        for engine in ("0.x", "beta"):
            self.refuses(engine, both, "and this one has both")
            self.refuses(engine, neither, "needs a tag to build again")


class BothPlans(Case):
    def test_a_workflow_no_release_waits_for_releases_nothing(self):
        for engine, version in (("0.x", "0.2.1"), ("beta", "0.3.0-beta.1")):
            a = after("nightly", version, runs={"beta": [run(41)], "test": [run(40)]})
            self.decides(engine, a, "none", why="the workflow that finished, 'nightly', is one no release waits for")
            self.assertEqual(a.asked, [])

    def test_an_empty_release_tag_fails(self):
        for engine in ("0.x", "beta"):
            self.refuses(engine, Asked("release", release_tag=""), "the tag '' isn't a tag")


FAKE_GIT = """#!/bin/sh
case "$*" in
"ls-remote origin refs/heads/main") printf '%s\\trefs/heads/main\\n' "$FAKE_MAIN" ;;
"ls-remote --tags --refs origin") for t in $FAKE_TAGS; do printf '%s\\trefs/tags/%s\\n' 0123abc "$t"; done ;;
*) echo "git $*: not faked" >&2; exit 2 ;;
esac
"""
FAKE_GH = """#!/bin/sh
echo "$*" >> "$FAKE_GH_CALLS"
cat "$FAKE_RUNS"
"""


class TheScript(unittest.TestCase):
    """The script as the workflow runs it."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp)
        root = os.path.join(self.tmp, "repo")
        os.makedirs(os.path.join(root, "scripts"))
        os.makedirs(os.path.join(root, "beta", "hypercrux"))
        shutil.copy(SCRIPT, os.path.join(root, "scripts"))
        self.script = os.path.join(root, "scripts", "release-plan.py")
        self.root = root
        self.versions("0.2.0", "0.0.0-beta")
        bin = os.path.join(self.tmp, "bin")
        os.makedirs(bin)
        for name, text in (("git", FAKE_GIT), ("gh", FAKE_GH)):
            with open(os.path.join(bin, name), "w") as f:
                f.write(text)
            os.chmod(os.path.join(bin, name), 0o755)
        self.out = os.path.join(self.tmp, "output")
        self.calls = os.path.join(self.tmp, "gh-calls")
        self.runs = os.path.join(self.tmp, "runs.json")
        with open(self.runs, "w") as f:
            f.write('{"total_count": 1, "workflow_runs": [{"id": 41, "status": "completed", "conclusion": "success", '
                    f'"head_sha": "{SHA}", "event": "push", "head_branch": "main", '
                    f'"head_repository": {{"full_name": "{REPO}"}}}}]}}')
        self.env = dict(os.environ, PATH=bin + os.pathsep + os.environ["PATH"], GITHUB_ACTIONS="true",
                        GITHUB_OUTPUT=self.out, GITHUB_REPOSITORY=REPO, FAKE_MAIN=SHA, FAKE_TAGS="v0.1.0 v0.2.0",
                        FAKE_RUNS=self.runs, FAKE_GH_CALLS=self.calls, SHA=SHA)

    def versions(self, zerox, beta):
        for path, v in (("hypercrux.go", zerox), ("beta/hypercrux/hypercrux.go", beta)):
            with open(os.path.join(self.root, path), "w") as f:
                f.write(f'package hypercrux\n\n// Version is the version.\nconst Version = "{v}"\n')

    def plan(self, engine, **env):
        r = subprocess.run([sys.executable, self.script, engine], env=dict(self.env, **env), capture_output=True,
                           text=True)
        out = ""
        if os.path.exists(self.out):
            with open(self.out) as f:
                out = f.read()
            os.remove(self.out)
        return r, out

    def test_0x_releases_after_the_tests(self):
        self.versions("0.2.1", "0.0.0-beta")
        r, out = self.plan("0.x", EVENT="workflow_run", RUN_WORKFLOW="test", RUN_SHA=SHA)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(out, f"tag=v0.2.1\nref={SHA}\nnew=true\n")
        self.assertIn("::notice title=0.x release plan::0.x: release: releasing v0.2.1 from 9096d53", r.stdout)
        self.assertFalse(os.path.exists(self.calls))

    def test_the_beta_releases_once_both_have_passed(self):
        self.versions("0.2.0", "0.3.0-beta.1")
        r, out = self.plan("beta", EVENT="workflow_run", RUN_WORKFLOW="test", RUN_SHA=SHA)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(out, f"tag=v0.3.0-beta.1\nref={SHA}\nversion=0.3.0-beta.1\nmode=release\n")
        with open(self.calls) as f:
            self.assertEqual(f.read(), f"api repos/{REPO}/actions/workflows/beta.yml/runs?head_sha={SHA}"
                                       "&event=push&per_page=100\n")

    def test_the_betas_0_0_0_beta_asks_nothing_of_github(self):
        r, out = self.plan("beta", EVENT="workflow_run", RUN_WORKFLOW="beta", RUN_SHA=SHA)
        self.assertEqual((r.returncode, out), (0, ""), r.stderr)
        self.assertIn("the Beta: no release: the Beta's Version is 0.0.0-beta", r.stdout)
        self.assertFalse(os.path.exists(self.calls))

    def test_a_dry_run(self):
        r, out = self.plan("beta", EVENT="workflow_dispatch", INPUT_DRY_RUN="0.3.0-beta.1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(out, f"tag=v0.3.0-beta.1\nref={SHA}\nversion=0.3.0-beta.1\nmode=dry-run\n")
        r, out = self.plan("0.x", EVENT="workflow_dispatch", INPUT_DRY_RUN="0.3.0-beta.1")
        self.assertEqual((r.returncode, out), (0, ""), r.stderr)

    def test_the_beta_waits_for_0xs_version_at_the_same_commit(self):
        self.versions("0.3.0", "0.3.0-beta.1")
        r, out = self.plan("beta", EVENT="workflow_run", RUN_WORKFLOW="test", RUN_SHA=SHA)
        self.assertEqual((r.returncode, out), (1, ""))
        self.assertIn("v0.3.0-beta.1's X.Y.Z isn't above 0.3.0, 0.x's Version in hypercrux.go at the same commit",
                      r.stdout)
        self.assertFalse(os.path.exists(self.calls))

    def test_a_plan_that_fails_says_why(self):
        self.versions("0.2.0", "0.3.0-rc.1")
        r, out = self.plan("beta", EVENT="workflow_run", RUN_WORKFLOW="test", RUN_SHA=SHA)
        self.assertEqual((r.returncode, out), (1, ""))
        self.assertIn("::error title=Beta release plan::the Beta: the plan fails: "
                      "beta/hypercrux/hypercrux.go's Version, '0.3.0-rc.1', is neither", r.stdout)

    def test_git_failing_fails_the_plan(self):
        self.versions("0.2.1", "0.0.0-beta")
        r, out = self.plan("0.x", EVENT="workflow_run", RUN_WORKFLOW="test", RUN_SHA=SHA, PATH="/nonexistent")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(out, "")

    def test_its_usage(self):
        r = subprocess.run([sys.executable, self.script], capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("usage: python3 scripts/release-plan.py 0.x|beta", r.stderr)


if __name__ == "__main__":
    unittest.main()
