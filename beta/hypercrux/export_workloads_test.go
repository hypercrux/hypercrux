// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// What the crash-point driver's workloads through the public package
// (workloads_test.go, I1) need from inside it.

// StateOf is r's state as text: its snapshot, a change a line, and after
// each record's put, its vector as Get gives it and its links both ways as
// Neighbours gives them. Two copies that hold the same live data give the
// same text, however their files hold it, and the text holds every table
// with its vector size and its fields in order, every record with every
// value's bits, and every link. The vector and the links come from reads of
// their own as well as from the snapshot, so a snapshot that leaves
// something out, which a compaction would leave out of its file too, still
// gives another text.
func StateOf(r store.Reader) string {
	var b strings.Builder
	for c := range r.Snapshot() {
		b.WriteString(c.String())
		b.WriteByte('\n')
		if c.Op != format.Put {
			continue
		}
		rec, err := r.Get(c.Key)
		fmt.Fprintf(&b, "  vector %v, %v\n", value.Vector(rec.Vec), err)
		links, err := r.Neighbours(c.Key, store.Both, "")
		fmt.Fprintf(&b, "  links %v, %v\n", links, err)
	}
	return b.String()
}

// TxState is the state the transaction tx sees, with its own changes: what
// its Update leaves once it commits.
func TxState(tx *Tx) string { return StateOf(tx.stx) }

// ReadState is the state of db's copy, read as every read through db reads
// it: it follows the file first, and reloads it when another file has taken
// the path.
func ReadState(db *DB) (string, error) {
	var state string
	err := db.read(func(r store.Reader) error { state = StateOf(r); return nil })
	return state, err
}
