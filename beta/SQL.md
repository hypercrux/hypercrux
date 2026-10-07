# The Beta's SQL

This document sets out the Beta's SQL: which statements it takes, and the
answers they give. Task P5 wrote it on 7 October 2026, from BETA.md's
"Queries and SQL" and from 0.x's answers. The tasks that build the SQL work
from it: Q1 to Q6, G3 and G4.

## The rule behind it

The Beta's SQL is a part of SQLite's, and it gives SQLite's answers. A
statement inside the subset gives the answer 0.x gives:

- the same result columns, with the same names;
- the same rows, in the same order where the statement orders them;
- values of the same kind, with the same bits for reals;
- the same count of changed rows;
- an error of the same kind where 0.x gives one. Messages may differ.

A statement outside the subset fails with an error of kind "error", as
"Errors and their kinds" describes. It doesn't give an answer of its own.

0.x here means HyperCrux 0.2.0. Its SQLite is the one inside go-sqlite3
v1.14.52, SQLite 3.53.4. Where this document leaves a corner open, that
SQLite's source settles it, and the routines named below are in go-sqlite3's
`sqlite3-binding.c`.

The SQL corpus in `beta/sqlcorpus` holds 0.x's answers, and checks a build
against this document:

- every statement is marked in or out to match it, and each one marked out
  says why in its note;
- all 3,000 generated expressions are in;
- the 608 date cases on fixed dates are out, since the subset has dates on
  `'now'` only, and they're there to check Q2's arithmetic;
- the 14 cases on `'now'` are in.

A go-sqlite3 upgrade can change some answers. A4.md says how to record the
corpus again.

A few things 0.x does are left out on purpose, or done differently. "Where
the Beta differs from 0.x", near the end, lists them.

## Statements

A call runs one statement: `SELECT`, `INSERT`, `UPDATE` or `DELETE`. It may
end with a semicolon, and only spaces and comments may follow that. Text
that holds no statement, or a second one, is an error.

Everything else is outside the subset, among others `WITH`, `VALUES` on its
own, `EXPLAIN`, `PRAGMA`, `CREATE`, `DROP`, `ALTER`, `REPLACE`, `BEGIN`,
`COMMIT`, `ROLLBACK`, `SAVEPOINT`, `ATTACH` and `VACUUM`. Transactions go
through `Update`, and tables and fields come from `Put`. `Compact` does
`VACUUM`'s job.

`Exec` runs any statement, and drops a SELECT's rows. `Query` and `QueryRow`
run any statement too, and a write gives them no columns and no rows, as in
0.x. `RowsAffected` gives the count of changed rows of an INSERT, UPDATE or
DELETE, and 0 for a SELECT. `LastInsertId` returns an error, since records
have no row numbers.

## Words and symbols

### Spaces and comments

Spaces are the space, tab, line feed, form feed and carriage return. A
comment runs from `--` to the end of the line, or from `/*` to `*/`, and a
`/*` without an end runs to the end of the text. Comments count as spaces,
with one exception, under "What a result column is called". So `--1` is a
comment, and two minus signs need a space between them: `- -1`.

### Keywords

Keywords match in any mix of upper and lower case. These are reserved, and
can't be bare names:

ADD, ALL, ALTER, AND, AS, AUTOINCREMENT, BETWEEN, CASE, CAST, CHECK,
COLLATE, COMMIT, CONSTRAINT, CREATE, CURRENT_DATE, CURRENT_TIME,
CURRENT_TIMESTAMP, DEFAULT, DEFERRABLE, DELETE, DISTINCT, DROP, ELSE,
ESCAPE, EXCEPT, EXISTS, FOREIGN, FROM, GROUP, HAVING, IN, INDEX, INSERT,
INTERSECT, INTO, IS, ISNULL, JOIN, LIMIT, NOT, NOTHING, NOTNULL, NULL, ON,
OR, ORDER, PRIMARY, RAISE, REFERENCES, RETURNING, SELECT, SET, TABLE, THEN,
TO, TRANSACTION, UNION, UNIQUE, UPDATE, USING, VALUES, WHEN, WHERE.

They're the words SQLite won't read as a field in an expression, so a bare
name the Beta takes there, SQLite takes too. Most of them belong to SQL the
Beta leaves out. A field with one of these names is written in double
quotes, as in 0.x.

Six more words are keywords only where the grammar has them, and names
anywhere else, as in SQLite: ASC, BY, DESC, INNER, LIKE and OFFSET. So a
field called `desc` can be read as `SELECT desc FROM docs`. LIKE and INNER
can't be a bare alias, since right after an expression LIKE is the operator,
and right after a source INNER starts a join.

Function names, `walk` and `json_each` are names.

### Names

A bare name is made of letters, digits, underscores, dollar signs and bytes
from 0x80 up. It starts with a letter, an underscore or a byte from 0x80 up,
and it isn't a reserved word. A quoted name is any text in double quotes,
with `""` for a quote inside it, and it may be a reserved word.

Names compare with their ASCII letters folded to one case, as SQLite
compares them, so `TITLE`, `"Title"` and `title` are one name. Other
characters compare exactly.

Backquotes and square brackets don't quote names in the Beta. Text in single
quotes is always a value.

### Literals

- An integer is a run of digits. One too big for 64 bits is read as a real,
  apart from the case under "A sign before a literal".
- A real has a decimal point, an exponent or both: `1.5`, `.5`, `5.`,
  `1e10`, `2.5E-3`. It becomes the nearest float64, as Go's
  `strconv.ParseFloat` makes it: SQLite 3.53.4 gave the same bits on 60,000
  numbers tried, the hard cases included. A real too big becomes infinity
  (`1e999`), and one too small becomes zero.
- A number followed straight away by a character that could be part of a
  bare name, such as `12abc` or `1e`, is an error. Hex numbers (`0x10`) and
  digit separators (`1_000`) are outside the subset.
- Text is in single quotes, with `''` for a quote inside it. It may hold any
  bytes, line breaks included.
- Bytes are written `x'...'` or `X'...'`, with an even number of hex digits.
- `NULL` is NULL. `TRUE`, `FALSE`, `CURRENT_DATE`, `CURRENT_TIME` and
  `CURRENT_TIMESTAMP` are outside the subset.

A minus sign isn't part of a number. "A sign before a literal" says what it
does.

### Parameters

`?` is the only parameter, and the arguments fill the marks in order.
`?NNN`, `:name`, `@name` and `$name` are outside the subset.

### Symbols

`(` `)` `,` `;` `.` `*` `/` `%` `+` `-` `||` `=` `==` `!=` `<>` `<` `<=`
`>` `>=`. SQLite's others, `&`, `|`, `~`, `<<`, `>>`, `->` and `->>`, are
outside the subset.

## Grammar

`[ x ]` is optional, `{ x }` repeats zero or more times, `|` separates
choices, and quoted words and symbols are keywords and symbols.

```
statement = ( select | insert | update | delete ) [ ";" ] .

select    = "SELECT" results [ "FROM" from ] [ "WHERE" expr ]
            [ "ORDER" "BY" term { "," term } ]
            [ "LIMIT" expr [ "OFFSET" expr ] ] .
results   = "*" | result { "," result } .
result    = expr [ [ "AS" ] name ] .
from      = source [ [ "INNER" ] "JOIN" source "ON" on ] .
source    = ( name | walk | each ) [ [ "AS" ] name ] .
walk      = "walk" "(" expr "," expr [ "," expr [ "," expr ] ] ")" .
each      = "json_each" "(" walk ")" .
on        = column "=" column | "(" on ")" .
term      = expr [ "ASC" | "DESC" ] .

insert    = "INSERT" "INTO" name "(" name { "," name } ")"
            "VALUES" row { "," row } .
row       = "(" expr { "," expr } ")" .
update    = "UPDATE" name "SET" name "=" expr { "," name "=" expr }
            [ "WHERE" expr ] .
delete    = "DELETE" "FROM" name [ "WHERE" expr ] .

expr      = an expression of the operators below, over primaries .
primary   = literal | "?" | column | call | cast | "(" expr ")" | record .
column    = name [ "." name ] .
call      = name "(" [ "*" | expr { "," expr } ] ")" .
cast      = "CAST" "(" expr "AS" type ")" .
type      = "INTEGER" | "REAL" | "TEXT" | "BLOB" | "NUMERIC" .
record    = "(" "SELECT" name "FROM" name "WHERE" "key" "=" expr ")" .
in        = expr [ "NOT" ] "IN" "(" [ expr { "," expr } | walks ] ")" .
walks     = "SELECT" "key" "FROM" walk | "SELECT" "value" "FROM" each .
```

Beside the grammar:

- Every name must find what it names, as "Names and columns" describes.
- A `call` is one of the functions under "Functions", with the number of
  arguments given there. `*` goes only in `count(*)`.
- `walk(...)` in a FROM is the walk as a table, and in an expression it's
  the function. Both are under "Walks".
- `record` is the one-record subquery, and `walks` are the subqueries IN
  takes. Both are under "Queries".
- These can't use fields: the arguments of a walk in a FROM or in `walks`,
  the key in `record`, LIMIT, OFFSET, and the values of an INSERT.
- `date()` and `datetime()` take literals only, as "Dates" describes.
- The word `key` in `record` is the name of the key field, in any case.

### Operators and how they group

| Level | Operators | Order |
|---|---|---|
| 1, loosest | `OR` | left to right |
| 2 | `AND` | left to right |
| 3 | `NOT` x | prefix |
| 4 | `=` `==` `!=` `<>` `IS` `IS NOT` `IN` `NOT IN` `LIKE` `NOT LIKE` `BETWEEN` `NOT BETWEEN` | left to right |
| 5 | `<` `<=` `>` `>=` | left to right |
| 6 | `+` `-` | left to right |
| 7 | `*` `/` `%` | left to right |
| 8 | `\|\|` | left to right |
| 9, tightest | `-` x, `+` x | prefix |

These are SQLite's levels. The expression corpus mixes them without
parentheses, so it checks a parser's trees against SQLite's. A parser that
climbs the levels (a Pratt parser) builds SQLite's trees when it follows
these rules:

- A binary operator's right operand takes only operators of tighter levels.
  So `1 = 2 < 3` is `1 = (2 < 3)`, and `1 = 1 = 1` is `(1 = 1) = 1`.
- A prefix operator can start any operand, and its own operand then runs on
  as far as its level allows. NOT's operand takes every operator of level 4
  and tighter, so `1 = NOT 2 = 3` is `1 = (NOT (2 = 3))`, and `- NOT 1 + 2`
  is `-(NOT (1 + 2))`. A sign's operand takes no binary operator at all, so
  `-2 || 3` is `(-2) || 3`.
- In `x BETWEEN a AND b`, `a` takes every operator of level 3 and tighter,
  another BETWEEN included: `x BETWEEN y BETWEEN 0 AND 1 AND 2` is
  `x BETWEEN (y BETWEEN 0 AND 1) AND 2`. An OR in `a` is a syntax error, as
  in SQLite. The AND after `a` belongs to BETWEEN, and `b` takes only
  operators tighter than level 4.
- In `x LIKE p ESCAPE e`, `p` and `e` take only operators tighter than
  level 4.
- The items of `x IN (...)` are whole expressions.
- `IS NULL` and `IS NOT NULL` are IS and IS NOT with NULL.

### A sign before a literal

Signs work as SQLite's parser makes them work:

- A sign in front of a plus sign's expression, with or without parentheses
  between them, takes the plus sign's place: `-+x` is `-x`, and `+(+x)` is
  `+x`.
- A minus sign in front of a number literal, with or without parentheses
  between them, negates the literal as written. So `-0.0` is negative zero,
  `-1e999` is minus infinity, and `-9223372036854775808` is the smallest
  integer. Any other `-x` is worked out as `0 - x` (see "Arithmetic").
- A plus sign leaves its operand's value as it is. It only takes away its
  affinity (see "Conversion before comparing").

## Names and columns

### Tables and fields

- A table name finds the record table of that name. Record tables have
  lower-case names, so `DOCS` finds `docs`. A table that doesn't exist is an
  error, "no such table". Tables come from `Put`, and SQL can't create one.
- A field name finds the table's field of that name, and `key` is the
  record's key. A field the table hasn't got is an error, "no such column".
  `rowid`, `oid` and `_rowid_` aren't fields in the Beta. Fields come from
  `Put`, and an INSERT or UPDATE can't add one.
- A table has the fields `Put` has given it, in a fixed order. A field
  stays when no record holds it any more, until the table is dropped.

### Sources, qualified names and clashes

- A FROM has one source, or two joined. A source's name is its alias if it
  has one. Otherwise it's the table's name, or `walk` or `json_each`.
- A name with a dot, such as `d.title`, is the field of the source with that
  name. Once a table has an alias, its own name no longer finds it:
  `SELECT docs.key FROM docs d` is an error, as in SQLite.
- A name without a dot looks in every source. In a join, a name both
  sources have is an error, "ambiguous column name".
- The walk as a table has the columns `key` and `depth`.
- `json_each(walk(...))` has SQLite's columns for json_each when names are
  looked up (key, value, type, atom, id, parent, fullkey and path), so names
  clash as they do in SQLite. Only `value` may be used, and the others are
  outside the subset.
- Result columns' aliases can be used only in ORDER BY. WHERE sees fields
  only.

### What a result column is called

- A column with an alias is called by its alias.
- A column that is a field, with or without a source's name and with or
  without parentheses, is called by the field's name as the table spells
  it. So `SELECT TITLE FROM docs` gives a column `title`. The walk's columns
  are `key` and `depth`, and json_each's is `value`.
- Any other column is called by its text: from its first character to just
  before the token after it, with spaces trimmed from both ends. The text is
  kept as written, comments in it included, and so is a comment between the
  expression and the next token. `SELECT 1 + 2 /* x */, n FROM docs` gives
  `1 + 2 /* x */` and `n`. A comment at the very end of the statement belongs
  to the last column's name: `SELECT 1 --x` gives `1 --x`.
- `*` gives `key` and then the table's fields in their order, and over the
  walk it gives `key` and `depth`.

## Queries

### FROM

- No FROM: the query has one row, with no fields.
- A table: one row for each record.
- `walk(...)`: one row for each record the walk reaches, as "Walks"
  describes.
- `json_each(walk(...))`: the same rows, with the key in `value`. This is
  0.x's form, accepted unchanged.
- A join: a walk and a table, in either order, `ON` the table's `key`
  equal to the walk's `key`, or json_each's `value`, written either way
  round and perhaps in parentheses. It gives one row for each record the
  walk reaches that belongs to the table. `JOIN` and `INNER JOIN` are the
  same.
- Other joins, such as two tables, two walks, LEFT JOIN or other ON
  conditions, are outside the subset, and so are subqueries in FROM.

### The order of rows

A query without ORDER BY gives its rows in a fixed order:

- over a table, in key order, byte by byte, as `Scan` gives them;
- over a walk, and over a join, in the walk's order.

ORDER BY sorts stably, so rows that tie on every term keep that order, and a
LIMIT after a tie keeps the rows that come first in it. 0.x's order without
ORDER BY depends on SQLite's plan, and its ties aren't settled, so the corpus
orders every query that can return more than one row.

### WHERE

The condition keeps a row when it's true. False and NULL leave the row out
(see "Truth"). It may use the sources' fields, functions, the one-record
subquery and walks in IN, and no aggregates.

### Result columns

Each is an expression, perhaps with an alias, and `*` gives every column, as
above. `*` over a join or over json_each is outside the subset, and so are
`d.*` and `DISTINCT`.

### Aggregates

- A query whose result columns use `count()`, `sum()`, `total()`, `avg()`,
  or `min()` or `max()` with one argument, is an aggregate query. It gives
  exactly one row, even when no row passes WHERE, and the aggregates run
  over every row that passes.
- In an aggregate query's result columns and ORDER BY, a field may appear
  only inside an aggregate's argument. So `SELECT key, count(*) FROM docs`
  is outside the subset. Expressions without fields, such as
  `walk('people:1', 2)`, may stand beside aggregates.
- Aggregates can't nest. They can't go in WHERE, LIMIT or OFFSET, or in a
  query whose result columns have none: those are errors, as in SQLite.
- ORDER BY, LIMIT and OFFSET apply to the one row.
- `DISTINCT` inside an aggregate, `group_concat()`, `GROUP BY` and `HAVING`
  are outside the subset.

### ORDER BY

Each term is worked out as SQLite works it out:

1. A term that is a bare name, the alias of a result column, sorts by that
   column.
2. A term that is an integer literal that fits in 32 bits, perhaps with
   signs in front or in parentheses, is a column number, counting from 1.
   A number outside 1 to the number of columns is an error, so `ORDER BY 0`
   and `ORDER BY -1` are errors.
3. Any other term is an expression, worked out for each row. Its names are
   the sources' fields first, and then the result columns' aliases. So
   `ORDER BY 1.5` or `ORDER BY '2'` sorts by a constant and changes nothing.

ASC is the default and DESC reverses. Going up, NULL comes first, then
numbers, then text, then bytes, as "Comparing" describes. `NULLS FIRST`,
`NULLS LAST` and `COLLATE` are outside the subset.

### LIMIT and OFFSET

`LIMIT n` keeps the first n rows, and `OFFSET m` skips m rows before them.
Both are worked out once, and can't use fields or aggregates. Each must give
a whole number: an integer, a real without a fraction that fits in 64 bits,
or text that reads as one of those, with spaces at either end. Anything else,
NULL included, is an error, "datatype mismatch". A negative LIMIT means no
limit, and a negative OFFSET means 0. OFFSET needs a LIMIT. SQLite's
`LIMIT m, n` is outside the subset.

### The one-record subquery

`(SELECT f FROM t WHERE key = e)` gives field `f`, or `key`, of the record
in table `t` whose key is `e`. It gives NULL when there's no such record, or
when the record has no value for `f`. `e` can't use fields, and the
subquery is worked out once for each statement. It may go anywhere an
expression may, for example `distance(vec, (SELECT vec FROM photo WHERE key
= ?))`. The table and the field must exist.

Other subqueries are outside the subset, among them `(SELECT max(n) FROM
docs)`, `EXISTS (...)` and `IN (SELECT key FROM people)`.

### Walks in IN

`x IN (SELECT key FROM walk(...))` and 0.x's
`x IN (SELECT value FROM json_each(walk(...)))`, and both with `NOT IN`,
test x against the keys the walk reaches, worked out once for each
statement. They follow IN's rules under "Values".

### Nearest's filter

`Nearest(table, q, k, where, args...)` takes in `where` a condition on the
table's records, as a WHERE over the table would take it, with its `?` marks
filled from `args`. A `where` that holds only spaces keeps every record with a
vector. 0.x runs the filter inside a SELECT, so its errors are of kind
"error", and the Beta's are too.

## Walks

`walk(start, depth [, type [, direction]])` follows links from the record
with the key `start`, as `Walk` does. It reaches each record with the fewest
links needed, and gives them nearest first, then by key, byte by byte, each
once, without `start` itself.

- `start` must be text. A key with no record, or one that isn't a valid key
  at all, reaches nothing. That isn't an error, where `Walk` gives
  ErrNotFound.
- `depth` must be an integer from 1 to 32. The real 2.0 and the text '2' are
  errors.
- `type` is NULL or text. NULL and '' follow links of every type.
- `direction` is NULL or text, and after Go's `strings.ToLower` it must be
  'out', 'in', 'both' or ''. NULL and '' mean out.
- Anything else is an error, and so are fewer than 2 arguments or more
  than 4.

It comes in three forms:

- **The function** gives text: the keys as a JSON array, as Go's
  `encoding/json` writes a `[]string`. So `<`, `>` and `&` come out as
  `\u003c`, `\u003e` and `\u0026`, and no keys give `[]`. Its arguments may
  use fields. This is 0.x's form, and the conformance tests WalkInSQL and
  UpdateIsAllOrNothing use it.
- **The table**, `walk(...)` in a FROM, gives a row for each record reached,
  with the columns `key`, text, and `depth`, an integer from 1, in the
  walk's order.
- **`json_each(walk(...))`** gives the same rows, with the key in `value`.

## Vectors

- A record's vector is its field `vec`. In SQL it reads as bytes: its
  float32 values, four bytes each, little-endian, as 0.x stores it. So
  `typeof(vec)` is 'blob', and `length(vec)` is four times its size.
- `distance()` and `vector()` take a vector as bytes of that kind, or as text
  holding a JSON array of numbers. They're under "Functions".
- `ORDER BY distance(vec, q)` puts rows without a vector first, since their
  distance is NULL. With `vec IS NOT NULL` and a LIMIT, the planner may use a
  nearest search (task Q5), and the answer stays the same, errors included:
  an error comes only when a row that passes WHERE reaches `distance()`.
- "Writes" says what `vec` takes.

## Dates

`date('now' [, m ...])` gives today's date in UTC as text,
'YYYY-MM-DD', and `datetime('now' [, m ...])` gives the date and time,
'YYYY-MM-DD HH:MM:SS'. Each modifier m moves the moment in turn.

- The first argument is the literal 'now', in any case.
- Each modifier is a text literal: a plus or minus sign, a whole number, one
  or more spaces, and `day`, `days`, `month`, `months`, `year` or `years`, in
  any case. For example '+1 day', '-13 months' and '+4 years'.
- Days move the moment by whole days. Months and years move the month or
  the year and keep the day, and a day past the end of its month runs on
  into the next, as SQLite does it: from 31 January, '+1 month' gives 2 or
  3 March. Where SQLite gives NULL, the Beta does too.
- 'now' is read once for each statement, so every row of a query sees the
  same moment.
- Other first arguments, such as a stored date, other modifiers, and the
  other date functions (`time()`, `julianday()`, `strftime()`,
  `unixepoch()`) are outside the subset.

SQLite's routines are `parseModifier`, `computeJD`, `computeYMD`, `dateFunc`
and `datetimeFunc` in its date code. `dates.jsonl` holds 608 cases on fixed
dates, with month ends and leap days, for Q2's arithmetic.

## Writes

Each write works on one table and goes through the checks of `Put` and
`Delete`.

### INSERT

`INSERT INTO t (f, ...) VALUES (e, ...), ...` adds a record for each row of
values. The list of fields must name `key`, and may name any of the table's
fields, each once. Fields left out have no value. Each row has as many values
as there are fields. The count of changed rows is the number of records
added.

### UPDATE

`UPDATE t SET f = e, ... [WHERE c]` changes each record that matches `c`,
or every record without a WHERE. Each field may be set once. The values come
from the record as it was, so `SET a = b, b = a` swaps two fields. `key` may
only be set to the value it has, exactly. The count is the number of records
that matched, whether or not a value changed.

### DELETE

`DELETE FROM t [WHERE c]` deletes each record that matches, with its links
in both directions, as `Delete` does. The count is the number of records
deleted.

### For all three

- The table and the fields must exist. A write can't create either.
- Each row written is checked as `Put` checks it, and "Errors and their
  kinds" gives the kind of each failure:
  - the key is text, starts with the table's name and a colon, has at least
    one byte after the colon, is at most 1,024 bytes, and is valid UTF-8
    without NUL bytes;
  - `vec` takes NULL, or bytes holding 1 to 65,536 float32 values, as many
    as the table's vectors have, all finite and not all zero, with -0
    counting as zero. The first vector a table takes sets its size;
  - any other field takes NULL, an integer, a finite real, valid UTF-8 text,
    or bytes.
- A statement that writes no rows can't break these rules.
- A statement is all or nothing. If one of its rows fails, none of its
  changes stay. Inside `Update`, the transaction goes back to where it was
  before the statement and carries on.
- A statement's conditions and values come from the data as it was before
  the statement changed anything. Its changes then go in: an INSERT's rows
  in the order given, an UPDATE's or a DELETE's records in key order.
- A statement outside `Update` is a transaction of its own, as a single
  `Put` is.
- Outside the subset: `INSERT` without a list of fields, `INSERT ...
  SELECT`, `DEFAULT VALUES`, `OR REPLACE` and SQLite's other conflict
  clauses, upserts, `REPLACE`, `RETURNING`, `UPDATE ... FROM`, and ORDER BY
  or LIMIT on a write.

## Values

### Kinds

SQL values are NULL, integers of 64 bits, reals (float64), text, and bytes,
which SQLite calls blobs. A vector reads as bytes.

- Text in SQL may hold any bytes, even ones that aren't valid UTF-8, such as
  `CAST(x'ff' AS TEXT)` or an argument. Only text that's stored must be
  valid.
- A real may be infinite, and -0 keeps its sign.
- A real is never NaN. SQLite turns NaN into NULL wherever it comes from,
  so `1e999 - 1e999` is NULL.

### Arguments

Arguments become SQL values as 0.x makes them, through database/sql and
go-sqlite3:

| Go value | SQL value |
|---|---|
| `nil` | NULL |
| `int`, `int8` to `int64`, `uint8` to `uint32`, and `uint` and `uint64` up to 2^63 - 1 | integer |
| `bool` | integer, 1 or 0 |
| `float64`, and `float32` widened | real, with NaN becoming NULL |
| `string` | text, as given |
| `[]byte` | bytes. A nil slice gives NULL, and an empty one gives empty bytes |
| `time.Time` | text in the layout `2006-01-02 15:04:05.999999999-07:00`, in the time's own zone |
| a `driver.Valuer`, such as `Vector` | what its `Value` method gives. A `Vector` gives its bytes |

Named types and pointers go through database/sql's usual conversion, and
other types are an error. A statement takes exactly as many arguments as it
has `?` marks.

### Results

Values come back as `int64`, `float64`, `string`, `[]byte` and `nil`, and
database/sql converts them into other destinations as usual.

### Comparing

- NULL compared with anything gives NULL, under every operator apart from
  IS and IS NOT.
- Values of different kinds go NULL first, then numbers, then text, then
  bytes.
- Integers and reals compare by value, exactly, without rounding one to the
  other: 9007199254740993 is greater than 9007199254740992.0. -0 equals 0.
- Text compares byte by byte, and when one is the start of the other, the
  shorter comes first. Bytes compare the same way. Case counts: 'A' comes
  before 'a'.
- ORDER BY, `min()`, `max()` and `nullif()` use this order.

SQLite's routines are `sqlite3MemCompare` and `sqlite3IntFloatCompare`.

### Conversion before comparing

SQLite converts some operands of `=`, `<` and the other comparisons, IS,
IN and BETWEEN, by their affinity. In the subset:

- Fields compare as they are, whatever they hold, and so do the walk's
  columns and json_each's value. `n = '5'` is true only when n holds the text
  '5', and `n = 5` only when it holds the number. That's 0.x's behaviour,
  since its fields are columns without a type. A field is converted only
  when the other side is a CAST to INTEGER, REAL or NUMERIC, as below.
- In 0.x the key has text affinity. That never changes a result, since
  every key starts with a lower-case letter, so the Beta compares the key
  like any other field.
- A one-record subquery compares like the field it reads.
- A CAST brings its type into a comparison:
  - CAST to INTEGER, REAL or NUMERIC, against a field, another CAST or an
    expression without affinity such as a literal or `?`, turns text on the
    other side into a number when all of the text reads as one, apart from
    spaces at either end. So `CAST(5 AS INTEGER) = '5'` is true, and
    `CAST(5 AS INTEGER) = '5x'` is false.
  - CAST to TEXT turns a number into its text when the other side has no
    affinity: `CAST('5' AS TEXT) = 5` is true. Against a field or a CAST to
    TEXT or BLOB, it changes nothing.
  - CAST to BLOB changes nothing.
- In `x IN (...)`, x's affinity applies to the items, and the items' own
  affinity doesn't count. BETWEEN is two comparisons, each by these rules.
- Parentheses keep an operand's affinity, and a plus sign takes it away:
  `+CAST(5 AS INTEGER) = '5'` is false.
- Function arguments aren't converted: `nullif(CAST(5 AS INTEGER), '5')`
  gives 5.

SQLite's routines are `sqlite3CompareAffinity`, `sqlite3ExprAffinity` and
`applyAffinity`.

### Truth

Where SQL wants true or false, in WHERE and under NOT, AND and OR, NULL is
unknown, a number is true when it isn't zero, and text and bytes are read as
a real first, so 'a' is false and '1' is true.

- Comparisons, IS, IN, BETWEEN, LIKE, NOT, AND and OR give 1, 0 or NULL.
- NOT NULL is NULL.
- AND gives 0 when either side is false, then NULL when either side is
  NULL, and 1 otherwise.
- OR gives 1 when either side is true, then NULL when either side is NULL,
  and 0 otherwise.

### Arithmetic

- `+`, `-` and `*` on two integers give an integer, or a real when the
  result won't fit in 64 bits.
- `/` on two integers gives the quotient rounded toward zero, so `-7 / 2` is
  -3. The smallest integer divided by -1 gives a real.
- `%` on two integers gives the remainder, with the sign of the left side,
  so `-7 % 2` is -1.
- With a real on either side, `+`, `-`, `*` and `/` work in float64. `%`
  turns both sides into integers, rounding toward zero, and gives the
  remainder as a real: `5.5 % 2` is 1.0.
- Dividing by zero gives NULL, under `/` and `%` alike, and so does a result
  that would be NaN, such as `1e999 * 0`. NULL on either side gives NULL.
- Text and bytes are read as numbers first, as "Text as numbers" says for
  arithmetic. So `'12' + 1` is 13, `'12.5' + 1` is 13.5, `'1e3' + 0` is
  1000.0, `'12abc' + 1` is 13, `'abc' + 1` is 1, and `'0x10' + 0` is 0.
- `-x` is `0 - x`, apart from a minus sign in front of a number literal. So
  `-'5'` is -5, `-'abc'` is 0, and `-n` is 0.0 when n holds -0.0 or 0.0.
- `+x` is x, unchanged. `+'5'` stays the text '5'.

SQLite's routines are the arithmetic opcodes in `sqlite3VdbeExec`, from
`OP_Add` to `OP_Remainder`.

### Numbers as text

Where text is wanted, such as for `||`, CAST to TEXT, the text functions and
LIKE, an integer is written in decimal, and a real as SQLite 3.53.4 writes
it:

- with 17 significant digits, or fewer when SQLite's rule finds a shorter
  form that reads back to the same bits: 0.1 gives `0.1`, 0.1 + 0.2 gives
  `0.30000000000000004`, and 5e-324 gives `4.9406564584124654e-324`;
- with a decimal point even when it's a whole number: `1.0`, `100.0`;
- with an exponent of at least two digits, as `1.0e+21` or `1.0e-07`, when
  its decimal exponent is below -4 or above 16;
- as `0.0` for both zeros, and `Inf` and `-Inf` for the infinities.

It always reads back to the same bits. Go's shortest form differs for about
half of random reals, so Q1 needs SQLite's rule. SQLite's routines are
`vdbeMemRenderNum`, `sqlite3FpDecode`, and the `etGENERIC` case of
`sqlite3_str_vappendf`, with `nFpDigit` at 17.

### Text as numbers

A decimal number is an optional sign, then digits with an optional point and
fraction, or a point and digits, then an optional exponent: `e` or `E`, an
optional sign, and digits. Hex isn't read. A number too big becomes
infinity, and bytes are read as their text. Where a number is wanted, text
is read like this:

| Where | How | SQLite's routine |
|---|---|---|
| Arithmetic | the longest start, after leading spaces, that reads as a number. An integer when it has no point or exponent and fits in 64 bits, a real otherwise, and the integer 0 when nothing reads | `computeNumericType` |
| Comparisons against a numeric CAST | only when all of it reads as a number, apart from spaces at either end | `applyNumericAffinity` |
| CAST to INTEGER, and integer arguments such as `substr()`'s positions | the longest start, after leading spaces, that reads as an integer, clamped to 64 bits, and 0 when nothing reads | `sqlite3Atoi64` |
| CAST to REAL, truth, and real arguments such as `abs()`'s and `round()`'s | the longest start that reads as a number, as a real, and 0.0 when nothing reads | `sqlite3AtoF` |
| CAST to NUMERIC | the longest start that reads as a number. An integer when it's written as one that fits, or when its value is whole and less than 2^51 in size, and a real otherwise | `sqlite3VdbeMemNumerify` |
| `sum()`, `total()` and `avg()` | text that reads as a number, all of it, counts as that number. Other text counts as a real, read as for CAST to REAL | `sumStep` |

### CAST

`CAST(x AS type)` takes the type INTEGER, REAL, TEXT, BLOB or NUMERIC, in
any case. NULL stays NULL.

- INTEGER: an integer stays as it is, and a real is rounded toward zero and
  clamped to 64 bits. Text and bytes are read as the table above says.
- REAL: a number becomes a real, and text and bytes are read as above.
- NUMERIC: a number stays as it is, so `CAST(2.0 AS NUMERIC)` is the real
  2.0, and text and bytes are read as above.
- TEXT: a number becomes its text, and bytes become text with the same
  bytes.
- BLOB: text becomes bytes with the same bytes, and a number becomes the
  bytes of its text.

Other type names, such as INT or VARCHAR(10), are outside the subset.
SQLite's routine is `sqlite3VdbeMemCast`.

### Concatenation

`a || b` is NULL when either side is NULL. Otherwise it's the text of `a`
followed by the text of `b`, with numbers written as above and bytes taken
as they are, and the result is text.

### LIKE

`x LIKE p` is NULL when x or p is NULL. Otherwise it's 1 when x matches the
pattern p and 0 when it doesn't. Both sides are taken as text, with numbers
written as above and bytes taken as they are.

- `%` matches any run of characters, an empty one included, and `_` matches
  one character. Characters are UTF-8, as `sqlite3Utf8Read` reads them.
- ASCII letters match regardless of case, so 'Q3%' matches 'q3 plan'. Other
  characters match only themselves, so 'É%' doesn't match 'é'.
- `ESCAPE e` makes the character after e in p match only itself. e must be
  one character, and a NULL e gives NULL.
- Matching stops at a NUL byte in x or in p, since SQLite reads both as C
  strings.
- An e that isn't one character is an error, and so is a pattern longer
  than 50,000 bytes.
- `NOT LIKE` gives the opposite, and NULL stays NULL.

SQLite's routines are `likeFunc` and `patternCompare`.

### IN, BETWEEN and IS

- `x IN (a, b, ...)` is 1 when x equals an item. Otherwise it's NULL when x
  is NULL or an item is NULL, and 0 when neither is. An empty list gives 0,
  even when x is NULL. `NOT IN` gives the opposite, with NULL staying NULL,
  so an empty list gives 1.
- `x BETWEEN a AND b` is `x >= a AND x <= b`, with x worked out once.
  `NOT BETWEEN` is NOT of that.
- `x IS y` is 1 when both are NULL or they're equal, and 0 otherwise. It
  never gives NULL. `IS NOT` gives the opposite.

### What gets worked out

The order only shows when an operand raises an error, such as `abs()` of
the smallest integer, or `distance()` of two vectors of different sizes.

- In an expression, operands are worked out left to right, all of them,
  apart from these cases, as in SQLite:
  - When one side of AND or OR is an integer literal that fits in 32 bits,
    and it settles the result, the other side isn't worked out: `0 AND x`
    and `x AND 0` give 0, and `1 OR x` and `x OR 1` give 1. SQLite's routine
    is `sqlite3ExprSimplifiedAndOr`.
  - `coalesce()` and `ifnull()` stop at the first argument that isn't NULL.
  - `x IN (...)` works out x, then the items in turn, and stops at the first
    that equals x. An empty list doesn't work out x.
- In WHERE, AND stops at the first condition that's false or NULL, and OR
  stops at the first that's true. 0.x's planner may check conditions in
  another order, or skip rows by their key, so a test shouldn't count on an
  error from one condition when another rules the row out.
- ORDER BY terms are worked out for every row that passes WHERE, before
  LIMIT.

## Functions

Names match in any case. A function missing from these lists is an error,
"no such function", and so is a wrong number of arguments, "wrong number of
arguments to function".

### Text and number functions

| Function | What it gives |
|---|---|
| `abs(x)` | NULL for NULL. For an integer, its absolute value, and an error, "integer overflow", for -9223372036854775808. For anything else, the absolute value of x read as a real, which leaves -0.0 as -0.0 |
| `coalesce(x, y, ...)` | 2 arguments or more. The first that isn't NULL, or NULL, without working out the ones after it |
| `ifnull(x, y)` | `coalesce(x, y)` |
| `instr(x, y)` | NULL when either is NULL. Otherwise the position of the first y in x, counting from 1, and 0 when there's none. It counts bytes when both are bytes, and characters otherwise, with numbers and bytes taken as text. An empty y gives 1 |
| `length(x)` | NULL for NULL. Bytes for bytes, the characters before the first NUL byte for text, and the length of its text for a number |
| `lower(x)`, `upper(x)` | NULL for NULL. Otherwise x as text with its ASCII letters changed and every other byte as it was |
| `max(x, y, ...)`, `min(x, y, ...)` | 2 arguments or more. NULL when any is NULL. Otherwise the largest argument for `max()` and the smallest for `min()`, in the order of "Comparing", given back as it is. On a tie, `max()` keeps the earliest of the tied arguments and `min()` the latest, so `min(1, 1.0)` is 1.0 |
| `nullif(x, y)` | NULL when x and y are equal in the order of "Comparing", two NULLs included, and x otherwise |
| `replace(x, y, z)` | NULL when x or y is NULL. x as text when y is empty, even when z is NULL. Otherwise NULL when z is NULL, and else x as text with every y replaced by z, byte for byte |
| `round(x [, n])` | NULL when x or n is NULL. Otherwise a real: x read as a real, rounded to n places, with n clamped to 0 to 30 and 0 when left out. A real more than 2^52 in size stays as it is. With no places, SQLite adds or takes away a half and cuts to an integer, so halves go away from zero and `round(-0.4)` is 0.0. With places, the rounding is SQLite's printf `%!.nf` read back, so `round(2.675, 2)` is 2.67 |
| `substr(x, y [, z])` | NULL when x, y or z is NULL. Otherwise z characters of x's text, or z bytes of bytes, from position y, counting from 1, and to the end when z is left out. A negative y counts from the end, a negative z takes the characters before y, and y of 0 takes one fewer than y of 1 would. Text stops at a NUL byte |
| `trim(x [, y])` | NULL when x or y is NULL. Otherwise x as text, with any run of y's characters taken off both ends, and spaces when y is left out |
| `typeof(x)` | 'null', 'integer', 'real', 'text' or 'blob' |
| `CAST(x AS type)` | as "CAST" says |

SQLite's routines are `absFunc`, `instrFunc`, `lengthFunc`, `upperFunc`,
`lowerFunc`, `minmaxFunc`, `nullifFunc`, `replaceFunc`, `roundFunc`,
`substrFunc`, `trimFunc` and `typeofFunc`. `coalesce()` and `ifnull()` are
built into SQLite's code generator.

### Aggregates

| Function | What it gives |
|---|---|
| `count(*)` | the number of rows |
| `count(x)` | the number of rows where x isn't NULL |
| `sum(x)` | NULL when there are no rows or every x is NULL. Otherwise an integer when every x is an integer, with an error, "integer overflow", when the sum leaves 64 bits. Otherwise a real, added up as SQLite adds it, with Kahan-Babuska-Neumaier summation in the order of rows |
| `total(x)` | `sum(x)` as a real, 0.0 when there are no rows or every x is NULL, and never an error |
| `avg(x)` | NULL when there are no rows or every x is NULL. Otherwise the sum as a real, divided by the number of values |
| `min(x)`, `max(x)` | the smallest or largest x that isn't NULL, in the order of "Comparing", or NULL when there's none. On a tie, the earliest row's value |

NULL values don't count in any of them, apart from `count(*)`. "Text as
numbers" says how text counts in `sum()`, `total()` and `avg()`. SQLite's
routines are `countStep`, `sumStep`, `sumFinalize`, `totalFinalize`,
`avgFinalize` and `minmaxStep`.

A sum of reals depends on the order of its rows in the last bits. The Beta
adds a table's rows in key order and 0.x in its own scan order, so two sums
can differ where rows were inserted out of key order. Kahan-Babuska-Neumaier
summation makes that rare.

### HyperCrux's functions

| Function | What it gives |
|---|---|
| `distance(a, b)` | NULL when either is NULL. Otherwise the cosine distance, a real from 0 to 2: one minus the dot product over the two norms, in float64. It's 1 when either side is bytes whose values are all zero, -0 included. Each side is bytes, read as float32 values, or text, read as `vector()` reads it. Anything else is an error, and so are two sides of different lengths, bytes that aren't whole float32 values, an empty side, and a side holding NaN or infinity |
| `vector(x)` | NULL for NULL. For text, a JSON array of numbers, as Go's `encoding/json` reads it into float64 values, each made a float32. For bytes, whole float32 values, given back unchanged. Either way it needs 1 to 65,536 values, all finite and not all zero, with -0 counting as zero, so `'[1e39]'` and `'[1e-46]'` are errors. Anything else is an error. The result is bytes |
| `walk(start, depth [, type [, direction]])` | as "Walks" says |

The Beta adds a dot product in 8 running sums (A5.md), so its distances may
differ from 0.x's in the last bits. `conformance.DistanceBound`, 1e-9, is the
allowance, and the corpus marks such cases `close`.

### Date functions

`date()` and `datetime()`, as "Dates" says.

### Left for later

SQLite's other functions, among them `ltrim()`, `rtrim()`, `hex()`,
`quote()`, `printf()`, `format()`, `iif()`, `char()`, `unicode()`,
`random()`, `group_concat()`, the JSON functions apart from json_each over a
walk, and SQLite's own functions such as `sqlite_version()`.

## Errors and their kinds

difftest compares errors by kind, and so does the corpus. An error from SQL
is "invalid", which wraps `ErrInvalid`, or "error", which wraps neither of
the engine's two errors. SQL never gives "not found".

| What went wrong | Kind | 0.x's message, for reference |
|---|---|---|
| A syntax error, an unfinished statement, or a token SQLite doesn't know | error | `near "X": syntax error`, `incomplete input`, `unrecognized token: "X"` |
| No statement, or more than one | error | |
| A statement outside the subset | error | 0.x runs it |
| A table that doesn't exist | error | `no such table: X` |
| A field that doesn't exist, or a name the statement can't see | error | `no such column: X`, `table X has no column named Y` |
| A name two sources have | error | `ambiguous column name: X` |
| A function that doesn't exist, or the wrong number of arguments | error | `no such function: X`, `wrong number of arguments to function X()` |
| An aggregate where it can't go | error | `misuse of aggregate: X()` |
| An ORDER BY column number out of range | error | `1st ORDER BY term out of range - should be between 1 and N` |
| LIMIT or OFFSET that isn't a whole number | error | `datatype mismatch` |
| Fewer arguments than `?` marks, or more | error | `not enough args to execute query: want N got M`, and 0.x ignores extra arguments |
| `abs()` of the smallest integer, or `sum()` past 64 bits | error | `integer overflow` |
| An ESCAPE that isn't one character, or a LIKE pattern over 50,000 bytes | error | `ESCAPE expression must be a single character`, `LIKE or GLOB pattern too complex` |
| `distance()`, `vector()` or `walk()` refusing their arguments, in a SELECT | error | `hypercrux: ...` |
| The same in an INSERT, UPDATE or DELETE | invalid | `hypercrux: invalid: ...` |
| An INSERT without a key, or with a NULL key | error | `NOT NULL constraint failed: docs.key` |
| An INSERT of a key that's taken, twice in one statement included | error | `UNIQUE constraint failed: docs.key` |
| A key that doesn't fit the table: not text, another table's, nothing after the colon, over 1,024 bytes, or not valid UTF-8 without NUL bytes | invalid | `keys in table docs are text that starts with docs:, up to 1024 bytes` |
| An UPDATE that changes a key | invalid | `a key never changes; delete the record and put it again` |
| A value for `vec` that isn't a vector the table takes | invalid | `vec must be a blob of float32 values, the same size in the whole table, and not all zero` |
| A value `Put` refuses, for any other field: an infinite real, or text that isn't valid UTF-8 | invalid | 0.x stores it |
| A field named twice in one INSERT or UPDATE | error | 0.x takes one of the values |

For a key that doesn't fit, a key that changes and a `vec` that doesn't fit,
the Beta keeps 0.x's messages word for word, after `hypercrux: invalid: `,
since the command's tests look for the first of them. Other messages are
free. SQLite's wording is a good default, since people search for it.

The filter given to `Nearest` counts as part of a SELECT, so its errors are
of kind "error", as in 0.x.

These kinds are those of `Query`, `QueryRow` and `Exec` on a database or a
transaction. Through `SQL()`, 0.x wraps no errors at all, even in an
`Exec`. G3 settles what `SQL()` gives in the Beta.

## 0.x's small behaviours, settled

- **Field names match regardless of case.** `Put` and SQL alike match a
  field's name with its ASCII letters folded, and field names are ASCII. A
  field keeps the spelling of the first `Put` that used it, and `Get`,
  `Scan` and result columns show that spelling. A `Put` that gives one field
  in two spellings is invalid, as in 0.x. Table names in SQL match the same
  way.
- **`SELECT *` order:** `key`, then the table's fields in the order `Put`
  first used them. A `Put` adds its new fields in byte order of their
  names, so a first `Put` of title, n and vec adds n, then title, then vec.
  `vec` is a field like the others in this order, and links never show.
- **Comparisons on untyped fields:** fields compare as they are, without
  conversion, as "Conversion before comparing" says.
- **The function form of `walk()`:** in, as 0.x has it, giving JSON text.
- **The columns of the walk as a table:** `key` and `depth`.
- **"no such table" and the like:** kind "error", as in 0.x.
- **CAST and `nullif()`:** both in. The expression corpus has 627
  expressions with CAST and 141 with `nullif()`.

## Where the Beta differs from 0.x

On purpose, and outside the corpus:

- A statement outside the subset is an error, where 0.x runs it. That takes
  in small things as well as whole features: backquoted and bracketed
  names, text in single quotes as a name or an alias, hex numbers, digit
  separators, numbered and named parameters, and `TRUE` and `FALSE`.
- More arguments than `?` marks is an error, where 0.x ignores the extra
  ones.
- A semicolon followed by a comment is fine. 0.x then gives no rows and no
  columns, since go-sqlite3 runs the comment as a second statement.
- Text after the semicolon is an error, where 0.x runs each statement and
  gives the last one's rows.
- A double-quoted name that isn't a field is an error, "no such column".
  0.x reads it as text, as SQLite does by default.
- A result column's alias can't be used in WHERE, where SQLite allows it
  when no field has that name.
- A field named twice in one INSERT or UPDATE is an error. 0.x keeps the
  first value in an INSERT and the last in an UPDATE.
- Values `Put` refuses can't be written through SQL either: an infinite
  real, text that isn't valid UTF-8, a key with a NUL byte or that isn't
  valid UTF-8, and a vector holding NaN or infinity or only zeros and -0.
  0.x stores them, and its export refuses them later (EXPORT.md).
- An error from `vector()` on bytes that aren't whole float32 values is
  "invalid" in a write, as the other errors of HyperCrux's functions are.
  0.x gives "error" there, since that one message lacks the "hypercrux: "
  prefix its error wrapping looks for.
- An error's kind follows the statement. 0.x wraps errors in `Exec` only, so
  a write it runs through `Query` reports a broken rule as "error", where the
  Beta reports "invalid".
- 'now' is read once for each statement. 0.x reads it again for each row it
  returns, which only shows when a query's rows fall either side of a
  second.
- Rows without ORDER BY come in key order, where 0.x's order depends on
  SQLite's plan.

## The named tests

The Beta must pass these tests from 0.x, by name: the conformance suite in
`beta/conformance`, which `conformance.Tests()` lists, and the sections of
the command's test, `TestCommands` in `beta/conformance/cmdtest`. A row
saying "left out" names a test the Beta skips, and why.
`TestEveryNamedTestExists`, in `beta/conformance/cmdtest`, checks this table
against both: every row names a test that exists, and every test has a row.

| Test | Suite | For the Beta |
|---|---|---|
| `OpenAndReopen` | conformance | must pass |
| `OpenNeedsAFile` | conformance | must pass |
| `PutAndGetRoundTrip` | conformance | must pass |
| `PutKeepsFieldsItIsNotGiven` | conformance | must pass |
| `KeyAndFieldRules` | conformance | must pass |
| `MoreFieldTypes` | conformance | must pass |
| `GetAndDeleteMissing` | conformance | must pass |
| `Scan` | conformance | must pass |
| `NewFieldsShowAtOnce` | conformance | must pass |
| `LinksFollowTheirRecords` | conformance | must pass |
| `Walk` | conformance | must pass |
| `RulesForLongKeysAndLinkTypes` | conformance | must pass |
| `TableNamesLikeHyperCruxsOwn` | conformance | must pass |
| `DropTable` | conformance | must pass |
| `Vectors` | conformance | must pass |
| `NearestIsExact` | conformance | must pass |
| `WalkInSQL` | conformance | must pass |
| `OneStatementCrossesAllFour` | conformance | must pass |
| `VectorAsQueryArgument` | conformance | must pass |
| `PlainSQLFollowsTheRules` | conformance | must pass |
| `UpdateIsAllOrNothing` | conformance | must pass |
| `GoroutinesShareADB` | conformance | must pass |
| `OneConnection` | conformance | must pass |
| `KilledWritersNeverLeaveAMess` | conformance | must pass |
| `ProcessesShareAFile` | conformance | must pass |
| `VersionAndHelp` | command | must pass |
| `PutGetAndLink` | command | must pass |
| `Scan` | command | must pass |
| `NeighboursAndWalk` | command | must pass |
| `Nearest` | command | must pass |
| `SQL` | command | must pass |
| `SQLTriggers` | command | left out: it creates and drops one of SQLite's triggers |
| `UnlinkDeleteAndCheck` | command | must pass |
| `AdoptAndDrop` | command | left out: it makes a table with plain SQL and adopts it, and the Beta has neither |
| `ExportAndImport` | command | must pass |
| `Mistakes` | command | must pass |

The SQL in these tests is inside the subset. The tests that need care:

- WalkInSQL and UpdateIsAllOrNothing use the function form of `walk()`.
- NearestIsExact gives `Nearest` the filter `"group" = ?`, with a quoted
  name.
- PlainSQLFollowsTheRules writes the key 7, a vector of the wrong size, text
  into `vec` and a vector of zeros, and each must be invalid.
- The command's SQL section looks for the message "keys in table docs are
  text that starts with docs:", and puts a record with `{"phone": null}`
  before setting `phone` through SQL, so that `Put` must add a field even
  when its value is nil.
- The Beta's run of the command's tests sets
  `HYPERCRUX_CMD_SKIP=SQLTriggers,AdoptAndDrop`. AdoptAndDrop also runs the
  `drop` command, which no other section covers.

The tests A1.md lists as left in 0.x, such as TestAdopt and TestStrictTables,
aren't in either suite, and the Beta doesn't run them. The SQL corpus comes
on top of the named tests: Q1 to Q6 and G4 replay its cases marked in.
