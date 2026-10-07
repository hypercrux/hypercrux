// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"errors"
	"iter"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// stubReader holds one empty table and nothing else, so the read API
// compiles against something until S1 writes the store. It's in another
// package, as Q4's fake store will be. If Reader changes, it stops
// compiling, which is the reminder that every implementation and caller
// changes with it.
type stubReader struct{}

var errStub = errors.New("store: a stub")

var _ store.Reader = stubReader{}

func (stubReader) Table(name string) (store.Table, bool) {
	return store.Table{Name: name, Vec: -1}, name == "docs"
}

func (stubReader) Get(string) (store.Record, error)          { return store.Record{}, errStub }
func (stubReader) Scan(string, string) (store.Cursor, error) { return stubCursor{}, nil }
func (stubReader) Neighbours(string, store.Direction, string) ([]store.Link, error) {
	return nil, errStub
}
func (stubReader) Walk(string, store.Direction, string, int) ([]store.Step, error) {
	return nil, errStub
}
func (stubReader) Nearest(string, []float32, int, store.Filter) ([]store.Hit, error) {
	return nil, errStub
}

func (stubReader) Snapshot() iter.Seq[format.Change] {
	return func(yield func(format.Change) bool) {
		if !yield(format.Change{Op: format.CreateTable, Table: "docs"}) {
			return
		}
		yield(format.Change{Op: format.Put, Key: "docs:1"})
	}
}

type stubCursor struct{}

func (stubCursor) Next() (store.Record, bool) { return store.Record{}, false }

func TestTheReadAPICompiles(t *testing.T) {
	var r store.Reader = stubReader{}
	if tb, ok := r.Table("docs"); !ok || tb.Name != "docs" || tb.Vec != -1 {
		t.Errorf("Table(docs) = %+v, %v", tb, ok)
	}
	if _, ok := r.Table("nosuch"); ok {
		t.Error("Table(nosuch) found a table")
	}
	_, err := r.Get("docs:1")
	errs := []error{err}
	_, err = r.Neighbours("docs:1", store.Both, "")
	errs = append(errs, err)
	_, err = r.Walk("docs:1", store.Out, "cites", 2)
	errs = append(errs, err)
	keep := func(rec store.Record) (bool, error) { return rec.Field(0).Kind() == value.KindText, nil }
	_, err = r.Nearest("docs", []float32{1, 0}, 10, keep)
	errs = append(errs, err)
	for i, err := range errs {
		if !errors.Is(err, errStub) {
			t.Errorf("call %d returned %v", i+1, err)
		}
	}

	c, err := r.Scan("docs:", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := c.Next(); ok {
		t.Errorf("an empty scan gave %+v", rec)
	}

	// A consumer stops a snapshot by breaking out, as compaction does when
	// a write fails.
	var got []format.Change
	for ch := range r.Snapshot() {
		got = append(got, ch)
		break
	}
	if len(got) != 1 || got[0].Op != format.CreateTable {
		t.Errorf("the snapshot gave %v", got)
	}
}

func TestRecordField(t *testing.T) {
	rec := store.Record{
		Key: "docs:1",
		Fields: []store.FieldValue{
			{Index: 0, Value: value.Text("Q3 plan")},
			{Index: 2, Value: value.Int(12)},
		},
		Vec: []float32{0.5, -0.25},
	}
	for i, want := range []value.Value{value.Text("Q3 plan"), value.Null(), value.Int(12), value.Null()} {
		if got := rec.Field(i); got != want {
			t.Errorf("Field(%d) = %v, want %v", i, got, want)
		}
	}
}

// TestTheTypesMatch0x checks the strings that 0.x's Link and Direction
// give, so the public package can hand these types out as they are.
func TestTheTypesMatch0x(t *testing.T) {
	if got := (store.Link{From: "customer:42", Type: "owns", To: "docs:7"}).String(); got != "customer:42 -owns-> docs:7" {
		t.Errorf("Link.String() = %q", got)
	}
	for d, want := range map[store.Direction]string{store.Out: "out", store.In: "in", store.Both: "both", 3: "Direction(3)"} {
		if got := d.String(); got != want {
			t.Errorf("Direction(%d).String() = %q, want %q", int(d), got, want)
		}
	}
	if store.Out != 0 || store.In != 1 || store.Both != 2 {
		t.Error("the directions don't have 0.x's numbers")
	}
	_ = store.Step{Key: "docs:2", Depth: 1}
	_ = store.Hit{Key: "docs:2", Distance: 0.5}
}
