// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package hypercrux

import (
	"os"
	"slices"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - hypercrux/append-error-dropped: Update takes no notice when Append
//     fails, so the copy keeps a commit the file may not have.
//   - hypercrux/reset-kept: Reset keeps the old copy, so the batches of a
//     file that has taken the path go in on top of it.
//   - hypercrux/load-after-open: batches the log reads after Open go
//     straight into the copy, as on opening, instead of through a
//     transaction, so a batch that breaks the rules goes in by halves.
//   - hypercrux/vector-shared: Get hands out the copy's own vector, so a
//     caller that changes it changes the database.
//   - hypercrux/scan-limit-first: Scan checks its limit before its prefix,
//     so a bad prefix with a limit below 0 gives the limit's error, where
//     0.x gives the prefix's.
//   - hypercrux/scan-vector-kept: Scan gives each record's vector among its
//     fields, which 0.x's Scan leaves out.
//   - hypercrux/neighbours-unlocked: DB.Neighbours reads the copy without
//     its Read, so it neither waits for an Update's changes to commit nor
//     fails inside one, and can read changes that roll back.
//   - hypercrux/nearest-nil-for-none: Nearest gives nil for a search that
//     finds nothing in a table with a vector size, where 0.x gives an empty
//     list.
//   - hypercrux/filter-ignored: Nearest with a filter searches without it.
//   - hypercrux/blob-shared: the driver hands bytes and vectors to
//     database/sql without copying them, so a caller that writes into a
//     sql.RawBytes writes into the store's copy.
//   - hypercrux/time-layout: the driver turns a time.Time argument into text
//     in RFC 3339's layout, where go-sqlite3 writes
//     2006-01-02 15:04:05.999999999-07:00.
//   - hypercrux/tx-dropped: a Tx's Query, QueryRow and Exec run through the
//     database, outside the transaction's Update.
//   - hypercrux/row-kept: the driver keeps the slice the engine hands over
//     for each row without copying its values, so when the engine uses one
//     buffer for every row, every row reads as the last.
//   - hypercrux/rows-pulled-late: the driver reads a SELECT's rows from the
//     engine as database/sql asks for them, after the read has ended, so
//     they see changes made since Query returned.
//   - hypercrux/write-kept: a write that fails inside an Update keeps the
//     changes it made before it failed, so the Update commits half a
//     statement.
//   - hypercrux/sql-wraps-invalid: an error through SQL() still wraps
//     ErrInvalid, where 0.x's wraps nothing.
//
// F9's, in reads that follow other processes and reload:
//
//   - hypercrux/reads-never-follow: a read through the database reads the
//     copy as it is, without following the file, so it sees other
//     processes' commits only after an Update of its own.
//   - hypercrux/reload-unheld: a reload doesn't hold the reads off, so a read
//     made while it fills the new copy sees part of the new file.
//   - hypercrux/reload-keeps-the-old-copy: a reload keeps the old copy until
//     the new file has been read, so the garbage collection before the read
//     can't take it, and memory holds the two at once.
//   - hypercrux/failed-reload-read: after a reload that failed, a read that
//     finds an Update holding the write lock reads the copy, which holds part
//     of the new file, where it should give the reload's error.
//
// G6's, in export and import:
//
//   - hypercrux/export-fields-sorted: Export writes each table's fields,
//     and so each record's, in byte order of name, where 0.x writes them in
//     the table's order. An export and an import in the Beta alone still
//     agree, and 0.x's export doesn't.
//   - hypercrux/export-unlocked: Export reads the copy without its Read, so
//     it neither waits for an Update's changes to commit nor fails inside
//     one, and can write changes that roll back.
//   - hypercrux/import-size-dropped: Import makes each table without its
//     vector size, so the table's first vector sets it again, and a table
//     whose vectors have all gone loses it.
//   - hypercrux/import-kept-in-part: an import that fails on a line commits
//     what came before it, and still gives the error.
//   - hypercrux/import-twice-merged: a record that's there twice is merged
//     into the first, as Put merges, and a link that's there twice is taken
//     as made, where both are refused.
//   - hypercrux/import-over-tables: Import goes ahead in a database that
//     holds tables, where it should refuse.
//
// G7's, in Check, and in Export's check:
//
//   - hypercrux/check-file-skipped: Check reads the copy alone, and never
//     reads the file again, so damage in the part of the file this process
//     read long ago goes unseen until the next Open.
//   - hypercrux/check-marker-at-the-batch: Check names damage in a batch's
//     marker at the batch's offset, where it should name the marker's.
//   - hypercrux/check-follow-damage-dropped: Check takes no notice of
//     damage the read before it meets as it follows the file, and reports
//     the copy alone.
//   - hypercrux/check-reload-copy-read: after a reload that failed on
//     damage, Check checks and counts the copy, which holds part of the file
//     at the path, as if it held the database.
//   - hypercrux/export-unchecked: Export writes a copy that Check finds
//     problems in, where it should refuse, as 0.x's does.
//   - hypercrux/vector-bits-shared: a Vector put in the vector field becomes
//     a value that shares the caller's memory, so a caller that fills the
//     same buffer again before the commit changes what the batch holds, and
//     the file differs from the copy.
//
// G4's, in the engine, Nearest's filter and Update:
//
//   - hypercrux/moment-kept: the engine reads its moment for the first
//     statement only, and every later statement gets the same one, so
//     date('now') stands still.
//   - hypercrux/vector-argument-shared: a Vector given as an argument
//     becomes a value that shares the caller's memory, so a caller that
//     changes the vector after a write changes what the copy holds, and the
//     file differs from it.
//   - hypercrux/filter-checks-reordered: Nearest reads its filter before
//     0.x's checks, so a broken filter fails a search of a table with no
//     vectors yet, or of one that doesn't exist, where 0.x gives no hits or
//     says the table isn't found.
//   - hypercrux/filter-none-nil: a filter whose constant terms rule every
//     record out gives no hits as nil, where 0.x gives an empty list.
//   - hypercrux/unlock-error-kept: when only letting go of flock fails after
//     a commit, Update returns an error, though the commit is in the file,
//     so a caller makes it again.
//
// G5's, in Compact and the compaction after a commit:
//
//   - hypercrux/compaction-error-returned: when the compaction after a
//     commit fails, Update returns its error, though the commit is in the
//     file, so a caller makes it again.
//   - hypercrux/compaction-error-dropped: a compaction's error isn't kept,
//     so Check never shows that one failed.
//   - hypercrux/no-compaction-after-commit: Update never compacts, so a file
//     written over again and again grows without end.
//   - hypercrux/compact-stale-snapshot: Compact takes its snapshot from the
//     copy as it is, before the write lock reads on to the end of the log,
//     so the compacted file leaves out what other processes committed since
//     this one last read the file.
//
// V2's, in vector blocks:
//
//   - hypercrux/compaction-without-blocks: a compaction writes the copy's
//     snapshot as S3 gave it, each vector in its record's put, so opening
//     the compacted file decodes and checks every vector as a value again.
//
// C1's, in when a compaction is due:
//
//   - hypercrux/dead-uncounted: the copy tells the log of no dead bytes, so
//     a database whose commits write over its data is compacted only at the
//     limit on commits, and its file grows to many times its live data.
//
// R2's:
//
//   - hypercrux/check-copy-over-unseen: Check's read of the file takes the
//     header and the batches it finds there, as before R2, so another
//     database's file of the same length copied over the file in place reads
//     as sound (G7.md's note).
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "hypercrux/append-error-dropped", "hypercrux/reset-kept", "hypercrux/load-after-open", "hypercrux/vector-shared",
		"hypercrux/scan-limit-first", "hypercrux/scan-vector-kept", "hypercrux/neighbours-unlocked",
		"hypercrux/nearest-nil-for-none", "hypercrux/filter-ignored",
		"hypercrux/blob-shared", "hypercrux/time-layout", "hypercrux/tx-dropped", "hypercrux/row-kept",
		"hypercrux/rows-pulled-late", "hypercrux/write-kept", "hypercrux/sql-wraps-invalid",
		"hypercrux/reads-never-follow", "hypercrux/reload-unheld", "hypercrux/reload-keeps-the-old-copy", "hypercrux/failed-reload-read",
		"hypercrux/export-fields-sorted", "hypercrux/export-unlocked", "hypercrux/import-size-dropped",
		"hypercrux/import-kept-in-part", "hypercrux/import-twice-merged", "hypercrux/import-over-tables",
		"hypercrux/check-file-skipped", "hypercrux/check-marker-at-the-batch", "hypercrux/check-follow-damage-dropped",
		"hypercrux/check-reload-copy-read", "hypercrux/export-unchecked", "hypercrux/vector-bits-shared",
		"hypercrux/moment-kept", "hypercrux/vector-argument-shared", "hypercrux/filter-checks-reordered",
		"hypercrux/filter-none-nil", "hypercrux/unlock-error-kept",
		"hypercrux/compaction-error-returned", "hypercrux/compaction-error-dropped", "hypercrux/no-compaction-after-commit",
		"hypercrux/compact-stale-snapshot",
		"hypercrux/compaction-without-blocks",
		"hypercrux/dead-uncounted", "hypercrux/check-copy-over-unseen":
		return p
	}
	return ""
}()

// compactStale is hypercrux/compact-stale-snapshot's Compact: the snapshot
// comes from the copy s as it is, before Lock reads on to the end of the
// log, and goes into the compacted file once the lock is held.
func (db *DB) compactStale(s *store.Store) error {
	var changes []format.Change
	err := s.Read(func(r store.Reader) error {
		for c := range r.Snapshot() {
			c.Names, c.Fields = slices.Clone(c.Names), slices.Clone(c.Fields)
			changes = append(changes, c)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := db.log.Lock(); err != nil {
		return err
	}
	defer db.log.Unlock()
	err = db.log.Compact(slices.Values(changes))
	db.keep(err)
	return err
}

// shared returns the bytes of s without copying them, for
// hypercrux/blob-shared, and an empty slice for empty s, as []byte(s) does.
func shared(s string) []byte {
	if s == "" {
		return []byte{}
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// sharedVector returns v's memory as the bits of its values, without
// copying it, for hypercrux/vector-argument-shared. On a little-endian
// machine, as amd64 and arm64 are, a float32's memory is its bits in the
// order SQL gives them.
func sharedVector(v Vector) string {
	return unsafe.String((*byte)(unsafe.Pointer(unsafe.SliceData(v))), 4*len(v))
}
