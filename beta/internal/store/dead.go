// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import "github.com/hypercrux/hypercrux/beta/internal/value"

// Dead bytes (C1). The store counts what the changes it takes make dead: the
// bytes of changes in the file that no longer reach the state, which a
// compaction would leave out. The log weighs the count when it says whether a
// compaction is due (logfile's Due), as BETA.md's "Compaction" has it: once
// the batches since the last compaction have made half the file dead, and a
// megabyte.
//
// What counts, each in the bytes FORMAT.md gives the change in a commit:
//
//   - a put of a record that's there already: the frame of one of its two
//     puts, its kind, key and count of fields, 5 bytes and the key's, since
//     a compaction writes one put a record;
//   - each value a put sets in place of one the record held, and each
//     vector, which a put writes over or takes away: the old value, with its
//     field's name;
//   - each null a put gives, which clears a field and is never kept;
//   - a delete: the delete itself, and the record, with every link to it
//     and from it;
//   - a drop: the drop itself, the table's create, and every record of the
//     table, with every link to them and from them, each link once;
//   - a removed link: the remove change, and the link it takes away.
//
// Creating a table, a put that makes a record, a new link and a vector block
// make nothing dead. Nor does anything a batch holds around its changes: its
// head, its checksum and its marker, and a vector block's head. So a database
// that only grows counts no dead bytes, however small its commits, and is
// compacted only at the limit on commits.
//
// Each size is the one a commit writes, whatever the change's own form:
// a table's create as a put into a new table writes it, with no field list,
// and a vector as a put writes it, with the field's name, its kind and its
// count of values. A compacted part writes both in a form of its own, the
// create with the table's whole field list, and the vector in a vector block,
// where its record's key takes the place of the field's name. Measuring
// everything one way makes the count a function of the state and the
// changes alone: a store that holds a record its own commits made, and one
// that loaded it from a compacted part, count the same when it goes. Every
// process applies the same changes to the same state (S3), so every process
// counts the same, the writer that compacted and one that loaded the file it
// wrote among them.
//
// So the count is exact for everything commits write, apart from the field
// list of a create that names its fields, as an import's does, which a
// dropped table's count leaves out. It's within a few bytes of the file for
// what a compacted part writes: a dropped table leaves out its create's field
// list, 2 bytes and each name's, and a vector a block held counts 7 bytes and
// the field's name for the block's 2 and the key's. For 384 values, 1,536
// bytes, and keys of 10 to 20 bytes, that's under 1 per cent.
//
// The count lives in the store, in dead, and only grows, apart from what a
// rollback takes back. A transaction notes it at Begin and at each Mark, and
// Rollback and RollbackTo put it back with the copy, so the count never holds
// a change that no batch holds (tx.go). The log keeps where a file's count
// starts: what Dead said when the log began to read the file from its start,
// or when it compacted into it, so a compaction starts the count from nothing.

// Dead returns how many bytes of changes the changes this store has taken
// have made dead, in all, by the measure above: the batches it has applied
// and loaded, and the commits made through its transactions, less what
// rollbacks took back. A new store holds 0, and loading a compacted part adds
// nothing, since it holds only live data.
//
// Dead is for the goroutine that has the store to itself, or a read inside
// Read, or a caller that keeps every write out, as the log's write lock and
// its mutex keep them out for the public package's copy.
func (s *Store) Dead() int64 { return s.dead }

// frameSize is what a put takes besides its fields: its kind, its key and
// the count of its fields.
func frameSize(key string) int64 { return 1 + 2 + int64(len(key)) + 2 }

// fieldSize is what the field called name takes in a put, with the value v:
// the name, then the value's kind and what the kind holds.
func fieldSize(name string, v value.Value) int64 {
	n := 2 + int64(len(name)) + 1
	switch v.Kind() {
	case value.KindInt, value.KindReal:
		n += 8
	case value.KindText, value.KindBytes, value.KindVector:
		n += 4 + int64(len(v.Raw()))
	}
	return n
}

// vectorSize is what a vector of t's takes in a put: the vector field's name,
// as t spells it, the kind, the count of values and 4 bytes a value. t has a
// vector size.
func vectorSize(t *table) int64 {
	return 2 + int64(len(t.fields[t.vec])) + 1 + 4 + 4*int64(t.size)
}

// size is what the record takes in the file by the measure above: a put's
// frame, each field that holds a value, and its vector, if it has one.
func (r *record) size() int64 {
	t := r.table
	n := frameSize(r.key)
	for _, f := range r.fields {
		n += fieldSize(t.fields[f.Index], f.Value)
	}
	if r.slot >= 0 {
		n += vectorSize(t)
	}
	return n
}

// deleteSize is what a delete of the record with this key takes: its kind
// and the key.
func deleteSize(key string) int64 { return 1 + 2 + int64(len(key)) }

// dropSize is what a drop of the table called name takes, its kind and the
// name, and createSize what its create takes, as a put into a new table
// writes it: its kind, the name, the vector size and a count of no fields.
func dropSize(name string) int64   { return 1 + 2 + int64(len(name)) }
func createSize(name string) int64 { return 1 + 2 + int64(len(name)) + 4 + 2 }

// linkSize is what a link of the type typ from the record with the key from
// to the one with the key to takes, as an add or a remove change: its kind
// and the three strings.
func linkSize(from, typ, to string) int64 {
	return 1 + 2 + int64(len(from)) + 2 + int64(len(typ)) + 2 + int64(len(to))
}

// linksSize is what r's links take, for a delete of r, when gone is nil, or a
// drop of r's table, gone: every link from r, and every link to r from a
// record that stays, so a drop counts a link between two of its records once,
// as one's link out, and a link from r to itself counts once, for a delete
// too.
func linksSize(r *record, gone *table) int64 {
	var n int64
	for h := range r.out.ofType("") {
		n += linkSize(r.key, h.typ.Value(), h.other.key)
	}
	for h := range r.in.ofType("") {
		if h.other != r && (h.other.table != gone || plant == "store/dead-inner-links-twice") {
			n += linkSize(h.other.key, h.typ.Value(), r.key)
		}
	}
	return n
}
