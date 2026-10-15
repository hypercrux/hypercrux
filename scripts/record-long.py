#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Writes a long run's results to test/results/beta-long.txt (task R4), from
# GitHub's REST API: the run's commit and times, and for each job its
# architecture, result and times, with the notices it wrote, which say what
# it ran and at what length. The long workflow, .github/workflows/long.yml,
# runs on GitHub's machines, and a cloud session can fetch neither a job's
# log nor its artifacts, so the notices are the record (R1.md).
#
#   python3 scripts/record-long.py RUN_ID                # writes test/results/beta-long.txt
#   python3 scripts/record-long.py RUN_ID RUN_ID ...     # a run, then the runs of failed jobs again
#   python3 scripts/record-long.py RUN_ID -o FILE        # writes FILE instead
#
# It needs gh, signed in, and refuses a run that hasn't finished. Of a job's
# annotations it keeps the ones the scripts wrote, which have a title, and
# every warning and error, and leaves out GitHub's own notices about its
# machines. A job whose run step was skipped, as the run's `only` input
# leaves jobs out, wrote no notice, and the file says so.
import datetime
import json
import os
import subprocess
import sys

ROOT = os.path.realpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
REPO = "hypercrux/hypercrux"
JOBS = ["fuzz-1", "fuzz-2", "crash", "procs", "diff", "planted"]  # long.yml's order
ARCHES = ["amd64", "arm64"]
LABELS = {"ubuntu-latest": "amd64", "ubuntu-24.04-arm": "arm64"}


def api(path):
    r = subprocess.run(["gh", "api", path], capture_output=True, text=True)
    if r.returncode:
        sys.exit(f"gh api {path} failed: {(r.stderr or r.stdout).strip()}")
    return json.loads(r.stdout)


def when(stamp):
    return datetime.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ") if stamp else None


def took(start, end):
    if not (start and end):
        return "?"
    s = int((end - start).total_seconds())
    h, m, s = s // 3600, s % 3600 // 60, s % 60
    if h:
        return f"{h} h {m} min {s} s"
    if m:
        return f"{m} min {s} s"
    return f"{s} s"


def arch_of(job):
    name = job.get("name", "")
    for a in ARCHES:
        if name.endswith(" on " + a):
            return a
    for label in job.get("labels") or []:
        if label in LABELS:
            return LABELS[label]
    return "?"


def order(job):
    name = job.get("name", "")
    kind = name.split(" on ")[0]
    return (JOBS.index(kind) if kind in JOBS else len(JOBS), ARCHES.index(arch_of(job)) if arch_of(job) in ARCHES
            else len(ARCHES), name)


def skipped_run_step(job):
    kind = job.get("name", "").split(" on ")[0]
    for step in job.get("steps") or []:
        if step.get("name") == "Run " + kind:
            return step.get("conclusion") == "skipped"
    return False


def section(run_id):
    run = api(f"repos/{REPO}/actions/runs/{run_id}")
    if run.get("status") != "completed":
        sys.exit(f"run {run_id} is {run.get('status')}; record it once it has finished")
    jobs = api(f"repos/{REPO}/actions/runs/{run_id}/jobs?per_page=100&filter=latest")["jobs"]
    jobs.sort(key=order)
    started = when(run.get("run_started_at") or run.get("created_at"))
    ends = [when(j.get("completed_at")) for j in jobs if j.get("completed_at")]
    ended = max(ends) if ends else when(run.get("updated_at"))
    passed = sum(1 for j in jobs if j.get("conclusion") == "success")
    lines = [
        f"Run {run_id} of the {run.get('name')} workflow, {run.get('html_url')}",
        f"  commit {run.get('head_sha')}, on {run.get('head_branch')}",
        f"  started by {run.get('event')}, attempt {run.get('run_attempt')}",
        f"  started {started:%Y-%m-%d %H:%M:%S} UTC, ended {ended:%Y-%m-%d %H:%M:%S} UTC, after {took(started, ended)}",
        f"  result: {run.get('conclusion')}, {len(jobs)} jobs, {passed} passed",
        "",
    ]
    rows = [("Job", "Architecture", "Result", "Started", "Ended", "Took")]
    for j in jobs:
        s, e = when(j.get("started_at")), when(j.get("completed_at"))
        rows.append((j.get("name", "?"), arch_of(j), j.get("conclusion") or j.get("status") or "?",
                     f"{s:%H:%M:%S}" if s else "?", f"{e:%H:%M:%S}" if e else "?", took(s, e)))
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    for r in rows:
        lines.append("  " + "  ".join(c.ljust(w) for c, w in zip(r, widths)).rstrip())
    lines.append("")
    for j in jobs:
        s, e = when(j.get("started_at")), when(j.get("completed_at"))
        labels = ", ".join(j.get("labels") or []) or "?"
        lines.append(f"{j.get('name')}, on {labels}: {j.get('conclusion') or j.get('status')}, "
                     f"{s:%Y-%m-%d %H:%M:%S} to {e:%H:%M:%S} UTC, {took(s, e)}" if s and e else
                     f"{j.get('name')}, on {labels}: {j.get('conclusion') or j.get('status')}")
        notes = api(f"repos/{REPO}/check-runs/{j['id']}/annotations?per_page=100")
        kept = [a for a in notes if a.get("title") or a.get("annotation_level") in ("warning", "failure")]
        for a in kept:
            level = a.get("annotation_level")
            level = "error" if level == "failure" else level
            lines.append(f"  {level}, {a.get('title') or '(no title)'}:")
            for line in (a.get("message") or "").splitlines():
                lines.append("    " + line.rstrip())
        if not kept:
            if skipped_run_step(j):
                lines.append("  its run step was skipped, as the run's `only` input leaves a job out, so it wrote no notice")
            else:
                lines.append("  it wrote no notice")
        lines.append("")
    return run, lines


def main():
    args = sys.argv[1:]
    out = os.path.join(ROOT, "test", "results", "beta-long.txt")
    if "-o" in args:
        i = args.index("-o")
        if i + 1 >= len(args):
            sys.exit("-o needs the file to write")
        out = args[i + 1]
        del args[i:i + 2]
    if not args or any(not a.isdigit() for a in args):
        sys.exit("usage: python3 scripts/record-long.py RUN_ID [RUN_ID...] [-o FILE]")
    body, commits = [], []
    for run_id in args:
        run, lines = section(run_id)
        commits.append(run.get("head_sha", "?")[:7])
        body += lines
    head = [
        "# The long workflow's runs (task R1): each job on amd64 and arm64, with its result and times and the "
        "notices it wrote, which say what it ran and at what length. Read from GitHub's REST API",
        f"# recorded {datetime.datetime.now(datetime.timezone.utc):%Y-%m-%d %H:%M} UTC from "
        f"{'runs' if len(args) > 1 else 'run'} {', '.join(args)}, at {'commits' if len(args) > 1 else 'commit'} "
        f"{', '.join(commits)}",
        "# GitHub's runners: ubuntu-latest for amd64 and ubuntu-24.04-arm for arm64",
        f"# command: python3 scripts/record-long.py {' '.join(args)}",
        "",
    ]
    with open(out, "w", encoding="utf-8") as f:
        f.write("\n".join(head + body).rstrip("\n") + "\n")
    print(f"results written to {out}")


if __name__ == "__main__":
    main()
