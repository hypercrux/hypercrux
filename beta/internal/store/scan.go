// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

// Scan (S4). A scan reads one table's keys in byte order, from the first
// that starts with the prefix and comes after after, until the first that
// doesn't start with the prefix. A Cursor gives one record at a time, so a
// caller that has enough stops pulling, and the public package applies
// 0.x's limit that way.

// Scan returns a Cursor over the records whose keys start with prefix and
// come after after, in byte order of key, as 0.x's Scan gives them. The
// prefix starts with a table's name and a colon, such as "docs:" or
// "docs:2026-". after can be anything: "" starts at the table's first key,
// and a key from before the prefix starts there too. A table that doesn't
// exist gives no records.
//
// A prefix without a colon, or with a table name that breaks 0.x's rules,
// gives an error that wraps errs.ErrInvalid, with 0.x's message, in 0.x's
// order. 0.x's Scan checks its limit after those, so the public package
// calls Scan first and checks the limit after it.
//
// The Cursor is good until the read that called Scan ends. The records it
// gives share the store's memory, as Get's do, and Next allocates nothing.
func (s *Store) Scan(prefix, after string) (Cursor, error) { return s.scan(nil, prefix, after) }

func (s *Store) scan(tx *Tx, prefix, after string) (Cursor, error) {
	name, err := scanTable(prefix)
	if err != nil {
		return nil, err
	}
	c := &cursor{s: s, tx: tx, table: name, prefix: prefix, from: prefix}
	if after >= prefix {
		// A key after after comes after the prefix too, so the scan starts
		// after after. Otherwise every key with the prefix comes after
		// after, and the scan starts at the prefix.
		c.from, c.past = after, plant != "store/after-included"
	}
	c.seek()
	return c, nil
}

// scanTable checks a scan's prefix, and returns its table: the part before
// the first colon. The checks and their messages are 0.x's, in 0.x's order.
// The rest of the prefix can be anything, as a key's id can.
func scanTable(prefix string) (string, error) {
	i := strings.IndexByte(prefix, ':')
	if i < 0 {
		return "", fmt.Errorf("%w: scan prefix %q needs a table, such as docs:", errs.ErrInvalid, prefix)
	}
	if err := rules.Table(prefix[:i]); err != nil && plant != "store/prefix-table-unchecked" {
		return "", err
	}
	return prefix[:i], nil
}

// cursor is the store's Cursor. It keeps its place in its table's keys,
// with the store's epoch when it found it. A change to the store's tables
// or to any table's keys moves the epoch on, and then the cursor finds its
// place again from the last key it gave, in the table as it is now. So a
// cursor through a transaction carries on through the transaction's
// changes: it gives the next key after the last one it gave, among the keys
// the table holds at that moment, and never gives a key twice.
type cursor struct {
	s      *Store
	tx     *Tx    // the transaction the scan came through, or nil
	table  string // the prefix's table
	prefix string
	// The next record is the first whose key comes after from when past is
	// true, or the first at or after it when past is false.
	from string
	past bool
	// t is the table the cursor's place is in, or nil when there's none.
	// bi and i are the place: the block, and the key in it. epoch is the
	// store's epoch when the cursor found it.
	t     *table
	bi, i int
	epoch uint64
	done  bool // Next has given false, and gives it from now on
}

// seek finds the cursor's place.
func (c *cursor) seek() {
	c.epoch = c.s.epoch
	c.t = c.s.tables[c.table]
	if c.t != nil {
		c.bi, c.i = c.t.keys.find(c.from, c.past)
	}
}

// Next returns the next record, or false when there are no more. Once it
// has given false, it always does. Through a transaction, it gives false
// once the transaction has ended, as database/sql's Rows do once they're
// closed.
func (c *cursor) Next() (Record, bool) {
	if c.done || c.tx != nil && c.tx.done {
		c.done = true
		return Record{}, false
	}
	if c.epoch != c.s.epoch && plant != "store/cursor-keeps-its-place" {
		c.seek()
	}
	if c.t == nil || c.bi >= len(c.t.keys.blocks) {
		c.done = true
		return Record{}, false
	}
	e := c.t.keys.blocks[c.bi][c.i]
	if !strings.HasPrefix(e.key, c.prefix) {
		c.done = true
		return Record{}, false
	}
	c.from, c.past = e.key, true
	if c.i++; c.i == len(c.t.keys.blocks[c.bi]) {
		c.bi, c.i = c.bi+1, 0
	}
	return e.r.read(), true
}
