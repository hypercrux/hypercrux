# HyperCrux Beta: a plan for a new engine in Go

**Status: a plan. Nothing in it is built.** HyperCrux 0.x stays what it is,
a Go library and command on SQLite, and keeps getting fixes. This document
describes a new engine for HyperCrux, written in Go, with all four handles
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
copy of the data in memory. It's written in Go, like 0.1, with no SQLite and
no C underneath. The cost is the SQLite file, with everything that comes
with it, from its tools to its decades of testing.

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
- cgo and the C compiler. 0.x builds SQLite from C; the Beta builds with the
  Go toolchain alone.
- Data bigger than memory. Every process that opens a database holds all of
  it in memory, and opening reads the whole file.
- Reading while an `Update` is changing data in the same process. In 0.x,
  readers never wait for a writer. In the Beta, the other goroutines in the
  writer's process wait from the `Update`'s first change until it commits,
  while other processes keep reading. A read through the database instead of
  the transaction, made inside an `Update` after its first change, returns
  an error; in 0.x it works.
- `VACUUM` and `VACUUM INTO`. `Compact()` or `hypercrux compact` rewrites
  the file with only the live data, which also takes deleted data off the
  disk, and a plain copy is a backup.
- macOS and Windows, for now. The Beta runs on Linux, on both x86 and ARM.
  macOS comes after the Beta, and 0.x keeps its macOS and Windows builds.
- Full SQL. The Beta's SQL is a defined subset, listed below. Missing from
  it, among others: `GROUP BY` and `HAVING`, joins other than the one to
  `walk()`, subqueries other than over `walk()` or reading one record by
  key, `CASE`, JSON functions over fields, declared column types, `UNIQUE`
  and `CHECK` constraints, upserts and `INSERT OR REPLACE`, indexes, date
  functions other than `date` and `datetime` on `'now'`, and recursive
  queries over ordinary fields, since walks follow links only.
- Transactions started with `SQL().Begin`. Transactions go through `Update`.
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
compacted part ends. The Beta doesn't use those last fields yet, since other
processes reload after a compaction, but they let a later version carry on
without reloading and without a format change. Then come batches.

A batch is one commit: a magic number, the generation, a length, a sequence
number that follows the one before, the changes, and a CRC32C checksum over
all of it, which Go's `hash/crc32` computes with the processor's own
instruction on x86 and ARM. The magic number is never zero and the checksum
uses CRC32C's standard start and final values, so a run of zeros never
passes as a batch. The changes are:

- create a table, with its vector size and its fields in order;
- put fields;
- delete a record;
- add or remove a link;
- drop a table.

After a batch is synced, the writer adds a small marker naming the batch's
sequence number and checksum, with a check value of its own so that a
half-written marker never counts. A batch counts when its checksum matches.
Other processes apply it only once a marker naming that same checksum
follows it, so they never see a commit before it's on disk, or mistake a
batch for one that took its place after a failure.

### In memory

Opening a database reads the whole file into one in-memory copy:

- a hash table from key to record;
- for each record, its fields as a short list of field numbers and values,
  its links as a list of (type, target), and its place in the table's vector
  array;
- for each table, its keys, kept in byte order as they come and go, and its
  fields in their fixed order;
- for each table, its vector array: the vectors back to back in one block
  that holds no pointers, so Go's garbage collector never has to scan it,
  with their norms in float64 in an array of their own and a mark on every
  free slot;
- a reverse index from each record to the links pointing at it, for incoming
  walks and deletes.

Every read goes to this one copy. In each process the copy takes about 1.7
KB for a record with a 384-value vector, which comes to about 170 MB at
100,000 records and 1.7 GB at a million, and nearly four times as much with
1,536 values, on top of the page cache. Go's collector can let the heap grow
to twice the live data between collections; setting `GOMEMLIMIT` a little
above the live size keeps it close to that.

### Commits

A commit:

1. Takes the write lock: first a mutex inside the process, then `flock` on
   the database file, waiting up to 10 seconds in all before failing, as 0.x
   does, or longer while a compaction runs. It releases them in the reverse
   order. Once it holds both, it checks that the file it locked is still the
   one at the path, comparing device and inode numbers, and if a compaction
   or a backup moved into place has replaced it, starts again on the new
   file.
2. Runs the check described under Recovery, which also catches up with any
   batches it hasn't applied yet.
3. Makes the changes in its own copy, checking each against the current
   state: link targets exist, vectors have the table's size, hold only
   finite values and aren't all zero, and keys and names are valid. The
   first change takes the copy's lock for itself, and an undo list records
   how to reverse each change.
4. Writes the changes as one batch, syncs the file once, and writes the
   marker.
5. Drops the undo list and releases the locks.

If a check fails, the undo list puts the copy back and nothing reaches the
file. If the append, the sync or the marker fails, the writer makes the
batch unusable before releasing the lock: it cuts the file back to just
after the last marker and syncs. If that fails, the writer keeps the write
lock and refuses further writes, so no process can write until it closes the
database. Either way the undo list puts the copy back. A power cut can still
land between a failed sync and the cut, so an error from a commit means its
outcome is unknown.

Each sync of a growing file also commits its new size, which can cost more
than overwriting space that's already there. Task F7 measures it. Reserving
space ahead with `fallocate` wouldn't help, since on ext4 and XFS the first
write into that space still needs a journal commit. Writing zeros ahead of
the log would, but then readers couldn't trust the file's size to show where
the log ends, so it waits until after the Beta and comes only if the
measurement calls for it.

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

Until its first change, an `Update` holds up no reader: other goroutines in
its process read as normal, so work done first, such as computing an
embedding, keeps nobody waiting. From the first change until the commit, the
copy holds changes that aren't committed yet, so those readers wait. Other
processes keep reading throughout.

Inside an `Update`, a call through the database instead of the transaction
would sometimes have to wait for that same `Update`: any write, and any read
after the first change. Such a call returns an error at once. `Update` notes
the ID of the goroutine running it, which Go only shows in the first line of
`runtime.Stack`, and a call that has to wait checks it. A call from another
goroutine that the function starts and then waits for can't be caught that
way, and hangs.

### Recovery

Whoever takes the write lock checks the end of the file before it appends
anything. Each batch past the last marked one must start with the magic
number and carry the file's generation, a length that fits, a sequence
number that follows the one before, and a checksum that matches. The first
batch that fails ends the log. A header of zeros ends it straight away, so
an ordinary check never reads through a run of zeros.

A complete batch without its marker, left by a writer that died after its
sync, is written again in place, synced and marked, and a torn marker is
rewritten in place. The batch is written again because after a failed sync
Linux can mark its pages clean, and a sync on its own could then report
success without the batch ever reaching the disk.

Everything from the first failed batch on is cut off, and the cut is synced.
If valid batches follow a failed one, that's damage in the middle of the
file: the database reports it and cuts nothing. A header of zeros could hide
them, so opening, which reads the whole file anyway, looks past the end of
the log for a marked batch, and so does the check before it cuts anything.
Nothing is cut without the write lock.

Opening a database reads every marked batch, checking each checksum on the
way, and only then tries the write lock, without waiting. If it gets the
lock, it runs the check above. If not, whoever holds the lock runs the check
before appending anything.

A marked batch whose checksum fails can't come from a crash, since the
marker follows the sync. It's damage, or a read that raced a failed commit's
cut. The process reads it once more under the write lock, where nothing can
change it. If it still fails, opening fails, reads return a damage error,
and `check` names the batch.

### Other processes

One writer at a time per database, any number of readers, across processes.
Processes read the file with plain read calls.

Before each read, a process makes one `stat` call on the database's path. If
the path names a different file, a compaction or a backup moved into place
has replaced it, and the process reloads, as described under Compaction. If
the file is longer than the point the process has applied, it reads on from
there and applies each batch that's complete and marked, stopping at the
first that isn't. It keeps nothing unmarked for next time. A complete batch
at the end without its marker is normally a commit under way. If the process
can take the write lock without waiting, though, its writer is gone, and the
process runs the check under Recovery, as on opening.

Inside a process, reads share a lock on the in-memory copy. Applying a batch
takes the lock alone and applies the whole batch or none of it, using an
undo list like `Update`'s. Go's read-write lock lets a waiting writer in
ahead of readers that arrive after it, so a steady stream of reads can't
hold commits back, and a read always sees one consistent point in the log.

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
   tables and vector sizes come through. Vectors go in as ordinary fields of
   their records.
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

A process that finds a different file at the path reloads from it. It drops
its old copy and runs a garbage collection before reading the new file, so
memory doesn't double, and reads in that process wait meanwhile, about as
long as opening takes. The same happens when someone moves a backup into
place. Linux keeps a replaced file alive for as long as someone has it open,
so a read already under way finishes on the old file.

Writers wait while a compaction runs, including the one whose commit set it
off. It takes about as long as writing and syncing the live data once, and
the pause grows with the live data, so compacting earlier would only make
pauses more frequent. If writing and syncing run at half a gigabyte a
second, a million records with 384-value vectors, about 1.6 GB, would hold
writers up for about three seconds; task F8 measures the real rate. A
waiting writer that finds `NAME.compact` locked keeps waiting past the usual
10 seconds, until the compaction ends. Compacting in the background would
take most of the pause away later without changing the format.

`Compact()` in Go and `hypercrux compact` run a compaction on demand. A
compacted file holds only live data, so that's also how deleted data leaves
the disk, the job `VACUUM` does in 0.x.

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
  is kept beside it, and the distance is one minus the dot product divided
  by the two norms, kept between 0 and 2, the formula 0.x uses.
- **One loop.** One plain Go loop that widens values to float64 and adds
  them in 8 running sums, with value i going to sum i mod 8, and combines
  the sums in a fixed order at the end. Go's compiler doesn't use SIMD
  instructions, and the 8 sums let the processor work on several additions
  at once; in a quick check, 8 sums ran faster than 16. Go doesn't reorder
  floating-point additions or flush denormals. It may fuse a multiply and an
  add, on ARM for example, but a float32 times a float32 is exact in
  float64, so fusing can't change the result. amd64 and arm64 return the
  same bits, and so would SIMD code added later, as long as it keeps the 8
  sums.
- **Exact.** Every vector that passes the filter is compared. Zero vectors
  are refused, as in 0.x, and `distance()` treats zero vectors as 0.x does.

### Links

A walk is a breadth-first search over records with a visited set, following
each record's link list forwards or the reverse index backwards. It never
touches SQL or builds temporary tables. `Neighbours` reads one record's
lists.

### Queries and SQL

A few operators serve every handle: scan a table, filter record by record,
walk, nearest, aggregate, sort, top k, limit and offset. The Go API calls
them directly, and SQL is parsed onto them. The Beta's SQL:

- `SELECT` with expressions and aliases, from one table, with `WHERE`,
  `ORDER BY`, `LIMIT` and `OFFSET`, and the usual aggregates over the whole
  result;
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
- `IN` and `NOT IN` lists, `BETWEEN`, `LIKE`, `IS NULL` and the text and
  number functions on a list fixed in task P5;
- `date` and `datetime` on `'now'`, plus or minus days, months and years,
  giving SQLite's results, month ends included;
- `INSERT`, `UPDATE` and `DELETE` on one table, which go through the same
  checks as `Put` and `Delete`.

Left for later: `GROUP BY` and `HAVING`, `date` and `datetime` on a stored
date, other joins and subqueries, `CASE`, indexes, `WITH` and recursive
queries (walks cover what 0.x used them for), views, triggers and window
functions.

### The Go package and the command

- The engine is the Go package. It keeps 0.x's exported API apart from
  `Adopt`, `ApplicationID` and `DriverName`, which have no meaning without
  SQLite.
- `Query`, `QueryRow`, `Exec` and `SQL()` go through a small private
  database/sql driver, so they return `*sql.Rows`, `*sql.Row`, `sql.Result`
  and `*sql.DB` exactly as in 0.x, and database/sql does every conversion
  into Go values, `sql.Scanner` types included. The transaction travels in
  the query's context, so `tx.Query` runs inside its `Update`. The driver
  copies results out before `Query` returns, so open rows never hold anyone
  up. `SQL().Begin` returns an error, since transactions go through
  `Update`.
- `Compact()` runs a compaction on demand.
- The `hypercrux` command with the same verbs apart from `adopt`, plus
  `import`, `export` and `compact`. Other languages use it, as with 0.x, and
  each run opens the database afresh, reading the whole file.
- `FORMAT.md` for the new format, as complete as 0.x's.

### Rules for the file

- Local file systems only. Network file systems such as NFS and SMB, and
  folders shared into containers through a virtual machine, don't keep the
  locks honest.
- A database is opened by its real path, with symbolic links resolved, and a
  file with more than one hard link is refused, because compaction can only
  replace the file at one path.
- Go opens files with `O_CLOEXEC` and can't fork without exec, so no child
  process can end up holding the lock.
- To back up, copy the file somewhere else. To restore, stop the processes
  using the database and copy the backup in, or copy it beside the database
  and move it into place with `mv`. Running processes notice the new file
  and reload. A commit that races the move is lost along with everything
  after the backup, and the file's owner and the directory sync are up to
  whoever moves it. Don't copy anything over a live database.

## Targets

Measured the same way as 0.1's: on a two-core cloud machine, middle of three
runs, recorded by `scripts/record-tests.sh`. Cloud machines vary from one
run to the next, so 0.1's benchmarks run again alongside the Beta's and the
two are compared from the same run. These are goals with a stated basis.
None has been measured.

| Operation | 0.1 recorded | Beta target | Basis |
|---|---|---|---|
| Nearest 10, 100,000 vectors of 384 values | 0.41 s | 65 ms | 154 MB of floats through the plain Go loop at about 2.4 GB/s on one core, about what a quick check measured with the heap and the norms included |
| Nearest 10, 100,000 of 384, a tenth passing a filter on an unindexed field | 0.13 s | 12 ms | Checking 100,000 records at about 50 ns each, then 15 MB of vectors at the same rate |
| Nearest 10, 10,000 vectors of 1,536 values | 0.12 s | 26 ms | 61 MB of floats at the same rate |
| Get by key | 18 µs | 3 µs | One hash lookup and one `stat` call to check for new commits |
| Walk 1 link out / 3 links out | 43 µs / 0.51 ms | 3 µs / 40 µs | Lists in memory, no SQL |
| Put with a 384-value vector, 1,000 per transaction | 51 µs | 5 µs | An append and an update in memory for each record, and one sync of about 1.6 MB for every 1,000 |
| Put one record, committed | 0.35 ms | no slower than 0.1 | One sync per commit |
| Open a compacted database of 100,000 records with 384-value vectors | not recorded | 250 ms | Reading about 160 MB from the page cache into fresh memory, checking its checksums with the processor's CRC32C instruction, computing the vectors' norms and building 100,000 records. Other processes take about as long to reload after a compaction |

## The work, task by task

The Beta lives in a `beta/` folder in this repository, beside 0.x, which
stays as it is. The folder builds only on Linux, so 0.x's macOS and Windows
builds and its CI carry on unchanged.

The work is 48 tasks of two to five hours, plus one that runs only if a
target needs it. Each fits in one working session and closes with its own
automatic test, which becomes a CI job on amd64 and arm64. Hours are hours
of Claude's working time, and a working day is about 8 of them for each
agent.

Three thin slices run the whole stack early. G1 puts and gets a record
through the file from Go, G2 adds every handle but SQL and is the first
working version, and G4 adds SQL. One agent takes the file tasks F1, F2, F3,
F5, F8 and F9 in order, since recovery, failed commits and compaction share
their rules.

The five tasks marked A don't depend on the Beta and help 0.x whatever is
decided, so they can start before the go-ahead. The rest wait for it.

| ID | Task | Needs | Done when | Hours |
|---|---|---|---|---|
| A1 | Test suites from 0.x: its tests that don't depend on SQLite internals, in a Go suite that runs on either engine through a thin adapter, and its command tests run against any binary named in an environment variable | none | Both pass against 0.1 unchanged | 4 |
| A2 | Export and import for 0.x: JSON lines with each table's vector size and field order, links, and numbers that read back to the same bits | none | Random files with awkward values (subnormals, -0, extremes, bytes, Unicode) go through export, import and export byte for byte | 4 |
| A3 | Differential harness: random sequences of operations run on two engines and compared, distances within the stated bound, failing sequences kept | A1 | 0.x against itself passes, and a deliberately broken 0.x fails | 3 |
| A4 | SQL corpus: statements inside and outside the subset, a few thousand generated expressions and date cases, with answers recorded through 0.x | none | It replays clean on 0.x twice | 3 |
| A5 | The search loop: 8 running sums in plain Go | none | amd64 and arm64 return the same bits on random and awkward vectors | 2 |
| P1 | The `beta/` folder beside 0.x, its build, and CI on amd64 and arm64 with the race detector, fuzzing and planted-bug jobs; the task board | A3 | A trivial test passes in every job, and 0.x's own tests and builds pass unchanged | 2 |
| P2 | File format, byte by byte, with golden fixtures for the header, each change, the batch, the marker and the compaction header | none | The fixtures check out with Go's CRC32C, and a review agent passes the spec against this plan | 3 |
| P3 | Internal interfaces: the file calls, so a fault layer can sit under them; the change list; the snapshot compaction reads; the store's read API; the value type; the operator iterator; error values | P2 | It compiles against stubs | 2 |
| P4 | Package skeleton: 0.x's exported API as stubs | P3 | A1's adapter compiles against it | 2 |
| P5 | SQL subset spec: the grammar, the list of functions, value rules, and the named 0.x tests the Beta must pass, settling small 0.x behaviours such as field names that match regardless of case, `SELECT *` order, comparisons on untyped fields and the scalar `walk()` form | A1, A4 | Every corpus statement is marked in or out, and every named test exists | 2 |
| T1 | Fault layer for data: unsynced writes lost or torn at a simulated power cut, and any call made to fail at its nth use | P3 | Its own tests show synced data surviving every cut | 3 |
| T2 | Fault layer for names: create, rename, remove and directory sync, each kept or lost at a cut | T1 | A rename without a directory sync can vanish and one with it can't | 2 |
| T3 | Crash-point driver: a cut at every write and a failure at every call, then a reopen compared with the model, plus a copy taken mid-commit | T1, P1 | It catches a planted "marker before sync" bug in a toy log | 3 |
| T4 | Many-process harness: writer and reader processes, killed at random | P4, P1 | It catches a planted reordering bug | 3 |
| T5 | Benchmarks mirroring 0.1's, with 0.1 run alongside | P4 | One results file holds both | 2 |
| F1 | Codec: batches, markers and headers encoded as pure functions | P2, P3 | The golden fixtures match byte for byte, random round trips pass, and a 30-minute fuzz of the decoder runs clean | 2 |
| F2 | Log and write lock: create, append, sync, marker, reading on open; the mutex then `flock` with the 10-second wait; the device and inode check | F1 | Batches survive a reopen, the lock times out across processes, and a replaced file restarts the commit | 3 |
| F3 | Crash recovery: the tail check, a complete batch rewritten, synced and marked, a torn marker rewritten, a cut from the first bad batch | F2, T3 | T3 passes with a cut at every write | 4 |
| F4 | Damage: reported in the middle with nothing cut, a look past zeros before cutting, a bad marked batch read again under the lock | F3 | Every damaged fixture is reported and nothing is cut | 2 |
| F5 | Failed commits: a cut back to just after the last marker, or else the lock kept and writes refused | F3 | T3 passes with a failure at every call, and a handle stuck that way blocks other writers until it's closed | 2 |
| F6 | Following other processes: `stat`, reading on, applying marked batches, the check when the writer is gone | F3, T4 | T4 shows every reader seeing every commit in order, with writers killed at random | 3 |
| F7 | File rules and growth: real paths, hard links refused, the sync cost of a growing file measured | F5, T5 | The rules have tests, and a committed put is no slower than 0.1 in the same run | 2 |
| F8 | Compaction: `NAME.compact` with the database's permissions, sync, rename, directory sync, cleanup on failure, writers waiting it out | F5, T2 | T3 passes with a cut at every write and a failure at every call during a compaction, and a copy taken mid-compaction opens | 3 |
| F9 | Reloading: a process that finds a different file at the path reloads it, and a leftover `NAME.compact` goes after the inode check | F6, F8 | T4 shows every reader seeing every commit in order through compactions and a backup moved into place | 2 |
| S1 | Records, fields and rules: the hash table, field lists, each table's field order, and 0.x's rules for keys, fields, link types and vectors | P3 | A random model test and 0.x's rule cases pass | 3 |
| S2 | Transactions: the undo list; begin, commit and roll back; the copy's lock taken at the first change; the error for a call through the database inside `Update` | S1 | Random transactions with random rollbacks match the model under the race detector | 3 |
| S3 | Change lists: emitted as a transaction runs, applied whole or not at all, and a snapshot for compaction | S2 | Store A's change lists applied to store B, and A's snapshot loaded into store C, both give back A | 2 |
| S4 | Key order and `Scan`: each table's keys in byte order as they come and go; `Scan` by prefix, after and limit | S1 | The model test and 0.x's `Scan` cases pass | 3 |
| S5 | Links and walks: link lists, the reverse index, deletes and drops removing links both ways, breadth-first walks by type and direction | S2, S3, S4 | The model test, 0.x's link and walk cases, and random graphs against a reference walk pass | 4 |
| V1 | Vector arrays and `Nearest`: arrays without pointers, float64 norms, free slots, the size set by the first vector and reset by `Drop`, top k with ties broken by key, a filter hook | S2, A5 | The model test passes and results match a brute-force search | 4 |
| V2 | Vector blocks, only if the open target is missed: compaction writes each table's vectors as one block, read on opening without parsing | V1, F8, S3 | A compaction round trip keeps every vector's bits and the open target is met | (2) |
| Q1 | Values and functions: SQLite's comparison rules for untyped values, arithmetic, the listed text and number functions, `LIKE`, `BETWEEN`, `IN`, `IS NULL` | P3, P5 | A4's expression answers match exactly | 4 |
| Q2 | Dates: `date` and `datetime` on `'now'`, plus or minus days, months and years | P3, P5 | A4's date cases, month ends included, match exactly | 2 |
| Q3 | Parser: the whole subset, with the walk forms, the one-record subquery, `INSERT`, `UPDATE`, `DELETE` and parameters | P5 | Every corpus statement parses or is refused as marked, and a fuzz of the parser runs clean | 5 |
| Q4 | Operators: scan, filter, project, sort, top k, limit and offset, aggregates over the whole result | P3, Q1 | Each operator passes against a fake store, and aggregates match A4's answers | 2 |
| Q5 | Planner: a nearest search for `ORDER BY distance` with `LIMIT`, rows without a vector first otherwise, walk joins, `IN` and `NOT IN` over walks, the one-record subquery | Q1 to Q4, S5, V1 | Plan-shape tests pass, and the crux query matches 0.x on fixed random data | 5 |
| Q6 | SQL writes: `INSERT`, `UPDATE` and `DELETE` through the same checks as `Put` and `Delete`, with row counts | Q5, S1 | 0.x's in-scope write cases pass | 2 |
| G1 | Slice 1: `Open`, `Update`, `Put`, `Get` and `Delete` through the file | P4, F2, S1, S3, A1 | A1's put, get and delete cases pass after a reopen | 2 |
| G2 | Slice 2, the first working version: `Scan`, `Drop`, `Link`, `Unlink`, `Neighbours`, `Walk` and unfiltered `Nearest` | G1, S4, S5, V1, T5 | A1's cases for those pass, A3 runs clean against 0.x, and `Get` and `Walk` are timed against their targets | 2 |
| G3 | The private database/sql driver behind `Query`, `QueryRow`, `Exec` and `SQL()`, with the transaction carried in the context, results copied out and `Begin` refused | P4 | Every value and destination type scans as it does in 0.x, and `SQL().Begin` returns an error | 2 |
| G4 | Slice 3: SQL through the driver, vector arguments, errors wrapping `ErrInvalid` | G1, G3, Q5, A3 | A1's in-scope SQL cases pass, and A3 runs the crux query on random data against 0.x | 2 |
| G5 | The command, with the same verbs apart from `adopt` plus `import`, `export` and `compact`; `Compact()` in Go | G2, G4, G6, F9 | A1's command suite passes against both binaries, apart from listed differences | 3 |
| G6 | Import and export in the Beta: the same JSON lines as A2 | G2, A2 | Going from 0.x to the Beta and back gives a byte-identical export | 2 |
| G7 | `check`: the in-memory copy's consistency, a damaged batch named, sizes reported | S5, V1, F4 | Planted damage in memory and in files is reported | 2 |
| I1 | Integration: the real store in place of the model in the file suites, and the 200 SIGKILLs and many processes through the public API | F9, S5, G1 | Those suites pass on amd64 and arm64, and opening 100,000 records is timed against its target, which decides whether V2 runs | 3 |
| R1 | Long runs: hours of fuzzing and crash runs on both architectures, every planted bug tried | I1, G5, G7, Q6 | Every job runs green for its full length, and every planted bug is caught | 3 |
| R2 | Independent review: a review agent's findings, each fixed with a test or answered | I1, G5, G6, G7, Q6 | None is open | 5 |
| R3 | Docs and site: `FORMAT.md` for the Beta, the README, notes on moving from 0.x, posts | G5 | A script checks the links and runs the examples | 3 |
| R4 | Release run: static Linux builds for amd64 and arm64 with smoke tests, a last overnight run, tests and benchmarks recorded beside 0.1's | R1, R2, R3, T5 | A dry run passes and the results are written; then the owner publishes | 2 |

How the work is run:

- The task board lives in the repository, one file per task, with its
  contract, the files it owns, its closing test, its status and notes. A
  session starts from the task's file and ends with its test green and a
  note.
- The interfaces from P3 change only through a small task that updates every
  caller at once.
- Each task owns its files. Shared files, such as CI, the interfaces and the
  board, change only through the session that coordinates the work.
- The differential harness runs on every merge.

### The owner's part

- The go-ahead, with the open decisions below settled at the same time.
- Publishing releases: a 0.x release with export and import whenever A2 is
  done, if wanted, and the Beta itself.
- One check-in, only if the file tasks run past one and a half times their
  hours, or the first working version misses a speed target by more than
  double. Otherwise progress is a note in the repository.

Everything else is automatic. Each task's test is a CI job, a review agent
checks the spec against this plan, a script compares the targets with 0.1's
numbers from the same run, and a review agent does the final review, as one
did for 0.1.

## Time estimate

The work comes to 133 hours in all: 16 for the five early tasks and 117
after the go-ahead.

The longest chain of tasks that must run one after another goes from the
file format and the interfaces through records, transactions, change lists,
links and walks, the planner, SQL from Go and the command to the review and
the release. It comes to 34 hours, about four working days, and no number of
agents can finish sooner. The file tasks add up to 23 hours, but the ones
that must run in order come to 16, and they can be done by hour 22.

| | One agent | Two agents | Three agents |
|---|---|---|---|
| Working days after the go-ahead | about 15 | about 8 to 9 | about 6 |
| First working version, with its tests, in working days | about 5 | about 4 | about 3 |

The plan assumes two agents, which keeps most of the gain for little
coordination: about 8 to 9 working days after the go-ahead, roughly two
weeks of calendar time with a working session most days. Work split between
agents costs 10 to 20 per cent more effort in merging and integration, which
the two- and three-agent figures include. The schedule does the first
working version's tasks first, which doesn't delay the finish. The five
early tasks take another day with two agents, before the go-ahead or
alongside it.

HyperCrux is developed in working sessions by Claude, an AI coding agent
made by Anthropic. The project's owner gives the go-ahead and publishes the
releases. The yardstick is 0.1: about 5,400 lines of Go and tests, from the
go-ahead to a published release in about a day. The Beta is roughly 5,000
lines of Go and as many again in tests, about twice 0.1's size. At 0.1's
pace, size alone would mean a few days. The Beta also does itself what 0.1
left to SQLite, from storage and crash safety to SQL, and code that has to
survive a crash or a failed write at any moment takes many rounds of testing
and fixing before it passes. Here the test harnesses, the spec and the
integration are tasks of their own, about a fifth of the hours.

What would make it longer:

- The file, the main risk. Recovery and compaction are where data loss would
  come from. If the crash tests keep finding problems there, the file tasks
  could double, their chain would become the longest at 47 hours, and two
  agents would need about 10 to 11 working days. More agents can't absorb
  that.
- More SQL. `GROUP BY` would add about a day, and each feature past the
  subset, such as other joins or `CASE`, adds time.
- Large databases. Opening time and memory grow with the data, and so do
  compaction pauses and the reloads after them. Keeping them reasonable near
  a million records may need work that isn't counted here.
- Machine time. Long fuzzing and crash-test runs take hours each, though
  most of it overlaps with other work.
- Fresh starts. Claude only remembers what's written down in the repository
  between sessions, which is why every task fits in one session and starts
  from its own file.

Passing every test earns a release. Trust with real data takes years of use.
The estimate is written up for readers of the site in
[How Long a New HyperCrux Engine in Go Would Take](https://hypercrux.com/how-long-a-new-hypercrux-engine-in-go-would-take-about-two-weeks-task-by-task/).

## Testing

Everything 0.x tests, and more, on x86 and ARM Linux alike:

- **Differential tests.** The same random workload on 0.x and on the Beta,
  through the same Go API, with every answer compared. 0.x on SQLite is the
  oracle. Distances agree within a stated bound, since 0.x adds in a
  different order, and results may differ only where two distances fall
  within it.
- **Crash safety and failures.** The engine reaches the file only through a
  small Go interface, so the tests can swap in a layer that loses unsynced
  writes or tears the last one, and can make any call fail. It's used for a
  simulated power cut at every write and a failure at every call in commits
  and compactions, on top of HyperCrux's 200 SIGKILLs at random moments.
  After each, the database opens at the last commit that succeeded or at the
  one under way, and so does a copy taken in the middle of a commit.
- **Many processes.** Writers and readers on one database at once, every
  reader seeing every commit in order, through compactions and a backup
  moved into place.
- **Exact results.** The search loop against a brute-force search, on random
  vectors including awkward ones (very small, very large, nearly identical),
  with amd64 and arm64 returning the same bits.
- **Fuzzing and the race detector.** Go's fuzzing on damaged files,
  sequences of API calls and SQL, and every test under the race detector.
- **Planted bugs.** Every planted bug in commits, recovery, compaction,
  vectors and links has to be caught, and each task adds its own.

## Risks

| Risk | Why it matters | What limits it |
|---|---|---|
| Recovery and compaction | Where data loss would come from | Checksums and sequence numbers on every batch, markers after syncs, one rename as the switch, and tests with a simulated power cut at every write and a failure at every file call |
| Memory | Every process holds the whole database, about 1.7 GB at a million records with 384-value vectors, and Go's collector can let the heap grow to twice the live data | The limit is documented, `check` reports sizes, and `GOMEMLIMIT`, set a little above the live size, keeps the heap near it |
| Opening and reloading | Opening reads the whole file, so does every run of the command, and other processes reload after each compaction | Heavy scripting at large sizes belongs in the Go package; carrying on without a reload can come later |
| Pauses | Writers wait while a compaction runs, for longer as the data grows | Writers wait out a running compaction instead of failing; compacting in the background can come later |
| A growing file | Each sync also commits a size change | Measured in F7; writing zeros ahead of the log can follow the Beta if needed |
| Search speed | A plain Go loop is slower than C with SIMD instructions, and the loop, more than memory, sets the pace of a search | The targets are set for the plain loop; Go assembly that keeps the 8 sums can come later |
| SQL scope | People expect all of SQLite's SQL | A documented subset; anything outside it can go back to 0.x through `export` and `import` |
| Parallel work | Interfaces that change under several agents, and bugs that only show when the parts meet | Interfaces fixed in P3, slice 1 running them end to end early, the differential harness on every merge, one agent owning the file tasks |
| Upkeep | A storage engine needs care, and this project is meant to run with little | 0.x stays the stable line; the Beta stays in its own `beta/` folder until its release |

## Left out of the Beta

- Data bigger than memory.
- macOS, until after the Beta, and Windows. Go builds for both, so the macOS
  port should be small: checking how syncing and locking behave there, plus
  a test runner and release builds. Windows needs its own file locking.
- Approximate vector indexes. They're the usual way to search tens of
  millions of vectors in milliseconds, and they can miss results.
- Columns and indexes. Checking records one by one should be fast enough at
  HyperCrux's sizes; both can come later.
- A C library or a Python binding. Other languages use the command, as with
  0.x.
- `GROUP BY` and `HAVING`, `date` and `datetime` on a stored date, and the
  rest listed under Queries and SQL.
- Read-only opening and a restore command. 0.x has neither.
- Carrying on after a compaction without reloading. The header keeps the
  fields it needs.
- Vector blocks that open without parsing, unless the open target needs them
  (task V2).
- Writing zeros ahead of the log, which can follow the Beta if F7's
  measurement calls for it.
- Speed-ups that can come later without changing the format: splitting one
  search across several cores, an 8-bit quick pass before the full vectors,
  Go assembly for the search loop that keeps the 8 sums, compacting in the
  background, and letting several writers share one sync.
- Mapping vectors into memory so processes share them, which would change
  the format.
- Servers, replication and anything across machines.
- 32-bit systems.

## Open decisions

1. The go-ahead for the Beta.
2. Whether to start the five early tasks now, and whether to ship 0.x's
   export and import in a release of their own.
3. Whether 0.x gets a smaller step meanwhile: a C search loop inside SQLite
   that uses SIMD and, like 0.x today, adds in float64. It would stay in
   0.x, since the Beta has no C.

Decided: the Beta lives in a `beta/` folder in this repository, beside 0.x.
A branch wouldn't do, because the tests run 0.x and the Beta side by side in
one binary.
