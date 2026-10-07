# HyperCrux Beta

The Beta is a new engine for HyperCrux, written in Go, with all four handles
built in. The plan is [BETA.md](../BETA.md). The work lives here, beside 0.x,
which stays as it is. Everything in this folder builds only on Linux. On
macOS and Windows its packages are empty, so 0.x's builds and CI there carry
on unchanged.

Where the work stands, and what comes next: [tasks/README.md](tasks/README.md).

## What's here so far

- `conformance/`: 0.x's tests that don't depend on SQLite, written against a
  small `Engine` interface so the same tests run on 0.x and on the Beta.
- `conformance/zerox/`: the adapter for 0.x. Its test runs the whole suite on
  0.x.
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
- `internal/vecmath/`: the search loop. Dot products, norms and distances
  in 8 running sums, giving the same bits on amd64 and arm64, which CI
  checks on both against a golden file.
- `sqlcorpus/`: SQL with 0.x's answers, in `sqlcorpus/testdata` as JSON
  lines: statements inside and outside the Beta's subset, 3,000 generated
  expressions and 600 date cases. They run on a small fixed database that
  any engine can build. The answers are recorded through 0.x with
  `HYPERCRUX_CORPUS_RECORD=1 go test -run TestRecord ./beta/sqlcorpus`,
  and replayed by the package's tests.

## Running the tests

On Linux:

```sh
go test ./beta/...           # everything, with 200 killed writers
go test -short ./beta/...    # 20 killed writers instead of 200, 300 sequences instead of 2,000
HYPERCRUX_BIN=/path/to/hypercrux go test ./beta/conformance/cmdtest
HYPERCRUX_DIFF_SEQUENCES=50000 go test -run TestZeroxAgainstItself ./beta/difftest   # a long run
```
