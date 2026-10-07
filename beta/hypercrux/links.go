// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import "fmt"

// Link is a typed, one-way connection between two records.
type Link struct {
	From string
	Type string
	To   string
}

// String shows the link as from -type-> to.
func (l Link) String() string { return l.From + " -" + l.Type + "-> " + l.To }

// Direction picks which links to follow from a record.
type Direction int

const (
	Out  Direction = iota // links from the record to others
	In                    // links from others to the record
	Both                  // either way
)

// String returns the direction's name, as ParseDirection reads it, or
// Direction(n) for a number that isn't a direction.
func (d Direction) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	case Both:
		return "both"
	}
	return fmt.Sprintf("Direction(%d)", int(d))
}

// ParseDirection reads "out", "in" or "both", and reads "" as Out.
func ParseDirection(s string) (Direction, error) {
	switch s {
	case "out", "":
		return Out, nil
	case "in":
		return In, nil
	case "both":
		return Both, nil
	}
	return 0, fmt.Errorf("%w: direction %q: use out, in or both", ErrInvalid, s)
}

// MaxDepth is the furthest Walk goes.
const MaxDepth = 32

// Link connects from to to with a link of the given type, such as "owns" or
// "cites". Both records must exist. Adding a link that's already there
// does nothing.
func (db *DB) Link(from, typ, to string) error { return notYet("DB.Link", "G2") }

// Link is DB.Link inside the transaction.
func (t *Tx) Link(from, typ, to string) error { return notYet("Tx.Link", "G2") }

// Unlink removes the link of the given type from from to to, or every link
// from from to to when typ is "". It returns ErrNotFound if there was none.
func (db *DB) Unlink(from, typ, to string) error { return notYet("DB.Unlink", "G2") }

// Unlink is DB.Unlink inside the transaction.
func (t *Tx) Unlink(from, typ, to string) error { return notYet("Tx.Unlink", "G2") }

// Neighbours returns the links of the record with this key in the direction
// dir, of the type typ, or of every type when typ is "". They're sorted by
// type and then by the key at the other end, and with Both by type, then
// the key each link is from, then the key it's to. It returns ErrNotFound if
// there's no such record.
func (db *DB) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	return nil, notYet("DB.Neighbours", "G2")
}

// Neighbours is DB.Neighbours inside the transaction.
func (t *Tx) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	return nil, notYet("Tx.Neighbours", "G2")
}

// Step is one record Walk reached, and how many links it took to get there.
type Step struct {
	Key   string
	Depth int
}

// Walk follows links from the record with this key, up to depth links away,
// and returns every record it reaches with the fewest links needed, nearest
// first and then by key. The record itself isn't included. typ "" follows
// links of every type. depth runs from 1 to MaxDepth. It returns
// ErrNotFound if there's no such record.
func (db *DB) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	return nil, notYet("DB.Walk", "G2")
}

// Walk is DB.Walk inside the transaction.
func (t *Tx) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	return nil, notYet("Tx.Walk", "G2")
}
