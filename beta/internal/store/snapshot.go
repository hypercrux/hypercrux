// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
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
// as the table spells it, in byte order of name. Every link follows the
// tables, as a Link, in byte order of the key it's from, then its type,
// then the key it's to.
//
// The changes are built one at a time as they're ranged over, so a caller
// ranges over them inside the Read that handed it the store, or inside the
// transaction, while nothing changes the store. Ranging again starts again
// from the first table. A CreateTable's Names are the table's own list,
// which nothing may change. A Put's Fields slice is used again for the
// next Put, so a caller that keeps a Put clones its Fields.
//
// Each table's records come from its keys, which are in byte order already
// (S4), and each record's links out are in order of type and then of the
// key they're to (S5), so the snapshot sorts only the tables' names.
func (s *Store) Snapshot() iter.Seq[format.Change] { return s.snapshot }

// Compacted returns the whole copy as the changes of a compacted part as
// FORMAT.md's "Compaction" gives them since V2, which is what compaction
// writes: Snapshot's changes, with each table's vectors out of its Puts and
// in vector blocks after them. Each table comes as a CreateTable, then a Put
// for each of its records, carrying every field that holds a value apart
// from the vector, then a Vectors change for each of its records that holds
// a vector, in byte order of key. The links come last, as in Snapshot.
//
// Each Vectors change holds one record's vector, and the log's writer joins
// a table's into vector blocks that fill its batches, so the snapshot holds
// nothing of the database beyond the record it's on. A vector's Bits are
// its slot's own values, handed out without a copy on a little-endian
// processor (bitsOf), and compaction writes them from there into its batch,
// with no copy into a value.Vector. So, like the rest of a snapshot, the
// changes are good only while nothing changes the store: a caller ranges
// over them inside the Read that handed it the store. The Vecs slice is used
// again for the next vector, as a Put's Fields slice is for the next Put.
func (s *Store) Compacted() iter.Seq[format.Change] { return s.compacted }

func (s *Store) snapshot(yield func(format.Change) bool) { s.part(yield, false) }

func (s *Store) compacted(yield func(format.Change) bool) { s.part(yield, true) }

// part yields the copy as a compacted part's changes: the vectors in the
// Puts, or, with blocks set, in Vectors changes after each table's Puts.
func (s *Store) part(yield func(format.Change) bool, blocks bool) {
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	var fields []format.Field // the Put's, used again for each record
	var one [1]format.Vector  // a Vectors change's, used again for each vector
	for _, name := range names {
		t := s.tables[name]
		create := format.Change{Op: format.CreateTable, Table: t.name, Size: t.size, Names: clip(t.fields)}
		if plant == "store/snapshot-size-left-out" {
			create.Size = 0
		}
		if !yield(create) {
			return
		}
		for r := range s.inOrder(t) {
			fields = r.snapshot(fields[:0], !blocks)
			if !yield(format.Change{Op: format.Put, Key: r.key, Fields: clip(fields)}) {
				return
			}
		}
		if blocks && !s.vectorsOf(t, one[:], yield) {
			return
		}
	}
	// The links, in byte order of the key they're from. Every key of a
	// table starts with the table's name and a colon, so all the keys of
	// one table come before all the keys of another when its name with a
	// colon after it does: users2:zed before users:ann, since a digit sorts
	// before a colon. So the tables go in that order, and each table's
	// records in the order of their keys.
	if plant != "store/snapshot-links-by-table" {
		slices.SortFunc(names, compareAsKeys)
	}
	for _, name := range names {
		for r := range s.inOrder(s.tables[name]) {
			for h := range r.out.ofType("") {
				if !yield(format.Change{Op: format.Link, Key: r.key, Type: h.typ.Value(), To: h.other.key}) {
					return
				}
			}
		}
	}
}

// compareAsKeys compares two tables' names as their records' keys compare:
// each name with a colon after it, in byte order. No name holds a colon.
func compareAsKeys(a, b string) int {
	n := min(len(a), len(b))
	if c := strings.Compare(a[:n], b[:n]); c != 0 {
		return c
	}
	switch {
	case len(a) < len(b):
		return cmp.Compare(byte(':'), b[n])
	case len(a) > len(b):
		return cmp.Compare(a[n], byte(':'))
	}
	return 0
}

// inOrder ranges over a table's records in byte order of key.
func (s *Store) inOrder(t *table) iter.Seq[*record] {
	if plant == "store/snapshot-in-map-order" {
		return func(yield func(*record) bool) {
			for _, r := range s.records {
				if r.table == t && !yield(r) {
					return
				}
			}
		}
	}
	return t.keys.records
}

// vectorsOf yields a Vectors change for each of t's records that holds a
// vector, in byte order of key, each with one vector in one, its Bits the
// slot's values as they are (bitsOf). It returns false when the caller
// stops. A table's slots can come in another order than its keys, since a
// put takes the slot a delete freed last, so the vectors are found by their
// records, a slot at a time.
func (s *Store) vectorsOf(t *table, one []format.Vector, yield func(format.Change) bool) bool {
	if t.size == 0 {
		return true
	}
	left := -1 // the vectors to give, for store/compacted-last-vector-dropped alone
	if plant == "store/compacted-last-vector-dropped" {
		left = len(t.vecs.owners) - len(t.vecs.free)
	}
	slot := 0 // the next slot in use, for store/compacted-in-slot-order alone
	for r := range s.inOrder(t) {
		if r.slot < 0 {
			continue
		}
		if left--; left == 0 {
			return true
		}
		values := t.vector(r.slot)
		if plant == "store/compacted-in-slot-order" {
			for t.vecs.owners[slot] == nil {
				slot++
			}
			values, slot = t.vector(slot), slot+1
		}
		one[0] = format.Vector{Key: r.key, Bits: bitsOf(values)}
		if !yield(format.Change{Op: format.Vectors, Table: t.name, Size: t.size, Vecs: one}) {
			return false
		}
	}
	return true
}

// snapshot appends the record's fields to dst as its Put in a snapshot
// carries them: every field that holds a value, each spelt as its table
// spells it, in byte order of name, with the vector among them when vec is
// set. The record holds its fields in its table's order, which can differ
// from byte order, since a later put's new fields join the end of the list.
// The vector is a copy of its slot's values, so the snapshot holds nothing
// that later changes to the store change.
func (r *record) snapshot(dst []format.Field, vec bool) []format.Field {
	t := r.table
	for _, f := range r.fields {
		dst = append(dst, format.Field{Name: t.fields[f.Index], Value: f.Value})
	}
	if vec && r.slot >= 0 && plant != "store/snapshot-no-vectors" {
		dst = append(dst, format.Field{Name: t.fields[t.vec], Value: value.Vector(t.vector(r.slot))})
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
