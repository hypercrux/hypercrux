# HyperCrux Beta

The Beta is a new engine for HyperCrux, written in Go, with SQL, key-value,
graph and vector built into the engine itself. The plan is
[BETA.md](../BETA.md). The work lives here, beside 0.x,
which stays as it is. Everything in this folder builds only on Linux. On
macOS and Windows its packages are empty, so 0.x's builds and CI there carry
on unchanged.

Where the work stands, and what comes next: [tasks/README.md](tasks/README.md).

## What's here so far

- `conformance/`: 0.x's tests that don't depend on SQLite, written against a
  small `Engine` interface so the same tests run on 0.x and on the Beta.
- `conformance/zerox/`: the adapter for 0.x. Its test runs the whole suite on
  0.x.
- `conformance/betax/`: the adapter for the Beta, zerox's code over the
  Beta's package. Its test runs the whole suite on the Beta from task G1 on.
- `conformance/cmdtest/`: 0.x's tests of the `hypercrux` command, run from
  outside against the binary `HYPERCRUX_BIN` names, or against 0.x's command,
  built for the test, when it's unset. `HYPERCRUX_CMD_SKIP` lists sections to
  leave out, separated by commas.
- `difftest/`: the differential harness. It runs random sequences of steps
  on two engines and compares every answer and then the whole database. A
  sequence they disagree on is shrunk and saved in `difftest/testdata`,
  where it replays as a test. For now it runs 0.x against itself, and
  against copies of 0.x with one thing each done wrong, which it has to
  catch.
- `FORMAT.md`: the Beta's file format, byte by byte, with the rules for
  reading, checking and writing the log.
- `hypercrux/`: the Beta's Go package, beside 0.x's until the release. It has
  0.x's API. The helpers that hold no state work already, and the rest are
  stubs that name the task that makes them work.
- `internal/errs/`: the error values every layer returns, each of one kind
  in the differential harness's terms.
- `internal/format/`: the change list, `Change`, which the format's batches
  carry, and the format's golden fixtures in `internal/format/testdata`,
  annotated hex for each part of the format and two small databases, with
  the test that checks them against FORMAT.md. The codec joins them here.
- `internal/fsys/`: the file calls, `File` and `FS`, which the crash tests'
  fault layers wrap. The real ones, through Go's `syscall` package, come
  with the log.
- `internal/query/`: SQL. For now, the operator iterator, `Rows`.
- `internal/store/`: the in-memory copy. For now, its read API, `Reader`.
- `internal/value/`: the value type, holding FORMAT.md's six kinds of value
  bit for bit.
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

Each job takes three minutes or less. [tasks/P1.md](tasks/P1.md) has the
reasons, and the steps for a task that plants a bug.
