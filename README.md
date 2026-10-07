# HyperCrux

**One record, four handles: key, SQL, links and similarity, in one SQLite file.**

[HyperCrux](https://hypercrux.com) is a small database where every record can
be reached four ways. Look it up by its key. Query it with SQL. Follow its
links to other records. Or find the records whose vectors are closest to a
question. All four work on the same records, in the same file and the same
transactions, so they can't drift apart.

There's no server to run. Embed HyperCrux in a Go program, or use the one
small `hypercrux` binary from a script or any other language. The file is
plain SQLite, and the rules that keep it consistent are stored inside it, so
even a program that has never heard of HyperCrux reads and writes it safely.

Version 0.1.0 · Apache License 2.0 · [hypercrux.com](https://hypercrux.com)

- [Why one file for all four](#why-one-file-for-all-four)
- [Quick start](#quick-start)
- [Use it from Go](#use-it-from-go)
- [Keys, fields, links and vectors](#keys-fields-links-and-vectors)
- [One statement across all four](#one-statement-across-all-four)
- [Use it from any language](#use-it-from-any-language)
- [The hypercrux command](#the-hypercrux-command)
- [What HyperCrux promises, and what it doesn't](#what-hypercrux-promises-and-what-it-doesnt)
- [How it was tested](#how-it-was-tested)
- [Build it yourself](#build-it-yourself)
- [The name](#the-name)
- [License](#license)

## Why one file for all four

An app that uses AI tends to keep the same facts in three places. A regular
database holds the records. A vector database holds an embedding of each one,
so the app can find "documents like this question". A graph database holds who
is connected to what. Code in between copies every change from one to the
others, and when it misses one, the answers stop agreeing: a search turns up a
document that was deleted an hour ago, or a link points at nothing.

HyperCrux keeps one copy. A record is a row in a table. Its key names it, its
columns hold its fields, it can link to other records, and it can carry a
vector. Delete it and its links go in the same transaction. Change its vector
and the next search sees the new one. There's nothing to sync.

The price is size. Every search compares the question with every vector that
passes its filter. That keeps results exact and makes filters and
transactions simple, and it means HyperCrux suits thousands to around a
hundred thousand vectors per table, not millions. The numbers are under
[how it was tested](#how-it-was-tested).

## Quick start

Download the `hypercrux` binary for your system from the
[latest release](https://github.com/hypercrux/hypercrux/releases/latest) and
put it on your PATH. Then put some records in a file, with fields as JSON:

```sh
hypercrux put notes.db customer:42 '{"name": "Dana"}'
hypercrux put notes.db docs:1 '{"title": "Q3 plan", "status": "open", "vec": [0.9, 0.1, 0]}'
hypercrux put notes.db docs:2 '{"title": "Hiring notes", "status": "done", "vec": [0.1, 0.9, 0.1]}'
hypercrux put notes.db docs:3 '{"title": "Q3 budget", "status": "open", "vec": [0.8, 0.2, 0.1]}'
```

The part of the key before the colon is the table, so `docs:1` lands in a
table called `docs`. HyperCrux creates the table, and a column for each new
field, as it goes. `vec` is the record's vector. Real vectors come from an
embedding model and have hundreds of values; these have three to keep the
example short.

Link records, and walk the links:

```sh
hypercrux link notes.db customer:42 owns docs:1
hypercrux link notes.db customer:42 owns docs:2
hypercrux link notes.db docs:1 cites docs:3
hypercrux walk notes.db customer:42 2
```

```
1  docs:1
1  docs:2
2  docs:3
```

Find the documents closest to a question's vector, or to another record's:

```sh
hypercrux nearest notes.db docs '[1, 0, 0]' -k 2
hypercrux nearest notes.db docs docs:1 --where "status = 'open'"
```

```
0.0061  docs:1
0.0369  docs:3
```

And ask SQL anything, with the records in plain tables:

```sh
hypercrux sql notes.db "SELECT key, title FROM docs WHERE status = ?" open
hypercrux check notes.db
```

## Use it from Go

```sh
go get github.com/hypercrux/hypercrux
```

```go
import "github.com/hypercrux/hypercrux"

db, err := hypercrux.Open("notes.db")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

// Writes that belong together go in one transaction.
err = db.Update(func(tx *hypercrux.Tx) error {
	if err := tx.Put("docs:1", hypercrux.Fields{"title": "Q3 plan", "status": "open",
		"vec": hypercrux.Vector{0.9, 0.1, 0}}); err != nil {
		return err
	}
	return tx.Link("customer:42", "owns", "docs:1")
})

fields, err := db.Get("docs:1")                                   // by key
steps, err := db.Walk("customer:42", hypercrux.Out, "", 2)        // by link
hits, err := db.Nearest("docs", question, 10, "status = ?", "open") // by similarity
rows, err := db.Query("SELECT key, title FROM docs WHERE status = ?", "open") // by SQL
```

Inside `Update`, use `tx` and not `db`: the methods of `db` run outside the
transaction and don't see its changes.

Building needs cgo and a C compiler, because SQLite is compiled in through
[go-sqlite3](https://github.com/mattn/go-sqlite3). The full API is on
[pkg.go.dev](https://pkg.go.dev/github.com/hypercrux/hypercrux), and
[examples/go](examples/go/main.go) is a program you can run.

## Keys, fields, links and vectors

| Handle | Go | What it does |
|---|---|---|
| Key | `Get`, `Put`, `Delete`, `Scan` | Read, write and remove a record by key, and list records by key prefix |
| SQL | `Exec`, `Query`, `QueryRow` | Plain SQLite SQL on the same tables |
| Links | `Link`, `Unlink`, `Neighbours`, `Walk` | Typed one-way links, and walks up to 32 links deep, forwards, backwards or both |
| Similarity | `Nearest` | The k closest vectors by cosine distance, with an optional SQL filter |

`Update` runs a function as one transaction, and a `Tx` has all the same
methods. `Adopt` turns a table you made with plain SQL into a record table,
or brings one back in step after its schema changed. `Drop` deletes a record
table with its records and their links. `Check` reads the whole file and
confirms the four handles agree. `Export` writes every table, record and
link as JSON lines, and `Import` reads them into a new file.

- **Keys** are `table:id`. The table name is lower-case letters, digits and
  underscores. Anything can follow the colon, up to 1,024 bytes in all.
- **Fields** are columns. `Put` sets the fields it's given and leaves the
  rest alone, and a nil value clears one. Strings, numbers, booleans, bytes
  and times go in as themselves; maps, slices and structs are stored as JSON
  text.
- **Links** have a type, such as `owns` or `cites`, and both records must
  exist. Adding a link twice keeps one.
- **Vectors** go in the field `vec`. The first vector in a table sets its
  size, and every vector after it must match. A search compares every vector
  that passes the filter, so it never misses a closer one.

## One statement across all four

HyperCrux adds three functions to SQL: `distance(a, b)` for the cosine
distance between two vectors, `vector('[...]')` to turn a JSON array into a
stored vector, and `walk(key, depth)`, which returns the keys within `depth`
links as a JSON array. SQLite's `json_each` turns that array into rows, so a
single query can follow links, filter on fields and rank by similarity:

```sql
SELECT d.key, d.title
FROM json_each(walk('customer:42', 2)) w
JOIN docs d ON d.key = w.value
WHERE d.status = 'open' AND d.vec IS NOT NULL
ORDER BY distance(d.vec, ?)
LIMIT 10
```

That's the open documents within two links of a customer, closest to the
question first. In Go, pass the question as a `Vector`. `d.vec IS NOT NULL`
leaves out documents without a vector, whose distance is NULL, which SQLite
would sort first. `walk` takes a link type and a direction too:
`walk('docs:1', 1, 'cites', 'in')` gives the documents that cite `docs:1`.

## Use it from any language

**Through the command.** Every operation is a `hypercrux` command that reads
and writes JSON, so any language that can run a program can use it.

**Straight into the file.** The rules that keep keys, rows, links and
vectors in step are SQLite triggers stored in the file. A program that
inserts a row with plain SQL gets its key registered; one that deletes a row
takes its links with it; one that links to a key that doesn't exist, or
stores a vector of the wrong size, gets an error and changes nothing. Such a
program should turn on `PRAGMA recursive_triggers`, so rows that a REPLACE
removes fire their triggers too, and changes to a record table's schema,
such as dropping or rebuilding it, need `hypercrux drop` or
`hypercrux adopt` afterwards. [FORMAT.md](FORMAT.md) describes the tables,
the triggers and these rules, and
[examples/python/hcfile.py](examples/python/hcfile.py) puts, links, deletes
and searches with Python's standard library alone:

```sh
python3 examples/python/hcfile.py put notes.db docs:4 '{"title": "Q4 plan", "vec": [0.7, 0.3, 0]}'
python3 examples/python/hcfile.py link notes.db customer:42 owns docs:4
python3 examples/python/hcfile.py nearest notes.db docs '[1, 0, 0]' 3
```

**Through an export.** `hypercrux export` writes every table, record and
link as JSON lines, one object a line, which any language can read.
`hypercrux import` reads them into a new file, so an export is also the way
to bring data in from elsewhere and to move a file to a later HyperCrux.
[EXPORT.md](EXPORT.md) describes the format:

```sh
hypercrux export notes.db > notes.jsonl
hypercrux import copy.db < notes.jsonl
```

## The hypercrux command

| Command | Does |
|---|---|
| `hypercrux init FILE` | Creates an empty HyperCrux file, or adds HyperCrux's tables to an existing SQLite database |
| `hypercrux put FILE KEY [JSON]` | Stores fields from a JSON object, or from standard input |
| `hypercrux get FILE KEY` | Prints a record as JSON, vector included |
| `hypercrux delete FILE KEY` | Deletes a record and its links |
| `hypercrux scan FILE PREFIX` | Lists records whose keys start with PREFIX, one JSON object a line. `--after KEY`, `--limit N`, `--vec` |
| `hypercrux sql [--json] FILE STATEMENT [ARG...]` | Runs one SQL statement, with `?` arguments taken as they are |
| `hypercrux link FILE FROM TYPE TO` | Links two records |
| `hypercrux unlink FILE FROM TYPE TO` | Removes a link. `*` as TYPE removes every type |
| `hypercrux neighbours FILE KEY` | Lists a record's links. `--in`, `--both`, `--type T`, `--json` |
| `hypercrux walk FILE KEY DEPTH` | Lists the records up to DEPTH links away. `--in`, `--both`, `--type T`, `--json` |
| `hypercrux nearest FILE TABLE VECTOR\|KEY` | Lists the closest vectors to a JSON array or to a record's own vector. `-k N`, `--where SQL`, `--json` |
| `hypercrux adopt FILE TABLE` | Makes a table created with plain SQL a record table, or brings one back in step after a schema change |
| `hypercrux drop FILE TABLE` | Deletes a record table with its records and their links, or clears what's left of one dropped with plain SQL |
| `hypercrux check FILE` | Confirms keys, rows, links and vectors agree, and SQLite's integrity check passes |
| `hypercrux export FILE` | Writes every record table, record and link to standard output as JSON lines |
| `hypercrux import FILE` | Reads an export from standard input into a new file, or one without record tables, all in one transaction |
| `hypercrux version` | Prints the version |

Options can go before or after the other arguments, except with `sql`,
whose options go before FILE: everything after FILE is the statement and its
arguments. Only `init`, `put` and `import` create a file. Every other
command needs an existing HyperCrux file, so a typo in a file name is caught
and other SQLite files are left alone.

## What HyperCrux promises, and what it doesn't

- **The four handles agree.** Every insert, update and delete is an SQLite
  transaction, and the triggers in the file run inside it, whatever program
  made the change. A record, its key, its links and its vector appear and
  disappear together. Schema changes made with plain SQL, such as dropping
  or rebuilding a record table, are the one exception: follow them with
  `Adopt` or `Drop`, and `Check` reports anything left out of step.
- **Durable.** HyperCrux runs SQLite with `synchronous = FULL`, so a write
  that returned is still there after a crash or a power cut, as long as the
  disk really writes what it's told to flush.
- **Exact search, up to a size.** Search compares every vector that passes
  the filter, so it finds the true nearest ones. On a two-core cloud machine, a search
  for the closest 10 took about 4 milliseconds among 1,000 vectors of 384
  values, 42 milliseconds among 10,000 and 0.41 seconds among 100,000. That
  makes HyperCrux a good fit up to around a hundred thousand vectors per
  table. Millions of vectors want an approximate index, which HyperCrux
  doesn't have.
- **Short walks.** Walks go up to 32 links deep and visit every record they
  reach. On a graph of 100,000 records with five
  links each, a walk took 43 microseconds one link out and 0.51 milliseconds
  three links out, where it reached 155 records on average. Deep searches across large, dense graphs belong in a
  graph database.
- **One machine per file.** Many processes on one machine can share the
  file. SQLite's locking doesn't work over network file systems, so keep the
  file on a local disk.
- **Small.** Each put or link on its own is a transaction flushed to disk,
  which took about 0.35 milliseconds on a two-core cloud machine. Putting
  1,000 records with 384-value vectors in one transaction took about 51
  microseconds a record, close to 20,000 a second. The disk sets the pace of single writes more
  than anything else.

## How it was tested

The tests run every operation, every rule and every command, and then some
harder cases. The results are recorded in [test/results](test/results):

- **Killed writers.** A process wrote to a file while it was killed with
  SIGKILL at a random moment, 200 times. Each of its transactions put a
  record with fields and a vector, linked it to the record before and to an
  anchor, deleted an old record every seventh time and moved a counter. After
  every kill, the file matched the last committed transaction exactly. That
  was 28,017 transactions in all, ending with 24,017 records and 44,034 links:
  no torn record, no link to a missing record, Check and SQLite's integrity
  check passing every time, and searches and walks finding what they should.
- **Processes sharing a file.** Four processes wrote 300 transactions each to
  one file at once, searching and walking as they went, while another read
  it. Every record and link arrived, and Check passed.
- **Python through the rules.** Python's own sqlite3 module put a record with
  a vector and linked it, with plain SQL and nothing from HyperCrux. The
  triggers refused it when it broke a rule: a key in the wrong table, a link
  to nothing, a vector of the wrong size, a changed key. When it deleted a
  record, the links went too. Python and Go then wrote 600 records and links
  to one file at the same time, and Check passed.
- **Exact search.** Nearest matched a brute-force comparison of 2,000 vectors
  over 20 queries, with and without a filter. Python, reading the raw vectors
  and comparing them itself, found the same closest records, with distances
  equal to within a billionth.
- **Export and import.** Forty random files went through export, import and
  a second export, with integers and reals at their limits, negative zero,
  subnormal numbers, empty and random bytes, text in many scripts and with
  control characters, and vectors of awkward floats. Each time the two
  exports matched byte for byte, and every value came back with its SQLite
  type and its exact bits. An export cut short at any byte was refused and
  left nothing behind, and the format's reader ran under Go's fuzzer.
- **Plain SQL that breaks a rule is refused.** Thirteen statements that would leave
  the file inconsistent, from changing a key to storing a vector of zeros,
  were all refused.
- **Problems found in review.** An independent review before the first
  release found ways to get the handles out of step: tables named like
  HyperCrux's own bookkeeping, REPLACE on a second unique column, schema
  changes from another program or rolled back, vectors holding NaN written
  with plain SQL, and transactions on a single connection. Each is fixed and
  has a test of its own.
- **The race detector** found nothing, running the suite in short mode (20
  kills instead of 200).

The speed of each operation, the middle of three runs on a two-core cloud
machine:

| Operation | Time |
|---|---|
| Put one record, committed and flushed to disk | 0.35 ms |
| Put records with 384-value vectors, 1,000 per transaction | 51 µs a record |
| Get a record by key | 18 µs |
| Link two records, committed and flushed | 0.33 ms |
| Walk 1, 2 and 3 links out, among 100,000 records with 5 links each | 43 µs, 0.12 ms, 0.51 ms |
| Nearest 10 among 1,000, 10,000 and 100,000 vectors of 384 values | 4.4 ms, 42 ms, 0.41 s |
| Nearest 10 among 100,000, filtered to a tenth by an unindexed field | 0.13 s |
| Nearest 10 among 10,000 vectors of 1,536 values | 0.12 s |

A filter on an unindexed field still reads every row to test it. An index on
the field, made with plain SQL, lets SQLite skip the rows that don't match.

To run it all yourself: `sh scripts/record-tests.sh`.

## Build it yourself

You need Go 1.24 or later and a C compiler, because go-sqlite3 compiles SQLite
with cgo.

```sh
go install github.com/hypercrux/hypercrux/cmd/hypercrux@latest
```

From a clone, `go build ./cmd/hypercrux` builds the binary and `go test ./...`
runs the tests. The release binaries are built by
[the release workflow](.github/workflows/release.yml) on GitHub.

## The name

A crux is the point where things cross. It's Latin for a cross, and in
English it's the heart of a problem, the place everything meets. In
HyperCrux, it's where the four handles meet: one record you can reach by key,
by SQL, by link and by similarity.

Hyper is there twice over. Links between records are what the web calls
hyperlinks. And vectors live in hyperspace, the mathematician's word for
space with more than three dimensions; an embedding with 384 values is a
point in a 384-dimensional one.

## License

Copyright HyperCrux.com 2026. Licensed under the [Apache License 2.0](LICENSE).
HyperCrux also contains go-sqlite3 (MIT), SQLite (public domain) and the Go
runtime (BSD), as listed in [NOTICE](NOTICE) and
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
