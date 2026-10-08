// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// putVec puts a record whose only field given is its vector, or a null
// when v is nil, straight into s.
func putVec(t *testing.T, s *Store, key string, v []float32) {
	t.Helper()
	val := value.Null()
	if v != nil {
		val = value.Vector(v)
	}
	if _, err := s.Put(nil, key, []format.Field{{Name: "vec", Value: val}}); err != nil {
		t.Fatal(err)
	}
}

// filled returns a vector of size values, each x plus its place.
func filled(size int, x float32) []float32 {
	v := make([]float32, size)
	for i := range v {
		v[i] = x + float32(i)
	}
	return v
}

// TestBlockShifts pins how many slots a block holds for vectors of each
// size: the most, as a power of two, whose values fit in a megabyte.
func TestBlockShifts(t *testing.T) {
	for size, want := range map[int]int{1: 1 << 18, 2: 1 << 17, 3: 1 << 16, 384: 512, 1000: 256, 1536: 128, 4096: 64,
		65536: 4, 32768: 8, 65535: 4} {
		if got := 1 << blockShift(size); got != want {
			t.Errorf("a block of vectors of %d values holds %d, want %d", size, got, want)
		}
	}
}

// TestTheArrayGrows: a table's first block starts with room for one slot
// and doubles until it's full, every block after it is made full, and a
// slot's values sit where vectors.go says. Record.Vec is the slot itself,
// with its capacity cut at its length, and the snapshot's vector is a copy.
func TestTheArrayGrows(t *testing.T) {
	const size = 32768 // 8 slots a block
	s := New()
	var rooms []int
	for i := range 20 {
		putVec(t, s, fmt.Sprintf("big:%02d", i), filled(size, float32(i)))
		b := s.tables["big"].vecs.blocks
		rooms = append(rooms, len(b[0])/size+(len(b)-1)*8)
	}
	if want := []int{1, 2, 4, 4, 8, 8, 8, 8, 16, 16, 16, 16, 16, 16, 16, 16, 24, 24, 24, 24}; !slices.Equal(rooms, want) {
		t.Fatalf("the array's room grew as %v, where it should grow as %v", rooms, want)
	}
	tb := s.tables["big"]
	for i := range 20 {
		r := must(s.Get(fmt.Sprintf("big:%02d", i)))
		block := tb.vecs.blocks[i/8]
		if &r.Vec[0] != &block[(i%8)*size] || len(r.Vec) != size || cap(r.Vec) != size || r.Vec[0] != float32(i) {
			t.Fatalf("big:%02d's vector isn't slot %d of the array", i, i)
		}
	}
	checkInvariants(t, s)
	var snap []format.Change
	for c := range s.Snapshot() {
		c.Fields = slices.Clone(c.Fields)
		snap = append(snap, c)
	}
	putVec(t, s, "big:00", filled(size, 100))
	if c := snap[1]; c.Key != "big:00" || c.Fields[0].Value.Vector()[0] != 0 {
		t.Fatalf("the snapshot's vector changed with its slot: %.80v", c)
	}
}

// TestSlotsComeBack: a slot freed outside a transaction is used again at
// once, the slot freed last first. One freed inside a transaction stays out
// of use until the commit, so a vector the transaction puts takes another,
// and a rollback gives the slot back to its record with the values it held.
// After the commit the slot is free for the next vector.
func TestSlotsComeBack(t *testing.T) {
	s := New()
	for i := range 6 {
		putVec(t, s, "docs:"+strconv.Itoa(i), filled(3, float32(i)))
	}
	slot := func(key string) int { return s.records[key].slot }
	if _, err := s.Delete(nil, "docs:1"); err != nil {
		t.Fatal(err)
	}
	putVec(t, s, "docs:4", nil)
	putVec(t, s, "docs:a", filled(3, 10)) // takes docs:4's slot, freed last
	putVec(t, s, "docs:b", filled(3, 11)) // then docs:1's
	putVec(t, s, "docs:c", filled(3, 12)) // then a new one
	if slot("docs:a") != 4 || slot("docs:b") != 1 || slot("docs:c") != 6 || slot("docs:4") != -1 {
		t.Fatalf("the slots are a %d, b %d, c %d and docs:4's %d", slot("docs:a"), slot("docs:b"), slot("docs:c"), slot("docs:4"))
	}
	checkInvariants(t, s)

	before := dump(s)
	tx := begin(t, s)
	ok(t, tx.Delete("docs:2"))
	ok(t, tx.Put("docs:3", []format.Field{{Name: "vec", Value: value.Null()}}))
	ok(t, tx.Put("docs:d", []format.Field{{Name: "vec", Value: value.Vector(filled(3, 13))}}))
	ok(t, tx.Put("docs:5", []format.Field{{Name: "vec", Value: value.Vector(filled(3, 14))}})) // written over in its slot
	if slot("docs:d") != 7 || len(s.tables["docs"].vecs.free) != 0 {
		t.Fatalf("a vector put in the transaction took slot %d, with %v free", slot("docs:d"), s.tables["docs"].vecs.free)
	}
	checkInvariants(t, s)
	tx.Rollback()
	checkInvariants(t, s)
	if got := dump(s); got != before {
		t.Fatalf("after the rollback the store holds\n%s\nwhere it held\n%s", got, before)
	}
	if slot("docs:2") != 2 || slot("docs:3") != 3 || len(s.tables["docs"].vecs.owners) != 7 {
		t.Fatalf("after the rollback docs:2 has slot %d and docs:3 slot %d, of %d", slot("docs:2"), slot("docs:3"), len(s.tables["docs"].vecs.owners))
	}

	tx = begin(t, s)
	ok(t, tx.Delete("docs:2"))
	ok(t, tx.Put("docs:3", []format.Field{{Name: "vec", Value: value.Null()}}))
	ok(t, tx.Put("docs:d", []format.Field{{Name: "vec", Value: value.Vector(filled(3, 13))}}))
	ok(t, tx.Commit(nil))
	checkInvariants(t, s)
	if free := s.tables["docs"].vecs.free; !slices.Equal(free, []int{2, 3}) || slot("docs:d") != 7 {
		t.Fatalf("after the commit the free slots are %v, and docs:d has slot %d", free, slot("docs:d"))
	}
	putVec(t, s, "docs:e", filled(3, 15))
	if slot("docs:e") != 3 {
		t.Fatalf("the next vector took slot %d", slot("docs:e"))
	}
	checkInvariants(t, s)
}

// TestDropTakesTheArray: a drop takes the table's array and vector size
// with it, so a table made again with the same name starts at size 0 and
// takes vectors of another size, and a search of the old size fails. Undoing
// the drop gives back the old table with its array whole. Undoing a table's
// first vector empties its array, so the next first vector can have another
// size.
func TestDropTakesTheArray(t *testing.T) {
	s := New()
	for i := range 5 {
		putVec(t, s, "docs:"+strconv.Itoa(i), filled(3, float32(i)))
	}
	q := []float32{1, 1, 1}
	before := must(s.Nearest("docs", q, 5, nil))
	tx := begin(t, s)
	ok(t, tx.Drop("docs"))
	if _, err := tx.Nearest("docs", q, 5, nil); kindOf(err) != "not found" {
		t.Fatalf("a search of a dropped table gave %v", err)
	}
	ok(t, tx.Put("docs:9", []format.Field{{Name: "vec", Value: value.Vector([]float32{1, 2})}}))
	if tb, _ := tx.Table("docs"); tb.Size != 2 || len(s.tables["docs"].vecs.owners) != 1 {
		t.Fatalf("the table made again is %+v", tb)
	}
	if _, err := tx.Nearest("docs", q, 5, nil); kindOf(err) != "invalid" {
		t.Fatalf("a search of the old size gave %v", err)
	}
	if hits := must(tx.Nearest("docs", []float32{2, 4}, 5, nil)); len(hits) != 1 || hits[0].Key != "docs:9" {
		t.Fatalf("a search of the new size gave %v", hits)
	}
	tx.Rollback()
	checkInvariants(t, s)
	if after := must(s.Nearest("docs", q, 5, nil)); !sameHits(after, before) {
		t.Fatalf("after the rollback the search gives %v, where it gave %v", after, before)
	}

	putVec(t, s, "pics:1", nil) // a table with a vector field and no size yet
	tx = begin(t, s)
	ok(t, tx.Put("pics:1", []format.Field{{Name: "vec", Value: value.Vector(filled(384, 1))}}))
	ok(t, tx.Put("pics:2", []format.Field{{Name: "vec", Value: value.Vector(filled(384, 2))}}))
	tx.Rollback()
	checkInvariants(t, s) // which requires the array to be empty again
	putVec(t, s, "pics:1", []float32{5})
	if tb, _ := s.Table("pics"); tb.Size != 1 || s.tables["pics"].vecs.shift != blockShift(1) {
		t.Fatalf("pics is %+v, with the shift %d", tb, s.tables["pics"].vecs.shift)
	}
	checkInvariants(t, s)
}

// TestTheVectorCheck: checkVector takes exactly the vectors rules.Vector
// takes, and refuses the rest with its error, on random bits, which are NaN
// or infinite about one time in 256, and on vectors made of awkward values:
// zeros of both signs, the smallest and largest float32s, NaNs and
// infinities. A vector it takes comes back decoded, with its dot product
// with itself as vecmath gives it.
func TestTheVectorCheck(t *testing.T) {
	s := New()
	r := rand.New(rand.NewPCG(9, 0x57))
	awkward := []float32{0, float32(math.Copysign(0, -1)), math.Float32frombits(1), -math.Float32frombits(1), math.MaxFloat32,
		-math.MaxFloat32, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), 1, -2.5}
	try := func(v []float32) {
		t.Helper()
		vals, dot, err := s.checkVector(value.Vector(v))
		want := rules.Vector(v)
		switch {
		case fmt.Sprint(err) != fmt.Sprint(want):
			t.Fatalf("checkVector(%v) gave %v, where rules.Vector gives %v", v, err, want)
		case err == nil && (!slices.EqualFunc(vals, v, func(a, b float32) bool { return math.Float32bits(a) == math.Float32bits(b) }) ||
			math.Float64bits(dot) != math.Float64bits(vecmath.Dot(v, v))):
			t.Fatalf("checkVector(%v) gave %v and %v", v, vals, dot)
		}
	}
	for range 20000 {
		v := make([]float32, 1+r.IntN(20))
		for i := range v {
			if r.IntN(3) == 0 {
				v[i] = awkward[r.IntN(len(awkward))]
			} else {
				v[i] = math.Float32frombits(r.Uint32())
			}
		}
		if r.IntN(4) == 0 {
			for i := range v {
				v[i] = awkward[r.IntN(2)] // zeros of both signs
			}
			if r.IntN(2) == 0 {
				v[r.IntN(len(v))] = awkward[r.IntN(len(awkward))]
			}
		}
		try(v)
	}
	try(nil)
	try(make([]float32, rules.MaxDims+1))
	most := make([]float32, rules.MaxDims)
	for i := range most {
		most[i] = math.MaxFloat32
	}
	try(most) // the largest sum of squares there can be, which is finite
	most[0] = float32(math.Inf(1))
	try(most)
}

// TestNearestTimings times the searches in BETA.md's table of targets, 10
// nearest among 100,000 vectors of 384 values, the same with a tenth
// passing a filter on a field, and among 10,000 of 1,536, with LoadBatch of
// the 100,000 records with their vectors, as opening a compacted file loads
// them: in key order, in batches of about a megabyte, each made just before
// it's loaded, as the codec decodes each batch just before. It prints the
// times, the middle of five runs of each search, for G2 and I1 to check
// against the targets, and the memory a record takes and the arrays take
// beside the vectors' values. It checks the hits against a search written
// plainly for a few queries, and nothing about the times, which depend on
// the machine and on what else runs on it.
//
// The times show only with -v, so it takes the full sizes only then, and
// without -short. Otherwise it takes a tenth of them, which checks the same
// results quickly, since every job that runs the store's tests, the one for
// planted bugs among them, runs it, and with -short a fiftieth, which still
// fills several blocks, for the race detector.
func TestNearestTimings(t *testing.T) {
	n384, n1536 := 10_000, 1_000
	switch {
	case testing.Short():
		n384, n1536 = 2_000, 300
	case testing.Verbose():
		n384, n1536 = 100_000, 10_000
	}
	r := rand.New(rand.NewPCG(uint64(n384), 384))
	s := New()
	seq := uint64(0)
	// load loads a table of n records with a field and a vector of size
	// values, and returns how long the calls to LoadBatch took.
	load := func(table string, n, size int) time.Duration {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = table + ":" + strconv.Itoa(i)
		}
		slices.Sort(keys) // in a compacted file's order
		b := []format.Change{{Op: format.CreateTable, Table: table, Size: size, Names: []string{"grp", "vec"}}}
		var took time.Duration
		for i, key := range keys {
			v := make([]float32, size)
			for j := range v {
				v[j] = float32(r.NormFloat64())
			}
			b = append(b, format.Change{Op: format.Put, Key: key, Fields: []format.Field{
				{Name: "grp", Value: value.Int(int64(i % 10))}, {Name: "vec", Value: value.Vector(v)},
			}})
			if len(b)*size*4 >= 1<<20 || i == n-1 {
				seq++
				start := time.Now()
				ok(t, s.LoadBatch(seq, b))
				took += time.Since(start)
				b = b[:0]
			}
		}
		return took
	}
	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	took := load("docs", n384, 384)
	after := heap()
	load("wide", n1536, 1536)
	tb := s.tables["docs"]
	a := &tb.vecs
	values := 0
	for _, b := range a.blocks {
		values += len(b)
	}
	beside := float64(4*(values-n384*384)+8*cap(a.norms)+8*cap(a.owners)) / float64(n384)
	t.Logf("LoadBatch of %d records with a field and a vector of 384 values, in batches of a megabyte: %v, %.2f µs a record, and %.0f bytes a record in all",
		n384, took.Round(time.Millisecond), float64(took.Nanoseconds())/float64(n384)/1e3, float64(after-before)/float64(n384))
	t.Logf("the array beside the vectors' values: %.1f bytes a vector, of %d blocks of %d slots", beside, len(a.blocks), 1<<a.shift)

	grp := s.tables["docs"].index["grp"]
	tenth := func(rec Record) (bool, error) { return rec.Field(grp) == value.Int(3), nil }
	for _, c := range []struct {
		what  string
		table string
		size  int
		keep  Filter
		n     int
	}{
		{fmt.Sprintf("10 nearest among %d vectors of 384 values", n384), "docs", 384, nil, n384},
		{fmt.Sprintf("10 nearest among %d of 384, a tenth passing a filter", n384), "docs", 384, tenth, n384 / 10},
		{fmt.Sprintf("10 nearest among %d vectors of 1,536 values", n1536), "wide", 1536, nil, n1536},
	} {
		var times []time.Duration
		for run := range 5 {
			q := make([]float32, c.size)
			for j := range q {
				q[j] = float32(r.NormFloat64())
			}
			start := time.Now()
			hits, err := s.Nearest(c.table, q, 10, c.keep)
			times = append(times, time.Since(start))
			if err != nil || len(hits) != 10 {
				t.Fatalf("%s gave %v, %v", c.what, hits, err)
			}
			if run < 2 {
				checkAgainstPlain(t, s, c.table, q, c.keep, hits)
			}
		}
		slices.Sort(times)
		t.Logf("%s: %v, from %v to %v", c.what, times[2].Round(100*time.Microsecond), times[0].Round(100*time.Microsecond),
			times[4].Round(100*time.Microsecond))
	}
	// The most a search gives, which with -short is every vector, in every
	// block.
	q := make([]float32, 384)
	for j := range q {
		q[j] = float32(r.NormFloat64())
	}
	hits, err := s.Nearest("docs", q, MaxK, nil)
	if err != nil || len(hits) != min(MaxK, n384) {
		t.Fatalf("a search for the most hits gave %d, %v", len(hits), err)
	}
	checkAgainstPlain(t, s, "docs", q, nil, hits)
	checkInvariants(t, s)
}

// checkAgainstPlain checks a search's hits against every record of the
// table, compared plainly with vecmath.
func checkAgainstPlain(t *testing.T, s *Store, table string, q []float32, keep Filter, hits []Hit) {
	t.Helper()
	var want []Hit
	for r := range s.tables[table].keys.records {
		rec := r.read()
		if keep != nil {
			if ok, _ := keep(rec); !ok {
				continue
			}
		}
		want = append(want, Hit{r.key, vecmath.Distance(vecmath.Dot(rec.Vec, q), vecmath.Norm(rec.Vec), vecmath.Norm(q))})
	}
	slices.SortFunc(want, func(a, b Hit) int { return cmp.Or(cmp.Compare(a.Distance, b.Distance), strings.Compare(a.Key, b.Key)) })
	if !sameHits(hits, want[:len(hits)]) {
		t.Fatalf("a search of %s gave %v, where every record compared gives %v", table, hits, want[:len(hits)])
	}
}
