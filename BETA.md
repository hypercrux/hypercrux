# HyperCrux Beta: a plan for a new engine in C

**Status: a plan. Nothing in it is built.** HyperCrux 0.x stays what it is,
a Go library and command on SQLite, and keeps getting fixes. This document
describes a new engine for HyperCrux, written in C, with all four handles
built in, and what it would take to build. Every 0.1 measurement below comes
from [test/results](test/results). The Beta figures are targets, to be
measured the same way before anyone quotes them.

## What the Beta is for

HyperCrux 0.1 proved the idea: one record, reachable by key, SQL, links and
similarity, with all four kept in step by one transaction. It runs on SQLite
and keeps its rules in the file as triggers, which is why any program that
speaks SQLite and follows [FORMAT.md](FORMAT.md) can share a HyperCrux file
safely.

The Beta keeps the idea and replaces the machinery with the smallest thing
that does the job: one file on disk and, in each process that opens it, one
copy of the data in memory. The cost is the SQLite file, with everything
that comes with it, from its tools to its decades of testing.

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

The 1 µs figure is derived: the filtered run compares as many vectors as the
10,000-vector run, reads 90,000 more rows that fail the filter, and takes
about 86 ms longer.

## The idea

Start from what each handle needs:

- **Key:** find one record. A hash lookup does it.
- **SQL:** filter and sort fields. At HyperCrux's sizes, checking every
  record in memory should be fast enough without indexes. The filtered
  search target below assumes about 50 ns a record.
- **Links:** follow references. That's fastest when each record's references
  sit in a list in memory.
- **Similarity:** stream vectors. That's fastest when a table's vectors sit
  in one block.

All four want the data in memory, and HyperCrux's data, on one machine, from
thousands to around a million records, fits.

Two more observations finish the design. A link is a field that holds other
records' keys, and a vector is a field that holds numbers, so there's only
one thing to store: records with fields. And if everything in memory is
built from the records, only the records have to survive a crash. For that,
nothing is simpler than an append-only log, where a crash can only cut off
the end.

So a Beta database is one file, an append-only log, and every process that
opens it reads it into its own copy in memory and keeps that copy up to date
as other processes add to the file. There's no second format on disk and
nothing to merge in memory. The four handles can't disagree, because they're
all views of the same point in the same log.

## What stays and what goes

**Stays:**

- The promise that the four handles never disagree. A record, its fields,
  its links and its vector change in one transaction, and a delete takes the
  record's links with it, in both directions.
- Exact search. A search compares every vector that passes the filter.
- Many processes sharing one database on one machine.
- One file, and a plain copy of it is a backup. Commits only add to the end,
  and compaction swaps in a finished file in one step, so a copy taken at
  any moment is a valid database up to some commit. A half-copied last batch
  is ignored on opening and cut off by the first writer.
- The Go API and the command's verbs, so programs that stick to the Go API
  and the SQL subset move over with little change.
- The test philosophy: kill the writer at random and run many processes on
  one database. Every figure gets recorded.

**Goes:**

- The SQLite file. SQLite tools can't open a Beta database, and other
  languages reach it through the `hypercrux` command, which reads and writes
  JSON, instead of their own SQLite driver.
- Rules in the file. The engine enforces them, which is safe because every
  write goes through the engine.
- Data bigger than memory. Every process that opens a database holds all of
  it in memory, and opening reads the whole file.
- Reading during an `Update` in the same process. In 0.x, readers never wait
  for a writer. In the Beta, the other threads in the writer's process wait
  until its `Update` ends, sync included, while other processes keep
  reading. A read through the database instead of the transaction inside an
  `Update`, which works in 0.x, returns an error.
- macOS and Windows, for now. The Beta runs on Linux, on both x86 and ARM.
  macOS comes after the Beta, and 0.x keeps its macOS and Windows builds.
- Full SQL. The Beta's SQL is a defined subset, listed below. Missing from
  it, among others: joins other than the one to `walk()`, subqueries other
  than over `walk()` or reading one record by key, `CASE`, JSON functions
  over fields, declared column types, `UNIQUE` and `CHECK` constraints,
  upserts and `INSERT OR REPLACE`, indexes, most date functions, and
  recursive queries over ordinary fields, since walks follow links only.
- The `adopt` command, and creating a database inside an existing SQLite
  file.

**Bridges:** 0.x gains `hypercrux export` and `hypercrux import`, which
write and read a database as JSON lines, and the Beta's `import` and
`export` use the same format, so data can move either way. Numbers are
written so they read back to the same bits. 0.x keeps working on its own
files for as long as anyone uses it.

## Design

### Records

A record is a key and its fields. The key is `table:id`, as in 0.x. A field
holds null, a whole number, a real number, text or bytes. Two kinds of field
carry the other handles:

- **The vector**, in the field `vec`, as in 0.x: up to 65,536 float32
  values, the same size across a table. The table's first vector sets the
  size, which stays until the table is dropped.
- **Links**, named by their type. `customer:42` linking to `docs:7` with the
  type `owns` means `customer:42` has the link `owns` holding `docs:7` among
  its targets. Links have a namespace of their own: a link type is any text
  of 1 to 200 characters, as in 0.x, and never clashes with an ordinary
  field of the same name. A link must point at a record that exists. `Get`,
  `Scan` and `SELECT *` return ordinary fields only; links are read with
  `Neighbours` and walks.

Tables and fields appear when a record first uses them, as `Put` does in
0.x. A table stays, even when it's empty, until it's dropped, and its fields
keep a fixed order, the order `SELECT *` shows them in.

### The file

A Beta database is one file. It starts with a header: a magic number, the
format version, a database ID chosen at random when the database is created,
and the generation. A file written by compaction also names the batch it
continues from, by generation, sequence number and checksum, and where its
compacted part ends. Then come batches.

A batch is one commit: a magic number, the generation, a length, a sequence
number that follows the one before, the changes, and a CRC32C checksum over
all of it. The magic number is never zero and the checksum uses CRC32C's
standard start and final values, so a run of zeros never passes as a batch.
The changes are:

- create a table, with its vector size and its fields in order;
- put fields;
- delete a record;
- add or remove a link;
- drop a table;
- a table's vectors as one block, which only compaction writes.

After a batch is synced, the writer adds a small marker naming the batch's
sequence number and checksum, with a check value of its own so that a
half-written marker never counts. A batch counts when its checksum matches.
Other processes apply it only once a marker naming that same checksum
follows it, or after syncing it themselves under the write lock (see
Recovery), so they never see a commit before it's on disk, or mistake a
batch for one that took its place after a failure.

### In memory

Opening a database reads the whole file into one in-memory copy:

- a hash table from key to record;
- for each record, its fields as a short list of field numbers and values,
  its links as a list of (type, target), and its place in the table's vector
  array;
- for each table, its keys, kept in byte order as they come and go, and its
  fields in their fixed order;
- for each table, its vector array: the vectors back to back, starting
  64-byte aligned and padded at the end, with their norms in float64 in an
  array of their own, and a mark on every free slot;
- a reverse index from each record to the links pointing at it, for incoming
  walks and deletes.

Every read goes to this one copy. A compacted file stores each table's
vectors as one block, so opening reads them straight into the array without
parsing. In each process the copy takes about 1.7 KB for a record with a
384-value vector, which comes to about 170 MB at 100,000 records and 1.7 GB
at a million, and nearly four times as much with 1,536 values, on top of the
page cache.

### Commits

A commit:

1. Takes the write lock: first a mutex inside the process, then `flock` on
   the database file, waiting up to 10 seconds in all before failing, as 0.x
   does, or longer while a compaction runs. It releases them in the reverse
   order. Once it holds both, it checks that the file it locked is still the
   one at the path, comparing device and inode numbers, and if a compaction
   or a restore has replaced it, starts again on the new file.
2. Runs the check described under Recovery, which also catches up with any
   batches it hasn't applied yet.
3. Makes the changes in its own copy, checking each against the current
   state: link targets exist, vectors have the table's size, hold only
   finite values and aren't all zero, and keys and names are valid. An undo
   list records how to reverse each change.
4. Writes the changes as one batch, syncs the file once, and writes the
   marker.
5. Drops the undo list and releases the locks.

If a check fails, the undo list puts the copy back and nothing reaches the
file. If the append, the sync or the marker fails, the writer makes the
batch unusable before releasing the lock: it zeroes everything from just
after the last marker on, or, if that fails, overwrites the batch's
checksum, and syncs. If neither works, the writer keeps the write lock and
refuses further writes, so no process can write until it closes the
database. Either way the undo list puts the copy back. A power cut can still
land between a failed sync and the zeroing, so an error from a commit means
its outcome is unknown.

Each sync of a growing file also commits its new size, which can cost more
than overwriting space that's already there. Phase 1 measures it first. If
it's too slow, the file is grown in large steps ahead of time. That works
because no process takes the file's size as the end of the log: each reads
on until a batch fails its checks or lacks its marker, and zeros fail at
once. The cost is one more small read whenever a process checks for new
commits.

On Linux, a sync returns only once the data is on the drive, and drives that
honour cache flushes keep it through a power cut. That one rule covers
commits and compaction alike.

### Transactions

`Update` takes the write lock and catches up before running its function, as
0.x's `BEGIN IMMEDIATE` does, so a read followed by a write inside it can't
lose another process's commit. The function's changes go straight into the
in-memory copy, with the undo list kept as they're made. When the function
returns, its changes become one batch, committed as above; if the function
fails, or the batch can't be written, the undo list puts the copy back. Each
call outside `Update`, such as a single `Put`, is a transaction of its own.

While an `Update` runs, the copy holds changes that aren't committed yet, so
the other threads in its process wait, readers included. Other processes
keep reading. A call through the database instead of the transaction inside
an `Update` returns an error, where it would otherwise wait forever on the
lock its own `Update` holds. In Go, `Update` keeps its goroutine on one OS
thread with `runtime.LockOSThread` while it runs, because a pthread lock has
to be released by the thread that took it. The error can only be raised on
that goroutine. A call from another goroutine, started by the function and
waited for, hangs instead.

### Recovery

Whoever takes the write lock checks the end of the file before it appends
anything. Each batch past the last marked one must start with the magic
number and carry the file's generation, a length that fits, a sequence
number that follows the one before, and a checksum that matches. The first
batch that fails ends the log. A header of zeros ends it straight away, so
an ordinary check never reads through a run of zeros. A complete batch
without its marker, left by a writer that died after its sync, is synced
again and marked, and a torn marker is rewritten in place. Everything from
the first failed batch on is zeroed and synced. If valid batches follow a
failed one, that's damage in the middle of the file: the database reports it
and cuts nothing. A header of zeros could hide them, so opening, which reads
the whole file anyway, looks past the end of the log for a marked batch, and
so does the check before it zeroes anything. Nothing is cut without the
write lock.

Opening a database reads every marked batch, checking each checksum on the
way, and only then tries the write lock, without waiting. If it gets the
lock, it runs the check above. If not, whoever holds the lock runs the check
before appending anything.

A process opened read-only still takes the lock, which `flock` allows on a
file opened for reading, and Linux lets it sync such a file too. Under the
lock, it syncs the file and applies a complete batch that lacks its marker,
without marking it. It leaves zeroing to a process that can write, and
reports damage in the middle of the file as an error.

A marked batch whose checksum fails can't come from a crash, since the
marker follows the sync. It's damage, or a read that raced a failed commit's
cut. The process reads it once more under the write lock, where nothing can
change it. If it still fails, opening fails, reads return a damage error,
and `check` names the batch.

### Other processes

One writer at a time per database, any number of readers, across processes.
Processes read the file with plain read calls.

Before each read, a process makes one `stat` call on the database's path. If
the path names a different file, a compaction or a restore has replaced it,
as described under Compaction. If the file reaches past the point the
process has applied, it reads on from there and applies each batch that's
complete and marked, stopping at the first that isn't; a header of zeros
means nothing new. It keeps nothing unmarked for next time. A complete batch
at the end without its marker is normally a commit under way. If the process
can take the write lock without waiting, though, its writer is gone, and the
process runs the check under Recovery, as on opening.

Inside a process, reads share a lock on the in-memory copy. Applying a batch
takes the lock alone and applies the whole batch or none of it, using an
undo list like `Update`'s. The lock lets an applier in ahead of readers that
arrive after it, so a steady stream of reads can't hold commits back, and a
read always sees one consistent point in the log.

### Compaction

When the file holds about twice as much as the live data, the writer
compacts it at the end of a commit, still holding the lock, so the old file
never changes again:

1. It creates `NAME.compact` beside the database, with the database's
   permissions and, where it's allowed to, its owner, and takes `flock` on
   it, so the new file is locked before anyone can see it.
2. It writes the current state into it, in the same batch format, with a
   header naming the database ID, generation N+1, the batch it continues
   from and where the compacted part ends. Each table starts with a change
   that creates it, with its vector size and its fields in order, so empty
   tables and vector sizes come through. Each table's vectors go in as one
   block, starting on a page boundary.
3. It syncs the new file.
4. It renames the new file to the database's path and syncs the directory.
   This rename is the switch: before it, the database is generation N; after
   it, N+1.
5. It moves over to the new file and releases both files' locks and then the
   mutex. A writer that was waiting on the old file gets that lock, sees
   that the path names a different file, and starts again there.

If writing or syncing the new file fails, because the disk is full for
example, or the rename fails, the writer removes `NAME.compact` and carries
on with the old file, which is still whole. The commit that set off the
compaction has already succeeded either way. If the directory sync after the
rename fails, nobody can tell which file a power cut would leave at the
path, so the writer keeps the write lock in the same way and no process can
write until it closes the database.

A crash before the rename leaves the old file whole, plus a leftover
`NAME.compact`. The next holder of the write lock removes it once it has
checked that the file it locked is still the one at the path, and nobody
else ever removes it. A crash after the rename leaves the new file whole.

A process that sees a new file at the path finishes reading the old one,
which no longer changes, then opens the new one. If the new file has the
same database ID and continues from the last batch the process applied,
matching its generation, sequence number and checksum, the process carries
on from where the compacted part ends, without reloading. Otherwise, as
after a restore, it reloads. Linux keeps a replaced file alive for as long
as someone has it open, so nothing is pulled from under a reader.

Writers wait while a compaction runs, including the one whose commit set it
off. It takes about as long as writing and syncing the live data once. The
pause grows with the live data, so compacting earlier would only make pauses
more frequent. If writing and syncing run at half a gigabyte a second, a
million records with 384-value vectors, about 1.6 GB, would hold writers up
for about three seconds; Phase 1 measures the real rate. A waiting writer
that finds `NAME.compact` locked keeps waiting past the usual 10 seconds,
until the compaction ends. Compacting in the background, listed under Left
out, would take most of the pause away without changing the format.

### Keys, fields and deletes

- `Get` is one hash lookup.
- `Put` merges the given fields into the record, as in 0.x.
- `Scan` by key prefix reads the table's keys in byte order. SQL scans
  tables in the same order, so a query with `LIMIT` and no `ORDER BY`
  returns the same rows in every process.
- `Delete` removes the record and its links in both directions. The file
  stores only the delete; every process works out the same links to remove
  from the same state, so the batch stays small.
- `Drop` removes a table with its records and their links, and its vector
  size goes with it.

### Vectors

A search streams through the table's vector array, skipping free slots and
rows a filter rules out, computing one dot product per vector and keeping
the closest k in a heap.

- **Stored as given.** A vector comes back exactly as it went in. Its norm
  is kept beside it, and the distance is the dot product divided by the two
  norms, the formula 0.x uses.
- **One kernel.** One portable C loop that widens values to float64 and adds
  them in 16 running sums, with value i going to sum i mod 16, and combines
  the sums in a fixed order at the end. GCC and Clang turn it into SIMD
  instructions without changing the order of the additions. The library is
  built with `-O3`, which GCC needs to keep the 16 sums in registers. On x86
  the same source is built for AVX2 and AVX-512 as well. When the library
  loads, it asks the CPU and the operating system what they support, with
  `cpuid` and `xgetbv`, and sets a function pointer to the fastest build,
  which works with musl, used by 0.x's static Linux releases, as well as
  with glibc. On ARM, the SIMD instructions are part of every 64-bit chip. A
  float32 times a float32 is exact in float64, so fused multiply-add can't
  change the result. The C is built with `-ffp-contract=off` and never with
  `-ffast-math`. All vector math, from norms and the not-all-zero check to
  searches and `distance()`, turns off denormal flushing in the calling
  thread and restores it after. Every build returns the same bits.
- **Exact.** Every vector that passes the filter is compared. Zero vectors
  are refused, as in 0.x, and `distance()` treats zero vectors as 0.x does.

### Links

A walk is a breadth-first search over records with a visited set, following
each record's link list forwards or the reverse index backwards. It never
touches SQL or builds temporary tables. `Neighbours` reads one record's
lists.

### Queries and SQL

A few operators serve every handle: scan a table, filter record by record,
walk, nearest, group and aggregate, sort, top k, limit and offset. The Go
API calls them directly, and SQL is parsed onto them. The Beta's SQL:

- `SELECT` with expressions and aliases, from one table, with `WHERE`,
  `GROUP BY` with `HAVING` and the usual aggregates, `ORDER BY`, `LIMIT` and
  `OFFSET`;
- `walk(key, depth [, type [, direction]])` as a table, either on its own,
  joined to a table on the key, or inside `IN (...)` or `NOT IN (...)`, with
  the 0.x forms `json_each(walk(...))` and `IN` or
  `NOT IN (SELECT value FROM json_each(walk(...)))` accepted unchanged, so
  the crux query and 0.x's filters on walks run as they do today;
- a subquery that reads one field of one record by its key, such as
  `(SELECT vec FROM photo WHERE key = ?)`, so a search can start from a
  stored vector;
- `distance(a, b)` and `vector('[...]')`. Rows without a vector sort first
  under `ORDER BY distance(...)`, as in SQLite, so the crux query's
  `vec IS NOT NULL` keeps its meaning; with that condition and a `LIMIT`,
  the planner uses a nearest search;
- `IN` and `NOT IN` lists, `BETWEEN`, `LIKE`, `IS NULL` and the usual text
  and number functions;
- `date` and `datetime` on `'now'` or a stored date written as `YYYY-MM-DD`,
  with or without a time, plus or minus days, months and years, giving
  SQLite's results, edge cases included;
- `INSERT`, `UPDATE` and `DELETE` on one table, which go through the same
  checks as `Put` and `Delete`.

Left for later: other joins and subqueries, `CASE`, indexes, `WITH` and
recursive queries (walks cover what 0.x used them for), views, triggers and
window functions.

### Bindings and tools

- A small, stable C API.
- The Go package, keeping 0.x's exported API apart from `Adopt`, `SQL()`,
  `ApplicationID` and `DriverName`, which have no meaning without SQLite.
  `Query` and `QueryRow` return the package's own `Rows` and `Row` types, so
  there's no driver to ship. They behave like database/sql's: `Next`,
  `Scan`, `Err`, `Columns` and `Close`, a `Row.Scan` that returns
  `sql.ErrNoRows` when nothing matched, and the same conversions into Go
  values, `sql.Scanner` types such as `sql.NullString` included. `Query`
  copies its results out before it returns, so open rows never hold anyone
  up.
- The `hypercrux` command with the same verbs apart from `adopt`, plus
  `import`, `export`, `compact` and `restore`. Other languages use it, as
  with 0.x, and each run opens the database afresh, reading the whole file.
- `FORMAT.md` for the new format, as complete as 0.x's.

### Rules for the file

- Local file systems only. Network file systems such as NFS and SMB, and
  folders shared into containers through a virtual machine, don't keep the
  locks honest.
- A database is opened by its real path, with symbolic links resolved, and a
  file with more than one hard link is refused, because compaction can only
  replace the file at one path.
- The file is opened with `O_CLOEXEC`, which covers exec only. Each handle
  remembers which process opened it, and in a child made by `fork`, every
  call returns an error. A `pthread_atfork` handler closes the child's
  copies of the files straight away, without unlocking, so a child can't
  keep a dead parent's lock alive or release a live parent's.
- To back up, copy the file somewhere else. To restore, use
  `hypercrux restore`, which writes the copy beside the database with its
  permissions and, where allowed, its owner, syncs it, renames it into place
  under the write lock and syncs the directory, as compaction does. Don't
  copy anything over a live database.

## Targets

Measured the same way as 0.1's: on a two-core cloud machine, middle of three
runs, recorded by `scripts/record-tests.sh`. Cloud machines vary from one
run to the next, so 0.1's benchmarks run again alongside the Beta's and the
two are compared from the same run. These are goals with a stated basis.
None has been measured.

| Operation | 0.1 recorded | Beta target | Basis |
|---|---|---|---|
| Nearest 10, 100,000 vectors of 384 values | 0.41 s | 50 ms | 154 MB of floats, streamed from memory on one core at about 3 GB/s |
| Nearest 10, 100,000 of 384, a tenth passing a filter on an unindexed field | 0.13 s | 10 ms | Checking 100,000 records at about 50 ns each, then 15 MB of vectors |
| Nearest 10, 10,000 vectors of 1,536 values | 0.12 s | 20 ms | 61 MB of floats at the same rate |
| Get by key, from C / from Go | 18 µs (Go) | 2 µs / 3 µs | One hash lookup and one `stat` call to check for new commits |
| Walk 1 link out / 3 links out, from Go | 43 µs / 0.51 ms | 3 µs / 40 µs | Lists in memory, no SQL |
| Put with a 384-value vector, 1,000 per transaction | 51 µs | 5 µs | An append and an update in memory for each record, and one sync of about 1.6 MB for every 1,000 |
| Put one record, committed | 0.35 ms | no slower than 0.1 | One sync per commit |
| Open a compacted database of 100,000 records with 384-value vectors | not recorded | 200 ms | Reading about 170 MB from the page cache into fresh memory, where page faults cost the most, and checking its checksums; vectors without parsing; then building 100,000 records |

## Phases and gates

Each phase ends at a gate that decides whether the next one starts. Days are
estimated working days of development, explained under Time estimate below.

| Phase | Work | Gate | Days |
|---|---|---|---|
| 0. Spec | The file format, with vector blocks on page boundaries; the C API header; the SQL subset; the test plan | The spec is approved | 0.5 to 1 |
| 1. The file | Batches, markers, recovery, the lock, read-only openers, following other processes' commits, compaction and its failures, restores, the cost of syncing a growing file | Every crash and failure test passes: a kill or a simulated power cut at every write, and a failure at every file call, in commits and compactions alike; many processes see every commit, in order, through compactions; a committed put is no slower than 0.1 | 3 to 4 |
| 2. Records, keys and links | The in-memory copy, `Put`, `Get`, `Delete`, `Scan`, `Drop`, links, walks, transactions | Model tests against 0.x as the oracle, vectors aside; 0.x's link tests pass; the get and walk targets | 2 to 3 |
| 3. Vectors | Vector arrays, the kernel and its builds, searches without filters | Every build returns the same keys in the same order as the plain C build, ties broken by key; vectors join the model tests; the unfiltered search, batch put and open targets | 1 to 2 |
| 4. Queries and SQL | The operators, the SQL subset, `walk()`, `distance()`, filtered searches | 0.x's SQL tests within the subset pass; the crux query gives 0.x's answers on random data, apart from distances within the stated bound; the filtered search target | 2 to 3 |
| 5. The Go package and the command | The Go API with its `Rows` and `Row` types, the command, and import and export in the Beta and in 0.x | Every 0.x test of the Go API and the command within the Beta's scope passes through the Go package; data goes from 0.x to the Beta and back unchanged | 2.5 to 3.5 |
| 6. Review and release | Independent review, long fuzzing and crash runs, sanitizers, planted bugs, recorded benchmarks, docs, site | Released as a Beta for Linux on x86 and ARM | 1.5 to 2.5 |

## Time estimate

About 13 to 19 working days of development to reach the Beta gate, or
roughly three weeks of calendar time with a working session most days.

A first working version would take four or five days, with every handle
except SQL: the file without compaction, the in-memory copy, the plain C
search loop, and the Go API, with searches unfiltered until SQL arrives. It
skips the tests the gates ask for, which is why it comes in under the first
phases' sums. That's enough to try the design and well short of the Beta's
bar.

HyperCrux is developed in working sessions by Claude, an AI coding agent
made by Anthropic. The project's owner decides at each gate and publishes
the releases. The estimate is counted in those session days. Its one
yardstick is 0.1: about 5,400 lines of Go and tests, from the go-ahead to a
published release in about a day. The Beta is roughly 5,000 to 6,000 lines
of C and as many again in tests, about twice 0.1's size, plus a reworked Go
package. At 0.1's pace, size alone would mean a few days. The Beta also has
to do itself what 0.1 left to SQLite, from storage and crash safety to SQL,
and code that has to survive a crash or a failed write at any moment takes
many rounds of testing and fixing before it passes. That's why it takes
weeks.

What would make it longer:

- The file, the main risk. Recovery and compaction are where data loss would
  come from, and if the crash tests keep finding problems there, the phase
  could double.
- More SQL. Each feature past the subset, such as other joins or `CASE`,
  adds time. A first subset without `GROUP BY` would take a day off.
- Large databases. Opening time and memory grow with the data, and so do
  compaction pauses. Keeping them reasonable near a million records may need
  work that isn't counted here.
- Machine time. Long fuzzing and crash-test runs take hours each, though
  most of it overlaps with other work.
- Fresh starts. Claude only remembers what's written down in the repository
  between sessions, so every session begins with some reading. The phases
  are sized so each fits in a week of sessions or less.

Passing every test at the Beta gate earns a release. Trust with real data
takes years of use. The estimate is written up for readers of the site in
[How Long a New HyperCrux Engine in C Would Take](https://hypercrux.com/how-long-a-new-hypercrux-engine-in-c-would-take-about-three-weeks-phase-by-phase/).

## Testing

Everything 0.x tests, and more, on x86 and ARM Linux alike:

- **Differential tests.** The same random workload on 0.x and on the Beta,
  through the same Go API, with every answer compared. 0.x on SQLite is the
  oracle. Distances agree within a stated bound, since 0.x adds in a
  different order, and results may differ only where two distances fall
  within it.
- **Crash safety and failures.** HyperCrux's 200 SIGKILLs at random moments.
  A file layer for tests that can lose unsynced writes or tear the last one,
  and can make any file call fail, used for a simulated power cut at every
  write and a failure at every call in commits and compactions. After each,
  the database opens at the last commit that succeeded or at the one under
  way, and so does a copy taken in the middle of a commit.
- **Many processes.** Writers and readers on one database at once, read-only
  ones among them, every reader seeing every commit in order, through
  compactions and restores. A forked child's calls fail, and the parent's
  lock is left alone.
- **Exact results.** Every build of the kernel against a brute-force search,
  on random vectors including awkward ones (very small, very large, nearly
  identical).
- **Fuzzing and sanitizers.** Damaged files, API call sequences and SQL,
  under AddressSanitizer, UBSan and ThreadSanitizer.
- **Planted bugs.** Every planted bug in commits, recovery, compaction,
  vectors and links has to be caught.

## Risks

| Risk | Why it matters | What limits it |
|---|---|---|
| Recovery and compaction | Where data loss would come from | Checksums and sequence numbers on every batch, markers after syncs, one rename as the switch, and tests with a simulated power cut at every write and a failure at every file call |
| Memory | Every process holds the whole database, about 1.7 GB at a million records with 384-value vectors | The limit is documented, and `check` reports sizes |
| Opening time | Opening reads the whole file, and so does every run of the command | Vectors load without parsing; heavy scripting at large sizes belongs in the Go library |
| Pauses | Writers wait while a compaction runs, for longer as the data grows | Writers wait out a running compaction instead of failing; compacting in the background can come later |
| A growing file | Each sync also commits a size change | Measured in Phase 1; grow the file ahead of time if needed |
| SQL scope | People expect all of SQLite's SQL | A documented subset; anything outside it can go back to 0.x through `export` and `import` |
| C memory safety | A memory bug can sit quietly for months | Sanitizers, fuzzing, planted bugs, the plain C build as reference |
| Upkeep | A storage engine needs care, and this project is meant to run with little | 0.x stays the stable line; the Beta lives apart until Gate 6 |

## Left out of the Beta

- Data bigger than memory.
- macOS, until after the Beta, and Windows. Apart from syncing and growing
  the file, the calls the code makes exist on macOS too, so the port should
  be small: those calls, plus a test runner and release builds.
- Approximate vector indexes. They're the usual way to search tens of
  millions of vectors in milliseconds, and they can miss results.
- Columns and indexes. Checking records one by one should be fast enough at
  HyperCrux's sizes; both can come later.
- A Python binding. Other languages use the command, as with 0.x.
- Speed-ups that can come later without changing the format: splitting one
  search across several cores, an 8-bit quick pass before the full vectors,
  compacting in the background, letting several writers share one sync, and
  mapping vector blocks, which start on page boundaries for this, so
  processes share them and open faster.
- Servers, replication and anything across machines.
- 32-bit systems.

## Open decisions

1. Whether to start Phase 0, the spec.
2. Whether `GROUP BY` is in the first SQL subset. The estimate assumes it
   is.
3. Where the Beta lives until Gate 6: a folder in this repository, a branch
   or a repository of its own.
4. Whether 0.x gets a smaller step meanwhile: a C search loop inside SQLite
   that uses SIMD and, like 0.x today, adds in float64, which keeps the file
   and every rule as they are. Its kernel could carry over to the Beta.
