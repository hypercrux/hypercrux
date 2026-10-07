#!/usr/bin/env python3
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Checks the Beta's task board against BETA.md's task table: the same tasks,
# the same hours, and every task after the tasks it needs.
#
#   python3 scripts/check-board.py
import os
import re
import sys

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
beta = open(os.path.join(root, "BETA.md")).read()
board = open(os.path.join(root, "beta", "tasks", "README.md")).read()

plan = {}
for line in beta.splitlines():
    m = re.match(r"^\| (V2|[A-Z]\d) \| (.*?) \| (.*?) \| (.*?) \| \(?(\d+)\)? \|$", line)
    if not m:
        continue
    tid, _, needs, _, hours = m.groups()
    deps = []
    if needs.strip() != "none":
        for d in re.split(r",\s*", needs):
            r = re.match(r"([A-Z])(\d) to [A-Z](\d)", d)
            if r:
                deps += [f"{r.group(1)}{i}" for i in range(int(r.group(2)), int(r.group(3)) + 1)]
            else:
                deps.append(d.strip())
    plan[tid] = (int(hours), deps)

rows = []
for line in board.splitlines():
    m = re.match(r"^\|\s*(\d*)\s*\|\s*(V2|[A-Z]\d) (.*?)\|\s*\(?(\d+)\)?\s*\|\s*(.*?)\s*\|$", line)
    if m:
        rows.append((m.group(2), int(m.group(4))))

problems = []
if set(plan) != {t for t, _ in rows}:
    problems.append("the tasks differ: " + ", ".join(sorted(set(plan) ^ {t for t, _ in rows})))
pos = {t: i for i, (t, _) in enumerate(rows)}
for t, hours in rows:
    if t not in plan:
        continue
    if plan[t][0] != hours:
        problems.append(f"{t} has {hours} hours on the board and {plan[t][0]} in BETA.md")
    for d in plan[t][1]:
        if d in pos and pos[d] > pos[t]:
            problems.append(f"{t} comes before {d}, which it needs")
for p in problems:
    print(p)
total = sum(h for t, h in rows if t != "V2")
print(f"{len(rows)} tasks, {total} hours without V2, {len(problems)} problems")
sys.exit(1 if problems else 0)
