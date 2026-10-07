# The HyperCrux export format, version 1

`hypercrux export FILE`, and `Export` in Go, write a database's record
tables, records and links as JSON lines. `hypercrux import FILE`, and
`Import` in Go, read them back into a new file. An export is the way to move
a database to another HyperCrux version or engine, and a backup that any
language can read. This document describes the format, so other programs
can read an export, or write one for HyperCrux to import.

The code is in [internal/export](internal/export), which every HyperCrux
engine shares, so they all write the same bytes for the same data.

## An example

```
{"hypercrux":"export","version":1}
{"table":"customer","dims":null,"fields":["name"]}
{"table":"docs","dims":3,"fields":["title","vec","pages","score","scan"]}
{"key":"customer:42","fields":{"name":"Dana"}}
{"key":"docs:1","fields":{"title":"Q3 plan","vec":[0.9,0.1,0],"pages":3,"score":1.0}}
{"key":"docs:2","fields":{"scan":{"base64":"AAEC/w=="}}}
{"from":"customer:42","type":"owns","to":"docs:1"}
{"from":"customer:42","type":"owns","to":"docs:2"}
{"end":{"tables":2,"records":3,"links":2}}
```

## The lines

Each line is one JSON object in UTF-8, ending with a line break. They come
in this order:

1. **The first line** names the format and its version,
   `{"hypercrux":"export","version":1}`. A reader refuses a version newer
   than it knows.
2. **A line for each record table**, in order of name. `table` is its name.
   `dims` is its vector size, or `null` when it has none recorded; a table
   keeps its size after its vectors are deleted, and an import keeps it too.
   `fields` lists its columns after the key, in order, with the vector's
   column, `vec`, where it falls.
3. **A line for each record**, table by table in the tables' order, and in
   order of key within each table. `key` is the record's key, and `fields`
   holds its fields that aren't NULL, in the table's order.
4. **A line for each link**, in order of `from`, then `type`, then `to`.
5. **The last line** counts the tables, records and links. An export without
   it was cut short, or its export failed, and import refuses it.

Names, keys and link types are put in order byte by byte, as SQLite's
`BINARY` collation does.

## Values

A field holds one of five kinds of value, and each keeps its kind through an
export and an import:

| Stored | Written as | Examples |
|---|---|---|
| Integer | A JSON number without a decimal point or an exponent | `3`, `-9223372036854775808` |
| Real | A JSON number with a decimal point or an exponent | `1.0`, `-0.0`, `0.1`, `1e+21`, `5e-324` |
| Text | A JSON string | `"Q3 plan"` |
| Bytes | An object holding them in standard base64, with padding | `{"base64":"AAEC/w=="}` |
| A vector | A list of numbers, in the field `vec` only | `[0.9,0.1,0]` |

NULL fields are left out. Integers are 64-bit. The vector's field is the one
called `vec`, in any mix of upper and lower case.

A real is written as the shortest decimal that reads back to the same 64-bit
float, the way JavaScript writes numbers: plain digits from 0.000001 up to
1e21, and an exponent outside that range, as in `1e-7` and `1e+21`. A whole
number gets `.0`, so it can't be taken for an integer, and negative zero
keeps its sign. Vector values are written the same way, as the shortest
decimal for a 32-bit float, without the `.0`.

A string escapes the quote, the backslash and the control characters below
U+0020, and nothing else. Control characters use JSON's short forms, such as
`\n` and `\t`, where it has them, and `\u00XX` otherwise.

## What an export holds

Only record tables and links. Tables HyperCrux hasn't adopted stay behind,
and so do a table's declared column types and constraints, its indexes and
any triggers of its own. In an imported table the key is the first column.

Export writes what `Put` and `Link` take. Plain SQL can store values they'd
refuse, and export stops at the first one with an error that names the
record:

- an infinite number;
- text that isn't valid UTF-8;
- a key that `Put` wouldn't take, such as one with a NUL in it;
- a field name that `Put` wouldn't take, such as `first name`;
- a link type that isn't 1 to 200 characters of UTF-8.

Change the value with SQL, and export again. Export also refuses a file that
`hypercrux check` finds problems in. What it wrote before stopping has no
last line, so import won't take it.

Export reads one moment of the file, so writers carry on while it runs.

## Importing

Import goes into a new file, or into a HyperCrux file without any record
tables.
It runs in one transaction, so either everything goes in or nothing does.
Records and links go through the same rules as `Put` and `Link`, and tables
come back with their fields in the same order and the same vector sizes, so
exporting the result gives the same bytes.

Import refuses:

- a file that already holds record tables;
- a table whose name is taken by a table, view or index already in the
  file, in any mix of upper and lower case;
- a record or a link that is there twice;
- a link to a record that isn't in the export;
- anything else `Put` or `Link` would refuse.

## Writing an export yourself

An export made by another program needs the five kinds of line, in the
order above, and the counts at the end. Import also takes a few things
`Export` never writes:

- an object's members in any order, with spaces between them;
- any JSON form of a number, such as `1.50` or `1E3`, with a number that has
  a decimal point or an exponent read as a real;
- a field set to `null`, which is left out;
- records and links in any order, as long as every table comes before the
  records and every record before the links;
- lines that end in CR LF, and a last line without a line break.

It doesn't take `true` or `false` (use `1` and `0`), any other list or
object in a field, base64 in any form but the standard one, a member that
appears twice in one object, or a `\u` escape that is half of a surrogate
pair.

## What stays the same

Within version 1, later releases may read more than this document
describes, and they'll write exactly what it describes. A change that older
readers couldn't handle gets a new version number.
