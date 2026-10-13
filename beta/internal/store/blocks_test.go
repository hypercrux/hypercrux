// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// The vector blocks' tests (V2). The compacted snapshot gives each table's
// vectors after its puts, in byte order of key, straight from the slots, and
// a vector block puts them back bit for bit, with the norms a put works out.
// A block that breaks the store's rules changes nothing.

// awkward holds float32s whose bits have to come back as they are: -0, the
// smallest and the largest subnormal, the smallest normal value, the largest
// and the smallest finite values, and 1.
var awkward = []float32{
	float32(math.Copysign(0, -1)), math.Float32frombits(1), math.Float32frombits(0x007fffff),
	math.Float32frombits(0x00800000), math.MaxFloat32, -math.MaxFloat32, 1,
}

// blockStore returns a store whose compacted snapshot has each thing a
// vector block has to carry: several tables with vectors, of sizes 1, 3 and
// 65,536, with the awkward values; records with no vector beside ones with
// one; a table whose slots were freed and taken again, so its slots come in
// another order than its keys; a table with a vector size and no vectors
// left; a table with no vector size; and links.
func blockStore(t *testing.T) *Store {
	t.Helper()
	s := New()
	put := func(key string, fields ...format.Field) {
		t.Helper()
		if _, err := s.Put(nil, key, fields); err != nil {
			t.Fatal(err)
		}
	}
	del := func(key string) {
		t.Helper()
		if _, err := s.Delete(nil, key); err != nil {
			t.Fatal(err)
		}
	}
	title := func(x string) format.Field { return format.Field{Name: "title", Value: value.Text(x)} }

	// docs: slots 0 to 3 go to docs:1 to docs:4. Then docs:9 takes slot 1,
	// which docs:2 freed, and docs:0 takes slot 2, which docs:3 freed when
	// its vector was set to null. So the slots hold docs:1, docs:9, docs:0
	// and docs:4, and the keys run docs:0, docs:1, docs:3, docs:4, docs:5,
	// docs:9, with no vector for docs:3 and docs:5.
	putVec(t, s, "docs:1", awkward[0:3])
	putVec(t, s, "docs:2", awkward[2:5])
	putVec(t, s, "docs:3", awkward[4:7])
	putVec(t, s, "docs:4", []float32{1, 1, 1})
	put("docs:5", title("no vector"))
	del("docs:2")
	putVec(t, s, "docs:9", []float32{awkward[0], awkward[0], 9})
	putVec(t, s, "docs:3", nil)
	putVec(t, s, "docs:0", []float32{awkward[5], awkward[1], awkward[3]})

	// ones: vectors of one value each.
	putVec(t, s, "ones:a", awkward[1:2])
	putVec(t, s, "ones:b", awkward[5:6])
	put("ones:c", title("no vector"))

	// wide: vectors of 65,536 values, the most a vector holds.
	putVec(t, s, "wide:1", filled(65536, -0.5))
	big := make([]float32, 65536)
	for i := range big {
		big[i] = awkward[i%len(awkward)]
	}
	putVec(t, s, "wide:2", big)

	// bare: a vector size and no vectors left, its only slot free.
	putVec(t, s, "bare:1", []float32{1, 2})
	put("bare:2", title("no vector"))
	del("bare:1")

	// plain: no vector size at all.
	put("plain:1", title("a"))

	for _, l := range [][3]string{{"docs:1", "cites", "docs:9"}, {"ones:a", "in", "docs:0"}, {"plain:1", "shows", "wide:2"}} {
		if _, err := s.Link(nil, l[0], l[1], l[2]); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// compactedOf returns s's compacted snapshot, taken inside Read as
// compaction takes it, with each change's slices copied out, its vectors'
// bits among them, since those are the slots' own.
func compactedOf(s *Store) []format.Change {
	var out []format.Change
	s.Read(func(Reader) error {
		out = collectCompacted(s)
		return nil
	})
	return out
}

// collectCompacted is compactedOf for a caller that holds the store's lock
// already, inside Read.
func collectCompacted(s *Store) []format.Change {
	var out []format.Change
	for c := range s.Compacted() {
		c.Names, c.Fields, c.Vecs = slices.Clone(c.Names), slices.Clone(c.Fields), slices.Clone(c.Vecs)
		for i := range c.Vecs {
			c.Vecs[i].Bits = strings.Clone(c.Vecs[i].Bits)
		}
		out = append(out, c)
	}
	return out
}

// checkCompacted checks part, a store's compacted snapshot, against snap,
// the same store's snapshot, by FORMAT.md's order for a compacted part since
// V2: snap's changes in the same order, with each Put's vector out of its
// fields, and after each table's Puts a Vectors change for each of its
// records that holds a vector, in byte order of key, with the table's vector
// size and the vector's bits. checkSnapshot checks snap against the model.
func checkCompacted(t *testing.T, snap, part []format.Change) {
	t.Helper()
	var want []format.Change
	var vecs []format.Change // the Vectors changes of the table snap is on
	flush := func() { want, vecs = append(want, vecs...), nil }
	size := 0
	for _, c := range snap {
		switch c.Op {
		case format.CreateTable:
			flush()
			size = c.Size
		case format.Put:
			i := slices.IndexFunc(c.Fields, func(f format.Field) bool { return f.Value.Kind() == value.KindVector })
			if i >= 0 {
				vecs = append(vecs, format.Change{Op: format.Vectors, Table: tableOfKey(c.Key), Size: size,
					Vecs: []format.Vector{{Key: c.Key, Bits: c.Fields[i].Value.Raw()}}})
				c.Fields = slices.Delete(slices.Clone(c.Fields), i, i+1)
			}
		case format.Link:
			flush()
		}
		want = append(want, c)
	}
	flush()
	if !slices.EqualFunc(part, want, equalChange) {
		t.Fatalf("the compacted snapshot is\n%v\nwhere the snapshot gives\n%v", part, want)
	}
}

// joined cuts a compacted snapshot into batches as the log's compaction
// does, with a vector block of no more than n() vectors: each table's run of
// Vectors changes joined into blocks, each alone in its batch, and the
// changes between them cut into batches of no more than cut() changes.
func joined(part []format.Change, n, cut func() int) [][]format.Change {
	var out [][]format.Change
	var run []format.Change // changes waiting for a batch
	flush := func() {
		for len(run) > 0 {
			k := min(cut(), len(run))
			out = append(out, run[:k:k])
			run = run[k:]
		}
		run = nil
	}
	for i := 0; i < len(part); {
		c := part[i]
		if c.Op != format.Vectors {
			run = append(run, c)
			i++
			continue
		}
		flush()
		block := format.Change{Op: format.Vectors, Table: c.Table, Size: c.Size}
		for most := n(); i < len(part) && part[i].Op == format.Vectors && part[i].Table == c.Table && len(block.Vecs) < most; i++ {
			block.Vecs = append(block.Vecs, part[i].Vecs...)
		}
		out = append(out, []format.Change{block})
	}
	flush()
	return out
}

// inKeyOrder fails the test unless each of s's tables holds its vectors in
// slots in byte order of key, as a store that took them from vector blocks
// does.
func inKeyOrder(t *testing.T, s *Store) {
	t.Helper()
	for name, tb := range s.tables {
		for i, r := range tb.vecs.owners {
			if r == nil || i > 0 && r.key <= tb.vecs.owners[i-1].key {
				t.Fatalf("table %s holds its vectors in slots in another order than its keys: %v", name, slotKeys(tb))
			}
		}
	}
}

// slotKeys returns the key of each slot's record in t's array, or "" for a
// free slot.
func slotKeys(t *table) []string {
	keys := make([]string, len(t.vecs.owners))
	for i, r := range t.vecs.owners {
		if r != nil {
			keys[i] = r.key
		}
	}
	return keys
}

// TestVectorBlocks is V2's round trip in the store: a compacted snapshot,
// its vectors joined into vector blocks, written and read back by the codec,
// and loaded into an empty store, gives back every vector bit for bit, with
// the norm a put gives it, whether the blocks hold one vector or all of a
// table's, are decoded in place or copied, and go in through LoadBatch,
// ApplyBatch or Apply. The store that wrote the snapshot holds its slots in
// another order than its keys, which the snapshot mustn't follow, and the
// store that takes it holds its vectors in slots in key order.
func TestVectorBlocks(t *testing.T) {
	a := blockStore(t)
	if !slices.Equal(slotKeys(a.tables["docs"]), []string{"docs:1", "docs:9", "docs:0", "docs:4"}) {
		t.Fatalf("docs's slots hold %q, where the test means them to hold docs:1, docs:9, docs:0 and docs:4", slotKeys(a.tables["docs"]))
	}
	part := compactedOf(a)
	checkCompacted(t, snapshotOf(a), part)
	if n := len(slices.DeleteFunc(slices.Clone(part), func(c format.Change) bool { return c.Op != format.Vectors })); n != 8 {
		t.Fatalf("the compacted snapshot gives %d vectors, where the store holds 8: %v", n, part)
	}
	want := dump(a)
	for _, per := range []int{1, 2, 3, 1000} {
		for _, how := range []string{"LoadBatch", "ApplyBatch", "Apply"} {
			for _, inPlace := range []bool{false, true} {
				b := New()
				for i, batch := range joined(part, func() int { return per }, func() int { return 3 }) {
					seq := uint64(i + 1)
					decode := format.DecodeBatch
					if inPlace {
						decode = format.DecodeBatchInPlace
					}
					decoded := writableAs(t, decode, 2, seq, batch)
					switch how {
					case "LoadBatch":
						ok(t, b.LoadBatch(seq, decoded))
					case "ApplyBatch":
						ok(t, b.ApplyBatch(seq, decoded))
					case "Apply":
						for _, c := range decoded {
							ok(t, b.Apply(c))
						}
					}
				}
				if got := dump(b); got != want {
					t.Fatalf("blocks of %d vectors, through %s, in place %v, give the store\n%s\nwhere it should hold\n%s", per, how, inPlace, got, want)
				}
				if c := b.Check(); len(c.Problems) != 0 || c.Vectors != 8 {
					t.Fatalf("blocks of %d vectors, through %s, give a store whose check finds %d vectors and %q", per, how, c.Vectors, c.Problems)
				}
				inKeyOrder(t, b)
				if again := compactedOf(b); !slices.EqualFunc(again, part, equalChange) {
					t.Fatalf("the store the blocks gave has the compacted snapshot\n%v\nwhere it should be\n%v", again, part)
				}
			}
		}
	}
}

// TestTheCompactedSnapshotSharesTheSlots: on a little-endian processor, each
// vector a compacted snapshot gives is its slot's own memory, handed out
// without a copy into a value.Vector, so ranging over the snapshot costs the
// same few allocations however many vectors the store holds.
func TestTheCompactedSnapshotSharesTheSlots(t *testing.T) {
	if !littleEndian {
		t.Skip("the vectors' bits are worked out into strings of their own on a big-endian processor")
	}
	allocs := func(n int) float64 {
		s := New()
		for i := range n {
			putVec(t, s, "docs:"+strconv.Itoa(i), filled(384, float32(i)))
		}
		var vectors int
		got := testing.AllocsPerRun(5, func() {
			vectors = 0
			s.Read(func(Reader) error {
				for c := range s.Compacted() {
					if c.Op != format.Vectors {
						continue
					}
					vectors++
					r := s.records[c.Vecs[0].Key]
					if slot := r.table.vector(r.slot); unsafe.StringData(c.Vecs[0].Bits) != (*byte)(unsafe.Pointer(&slot[0])) || len(c.Vecs[0].Bits) != 4*len(slot) {
						t.Errorf("the compacted snapshot gives %s's vector as a copy of its slot", r.key)
					}
				}
				return nil
			})
		})
		if vectors != n {
			t.Fatalf("the compacted snapshot gives %d vectors, where the store holds %d", vectors, n)
		}
		return got
	}
	if few, many := allocs(10), allocs(2000); many != few {
		t.Fatalf("ranging over the compacted snapshot of 10 vectors takes %v allocations, and of 2,000 takes %v", few, many)
	}
}

// TestVectorBlockDamage applies vector blocks that break the store's rules
// to a store whose table pics has two free slots, through Apply, ApplyBatch
// and LoadBatch. Each must give its error, as damage from a batch, and
// leave the store as it was, its free list and slots included, even when
// the vector that breaks the rules comes after others the block put in
// slots already. Then a good block takes the free slots as puts take them,
// and gives the norms puts give.
func TestVectorBlockDamage(t *testing.T) {
	bits := func(v ...float32) string { return value.Vector(v).Raw() }
	block := func(table string, size int, keyBits ...string) format.Change {
		c := format.Change{Op: format.Vectors, Table: table, Size: size}
		for i := 0; i+1 < len(keyBits); i += 2 {
			c.Vecs = append(c.Vecs, format.Vector{Key: keyBits[i], Bits: keyBits[i+1]})
		}
		return c
	}
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	start := func() *Store {
		s := New()
		putVec(t, s, "pics:1", []float32{1, 2})
		putVec(t, s, "pics:8", []float32{3, 4})
		putVec(t, s, "pics:9", []float32{5, 6})
		for _, key := range []string{"pics:8", "pics:9"} {
			if _, err := s.Delete(nil, key); err != nil {
				t.Fatal(err)
			}
		}
		for _, key := range []string{"pics:2", "pics:3", "pics:4"} {
			putVec(t, s, key, nil)
		}
		putVec(t, s, "other:1", []float32{1, 2, 3})
		putVec(t, s, "other:2", nil)
		if _, err := s.Put(nil, "docs:1", []format.Field{{Name: "title", Value: value.Text("no vectors")}}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	type arrays struct {
		free  []int
		slots []string
	}
	arraysOf := func(s *Store) arrays {
		return arrays{slices.Clone(s.tables["pics"].vecs.free), slotKeys(s.tables["pics"])}
	}
	if got := arraysOf(start()); !slices.Equal(got.free, []int{1, 2}) || len(got.slots) != 3 {
		t.Fatalf("pics's array is %+v, where the test means it to have slots 1 and 2 free, 2 last", got)
	}

	for _, c := range []struct {
		name string
		bad  []format.Change // the batch, whose last change breaks the rules
		kind error           // what Apply's error wraps
		why  string          // in the error
	}{
		{"no table", []format.Change{block("nosuch", 2, "nosuch:1", bits(1, 2))}, errs.ErrNotFound,
			"a vector block for table nosuch, which doesn't exist"},
		{"bad table name", []format.Change{block("hc_x", 2, "hc_x:1", bits(1, 2))}, errs.ErrInvalid, `table name "hc_x"`},
		{"no vector size", []format.Change{block("docs", 2, "docs:1", bits(1, 2))}, errs.ErrInvalid,
			"a vector block for table docs, which has no vector size"},
		{"another size", []format.Change{block("pics", 3, "pics:2", bits(1, 2, 3))}, errs.ErrInvalid,
			"table pics holds vectors of 2 values, and its vector block's have 3"},
		{"no vectors", []format.Change{block("pics", 2)}, errs.ErrInvalid, "a vector block for table pics with no vectors"},
		{"out of order", []format.Change{block("pics", 2, "pics:3", bits(1, 2), "pics:2", bits(1, 2))}, errs.ErrInvalid,
			"gives pics:2 after pics:3, out of byte order"},
		{"a key twice", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:2", bits(1, 2))}, errs.ErrInvalid,
			"gives pics:2 after pics:2, out of byte order"},
		{"no record", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:7", bits(1, 2))}, errs.ErrNotFound,
			"gives a vector to pics:7, which isn't one of its records"},
		{"another table's record", []format.Change{block("pics", 2, "other:2", bits(1, 2))}, errs.ErrNotFound,
			"gives a vector to other:2, which isn't one of its records"},
		{"a key outside the rules", []format.Change{block("pics", 2, "nocolon", bits(1, 2))}, errs.ErrInvalid,
			`key "nocolon" has no table`},
		{"a vector there already", []format.Change{block("pics", 2, "pics:1", bits(1, 2))}, errs.ErrInvalid,
			"gives a vector to pics:1, which holds one already"},
		{"short bits", []format.Change{block("pics", 2, "pics:2", bits(1, 2)[:7])}, errs.ErrInvalid,
			"gives pics:2 7 bytes of values, where vectors of 2 values take 8"},
		{"NaN first", []format.Change{block("pics", 2, "pics:2", bits(nan, 1), "pics:3", bits(1, 2))}, errs.ErrInvalid,
			"the vector of pics:2 in the vector block for table pics: hypercrux: invalid: vector value 0 is NaN"},
		{"infinity second", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:3", bits(1, inf), "pics:4", bits(1, 2))},
			errs.ErrInvalid, "the vector of pics:3 in the vector block for table pics: hypercrux: invalid: vector value 1 is +Inf"},
		{"zeros last", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:3", bits(3, 4), "pics:4", bits(awkward[0], 0))},
			errs.ErrInvalid, "the vector of pics:4 in the vector block for table pics: hypercrux: invalid: a vector of only zeros"},
		{"minus infinity in a new slot", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:3", bits(3, 4),
			"pics:4", bits(-inf, 1))}, errs.ErrInvalid, "the vector of pics:4 in the vector block for table pics: hypercrux: invalid: vector value 0 is -Inf"},
		{"a good block, then a bad change", []format.Change{block("pics", 2, "pics:2", bits(1, 2), "pics:3", bits(3, 4), "pics:4", bits(5, 6)),
			{Op: format.Drop, Table: "nosuch"}}, errs.ErrNotFound, "no record table nosuch"},
	} {
		for _, how := range []string{"Apply", "ApplyBatch", "LoadBatch"} {
			s := start()
			before, was := dump(s), arraysOf(s)
			var err error
			switch how {
			case "Apply":
				for _, ch := range c.bad {
					if err = s.Apply(ch); err != nil {
						break
					}
				}
			case "ApplyBatch":
				err = s.ApplyBatch(5, c.bad)
			case "LoadBatch":
				err = s.LoadBatch(5, c.bad)
			}
			if how == "Apply" {
				if !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.why) {
					t.Errorf("%s: Apply gave %v, where it should give %v: ...%s", c.name, err, c.kind, c.why)
				}
				if len(c.bad) > 1 {
					continue // the changes before the bad one stay
				}
			} else {
				var d *errs.Damage
				if !errors.As(err, &d) || d.Batch != 5 || !strings.HasPrefix(d.Reason, "change "+strconv.Itoa(len(c.bad))+": ") ||
					!strings.Contains(d.Reason, c.why) || errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
					t.Errorf("%s: %s gave %v, where it should give damage at change %d: ...%s", c.name, how, err, len(c.bad), c.why)
				}
				if how == "LoadBatch" && len(c.bad) > 1 {
					continue // the changes before the bad one stay, and the caller throws the store away
				}
			}
			if after := dump(s); after != before {
				t.Errorf("%s: %s changed the store from\n%s\nto\n%s", c.name, how, before, after)
			}
			if now := arraysOf(s); !slices.Equal(now.free, was.free) || !slices.Equal(now.slots, was.slots) {
				t.Errorf("%s: %s changed pics's array from %+v to %+v", c.name, how, was, now)
			}
			if ch := s.Check(); len(ch.Problems) != 0 {
				t.Errorf("%s: after %s, the store's check finds %q", c.name, how, ch.Problems)
			}
			if readersWait(s) {
				t.Fatalf("%s: %s left the copy locked", c.name, how)
			}
		}
	}

	// A good block takes the free slot freed last, then the other, then a
	// new one, as puts of the same vectors take them, and gives the norms
	// they give.
	vecs := [][]float32{{awkward[1], awkward[3]}, {awkward[4], awkward[5]}, {awkward[0], awkward[6]}}
	s, puts := start(), start()
	ok(t, s.Apply(block("pics", 2, "pics:2", bits(vecs[0]...), "pics:3", bits(vecs[1]...), "pics:4", bits(vecs[2]...))))
	for i, key := range []string{"pics:2", "pics:3", "pics:4"} {
		putVec(t, puts, key, vecs[i])
	}
	if got, want := dump(s), dump(puts); got != want {
		t.Fatalf("a good block gives the store\n%s\nwhere puts of its vectors give\n%s", got, want)
	}
	if got := slotKeys(s.tables["pics"]); !slices.Equal(got, []string{"pics:1", "pics:3", "pics:2", "pics:4"}) {
		t.Fatalf("a good block left pics's slots holding %q", got)
	}
	tb := s.tables["pics"]
	for i, key := range []string{"pics:2", "pics:3", "pics:4"} {
		r := s.records[key]
		if want := math.Sqrt(vecmath.Dot(vecs[i], vecs[i])); math.Float64bits(tb.vecs.norms[r.slot]) != math.Float64bits(want) {
			t.Fatalf("a good block gives %s the norm %v, where its values give %v", key, tb.vecs.norms[r.slot], want)
		}
	}
	if ch := s.Check(); len(ch.Problems) != 0 || ch.Vectors != 5 {
		t.Fatalf("after a good block, the store's check finds %d vectors and %q", ch.Vectors, ch.Problems)
	}
}

// TestATransactionRefusesBlocks: a transaction's change list is a commit's
// batch, and only a compacted part holds a vector block, so the
// transaction's Apply refuses one, changes nothing, and carries on.
func TestATransactionRefusesBlocks(t *testing.T) {
	s := New()
	putVec(t, s, "pics:1", []float32{1, 2})
	putVec(t, s, "pics:2", nil)
	tx := begin(t, s)
	err := tx.Apply(format.Change{Op: format.Vectors, Table: "pics", Size: 2, Vecs: []format.Vector{{Key: "pics:2", Bits: value.Vector([]float32{3, 4}).Raw()}}})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "only a compacted part holds") {
		t.Fatalf("a transaction's Apply of a vector block gave %v", err)
	}
	if readersWait(s) {
		t.Fatal("the refused block took the copy's lock")
	}
	ok(t, tx.Put("pics:3", []format.Field{{Name: "vec", Value: value.Vector([]float32{5, 6})}}))
	var list []format.Change
	ok(t, tx.Commit(func(changes []format.Change) error {
		list = slices.Clone(changes)
		return nil
	}))
	if len(list) != 1 || list[0].Op != format.Put || list[0].Key != "pics:3" {
		t.Fatalf("the commit handed on %v, where it should hand on the put alone", list)
	}
	if r, err := s.Get("pics:2"); err != nil || r.Vec != nil {
		t.Fatalf("after the refused block, pics:2 gives %+v, %v", r, err)
	}
	if ch := s.Check(); len(ch.Problems) != 0 || ch.Vectors != 2 {
		t.Fatalf("after the refused block, the store's check finds %d vectors and %q", ch.Vectors, ch.Problems)
	}
}
