# Task board

The Beta is built in rounds, in the order below. A round is two or three
tasks at once, each done by an agent of its own in its own git worktree,
and a coordinating session reviews and merges them. After each round the
work stops. The code, each task's closing test and record, and briefs for
the next round go into the repository, and the owner says whether to carry
on. That gives the owner a say over each step and over what the work costs.
The first seven tasks ran one at a time, before the owner approved more
agents.

The tasks and their closing tests are the table in [BETA.md](../../BETA.md),
under "The work, task by task". Each task gets a file here: a brief before it
starts, and a record of what was done when it's finished.

## How to pick up the work

1. The next round is the tasks marked "next" below: two or three whose
   needs are done, taken in the table's order.
2. Each task's agent reads the task's file here, its row in BETA.md's task
   table, and the files of the tasks it needs, for the decisions they made.
   It works in a worktree of its own, on a branch, until the closing test
   passes, with `go vet ./...`, the tests of what it touched, and
   `go test -short ./...`. It writes the task's record over the brief and
   commits on its branch. Shared files are the coordinator's: this board,
   BETA.md, CI, `scripts/` and other tasks' files.
3. The coordinator reviews each branch and merges it into main, runs
   `sh scripts/check-beta.sh`, `go vet ./...` and `go test -short ./...`,
   updates the shared files, and writes briefs for the next round. Then it
   pushes, checks CI, and stops to ask the owner.

## Order and status

The five early tasks come first, then the spec, then the path to the first
working version (G2), then the rest. Every task comes after the tasks it
needs. In this order the first working version arrives about 57 hours of
work in, and all 133 hours are done after R4.

| # | Task | Hours | Status |
|---|---|---|---|
| 1 | A1 Test suites from 0.x | 4 | done, [A1.md](A1.md) |
| 2 | A2 Export and import for 0.x | 4 | done, [A2.md](A2.md), released as 0.2.0 |
| 3 | A3 Differential harness | 3 | done, [A3.md](A3.md) |
| 4 | A4 SQL corpus | 3 | done, [A4.md](A4.md) |
| 5 | A5 The search loop | 2 | done, [A5.md](A5.md) |
| 6 | P1 The `beta/` folder, build and CI, the task board | 2 | done, [P1.md](P1.md) |
| 7 | P2 File format | 3 | done, [P2.md](P2.md) |
| 8 | P3 Internal interfaces | 2 | done, [P3.md](P3.md) |
| 9 | P4 Package skeleton | 2 | done, [P4.md](P4.md) |
| 10 | P5 SQL subset spec | 2 | done, [P5.md](P5.md), with P3 |
| 11 | S1 Records, fields and rules | 3 | done, [S1.md](S1.md), with P4 and F1 |
| 12 | F1 Codec | 2 | done, [F1.md](F1.md) |
| 13 | F2 Log and write lock | 3 | done, [F2.md](F2.md) |
| 14 | S2 Transactions | 3 | done, [S2.md](S2.md), with F2 and T1 |
| 15 | S3 Change lists | 2 | done, [S3.md](S3.md), with T2 and T3 |
| 16 | S4 Key order and `Scan` | 3 | done, [S4.md](S4.md), with G1 and F3 |
| 17 | S5 Links and walks | 4 | done, [S5.md](S5.md), with T4 and F4 |
| 18 | V1 Vector arrays and `Nearest` | 4 | done, [V1.md](V1.md), with T5 and F5 |
| 19 | T5 Benchmarks | 2 | done, [T5.md](T5.md), first recorded run in `test/results/beta-bench.txt` |
| 20 | G1 Slice 1: `Put` and `Get` | 2 | done, [G1.md](G1.md) |
| 21 | G2 Slice 2, the first working version | 2 | done, [G2.md](G2.md), with F6 and Q3 |
| 22 | T1 Fault layer for data | 3 | done, [T1.md](T1.md) |
| 23 | T2 Fault layer for names | 2 | done, [T2.md](T2.md) |
| 24 | T3 Crash-point driver | 3 | done, [T3.md](T3.md) |
| 25 | T4 Many-process harness | 3 | done, [T4.md](T4.md) |
| 26 | F3 Crash recovery | 4 | done, [F3.md](F3.md) |
| 27 | F4 Damage | 2 | done, [F4.md](F4.md) |
| 28 | F5 Failed commits | 2 | done, [F5.md](F5.md) |
| 29 | F6 Following other processes | 3 | done, [F6.md](F6.md) |
| 30 | F8 Compaction | 3 | done, [F8.md](F8.md), with Q1 and G3 |
| 31 | F9 Reloading | 2 | done, [F9.md](F9.md), with Q2 and Q4 |
| 32 | F7 File rules and growth | 2 | next, [F7.md](F7.md) |
| 33 | Q1 Values and functions | 4 | done, [Q1.md](Q1.md) |
| 34 | Q2 Dates | 2 | done, [Q2.md](Q2.md) |
| 35 | Q3 Parser | 5 | done, [Q3.md](Q3.md), ahead of Q1 and Q2 |
| 36 | Q4 Operators | 2 | done, [Q4.md](Q4.md) |
| 37 | Q5 Planner | 5 | next, [Q5.md](Q5.md) |
| 38 | Q6 SQL writes | 2 | |
| 39 | G3 The database/sql driver | 2 | done, [G3.md](G3.md) |
| 40 | G4 Slice 3: SQL | 2 | |
| 41 | G6 Import and export in the Beta | 2 | next, [G6.md](G6.md) |
| 42 | G5 The command and `Compact` | 3 | |
| 43 | G7 `check` | 2 | |
| 44 | I1 Integration | 3 | |
| 45 | R1 Long runs | 3 | |
| 46 | R2 Independent review | 5 | |
| 47 | R3 Docs and site | 3 | |
| 48 | R4 Release run | 2 | |
| | V2 Vector blocks, only if I1 finds the open target missed | (2) | |

Hours done so far: 99 of 133. The five early tasks, P1 to P5, S1 to S5, V1, F1 to F6, F8, F9, G1 to G3, Q1 to Q4 and T1 to T5 are done. G2 is the first working version. The recorded run, `test/results/beta-bench.txt`, was made again before F7, at fb0be77. It meets the targets for `Get`, a walk one link out, both searches without a filter and a committed put, which is 199 µs on the Beta against 228 µs on 0.x, so the check in F7's contract passes on the code F7 starts from. Reads are slower than in the run after G2: since F9, each read through a `DB` first makes the `stat` call BETA.md plans for, to look for other processes' commits. Beside the run, with the planted bug `hypercrux/reads-never-follow`, which skips the call, `Get` took 0.91 µs and a walk one link out 1.68 µs, against 1.97 µs and 2.81 µs without it. That leaves `Get` at 2.23 µs and a walk one link out at 2.9 µs in the recorded run, against 3 µs each, and one of the walk's three runs was over. A walk three links out is 12 per cent over. The search among vectors of 1,536 values went from 8.45 ms to 11.9 ms with the store's code unchanged, and took 9.1 to 10.8 ms run again beside it, so that's the machine. Two targets are missed by more than double: a put among 1,000 in a transaction, 14.3 µs against 5 µs, which F7 looks into, and opening 100,000 records with vectors, 606 ms against 250 ms, from a log that compaction will replace, which I1 measures again. The next round is F7, Q5 and G6. G7 works in the public package as G6 does, and I1 in the log's tests as F7 does, so they wait for the round after.

Releases publish themselves: raising `Version` in `hypercrux.go` on main
releases it once the tests pass. A2.md explains how.
