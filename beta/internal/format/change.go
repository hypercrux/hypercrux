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

// Op says which of FORMAT.md's six changes a Change is.
type Op uint8

// The six changes. The letter after each is its kind byte in the file,
// which only the codec deals in.
const (
	CreateTable Op = iota + 1 // T
	Put                       // P
	Delete                    // D
	Link                      // L
	Unlink                    // U
	Drop                      // X
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
	}
	return "Op(" + strconv.Itoa(int(o)) + ")"
}

// Change is one of FORMAT.md's six changes, holding what the file holds
// for it. A change list, []Change, is what a transaction makes as it runs
// (S3), what a batch holds once the codec has read it (F1), and what a
// store applies, all of it or none (S3). The store's snapshot gives a
// compacted part as a change list too, in the order FORMAT.md gives for
// one.
//
// Each change uses the fields its Op names below and leaves the rest
// empty. With one struct for all six, building a change list costs no
// allocation per change beyond a put's fields, and applying one is a
// switch on Op.
//
// A change in a change list owns its slices: nothing changes them once
// it's there. The changes of a snapshot are the exception, as
// store.Reader.Snapshot says.
//
// What a writer emits keeps FORMAT.md's rules. A put into a table that
// doesn't exist yet comes after a CreateTable with no fields and size 0.
// Removing every link between two records, whatever their types, is one
// Unlink for each type, in byte order of type. A change that would do
// nothing isn't emitted, apart from a put.
type Change struct {
	Op Op

	// CreateTable and Drop: the table's name.
	Table string
	// CreateTable: the vector size, 0 for none yet, and the table's
	// fields, in the table's order.
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
}

// Field is one field of a put: its name and its value.
type Field struct {
	Name  string
	Value val.Value
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
	}
	return c.Op.String()
}
