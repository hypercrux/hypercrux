# Task board

The Beta is built one task at a time, in the order below. After each task
the work stops. The code, its closing test and a note for the next session
go into the repository, and the owner says whether to carry on. That gives
the owner a say over each step and over what the work costs.

The tasks and their closing tests are the table in [BETA.md](../../BETA.md),
under "The work, task by task". Each task gets a file here: a brief before it
starts, and a record of what was done when it's finished.

## How to pick up the work

1. Read the next task's file here, and its row in BETA.md's task table.
2. Read the files of the tasks it needs, for the decisions they made.
3. Do the task until its closing test passes. Run `go vet ./...` and
   `go test ./beta/...`, plus `go test -short ./...` for 0.x.
4. Write the task's record, write a brief for the task after it, and update
   the table below. Commit and push, then stop and ask the owner before the
   next task.

## Order and status

The five early tasks come first, then the spec, then the path to the first
working version (G2), then the rest. Every task comes after the tasks it
needs. In this order the first working version arrives about 57 hours of
work in, and all 133 hours are done after R4.

| # | Task | Hours | Status |
|---|---|---|---|
| 1 | A1 Test suites from 0.x | 4 | done, [A1.md](A1.md) |
| 2 | A2 Export and import for 0.x | 4 | done, [A2.md](A2.md), released as 0.2.0 |
| 3 | A3 Differential harness | 3 | next, [A3.md](A3.md) |
| 4 | A4 SQL corpus | 3 | |
| 5 | A5 The search loop | 2 | |
| 6 | P1 The `beta/` folder, build and CI, the task board | 2 | |
| 7 | P2 File format | 3 | |
| 8 | P3 Internal interfaces | 2 | |
| 9 | P4 Package skeleton | 2 | |
| 10 | P5 SQL subset spec | 2 | |
| 11 | S1 Records, fields and rules | 3 | |
| 12 | F1 Codec | 2 | |
| 13 | F2 Log and write lock | 3 | |
| 14 | S2 Transactions | 3 | |
| 15 | S3 Change lists | 2 | |
| 16 | S4 Key order and `Scan` | 3 | |
| 17 | S5 Links and walks | 4 | |
| 18 | V1 Vector arrays and `Nearest` | 4 | |
| 19 | T5 Benchmarks | 2 | |
| 20 | G1 Slice 1: `Put` and `Get` | 2 | |
| 21 | G2 Slice 2, the first working version | 2 | |
| 22 | T1 Fault layer for data | 3 | |
| 23 | T2 Fault layer for names | 2 | |
| 24 | T3 Crash-point driver | 3 | |
| 25 | T4 Many-process harness | 3 | |
| 26 | F3 Crash recovery | 4 | |
| 27 | F4 Damage | 2 | |
| 28 | F5 Failed commits | 2 | |
| 29 | F6 Following other processes | 3 | |
| 30 | F8 Compaction | 3 | |
| 31 | F9 Reloading | 2 | |
| 32 | F7 File rules and growth | 2 | |
| 33 | Q1 Values and functions | 4 | |
| 34 | Q2 Dates | 2 | |
| 35 | Q3 Parser | 5 | |
| 36 | Q4 Operators | 2 | |
| 37 | Q5 Planner | 5 | |
| 38 | Q6 SQL writes | 2 | |
| 39 | G3 The database/sql driver | 2 | |
| 40 | G4 Slice 3: SQL | 2 | |
| 41 | G6 Import and export in the Beta | 2 | |
| 42 | G5 The command and `Compact` | 3 | |
| 43 | G7 `check` | 2 | |
| 44 | I1 Integration | 3 | |
| 45 | R1 Long runs | 3 | |
| 46 | R2 Independent review | 5 | |
| 47 | R3 Docs and site | 3 | |
| 48 | R4 Release run | 2 | |
| | V2 Vector blocks, only if I1 finds the open target missed | (2) | |

Hours done so far: 8 of 133.

Releases publish themselves: raising `Version` in `hypercrux.go` on main
releases it once the tests pass. A2.md explains how.
