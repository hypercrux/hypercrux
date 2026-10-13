// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"fmt"
	"strconv"
	"strings"

	// Named val because fixtures_test.go, in this package, has a type
	// called value.
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// Op says which of FORMAT.md's changes a Change is.
type Op uint8

// The changes. The letter after each is its kind byte in the file, which
// only the codec deals in. Vectors, a vector block, came with V2, and only a
// compacted part holds one.
const (
	CreateTable Op = iota + 1 // T
	Put                       // P
	Delete                    // D
	Link                      // L
	Unlink                    // U
	Drop                      // X
	Vectors                   // V
)

func (o Op) String() string {
	switch o {
	case CreateTable:
		return "create table"
	case Put:
		return "put"
	case Delete:
		return "delete"
	case Link:
		return "link"
	case Unlink:
		return "unlink"
	case Drop:
		return "drop table"
	case Vectors:
		return "vector block"
	}
	return "Op(" + strconv.Itoa(int(o)) + ")"
}

// Change is one of FORMAT.md's changes, holding what the file holds for
// it. A change list, []Change, is what a transaction makes as it runs (S3),
// what a batch holds once the codec has read it (F1), and what a store
// applies, all of it or none (S3). The store's snapshot gives a compacted
// part as a change list too, in the order FORMAT.md gives for one.
//
// Each change uses the fields its Op names below and leaves the rest
// empty. With one struct for all of them, building a change list costs no
// allocation per change beyond a put's fields, and applying one is a
// switch on Op.
//
// A change in a change list owns its slices: nothing changes them once
// it's there. The changes of a snapshot are the exception, as
// store.Reader.Snapshot says, and so are a vector block's bits, as Vector
// says.
//
// What a writer emits keeps FORMAT.md's rules. A put into a table that
// doesn't exist yet comes after a CreateTable with no fields and size 0.
// Removing every link between two records, whatever their types, is one
// Unlink for each type, in byte order of type. A change that would do
// nothing isn't emitted, apart from a put.
type Change struct {
	Op Op

	// CreateTable, Drop and Vectors: the table's name.
	Table string
	// CreateTable: the vector size, 0 for none yet, and the table's
	// fields, in the table's order. Vectors: the vector size, the number
	// of values in each of the block's vectors.
	Size  int
	Names []string

	// Put and Delete: the record's key. Link and Unlink: the key the link
	// is from.
	Key string
	// Put: the fields it sets, in byte order of name, each spelt as the
	// table spells it. A null clears a field, and the vector field holds a
	// vector or null.
	Fields []Field
	// Link and Unlink: the link's type, and the key it's to.
	Type string
	To   string
	// Vectors: the records the block gives a vector, in byte order of key,
	// each with its vector.
	Vecs []Vector
}

// Field is one field of a put: its name and its value.
type Field struct {
	Name  string
	Value val.Value
}

// Vector is one record's vector in a vector block: the record's key, and
// the vector's values as their bits, 4 bytes a value, little-endian, as
// FORMAT.md stores them and value.Value's Raw gives a vector's.
//
// Bits can share memory that isn't the change's own. The store's snapshot
// for compaction hands out each slot of its vector arrays as it is, and
// DecodeBatchInPlace hands out the bytes it was given. Either way the bytes
// stay as they are only for as long as their owner says, so a caller that
// keeps a vector past that copies it.
type Vector struct {
	Key  string
	Bits string
}

// String shows the change on one line, for a test's output.
func (c Change) String() string {
	switch c.Op {
	case CreateTable:
		return fmt.Sprintf("create table %s, vector size %d, fields %q", c.Table, c.Size, c.Names)
	case Put:
		parts := make([]string, len(c.Fields))
		for i, f := range c.Fields {
			parts[i] = f.Name + ": " + f.Value.String()
		}
		return "put " + c.Key + " {" + strings.Join(parts, ", ") + "}"
	case Delete:
		return "delete " + c.Key
	case Link, Unlink:
		return fmt.Sprintf("%s %s -%s-> %s", c.Op, c.Key, c.Type, c.To)
	case Drop:
		return "drop table " + c.Table
	case Vectors:
		parts := make([]string, len(c.Vecs))
		for i, v := range c.Vecs {
			parts[i] = v.Key + ": " + val.VectorBits(v.Bits[:len(v.Bits)&^3]).String()
		}
		return fmt.Sprintf("vector block %s, vector size %d {%s}", c.Table, c.Size, strings.Join(parts, ", "))
	}
	return c.Op.String()
}
