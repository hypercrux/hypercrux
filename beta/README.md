# HyperCrux Beta

The Beta is a new engine for HyperCrux, a native hybrid SQL, key-value,
graph and vector database in one file, written in Go with no SQLite and no
C underneath. It keeps what 0.x proved: four handles on one record, kept in
step by one transaction, with 0.x's Go API and 0.x's command. Underneath,
it's one append-only file on disk and, in each process that opens it, one
copy of the data in memory, so a search compares vectors straight from
memory and a key goes straight to its record. The plan, with the reasons
for each choice, is [BETA.md](../BETA.md).

The Beta's first release is 0.3.0-beta.1, a pre-release. 0.x stays the
stable line, and the repository's latest release. The Beta lives here, in
`beta/`, beside 0.x, which stays as it is at the repository's root and keeps
getting fixes. How the Beta was built, task by task:
[tasks/README.md](tasks/README.md).

Everything in this folder runs only on Linux, on x86 and ARM. On macOS and
Windows its packages are empty, apart from the command, which builds there
and says it runs on Linux only, so 0.x's builds and CI there carry on
unchanged, `go build ./...` among them.

Coming from 0.x? [MOVING.md](MOVING.md) has every difference, with what to
do about each, and how to move a database across with an export.

## Get it

The release's page,
[v0.3.0-beta.1](https://github.com/hypercrux/hypercrux/releases/tag/v0.3.0-beta.1),
has the command ready-built for Linux, a static binary in
`hypercrux-beta_linux_amd64.tar.gz` for x86-64 and in
`hypercrux-beta_linux_arm64.tar.gz` for ARM64, with SHA256SUMS. Each
archive holds `hypercrux`, its licence files and this README. Put the binary
on your PATH under a name of your own if 0.x's `hypercrux` is there already.

From a Go program, fetch the release by its version, since a plain
`go get` of the module stays on 0.x's latest release:

```sh
go get github.com/hypercrux/hypercrux/beta/hypercrux@v0.3.0-beta.1
```

and import `github.com/hypercrux/hypercrux/beta/hypercrux`. The package is
called `hypercrux`, as 0.x's is, so a program moves from one to the other
by its import path.

## Build it

You need Go 1.24 or later, and no C compiler: the Beta builds with the Go
toolchain alone, `CGO_ENABLED=0` included.

```sh
git clone https://github.com/hypercrux/hypercrux
cd hypercrux
go build -o hypercrux ./beta/cmd/hypercrux
./hypercrux version
```

At the release, the last line prints
`hypercrux 0.3.0-beta.1, file format 1`. The release's tag,
`v0.3.0-beta.1`, holds its code, and main the code since.

## Try the command

Records go in with fields as JSON. The part of the key before the colon is
the table, and `vec` is the record's vector:

<!-- run -->
```sh
hypercrux put notes.db customer:42 '{"name": "Dana"}'
hypercrux put notes.db docs:1 '{"title": "Q3 plan", "status": "open", "vec": [0.9, 0.1, 0]}'
hypercrux put notes.db docs:2 '{"title": "Hiring notes", "status": "done", "vec": [0.1, 0.9, 0.1]}'
hypercrux put notes.db docs:3 '{"title": "Q3 budget", "status": "open", "vec": [0.8, 0.2, 0.1]}'
hypercrux link notes.db customer:42 owns docs:1
hypercrux link notes.db customer:42 owns docs:2
hypercrux link notes.db docs:1 cites docs:3
```

Walk the links:

<!-- run -->
```sh
hypercrux walk notes.db customer:42 2
```

<!-- output -->
```text
1  docs:1
1  docs:2
2  docs:3
```

Find the closest vectors, with a filter in SQL or without one:

<!-- run -->
```sh
hypercrux nearest notes.db docs '[1, 0, 0]' -k 2
hypercrux nearest notes.db docs '[1, 0, 0]' --where "status = 'done'"
```

<!-- output -->
```text
0.0061  docs:1
0.0369  docs:3
0.8902  docs:2
```

And ask SQL, in the subset [SQL.md](SQL.md) sets out:

<!-- run -->
```sh
hypercrux sql notes.db "SELECT key, title FROM docs WHERE status = ? ORDER BY key" open
```

<!-- output -->
```text
key     title
docs:1  Q3 plan
docs:3  Q3 budget
```

`hypercrux check` confirms that keys, records, links and vectors agree, and
gives the file's size and the memory the copy takes. `hypercrux compact`
rewrites the file with only the live data, which the database also does by
itself now and then. `hypercrux export` and `hypercrux import` write and
read every table, record and link as JSON lines, in the format 0.x uses:

<!-- run -->
```sh
hypercrux check notes.db
hypercrux compact notes.db
hypercrux export notes.db > notes.jsonl
hypercrux import copy.db < notes.jsonl
```

`hypercrux help` lists every verb. They're 0.x's, with the same output,
messages and exit codes, apart from `adopt`, which the Beta hasn't got, and
`compact`, which is new. Other languages use the command, as with 0.x,
reading and writing JSON. Each run opens the database afresh, which reads
the whole file.

## Use it from Go

This program puts the same records in a database of its own and reads them
back all four ways. `go doc github.com/hypercrux/hypercrux/beta/hypercrux`
gives the whole API.

<!-- run -->
```go
package main

import (
	"fmt"
	"log"

	"github.com/hypercrux/hypercrux/beta/hypercrux"
)

func main() {
	db, err := hypercrux.Open("example.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Writes that belong together go in one transaction.
	err = db.Update(func(tx *hypercrux.Tx) error {
		for _, r := range []hypercrux.Record{
			{Key: "customer:42", Fields: hypercrux.Fields{"name": "Dana"}},
			{Key: "docs:1", Fields: hypercrux.Fields{"title": "Q3 plan", "status": "open", "vec": hypercrux.Vector{0.9, 0.1, 0}}},
			{Key: "docs:2", Fields: hypercrux.Fields{"title": "Hiring notes", "status": "done", "vec": hypercrux.Vector{0.1, 0.9, 0.1}}},
			{Key: "docs:3", Fields: hypercrux.Fields{"title": "Q3 budget", "status": "open", "vec": hypercrux.Vector{0.8, 0.2, 0.1}}},
		} {
			if err := tx.Put(r.Key, r.Fields); err != nil {
				return err
			}
		}
		for _, l := range []hypercrux.Link{
			{From: "customer:42", Type: "owns", To: "docs:1"},
			{From: "customer:42", Type: "owns", To: "docs:2"},
			{From: "docs:1", Type: "cites", To: "docs:3"},
		} {
			if err := tx.Link(l.From, l.Type, l.To); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	question := hypercrux.Vector{1, 0, 0}

	fields, err := db.Get("docs:1") // by key
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("get:", fields["title"], fields["vec"])

	steps, err := db.Walk("customer:42", hypercrux.Out, "", 2) // by link
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range steps {
		fmt.Println("walk:", s.Depth, s.Key)
	}

	hits, err := db.Nearest("docs", question, 2, "status = ?", "open") // by similarity
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range hits {
		fmt.Printf("nearest: %.4f %s\n", h.Distance, h.Key)
	}

	// By SQL, across all four: the open documents within two links of the
	// customer, closest to the question first.
	rows, err := db.Query(`SELECT d.key, d.title
		FROM json_each(walk('customer:42', 2)) w
		JOIN docs d ON d.key = w.value
		WHERE d.status = 'open' AND d.vec IS NOT NULL
		ORDER BY distance(d.vec, ?)
		LIMIT 10`, question)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, title string
		if err := rows.Scan(&key, &title); err != nil {
			log.Fatal(err)
		}
		fmt.Println("sql:", key, title)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}

	report, err := db.Check()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("check: ok %v, %d tables, %d records, %d links\n", report.OK(), report.Tables, report.Records, report.Links)
}
```

<!-- output -->
```text
get: Q3 plan [0.9,0.1,0]
walk: 1 docs:1
walk: 1 docs:2
walk: 2 docs:3
nearest: 0.0061 docs:1
nearest: 0.0369 docs:3
sql: docs:1 Q3 plan
sql: docs:3 Q3 budget
check: ok true, 2 tables, 4 records, 3 links
```

Inside `Update`, make every call through `tx`. A call through `db` there
would have to wait for the `Update` itself, so it fails at once with
`ErrInsideUpdate`. A `DB` is safe for many goroutines, and many processes
can share one file: each read first takes in what other processes have
committed.

## How fast, and what it costs

From the run recorded for the release on 15 October 2026, both engines on
one two-core cloud machine, the middle of three runs each
([test/results/beta-bench.txt](../test/results/beta-bench.txt)). Where the
Beta still misses one of the plan's targets, the target is beside its time:

| Operation | 0.x | The Beta |
|---|---|---|
| Nearest 10 among 100,000 vectors of 384 values | 294 ms | 58.7 ms |
| The same, a tenth passing a filter | 90.4 ms | 9.26 ms |
| Nearest 10 among 10,000 vectors of 1,536 values | 93.6 ms | 10.5 ms |
| Get a record by key | 11.2 µs | 1.8 µs |
| Walk one and three links out, 100,000 records with 5 links each | 24.8 µs and 345 µs | 3.03 µs and 43.9 µs, against targets of 3 µs and 40 µs |
| Put records with 384-value vectors, 1,000 a transaction | 34.9 µs a record | 16.1 µs a record, against a target of 5 µs |
| Put one record, committed | 262 µs | 210 µs |
| Open 100,000 records with 384-value vectors | 0.43 ms | 323 ms, against a target of 250 ms |

Cloud machines vary from one run to the next, so both engines ran in this
one run, and they're compared within it. This run's machine, with two cores
of an Intel Xeon at 2.10 GHz, was slower at reading memory than the machines
of the earlier recorded runs: the search of 100,000 vectors took 29 ms in
the run after L1, and on this machine that morning the earlier code took
about as long as this run's, so the code isn't the cause
([tasks/R4.md](tasks/R4.md)).

The speed comes from the copy in memory, and so do the costs:

- **Opening reads the whole file.** 0.x opens a database in under a
  millisecond, since SQLite reads pages as it needs them. A program that
  keeps the database open pays the Beta's cost once, and each run of the
  command pays it again.
- **Every process holds the whole database in memory.** `Check` and
  `hypercrux check` give the copy's size. Go's heap can grow to twice the
  live data between collections, and `GOMEMLIMIT`, set a little above the
  copy's size, keeps it near.
- **Writers wait while a compaction runs,** for about as long as writing
  and syncing the live data once takes. A database compacts itself after a
  commit once the commits since the last compaction have made half the file
  dead, and a megabyte at least, or once 100,000 commits follow it, so a
  database that only grows is rewritten once in 100,000 commits.

Three of the plan's speed targets are still missed in that run, each beside
its time above: the walks, the batched put and the open. The filtered
search, which missed its target in the runs before, met it in this one. The
owner accepted the misses for the Beta on 14 October 2026, and
[tasks/R2.md](tasks/R2.md) says where their time goes. Two fifths of a walk
one link out is the `stat` each read makes to see other processes' commits,
and nearly half of a walk three links out sorts each round's records by key,
as 0.x orders them. About 3 µs of each batched put is the sync of its batch.
The open applies the vector blocks, about half of that in filling their
fresh memory, builds each record through the general put, and decodes the
batches.

## What the Beta leaves out

- macOS, until after the Beta, and Windows, which needs its own file
  locking. 0.x keeps its macOS and Windows builds. 32-bit systems are out
  too.
- Data bigger than memory.
- Full SQL. The Beta's SQL is a subset of SQLite's, and [SQL.md](SQL.md)
  sets it out. Missing from it, among others: `GROUP BY` and `HAVING`,
  joins other than the one to `walk()`, other subqueries, `CASE`, JSON
  functions over fields, indexes, and date functions on anything but
  `'now'`.
- SQLite's file and its tools, and the rules stored in the file as
  triggers. Other languages reach a database through the command, or
  write the file by [FORMAT.md](FORMAT.md)'s rules.
- `Adopt`, the `adopt` verb, and transactions begun with `SQL().Begin`.
  Transactions go through `Update`.
- Approximate vector indexes. Every search compares every vector that
  passes its filter, so it never misses a closer one.
- Read-only opening and a restore command, which 0.x hasn't got either. A
  plain copy of the file is a backup, and FORMAT.md says how to restore one.
- Servers, replication and anything across machines.

The limits are 0.x's: keys up to 1,024 bytes, at most 1,999 fields a table,
vectors of up to 65,536 values, the same size across a table, text and
bytes of at most 1,000,000,000 bytes each, walks up to 32 links deep and up
to 10,000 hits a search.

## The specs

- [FORMAT.md](FORMAT.md): the file, byte by byte, with every rule a program
  keeps while it writes it: the write lock, commits, recovery, compaction,
  backups and the file systems a database can live on.
- [SQL.md](SQL.md): the Beta's SQL, with the grammar, the functions, the
  rules for values, the kind of each error, and every difference from 0.x.
- [EXPORT.md](../EXPORT.md): the export format, which both engines read and
  write.
- [MOVING.md](MOVING.md): moving from 0.x, with what to do about each
  difference.
- [BETA.md](../BETA.md): the design and the plan, and the decisions taken on
  the way.

## Where the Beta differs from 0.x

[MOVING.md](MOVING.md) gives each of these with what to do about it:

- **SQL:** [SQL.md](SQL.md) lists every difference, under "Where the Beta
  differs from 0.x". In short, a statement outside the subset is an error,
  and a few things 0.x lets through are refused.
- **The Go API:** `Adopt`, `ApplicationID` and `DriverName` are gone, since
  they mean nothing without SQLite, and `Compact` is new. The Beta's own
  errors are exported beside 0.x's two. `Report` gains `FileBytes`,
  `MemoryBytes` and `CompactionError` after 0.x's fields.
- **Rules:** a link type that starts with a zero byte, and a put that would
  take a table past 1,999 fields, give `ErrInvalid`. 0.x refuses both too,
  with a plain error.
- **The command:** `adopt` has gone and `compact` is new. `version` names no
  SQLite, `check` gives the sizes on a line of their own and prints damage
  that stops the database opening as a problem, `init` makes a database of an
  empty file and refuses any other file that isn't one, and `sql` takes the
  Beta's SQL. [tasks/G5.md](tasks/G5.md) lists each, with its reason.
- **The file:** a database file with more than one hard link is refused
  with `ErrInvalid`, and so is a symbolic link that leads to nothing. 0.x
  opens the first, and makes a database where the second leads. A hard link
  made while the database is open makes every write fail with `ErrInvalid`
  until it's gone.
- **Import** refuses a table that has a vector size and no field `vec`,
  which 0.x can hold, and refuses a table of more than 1,999 fields as
  invalid, where 0.x gives SQLite's own error. In its own process, an
  `Update`'s first change waits for an export under way to end, as it waits
  for any read, where 0.x's writers carry on through an export. The writer
  `Export` writes to mustn't call the database, since a read from inside it
  can wait for such an `Update`, which waits for the export.
- **Values:** text and bytes hold at most 1,000,000,000 bytes, SQLite's
  limit, refused with SQLite's error, "string or blob too big", as in 0.x.
  0.x's SQLite holds each whole record to that limit too, its key and every
  field in SQLite's own form, so 0.x's `Put`, SQL writes and import refuse a
  value a few bytes short of the limit, 999,999,987 bytes at most for
  `docs:1` with one field, and a record whose values add up to more. The
  Beta takes those, and an export holding one doesn't import into 0.x.
- **A write that fails** changes nothing, even inside an `Update` that goes
  on to commit. 0.x keeps the new fields a failed `Put` named, and the table
  when the `Put` made it. So a later `Put` that names such a field in
  another case spells it its own way in the Beta, and the failed `Put`'s way
  in 0.x, which `Get` and `Scan` show.

## For contributors

The Beta is built in tasks, each with a file in [tasks/](tasks/): a brief
before it starts, and a record of what was done once it's finished.
[tasks/README.md](tasks/README.md) is the board, and the records hold the
decisions, the measurements and the notes each task left for the next.

### What each folder holds

- `bench/`: 0.1's benchmarks on 0.x and on the Beta side by side, through
  their public packages on the same data, plus opening 100,000 records on
  both. What the Beta can't run yet is skipped, naming its task.
  `sh scripts/record-beta-bench.sh` writes `test/results/beta-bench.txt`,
  with the middle run on each engine beside the Beta's targets.
- `cmd/hypercrux/`: the Beta's `hypercrux` command, with 0.x's verbs apart
  from `adopt`, and 0.x's output, messages and exit codes, plus `compact`.
  Each run opens the database afresh, reading the whole file. Its own tests
  cover what 0.x's command hasn't got.
- `conformance/`: 0.x's tests that don't depend on SQLite, written against a
  small `Engine` interface so the same tests run on 0.x and on the Beta.
- `conformance/zerox/`: the adapter for 0.x. Its test runs the whole suite on
  0.x.
- `conformance/betax/`: the adapter for the Beta, zerox's code over the
  Beta's package. Its tests run the suite on the Beta, once as it is and once
  with the file opened afresh before every read, and skip by name the tests
  that wait for later tasks. They also move databases from 0.x to the Beta
  and back through an export, byte for byte.
- `conformance/cmdtest/`: 0.x's tests of the `hypercrux` command, run from
  outside against the binary `HYPERCRUX_BIN` names, or, when it's unset,
  against 0.x's command and the Beta's, both built for the test, the Beta's
  without the sections SQL.md leaves out. `HYPERCRUX_CMD_SKIP` lists sections
  to leave out, separated by commas. `TestDrop` runs the `drop` verb on both.
- `difftest/`: the differential harness. It runs random sequences of steps
  on two engines and compares every answer and then the whole database. A
  sequence they disagree on is shrunk and saved in `difftest/testdata`,
  where it replays as a test. It runs 0.x against itself, 0.x against the
  Beta with every kind of step, the crux query among them, and copies of
  0.x with one thing each done wrong, which it has to catch.
- `FORMAT.md`: the Beta's file format, byte by byte, with the rules for
  reading, checking and writing the log.
- `hypercrux/`: the Beta's Go package, beside 0.x's until the release. It
  has 0.x's API. `Open`, `Close`, `Update`, `Get`, `Put`, `Delete`, `Scan`,
  `Drop`, `Link`, `Unlink`, `Neighbours`, `Walk`, `Nearest` and `TableOf`
  work through the file, and so do `Export` and `Import`, in 0.x's export
  format, byte for byte as 0.x writes it. `Check` checks the copy in memory
  and reads the file again for damage, or for another file copied over it in
  place, and gives the file's size and the copy's, and Export and Import
  refuse a database it finds problems in. `Query`, `QueryRow`, `Exec`,
  `SQL()` and `Nearest` with a filter run the Beta's SQL through its
  database/sql driver, and `SQL().Begin` returns an error. Each read through
  a `DB` follows what other processes commit, and reloads the file after a
  compaction or a backup moved into place. `Compact` rewrites the file with
  only the live data, a commit is followed by a compaction once the commits
  since the last have made half the file dead, and a megabyte, or once
  100,000 commits follow it, and `Check` reports one that failed. Its tests
  drive the crash-point driver through it, on the fault layer, and time
  opening a compacted file.
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
  and writes headers, batches and markers as pure functions. A batch of
  vector blocks alone can be read without a copy (`DecodeBatchInPlace`).
  Decoding a batch makes its arrays once, the size a first walk of its
  syntax counts.
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
  log when the writer has gone. The holder of the write lock compacts the
  database from a snapshot of the copy into `NAME.compact`, each table's
  vectors in vector blocks of about a megabyte, which is synced and renamed
  over the database, while writers wait it out, and the next holder
  removes one a crash left. `Due` says when a compaction is due after a
  commit, from the dead bytes a Target that's a `Counter` counts and the
  commits past the compacted part, with `Options.MinGrowth` and
  `Options.MaxCommits` in place of a megabyte and 100,000, for tests. A
  process that finds another file at the path reads it from its start with
  `Reload`, once its old copy has gone and a garbage collection has run. A
  Target that keeps nothing of a vector block can take a batch of blocks
  straight from the log's read buffer (`Options.BlocksInPlace`). It opens a
  database by its real path, and refuses a file with more than one hard
  link, and anything at the path that isn't a regular file, before anything
  opens it. Its first commit into a file whose name it didn't make last
  itself syncs the folder first.
- `internal/procs/`: the many-process harness. It runs writer and reader
  processes on one database, copies of the test binary, kills them with
  SIGKILL at random moments and starts new ones in their place. Then it
  checks that every reader saw every commit once and in order, and that the
  file holds every commit a writer saw succeed, with a commit under way at a
  kill there whole or not at all. Its tests run a toy log with a planted
  bug, and the real log with readers that open the file afresh and with
  followers. Runs can leave gaps with no writer, wait for counts of a
  workload's own, and move backups into place at random moments, checking
  each era of the log against its own file. Runs can insist on writers
  killed in the middle of a commit. Its tests run the public package under
  it: the 200 SIGKILLs, and writers and readers through compactions and
  backups.
- `internal/query/`: SQL. So far the operator iterator, `Rows`, the parser,
  with the tree it gives and the printer that gives a tree back as SQL, and
  the evaluator, which works out expressions and conditions as SQLite does,
  with `Arg` for arguments and `Bind`, which holds them to SQLite's limit on
  length, and the dates on 'now' with SQLite's arithmetic. The operators
  read the store through `store.Reader` and hand rows on through `Rows`,
  with the aggregates, and the planner plans a SELECT over the store and
  runs it in one call, `Query`. The writes run an INSERT, UPDATE or DELETE
  inside a transaction in one call, `Exec`.
- `internal/rules/`: 0.x's rules for keys, table and field names, link
  types, vectors and stored values, with 0.x's errors and messages, and
  SQLite's limit on text and bytes, `MaxLength`, which `Length` checks. The
  store and `FromGo` check with it, and the public package can share it.
- `internal/store/`: the in-memory copy, with its read API, `Reader`. It
  holds the records with their fields, each table's field list, each table's
  keys in byte order, which `Scan` reads through a `Cursor`, and each
  record's links both ways, which `Neighbours` reads and `Walk` follows, and
  each table's vectors in an array of their own, which `Nearest` searches.
  Its writes give the change lists the log writes, `ApplyBatch` and
  `LoadBatch` take a batch the log has read whole or not at all, and
  `Snapshot` gives the copy as a compacted part, and `Compacted` gives it as
  compaction writes it, each table's vectors straight from its array for
  vector blocks. Reads share it through
  `Read`, and writes go through a transaction from `Begin`, which holds
  readers off from its first change until it ends, and puts the copy back
  with its undo list when it rolls back. `Check` checks that the copy's
  parts agree, and adds up its size. It counts the dead bytes its changes
  leave in the file, which the log weighs for a compaction (`Dead`).
- `internal/value/`: the value type, holding FORMAT.md's six kinds of value
  bit for bit, `FromGo`, 0.x's conversions from Go values, and SQLite's
  routines for reading numbers from text and writing reals as text.
- `internal/vecmath/`: the search loop. Dot products, norms and distances
  in 8 running sums, giving the same bits on amd64 and arm64, which CI
  checks on both against a golden file.
- `MOVING.md`: moving a program and its data from 0.x to the Beta.
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
  and replayed by the package's tests, on 0.x and on the Beta.

[tasks/P3.md](tasks/P3.md) maps the packages, and which task worked in
each.

### Running the tests

On Linux:

```sh
go test ./beta/...           # everything, with 200 killed writers
go test -short ./beta/...    # 20 killed writers instead of 200, 300 sequences instead of 2,000
HYPERCRUX_BIN=/path/to/hypercrux go test ./beta/conformance/cmdtest
HYPERCRUX_DIFF_SEQUENCES=50000 go test -run TestZeroxAgainstItself ./beta/difftest   # a long run
sh scripts/check-beta.sh     # the rules for this folder, and the board against BETA.md
python3 scripts/check-docs.py   # the docs' links and examples
python3 scripts/check-docs.py ../hypercrux.com   # and the site's posts too, with a clone of the site and Hugo
sh scripts/fuzz.sh 10m       # every fuzz target, for 10 minutes each
sh scripts/planted.sh        # switches on each planted bug, which the tests must catch
HYPERCRUX_CRASH_SEEDS=64 go test ./beta/internal/crash   # every crash point from 64 seeds, or a test's own if it asks for more
sh scripts/long.sh crash 3840   # a long run's job, as the long workflow runs it (tasks/R1.md)
HYPERCRUX_PROCS_TIME=10m go test ./beta/internal/procs   # many processes for 10 minutes a run
sh scripts/record-beta-bench.sh   # both engines' benchmarks; needs the machine to itself
sh scripts/record-beta-tests.sh   # the Beta's tests, the race detector, fuzzing and planted bugs, into test/results/
python3 scripts/record-long.py RUN_ID   # a long run's jobs and notices, from GitHub, into test/results/beta-long.txt
python3 scripts/release-plan-test.py   # the release workflow's plan, case by case
go build -o hypercrux ./beta/cmd/hypercrux   # the Beta's command
```

`scripts/record-beta-tests.sh` records the Beta's tests as
`scripts/record-tests.sh` records 0.1's, each to a file of its own in
`test/results/`: `beta-tests.txt`, every test in `beta/`, verbose;
`beta-race.txt`, the same with the race detector in short mode;
`beta-fuzz.txt`, each fuzz target for a minute; and `beta-planted.txt`, every
planted bug. With a folder and `quick` it checks itself in a minute or two.
`scripts/record-long.py` writes a long run's commit and times, and each job's
result, times and notices, to `test/results/beta-long.txt`, since the
notices are what a cloud session can read of a run.

`scripts/check-docs.py` checks the links in the Beta's docs and runs every
example they mark to run, on the Beta's package and command, and on 0.x's
command for the step that exports from 0.x, all built there and then. It
needs Go, and a C compiler to build 0.x's command. Given the path of a clone
of the site, github.com/photozeitgeist/hypercrux.com, it checks the site's
posts about the Beta too, and the docs' links to the site's pages, which
needs Hugo as well. Without the site, it lists those links among the ones
it leaves alone. The site's repository is private, so where the site can't
be checked out, as in CI, it runs with no argument, and whoever publishes
the posts runs it with the site's path before they go up. Its opening
comment says how a doc marks an example and its output.

### CI

Two workflows run on every push to main and every pull request, a third
runs by hand, and the release workflow follows the first two on main.

- `test` runs 0.x's tests and the Beta's on ubuntu and macOS, with the race
  detector on Linux. On macOS the Beta's packages are empty, apart from the
  command's message that it runs on Linux only. Releases of 0.x wait for
  this workflow, and the Beta's for it and `beta`.
- `beta` runs the Beta's own jobs, which the Beta's releases wait for (task
  R4), and 0.x's don't:
  - `rules`: `scripts/check-beta.sh`.
  - `arm64`: the Beta's tests on ARM Linux, with the race detector.
  - `fuzz`: every fuzz target for 30 seconds. An input that fails is kept
    as an artifact, to add to the package's `testdata/fuzz`.
  - `planted`: `scripts/planted.sh` in five shards side by side, each with
    every fifth bug in `plants.txt`.
  - `docs`: `scripts/check-docs.py` with no argument, which checks the links
    and runs the examples in the Beta's docs. The site's posts are checked
    with the site's path before they go up.
- `long` runs only when started by hand: six jobs on amd64 and six on
  arm64, side by side, with hours of fuzzing, the crash suites with many
  seeds, the many-process harness, the differential harness and every
  planted bug. [tasks/R1.md](tasks/R1.md) says how to start a run and read
  it.
- `release` publishes a release when a version goes up on main, as the next
  section says.

Each push job takes a few minutes; the fuzz job grows by 30 seconds with
each new target. A long run takes about three and a half hours.
[tasks/P1.md](tasks/P1.md) has the reasons for the push jobs, and the steps
for a task that plants a bug.

### Releases

Releases publish themselves, and nobody publishes one by hand.
[RELEASING.md](../RELEASING.md) has the whole procedure. A release of the
Beta goes out when `Version` in `hypercrux/hypercrux.go` goes up on main to
the form `X.Y.Z-beta.N`, above the last one, with its X.Y.Z above 0.x's
version, and the `test` and `beta` workflows have both passed on that
commit. The release workflow then builds the command in `cmd/hypercrux/`
without cgo for Linux on amd64 and arm64, each on its own architecture's
machine, checks each binary with `file`, `hypercrux version` and
`scripts/smoke.sh`, tags the commit `vX.Y.Z-beta.N` and publishes the two
archives with SHA256SUMS as a pre-release, so the repository's latest
release stays 0.x's. `0.0.0-beta` means unreleased, and releases nothing.

`scripts/release-plan.py` decides whether a run releases, and
`scripts/release-plan-test.py` runs its cases. `scripts/beta-release.sh` has
the checks, the archives, the checksums and the notes. A dry run, started by
hand with the version it rehearses, builds and checks the binaries and
writes the checksums and the notes, and publishes nothing; RELEASING.md says
how to start one and read it.

### Planted bugs

Tasks that change the code plant bugs in it on purpose, to show that their
tests can fail. A planted bug is a branch that checks `plant`, which is
`""` in an ordinary build, so the compiler drops it. A build with the
`hypercrux_planted` tag reads `HYPERCRUX_PLANT` instead, which names the one
bug to switch on. `plants.txt` lists each bug, with the packages whose tests
must catch it. `sh scripts/planted.sh` switches each on alone, runs those
tests, and fails if a bug's tests still pass, which shows a gap in them. The
planted job runs it on every push, and the long workflow on both
architectures. [tasks/P1.md](tasks/P1.md) has the steps for a task that
plants one.
