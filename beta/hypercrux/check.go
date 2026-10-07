// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

// Report is what Check found.
type Report struct {
	Tables   int      // tables
	Records  int      // records in them
	Links    int      // links between records
	Vectors  int      // records with a vector
	Problems []string // empty when the database is consistent
}

// OK reports whether Check found no problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Check reads the whole database and confirms that the four ways into it
// agree. Every key starts with its table's name, and every link joins two
// records that exist and is listed at both ends. Every vector has its
// table's size and holds finite values that aren't all zero. Check also
// names a damaged batch in the file. It reads a single point in the log.
func (db *DB) Check() (Report, error) { return Report{}, notYet("DB.Check", "G7") }
