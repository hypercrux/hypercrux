# Task board

The Beta is built in rounds, in the order below. A round is one task or a
few at once, each done by an agent of its own in its own git worktree, and
a coordinating session reviews and merges them. After each round the
work stops. The code, each task's closing test and record, and briefs for
the next round go into the repository, and the owner says whether to carry
on. That gives the owner a say over each step and over what the work costs.
The first seven tasks ran one at a time, before the owner approved more
agents. On 13 October 2026 the owner chose one task a round for the rest,
with R2 in two parts, so each round stays short.

The tasks and their closing tests are the table in [BETA.md](../../BETA.md),
under "The work, task by task". Each task gets a file here: a brief before it
starts, and a record of what was done when it's finished.

## How to pick up the work

1. The next round is the task marked "next" below, or the part of it that
   its status names: one task a round from R1 on.
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
| 32 | F7 File rules and growth | 2 | done, [F7.md](F7.md), with Q5 and G6 |
| 33 | Q1 Values and functions | 4 | done, [Q1.md](Q1.md) |
| 34 | Q2 Dates | 2 | done, [Q2.md](Q2.md) |
| 35 | Q3 Parser | 5 | done, [Q3.md](Q3.md), ahead of Q1 and Q2 |
| 36 | Q4 Operators | 2 | done, [Q4.md](Q4.md) |
| 37 | Q5 Planner | 5 | done, [Q5.md](Q5.md) |
| 38 | Q6 SQL writes | 2 | done, [Q6.md](Q6.md), with G7 and I1 |
| 39 | G3 The database/sql driver | 2 | done, [G3.md](G3.md) |
| 40 | G4 Slice 3: SQL | 2 | done, [G4.md](G4.md), alone |
| 41 | G6 Import and export in the Beta | 2 | done, [G6.md](G6.md) |
| 42 | G5 The command and `Compact` | 3 | done, [G5.md](G5.md), alone |
| 43 | G7 `check` | 2 | done, [G7.md](G7.md) |
| 44 | I1 Integration | 3 | done, [I1.md](I1.md) |
| 45 | R1 Long runs | 3 | done, [R1.md](R1.md), alone |
| | C1 Compaction by dead bytes, which the owner chose after R2's first part | (2) | done, [C1.md](C1.md), alone |
| 46 | R2 Independent review | 5 | done, [R2.md](R2.md), in two parts, each alone |
| 47 | R3 Docs and site | 3 | next, [R3.md](R3.md), alone, once the owner has answered R2's questions |
| 48 | R4 Release run | 2 | after R3, alone |
| | V2 Vector blocks, only if I1 finds the open target missed | (2) | done, [V2.md](V2.md), alone, vector blocks alone |

Hours done so far: 128 of 133, and 2 each for V2 and C1 besides, which the 133 leave out. Every task is done apart from R3 and R4: the five early tasks, P1 to P5, S1 to S5, V1 and V2, C1, F1 to F9, G1 to G7, I1, Q1 to Q6, R1, R2 and T1 to T5. G2 is the first working version. V2 ran alone, with its scope cut by the owner to vector blocks alone: compaction writes each table's vectors in blocks of about a megabyte, which opening copies straight into the arrays. In V2's own runs, the two builds taking turns, opening 100,000 records with 384-value vectors from a compacted file took about 555 ms, against 678 ms before it, and compacting them about 0.35 seconds, against 0.6. In the recorded run after C1, `test/results/beta-bench.txt`, on a machine about as fast as in the run after R2's first part, a put among 1,000 in a transaction takes 10.1 µs, where it took 15.7, against the target of 5 µs and 49.6 µs on 0.x: C1 took away the compactions that doubled it, and it's missed by just over double now. Opening the compacted database of 100,000 records with vectors takes 417 ms against the target of 250 ms. A committed put is 308 µs on the Beta against 325 µs on 0.x, so F7's check holds. Both searches without a filter meet their targets, the filtered search is 16.3 ms against 12 ms, where 0.x takes 110 ms, and `Get`, at 2.66 µs, meets its 3 µs. A walk one link out is 3.65 µs and three links out 56.4 µs, against 3 µs and 40 µs. On 13 October 2026 the owner chose to run the rest one task a round: R1, then R2 in two parts, the file's safety first and then the rest, then R3, then R4. R1 is done: a workflow of long runs on both architectures, started by hand, and the push workflow's planted job in five shards side by side, which takes under 6 minutes against 18 to 33. The long workflow's first full run, on 13 October 2026, passed ten of its twelve jobs at full length, every planted bug caught on both architectures among them. On both, `FuzzParse` found a condition that didn't print back as itself, a name made of a non-ASCII space, which the coordinator fixed with a test on 14 October 2026. The second run, that day, passed `fuzz-2`, the crash suites at 3,840 seeds, the differential harness at 800,000 sequences and all 235 planted bugs on both architectures. R2's first part, the review of the file's safety, ran alone on 14 October 2026. It fixed one bug, a decoder that could take about 89 times a damaged batch's size in memory, with a test, and answered four other findings and the notes on the file, with wording for BETA.md. It left two questions for the owner, in R2.md: when to compact, where it recommends counting dead bytes with a limit on the commits past the compacted part, as a small task before R4, and whether the format version should change, where it recommends keeping version 1 until R4. On 14 October 2026 the owner took both recommendations: a new task, C1, compacts by dead bytes with a limit on the commits past the compacted part, at 2 hours on top of the plan's 133, and the format version stays 1 until the release. C1 ran alone that day, before R2's second part, so the review takes in its code with the store's. A compaction after a commit now comes once the commits since the last have made half the file dead, and a megabyte, or once 100,000 commits follow the compacted part, so a database that only grows is compacted only at that limit. In C1's own runs a put among 1,000 in a transaction took about 11 µs, where it took about 16.9, the time it had with no compaction after a commit. After C1 the owner kept the limit on commits fixed, whatever the database's size. R2's second part, the review of the store, SQL, the public package and the command, ran alone on 14 October 2026. It fixed five of the notes earlier tasks left it, each with a test, and each with a planted bug apart from one: SQL's own nearest search through a filter no longer copies every record's vector, and takes about 14 ms where it took about 150; Check's read of the file finds another database's file, or another copy of this one, copied over it in place; a filter that ends inside a comment is incomplete input, as in 0.x; the command builds on macOS and Windows, where it says it runs on Linux only, so `go build ./...` works there again; and Open's error for a file that isn't a database names the package once. It answered the other notes and two findings of its own: the writer an export writes to mustn't call the database, which Export's comment now says, and the speed targets still missed, whose time it measured. It left two questions for the owner, in R2.md: whether text and bytes should stop at 0.x's 1 GB, where it recommends taking 0.x's limit, and whether to accept the missed targets for the Beta, where it recommends accepting them. The next round is R3, once the owner has answered them, then R4.

Releases publish themselves: raising `Version` in `hypercrux.go` on main
releases it once the tests pass. A2.md explains how.
