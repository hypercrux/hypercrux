// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/store"
)

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
const MaxDepth = store.MaxDepth

// Link connects from to to with a link of the given type, such as "owns" or
// "cites". Both records must exist. Adding a link that's already there
// does nothing.
func (db *DB) Link(from, typ, to string) error {
	return db.Update(func(tx *Tx) error { return tx.Link(from, typ, to) })
}

// Link is DB.Link inside the transaction.
func (t *Tx) Link(from, typ, to string) error {
	stx, err := t.open()
	if err != nil {
		return err
	}
	return stx.Link(from, typ, to)
}

// Unlink removes the link of the given type from from to to, or every link
// from from to to when typ is "". It returns ErrNotFound if there was none.
func (db *DB) Unlink(from, typ, to string) error {
	return db.Update(func(tx *Tx) error { return tx.Unlink(from, typ, to) })
}

// Unlink is DB.Unlink inside the transaction.
func (t *Tx) Unlink(from, typ, to string) error {
	stx, err := t.open()
	if err != nil {
		return err
	}
	return stx.Unlink(from, typ, to)
}

// Neighbours returns the links of the record with this key in the direction
// dir, of the type typ, or of every type when typ is "". They're sorted by
// type and then by the key at the other end, and with Both by type, then
// the key each link is from, then the key it's to. It returns ErrNotFound if
// there's no such record.
func (db *DB) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	if plant == "hypercrux/neighbours-unlocked" {
		s, err := db.current()
		if err != nil {
			return nil, err
		}
		return neighbours(s, key, dir, typ)
	}
	var links []Link
	err := db.read(func(r store.Reader) error {
		var err error
		links, err = neighbours(r, key, dir, typ)
		return err
	})
	return links, err
}

// Neighbours is DB.Neighbours inside the transaction.
func (t *Tx) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	stx, err := t.open()
	if err != nil {
		return nil, err
	}
	return neighbours(stx, key, dir, typ)
}

// neighbours reads a record's links from r, as 0.x's Neighbours gives them,
// and nil when there are none. The checks, the errors and the order are the
// store's, which are 0.x's, and the links come in a list of their own.
func neighbours(r store.Reader, key string, dir Direction, typ string) ([]Link, error) {
	found, err := r.Neighbours(key, store.Direction(dir), typ)
	if err != nil || found == nil {
		return nil, err
	}
	links := make([]Link, len(found))
	for i, l := range found {
		links[i] = Link(l)
	}
	return links, nil
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
	var steps []Step
	err := db.read(func(r store.Reader) error {
		var err error
		steps, err = walk(r, key, dir, typ, depth)
		return err
	})
	return steps, err
}

// Walk is DB.Walk inside the transaction.
func (t *Tx) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	stx, err := t.open()
	if err != nil {
		return nil, err
	}
	return walk(stx, key, dir, typ, depth)
}

// walk follows links from a record in r, as 0.x's Walk does, and gives nil
// when it reaches nothing. The checks, the errors and the order are the
// store's, which are 0.x's, and the steps come in a list of their own.
func walk(r store.Reader, key string, dir Direction, typ string, depth int) ([]Step, error) {
	found, err := r.Walk(key, store.Direction(dir), typ, depth)
	if err != nil || found == nil {
		return nil, err
	}
	steps := make([]Step, len(found))
	for i, s := range found {
		steps[i] = Step(s)
	}
	return steps, nil
}
