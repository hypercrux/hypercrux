// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import "io"

// Export writes the whole database to w as JSON lines, in the format
// EXPORT.md describes, which 0.x's Export writes too. Import reads it back,
// here or in 0.x, so a database moves between the two either way.
//
// Export reads a single point in the log. It refuses a database that Check
// finds problems in. What it has written by then stops short of the
// export's last line, and Import refuses an export without it.
func (db *DB) Export(w io.Writer) error { return notYet("DB.Export", "G6") }

// Import reads an export, as Export writes it, into this database, which
// must hold no tables yet. It all goes in one transaction: if the export is
// cut short, or anything in it breaks the rules Put and Link follow,
// nothing is kept. Tables come back with their fields in the same order and
// the same vector sizes, so exporting the result gives the same bytes.
func (db *DB) Import(r io.Reader) error { return notYet("DB.Import", "G6") }
