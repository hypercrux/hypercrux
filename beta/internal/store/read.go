// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"iter"
	"strconv"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Reader is the store's read API: the in-memory copy as it stands at one
// point in the log. The public package's Get, Scan, Neighbours, Walk and
// Nearest read through it, and so do SQL's operators, check, export and
// compaction.
//
// A Reader comes from the store under the copy's shared lock, for a read
// outside Update, or it's the transaction inside Update, which sees its own
// changes. It's good until that read or transaction ends. What it hands out
// is good for as long, or inside a transaction until the transaction's
// next change, since records and tables share the store's memory. So
// nothing in them may be changed, and a caller that keeps anything longer
// copies it out. One goroutine uses a Reader at a time.
//
// Each method checks its arguments by 0.x's rules, in 0.x's order, and
// fails as 0.x's method of the same name does, with errors that wrap
// errs.ErrInvalid or errs.ErrNotFound.
type Reader interface {
	// Table returns the table called name, or false when there's none.
	Table(name string) (Table, bool)

	// Get returns the record with this key.
	Get(key string) (Record, error)

	// Scan returns the records whose keys start with prefix and come after
	// after, in byte order of key. The prefix starts with a table's name
	// and a colon, as 0.x's does, and a table that doesn't exist gives no
	// records. Scan takes no limit: a caller stops when it has enough.
	Scan(prefix, after string) (Cursor, error)

	// Neighbours returns the links of the record with this key, out of it,
	// into it or both, of the type typ, or of every type when typ is "".
	// They come in 0.x's order: by type and then by the key at the other
	// end, or, both ways, by type, then from, then to.
	Neighbours(key string, dir Direction, typ string) ([]Link, error)

	// Walk returns every record within depth links of the record with this
	// key, following links of the type typ, or of every type when typ is
	// "", in the direction dir. Each comes with the fewest links it takes,
	// nearest first and then by key, and the record itself isn't among
	// them. depth runs from 1 to 32.
	Walk(key string, dir Direction, typ string, depth int) ([]Step, error)

	// Nearest returns the k records of the table whose vectors are closest
	// to q by cosine distance, closest first and then by key, comparing
	// every vector that keep lets through. A nil keep lets every record
	// through. k runs from 1 to 10,000, and a table with no vector size
	// yet gives no hits.
	Nearest(table string, q []float32, k int, keep Filter) ([]Hit, error)

	// Snapshot returns the whole copy as the changes of a compacted part,
	// in FORMAT.md's order. Each table comes in byte order of name, as a
	// CreateTable with its vector size and its whole field list, and then
	// a Put for each of its records in byte order of key. A Put carries
	// every field that holds a value, the vector among them, in byte order
	// of name. Every link follows, as a Link, in byte order of the key
	// it's from, then its type, then the key it's to. Compaction writes the
	// changes out (F8), export reads them (G6), and a store that applies
	// them ends up holding what this one holds (S3).
	//
	// It streams one change at a time, so it copies nothing of the
	// database beyond the record it's on. A Put's Fields slice is used
	// again for the next Put, so a caller that keeps one copies the slice;
	// the values in it never change.
	Snapshot() iter.Seq[format.Change]
}

// Cursor reads records one at a time, in byte order of key. SQL's scan
// pulls its rows through one, so a query with a LIMIT can stop early.
type Cursor interface {
	// Next returns the next record, or false when there are no more.
	Next() (Record, bool)
}

// Filter says whether a record takes part in a search: true lets it
// through, and an error stops the search, which Nearest then returns.
// Nearest calls it once for each record that has a vector, before working
// out that record's distance, in no particular order. It may call it from
// more than one goroutine at once, so that a search can be split across
// cores later, and a Filter has to be safe for that. SQL makes one from a
// WHERE clause (Q5).
type Filter func(r Record) (bool, error)

// Table is a table's shape.
type Table struct {
	Name string
	// Fields are the table's fields in its order, which is the order
	// SELECT * shows them in, with the vector field among them once a put
	// has named it.
	Fields []string
	// Vec is the vector field's place in Fields, or -1 when the table has
	// none. The vector field is the one whose name matches "vec"
	// regardless of case.
	Vec int
	// Size is the table's vector size: 0 until its first vector sets it,
	// and then that vector's length until the table is dropped.
	Size int
}

// Record is one record as a read sees it.
type Record struct {
	Key string
	// Fields holds the fields that have a value, by their place in the
	// table's field list, in that order. A null field isn't there, and
	// nor is the vector, which is in Vec.
	Fields []FieldValue
	// Vec is the record's vector, or nil when it has none. It's the
	// record's slot in its table's vector array (V1), shared with the
	// store, so it holds as long as the rest of the record does.
	Vec []float32
}

// FieldValue is one field of a record: its place in the table's field
// list, and its value, which isn't null.
type FieldValue struct {
	Index int
	Value value.Value
}

// Field returns the value of the field at place i in the table's field
// list, or null when the record has none there. The vector field gives
// null too, since the record's vector is in Vec.
func (r Record) Field(i int) value.Value {
	for _, f := range r.Fields {
		if f.Index >= i {
			if f.Index == i {
				return f.Value
			}
			break
		}
	}
	return value.Value{}
}

// Direction picks which links to follow from a record, as in 0.x.
type Direction int

const (
	Out  Direction = iota // links from the record to others
	In                    // links from others to the record
	Both                  // either way
)

func (d Direction) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	case Both:
		return "both"
	}
	return "Direction(" + strconv.Itoa(int(d)) + ")"
}

// Link is a typed, one-way connection between two records.
type Link struct {
	From string
	Type string
	To   string
}

func (l Link) String() string { return l.From + " -" + l.Type + "-> " + l.To }

// Step is one record a walk reached, and how many links it took to get
// there.
type Step struct {
	Key   string
	Depth int
}

// Hit is one result of a search.
type Hit struct {
	Key      string
	Distance float64 // cosine distance, from 0 (the same direction) to 2
}
