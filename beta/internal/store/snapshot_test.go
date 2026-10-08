// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// snapshot returns s's snapshot, taken inside Read, with each change's
// slices copied out, since the snapshot uses a Put's Fields again.
func snapshot(t *testing.T, s *store.Store) []format.Change {
	t.Helper()
	var out []format.Change
	ok(t, s.Read(func(r store.Reader) error {
		for c := range r.Snapshot() {
			c.Names, c.Fields = slices.Clone(c.Names), slices.Clone(c.Fields)
			out = append(out, c)
		}
		return nil
	}))
	return out
}

// TestSnapshot pins a snapshot written out by hand, of a store whose
// tables, keys and fields each come in an order other than byte order.
// Table users has the fields Zed and title from its first put, then VEC,
// alpha, phone, raw and small, so a record's fields by place differ from
// byte order of name. Its keys sort as bytes do, users:10 before users:9.
// phone has only ever held null, so it's in the field list and in no put.
// pics keeps the vector size its deleted record set. empty has fields and
// no records. tmp was dropped, so it's not there.
func TestSnapshot(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "users:ann", fields{"title": "Ann", "Zed": 1}))
	ok(t, putGo(s, "users:10", fields{"alpha": "a\x00b", "VEC": []float32{1, float32(math.Copysign(0, -1))}, "title": nil}))
	ok(t, putGo(s, "users:9", nil))
	ok(t, putGo(s, "users:Bob", fields{"zed": math.Copysign(0, -1), "phone": nil}))
	ok(t, putGo(s, "users2:zed", fields{"name": "Zed"}))
	ok(t, putGo(s, "pics:1", fields{"vec": []float32{1, 2, 3}}))
	must[[]format.Change](t)(s.Delete(nil, "pics:1"))
	ok(t, s.Apply(format.Change{Op: format.CreateTable, Table: "empty", Names: []string{"b", "a"}}))
	ok(t, putGo(s, "tmp:1", fields{"n": 1}))
	must[[]format.Change](t)(s.Drop(nil, "tmp"))
	ok(t, putGo(s, "users:ann", fields{"raw": []byte{0, 0xff}, "small": math.SmallestNonzeroFloat64}))

	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	want := []format.Change{
		{Op: format.CreateTable, Table: "empty", Names: []string{"b", "a"}},
		{Op: format.CreateTable, Table: "pics", Size: 3, Names: []string{"vec"}},
		{Op: format.CreateTable, Table: "users", Size: 2, Names: []string{"Zed", "title", "VEC", "alpha", "phone", "raw", "small"}},
		{Op: format.Put, Key: "users:10", Fields: []format.Field{
			f("VEC", value.Vector([]float32{1, float32(math.Copysign(0, -1))})), f("alpha", value.Text("a\x00b")),
		}},
		{Op: format.Put, Key: "users:9"},
		{Op: format.Put, Key: "users:Bob", Fields: []format.Field{f("Zed", value.Real(math.Copysign(0, -1)))}},
		{Op: format.Put, Key: "users:ann", Fields: []format.Field{
			f("Zed", value.Int(1)), f("raw", value.Bytes("\x00\xff")), f("small", value.Real(math.SmallestNonzeroFloat64)), f("title", value.Text("Ann")),
		}},
		{Op: format.CreateTable, Table: "users2", Names: []string{"name"}},
		{Op: format.Put, Key: "users2:zed", Fields: []format.Field{f("name", value.Text("Zed"))}},
	}
	got := snapshot(t, s)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the snapshot is\n%v\nwhere it should be\n%v", got, want)
	}

	// Written as one batch and read again, it loads into an empty store
	// that gives the same snapshot and the same records.
	b, _, err := format.AppendBatch(nil, 2, 1, got)
	ok(t, err)
	bt := must[format.Batch](t)(format.DecodeBatch(b, 2, 1))
	c := store.New()
	ok(t, c.LoadBatch(bt.Seq, bt.Changes))
	if again := snapshot(t, c); !reflect.DeepEqual(again, want) {
		t.Fatalf("the store loaded from the snapshot gives\n%v", again)
	}
	for _, key := range []string{"users:ann", "users:10", "users:9", "users:Bob", "users2:zed"} {
		if a, b := must[fields](t)(getGo(s, key)), must[fields](t)(getGo(c, key)); !reflect.DeepEqual(a, b) {
			t.Errorf("%s is %v, and loaded from the snapshot %v", key, a, b)
		}
	}
}

// TestSnapshotOfTheFixtures loads file-new.hex, P2's small database, and
// takes its snapshot. P2 wrote file-compacted.hex by hand from FORMAT.md, as
// that database compacted, and the first batch of its compacted part holds
// every table with its records. The snapshot, written as that batch, must
// match it byte for byte. The second batch holds the links, which the store
// leaves out until S5 keeps them, as it leaves out the links of
// file-new.hex. Loading the compacted part, then the commit after it, gives
// back what the store holds with that commit applied.
func TestSnapshotOfTheFixtures(t *testing.T) {
	s := store.New()
	for _, bt := range fixtureBatches(t, "file-new.hex", false) {
		ok(t, s.LoadBatch(bt.Seq, withoutLinks(bt.Changes)))
	}
	snap := snapshot(t, s)
	comp := fixtureBatches(t, "file-compacted.hex", true)
	got, sum, err := format.AppendBatch(nil, 2, 1, snap)
	ok(t, err)
	file := readHex(t, "file-compacted.hex")
	first := file[format.HeaderSize : format.HeaderSize+comp[0].Length]
	if !bytes.Equal(got, first) || sum != comp[0].Sum {
		t.Fatalf("the snapshot, written as a batch, is\n%x\nwhere the compacted part's first batch is\n%x\nits changes: %v", got, first, snap)
	}
	if len(comp) != 2 || len(withoutLinks(comp[1].Changes)) != 0 {
		t.Fatalf("the compacted part holds %d batches, and the store expects the second to hold the links alone", len(comp))
	}

	c := store.New()
	for _, bt := range comp {
		if changes := withoutLinks(bt.Changes); len(changes) > 0 {
			ok(t, c.LoadBatch(bt.Seq, changes))
		}
	}
	if again := snapshot(t, c); !slices.EqualFunc(again, snap, equalChanges) {
		t.Fatalf("the compacted part loads as\n%v\nwhere file-new.hex loads as\n%v", again, snap)
	}
	after := fixtureBatches(t, "file-compacted.hex", false)[len(comp):]
	for _, bt := range after {
		ok(t, c.ApplyBatch(bt.Seq, bt.Changes))
		ok(t, s.ApplyBatch(bt.Seq, bt.Changes))
	}
	if len(after) != 1 || !slices.EqualFunc(snapshot(t, c), snapshot(t, s), equalChanges) {
		t.Fatalf("after the %d commits past the compacted part, the two stores differ", len(after))
	}
}

func equalChanges(a, b format.Change) bool {
	return a.Op == b.Op && a.Table == b.Table && a.Size == b.Size && slices.Equal(a.Names, b.Names) && a.Key == b.Key &&
		slices.Equal(a.Fields, b.Fields) && a.Type == b.Type && a.To == b.To
}

// withoutLinks returns the changes that aren't links, which the store
// applies only from S5 on.
func withoutLinks(changes []format.Change) []format.Change {
	return slices.DeleteFunc(slices.Clone(changes), func(c format.Change) bool { return c.Op == format.Link || c.Op == format.Unlink })
}

// readHex reads one of P2's fixtures, annotated hex in which everything
// from a # to the end of a line is a comment.
func readHex(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "format", "testdata", name))
	ok(t, err)
	var b []byte
	for _, line := range strings.Split(string(text), "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, pair := range strings.Fields(line) {
			x, err := hex.DecodeString(pair)
			ok(t, err)
			b = append(b, x...)
		}
	}
	return b
}

// fixtureBatches reads a fixture's marked batches with the codec, from the
// first, and stops at the end of the compacted part when compacted is
// true, or else at the end of the file.
func fixtureBatches(t *testing.T, name string, compacted bool) []format.Batch {
	t.Helper()
	b := readHex(t, name)
	h := must[format.Header](t)(format.DecodeHeader(b, int64(len(b))))
	end := len(b)
	if compacted {
		end = int(h.CompactedEnd)
	}
	var out []format.Batch
	for off := format.HeaderSize; off < end; {
		bt := must[format.Batch](t)(format.DecodeBatch(b[off:], h.Gen, uint64(len(out)+1)))
		m, whole := format.DecodeMarker(b[off+bt.Length:], h.ID, h.Gen)
		if !whole || m != (format.Marker{Seq: bt.Seq, Sum: bt.Sum}) {
			t.Fatalf("%s: batch %d has no marker", name, bt.Seq)
		}
		out = append(out, bt)
		off += bt.Length + format.MarkerSize
	}
	return out
}

// TestSnapshotStops: a caller that stops ranging stops the snapshot, as
// compaction does when a write fails, and ranging again starts again from
// the first table. Go panics if an iterator carries on after its caller
// stops.
func TestSnapshotStops(t *testing.T) {
	s := store.New()
	for _, key := range []string{"docs:1", "docs:2", "notes:1"} {
		ok(t, putGo(s, key, fields{"n": 1}))
	}
	all := snapshot(t, s)
	if len(all) != 5 {
		t.Fatalf("the snapshot is %v", all)
	}
	for stop := 1; stop <= len(all); stop++ {
		var got []format.Change
		for c := range s.Snapshot() {
			c.Fields = slices.Clone(c.Fields)
			if got = append(got, c); len(got) == stop {
				break
			}
		}
		if !slices.EqualFunc(got, all[:stop], equalChanges) {
			t.Fatalf("stopping after %d changes gave %v", stop, got)
		}
	}
	var none []format.Change
	for c := range store.New().Snapshot() {
		none = append(none, c)
	}
	if none != nil {
		t.Fatalf("an empty store's snapshot is %v", none)
	}
}

// TestSnapshotSharesNothingThatChanges: a CreateTable's Names are the
// table's own list, handed out with their capacity cut at their length, as
// Table's Fields are, so appending to them never writes into the store,
// even once the table has grown in place since.
func TestSnapshotSharesNothingThatChanges(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"a": 1, "b": 2, "c": 3}))
	var names []string
	for c := range s.Snapshot() {
		if c.Op == format.CreateTable {
			names = c.Names
		}
	}
	ok(t, putGo(s, "docs:1", fields{"d": 4}))
	_ = append(names, "x")
	if tb, _ := s.Table("docs"); !slices.Equal(tb.Fields, []string{"a", "b", "c", "d"}) || !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("appending to a snapshot's field list changed the table to %q, or the list to %q", tb.Fields, names)
	}
}

// TestApplyRefusesUnusedFields: a change that sets a field of format.Change
// its Op doesn't use can't be written, so Apply refuses it as AppendBatch
// does, and changes nothing. Each change below, as it is, is one that both
// take.
func TestApplyRefusesUnusedFields(t *testing.T) {
	set := map[string]func(c *format.Change){
		"Table":  func(c *format.Change) { c.Table = "docs" },
		"Size":   func(c *format.Change) { c.Size = 2 },
		"Names":  func(c *format.Change) { c.Names = []string{"title"} },
		"Key":    func(c *format.Change) { c.Key = "docs:1" },
		"Fields": func(c *format.Change) { c.Fields = []format.Field{{Name: "title", Value: value.Text("a")}} },
		"Type":   func(c *format.Change) { c.Type = "cites" },
		"To":     func(c *format.Change) { c.To = "docs:2" },
	}
	start := func() *store.Store {
		s := store.New()
		ok(t, putGo(s, "docs:1", fields{"title": "a"}))
		ok(t, putGo(s, "docs:2", fields{"title": "b"}))
		return s
	}
	for _, c := range []struct {
		change format.Change
		uses   []string
	}{
		{format.Change{Op: format.CreateTable, Table: "notes", Size: 1, Names: []string{"vec"}}, []string{"Table", "Size", "Names"}},
		{format.Change{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "title", Value: value.Text("c")}}}, []string{"Key", "Fields"}},
		{format.Change{Op: format.Delete, Key: "docs:1"}, []string{"Key"}},
		{format.Change{Op: format.Link, Key: "docs:1", Type: "cites", To: "docs:2"}, []string{"Key", "Type", "To"}},
		{format.Change{Op: format.Unlink, Key: "docs:1", Type: "cites", To: "docs:2"}, []string{"Key", "Type", "To"}},
		{format.Change{Op: format.Drop, Table: "docs"}, []string{"Table"}},
	} {
		if _, _, err := format.AppendBatch(nil, 1, 1, []format.Change{c.change}); err != nil {
			t.Fatalf("AppendBatch refused %v: %v", c.change, err)
		}
		err := start().Apply(c.change)
		if c.change.Op == format.Link || c.change.Op == format.Unlink {
			wantErr(t, err, errors.ErrUnsupported) // until S5
		} else {
			ok(t, err)
		}
		for name, setIt := range set {
			if slices.Contains(c.uses, name) {
				continue
			}
			bad := c.change
			setIt(&bad)
			s := start()
			before := snapshot(t, s)
			_, _, codecErr := format.AppendBatch(nil, 1, 1, []format.Change{bad})
			err := s.Apply(bad)
			if !errors.Is(codecErr, errs.ErrInvalid) || !errors.Is(err, errs.ErrInvalid) || !strings.HasSuffix(err.Error(), " sets "+name+", which it doesn't use") {
				t.Errorf("%+v: the codec gives %v, and Apply %v", bad, codecErr, err)
			}
			if !slices.EqualFunc(snapshot(t, s), before, equalChanges) {
				t.Errorf("Apply(%+v) changed the store", bad)
			}
		}
	}
	err := store.New().Apply(format.Change{Op: format.Unlink, Key: "docs:1", Type: "x", To: "docs:2", Table: "docs", Size: 1})
	if !errors.Is(err, errs.ErrInvalid) || err.Error() != "hypercrux: invalid: an unlink change that sets Table and Size, which it doesn't use" {
		t.Errorf("an unlink with two fields it doesn't use gave %v", err)
	}
}

// TestAppliedChangesKeepTheirSlices: a transaction's Apply puts copies of a
// change's slices in its change list, so a caller can use its slices again.
// The snapshot does, so applying one store's snapshot through a transaction
// on another hands on the snapshot as it was.
func TestAppliedChangesKeepTheirSlices(t *testing.T) {
	a := store.New()
	ok(t, putGo(a, "docs:1", fields{"title": "a", "n": 1}))
	ok(t, putGo(a, "docs:2", fields{"title": "b", "vec": []float32{1, 2}}))
	ok(t, putGo(a, "notes:1", fields{"body": "c"}))
	want := snapshot(t, a)
	b := store.New()
	tx := must[*store.Tx](t)(b.Begin())
	defer tx.Rollback()
	ok(t, a.Read(func(r store.Reader) error {
		for c := range r.Snapshot() {
			if err := tx.Apply(c); err != nil {
				return err
			}
		}
		return nil
	}))
	var handed []format.Change
	ok(t, tx.Commit(func(c []format.Change) error { handed = c; return nil }))
	if !slices.EqualFunc(handed, want, equalChanges) {
		t.Fatalf("the commit handed on\n%v\nwhere the snapshot applied was\n%v", handed, want)
	}
	if got := snapshot(t, b); !slices.EqualFunc(got, want, equalChanges) {
		t.Fatalf("the store the snapshot was applied to gives\n%v", got)
	}
}
