# HyperCrux Beta: a plan for a new engine in C

**Status: a plan. Nothing in it is built.** HyperCrux 0.x stays what it is,
a Go library and command on SQLite, and keeps getting fixes. This document
describes a HyperCrux built for as much speed as a native engine allows: a
new database engine written in C with all four handles, and what it would
take. Every 0.1 figure below comes from [test/results](test/results). The
Beta figures are targets, to be measured the same way before anyone quotes
them.

## What the Beta is for

HyperCrux 0.1 proved the idea: one record, reachable by key, SQL, links and
similarity, with all four kept in step by one transaction. It runs on
SQLite and keeps its rules in the file as triggers, which is why any program
that speaks SQLite and follows [FORMAT.md](FORMAT.md) can share a HyperCrux
file safely.

The Beta keeps the idea and replaces the machinery. A native engine can lay
out vectors, links and keys for the way they're actually read, and it can do
the arithmetic of search with SIMD instructions inside the engine. The cost
is the SQLite file, with everything that comes with it, from its tools to
its decades of testing.

## Where 0.1 spends its time

From the recorded benchmarks, on a two-core cloud machine, middle of three
runs:

| Operation | 0.1 | Where the time goes |
|---|---|---|
| Nearest 10 among 10,000 vectors of 384 values | 42 ms | About 4.2 µs a vector. SQLite needs about 1 µs to read a row (see below); the rest is the driver copying each vector into Go and a Go loop that decodes each float on its own |
| Nearest 10 among 100,000 vectors of 384 values | 0.41 s | The same cost per vector |
| The same, a tenth passing a filter | 0.13 s | The 90,000 rows that fail the filter cost about 1 µs each, which is SQLite reading and testing a row |
| Nearest 10 among 10,000 vectors of 1,536 values | 0.12 s | Each extra value adds about 7 ns. Rows this long spill onto an SQLite overflow page and each vector is copied into Go first, so the loop is only part of it |
| Get a record by key | 18 µs | One indexed SQL query through Go's database/sql; not timed in parts |
| Walk 1 and 3 links out, 100,000 records with 5 links each | 43 µs and 0.51 ms | A recursive SQL query through database/sql; not timed in parts |
| Put one record, committed | 0.35 ms | Mostly the commit's sync, which any engine with the same guarantee pays |
| Put records with 384-value vectors, 1,000 per transaction | 51 µs a record | Each key stored three times (the row, its primary-key index and hc_keys, the last by a trigger), trigger checks on the vector, and three statements a record through database/sql; not timed apart |

The 1 µs figure is derived: the filtered run compares as many vectors as
the 10,000-vector run, reads 90,000 more rows that fail the filter, and
takes about 86 ms longer.

## What stays and what goes

**Stays:**

- The promise that the four handles never disagree. A record, its key, its
  links and its vector change in one transaction, and a delete takes the
  links with it.
- Exact search by default. A search compares every vector that passes the
  filter.
- One database in one file, shared by many processes on one machine, with a
  small lock file beside it.
- The Go API and the command's verbs, so programs written against 0.x move
  over with little change.
- The test philosophy: kill the writer at random, many processes on one
  file, a second language on the same file, every figure recorded.

**Goes:**

- The file stops being an SQLite file. SQLite tools can't open it, and other
  languages reach it through bindings instead of their own SQLite driver.
- Rules in the file. The engine enforces them, which is safe because every
  write goes through the engine.

**Bridges:** `hypercrux import` reads a 0.x file and writes a Beta file;
`hypercrux export` goes the other way. 0.x keeps working on its own files
for as long as anyone uses it.

## Design

### Storage

The base is AltSql DB's storage, described under AltSql below: a
copy-on-write B-tree in one file, two commit headers, large values on
overflow pages, and a free list that holds back the pages the last commit
freed for one more commit, so both headers always describe whole trees.
Four changes turn it into what HyperCrux needs:

1. **Readers in other processes.** A lock file holds a reader table in shared
   memory. A reader records the commit it started from and reads that
   snapshot for as long as it likes. The writer never reuses a page that a
   recorded snapshot can still see. Readers never wait for the writer, and
   the writer never waits for readers. Each reader holds a lock on its own
   byte of the lock file while its entry is in use, and the writer clears an
   entry only when it can take that lock. That works across containers and
   with reused process IDs, given care with POSIX locks, which a process
   loses when it closes any handle to the file.
2. **One sync per commit, if it survives the crash tests.** Every page
   carries a CRC32C checksum (the field exists in AltSql DB and is zero
   today). A commit writes all its new pages, then its header, and syncs
   once; it's durable when the sync returns. The header carries the
   checksum of a list of every page that commit wrote, and each listed page
   must carry this commit's number as well as the right checksum, so a page
   that kept its old contents is caught as surely as a torn one. On open,
   under the writer lock, if the newest header, the list or any page on it
   doesn't match, the file opens at the commit before, whose pages copy on
   write never touched. Only one commit is ever unsynced: the next starts
   writing after this one's sync returns. The writer records each new
   commit in the lock file once its sync returns, and readers take their
   starting commit from there, since the newest header on disk may not be
   durable yet. Opening a file then costs a check of the last commit's
   pages, so it grows with that commit's size. This has to pass AltSql's
   crash matrix (a cut at every write, torn sectors, lost unsynced writes)
   before it's trusted. Until it does, the two syncs AltSql DB does today
   stay.
3. **Group commit.** Writers in one process queue up, and their transactions
   commit together under a single header and a single sync. A lone committed
   write still costs one sync; many concurrent writers share them.
4. **Reads from a memory map.** Pages are read in place, with no copy into a
   cache. A page's checksum and structure are checked the first time a
   process reads it, and `check` verifies every page. Writes still go
   through the engine's own buffers.

### Keys and records

Every record gets an internal 64-bit ID when it's created. Two key spaces in
the tree hold it: text key to ID, and ID to the record's key, table and
packed fields. Links and vectors use IDs, which are eight fixed bytes and
compare quickly.

A record's fields are packed in one value: a field number from the table's
field dictionary, then the value, for each field present. HyperCrux's `Put`
can add new fields at any time, and this layout lets it do that without
rewriting existing rows, which AltSql DB's fixed row layouts can't.

`Get` is two lookups in a memory-mapped tree. `Scan` by key prefix walks the
key space in order, since its keys compare as bytes. Each key is stored
twice, once in each key space, where 0.1 stores it three times.

### Vectors

Each table's vectors live in a vector segment: runs of contiguous pages
holding fixed-size slots, one per vector, each with the record's ID. A
search streams through the segment and computes one dot product per slot.

- **Stored as given.** A vector comes back from `Get` exactly as it went in.
  The engine also keeps each vector's norm, computed in float64, so cosine
  distance needs only the dot product. The question is normalised once per
  search. Zero vectors stay refused, as in 0.x, and SQL's `distance()` keeps
  returning 1 when one side is zero.
- **Kernels.** AVX2 and AVX-512 on x86 and NEON on ARM, with a plain C loop
  as the fallback and the reference. Every kernel widens values to float64
  and adds them in one fixed order, with the same use of fused
  multiply-add, so every kernel returns the same bits as the reference.
- **Filters.** A filter is turned into a bitmap of the slots it allows, from
  an index when there is one, and the scan skips the rest.
- **A bounded quick pass, still exact.** Optionally, each table keeps a
  separate run of 8-bit copies of its normalised vectors, each with the size
  of its rounding error, measured against the values the search uses and
  rounded up. The quick pass reads that run, a quarter of the bytes, and
  gives each candidate a range its true distance must fall in, which follows
  from the Cauchy-Schwarz inequality. If the question is rounded to 8 bits
  too, its own error widens the range. A candidate is checked against its
  full vector unless its range starts above the k-th best distance so far
  plus a margin covering rounding in both passes; until k candidates have
  full distances, the k-th smallest upper bound stands in. The answer is the
  same as a full scan's.
- **Several cores.** Large segments split across threads, each keeping its
  own top k, merged at the end.
- **Upkeep.** Deleted slots are marked and reused. Copy on write breaks a
  run when it copies one page, so a later commit repacks runs that have
  split up.

### Links

Two key spaces hold every link, one for each direction: (source ID, type,
target ID) and (target ID, type, source ID), as fixed-width integers. Link
types are numbered in a small dictionary. A walk is a breadth-first search
over IDs with a visited set, reading one short key range per record. It
doesn't touch SQL or build temporary tables, and it turns IDs back into
keys only for the answer. Deleting a record reads both of its ranges and
removes each link from the other side.

If deep walks across very large graphs ever take most of the time, a
read-only compressed copy of the adjacency can be added later. The key
spaces come first because they reuse the tree and its crash safety.

### SQL

AltSql DB's SQL has no joins or subqueries yet, and reads up to 16 columns
a table. HyperCrux's SQL handle needs joins, `IN (SELECT ...)` filters and
the one-statement crux query. Two routes, one decision:

- **Route A: SQLite's query engine as the front end.** HyperCrux tables
  appear to SQLite as virtual tables. SQLite parses and runs the SQL, every
  row it reads and writes comes from the native engine, and SQLite keeps no
  file of its own. `distance()` becomes a C function that reads vectors in
  place. The scalar `walk()` that returns JSON stays, so the crux query
  keeps its form, and it gains table-valued twins: a `walk()` that returns
  rows and a new `nearest()`. Queries and row writes in 0.x's SQL keep
  working, the crux query included. Schema statements change: SQLite won't
  index, alter or put triggers on a virtual table, so record tables get
  their columns and indexes through the engine's own calls. SQLite tells a
  virtual table only about transactions that write to it, so nested
  savepoints and read snapshots spanning several statements have to come
  from the engine. The cost is about a megabyte of code and SQLite's per-row
  overhead between the two.
- **Route B: grow AltSql's SQL.** Add joins on keys, `IN` subqueries, more
  columns, and the walk and nearest operators, with a planner that knows
  them. The engine stays small and easy to follow, and the crux query could
  plan better than SQLite plans it. It takes longer, and SQL would cover
  less than 0.x does at first.

Route A is the pick for the Beta, with the native operators kept apart from
the front end so that route B can replace it later without touching
storage.

### Transactions and processes

One writer at a time per file, any number of readers, across processes.
Every write takes the writer lock and runs in one transaction across all
four handles. It commits with one sync once that protocol has passed
AltSql's crash matrix, and with two until then. Readers see a consistent
snapshot from start to finish. Writes made through SQL go through the same
write path as `Put`, `Link` and `Delete`, so the rules hold whichever way a
change comes in.

### Bindings and tools

- A small, stable C API.
- The Go package, keeping 0.x's API: `Open`, `Get`, `Put`, `Delete`, `Scan`,
  `Link`, `Unlink`, `Neighbours`, `Walk`, `Nearest`, `Update`, `Exec`,
  `Query`, `QueryRow`, `Drop` and `Check`. `Query` and `QueryRow` keep their
  database/sql types, so the binding ships a database/sql driver. `Adopt`
  and `SQL()` go, since there's no SQLite file to adopt tables from or hand
  out.
- The `hypercrux` command with the same verbs, plus `import` and `export`.
- A Python binding over the C API, in place of `hcfile.py`'s direct SQL.
- `FORMAT.md` for file format 2, as complete as 0.x's.

## AltSql: what to take and what to add

[AltSql](https://github.com/AltSql/altsql) is a small database for sensor
fleets: AltSql Core keeps key-value and time-series records on devices, and
AltSql DB keeps a whole fleet's records on a gateway. AltSql DB 0.3.0-alpha
is one C file of 5,468 lines on top of AltSql Core's single file, Apache
2.0, with no `malloc`, so vendoring it brings Core along. The AltSql README
calls the project "Written to prove the design and measure it" and "Not for
production."

**Worth taking:**

- The copy-on-write B-tree with two commit headers, overflow pages and free
  lists. AltSql measured a reopen at 0.03 ms; the one-sync commit above
  would add a check of the last commit's pages to that.
- The direct path: buckets, get, put, delete, and cursors that survive
  writes, with no SQL on the way. In AltSql's own Gate 1 runs it read
  cached keys 4.42 times faster than SQLite through SQL and wrote
  10,000-write transactions 2.58 times faster. With whole-number keys
  against SQLite's blob path it was 1.22 times faster, short of its own
  2x mark.
- Keys encoded so they compare as bytes in the values' order, and secondary
  indexes as key spaces of their own.
- The test harness, which matters as much as the code: model tests, a power
  cut at every write with torn sectors and lost writes, a failure at every
  file call, coverage-guided fuzzing, planted bugs, and random SQL compared
  with SQLite's answers.

**Still to add, most of it useful to AltSql too:**

- Readers in other processes. AltSql DB takes an exclusive lock today, so
  one process uses a file at a time.
- One sync per commit. AltSql DB syncs twice, and its README notes that this
  makes single-write transactions slower than SQLite in WAL mode.
- Page checksums, which AltSql DB leaves room for but sets to zero, and
  compaction after big loads.
- Reads from a memory map.
- An ordered scan as fast as SQLite's: AltSql's Gate 1 measured 0.74 times
  SQLite's speed and left the cause open.
- For HyperCrux alone: records with changing fields, vectors, links and the
  SQL front end.

**How to share the code.** For the Beta, vendor a pinned copy of AltSql's
storage into HyperCrux and carry improvements back by hand. Once both
projects rely on the same storage, a shared library becomes worth the work
of keeping the two in step. Both projects are Apache 2.0; HyperCrux's
NOTICE and THIRD_PARTY_LICENSES.md would carry AltSql's notice.

## Targets

Same machine as 0.1's benchmarks, middle of three runs, recorded by
`scripts/record-tests.sh`. These are goals with a stated basis. None has
been measured.

| Operation | 0.1 recorded | Beta target | Basis |
|---|---|---|---|
| Nearest 10, 100,000 vectors of 384 values, one thread | 0.41 s | 50 ms | 154 MB of floats, streamed from memory |
| The same with the bounded quick pass | 0.41 s | 15 ms | 38 MB in the quick pass, then a few full checks |
| Nearest 10, 100,000 of 384, a tenth passing a filter on an unindexed field | 0.13 s | 30 ms | Reading that field for 100,000 records, then 15 MB of vectors |
| Nearest 10, 10,000 vectors of 1,536 values | 0.12 s | 15 ms | 61 MB of floats |
| Get by key, from C / from Go | 18 µs (Go) | 2 µs / 4 µs | Two lookups in a mapped tree |
| Walk 1 link out / 3 links out | 43 µs / 0.51 ms | 3 µs / 60 µs | Integer key ranges, no SQL |
| Put with a 384-value vector, 1,000 per transaction | 51 µs | 10 µs | No triggers, each key stored twice, one write path |
| Put one record, committed | 0.35 ms | no slower than 0.1 | One sync per commit, if it passes the crash tests |
| File size for the same records and links | baseline | smaller | Keys stored twice instead of three times, links as integers |

## Phases and gates

Each phase ends at a gate that decides whether the next one starts. Sizes
are relative, from S for small to L for large.

| Phase | Work | Gate | Size |
|---|---|---|---|
| 0. Spec | File format 2, the C API header, the SQL route, the test plan | The spec is approved | S |
| 1. Storage | AltSql's storage vendored and extended: readers across processes, checksums, one sync per commit, group commit, mapped reads | AltSql's crash matrix and fault tests pass, plus HyperCrux's kill test and many-process test, on Linux, macOS and Windows; a committed put is no slower than 0.1 | L |
| 2. Keys and records | IDs, the key spaces, records with changing fields, scans | Model tests against 0.x as the oracle; the get target | M |
| 3. Vectors | Segments, kernels for each instruction set, filter bitmaps, the bounded quick pass, threads | Every kernel returns the same keys in the same order as the C reference, ties broken by key; the search targets | M |
| 4. Links | Link key spaces, walks, deletes that take links with them | 0.x's link tests pass; the walk targets | S |
| 5. SQL | Route A or B, with `walk()`, `nearest()` and `distance()` native | 0.x's SQL tests for queries and row writes pass; the crux query gives 0.x's answers on random data | L |
| 6. Bindings | The Go API, the command, the Python binding, import and export | Every 0.x test of the Go API and the command passes through the Go binding, and import and export tests take the place of the ones that write the file with plain SQLite | M |
| 7. Beta | Independent review, a long fuzzing run, sanitizers, planted bugs, recorded benchmarks, docs, site | Released as a Beta on Linux, macOS and Windows | M |

## Testing

Everything 0.x tests, and AltSql's harness on top:

- **Differential tests.** The same random workload on 0.x and on the Beta,
  through the same Go API, with every answer compared. 0.x on SQLite is the
  oracle. Distances agree within a stated bound, since 0.x adds in a
  different order, and results may differ only where two distances fall
  within it.
- **Crash safety.** HyperCrux's 200 SIGKILLs at random moments; AltSql's
  power cut at every write, with torn sectors and lost unsynced writes; a
  failure at every file call. After each one, the file opens at the last
  commit or the one under way, and `check` passes.
- **Many processes.** Writers and readers on one file at once, readers never
  blocking, every record and link accounted for.
- **Exactness.** Every SIMD kernel and the bounded quick pass against a
  brute-force search, on random vectors including awkward ones (very small,
  very large, nearly identical).
- **Fuzzing and sanitizers.** Damaged files, API call sequences and SQL,
  under AddressSanitizer, UBSan and ThreadSanitizer.
- **Planted bugs.** Every planted bug in storage, vectors and links has to
  be caught.

## Risks

| Risk | Why it matters | What limits it |
|---|---|---|
| A new commit protocol | One sync per commit is where data loss would come from | AltSql's crash matrix before anything else; two syncs stay until it passes |
| Readers across processes | Stale readers can pin old pages and grow the file | Per-reader byte locks, and `check` reporting pinned pages |
| Memory maps differ by system | Windows can't easily grow a mapped file | Reserve address space ahead; Windows runs the storage tests from Gate 1 on |
| C memory safety | A memory bug can sit quietly for months | Sanitizers, fuzzing, planted bugs, the plain C kernel as reference |
| Upkeep | A storage engine needs care, and this project is meant to run with little | 0.x stays the stable line; the Beta lives apart until Gate 7 |
| AltSql DB is alpha | Its format may change | Vendor a pinned copy; carry changes over by hand |

## Left out of the Beta

- Approximate vector indexes. They're the usual way to search tens of
  millions of vectors in milliseconds, and they can miss results. If they
  come, they come later, opt-in and clearly labelled.
- Servers, replication and anything across machines.
- 32-bit systems.

## Open decisions

1. Whether to start Phase 0, the spec.
2. Route A or route B for SQL.
3. AltSql's storage as a vendored copy or a shared library.
4. Where the Beta lives until Gate 7: a folder in this repository, a branch
   or a repository of its own.
5. Whether 0.x gets the smaller step meanwhile: a C search loop inside
   SQLite, using SIMD in the same float64 arithmetic 0.x uses now, which
   keeps the file and every rule as they are. Its dot-product kernels could
   carry over to the Beta.
