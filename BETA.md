# HyperCrux Beta: a plan for a new engine in C

**Status: a plan. Nothing in it is built.** HyperCrux 0.x stays what it is,
a Go library and command on SQLite, and keeps getting fixes. This document
describes a new engine for HyperCrux, written in C, with all four handles
built in, and what it would take to build. Every 0.1 measurement below
comes from [test/results](test/results). The Beta figures are targets, to be
measured the same way before anyone quotes them.

## What the Beta is for

HyperCrux 0.1 proved the idea: one record, reachable by key, SQL, links and
similarity, with all four kept in step by one transaction. It runs on SQLite
and keeps its rules in the file as triggers, which is why any program that
speaks SQLite and follows [FORMAT.md](FORMAT.md) can share a HyperCrux file
safely.

The Beta keeps the idea and replaces the machinery with something smaller
and faster, built for the way each handle actually reads. The cost is the
SQLite file, with everything that comes with it, from its tools to its
decades of testing.

## Where 0.1 spends its time

From the recorded benchmarks, on a two-core cloud machine, middle of three
runs:

| Operation | 0.1 | Where the time goes |
|---|---|---|
| Nearest 10 among 10,000 vectors of 384 values | 42 ms | About 4.2 µs a vector. SQLite needs about 1 µs to read a row (see below), which leaves about 3 µs for the driver handing each row and vector to Go and the Go loop that decodes each float; that split isn't timed |
| Nearest 10 among 100,000 vectors of 384 values | 0.41 s | The same cost per vector |
| The same, a tenth passing a filter | 0.13 s | The 90,000 rows that fail the filter cost about 1 µs each, which is SQLite reading and testing a row |
| Nearest 10 among 10,000 vectors of 1,536 values | 0.12 s | Each extra value adds about 7 ns. Rows this long spill onto an SQLite overflow page and each vector is copied into Go first, so the loop is only part of it |
| Get a record by key | 18 µs | One indexed SQL query through Go's database/sql; not timed in parts |
| Walk 1 and 3 links out, 100,000 records with 5 links each | 43 µs and 0.51 ms | A key check and a recursive SQL query through database/sql; not timed in parts |
| Put one record, committed | 0.35 ms | Mostly the commit, probably its sync, which any engine with the same guarantee pays; not timed in parts |
| Put records with 384-value vectors, 1,000 per transaction | 51 µs a record | Each key stored three times (the row, its primary-key index and hc_keys, the last by a trigger), trigger checks on the vector, and three statements a record through database/sql; not timed apart |

The 1 µs figure is derived: the filtered run compares as many vectors as
the 10,000-vector run, reads 90,000 more rows that fail the filter, and
takes about 86 ms longer.

## The idea

Start from what each handle needs:

- **Key:** find one record. A hash lookup does it.
- **SQL:** filter and sort fields. That's fastest when each field sits in a
  column that can be scanned in one pass.
- **Links:** follow references. That's fastest when each record's references
  sit in an array.
- **Similarity:** stream vectors. That's fastest when a table's vectors sit
  in one block.

Three of the four want their data as arrays in memory. A B-tree is the right
structure for data bigger than memory, and HyperCrux's data, on one machine,
from thousands to around a million records, fits in memory.

Two more observations finish the design. A link is a field that holds other
records' keys, and a vector is a field that holds numbers, so there's only
one thing to store: records with fields. And if every index is built from
the records, only the records have to survive a crash. For that, nothing is
simpler than an append-only log, where a crash can only cut off the end.

So the Beta has one durable truth, the log. Every index for every handle is
built from it, kept in a snapshot that processes share and a small delta in
each process. The four handles can't disagree, because they're all views of
the same point in the same log.

## What stays and what goes

**Stays:**

- The promise that the four handles never disagree. A record, its fields,
  its links and its vector change in one transaction, and a delete takes
  the record's links with it, in both directions.
- Exact search by default. A search compares every vector that passes the
  filter.
- Many processes sharing one database on one machine.
- The Go API and the command's verbs, so programs that stick to the Go API
  and the SQL subset move over with little change.
- The test philosophy: kill the writer at random, many processes on one
  database, a second language on the same database, every figure recorded.

**Goes:**

- The SQLite file. SQLite tools can't open a Beta database, and other
  languages reach it through bindings instead of their own SQLite driver.
- Rules in the file. The engine enforces them, which is safe because every
  write goes through the engine.
- Data bigger than memory. The Beta is built for data that fits; past that,
  it slows down sharply.
- Windows. The Beta runs on Linux and macOS. 0.x keeps its Windows build.
- Full SQL. The Beta's SQL is a defined subset, listed below. Missing from
  it, among others: JSON functions over fields, SQL on HyperCrux's own
  tables (links change through the API instead), declared column types,
  `UNIQUE` and `CHECK` constraints, upserts and `INSERT OR REPLACE`, and
  recursive queries over ordinary fields, since walks follow links only.
- The `adopt` command, and creating a database inside an existing SQLite
  file.
- Copying one file as a backup. A database is three files, and copying them
  by hand in the middle of a compaction can pair the wrong ones, so
  `hypercrux backup` writes a consistent copy.

**Bridges:** `hypercrux import` reads a 0.x file and writes a Beta
database; `hypercrux export` goes the other way. 0.x keeps working on its
own files for as long as anyone uses it.

## Design

### Records

A record is a key and its fields. The key is `table:id`, as in 0.x. A field
holds null, a whole number, a real number, text or bytes. Two kinds of field
carry the other handles:

- **The vector**, in the field `vec`, as in 0.x: up to 65,536 float32
  values, the same size across a table, set by the table's first vector.
- **Links**, named by their type. `customer:42` linking to `docs:7` with the
  type `owns` means `customer:42` has the link `owns` holding `docs:7`
  among its targets. Links have a namespace of their own: a link type is any
  text of 1 to 200 characters, as in 0.x, and never clashes with an ordinary
  field of the same name. A link must point at a record that exists. `Get`,
  `Scan` and `SELECT *` return ordinary fields only; links are read with
  `Neighbours` and walks.

Tables and fields appear when a record first uses them, as `Put` does in 0.x.

### Files

A Beta database is three files side by side:

| File | What it holds |
|---|---|
| `NAME` | The snapshot: every index, ready to read in place |
| `NAME-log-N` | The log of generation N: every change since that snapshot |
| `NAME-lock` | The write lock, and the generation and published end of the log |

The snapshot's header names its generation, and with it the one log that
goes with it, so opening the snapshot always finds the right log.

### The log

The log is the only file that changes in normal use, and it only grows. A
commit appends one batch: a length, the generation, a sequence number that
follows the one before, the changes (put fields, delete a record, add or
remove a link, create or drop a table), and a CRC32C checksum over all of
it. A batch counts only when its checksum matches, so a crash can only cut
off a batch that was being written.

A commit:

1. Takes the write lock (`flock` on the lock file, plus a mutex inside the
   process), waiting up to 10 seconds, as 0.x does, before failing.
2. Catches up with any batches it hasn't applied yet.
3. Checks the changes against the current state: link targets exist,
   vectors have the table's size, hold only finite values and aren't all
   zero, and keys and names are valid.
4. Appends the batch and syncs the log once.
5. Publishes the new end of the log in the lock file, then applies the batch
   to its own delta and releases the lock.

If the append or the sync fails, the writer cuts the log back to the
published end and syncs that before releasing the lock, and the commit
reports the error. If the cut fails too, the database stops taking writes
until it's reopened.

Readers only read up to the published end, so they never see a batch before
its sync has returned. Whoever takes the write lock first reads the log and
checks each batch in turn: its length fits, its sequence number follows the
one before, and its checksum, which also covers the generation, matches.
The first batch that fails ends the log. Complete batches past the
published end, left by a writer that died before publishing, are synced and
published, and everything from the first failed batch on is cut off. If
valid batches follow a failed one, that's damage in the middle of the log:
the database reports it and cuts nothing. Opening a database does the same
under the write lock, waiting for it if a writer holds it, so the published
end is rebuilt from the log and never trusted across a restart. Nothing is
cut without the write lock. A reader that meets a failed batch before the
published end, or finds that its snapshot and the lock file name different
generations, waits for the write lock and reads both again.

The log is read with ordinary reads, never mapped, so cutting off its tail
can't break a process that's reading it.

Each sync of a growing file also commits its new size, which can cost more
than overwriting space that's already there. Phase 1 measures it first. If
it's too slow, the log is grown in large steps ahead of time, and the
sequence numbers and checksums above mark where the real end is.

On macOS, a plain `fsync` doesn't make the drive flush its own cache. For
commits, the Beta does what SQLite does by default, a plain `fsync`, with a
full flush (`F_FULLFSYNC`) as an option. Compaction always uses the full
flush, as described below.

### Transactions

`Update` takes the write lock and catches up before running its function,
as 0.x's `BEGIN IMMEDIATE` does, so a read followed by a write inside it
can't lose another process's commit. Reads inside the function, SQL
included, see the function's own changes through a layer above the delta.
The function's changes become one batch when it returns, or vanish if it
fails. Each call outside `Update`, such as a single `Put`, is a transaction
of its own.

### The snapshot

The snapshot holds the whole database as of one point in the log, laid out
for reading in place. Every process maps it into memory, so they share one
copy through the operating system, and opening a database costs a map and a
read of the log since the snapshot. That matters for the `hypercrux`
command, where every call is a new process.

For each table, the snapshot holds:

- the keys, a hash table from key to row, and the rows in key order, for
  gets and prefix scans;
- one column per field, each cell a type tag and an 8-byte value, with
  text and bytes in a shared string area; a column stores only the rows
  that have the field, so a field few records use costs little;
- the vector block: every vector of the table back to back, in row order,
  starting 64-byte aligned and padded at the end, with the float64 norms in
  an array of their own;
- the links, as two arrays per table: each row's outgoing links (type,
  target) and each row's incoming links (type, source).

A header lists every section with its offset, length and CRC32C checksum.
Opening a database checks the header, and every offset read from the mapped
snapshot is bounds-checked. `check` reads every section.

### The delta

Each process keeps the changes since the snapshot in memory:

- the records that changed, with their fields, vectors and links, where a
  cleared field hides the snapshot's value;
- a mark on every snapshot row that a change replaced or deleted, which
  scans, searches and filters skip;
- the links added to and removed from records, indexed by target as well as
  by source, each holding its latest state, so a key deleted and created
  again starts with no links;
- the tables dropped and created since the snapshot, so a table created
  again doesn't see its old rows.

Reads look at the delta first, then the snapshot. The delta stays small
because compaction folds it into a new snapshot.

### Processes

One writer at a time per database, any number of readers, across processes.
The lock file is mapped into every process and holds the log's generation
and published end in one 64-bit word, written with a single atomic store, so
checking for new commits is one memory read, and a writer that dies
mid-update can't leave it half written. A process that finds new batches
reads and applies them before its next read starts. Inside a process, reads
share a lock on the delta and catching up takes it briefly on its own, so a
read always sees one consistent point in the log.

Readers in other processes never wait for the writer. The writer never
waits for readers; it can wait briefly while a process that's opening the
database checks the end of the log.

### Compaction

When the log passes a size limit, the writer, still holding the lock, moves
the database to the next generation:

1. It writes the new snapshot under a temporary name, from the current
   snapshot and its delta. Its header names generation N+1 and says it
   covers log N to its end. The writer creates the new, empty log N+1, syncs
   both files and syncs the directory.
2. It renames the new snapshot into place and syncs the directory. This
   rename is the switch: before it, the database is generation N; after it,
   N+1.
3. It publishes the new generation in the lock file.
4. It removes log N.

A crash before the rename leaves generation N whole, plus leftover files
that the next holder of the write lock removes before it does anything
else; only the lock holder ever removes them. A crash after the rename
leaves generation N+1 whole. A process that opened the old snapshot and
then can't find log N simply opens the snapshot again. Processes in the
middle of reading the old files keep reading them, since Linux and macOS
keep a removed or replaced file alive for as long as someone has it open or
mapped.

Every sync in a compaction is a full flush (`F_FULLFSYNC` on macOS), for
both files and both directory syncs, since losing a new snapshot after its
switch would lose the whole database. Compaction runs rarely, so the cost
is small.

Writers wait while compaction runs. Compacting before the log grows large
keeps those pauses short; building the next snapshot in the background is a
later improvement.

### Keys, fields and deletes

- `Get` is a lookup in the delta, then a lookup in the snapshot's hash table.
- `Put` merges the given fields into the record, as in 0.x.
- `Scan` by key prefix reads the snapshot's rows in key order and merges the
  delta's keys in.
- `Delete` removes the record and its links in both directions. The log
  stores only the delete; every process works out the same links to remove
  from the same state, so the batch stays small.
- `Drop` removes a table with its records and their links.

### Vectors

A search streams through the table's vector block, skipping rows the delta
replaced or deleted, then through the delta's vectors, computing one dot
product per vector and keeping the closest k in a heap.

- **Stored as given.** A vector comes back exactly as it went in. Its norm
  is kept beside it, and the distance is the dot product divided by the two
  norms, the formula 0.x uses.
- **Kernels.** AVX2 and AVX-512 on x86 and NEON on ARM, with a plain C loop
  as the fallback and the reference. Every kernel widens values to float64
  and adds them in one fixed order: 16 running sums, with value i going to
  sum i mod 16, and a fixed way of combining them at the end, which the
  plain C loop follows too. The question isn't scaled before the loop, and a
  float32 times a float32 is exact in float64, so fused multiply-add can't
  change the result. The C is built with `-ffp-contract=off` and never with
  `-ffast-math`, and a search clears denormal flushing in the calling thread
  and restores it after. Every kernel then returns the same bits as the
  reference.
- **Filters.** A filter becomes a bitmap of the rows it allows, built by
  scanning columns, and the search skips the rest.
- **Exact.** Every vector that passes the filter is compared. Zero vectors
  are refused, as in 0.x, and `distance()` treats zero vectors as 0.x does.

### Links

A walk is a breadth-first search over rows with a visited set, reading each
row's link arrays in the snapshot and merging the delta's added and removed
links. It never touches SQL or builds temporary tables. `Neighbours` reads
one row's arrays and merges the delta's links. Incoming links come from the
incoming arrays, so walking backwards costs the same as walking forwards.

### Queries and SQL

One small engine serves every handle. Its operators:

- scan a table's columns, skipping replaced rows, then the delta's rows;
- filter, a column at a time;
- walk, from a key, as a source of rows;
- nearest, over the rows a filter allows;
- join on equal values, with a hash table;
- group and aggregate;
- sort, top k, limit and offset.

The Go API calls them directly. SQL is parsed and planned onto them. The
Beta's SQL:

- `SELECT` with expressions and aliases, `FROM` one or more tables joined on
  equal values, `WHERE`, `GROUP BY` with `HAVING`, `ORDER BY`, `LIMIT` and
  `OFFSET`;
- `walk(key, depth [, type [, direction]])` as a table in `FROM`, and the
  0.x form `json_each(walk(...))` accepted unchanged, so the crux query runs
  as it does today;
- `distance(a, b)` and `vector('[...]')`. Rows without a vector sort first
  under `ORDER BY distance(...)`, as in SQLite, so the crux query's
  `vec IS NOT NULL` keeps its meaning; with that condition and a `LIMIT`,
  the planner uses a nearest search;
- `IN` lists and `IN (SELECT ...)`, `BETWEEN`, `LIKE`, `IS NULL`, `CASE` and
  the usual text and number functions;
- `date`, `time`, `datetime`, `julianday` and `strftime`, with `'now'` and
  SQLite's modifiers (plus or minus days, months and years, and the start of
  a day, month or year), giving SQLite's results, edge cases included;
- `INSERT`, `UPDATE` and `DELETE`, which go through the same checks as
  `Put` and `Delete`;
- `CREATE INDEX` and `DROP INDEX`, optional, for tables large enough that a
  sorted index beats a column scan.

Left for later: outer joins, `WITH` and recursive queries (walks cover what
0.x used them for), views, triggers and window functions.

### Bindings and tools

- A small, stable C API.
- The Go package, keeping 0.x's exported API apart from `Adopt`, `SQL()`
  and `ApplicationID`, which have no meaning without an SQLite file.
  `DriverName` names the new database/sql driver, which `Query` and
  `QueryRow` use as before.
- The `hypercrux` command with the same verbs apart from `adopt`, plus
  `import`, `export`, `compact` and `backup`. Only the command links SQLite,
  for import and export; the library doesn't.
- A Python binding over the C API, in place of `hcfile.py`'s direct SQL.
- `FORMAT.md` for the new format, as complete as 0.x's.

### Rules for the files

- Local file systems only. Network file systems such as NFS and SMB, and
  folders shared into containers through a virtual machine, don't keep the
  locks or the mappings honest.
- The lock file is created under a temporary name, sized, then linked into
  place, so no process ever maps it before it's complete. It's opened with
  `O_CLOEXEC`, so a child process can't keep the lock alive, and after
  taking the lock a writer checks that the file it locked is still the one
  in the folder.
- Snapshots are written with ordinary writes and never changed once
  they're in place. Overwriting a live database's files, with `cp` for
  example, can crash every process that has them mapped, which is what
  `backup` is for.
- A process opened read-only still takes the lock to check the end of the
  log, which `flock` allows on a file opened for reading. If the log needs
  repair, it reports that and leaves the repair to a process that can
  write.

## Targets

Same machine as 0.1's benchmarks, middle of three runs, recorded by
`scripts/record-tests.sh`. These are goals with a stated basis. None has
been measured.

| Operation | 0.1 recorded | Beta target | Basis |
|---|---|---|---|
| Nearest 10, 100,000 vectors of 384 values | 0.41 s | 50 ms | 154 MB of floats, streamed from memory on one core at about 3 GB/s |
| Nearest 10, 100,000 of 384, a tenth passing a filter on an unindexed field | 0.13 s | 10 ms | One column scan, then 15 MB of vectors |
| Nearest 10, 10,000 vectors of 1,536 values | 0.12 s | 20 ms | 61 MB of floats at the same rate |
| Get by key, from C / from Go | 18 µs (Go) | 1 µs / 2 µs | Two hash lookups in memory |
| Walk 1 link out / 3 links out, from Go | 43 µs / 0.51 ms | 3 µs / 40 µs | Arrays in memory, no SQL |
| Put with a 384-value vector, 1,000 per transaction | 51 µs | 5 µs | One append to the log, one update to the delta |
| Put one record, committed | 0.35 ms | no slower than 0.1 | One sync per commit |
| Open a database for one command | not recorded | 5 ms, plus the log since the snapshot | A map and a header check |

## Phases and gates

Each phase ends at a gate that decides whether the next one starts. Days
are estimated working days of development, explained under Time estimate
below.

| Phase | Work | Gate | Days |
|---|---|---|---|
| 0. Spec | The file format, the C API header, the SQL subset, the test plan | The spec is approved | 0.5 to 1 |
| 1. The log | Batches, checksums, recovery, the write lock, publishing, catching up across processes, the cost of syncing a growing file | A kill and a simulated power cut at every write of the log pass; many processes see every commit, in order; a committed put is no slower than 0.1 | 2 to 3 |
| 2. Snapshot and compaction | The mapped snapshot, the delta, generations, switching processes over | A cut at every write of a compaction leaves a database that opens at the last commit; processes switch without losing or repeating a commit; the open target | 3 to 4 |
| 3. Records and keys | The record model, the hash index, columns, `Put`, `Get`, `Delete`, `Scan`, `Drop`, transactions | Model tests against 0.x as the oracle; the get and batch put targets | 2 to 3 |
| 4. Vectors | Vector blocks, the kernels, filter bitmaps | Every kernel returns the same keys in the same order as the plain C reference, ties broken by key; the search targets | 2 to 3 |
| 5. Links | Link fields, the link arrays, walks, deletes that clear incoming links | 0.x's link tests pass; the walk targets | 1 to 2 |
| 6. Queries and SQL | The operators, the SQL subset, `walk()`, `distance()` | 0.x's SQL tests within the subset pass; the crux query gives 0.x's answers on random data, apart from distances within the stated bound | 4 to 6 |
| 7. Bindings and tools | The Go package, the command, the Python binding, import, export and backup | Every 0.x test of the Go API and the command within the Beta's scope passes through the Go package | 2 to 3 |
| 8. Review and release | Independent review, long fuzzing and crash runs, sanitizers, planted bugs, recorded benchmarks, docs, site | Released as a Beta for Linux and macOS | 2 to 3 |

## Time estimate

About 19 to 28 working days of development to reach the Beta gate, or
roughly four to six weeks of calendar time with a working session most days.
A first working version would take about a week, with every handle except
SQL: the log and an in-memory delta without a snapshot, the plain C search
loop, and the Go API, with searches unfiltered until SQL arrives. That's
enough to try the design and well short of the Beta's bar.

HyperCrux is developed in working sessions by Claude, an AI coding agent
made by Anthropic. The project's owner decides at each gate and publishes
the releases. The estimate is counted in those session days. Its one
yardstick is 0.1: about 5,400 lines of Go and tests, from the go-ahead to a
published release in about a day. The Beta is roughly 10,000 lines of C and
as many again in tests, close to four times 0.1's size. It also has to do
itself what 0.1 left to SQLite, from storage and crash safety to SQL, which
is why it takes weeks.

What would make it longer:

- Snapshot and compaction, the main risk. It's the one place where the
  files have to change together, and if the crash tests keep finding
  problems in the switch between generations, the phase could double.
- More SQL. Each feature past the subset, such as outer joins or `WITH`,
  adds time. A smaller first subset, with only the join to `walk()` that the
  crux query needs and no `GROUP BY`, would take a day or two off.
- Very large tables. Building a snapshot for millions of records without
  holding them twice in memory needs a streaming build, which isn't counted
  here.
- Machine time. Long fuzzing and crash-test runs take hours each, though
  most of it overlaps with other work.
- Fresh starts. Claude only remembers what's written down in the repository
  between sessions, so every session begins with some reading. The phases
  are sized so each fits in a week of sessions or less.

Passing every test at the Beta gate earns a release. Trust with real data
takes years of use. The estimate is written up for readers of the site in
[How Long a New HyperCrux Engine in C Would Take](https://hypercrux.com/how-long-a-new-hypercrux-engine-in-c-would-take-four-to-six-weeks-phase-by-phase/).

## Testing

Everything 0.x tests, and more:

- **Differential tests.** The same random workload on 0.x and on the Beta,
  through the same Go API, with every answer compared. 0.x on SQLite is the
  oracle. Distances agree within a stated bound, since 0.x adds in a
  different order, and results may differ only where two distances fall
  within it.
- **Crash safety.** HyperCrux's 200 SIGKILLs at random moments. A file layer
  for tests that can lose unsynced writes and tear the last one, with a cut
  at every write of the log and of a compaction. After each cut, the
  database opens at the last commit or the one under way, and `check`
  passes.
- **Many processes.** Writers and readers on one database at once, every
  reader seeing every commit in order, through compactions.
- **Exact results.** Every kernel against a brute-force search, on random
  vectors including awkward ones (very small, very large, nearly identical).
- **Fuzzing and sanitizers.** Damaged logs and snapshots, API call sequences
  and SQL, under AddressSanitizer, UBSan and ThreadSanitizer.
- **Planted bugs.** Every planted bug in the log, compaction, vectors and
  links has to be caught.

## Risks

| Risk | Why it matters | What limits it |
|---|---|---|
| Compaction | The one place where the files have to change together | Logs named by generation, one rename as the switch, full flushes, a cut at every write of a compaction |
| Memory | Data bigger than memory slows down sharply | The limit is documented, and `check` reports sizes |
| Pauses | Writers wait while a compaction runs | Compact early, while the log is small; build snapshots in the background later |
| A growing log | Each sync also commits a size change | Measured in Phase 1; grow the log ahead of time if needed |
| SQL scope | People expect all of SQLite's SQL | A documented subset, and export to 0.x for anything outside it |
| C memory safety | A memory bug can sit quietly for months | Sanitizers, fuzzing, planted bugs, the plain C kernel as reference |
| Upkeep | A storage engine needs care, and this project is meant to run with little | 0.x stays the stable line; the Beta lives apart until Gate 8 |
| macOS durability | A plain `fsync` there doesn't flush the drive's cache | The same default as SQLite for commits, with a full flush as an option; full flushes always in compaction |

## Left out of the Beta

- Data bigger than memory.
- Windows.
- Approximate vector indexes. They're the usual way to search tens of
  millions of vectors in milliseconds, and they can miss results.
- Speed-ups that can come later without changing the format: splitting one
  search across several cores, an 8-bit quick pass before the full vectors,
  building snapshots in the background, and letting several writers share
  one sync.
- Servers, replication and anything across machines.
- 32-bit systems.

## Open decisions

1. Whether to start Phase 0, the spec.
2. The first SQL subset: whether joins and `GROUP BY` are in from the start.
   The estimate assumes they are.
3. The files: three side by side, as above, or one folder holding them.
4. Where the Beta lives until Gate 8: a folder in this repository, a branch
   or a repository of its own.
5. Whether 0.x gets a smaller step meanwhile: a C search loop inside SQLite,
   using SIMD in float64 as 0.x does now, which keeps the file and every
   rule as they are. Its dot-product kernels could carry over to the Beta.
