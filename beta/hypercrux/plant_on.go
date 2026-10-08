// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package hypercrux

import (
	"os"
	"unsafe"
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
//   - hypercrux/filter-ignored: Nearest with a filter searches without it,
//     where it should wait for task G4 as a stub.
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
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "hypercrux/append-error-dropped", "hypercrux/reset-kept", "hypercrux/load-after-open", "hypercrux/vector-shared",
		"hypercrux/scan-limit-first", "hypercrux/scan-vector-kept", "hypercrux/neighbours-unlocked",
		"hypercrux/nearest-nil-for-none", "hypercrux/filter-ignored",
		"hypercrux/blob-shared", "hypercrux/time-layout", "hypercrux/tx-dropped", "hypercrux/row-kept",
		"hypercrux/rows-pulled-late", "hypercrux/write-kept", "hypercrux/sql-wraps-invalid",
		"hypercrux/reads-never-follow", "hypercrux/reload-unheld", "hypercrux/reload-keeps-the-old-copy", "hypercrux/failed-reload-read":
		return p
	}
	return ""
}()

// shared returns the bytes of s without copying them, for
// hypercrux/blob-shared, and an empty slice for empty s, as []byte(s) does.
func shared(s string) []byte {
	if s == "" {
		return []byte{}
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
