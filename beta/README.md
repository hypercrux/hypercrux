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

## Running the tests

On Linux:

```sh
go test ./beta/...           # everything, with 200 killed writers
go test -short ./beta/...    # 20 killed writers instead of 200
HYPERCRUX_BIN=/path/to/hypercrux go test ./beta/conformance/cmdtest
```
