# Damaged databases

Each `.hcx` file here is a Beta database file, damaged in one of the ways
[FORMAT.md](../../../../FORMAT.md) calls damage. Anything that reads one has
to report the damage, naming the batch and the offset that `damage.txt`
gives, and leave the file's bytes as they were: nothing in it may be cut,
written again or marked.

`damage.txt` lists them, one a line: the file, the batch its damage names
(0 for the header), the offset where the damage was found, and what's
damaged.

## What they hold

Every file is made from one of two small databases, with the database ID
`damaged fixtures` in ASCII:

- **A new one**, generation 1, of four commits: a table `docs` created with
  the record `docs:1`, a put of `docs:2`, a table `people` created with
  `people:1`, and the delete of `docs:1`.
- **The same state compacted**, generation 2, continuing from the new
  one's batch 4: a compacted part of two batches, one for each table with
  its whole field list and its records, and a commit after it that puts
  `docs:3`.

A name ending in `-end` is damaged in the log's last batch, so the only sign
past the end of the log is that batch's own marker. A name ending in
`-middle` is damaged in batch 2, with batches 3 and 4 after it, which a
reader must never take for the remains of a crash.

The kinds of damage, after FORMAT.md:

- the header: one that fails its checksum, and a compacted file cut short
  inside its compacted part;
- a batch that counts, with a change that's malformed, or that breaks the
  rules for a change on its own, marked or not;
- a marked batch whose changes break the rules for the state they apply
  to, a put into a table that doesn't exist;
- a batch that counts, followed by a whole marker naming another batch;
- a marked batch that fails one of the checks for a batch that counts (its
  checksum, magic number, generation, length or sequence number), or whose
  head or whole length is zeros, or that's missing, so that a whole marker
  past the end of the log names it or a later one;
- a batch that counts whose marker is damaged, with marked batches after
  it, which the check of the end of the log would otherwise mark, cutting
  off the batches after it;
- in a compacted file, a log that ends before the compacted part does.

## How they're made and changed

`damagedFixtures` in [damaged_test.go](../../damaged_test.go) builds every
file with the codec, then damages it, and says what reading it must
report. `go test ./beta/internal/logfile` checks that the files here are
the ones it builds, byte for byte, that `damage.txt` lists them, and that
opening each one and locking it reports its damage and changes nothing.

To change them, change `damagedFixtures`, then write the files and
`damage.txt` again:

```
HYPERCRUX_LOGFILE_RECORD=1 go test -run TestTheDamagedFixtures ./beta/internal/logfile
```

That also removes a `.hcx` file the table no longer names. A change to the
format raises its version, and every file here is written again with it.

## Who reads them

The log's tests, as above. G7's `check` and the `hypercrux check` command
can read them as they are, from this folder, and hold their reports to
`damage.txt`.
