# The HyperCrux Beta file format, version 1

A Beta database is one file: a header, then a log of batches. Each batch is
one commit, and a small marker follows it once it's safely on disk. Opening a
database reads the whole log into memory, so the file holds only what can't
be worked out again: the records with their fields and links, and each
table's field order and vector size.

This document is the Beta's FORMAT.md. It describes every byte, and every
rule a program keeps while it writes the file, from the write lock and its
waits to compaction and the file systems a database can live on. So a
program can read and write a Beta file with nothing else. 0.x keeps its
databases in SQLite, as the [FORMAT.md](../FORMAT.md) at the root describes,
and its export format, which the Beta reads and writes too, is in
[EXPORT.md](../EXPORT.md). The design behind this format, and the reasons
for it, are in [BETA.md](../BETA.md). [README.md](README.md) says how to
build and use the Beta.

The released format is version 1, the format of the Beta's first release,
[0.3.0-beta.1](https://github.com/hypercrux/hypercrux/releases/tag/v0.3.0-beta.1).
Every change to it from here raises the version, even one an older build
would report as damage, so a build refuses a file of a format version it
doesn't know, with an error that says so. Version 1 took in two such
changes before the release, while no build was out. One is the vector
block, a change of a kind a build from before it doesn't know, so that
build refuses a file that holds one, and reads every other file as before.
The other is the limit on text and bytes: a build since then reports a
value of more than 1,000,000,000 bytes as damage, which only an earlier
build could write. None of those earlier builds was released, and no build
misreads a file.

## The layout

```
header       68 bytes
batch 1      its length
marker 1     20 bytes
batch 2
marker 2
...
```

The first batch starts straight after the header, and each batch after
that straight after the marker before it.

Throughout the format:

- **Integers** have fixed widths and are little-endian: u16, u32 and u64
  are unsigned, of 2, 4 and 8 bytes, and i64 is signed, of 8 bytes in two's
  complement.
- **Real numbers** are IEEE 754 binary64 (f64), and vector values binary32
  (f32), stored as their bits, little-endian.
- **A string** is a u16 holding its length in bytes, then the bytes. Keys,
  table names, field names and link types are strings. Text and bytes values
  have a u32 length instead (see Values).
- **Checksums** are CRC32C, on the Castagnoli polynomial, with the standard
  start and final values. That's what Go's `crc32.Checksum(data,
  crc32.MakeTable(crc32.Castagnoli))` returns, and the processor computes it
  with an instruction of its own on x86 and ARM. The CRC32C of the nine bytes
  `123456789` is `0xe3069283`.

## The header

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | Magic number: `48 43 52 58 0d 0a 1a 0a`, the letters `HCRX` and then CR, LF, Ctrl-Z and LF |
| 8 | 4 | Format version, u32: 1 |
| 12 | 16 | Database ID |
| 28 | 8 | Generation, u64 |
| 36 | 8 | Continues from: generation, u64 |
| 44 | 8 | Continues from: sequence number, u64 |
| 52 | 4 | Continues from: checksum, u32 |
| 56 | 8 | Compacted part ends, u64 |
| 64 | 4 | Header checksum, u32: the CRC32C of bytes 0 to 63 |

- **The magic number** marks the file as HyperCrux's. As in PNG's, its last
  four bytes show up damage from a transfer that changes line endings or
  stops at a Ctrl-Z.
- **The database ID** is 16 random bytes, chosen when the database is
  created. Compaction keeps it, so every file a database moves through
  carries the same ID.
- **The generation** is 1 in a new database, and each compaction writes a
  file with the next one.
- **The last four fields** are set in a file written by compaction. The
  first three name the batch it continues from: the last batch of the file
  it replaced, by that file's generation and the batch's sequence number and
  checksum. When that file held no batches, the sequence number and
  checksum are 0. The fourth is the offset just past the compacted part,
  where the first commit after the compaction starts. In generation 1 all
  four are 0.
- **The header never changes** once it's written. A new header comes only
  with a new file.

A reader checks the header in this order, and refuses the file at the first
check that fails:

1. The file is at least 68 bytes long.
2. The magic number. A file that starts with `SQLite format 3` and a zero
   byte, SQLite's own header, is a 0.x database, which the Beta can't open:
   `hypercrux export` in 0.x and `hypercrux import` in the Beta move a
   database across.
3. The format version. A reader refuses a version it doesn't know, so an
   older HyperCrux never misreads a newer one's file.
4. The header checksum.
5. The generation is at least 1. In generation 1, the last four fields are
   0. In a later one, the continues-from generation is one less than the
   file's, and the compacted part ends at offset 68 or later, and no further
   than the end of the file.

A header that fails the checksum, or the last check, is damage. A reader
reports it and changes nothing in the file, since without a header nothing
after it can be checked. A file at the path always has a whole header, or
is empty, because of the way databases are created (see Writing).

## Batches

A batch is one commit.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | Magic number: `48 43 52 42`, the letters `HCRB` |
| 4 | 8 | Generation, u64: the file's |
| 12 | 8 | Length, u64: the whole batch's, from the magic number to the checksum |
| 20 | 8 | Sequence number, u64 |
| 28 | length - 32 | The changes, one after another |
| length - 4 | 4 | Checksum, u32: the CRC32C of every byte of the batch before it |

The first batch in a file has sequence number 1, and each batch after it the
next number, through a compaction's batches and the commits after them
alike.

A batch **counts** when all of this holds:

- it starts with the magic number;
- it carries the file's generation;
- its length is at least 36, the size of a batch holding the smallest
  change, and it ends within the file;
- its sequence number is one more than the batch before it, or 1 for the
  first;
- its checksum matches.

A batch that a crash cut short fails its checksum, all but certainly, so a
batch that counts holds what its writer wrote. If its changes then aren't
well formed, as Changes describes, or break the rules there, or there are
none, the file is damaged, or comes from a build of HyperCrux that writes a
format this one doesn't know. Either way it's reported, and nothing is cut.

The magic number isn't zero, so a run of zero bytes never passes as a batch,
and a reader that finds zeros where a batch should start knows at once that
it isn't one.

A batch that would end past the end of the file is incomplete. While its
writer holds the write lock, that's a commit under way; otherwise a crash
cut it short.

**How big a batch can be.** The length allows any size; the one limit is
that a batch ends within the file. A reader checks a length against the
file's size before it reads or allocates anything for the batch. A batch is
built in memory and written whole, so a very large transaction costs its
writer that much memory, and every other process a read and a checksum of
all of it before it can apply any.

## Markers

Once a batch is synced, its writer adds a marker straight after it:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | Magic number: `48 43 52 4d`, the letters `HCRM` |
| 4 | 8 | The batch's sequence number, u64 |
| 12 | 4 | The batch's checksum, u32 |
| 16 | 4 | Check value, u32: the CRC32C of the database ID, then the file's generation as a u64, then bytes 0 to 15 |

A marker is **whole** when its magic number and its check value are right.
A batch is **marked** when it counts and a whole marker naming its sequence
number and checksum follows it directly. Other processes apply a batch only
once it's marked, so they never see a commit before it's on disk, or take a
batch for another that replaced it after a failure.

- A half-written marker fails its check value, so it never counts.
- The database ID and the generation in the check value keep a marker from
  another database, or from an earlier generation of this one, from
  passing for one of this file's, when a copy of that file sits inside a
  stored value for example. A database made by copying another's file keeps
  its ID, so the check can't tell those two apart.
- A marker needs no sync of its own. A synced batch left without its marker
  gets one when the next writer checks the end of the log.
- A whole marker that names any batch but the one before it can't come from
  a crash, so it's damage.
- A marker survives damage to its batch. That makes markers the proof that
  a commit was made, which is how a reader tells damage from a torn end (see
  Reading the log).

## Changes

Each change starts with a byte giving its kind, an ASCII letter, so a hex
dump shows which change is which.

| Kind | Byte | Change | Then |
|---|---|---|---|
| `T` | `54` | Create a table | the table's name (string), its vector size (u32), the number of fields (u16), then each field's name (string), in the table's order |
| `P` | `50` | Put fields | the key (string), the number of fields (u16), then each field's name (string) followed by its value |
| `D` | `44` | Delete a record | the key (string) |
| `L` | `4c` | Add a link | the key the link is from, its type and the key it's to (strings) |
| `U` | `55` | Remove a link | the same as for adding one |
| `X` | `58` | Drop a table | the table's name (string) |
| `V` | `56` | Vector block | the table's name (string), the vector size (u32), the number of vectors (u32), then each record's key (string), then each record's vector, as Vector blocks describes |

Changes apply in order, each to the state the ones before it left, and each
has to be valid there:

- **Create a table.** No table of that name exists. The new one starts
  with the vector size and fields given, and no records. The field names
  follow the rules for them, and no two match regardless of case. A vector
  size of 0 means none yet; any other is at most 65,536, and then one of the
  fields is the vector field. A put into a table that doesn't exist yet is
  written after a create change with size 0 and no fields. Compaction, and
  any writer that knows a table's whole shape from the start, such as an
  import, writes its whole field list and its vector size instead.
- **Put fields.** The table exists. The record is created if it doesn't
  exist, even when the put names no fields. Then each field given is set to
  its value, and fields the put doesn't name keep theirs. Null clears a
  field.
  - The fields come in byte order of their names as written, and no two of
    them match regardless of case.
  - A field the table has is written exactly as the table spells it. Any
    other name is a new field, which joins the end of the table's field
    list, in the order the put gives them, even when its value is null.
    Byte order is the order 0.x adds a put's new fields in.
  - The vector field is the one whose name matches `vec` regardless of
    case. It holds a vector or null, and no other field holds a vector. A
    table's first vector sets its vector size, if that's 0, and every vector
    after it must have that many values.
  - Text and bytes hold at most 1,000,000,000 bytes each, though their
    length can say more (see Names and limits).
  - A table holds at most 65,535 fields, the most a create change can
    carry, so HyperCrux refuses a put that would add one more.
- **Delete a record.** The record exists. It goes, with every link to or
  from it. The change doesn't list the links: every reader works out the
  same ones from the same state.
- **Add a link.** Both records exist, and the link isn't there already.
- **Remove a link.** The link is there.
- **Drop a table.** The table exists. It goes with its records, every link
  to or from them, its field list and its vector size. A later put into a
  table of the same name needs a new create change first.
- **Vector block.** Only a compacted part holds one. It gives records of one
  table their vectors, by the rules in Vector blocks.

Puts are always written, even when they set the values a record already
has. Any other change that would do nothing isn't written: adding a link
twice writes one change, a delete of a record that isn't there fails before
anything reaches the file, and a transaction that changes nothing writes no
batch. Removing every link from one record to another, whatever their
types, writes one remove change for each, in byte order of type.

## Values

Each value in a put starts with a byte giving its kind, again an ASCII
letter.

| Kind | Byte | Value | Then |
|---|---|---|---|
| `n` | `6e` | Null | nothing |
| `i` | `69` | Whole number | i64 |
| `r` | `72` | Real number | f64 |
| `t` | `74` | Text | the length in bytes (u32), then the bytes |
| `b` | `62` | Bytes | the length (u32), then the bytes |
| `v` | `76` | Vector | the number of values (u32), then each value as f32 |

- Reals and vector values keep their bits, so -0 and subnormal numbers come
  back exactly. Neither may be NaN or infinite.
- A vector has 1 to 65,536 values, and they aren't all zero. -0 counts as
  zero.
- Text is valid UTF-8, as 0.x requires, and may hold zero bytes.
- Text and bytes are different kinds, as they are in 0.x and in its export.

## Names and limits

Keys and names follow 0.x's rules, which the Beta keeps:

| | Rule | Length |
|---|---|---|
| Table name | lower-case letters, digits and underscores, starting with a letter, and not starting with `hc_` or `sqlite_` | 1 to 63 bytes |
| Key | a table name, a colon, and at least one more byte, in UTF-8 without zero bytes | 3 to 1,024 bytes |
| Field name | letters, digits and underscores, starting with a letter or an underscore, and not `key`, `rowid`, `oid` or `_rowid_` in any case | 1 to 64 bytes |
| Link type | UTF-8 that doesn't start with a zero byte | 1 to 200 code points |

The format's own widths set the other limits. A table has at most 65,535
fields, and a put sets at most 65,535. A text or bytes value's length can
say up to 4,294,967,295 bytes, and a vector block holds at most
4,294,967,295 vectors. HyperCrux's own limits are lower, and both are 0.x's,
from SQLite. A put can't take a table past 1,999 fields, though a batch in
the file can take it to 65,535, so that limit can rise without a change to
the format. A text or bytes value holds at most 1,000,000,000 bytes, in a
batch in the file too, so a put of a longer one is damage.

## Compaction

Compaction writes a new file holding only the live data, and then renames
it over the database. Only the holder of the write lock compacts, and it
holds the lock until the new file has taken the path, so the old file never
changes again, and a copy taken at any moment holds the one file or the
other. Compacting is also how deleted data leaves the disk, the job
`VACUUM` does in 0.x.

### When HyperCrux compacts

HyperCrux compacts at the end of a commit, still holding the write lock,
once the commits since the last compaction have made half the file dead,
and a megabyte at least, or once 100,000 commits follow the compacted part.
`Compact()` in Go and `hypercrux compact` compact on demand.

Dead bytes are the changes in the file that no longer reach the state,
which a compaction leaves out: a put's old values, a deleted record with its
links, a dropped table with its records, and a removed link with the change
that removed it. Each process counts them in its copy as it applies the
changes, and counts the commits from the sequence numbers, so every
HyperCrux process takes the same choice from the same file, and a
compaction starts both counts from nothing. A batch's head, its checksum and
its marker never count, so a database that only grows is compacted only at
the limit on commits. [tasks/C1.md](tasks/C1.md) sets out what each change
makes dead, byte by byte.

That rule is HyperCrux's own, and the format doesn't need it. A writer may
compact whenever it holds the write lock, or never, and every reader takes
a compacted file the same way, by the rules below. When a compaction fails
before its rename, HyperCrux holds off until the file has grown as much
again as it had since its last compaction, and each process keeps that
hold-off to itself.

### What a compacted file holds

The new file has a header with the next generation and its last four fields
set, then the compacted part: ordinary batches, each with its marker,
holding

- every table, in byte order of its name, as a create change with its vector
  size and its whole field list, then a put for each of its records, in byte
  order of their keys, carrying every field that holds a value apart from
  the vector, and no nulls, then the vectors of the records that hold one,
  in vector blocks, in byte order of their keys (see Vector blocks);
- then every link, as an add change, in byte order of the key it's from,
  then its type, then the key it's to. That's the order of whole keys,
  which can differ from the order of their tables: `users2:zed` comes
  before `users:ann`, since a digit sorts before a colon.

The writer splits the compacted part into batches as it likes, keeping each
change whole, so it can stream a large database out without building it all
in memory. HyperCrux gives each vector block a batch of its own, filled to
about a megabyte, so a table's vectors take a run of blocks. The batches are
numbered from 1, and the first commit after them takes the next number,
which a reader finds in the marker just before the end of the compacted
part, or is 1 when nothing was compacted. The header's last field points
just past the last compacted batch's marker; a database with no tables has
no compacted batches, and then it's 68. The writer only knows that offset at
the end, so it can write the header last.

Nobody can read the new file before the rename, and the file is synced
once before it, so the compacted part's markers need no sync of their own
either.

### The steps

The holder of the write lock compacts in five steps:

1. It creates `NAME.compact` beside the database, where NAME is the
   database's file name, with the database's permissions and, where it's
   allowed to, its owner and group, and takes `flock` on it before it writes
   anything, so the new file is locked before anyone can see what's in it.
   The creation fails if a file of that name is there already; the writer
   then removes it if it can lock it, as below, and creates it again.
2. It writes the current state into it, as the section above describes,
   and the header last, once it knows where the compacted part ends. Each
   table's create change carries its vector size and its fields in order,
   so empty tables and vector sizes come through.
3. It syncs the new file.
4. It checks that the file at the path is still the one it locked, by
   device and inode number, so a backup moved into place while it ran is
   left in place, and that the database still keeps the rules for the file:
   one name, at a path whose last element hasn't become a symbolic link. A
   move that lands between that check and the rename is replaced all the
   same, and the restore is lost without an error. Then it renames the new
   file to the database's path and syncs the folder. This rename is the
   switch: before it, the database is generation N; after it, N+1.
5. It moves over to the new file, whose lock it holds now as the write
   lock, and closes the old one, which lets go of the old file's lock. It
   lets go of the new file's lock as it would at the end of a commit. A
   writer that was waiting for the old file's lock gets it, finds that the
   path names another file, and starts again there.

If creating, writing or syncing the new file fails, because the disk is
full for example, or the check or the rename fails, the writer removes
`NAME.compact` and carries on with the old file, which is still whole. A
commit made under the same lock before the compaction has succeeded either
way. If the folder's sync after the rename fails, nobody can tell which file
a power cut would leave at the path, so the writer keeps the write lock and
writes nothing more until it closes the database, as after a failed commit
it can't cut back (see Writing). Each writer syncs the folder once before
its first commit into a file whose name it didn't make last itself, so a
commit never relies on a rename that a power cut could take back, whoever
made the rename and however its folder sync went.

A crash before the rename leaves the old file whole, and a leftover
`NAME.compact`. A crash after it leaves the new file whole.

### `NAME.compact`

- Only the holder of the write lock creates `NAME.compact`, and it holds
  `flock` on it for as long as the compaction runs.
- The next holder of the write lock removes a leftover one, once it has
  checked that the file it locked is still the one at the path, and only
  when it can take `flock` on `NAME.compact` itself, since one that's
  locked is a compaction under way. Nobody else ever removes it.
- A writer waiting for the write lock looks for `NAME.compact` between its
  tries, and while one is there and locked, it keeps waiting past its usual
  10 seconds, as "The write lock and its waits" says.

### Reading a compacted file

A reader applies the compacted part like any other batches. A put in it
can carry its record's vector in place of a vector block, and a reader
takes either. A compacted file is whole before anyone can see it, so a file
shorter than its compacted part, or a log that ends inside it, is damage,
and nothing before the end of the compacted part is ever cut. The
continues-from fields aren't needed to read the file. They're there for a
later version that has the old file open, so it can carry on after a
compaction without reading the new file from the start: the state at the end
of the compacted part is the state after the batch the header names.

## Vector blocks

A vector block gives records of one table their vectors, back to back, as
the table's array in memory holds them, so a reader puts them straight into
its own array. Compaction writes them, after each table's puts, and nothing
else does.

| Size | Field |
|---|---|
| 1 | Kind: `V`, `56` |
| string | The table's name |
| 4 | The vector size, u32 |
| 4 | The number of vectors, u32 |
| a string each | Each record's key, in byte order |
| 4 times the vector size, each | Each record's vector, in the order of the keys, each value an f32 |

The keys come first and the vectors after them, so the values are one run of
bytes. A block names each record it gives a vector, and leaves out the
records without one, so every vector says whose it is. The norms aren't in
the block: a reader works each one out from the values, in the pass that
checks them.

A block applies to the state the changes before it left, as any change
does, and has to be valid there:

- The table exists, and its vector size is the block's, which is 1 to
  65,536.
- The block holds one vector at least. Each key belongs to the table, and
  the keys come in byte order, so none comes twice.
- Each key names a record of the table that holds no vector, and the vector
  becomes that record's. Its values follow the rules for a vector's values:
  none is NaN or infinite, and they aren't all zero.

A vector block anywhere but in a compacted part is damage: in a batch past
the compacted part, or in a file of generation 1, which has none. So is a
block that breaks the rules above. The batch's checksum covers a block as it
covers any change, and the syntax shows a count that disagrees with the keys
and the values given, since the change then runs past the end of its batch,
or leaves bytes over.

A build from before vector blocks reads one as a change of a kind it doesn't
know, which is damage, so it refuses the file.

## Reading the log

A reader takes batches in order from offset 68, each followed by its
marker. The log ends at the first batch that isn't marked, or at the end of
the file. Whatever comes after the last marked batch is a commit under way,
the remains of one a crash cut short, or damage.

- **Opening** applies every marked batch in order, checking each checksum
  on the way. A batch that counts but whose changes are malformed or break
  the rules is damage, and opening fails. Then opening looks past the end of
  the log, as below, so damage in the middle of a file is never taken for
  its end. Only then does it try the write lock, without waiting. If it
  gets the lock, it checks the end of the log, as the next section says.
  If not, whoever holds the lock checks before it appends anything.
- **A reader that keeps the database open** makes one `stat` call on the
  database's path before each read. When the path names another file than
  the one it has open, by device and inode number, a compaction or a backup
  moved into place has replaced it, and the reader reads the new file from
  its start, as on opening, having dropped what it held of the old one.
  Linux keeps a replaced file alive for as long as someone has it open, so
  a read already under way finishes on the old file. Otherwise, when the
  file is longer than the point it has applied, it reads on from where it
  stopped. For each batch it reads the first 28 bytes, then checks for a
  whole marker at the batch's end before reading the rest, so it never reads
  a commit still being written. It applies each batch that's complete and
  marked, stops at the first that isn't, and keeps nothing unmarked for next
  time. When it stops with bytes still past its point, it tries the write
  lock without waiting, and if it gets it, the writer is gone, so it checks
  the end of the log, as below. And when a whole marker names the batch it
  stopped at, but the batch fails its checks, it waits for the write lock,
  reads the batch again, and reports damage if it still fails. It does the
  same when a whole marker after a batch that counts names another batch,
  or the same one with another checksum. Before it waits, it reads the
  batch once more, head first, since a read that brought in the batch and
  its marker together can have caught the batch as its writer finished it.
  A file shorter than the point it has applied is damage too, or a copy
  over the live database.

### Looking past the end of the log

A reader searches from the end of the log to the end of the file for the
marker magic number, at every offset. A whole marker found there whose
sequence number is at least the next one the log expects shows that a commit
was made past the end of the log, so the file is damaged. The rule about the
sequence number keeps an older copy of this database, stored as a value
inside a torn batch, from passing for a commit.

In a compacted file, a log that ends before the compacted part does is
damage too, since a compacted file is whole before anyone can see it.

What lies past the end of the log can change while a writer is at work: a
marker can appear behind a batch that was half written a moment earlier,
and a failed commit's batch can be cut and another written in its place. So
before it reports damage, a reader takes the write lock, waiting for it as a
writer does, and reads the file again while nothing can change it. A reader
treats a batch that counts followed by a whole marker naming another batch
the same way. Holding the lock, it checks the end of the log as below, as
when it gets the lock at once. If another file has taken the path by then,
it reads that one afresh. If the wait runs out, it reports a timeout, since
nothing has confirmed the damage.

## Checking the end of the log

Nothing is cut, and nothing is written again in place, without the write
lock. Its holder checks the end of the log before it appends anything; so
does opening once it holds the lock, whether it got it at once or waited for
it to read again what looked like damage, and so does a reader that finds it
can take the lock, because the writer is gone. The check, in this order:

1. Looks past the end of the log for damage, as above: a whole marker
   naming the next sequence number or a later one, or in a compacted file a
   log that ends before the compacted part does. If it finds damage, it
   reports it and changes nothing.
2. Looks at the first batch after the last marked one. If it counts:
   - when a whole marker that names another batch follows it, that's
     damage;
   - when its changes are malformed or break the rules for a change on its
     own, that's damage too;
   - otherwise a writer left it there, having died or stuck after a failed
     commit, before or after the batch's sync, with nothing or a torn marker
     after it. The batch is written again in place, with the marker before
     it, synced, and marked. It's written again because after a failed sync,
     Linux can mark its pages clean, and a sync on its own could then report
     success without the batch ever reaching the disk. The marker before it
     may have been written after the last sync that worked, so the same goes
     for its page. Before the first batch in the file comes the header
     instead, which was synced when the database was made.

   Changes that break the rules for the state they apply to are found once
   the batch is marked, since only marked batches are applied, and they're
   damage then.
3. Cuts off everything after the last marker, as a writer cuts back a
   failed commit (see Writing): it writes the last marker again in place and
   syncs, then cuts and syncs the cut. Before the first batch in the file
   comes the header instead, and only the cut and its sync are made.

A failure while writing the batch again, syncing it, marking it or cutting
stops the check, and the writer lets go of the lock, leaving the end of the
log for the next check. That check finds the same end, or a cut that's
complete with the last marker on the drive, and starts again. Each step
writes only what the file holds already, so it can be made again, and a
batch whose commit may have succeeded is never cut. In a compacted file,
step 1 stops the check whenever the log ends before the compacted part
does, so a cut never reaches into the compacted part.

What the check finds after the last marked batch, and what follows:

| Found | Meaning | What happens |
|---|---|---|
| Nothing | The log is whole | Nothing |
| Zeros, or the start of a batch, and no whole marker further on naming the next sequence number or a later one | A commit a crash cut short | It's cut off |
| A batch that counts, with changes that keep the rules for a change on its own, and nothing or a torn marker after it | A writer died after writing it, or stuck after a failed commit | It's written again with the marker before it, synced and marked, and anything after it is cut off |
| A batch that counts, then a whole marker naming another batch | Damage | Reported |
| A batch that counts, with changes that are malformed or break the rules for a change on its own | Damage, or a format this build doesn't know | Reported |
| A batch the check has just marked, with changes that break the rules for the state they apply to | Damage, or a format this build doesn't know | Reported, and the marker stays |
| A whole marker further on, naming the next sequence number or a later one | Damage, once a read under the write lock agrees | Reported |
| In a compacted file, a log that ends before the compacted part does | Damage | Reported |

Once damage is found, nothing more is written: opening fails, reads return
a damage error, and `hypercrux check` names the batch.

## Writing

One process writes at a time, and a program that writes a Beta file has to
keep to all of this. Reading takes no lock.

### The write lock and its waits

The write lock is an exclusive `flock` on the database file.

- **Taking it.** HyperCrux first takes a mutex inside its process, so two
  goroutines never write through one open file at once, then `flock`,
  trying without waiting and pausing between tries, for up to 10 seconds
  for the two together, as 0.x waits for SQLite's lock. When the wait runs
  out, the commit fails before anything is written, with an error that
  wraps `ErrLockTimeout`. A program of its own takes `flock` through a file
  it opened itself, and lets one thread at a time write through it, since
  every thread that shares an open file shares its lock.
- **While a compaction runs,** writers wait it out, the one whose commit
  set it off included. Between its tries, a waiting writer looks for
  `NAME.compact`, and while one is there and locked, it keeps waiting past
  the 10 seconds, until 10 seconds after it last found it locked. So a
  writer has time to get the lock as a compaction lets go, and one stuck
  after its rename holds writers up no longer than a stuck commit does.
- **Holding it,** the writer checks that the file it locked is still the
  one at the path, by device and inode number. If a compaction or a backup
  moved into place has replaced it, the writer lets go, reads the new file
  from its start, and starts again on it.
- **Letting go** goes in the reverse order: `flock` first, then the mutex.
- **No child process holds it.** Go opens every file with `O_CLOEXEC` and
  can't fork without exec, so no child process can end up holding the
  lock. A program in another language needs the same care, since a child
  that keeps the open file keeps its lock.

### A commit

A writer:

1. takes the write lock, as above, and checks that the last marker it
   applied is still where it was. A replaced file is read afresh before
   anything is written. A marker that has changed, or a file shorter than
   the point the writer has applied, can't come from HyperCrux, since
   nothing before a marker ever changes: the file was copied over or
   damaged, and the writer reports damage;
2. checks the end of the log, as above, which reads on to its end, so the
   writer applies every batch it hasn't applied yet and makes its changes
   against the latest state;
3. checks each change against that state, as Changes describes, and writes
   nothing if one breaks a rule;
4. appends its batch, having synced the folder first when this is its first
   commit into a file whose name it didn't make last itself, syncs the file,
   and appends the marker.

### A commit that fails

If appending, syncing or writing the marker fails, the writer cuts the file
back to just after the last marker before it lets go of the lock. It writes
the last marker again in place and syncs, since a failed sync can mark the
marker's page clean without it reaching the drive while reads go on seeing
it; then it cuts the file and syncs the cut. With the sync before the cut,
a writer that stops partway leaves either the end it was cutting, which the
next check cuts again, or the last marker on the drive. If any of that
fails, the writer keeps the lock and writes nothing more until the database
is closed, so no other process can write meanwhile. The next writer's check
then deals with what's left as with what a crash leaves: it marks the
failed batch when it counts, since the commit's outcome is unknown, and
cuts it otherwise.

So an error from a commit means the commit isn't in the file, unless the
writer is stuck that way, when its outcome is unknown. Once its commit is
in the file, HyperCrux's `Update` returns nil, even when letting go of the
lock fails after it, so a caller never makes a commit twice; the lock then
goes at the handle's next commit, or when it closes. A power cut can still
land between a failed sync and the cut, but then no caller is left to see
the error.

A writer works from the database's real path, with every symbolic link
resolved, and writes to no file with more than one name, since a compaction
replaces the file at one path only (see The rules for the file).

### Creating a database

A new database starts as a file holding only its header, written beside the
database under a name of its own, the database's name with `.new-` and
random letters added. The creator takes `flock` on it before writing, syncs
it, then renames it to the database's path in a way that fails if a file has
appeared there meanwhile (on Linux, `renameat2` with `RENAME_NOREPLACE`),
syncs the directory, and only then lets go of the lock. Until the directory
sync makes the rename last, nobody else can lock the new database, so nobody
can commit to a file a power cut could still take away. A creator whose
rename fails removes its own file and opens the database at the path
instead. On a file system without that kind of rename, creating a database
fails, since a plain rename could replace a database that appeared
meanwhile. So a file at the path always has a whole header, or is empty, and
only ever one name. A crash can leave a `.new-` file behind. The next holder
of the write lock removes any it can lock, and leaves alone one it can't,
which is a creation under way.

### An empty file

An empty file, made with `touch` for example, holds no database yet. A
writer makes one of it while holding the write lock on the empty file, once
it has checked that the file is still the one at the path and still empty:
it writes a new file in the same way, under the same lock rules, and renames
it over the empty one. Anyone who had the empty file open notices the new
one, as after a compaction.

## Copies

Bytes before the end of a marker never change again, since a cut only ever
goes back to just after the last marker. So a copy that stops at the end of
a batch marked before the copy began is always a good database. A plain
copy of the whole file can catch the end of the log while it's cut and
written again, after a failed commit or by the check after a crash, and
then hold the start of one batch and the end of another, which reads as
damage. Copying again gives a good copy.

### Backing up and restoring

- **To back up,** copy the file somewhere else, while it's in use if need
  be. Commits only add to the end of the file, and compaction swaps in a
  finished file in one step, so a copy taken at any moment is a database up
  to some commit, apart from the rare copy above that reads as damage.
- **To restore,** stop the processes using the database and copy the
  backup in, or copy it beside the database and move it into place with
  `mv`, which renames it over the database. Running processes notice the
  new file at their next read or commit, and read it from its start.
- **What a restore can lose.** A commit that races the move is lost, along
  with everything after the backup. A compaction that races it can undo the
  restore, by renaming the old data over the backup, as step 4 of the
  compaction says. The file's owner and the folder's sync are up to
  whoever moves it.
- **Never copy anything over a live database.** A process that has read
  the file finds bytes it read changed under it, which it takes for damage.

## The rules for the file

A Beta database keeps its promises only on a file system and a drive that
keep theirs. The Beta runs on Linux, and these rules are Linux's.

- **Local file systems only.** Network file systems such as NFS and SMB,
  and folders shared into containers through a virtual machine, don't keep
  the locks honest.
- **The file's data in step with its size.** The file system has to keep a
  file's data in step with its size through a power cut, as ext4 does by
  default (data=ordered), and XFS and btrfs do. ext4 mounted with
  data=writeback can show a file's old blocks at its end after a power cut,
  and those can hold a batch cut back after a failed commit, which the next
  check would take back in.
- **Syncs that reach the drive.** On Linux, a sync returns only once the
  data is on the drive, and drives that honour cache flushes keep it
  through a power cut. That one rule covers commits and compaction alike.
- **The real path.** A database is opened by its real path, with symbolic
  links resolved and a relative path made absolute, and a new one by its
  folder's real path and its name.
- **A regular file with one name.** A file with more than one hard link is
  refused, because compaction can only replace the file at one path, and so
  is anything else at the path, such as a folder or a FIFO, before anything
  opens it. A symbolic link that leads to nothing is refused too, since a
  new database renamed into place would replace the link. A file that
  breaks these rules and takes the path later is refused at the reload, a
  hard link made while the database is open stops its writes until it's
  gone, and a path whose last element has become a symbolic link stops a
  compaction before its rename.
- **A torn end that won't read.** A drive can leave a sector that a power
  cut tore failing its own check, so a read of it fails with an I/O error
  until something writes it again. In the torn end of the log, that fails
  every open and every check with the read's error. Nothing is cut and
  nothing is lost, and the database can't open until the end is cut back by
  hand: keep a copy of the file, then cut it to just past the last marker
  that reads, with `truncate` for example.

## What isn't in the file

Opening rebuilds everything else from the log: the table of keys, each
table's keys in byte order, the index of links into each record, and the
vectors' norms. So none of it can ever disagree with the records.

## Fixtures

[internal/format/testdata](internal/format/testdata) holds a file of
annotated hex for each part of the format: a new database's header, a
compacted one's, a batch for each change, a batch of several changes, a
marker, and two small databases, one new and one compacted. Every field
has a comment. `go test ./beta/internal/format` checks them: their
checksums against Go's `hash/crc32`, their lengths, and every change against
the rules above. It also checks that the compacted database holds the same
state as the new one it was compacted from, in the order a compacted part
takes.
