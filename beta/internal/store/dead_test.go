// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The count of dead bytes (C1, dead.go): the sizes it measures by, what each
// change makes dead, rollbacks, and a random run in which the count has to be
// the changes' bytes less the live state's, the same in every store that
// holds the same changes, however it took them.

// encoded is how many bytes c takes in a batch, as format.AppendBatch writes
// it: the batch's length less its head and its checksum.
func encoded(t *testing.T, c format.Change) int64 {
	t.Helper()
	b, _, err := format.AppendBatch(nil, 1, 1, []format.Change{c})
	if err != nil {
		t.Fatalf("AppendBatch refused %v: %v", c, err)
	}
	return int64(len(b) - format.BatchHeadSize - 4)
}

// TestDeadSizesAreTheCodecs: each size the count measures by is the bytes
// format.AppendBatch writes for the change it stands for, for every kind of
// value, and a key, a name and a link type of every length the rules allow at
// their ends.
func TestDeadSizesAreTheCodecs(t *testing.T) {
	long := func(n int) string { return string(slices.Repeat([]byte("k"), n)) }
	keys := []string{"a:1", "docs:12345", "t:" + long(1022)}
	names := []string{"n", "Vec", long(64)}
	values := []value.Value{value.Null(), value.Int(-7), value.Real(0.5), value.Text(""), value.Text("héllo"), value.Bytes("\x00\x01"), value.Vector([]float32{1, 2, 3})}
	for _, key := range keys {
		if got, want := frameSize(key), encoded(t, format.Change{Op: format.Put, Key: key}); got != want {
			t.Errorf("frameSize(%.12q) is %d, where a put with no fields takes %d", key, got, want)
		}
		if got, want := deleteSize(key), encoded(t, format.Change{Op: format.Delete, Key: key}); got != want {
			t.Errorf("deleteSize(%.12q) is %d, where a delete takes %d", key, got, want)
		}
		for _, name := range names {
			for _, v := range values {
				if (v.Kind() == value.KindVector) != rules.IsVec(name) && !v.IsNull() {
					continue // the vector field holds a vector or null, and no other field a vector
				}
				put := format.Change{Op: format.Put, Key: key, Fields: []format.Field{{Name: name, Value: v}}}
				if got, want := frameSize(key)+fieldSize(name, v), encoded(t, put); got != want {
					t.Errorf("a put of %.12q with %s %v measures %d bytes, where it takes %d", key, name, v, got, want)
				}
			}
		}
		for _, typ := range []string{"x", long(200)} {
			for _, op := range []format.Op{format.Link, format.Unlink} {
				if got, want := linkSize(key, typ, "b:2"), encoded(t, format.Change{Op: op, Key: key, Type: typ, To: "b:2"}); got != want {
					t.Errorf("linkSize(%.12q, %.12q) is %d, where the %v takes %d", key, typ, got, want, op)
				}
			}
		}
	}
	for _, name := range []string{"a", "docs", long(63)} {
		if got, want := dropSize(name), encoded(t, format.Change{Op: format.Drop, Table: name}); got != want {
			t.Errorf("dropSize(%.12q) is %d, where a drop takes %d", name, got, want)
		}
		if got, want := createSize(name), encoded(t, format.Change{Op: format.CreateTable, Table: name}); got != want {
			t.Errorf("createSize(%.12q) is %d, where a create with no fields takes %d", name, got, want)
		}
	}
	// A vector, as a put writes it, with the field's name as the table
	// spells it.
	s := New()
	if _, err := s.Put(nil, "v:1", []format.Field{{Name: "Vec", Value: value.Vector([]float32{1, 2, 3})}}); err != nil {
		t.Fatal(err)
	}
	if got, want := vectorSize(s.tables["v"]), fieldSize("Vec", value.Vector([]float32{1, 2, 3})); got != want {
		t.Errorf("vectorSize is %d, where the put's vector field measures %d", got, want)
	}
	r := s.records["v:1"]
	if got, want := r.size(), encoded(t, format.Change{Op: format.Put, Key: "v:1", Fields: []format.Field{{Name: "Vec", Value: value.Vector([]float32{1, 2, 3})}}}); got != want {
		t.Errorf("the record measures %d bytes, where its put takes %d", got, want)
	}
}

// deadStep is one write in TestWhatEachChangeMakesDead, through a writer
// (tx_cases_test.go), and what it makes dead.
type deadStep struct {
	what string
	do   func(w writer) error
	dead int64
}

// deadSteps are the writes TestWhatEachChangeMakesDead makes, in order, each
// with what it makes dead, worked out by hand from the sizes the codec gives
// (TestDeadSizesAreTheCodecs).
func deadSteps() []deadStep {
	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	put := func(key string, fields ...format.Field) func(w writer) error {
		return func(w writer) error { return w.Put(key, fields) }
	}
	vec := func(x float32) value.Value { return value.Vector([]float32{x, 1, 2}) }
	vecSize := fieldSize("vec", vec(0))
	return []deadStep{
		{"a put that makes a table and a record", put("docs:1", f("n", value.Int(1)), f("s", value.Text("abc"))), 0},
		{"a put over one of the record's values", put("docs:1", f("s", value.Text("abcd"))),
			frameSize("docs:1") + fieldSize("s", value.Text("abc"))},
		{"a put of a null over a value", put("docs:1", f("n", value.Null())),
			frameSize("docs:1") + fieldSize("n", value.Null()) + fieldSize("n", value.Int(1))},
		{"a put that makes a record with a null", put("docs:2", f("n", value.Null()), f("s", value.Text("x"))), fieldSize("n", value.Null())},
		{"a put of a record with no fields", put("docs:2"), frameSize("docs:2")},
		{"a put of a new field into a record", put("docs:2", f("z", value.Real(2))), frameSize("docs:2")},
		{"a put that gives a record its first vector", put("v:1", f("vec", vec(1))), 0},
		{"a put of a vector over the record's", put("v:1", f("vec", vec(2))), frameSize("v:1") + vecSize},
		{"a put of a null over the vector", put("v:1", f("vec", value.Null())), frameSize("v:1") + fieldSize("vec", value.Null()) + vecSize},
		{"a put of a null where there's no vector", put("v:1", f("vec", value.Null())), frameSize("v:1") + fieldSize("vec", value.Null())},
		{"links, and a record with a vector", func(w writer) error {
			for _, step := range []func() error{
				func() error { return w.Put("v:2", []format.Field{f("vec", vec(3)), f("t", value.Text("two"))}) },
				func() error { return w.Link("docs:1", "a", "v:2") },
				func() error { return w.Link("v:2", "b", "docs:1") },
				func() error { return w.Link("v:2", "self", "v:2") },
				func() error { return w.Link("docs:2", "a", "v:2") },
				func() error { return w.Link("docs:1", "a", "v:2") }, // there already, so nothing changes
			} {
				if err := step(); err != nil {
					return err
				}
			}
			return nil
		}, 0},
		{"an unlink", func(w writer) error { return w.Unlink("docs:2", "a", "v:2") }, 2 * linkSize("docs:2", "a", "v:2")},
		{"a delete of a record with links both ways and to itself", func(w writer) error { return w.Delete("v:2") },
			deleteSize("v:2") + frameSize("v:2") + fieldSize("t", value.Text("two")) + vecSize +
				linkSize("docs:1", "a", "v:2") + linkSize("v:2", "b", "docs:1") + linkSize("v:2", "self", "v:2")},
		{"a table with links inside it and out of it", func(w writer) error {
			for _, step := range []func() error{
				func() error { return w.Put("tmp:1", []format.Field{f("n", value.Int(1))}) },
				func() error { return w.Put("tmp:2", nil) },
				func() error { return w.Link("tmp:1", "in", "tmp:2") },
				func() error { return w.Link("tmp:2", "in", "tmp:1") },
				func() error { return w.Link("tmp:1", "self", "tmp:1") },
				func() error { return w.Link("tmp:1", "out", "docs:1") },
				func() error { return w.Link("docs:1", "into", "tmp:2") },
			} {
				if err := step(); err != nil {
					return err
				}
			}
			return nil
		}, 0},
		{"a drop of that table", func(w writer) error { return w.Drop("tmp") },
			dropSize("tmp") + createSize("tmp") + frameSize("tmp:1") + fieldSize("n", value.Int(1)) + frameSize("tmp:2") +
				linkSize("tmp:1", "in", "tmp:2") + linkSize("tmp:2", "in", "tmp:1") + linkSize("tmp:1", "self", "tmp:1") +
				linkSize("tmp:1", "out", "docs:1") + linkSize("docs:1", "into", "tmp:2")},
		{"a create with its fields, as an import makes one", func(w writer) error {
			return w.Apply(format.Change{Op: format.CreateTable, Table: "imp", Size: 3, Names: []string{"a", "vec"}})
		}, 0},
		{"a drop of that table, whose create counts with no fields", func(w writer) error { return w.Drop("imp") },
			dropSize("imp") + createSize("imp")},
		{"a delete of a record without links", func(w writer) error { return w.Delete("docs:2") },
			deleteSize("docs:2") + frameSize("docs:2") + fieldSize("s", value.Text("x")) + fieldSize("z", value.Real(2))},
	}
}

// TestWhatEachChangeMakesDead: each kind of write makes dead what dead.go
// says, worked out by hand, straight on the store and through a transaction
// alike, and the commit's change list applied to another store, or loaded
// into one, makes the same dead.
func TestWhatEachChangeMakesDead(t *testing.T) {
	s, tx := New(), New()
	follower, loader := New(), New()
	for i, step := range deadSteps() {
		before := s.Dead()
		if err := step.do(directWriter{s}); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if got := s.Dead() - before; got != step.dead {
			t.Errorf("%s straight on the store makes %d bytes dead, where %d are", step.what, got, step.dead)
		}
		before = tx.Dead()
		x, err := tx.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := step.do(x); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		list := slices.Clone(x.changes)
		if err := x.Commit(func([]format.Change) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if got := tx.Dead() - before; got != step.dead {
			t.Errorf("%s through a transaction makes %d bytes dead, where %d are", step.what, got, step.dead)
		}
		if len(list) == 0 {
			continue // nothing for the file
		}
		batch := writable(t, 1, uint64(i+1), list)
		before = follower.Dead()
		if err := follower.ApplyBatch(uint64(i+1), batch); err != nil {
			t.Fatal(err)
		}
		if got := follower.Dead() - before; got != step.dead {
			t.Errorf("%s, applied as a batch, makes %d bytes dead, where %d are", step.what, got, step.dead)
		}
		before = loader.Dead()
		if err := loader.LoadBatch(uint64(i+1), batch); err != nil {
			t.Fatal(err)
		}
		if got := loader.Dead() - before; got != step.dead {
			t.Errorf("%s, loaded as a batch, makes %d bytes dead, where %d are", step.what, got, step.dead)
		}
	}
}

// TestARollbackTakesTheCountBack: a transaction rolled back, a commit whose
// write fails, and a statement taken back to its mark leave the count where
// it was, and a commit keeps it.
func TestARollbackTakesTheCountBack(t *testing.T) {
	s := New()
	f := []format.Field{{Name: "s", Value: value.Text("one")}}
	if _, err := s.Put(nil, "docs:1", f); err != nil {
		t.Fatal(err)
	}
	over := func(tx *Tx) {
		t.Helper()
		if err := tx.Put("docs:1", f); err != nil {
			t.Fatal(err)
		}
		if err := tx.Put("docs:2", nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Delete("docs:2"); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := s.Begin()
	over(tx)
	if s.Dead() == 0 {
		t.Fatal("writing over a record and deleting another made nothing dead")
	}
	tx.Rollback()
	if s.Dead() != 0 {
		t.Errorf("after a rollback the count is %d", s.Dead())
	}
	tx, _ = s.Begin()
	over(tx)
	if err := tx.Commit(func([]format.Change) error { return fmt.Errorf("the disk is full") }); err == nil {
		t.Fatal("a commit whose write failed worked")
	}
	if s.Dead() != 0 {
		t.Errorf("after a commit whose write failed the count is %d", s.Dead())
	}
	tx, _ = s.Begin()
	over(tx)
	kept := s.Dead()
	m := tx.Mark()
	over(tx)
	tx.RollbackTo(m)
	if s.Dead() != kept {
		t.Errorf("a statement taken back to its mark leaves the count at %d, where it was %d at the mark", s.Dead(), kept)
	}
	if err := tx.Commit(func([]format.Change) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s.Dead() != kept {
		t.Errorf("after the commit the count is %d, where the transaction left %d", s.Dead(), kept)
	}
}

// canonical is what changes take by the count's measure, worked out from
// the codec's own bytes for each change: as format.AppendBatch writes it,
// apart from a create, which counts as it does with no fields, and a vector
// block, each of whose vectors counts as a put of the vector field writes
// it. vecs holds each table's vector field as its create names it, for the
// blocks after it, and canonical adds to it.
func canonical(t *testing.T, changes []format.Change, vecs map[string]string) int64 {
	t.Helper()
	var n int64
	for _, c := range changes {
		switch c.Op {
		case format.CreateTable:
			for _, name := range c.Names {
				if rules.IsVec(name) {
					vecs[c.Table] = name
				}
			}
			n += encoded(t, format.Change{Op: format.CreateTable, Table: c.Table})
		case format.Vectors:
			for _, v := range c.Vecs {
				n += 2 + int64(len(vecs[c.Table])) + 1 + 4 + int64(len(v.Bits))
			}
		default:
			n += encoded(t, c)
		}
	}
	return n
}

// liveOf is what s's state takes by the count's measure: its snapshot, which
// gives each table, each record with its vector, and each link once.
func liveOf(t *testing.T, s *Store) int64 {
	t.Helper()
	return canonical(t, snapshotOf(s), map[string]string{})
}

// TestTheCountIsTheChangesLessTheLive: through random transactions on store
// A, S3's, with writes that fail, statements taken back, commits whose write
// fails and rollbacks, A's count is always the bytes of the changes it has
// committed less the bytes of its live state, by the count's measure
// (canonical). So the count is exact by that measure, and a function of the
// changes and the state alone. Each commit's batch goes to B through
// ApplyBatch, now and then after a damaged copy that B refuses whole, and to
// C through LoadBatch, and both count what A counts. Every 25 transactions A's
// compacted part, joined into vector blocks, is loaded into a new store D,
// which counts nothing dead for it, and from then on takes the batches too,
// counting what A has counted since, and its own count is its changes less
// its live state as well.
func TestTheCountIsTheChangesLessTheLive(t *testing.T) {
	seeds, txs := 8, 300
	if testing.Short() {
		seeds, txs = 3, 150
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runDeadCounts(t, seed, txs) })
	}
}

func runDeadCounts(t *testing.T, seed uint64, n int) {
	a, b, c := New(), New(), New()
	committed := newModel()
	g := &gen{r: rand.New(rand.NewPCG(seed, 0xde)), m: committed, side: rand.New(rand.NewPCG(seed, 0xad))}
	seen := &states{seen: map[string]bool{}}
	outcomes := map[string]int{}
	vecs := map[string]string{}
	var changes int64 // what A's committed changes take
	var d *Store
	var dSince, dChanges int64 // A's count when D took the compacted part, and what D's changes take
	dVecs := map[string]string{}
	dSeq := uint64(0)
	seq := uint64(0)
	for i := range n {
		var list []format.Change
		committed, list = changeListTx(t, g, a, committed, seen, outcomes)
		if list != nil {
			seq++
			batch := writable(t, 1, seq, list)
			follow(t, g, b, seq, batch, outcomes)
			if err := c.LoadBatch(seq, batch); err != nil {
				t.Fatalf("C couldn't load batch %d: %v", seq, err)
			}
			changes += canonical(t, batch, vecs)
			if d != nil {
				dSeq++
				if err := d.ApplyBatch(dSeq, writable(t, 2, dSeq, list)); err != nil {
					t.Fatalf("D couldn't apply batch %d: %v", dSeq, err)
				}
				dChanges += canonical(t, batch, dVecs)
			}
		}
		if b.Dead() != a.Dead() || c.Dead() != a.Dead() {
			t.Fatalf("after transaction %d, A counts %d bytes dead, B %d and C %d", i+1, a.Dead(), b.Dead(), c.Dead())
		}
		if live := liveOf(t, a); a.Dead() != changes-live {
			t.Fatalf("after transaction %d, A counts %d bytes dead, where its changes take %d and its live state %d, so %d are dead",
				i+1, a.Dead(), changes, live, changes-live)
		}
		if d != nil {
			if d.Dead() != a.Dead()-dSince {
				t.Fatalf("after transaction %d, D counts %d bytes dead since it loaded A's compacted part, where A counts %d", i+1, d.Dead(), a.Dead()-dSince)
			}
			if live := liveOf(t, d); d.Dead() != dChanges-live {
				t.Fatalf("after transaction %d, D counts %d bytes dead, where its changes take %d and its live state %d", i+1, d.Dead(), dChanges, live)
			}
		}
		if i%25 == 24 {
			d, dSince, dSeq, dVecs = New(), a.Dead(), 0, map[string]string{}
			side := g.aside()
			part := compactedOf(a)
			dChanges = canonical(t, part, dVecs)
			for _, batch := range joined(part, func() int { return 1 + side.r.IntN(5) }, func() int { return 1 + side.r.IntN(8) }) {
				dSeq++
				if err := d.LoadBatch(dSeq, writable(t, 2, dSeq, batch)); err != nil {
					t.Fatalf("D couldn't load batch %d of A's compacted part: %v", dSeq, err)
				}
			}
			if d.Dead() != 0 {
				t.Fatalf("a store that loaded A's compacted part counts %d bytes dead", d.Dead())
			}
			if live := liveOf(t, d); dChanges != live {
				t.Fatalf("A's compacted part takes %d bytes by the count's measure, where its live state takes %d", dChanges, live)
			}
		}
	}
	for _, o := range []string{"commit", "commit failed", "rollback", "statement rolled back", "damage found", "delete ok", "drop ok", "unlink ok"} {
		if outcomes[o] == 0 {
			t.Errorf("%q never came up in %d transactions: %v", o, n, outcomes)
		}
	}
	if a.Dead() == 0 {
		t.Error("the transactions made nothing dead")
	}
}
