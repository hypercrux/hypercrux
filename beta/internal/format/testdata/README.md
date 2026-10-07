# The format's golden fixtures

Each `.hex` file holds bytes in the Beta's file format, as pairs of hex
digits, with a comment on every field. Everything from a `#` to the end of a
line is a comment. They follow [FORMAT.md](../../../FORMAT.md), and
`fixtures_test.go` checks them against it.

| Fixture | What it holds |
|---|---|
| `header-new.hex` | A new database's header, generation 1 |
| `header-compacted.hex` | A compacted database's header, generation 2: the one in `file-compacted.hex` |
| `batch-create-table.hex` | A batch creating a table, with a vector size and fields, as compaction writes it |
| `batch-put.hex` | A batch putting fields, with every kind of value and some awkward ones |
| `batch-delete.hex` | A batch deleting a record |
| `batch-link.hex` | A batch adding a link |
| `batch-unlink.hex` | A batch removing a link |
| `batch-drop.hex` | A batch dropping a table |
| `batch-several.hex` | A batch of several changes, as an ordinary first commit writes them |
| `marker.hex` | The marker for `batch-several.hex` |
| `file-new.hex` | A small database: a header and three batches, each with its marker |
| `file-compacted.hex` | The same database compacted, then one more commit |

- The batches with one change each stand alone, to show the encoding, so
  their changes needn't fit any state. They all have sequence number 258,
  which shows a u64's byte order.
- The two small databases are whole: every change in them is valid where it
  stands, and the compacted one holds the same state as the new one, in the
  order FORMAT.md gives for a compacted part.
- Every fixture uses the database ID `0123456789abcdef fedcba9876543210`.

The codec has to match these bytes exactly, so they change only along with
FORMAT.md. To change one, edit its bytes and comments and run `go test
./beta/internal/format`; a checksum that no longer fits is reported with the
value that does.
