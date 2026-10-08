# HyperCrux Beta

The Beta is a new engine for HyperCrux, written in Go, with SQL, key-value,
graph and vector built into the engine itself. The plan is
[BETA.md](../BETA.md). The work lives here, beside 0.x,
which stays as it is. Everything in this folder builds only on Linux. On
macOS and Windows its packages are empty, so 0.x's builds and CI there carry
on unchanged.

Where the work stands, and what comes next: [tasks/README.md](tasks/README.md).

## What's here so far

- `bench/`: 0.1's benchmarks on 0.x and on the Beta side by side, through
  their public packages on the same data, plus opening 100,000 records on
  both. What the Beta can't run yet is skipped, naming its task.
  `sh scripts/record-beta-bench.sh` writes `test/results/beta-bench.txt`,
  with the middle run on each engine beside the Beta's targets.
- `conformance/`: 0.x's tests that don't depend on SQLite, written against a
  small `Engine` interface so the same tests run on 0.x and on the Beta.
- `conformance/zerox/`: the adapter for 0.x. Its test runs the whole suite on
  0.x.
- `conformance/betax/`: the adapter for the Beta, zerox's code over the
  Beta's package. Its tests run the suite on the Beta, once as it is and once
  with the file opened afresh before every read, and skip by name the tests
  that wait for later tasks.
- `conformance/cmdtest/`: 0.x's tests of the `hypercrux` command, run from
  outside against the binary `HYPERCRUX_BIN` names, or against 0.x's command,
  built for the test, when it's unset. `HYPERCRUX_CMD_SKIP` lists sections to
  leave out, separated by commas.
- `difftest/`: the differential harness. It runs random sequences of steps
  on two engines and compares every answer and then the whole database. A
  sequence they disagree on is shrunk and saved in `difftest/testdata`,
  where it replays as a test. It runs 0.x against itself, 0.x against the
  Beta without SQL until G4, and copies of 0.x with one thing each done
  wrong, which it has to catch.
- `FORMAT.md`: the Beta's file format, byte by byte, with the rules for
  reading, checking and writing the log.
- `hypercrux/`: the Beta's Go package, beside 0.x's until the release. It has
  0.x's API. `Open`, `Close`, `Update`, `Get`, `Put`, `Delete`, `Scan`,
  `Drop`, `Link`, `Unlink`, `Neighbours`, `Walk`, `Nearest` without a filter
  and `TableOf` work through the file, and `Check` counts. The helpers that
  hold no state work already, and the rest are stubs that name the task
  that makes them work.
- `internal/errs/`: the error values every layer returns, each of one kind
  in the differential harness's terms.
- `internal/crash/`: the crash-point driver. It runs a workload of commits
  on the fault layer's disk with the power cut in every write, with every
  call failing, and with copies of the file taken mid-commit, and checks
  after each that the database opens at the last commit that succeeded or
  the one under way. Its tests drive a toy log with a planted bug, and
  `internal/logfile`'s tests drive the real log.
- `internal/fault/`: the crash tests' fault layer, a disk held in memory
  behind `fsys.FS` and `fsys.File`. A simulated power cut keeps, loses or
  tears each sector written since the last sync, and keeps the changes to
  names made since their folder's last sync in call order up to a point, as
  ext4's journal does. Any call can be made to fail at its nth use, and a
  seed decides every choice.
- `internal/format/`: the change list, `Change`, which the format's batches
  carry, and the format's golden fixtures in `internal/format/testdata`,
  annotated hex for each part of the format and two small databases, with
  the test that checks them against FORMAT.md, and the codec, which reads
  and writes headers, batches and markers as pure functions.
- `internal/fsys/`: the file calls, `File` and `FS`, which the crash tests'
  fault layers wrap, and the real ones, `OS`, through Go's `syscall`
  package.
- `internal/logfile/`: the database file: creating a database, reading its
  log on opening, commits under the write lock, a mutex then `flock`, each
  with its batch, a sync and its marker, and the check of the end of the
  log, which writes again and marks a batch whose writer died before marking
  it, and cuts off what a crash left half written. Before anything is cut or
  written again, it looks past the end of the log for damage, which it
  reports after a read holding the lock, cutting nothing. Damaged files for
  each kind are in its `testdata/damaged`. A commit that fails is cut back
  out of the file before the lock goes, and when that fails too, the handle
  keeps the lock until it's closed. A process that keeps the database open
  follows other processes' commits with `Follow`: one stat, then each new
  batch's head, its marker and the rest, and the check of the end of the
  log when the writer has gone.
- `internal/procs/`: the many-process harness. It runs writer and reader
  processes on one database, copies of the test binary, kills them with
  SIGKILL at random moments and starts new ones in their place. Then it
  checks that every reader saw every commit once and in order, and that the
  file holds every commit a writer saw succeed, with a commit under way at a
  kill there whole or not at all. Its tests run a toy log with a planted
  bug, and the real log with readers that open the file afresh and with
  followers. Runs can leave gaps with no writer, and wait for counts of a
  workload's own.
- `internal/query/`: SQL. So far the operator iterator, `Rows`, and the
  parser, with the tree it gives and the printer that gives a tree back as
  SQL.
- `internal/rules/`: 0.x's rules for keys, table and field names, link
  types, vectors and stored values, with 0.x's errors and messages. The
  store and `FromGo` check with it, and the public package can share it.
- `internal/store/`: the in-memory copy, with its read API, `Reader`. It
  holds the records with their fields, each table's field list, each table's
  keys in byte order, which `Scan` reads through a `Cursor`, and each
  record's links both ways, which `Neighbours` reads and `Walk` follows, and
  each table's vectors in an array of their own, which `Nearest` searches.
  Its writes give the change lists the log writes, `ApplyBatch` and
  `LoadBatch` take a batch the log has read whole or not at all, and
  `Snapshot` gives the copy as a compacted part. Reads share it through
  `Read`, and writes go through a transaction from `Begin`, which holds
  readers off from its first change until it ends, and puts the copy back
  with its undo list when it rolls back.
- `internal/value/`: the value type, holding FORMAT.md's six kinds of value
  bit for bit, and `FromGo`, 0.x's conversions from Go values.
- `internal/vecmath/`: the search loop. Dot products, norms and distances
  in 8 running sums, giving the same bits on amd64 and arm64, which CI
  checks on both against a golden file.
- `plants.txt`: the bugs planted in the Beta's code, each built in only
  with the `hypercrux_planted` tag. Every one has to make its tests fail.
- `SQL.md`: the Beta's SQL, the subset of SQLite's that it takes, with the
  grammar, the functions, the rules for values, the kind of each error, and
  the named 0.x tests the Beta must pass.
- `sqlcorpus/`: SQL with 0.x's answers, in `sqlcorpus/testdata` as JSON
  lines: statements inside and outside the Beta's subset, 3,000 generated
  expressions and 600 date cases. They run on a small fixed database that
  any engine can build. The answers are recorded through 0.x with
  `HYPERCRUX_CORPUS_RECORD=1 go test -run TestRecord ./beta/sqlcorpus`,
  and replayed by the package's tests.

[tasks/P3.md](tasks/P3.md) maps the packages still to come, and which
task works in each.

## Running the tests

On Linux:

```sh
go test ./beta/...           # everything, with 200 killed writers
go test -short ./beta/...    # 20 killed writers instead of 200, 300 sequences instead of 2,000
HYPERCRUX_BIN=/path/to/hypercrux go test ./beta/conformance/cmdtest
HYPERCRUX_DIFF_SEQUENCES=50000 go test -run TestZeroxAgainstItself ./beta/difftest   # a long run
sh scripts/check-beta.sh     # the rules for this folder, and the board against BETA.md
sh scripts/fuzz.sh 10m       # every fuzz target, for 10 minutes each
sh scripts/planted.sh        # switches on each planted bug, which the tests must catch
HYPERCRUX_CRASH_SEEDS=64 go test ./beta/internal/crash   # every crash point from 64 seeds
HYPERCRUX_PROCS_TIME=10m go test ./beta/internal/procs   # many processes for 10 minutes a run
sh scripts/record-beta-bench.sh   # both engines' benchmarks; needs the machine to itself
```

## CI

Two workflows run on every push to main and every pull request.

- `test` runs 0.x's tests and the Beta's on ubuntu and macOS, with the race
  detector on Linux. On macOS the Beta's packages are empty. Releases of
  0.x wait for this workflow.
- `beta` runs the Beta's own jobs, which no release waits for until the
  Beta's first, in task R4:
  - `rules`: `scripts/check-beta.sh`.
  - `arm64`: the Beta's tests on ARM Linux, with the race detector.
  - `fuzz`: every fuzz target for 30 seconds. An input that fails is kept
    as an artifact, to add to the package's `testdata/fuzz`.
  - `planted`: `scripts/planted.sh`, with every bug in `plants.txt`.

Each job takes a few minutes; the fuzz job grows by 30 seconds with each
new target. [tasks/P1.md](tasks/P1.md) has the reasons, and the steps for a
task that plants a bug.

## Where the Beta differs from 0.x

- **SQL:** [SQL.md](SQL.md) lists every difference, under "Where the Beta
  differs from 0.x". In short, a statement outside the subset is an error,
  and a few things 0.x lets through are refused.
- **The Go API:** `Adopt`, `ApplicationID` and `DriverName` are gone, since
  they mean nothing without SQLite, and `Compact` is new. The Beta's own
  errors are exported beside 0.x's two.
- **Rules:** a link type that starts with a zero byte, and a put that would
  take a table past 1,999 fields, give `ErrInvalid`. 0.x refuses both too,
  with a plain error.
- **A write that fails** changes nothing, even inside an `Update` that goes
  on to commit. 0.x keeps the new fields a failed `Put` named. So a later
  `Put` that names such a field in another case spells it its own way in
  the Beta, and the failed `Put`'s way in 0.x, which `Get` and `Scan` show.
