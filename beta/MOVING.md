# Moving from HyperCrux 0.x to the Beta

HyperCrux 0.x is the line of releases on SQLite, 0.1 and 0.2, whose code is
at the root of this repository. The Beta is the new engine in this folder,
written in Go without SQLite, and it hasn't been released yet.
[README.md](README.md) says how to build it.

A program that sticks to 0.x's Go API and to the Beta's SQL moves over with
little change: a new import path, and its data through an export. The rest
of this document lists every difference the Beta's docs and its task
records name, with what to do about each.

## In short

1. **Check where it will run.** The Beta runs on 64-bit Linux, on x86 and
   ARM, with the database on a local disk. 0.x keeps its macOS and Windows
   builds.
2. **Move the data** with an export from 0.x and an import into the Beta,
   as the next section shows. 0.1 has no export, so use 0.2, which opens
   0.1's files as they are.
3. **Change the import path** in Go, and replace any use of `Adopt`,
   `ApplicationID` and `DriverName`.
4. **Check the SQL** against the subset in [SQL.md](SQL.md). A statement
   outside it fails with an error, where 0.x would have run it.
5. **Check the scripts** that call the command: `adopt` has gone, and
   `version` and `check` print a little differently.

## Moving the data across

The two engines share one export format, JSON lines, which
[EXPORT.md](../EXPORT.md) describes. 0.x's `hypercrux export` writes it and
the Beta's `hypercrux import` reads it, and the same goes the other way.

To try it, make a small database with 0.x's command:

<!-- run 0.x -->
```sh
hypercrux put notes.db customer:42 '{"name": "Dana"}'
hypercrux put notes.db docs:1 '{"title": "Q3 plan", "status": "open", "vec": [0.9, 0.1, 0]}'
hypercrux put notes.db docs:2 '{"title": "Hiring notes", "status": "done", "vec": [0.1, 0.9, 0.1]}'
hypercrux link notes.db customer:42 owns docs:1
hypercrux link notes.db customer:42 owns docs:2
```

Then, still with 0.x's command, check it and export it. Export refuses a
database that `check` finds problems in, so the check first shows anything
to fix:

<!-- run 0.x -->
```sh
hypercrux check notes.db
hypercrux export notes.db > notes.jsonl
```

With the Beta's command, import it into a new file:

<!-- run -->
```sh
hypercrux import notes-beta.db < notes.jsonl
```

<!-- output -->
```text
imported 2 tables, 3 records, 2 links, 2 vectors into notes-beta.db
```

The import runs in one transaction, so a database goes in whole or not at
all. Tables come in with their fields in the same order and the same vector
sizes, so the Beta's export of the new file is the same, byte for byte, as
0.x's export of the old one:

<!-- run -->
```sh
hypercrux export notes-beta.db > back.jsonl
cmp notes.jsonl back.jsonl
```

Going back works the same way, with the Beta's `export` and 0.x's `import`.
In Go, `Export` and `Import` on a `DB` do what the two verbs do.

### What doesn't come across, and what to do

- **Tables 0.x hasn't adopted,** made with plain SQL and never used by
  `Put`, stay behind, since an export holds record tables alone. *What to
  do:* in 0.x, adopt them with `hypercrux adopt FILE TABLE` before the
  export, or move them some other way.
- **A table's declared column types, its constraints, its indexes and its
  own triggers** stay behind, and a generated column comes across as a
  plain one holding its values. *What to do:* nothing in the Beta takes
  their place. Its fields have no types, and the engine keeps HyperCrux's
  own rules.
- **Values `Put` refuses**, which plain SQL can store in 0.x: an infinite
  number, text that isn't UTF-8, and keys, field names or link types that
  break HyperCrux's rules. 0.x's export stops at the first, naming the
  record. *What to do:* change the value with SQL in 0.x, and export again.
- **A table with a vector size and no `vec` field,** which 0.x can hold
  once its `vec` column has gone by plain SQL, is refused by the Beta's
  import. *What to do:* in 0.x, run `hypercrux adopt FILE TABLE`, which
  sets the table's vector size from the vectors it holds, none without a
  `vec` field, and export again.
- **Fields a failed `Put` left behind in 0.x** come across like any other
  field, since an export holds what its engine holds. *What to do:* nothing.
  The Beta's table gets the field too.

Going the other way, one more thing stops an export from the Beta at 0.x's
import: a record whose values come within a few bytes of 1,000,000,000
bytes, or add up to more, which 0.x's SQLite refuses as a whole record.

## Go programs

### The import path

Change

```go
import "github.com/hypercrux/hypercrux"
```

to

```go
import "github.com/hypercrux/hypercrux/beta/hypercrux"
```

The package is still called `hypercrux`, so nothing else in the code has to
change for that. Until the Beta's release, fetch the module at main, with
`go get github.com/hypercrux/hypercrux@main`, since the release 0.2.0 holds
0.x alone. The Beta needs no cgo and no C compiler.

### What's gone

- **`Adopt`,** on `DB` and `Tx`. A Beta database has no tables made with
  plain SQL to adopt: tables and fields come from `Put`. *What to do:* make
  the records with `Put`, or bring them in with `Import`.
- **`ApplicationID`,** the mark in an SQLite file's header. *What to do:*
  nothing. A Beta file starts with a magic number of its own, which
  [FORMAT.md](FORMAT.md) gives, and `Open` refuses any other file.
- **`DriverName`,** 0.x's database/sql driver for SQLite. *What to do:* use
  `db.SQL()`, which gives a `*sql.DB` on the Beta's own driver.

### What's new

- **`Compact`** rewrites the file with only the live data, the job `VACUUM`
  did in 0.x, which also takes deleted data off the disk. The database
  compacts itself now and then too.
- **Error values** for what 0.x reported with plain errors: `ErrDamaged`
  with a `*Damage` that says where, `ErrNotDatabase`, `ErrZeroX` for a 0.x
  file, `ErrFormatVersion`, `ErrLockTimeout`, `ErrStuck`,
  `ErrInsideUpdate` and `ErrClosed`. `ErrNotFound` and `ErrInvalid` are
  0.x's, with 0.x's texts.
- **`Report`'s fields,** after 0.x's: `FileBytes`, the file's size;
  `MemoryBytes`, about how much memory the copy takes in this process; and
  `CompactionError`, what went wrong in the last compaction through this
  `DB`, or `""`. A failed compaction leaves the database whole, so it isn't
  one of the `Problems`, and `OK` doesn't count it.
- **`Version`** is `0.0.0-beta` until the release, and `FormatVersion` is
  the Beta's format's, 1.

### Errors

- `ErrNotFound` and `ErrInvalid` mean what they meant in 0.x, for the same
  calls. *What to do:* nothing.
- Where 0.x gave SQLite's own error or a plain one, the Beta's wraps one of
  its new values: a lock that stays taken for 10 seconds wraps
  `ErrLockTimeout`, where 0.x gave SQLite's "database is locked", and a
  damaged file wraps `ErrDamaged`. *What to do:* test with `errors.Is`
  against the new values, in place of matching 0.x's texts.
- A link type that starts with a zero byte, and a `Put` that would take a
  table past 1,999 fields, give `ErrInvalid`, where 0.x refuses both with a
  plain error. *What to do:* test for `ErrInvalid`.
- Text or bytes longer than 1,000,000,000 bytes are refused with SQLite's
  own error, "string or blob too big", as in 0.x, which wraps neither
  `ErrInvalid` nor `ErrNotFound`. A statement longer than that is
  "statement too long", as in 0.x. *What to do:* nothing.

### Transactions and goroutines

- **A call through `db` inside its own `Update`.** In 0.x a read through
  `db` there works, outside the transaction, and a write waits for the
  transaction and fails after 10 seconds. In the Beta, any write, and any
  read after the transaction's first change, fails at once with
  `ErrInsideUpdate`. *What to do:* make every call inside `Update` through
  `tx`, which sees the transaction's changes. So does `Close`: inside an
  `Update` it fails with `ErrInsideUpdate`.
- **A goroutine that the function starts and then waits for** can't be told
  from another, so a read it makes through `db` after the first change
  hangs, and so does `Close`, and a write fails once the wait for the lock
  is over. *What to do:* hand `tx` to such a goroutine, or do the work
  before the first change.
- **Other goroutines' reads wait for an `Update`'s changes.** In 0.x,
  readers never wait for a writer. In the Beta, other goroutines in the
  writer's process wait from the `Update`'s first change until it commits,
  while other processes keep reading. Work done before the first change,
  such as computing an embedding, keeps nobody waiting. *What to do:* keep
  slow work out of the stretch between an `Update`'s first change and its
  end.
- **An export under way holds up an `Update`'s first change** in its own
  process, where 0.x's writers carry on through an export, and the writer
  `Export` writes to mustn't call the database, since a read from inside it
  can wait for such an `Update`, which waits for the export. *What to do:*
  export to a file or a buffer, and work with the database after `Export`
  returns.
- **`SQL().Begin` returns an error.** *What to do:* run the statements in an
  `Update`, through `tx.Exec` and `tx.Query`.
- **A write that fails changes nothing,** even inside an `Update` that goes
  on to commit. 0.x keeps the new fields a failed `Put` named, and the table
  when the `Put` made it. So a later `Put` that names such a field in
  another case spells it its own way in the Beta, and the failed `Put`'s way
  in 0.x, which `Get` and `Scan` show. *What to do:* nothing, unless a test
  of yours counted on 0.x's leftovers.
- **A commit that can't be undone.** When a commit fails and the file can't
  be put back, the error wraps `ErrStuck`, the commit's outcome is unknown,
  and every later write through that `DB` fails with `ErrStuck` until it's
  closed, while other handles wait for the lock. *What to do:* close the
  `DB` and open it again; the next writer settles the commit.
- **The lock's wait** is 10 seconds, as in 0.x, and longer while a
  compaction runs, until 10 seconds after it ends. *What to do:* nothing,
  or allow for writers to pause while the file is compacted.

### database/sql

- **`Query` reads every row before it returns,** so rows left open hold up
  nobody, and keep what the database held when the query ran. A large
  result takes its whole size in memory. *What to do:* page through a large
  result with `LIMIT` and `OFFSET`, or with `Scan`'s `after`.
- **`LastInsertId` returns an error,** since records have no row numbers.
  0.x gives SQLite's row id. *What to do:* use the key the statement wrote.
- **`ColumnTypes`** gives `""` for every column's type name and `any` for its
  scan type, with no answer for nullable. 0.x gives `TEXT` and
  `sql.NullString` for `key`. *What to do:* scan into the types you expect,
  or into `any`.
- **An argument with a name,** from `sql.Named`, is an error, and so are
  more arguments than `?` marks. 0.x leaves a named argument's mark NULL,
  and ignores the extra ones. *What to do:* pass one plain argument for each
  `?`.
- **`SQL()` has no limit on its connections,** where 0.x's has 8. A pool of
  one, set with `SetMaxOpenConns(1)`, runs a `tx.Exec` for each row of an
  open `tx.Query` on both engines. *What to do:* nothing.
- **Errors through `SQL()`** wrap neither `ErrInvalid` nor `ErrNotFound`,
  as in 0.x, where that handle gives SQLite's errors as they are. *What to
  do:* nothing.

### Values and results

- `Put` takes the same Go types as 0.x's, and `Get` gives the same back, with
  times stored as RFC 3339 text in UTC. Field names match regardless of
  case, and a table keeps the spelling it saw first, as in 0.x.
- **Distances** can differ from 0.x's in the last bits, since the Beta adds
  a dot product in 8 running sums. They agree to within 1e-9, and two hits
  can swap places only where their distances fall within that. The Beta
  gives the same bits on amd64 and arm64. *What to do:* compare distances
  within a bound, if you compare them at all.
- **Sums of reals** in SQL add a table's rows in key order, and 0.x in its
  own scan order, so `sum()` and `avg()` can differ in their last bits where
  rows went in out of key order. *What to do:* nothing, or round.

## SQL

The Beta's SQL is a part of SQLite's, and a statement in it gives 0.x's
answer: the same columns, rows, kinds of value and errors' kinds. A
statement outside it fails with an error of kind "error", where 0.x runs it.
[SQL.md](SQL.md) sets out the whole subset.

Missing from the subset, among others: `GROUP BY` and `HAVING`, joins other
than the one to `walk()`, subqueries other than over `walk()` or one that
reads one record by its key, `CASE`, JSON functions over fields, declared
column types, `UNIQUE` and `CHECK` constraints, upserts and `INSERT OR
REPLACE`, indexes, `WITH` and recursive queries, date functions other than
`date` and `datetime` on `'now'`, views, triggers, window functions, and
statements such as `CREATE`, `PRAGMA` and `VACUUM`. *What to do:* rewrite
the statement in the subset, or do the work in Go: a `GROUP BY`, for
example, becomes a loop over a `Scan` or an ordered query. For SQL that
needs all of SQLite, keep the data in 0.x, or export it there.

SQL.md's "Where the Beta differs from 0.x" lists what differs on purpose
inside the subset. Each, with what to do:

- **Small things outside the subset:** backquoted and bracketed names, text
  in single quotes as a name or an alias, hex numbers, digit separators,
  numbered and named parameters, and `TRUE` and `FALSE`. *What to do:* use
  double quotes for names, decimal numbers, `?` marks, and 1 and 0.
- **More arguments than `?` marks,** in a statement and in `Nearest`'s
  filter, is an error. *What to do:* pass one argument for each mark.
- **A semicolon followed by a comment** is fine. 0.x then gives no rows and
  no columns. *What to do:* nothing.
- **Text after the semicolon** is an error, where 0.x runs each statement
  and gives the last one's rows. *What to do:* run one statement a call.
- **A double-quoted name that isn't a field** is an error, "no such column",
  where 0.x reads it as text. *What to do:* write text in single quotes.
- **A result column's alias in WHERE** is an error. *What to do:* repeat the
  expression in WHERE.
- **A field named twice in one INSERT or UPDATE** is an error. 0.x keeps the
  first value in an INSERT and the last in an UPDATE. *What to do:* name
  each field once.
- **Values `Put` refuses can't be written through SQL either:** an infinite
  real, text that isn't UTF-8, a key with a NUL byte or that isn't UTF-8,
  and a vector holding NaN or infinity or only zeros. 0.x stores them, and
  its export refuses them later. *What to do:* check values before writing
  them.
- **`vector()`'s error on bytes that aren't whole float32 values** is of
  kind "invalid" in a write, where 0.x gives "error". *What to do:* test for
  `ErrInvalid` there.
- **An error's kind follows the statement.** 0.x wraps errors in `Exec`
  alone, so a write through `Query` reports a broken rule as "error" there,
  and a SELECT through `Exec` reports a refusal of `distance()`, `vector()`
  or `walk()` as "invalid". The Beta gives "invalid" for the write and
  "error" for the SELECT, whichever call runs them. *What to do:* run writes
  through `Exec` and reads through `Query`, as both engines expect.
- **`Exec` of a SELECT works out every row,** so it gives the error any row
  raises, and `RowsAffected` 0. 0.x's works out the first step alone, and
  gives the last write's count on its connection. *What to do:* run a
  SELECT through `Query`.
- **'now' is read once a statement,** where 0.x reads it again for each row
  it returns. *What to do:* nothing.
- **Rows without ORDER BY come in key order,** where 0.x's order depends on
  SQLite's plan. *What to do:* nothing, or add ORDER BY wherever the order
  matters, which both engines then keep.
- **A `Put` that fails inside an `Update`** leaves the table as it was, as
  under "Transactions and goroutines" above.
- **0.x's limit on a whole record.** 0.x's SQLite holds each record it
  stores to 1,000,000,000 bytes, with its key and every field in SQLite's own
  form, so an INSERT or UPDATE of a value within a few bytes of that, or of
  values that add up to more, fails there. The Beta holds each value to the
  limit, and takes such a row. *What to do:* nothing, unless the data has to
  go back to 0.x.
- **`walk()` with fewer than 2 arguments or more than 4** is an error of
  kind "error", found before anything runs. 0.x finds it as `walk()` runs,
  so in a write it's "invalid" there, and a statement that never reaches the
  call gives no error. *What to do:* give `walk()` 2 to 4 arguments.
- **An argument with a name** is an error, as under "database/sql" above.
- **A one-record subquery in an UPDATE's SET** that no record reaches until
  the UPDATE has changed a record reads that record as it was before the
  statement. 0.x reads the change. *What to do:* don't read a record in the
  SET of an UPDATE that changes it.

## The command

The Beta's `hypercrux` has 0.x's verbs, with 0.x's output, messages and exit
codes, apart from these ([tasks/G5.md](tasks/G5.md) gives each with its
reason):

- **`adopt`** is an unknown command, exit 2, since a Beta database has no
  plain SQL tables. *What to do:* put the records with `put` or `import`.
- **`compact`** is new. It rewrites the file with only the live data and
  prints `compacted FILE from N bytes to M bytes`. *What to do:* use it in
  place of `VACUUM` through `sql`.
- **`version`** prints `hypercrux 0.0.0-beta, file format 1`, with no
  SQLite version. *What to do:* parse only the first two words, if a script
  reads it.
- **`check`** prints 0.x's line, `ok: ...` or the problems, and then a line
  of its own, `size: N bytes in the file, about M bytes in memory`. On a file
  damaged so that it can't open, it prints `problems in FILE:` and the
  damage, and exits with 1, where 0.x gives SQLite's error. *What to do:*
  read the first line alone, if a script reads it.
- **`init`** makes a new database, or one of an empty file, and refuses any
  other file. 0.x adds its tables to any SQLite file. *What to do:* start
  from an empty file or none.
- **A file that isn't a database** gets Open's error, such as
  `not a HyperCrux database: ...`, and a 0.x file says how to move it
  across. *What to do:* export it with 0.x and import it.
- **`sql`** takes the Beta's SQL, and anything outside the subset exits
  with 1. *What to do:* as for SQL above.
- **A failed `put` or `import` on a new file** removes the file, and there's
  no `-wal` or `-shm` file to remove. *What to do:* nothing.

Each run of the command opens the database afresh, which reads the whole
file, so a script that makes many calls on a large database pays that each
time. *What to do:* for heavy scripting at large sizes, use the Go package,
which keeps the database open.

## The file

- **It isn't SQLite.** SQLite's tools, and any language's SQLite driver,
  can't open a Beta database, and 0.x's way of writing the file straight
  from another language, with plain SQL that the triggers check, has gone,
  with the [Python example](../examples/python/hcfile.py) that does it.
  `Open` refuses a 0.x file with an error that wraps `ErrZeroX`, which says
  how to move it across, and anything else at the path that isn't a Beta
  database with one that wraps `ErrNotDatabase`. *What to do:* use the
  `hypercrux` command, which reads and writes JSON, or an export. A program
  that writes the file itself has to keep every rule in
  [FORMAT.md](FORMAT.md).
- **One file.** There's no `-wal` or `-shm` file beside it. A plain copy of
  the file, taken at any moment, is a backup. *What to do:* back up by
  copying the file, and restore as FORMAT.md's
  [Backing up and restoring](FORMAT.md#backing-up-and-restoring) says.
- **Its rules.** The file has to be on a local file system that keeps a
  file's data in step with its size through a power cut, such as ext4 with
  its default `data=ordered`, XFS or btrfs. Network file systems, and
  folders shared into containers through a virtual machine, don't keep the
  locks honest. *What to do:* keep the database on a local disk. FORMAT.md's
  [The rules for the file](FORMAT.md#the-rules-for-the-file) has the rest.
- **Hard and symbolic links.** A database file with more than one hard link
  is refused with `ErrInvalid`, and so is a symbolic link that leads to
  nothing. 0.x opens the first, and makes a database where the second
  leads. A hard link made while the database is open makes every write fail
  with `ErrInvalid` until it's gone. A symbolic link that leads to a database
  works: the database is opened by its real path. *What to do:* give a
  database file one name, and point a symbolic link only at a file that's
  there.
- **No database inside another SQLite file.** 0.x can add its tables to an
  application's own SQLite database. *What to do:* keep the application's
  own data in a file of its own.
- **Memory and opening.** Every process holds the whole database in memory,
  and opening reads the whole file. *What to do:* see "How fast, and what it
  costs" in [README.md](README.md#how-fast-and-what-it-costs), and set
  `GOMEMLIMIT` a little above the size `Check` gives for the copy.
- **Deleted data stays in the file** until a compaction, as it stays in
  SQLite's free pages until `VACUUM`. *What to do:* call `Compact`, or
  `hypercrux compact`, when deleted data must leave the disk.
