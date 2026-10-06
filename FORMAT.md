# The HyperCrux file format, version 1

A HyperCrux file is an SQLite 3 database. Records live in ordinary tables,
and four small tables of HyperCrux's own keep track of keys, links and vector
sizes. The rules that tie them together are SQLite triggers stored in the
file, so they hold for every program that writes it, whichever language it's
in. This document describes the layout and the rules, so a program can read
and write a file next to the `hypercrux` command and the Go package with
nothing but SQLite.

[examples/python/hcfile.py](examples/python/hcfile.py) does all of it with
Python's standard library.

## What stays the same

The format version is stored in the file, in `hc_meta` under
`format_version`. This document describes version 1.

Within version 1, later releases may add indexes, `hc_meta` entries and
columns with default values. They won't remove or rename anything, and they
won't change what an existing table, column or trigger means. A program
written against this document keeps working.

A change older programs couldn't handle safely gets a new version number.
HyperCrux refuses to open a file whose version is newer than it knows, and
other programs should do the same.

## The file

- **SQLite 3, in WAL mode.** HyperCrux switches a file to WAL when it opens
  it, and SQLite records that in the file. Readers carry on while a writer
  commits.
- **The header mark.** A file HyperCrux creates for itself has
  `application_id` set to `0x48435258` (1212371544), the letters "HCRX".
  HyperCrux's tables can also sit inside an application's own database, and
  then `application_id` stays as the application set it.
- **One machine.** Many processes on one machine can share a file. SQLite's
  locking doesn't work over network file systems, so keep the file on a local
  disk.
- **SQLite 3.24 or later** for writers, which need upserts
  (`INSERT ... ON CONFLICT ... DO UPDATE`). Any SQLite 3 can read the file.

## Records

A record is a row in a **record table**.

- **Table names** are lower-case letters, digits and underscores, start with
  a letter, run up to 63 characters, and don't start with `hc_` or `sqlite_`.
- **The key** is a column called `key`, the table's `TEXT PRIMARY KEY` and
  its only primary key column. Keys are written `table:id`: the table's name,
  a colon, then anything at all, up to 1,024 bytes in total. `docs:7`,
  `docs:2026-10-06/report.pdf` and `customer:dana@example.com` are all keys of
  their tables. Because the table name leads the key, a key is unique across
  the whole file.
- **Fields** are the table's other columns, with whatever SQLite types you
  like. HyperCrux adds a column without a declared type the first time a put
  uses a new field name.
- **The vector**, if a record has one, is in a column called `vec`: a blob of
  float32 values, little-endian, four bytes each. Every vector in a table has
  the same number of values, set by the first vector stored, and a vector
  can't be all zeros. NaN and infinite values aren't allowed either; the
  triggers can't see those, so writers have to keep them out, and
  `hypercrux check` reports any that get in.
- **Links** join two records, one way, with a type such as `owns` or `cites`.

A table becomes a record table when HyperCrux creates it on a first put, or
when `hypercrux adopt` (`Adopt` in Go) takes over a table someone created with
plain SQL. Tables HyperCrux hasn't adopted are left alone.

## HyperCrux's tables

```sql
CREATE TABLE hc_meta (name TEXT PRIMARY KEY, value TEXT) WITHOUT ROWID;
CREATE TABLE hc_tables (name TEXT PRIMARY KEY, dims INTEGER) WITHOUT ROWID;
CREATE TABLE hc_keys (key TEXT PRIMARY KEY, tbl TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE hc_links (
  src  TEXT NOT NULL,
  type TEXT NOT NULL,
  dst  TEXT NOT NULL,
  PRIMARY KEY (src, type, dst)
) WITHOUT ROWID;
CREATE INDEX hc_links_in ON hc_links (dst, type, src);
```

| Table | Holds |
|---|---|
| `hc_meta` | `format_version`, which is `1` |
| `hc_tables` | One row per record table. `dims` is its vector size, NULL until the first vector |
| `hc_keys` | One row per record: its key and its table. The triggers keep it, and links point into it |
| `hc_links` | One row per link, from `src` to `dst`. The index serves links followed backwards |

Read these tables freely. Write to `hc_links` to add or remove links. Leave
`hc_keys` and `hc_tables` to the triggers; the only write HyperCrux expects
from outside is setting `hc_tables.dims` back to NULL for a table that no
longer holds any vectors, to let it take vectors of a new size.

## The triggers

HyperCrux's own:

| Trigger | Rule |
|---|---|
| `hc_links_insert` | A link needs a type of 1 to 200 characters, and both of its keys in `hc_keys` |
| `hc_links_update` | Links aren't changed in place. Delete one and insert another |
| `hc_keys_delete` | When a key goes, every link to or from it goes too |
| `hc_keys_update` | `hc_keys` isn't changed in place |

Each record table `T` has:

| Trigger | Rule |
|---|---|
| `hc_T_insert` | A new row's key is text that starts with `T:` and has something after the colon. The key goes into `hc_keys` |
| `hc_T_delete` | A deleted row's key leaves `hc_keys`, which takes its links with it |
| `hc_T_key` | A key never changes. Delete the record and insert it again |
| `hc_T_vec_insert`, `hc_T_vec_update` | Only with a `vec` column. A vector is a non-empty blob of whole float32 values, not all zero, the size in `hc_tables.dims`. The first vector sets that size |

The full SQL is in [schema.go](schema.go). A statement that breaks a rule
fails with a message that starts with `hypercrux:`, and nothing it did is
kept.

One SQLite quirk to know: `INSERT OR REPLACE` replaces a row by deleting it
first. With `PRAGMA recursive_triggers` on, that delete fires the record's
delete trigger and its links go. Use an upsert instead to update a record
and keep its links.

## Writing from another language

Each of these is a transaction. Start it with `BEGIN IMMEDIATE`, which takes
the write lock before reading, and set `PRAGMA busy_timeout` so a writer waits
for another instead of failing straight away.

**Put a record** with an upsert, naming only the fields you're setting:

```sql
INSERT INTO docs (key, title, vec) VALUES ('docs:7', 'Q3 plan', ?)
ON CONFLICT (key) DO UPDATE SET title = excluded.title, vec = excluded.vec;
```

The table and its columns have to exist already. Create a new table with a
`key TEXT PRIMARY KEY` column and adopt it, or add a column with
`ALTER TABLE ... ADD COLUMN`. After adding a `vec` column with SQL, run
`hypercrux adopt` on the table again so its vector triggers go in.

**Link two records**, which must both exist:

```sql
INSERT OR IGNORE INTO hc_links (src, type, dst) VALUES ('customer:42', 'owns', 'docs:7');
```

**Unlink** with a `DELETE` from `hc_links`. **Delete a record** with a plain
`DELETE` from its table, and its links go with it.

## Reading from another language

Fields are plain columns. A vector is `len(vec) / 4` float32 values. The
nearest vectors are found by comparing every vector with the query, which is
what HyperCrux does too; it ranks by cosine distance, `1 - dot(a, b) /
(|a| |b|)`, computed in float64 from the float32 values, closest first and
ties in key order.

To walk links, use a recursive query. This one gives every record within two
links of `customer:42`, following links forwards, with the fewest links
needed to reach it:

```sql
WITH RECURSIVE w(key, depth) AS (
  SELECT 'customer:42', 0
  UNION
  SELECT l.dst, w.depth + 1 FROM w JOIN hc_links l ON l.src = w.key WHERE w.depth < 2
)
SELECT key, min(depth) FROM w WHERE key <> 'customer:42' GROUP BY key ORDER BY 2, 1;
```

## SQL functions on HyperCrux's connections

The Go package and the `hypercrux` command add three functions to every
connection they open. Other programs don't have them, but can compute the
same things as above.

| Function | Returns |
|---|---|
| `distance(a, b)` | The cosine distance between two vectors, from 0 to 2. Each can be a stored blob or a JSON array such as `'[0.1, 0.8]'`. NULL if either is NULL |
| `vector(json)` | A JSON array of numbers as a stored vector blob |
| `walk(key, depth [, type [, direction]])` | The keys within `depth` links of `key`, nearest first, as a JSON array. `type` limits the links followed, NULL for any. `direction` is `out` (the default), `in` or `both` |

`walk` returns JSON so that `json_each` can turn it into rows, which lets one
statement cross keys, fields, links and vectors:

```sql
SELECT d.key, d.title
FROM json_each(walk('customer:42', 2)) w
JOIN docs d ON d.key = w.value
WHERE d.status = 'open'
ORDER BY distance(d.vec, '[1, 0, 0]')
LIMIT 10;
```
