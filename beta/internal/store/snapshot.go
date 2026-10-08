// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"iter"
	"slices"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Snapshot returns the whole copy as the changes of a compacted part, in
// FORMAT.md's order, as Reader's Snapshot says. Each table comes in byte
// order of name, as a CreateTable with its vector size and its whole field
// list, and then a Put for each of its records in byte order of key. A Put
// carries every field that holds a value, the vector among them, each spelt
// as the table spells it, in byte order of name. The links follow the
// tables once S5 adds them.
//
// The changes are built one at a time as they're ranged over, so a caller
// ranges over them inside the Read that handed it the store, or inside the
// transaction, while nothing changes the store. Ranging again starts again
// from the first table. A CreateTable's Names are the table's own list,
// which nothing may change. A Put's Fields slice is used again for the
// next Put, so a caller that keeps a Put clones its Fields.
//
// Until S4 keeps each table's keys in byte order, the snapshot sorts each
// table's keys itself before its first change, with a pointer to every
// record in lists of its own.
func (s *Store) Snapshot() iter.Seq[format.Change] { return s.snapshot }

func (s *Store) snapshot(yield func(format.Change) bool) {
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	order := s.byKey()
	var fields []format.Field // the Put's, used again for each record
	for _, name := range names {
		t := s.tables[name]
		create := format.Change{Op: format.CreateTable, Table: t.name, Size: t.size, Names: clip(t.fields)}
		if plant == "store/snapshot-size-left-out" {
			create.Size = 0
		}
		if !yield(create) {
			return
		}
		for _, r := range order[t] {
			fields = r.snapshot(fields[:0])
			if !yield(format.Change{Op: format.Put, Key: r.key, Fields: clip(fields)}) {
				return
			}
		}
	}
	// S5 adds every link here, as a Link, in byte order of the key it's
	// from, then its type, then the key it's to.
}

// byKey returns each table's records in byte order of key. Until S4 keeps
// them in that order as keys come and go, it looks at every record once,
// then sorts each table's.
func (s *Store) byKey() map[*table][]*record {
	order := make(map[*table][]*record, len(s.tables))
	for _, r := range s.records {
		order[r.table] = append(order[r.table], r)
	}
	if plant != "store/snapshot-in-map-order" {
		for _, recs := range order {
			slices.SortFunc(recs, func(a, b *record) int { return strings.Compare(a.key, b.key) })
		}
	}
	return order
}

// snapshot appends the record's fields to dst as its Put in a snapshot
// carries them: every field that holds a value, the vector among them, each
// spelt as its table spells it, in byte order of name. The record holds its
// fields in its table's order, which can differ from byte order, since a
// later put's new fields join the end of the list.
func (r *record) snapshot(dst []format.Field) []format.Field {
	t := r.table
	for _, f := range r.fields {
		dst = append(dst, format.Field{Name: t.fields[f.Index], Value: f.Value})
	}
	if r.vec != nil && plant != "store/snapshot-no-vectors" {
		dst = append(dst, format.Field{Name: t.fields[t.vec], Value: value.Vector(r.vec)})
	}
	if plant != "store/snapshot-fields-unsorted" {
		slices.SortFunc(dst, byName)
	}
	return dst
}

// clip returns s with its capacity cut at its length, so appending to it
// never writes into an array it shares, or nil when s is empty, as the
// codec decodes an empty list.
func clip[E any](s []E) []E {
	if len(s) == 0 {
		return nil
	}
	return s[:len(s):len(s)]
}
