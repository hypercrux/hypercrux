// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestTheUndoListsRoomIsKept: a transaction hands its undo list's room to the
// next one (F7), committed or rolled back, with every entry cleared, so the
// room keeps no record, table, field or vector alive. A list that has grown
// past maxSpare entries is let go instead. The next transaction takes the
// room, so two never share it, and a batch the log hands over takes it too.
func TestTheUndoListsRoomIsKept(t *testing.T) {
	s := New()
	puts := func(tx *Tx, first, n int) {
		t.Helper()
		for i := first; i < first+n; i++ {
			err := tx.Put(fmt.Sprintf("docs:%d", i), []format.Field{
				{Name: "n", Value: value.Int(int64(i))},
				{Name: "title", Value: value.Text("a title")},
				{Name: "vec", Value: value.Vector([]float32{1, float32(i)})},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	cleared := func(what string) {
		t.Helper()
		for i, u := range s.spare[:cap(s.spare)] {
			if u.op != 0 || u.name != "" || u.table != nil || u.record != nil || u.fields != nil || u.vec != nil || u.half.other != nil {
				t.Fatalf("%s: the spare room's entry %d holds %+v", what, i, u)
			}
		}
	}

	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	puts(tx, 0, 100)
	n := len(tx.undo)
	if err := tx.Commit(nil); err != nil {
		t.Fatal(err)
	}
	if len(s.spare) != 0 || cap(s.spare) < n {
		t.Fatalf("after a commit of %d undo entries the store keeps %d of %d room", n, len(s.spare), cap(s.spare))
	}
	cleared("after a commit")
	room := cap(s.spare)

	tx, err = s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if cap(tx.undo) != room || s.spare != nil {
		t.Fatalf("the next transaction took %d of the %d room, and the store kept %d", cap(tx.undo), room, cap(s.spare))
	}
	puts(tx, 100, 10)
	tx.Rollback()
	if cap(s.spare) != room {
		t.Fatalf("after a rollback the store keeps %d room, where it had %d", cap(s.spare), room)
	}
	cleared("after a rollback")
	if _, err := s.Get("docs:100"); err == nil {
		t.Fatal("the rollback left docs:100")
	}

	if err := s.ApplyBatch(1, []format.Change{{Op: format.Put, Key: "docs:0", Fields: []format.Field{{Name: "n", Value: value.Int(-1)}}}}); err != nil {
		t.Fatal(err)
	}
	cleared("after a batch from the log")

	tx, err = s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	puts(tx, 1000, maxSpare/3+1)
	if len(tx.undo) <= maxSpare {
		t.Fatalf("%d puts made %d undo entries, too few for the test", maxSpare/3+1, len(tx.undo))
	}
	if err := tx.Commit(nil); err != nil {
		t.Fatal(err)
	}
	if s.spare != nil {
		t.Errorf("after a transaction of more than maxSpare entries, the store keeps %d room", cap(s.spare))
	}
	if r, err := s.Get("docs:0"); err != nil || r.Fields[0].Value != value.Int(-1) {
		t.Errorf("docs:0 is %+v, %v", r, err)
	}
}
