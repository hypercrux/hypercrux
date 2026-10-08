// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

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
//
// Until task G7 writes those checks, Check only counts what the database
// holds, and finds no problems.
func (db *DB) Check() (Report, error) {
	s, err := db.current()
	if err != nil {
		return Report{}, err
	}
	var rep Report
	err = s.Read(func(r store.Reader) error {
		rep = count(r)
		return nil
	})
	return rep, err
}

// count counts the tables, records, links and vectors in r, from its
// snapshot, which holds each table as a CreateTable, each record as a Put
// with its vector among its fields, and each link as a Link once S5 adds
// them.
func count(r store.Reader) Report {
	var rep Report
	for c := range r.Snapshot() {
		switch c.Op {
		case format.CreateTable:
			rep.Tables++
		case format.Put:
			rep.Records++
			for _, f := range c.Fields {
				if f.Value.Kind() == value.KindVector {
					rep.Vectors++
					break
				}
			}
		case format.Link:
			rep.Links++
		}
	}
	return rep
}
