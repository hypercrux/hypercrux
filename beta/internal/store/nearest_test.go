// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux"
	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// 0.x's cases for searches, from its tests and the conformance suite, each
// run on 0.x and on the store side by side, as S1's, S4's and S5's tests run
// theirs, and searches of awkward vectors against a search written plainly.
// A search on both must give the same error, with the same message, or the
// same hits within conformance.DistanceBound, in the same order but where
// two distances fall within the bound, and nil for nil. 0.x adds its sums in
// another order, so its distances can differ from the store's in the last
// few bits. 0.x's filter is SQL, and the store's a Filter that says the
// same thing in Go.

// toHits converts the store's hits for conformance.CompareHits, keeping nil.
func toHits(hits []store.Hit) []c.Hit {
	if hits == nil {
		return nil
	}
	out := make([]c.Hit, len(hits))
	for i, h := range hits {
		out[i] = c.Hit(h)
	}
	return out
}

// zeroxHits converts 0.x's hits as toHits converts the store's.
func zeroxHits(hits []hc.Hit) []c.Hit {
	if hits == nil {
		return nil
	}
	out := make([]c.Hit, len(hits))
	for i, h := range hits {
		out[i] = c.Hit(h)
	}
	return out
}

// sameSearch fails the test unless the store's search and 0.x's agree: the
// same error with the same message, or hits that agree, nil for nil.
func sameSearch(t *testing.T, desc string, got []store.Hit, err error, want []hc.Hit, zerr error) {
	t.Helper()
	sameError(t, desc, zerr, err)
	if err != nil {
		return
	}
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: the store gives %#v, and 0.x %#v", desc, got, want)
	}
	if e := c.CompareHits(toHits(got), zeroxHits(want)); e != nil {
		t.Fatalf("%s: %v", desc, e)
	}
}

// nearest searches on both, which must agree, and returns the store's hits
// and error. where and args are 0.x's filter, and keep is the store's.
func (p *pair) nearest(table string, q []float32, k int, keep store.Filter, where string, args ...any) ([]store.Hit, error) {
	p.t.Helper()
	want, zerr := p.db.Nearest(table, hc.Vector(q), k, where, args...)
	got, err := p.s.Nearest(table, q, k, keep)
	sameSearch(p.t, fmt.Sprintf("Nearest(%q, %v, %d, %q)", table, q, k, where), got, err, want, zerr)
	return got, err
}

// putVec puts a record with a vector, and maybe more fields, into both.
func (p *pair) putVec(key string, v []float32, more fields) {
	p.t.Helper()
	f := fields{}
	for name, x := range more {
		f[name] = x
	}
	if v != nil {
		f["vec"] = v
	}
	zf := hc.Fields{}
	for name, x := range f {
		if v, ok := x.([]float32); ok {
			x = hc.Vector(v)
		}
		zf[name] = x
	}
	ok(p.t, p.db.Put(key, zf))
	ok(p.t, putGo(p.s, key, f))
}

// fieldIs returns a Filter that lets through the records whose field name,
// matched regardless of case, holds the whole number n, as 0.x's filter
// "name" = n does.
func fieldIs(s store.Reader, table, name string, n int64) store.Filter {
	tb, _ := s.Table(table)
	at := tb.Find(name)
	return func(r store.Record) (bool, error) { return r.Field(at) == value.Int(n), nil }
}

func keysOfHits(hits []store.Hit) string {
	var keys []string
	for _, h := range hits {
		keys = append(keys, h.Key)
	}
	return strings.Join(keys, " ")
}

func randomVector(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// TestNearestIsExact is 0.x's TestNearestIsExact and the suite's
// NearestIsExact, on 0.x and on the store side by side: 2,000 vectors of 32
// random values, searched 20 times without a filter and with one that lets
// a third through, then the errors those tests check.
func TestNearestIsExact(t *testing.T) {
	p := newPair(t)
	r := rand.New(rand.NewPCG(1, 2))
	ok(t, p.db.Update(func(tx *hc.Tx) error {
		for i := range 2000 {
			key := "docs:" + string(rune('a'+i%26)) + "-" + strconv.Itoa(i)
			v := randomVector(r, 32)
			if err := tx.Put(key, hc.Fields{"group": i % 3, "vec": hc.Vector(v)}); err != nil {
				return err
			}
			if err := putGo(p.s, key, fields{"group": i % 3, "vec": v}); err != nil {
				return err
			}
		}
		return nil
	}))
	for range 20 {
		q := randomVector(r, 32)
		if hits := must[[]store.Hit](t)(p.nearest("docs", q, 10, nil, "")); len(hits) != 10 {
			t.Fatalf("a search for 10 gave %v", hits)
		}
		hits := must[[]store.Hit](t)(p.nearest("docs", q, 7, fieldIs(p.s, "docs", "group", 1), `"group" = ?`, 1))
		for _, h := range hits {
			if n, _ := strconv.Atoi(h.Key[strings.IndexByte(h.Key, '-')+1:]); n%3 != 1 || len(hits) != 7 {
				t.Fatalf("a search of group 1 gave %v", hits)
			}
		}
	}
	_, err := p.nearest("docs", []float32{1, 2}, 5, nil, "")
	wantErr(t, err, errs.ErrInvalid)
	_, err = p.nearest("nosuch", randomVector(r, 32), 5, nil, "")
	wantErr(t, err, errs.ErrNotFound)
	_, err = p.nearest("docs", randomVector(r, 32), 0, nil, "")
	wantErr(t, err, errs.ErrInvalid)
	p.putVec("plain:1", nil, fields{"a": 1})
	if hits := must[[]store.Hit](t)(p.nearest("plain", []float32{1}, 3, nil, "")); hits != nil {
		t.Fatalf("a table without vectors gave %v", hits)
	}
}

// TestNearestChecksIn0xsOrder: each check fails with 0.x's error and
// message, in 0.x's order: the table's name, then the query, then k, then
// the table, then the query's size. A table with no vector size gives nil,
// and one with a size and nothing that gets through an empty list.
func TestNearestChecksIn0xsOrder(t *testing.T) {
	p := newPair(t)
	p.putVec("docs:1", []float32{1, 0, 0}, nil)
	p.putVec("docs:2", []float32{0, 1, 0}, fields{"n": 2})
	p.putVec("plain:1", nil, fields{"n": 1})
	p.putVec("gone:1", []float32{1, 2}, nil)
	p.putVec("gone:2", []float32{3, 4}, nil)
	p.putVec("gone:1", nil, fields{"vec": nil})
	ok(t, p.db.Delete("gone:2"))
	must[[]format.Change](t)(p.s.Delete(nil, "gone:2"))
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	long := make([]float32, rules.MaxDims+1)
	long[0] = 1
	most := make([]float32, rules.MaxDims)
	most[rules.MaxDims-1] = 1
	none := func(store.Record) (bool, error) { return false, nil }
	for _, c := range []struct {
		table string
		q     []float32
		k     int
		want  error
	}{
		{"Docs", nil, 0, errs.ErrInvalid},                         // the table's name first
		{"hc_x", []float32{1, 0, 0}, 5, errs.ErrInvalid},          // a name 0.x keeps for itself
		{"", []float32{1}, 1, errs.ErrInvalid},                    // no name
		{"docs", nil, 0, errs.ErrInvalid},                         // then the query: no values
		{"nosuch", []float32{0, 0, 0}, -1, errs.ErrInvalid},       // zeros
		{"nosuch", []float32{1, nan}, 10001, errs.ErrInvalid},     // NaN
		{"docs", []float32{inf, 1, 0}, 3, errs.ErrInvalid},        // infinity
		{"docs", long, 1, errs.ErrInvalid},                        // more than 65,536 values
		{"nosuch", []float32{1}, 0, errs.ErrInvalid},              // then k
		{"nosuch", []float32{1}, -1, errs.ErrInvalid},             // a k below 1
		{"nosuch", []float32{1}, store.MaxK + 1, errs.ErrInvalid}, // a k past 10,000
		{"nosuch", []float32{1}, store.MaxK, errs.ErrNotFound},    // then the table
		{"nosuch", []float32{1, 2, 3}, 1, errs.ErrNotFound},       // before the size
		{"docs", []float32{1, 2}, 1, errs.ErrInvalid},             // then the size
		{"docs", most, 1, errs.ErrInvalid},                        // 65,536 values, in a table of 3
		{"plain", []float32{1, 2}, 1, nil},                        // no vector size: nil
		{"gone", []float32{1, 2}, 5, nil},                         // a size, and no vectors left
		{"docs", []float32{0, 0, -1}, store.MaxK, nil},            // k past the records there are
	} {
		hits, err := p.nearest(c.table, c.q, c.k, nil, "")
		if !errors.Is(err, c.want) || c.want == nil && err != nil {
			t.Errorf("Nearest(%q, %d values, %d) gave %v, want %v", c.table, len(c.q), c.k, err, c.want)
		}
		switch {
		case err != nil:
		case c.table == "plain" && hits != nil, c.table == "gone" && (hits == nil || len(hits) != 0):
			t.Errorf("Nearest(%q) gave %#v", c.table, hits)
		case c.table == "docs" && len(hits) != 2:
			t.Errorf("Nearest(docs) for every record gave %v", hits)
		}
	}
	// Messages a program might look for.
	_, err := p.s.Nearest("docs", []float32{1, 2}, 1, nil)
	if err.Error() != "hypercrux: invalid: table docs holds vectors of 3 values, and the query has 2" {
		t.Errorf("the wrong size gave %q", err)
	}
	_, err = p.s.Nearest("docs", []float32{1, 0, 0}, 0, nil)
	if err.Error() != "hypercrux: invalid: k is from 1 to 10000, not 0" {
		t.Errorf("k of 0 gave %q", err)
	}
	// A filter that lets nothing through gives an empty list too, and one
	// on a field the other way gives what 0.x's gives.
	if hits := must[[]store.Hit](t)(p.nearest("docs", []float32{1, 1, 0}, 3, none, "1 = 0")); hits == nil || len(hits) != 0 {
		t.Errorf("a filter that lets nothing through gave %#v", hits)
	}
	if hits := must[[]store.Hit](t)(p.nearest("docs", []float32{1, 1, 0}, 3, fieldIs(p.s, "docs", "N", 2), "n = ?", 2)); keysOfHits(hits) != "docs:2" {
		t.Errorf("a filter on n gave %v", hits)
	}
}

// TestNearestTiesComeByKey: records as far from the query as each other
// come in byte order of key, from 0.x and from the store alike, whatever
// order they were put in, and so whichever slots they hold. Here they hold
// the same vector, or the same one doubled, which is as far in every
// direction, bit for bit, or they're opposite the query, or across it, where
// every distance is 2 or 1. A record put later can take the slot of one
// deleted, before the others' in the array.
func TestNearestTiesComeByKey(t *testing.T) {
	p := newPair(t)
	same := []float32{0.25, -1, 3}
	for _, key := range []string{"docs:z", "docs:9", "docs:10", "docs:B", "docs:a", "docs:é", "docs:a b"} {
		p.putVec(key, same, nil)
	}
	p.putVec("docs:x", []float32{0.5, -2, 6}, nil)    // the same doubled
	p.putVec("docs:op", []float32{-0.25, 1, -3}, nil) // opposite
	p.putVec("docs:o2", []float32{-0.5, 2, -6}, nil)
	p.putVec("docs:n1", []float32{3, 0, -0.25}, nil) // across it
	p.putVec("docs:n0", []float32{-6, 0, 0.5}, nil)
	ok(t, p.db.Delete("docs:9"))
	must[[]format.Change](t)(p.s.Delete(nil, "docs:9"))
	p.putVec("docs:0", same, nil) // into docs:9's slot, in the store
	for _, q := range [][]float32{same, {-0.25, 1, -3}, {3, 0, -0.25}, {1, 1, 1}} {
		for _, k := range []int{1, 3, 8, 12} {
			got := must[[]store.Hit](t)(p.nearest("docs", q, k, nil, ""))
			want := must[[]hc.Hit](t)(p.db.Nearest("docs", hc.Vector(q), k, ""))
			var wantKeys []string
			for _, h := range want {
				wantKeys = append(wantKeys, h.Key)
			}
			if keysOfHits(got) != strings.Join(wantKeys, " ") {
				t.Fatalf("Nearest(%v, %d) gives %v, and 0.x %v", q, k, got, want)
			}
		}
	}
	// The same vector and its double are as far as each other, bit for bit,
	// and so are the two opposite ones, and the two across it, at 1.
	all := must[[]store.Hit](t)(p.s.Nearest("docs", same, 20, nil))
	if len(all) != 12 || keysOfHits(all[:8]) != "docs:0 docs:10 docs:B docs:a docs:a b docs:x docs:z docs:é" ||
		keysOfHits(all[8:10]) != "docs:n0 docs:n1" || keysOfHits(all[10:]) != "docs:o2 docs:op" {
		t.Fatalf("a search for the vector they hold gave %v", all)
	}
	for i := 1; i < len(all); i++ {
		if i != 8 && i != 10 && all[i].Distance != all[i-1].Distance {
			t.Fatalf("hits %d and %d aren't as far as each other: %v", i-1, i, all)
		}
	}
	if all[0].Distance > 1e-15 || all[8].Distance != 1 || all[11].Distance < 2-1e-15 {
		t.Fatalf("the distances are %v", all)
	}
}

// TestNearestInsideATransaction is the part of 0.x's TestUpdateIsAllOrNothing
// about searches, with more: inside 0.x's Update and through the store's Tx,
// a search sees the transaction's own changes, a new record's vector, a
// vector changed, one taken away, a deleted record's gone, and a new table
// of another size, and after a rollback none of them.
func TestNearestInsideATransaction(t *testing.T) {
	p := newPair(t)
	p.putVec("docs:1", []float32{1, 0, 0}, fields{"n": 1})
	p.putVec("docs:2", []float32{0, 1, 0}, fields{"n": 2})
	p.putVec("docs:3", []float32{0, 0, 1}, fields{"n": 1})
	queries := [][]float32{{1, 2, 3}, {1, 0, 0}, {-1, -1, 0}}
	type search struct {
		q    []float32
		hits []c.Hit
		err  error
	}
	searchAll := func(nearest func(table string, q []float32) ([]c.Hit, error)) []search {
		var out []search
		for _, q := range queries {
			hits, err := nearest("docs", q)
			out = append(out, search{q, hits, err})
		}
		hits, err := nearest("fresh", []float32{1, 2})
		return append(out, search{[]float32{1, 2}, hits, err})
	}
	compare := func(what string, got, want []search) {
		t.Helper()
		for i := range want {
			if errText(got[i].err) != errText(want[i].err) || (got[i].hits == nil) != (want[i].hits == nil) {
				t.Fatalf("%s, %v: the store gives %v, %v, and 0.x %v, %v", what, want[i].q, got[i].hits, got[i].err, want[i].hits, want[i].err)
			}
			if err := c.CompareHits(got[i].hits, want[i].hits); err != nil {
				t.Fatalf("%s, %v: %v", what, want[i].q, err)
			}
		}
	}
	zeroxSearch := func(n interface {
		Nearest(string, hc.Vector, int, string, ...any) ([]hc.Hit, error)
	}) func(string, []float32) ([]c.Hit, error) {
		return func(table string, q []float32) ([]c.Hit, error) {
			hits, err := n.Nearest(table, hc.Vector(q), 10, "")
			return zeroxHits(hits), err
		}
	}
	storeSearch := func(r store.Reader) func(string, []float32) ([]c.Hit, error) {
		return func(table string, q []float32) ([]c.Hit, error) {
			hits, err := r.Nearest(table, q, 10, nil)
			return toHits(hits), err
		}
	}
	before := searchAll(storeSearch(p.s))
	compare("before", before, searchAll(zeroxSearch(p.db)))
	var inside []search
	rollBack := errors.New("roll back")
	err := p.db.Update(func(ztx *hc.Tx) error {
		ok(t, ztx.Put("docs:4", hc.Fields{"vec": hc.Vector{1, 2, 3}}))
		ok(t, ztx.Put("docs:1", hc.Fields{"vec": hc.Vector{0, -1, 0}}))
		ok(t, ztx.Put("docs:2", hc.Fields{"vec": nil}))
		ok(t, ztx.Delete("docs:3"))
		ok(t, ztx.Put("fresh:1", hc.Fields{"vec": hc.Vector{2, 1}}))
		inside = searchAll(zeroxSearch(ztx))
		return rollBack
	})
	if !errors.Is(err, rollBack) {
		t.Fatal(err)
	}
	tx := must[*store.Tx](t)(p.s.Begin())
	defer tx.Rollback()
	for _, w := range []struct {
		key string
		vec value.Value
	}{{"docs:4", value.Vector([]float32{1, 2, 3})}, {"docs:1", value.Vector([]float32{0, -1, 0})}, {"docs:2", value.Null()}} {
		ok(t, tx.Put(w.key, []format.Field{{Name: "vec", Value: w.vec}}))
	}
	ok(t, tx.Delete("docs:3"))
	ok(t, tx.Put("fresh:1", []format.Field{{Name: "vec", Value: value.Vector([]float32{2, 1})}}))
	compare("inside the transaction", searchAll(storeSearch(tx)), inside)
	tx.Rollback()
	compare("after the rollback", searchAll(storeSearch(p.s)), before)
	compare("after the rollback, against 0.x", searchAll(storeSearch(p.s)), searchAll(zeroxSearch(p.db)))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestNearestAfterDeletesAndDrops is the part of 0.x's
// TestTableNamesLikeHyperCruxsOwn and TestDropAndAdoptAfterSchemaChanges
// about searches, on both: tables named with the words 0.x uses for its
// own, a search after a delete finds what's left, and a table dropped and
// made again takes vectors of another size, which a search of the old size
// refuses.
func TestNearestAfterDeletesAndDrops(t *testing.T) {
	p := newPair(t)
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		p.putVec(tbl+":1", []float32{1, 0}, nil)
		p.putVec(tbl+":2", []float32{0, 1}, nil)
	}
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		p.delete(tbl + ":2")
		if hits := must[[]store.Hit](t)(p.nearest(tbl, []float32{1, 0}, 5, nil, "")); len(hits) != 1 || hits[0].Key != tbl+":1" {
			t.Errorf("%s: Nearest found %v", tbl, hits)
		}
	}
	p = newPair(t)
	p.putVec("customer:1", nil, nil)
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		p.putVec(k, []float32{1, 2, 3}, fields{"title": k})
	}
	must[[]store.Hit](t)(p.nearest("docs", []float32{3, 2, 1}, 5, nil, ""))
	p.drop("docs")
	_, err := p.nearest("docs", []float32{3, 2, 1}, 5, nil, "")
	wantErr(t, err, errs.ErrNotFound)
	p.putVec("docs:9", []float32{1, 2}, nil)
	_, err = p.nearest("docs", []float32{3, 2, 1}, 5, nil, "")
	wantErr(t, err, errs.ErrInvalid)
	if hits := must[[]store.Hit](t)(p.nearest("docs", []float32{2, 1}, 5, nil, "")); keysOfHits(hits) != "docs:9" {
		t.Fatalf("the new docs gave %v", hits)
	}
	if tb, _ := p.s.Table("docs"); tb.Size != 2 {
		t.Fatalf("the new docs has %+v", tb)
	}
}

// TestNearestAgainstBruteForce searches random and awkward vectors, and
// checks every result twice. Against a search written plainly with
// vecmath's arithmetic it must match bit for bit, in the same order, ties
// broken by key. And each hit's distance must agree within DistanceBound
// with the vector's distance worked out in plain float64 arithmetic, adding
// in order, as conformance's reference does. Two vectors that are as far
// as each other within the bound can fall either side of the k-th place in
// plain arithmetic, so it judges the distances, and vecmath's search the
// order. The
// vectors come in sizes from 1 to 32,768, which takes several blocks of 8:
// random values; values near float32's smallest and largest, and both at
// once; vectors nearly the same, one ulp apart; the same, opposite and
// across each other; and one big value among small ones. Deletes and
// vectors taken away leave free slots among the rest, which later vectors
// take again. The queries are random, stored vectors, their opposites, and
// tiny and huge ones, with k from 1 to past the records there are, and now
// and then a filter on the key.
func TestNearestAgainstBruteForce(t *testing.T) {
	sizes := []int{1, 2, 3, 7, 8, 9, 16, 17, 33, 384, 1536, 32768}
	rounds := 25
	if testing.Short() {
		rounds = 2
	}
	r := rand.New(rand.NewPCG(7, 0x56))
	kinds := map[string]int{}
	for round := range rounds {
		for _, size := range sizes {
			s := store.New()
			vecs := map[string][]float32{}
			n := 5 + r.IntN(60)
			switch {
			case size > 1536:
				n = 9 + r.IntN(30) // two blocks or more
			case size >= 384:
				n = 5 + r.IntN(20)
			}
			put := func(key string, v []float32) {
				t.Helper()
				ok(t, putGo(s, key, fields{"vec": v}))
				vecs[key] = v
			}
			var last []float32
			for i := range n {
				kind, v := awkwardVector(r, size, last)
				kinds[kind]++
				put(fmt.Sprintf("v:%03d", (i*37)%n), v)
				last = v
			}
			// Free slots in the middle, and some taken again.
			for _, key := range slices.Sorted(maps.Keys(vecs)) {
				switch r.IntN(6) {
				case 0:
					must[[]format.Change](t)(s.Delete(nil, key))
					delete(vecs, key)
				case 1:
					ok(t, putGo(s, key, fields{"vec": nil}))
					delete(vecs, key)
				}
			}
			for i := range n / 4 {
				_, v := awkwardVector(r, size, last)
				put(fmt.Sprintf("v:w%d", i), v)
			}
			for range 8 {
				q := queryFor(r, size, vecs)
				k := 1 + r.IntN(len(vecs)+3)
				var keep store.Filter
				lets := func(string) bool { return true }
				if r.IntN(3) == 0 {
					lets = func(key string) bool { return key[len(key)-1]%2 == 0 }
					keep = func(rec store.Record) (bool, error) { return lets(rec.Key), nil }
				}
				got := must[[]store.Hit](t)(s.Nearest("v", q, k, keep))
				exact, plain := bruteForce(vecs, q, k, lets)
				if !slices.Equal(got, exact) {
					t.Fatalf("round %d, size %d: Nearest gave\n%v\nwhere the search written plainly gives\n%v", round, size, got, exact)
				}
				for _, h := range got {
					if d := plain[h.Key]; math.Abs(h.Distance-d) > c.DistanceBound {
						t.Fatalf("round %d, size %d: %s is at %v, and at %v in plain arithmetic", round, size, h.Key, h.Distance, d)
					}
				}
				kinds["search"]++
				for i := 1; i < len(got); i++ {
					if got[i].Distance == got[i-1].Distance {
						kinds["a tie"]++
					}
				}
			}
		}
	}
	for _, k := range []string{"random", "tiny", "huge", "tiny and huge", "one ulp from the last", "the last", "the last opposite",
		"one big among small", "search", "a tie"} {
		if kinds[k] == 0 {
			t.Errorf("%q never came up: %v", k, kinds)
		}
	}
	if testing.Verbose() {
		t.Logf("%v", kinds)
	}
}

// awkwardVector returns a vector of size values of one kind, picked at
// random, and its kind. last is the vector made before, or nil.
func awkwardVector(r *rand.Rand, size int, last []float32) (string, []float32) {
	v := make([]float32, size)
	kind := []string{"random", "random", "tiny", "huge", "tiny and huge", "one ulp from the last", "the last",
		"the last opposite", "one big among small"}[r.IntN(9)]
	if last == nil && strings.Contains(kind, "last") {
		kind = "random"
	}
	for i := range v {
		switch kind {
		case "random":
			v[i] = float32(r.NormFloat64())
		case "tiny":
			v[i] = math.Float32frombits(uint32(r.IntN(1 << 10))) // subnormals, the smallest float32s
		case "huge":
			v[i] = math.MaxFloat32 * (r.Float32() - 0.5)
		case "tiny and huge":
			v[i] = math.Float32frombits(uint32(1 + r.IntN(8)))
			if r.IntN(2) == 0 {
				v[i] = -math.MaxFloat32 / float32(1+r.IntN(4))
			}
		case "one ulp from the last", "the last":
			v[i] = last[i]
		case "the last opposite":
			v[i] = -last[i]
		case "one big among small":
			v[i] = float32(r.NormFloat64()) * 1e-30
		}
	}
	switch kind {
	case "one ulp from the last":
		j := r.IntN(size)
		if next := math.Float32frombits(math.Float32bits(v[j]) + 1); !math.IsInf(float64(next), 0) {
			v[j] = next
		} else {
			v[j] = math.Float32frombits(math.Float32bits(v[j]) - 1) // one ulp the other way, from float32's largest
		}
	case "one big among small":
		v[r.IntN(size)] = 1e30
	}
	if rules.Vector(v) != nil {
		v[r.IntN(size)] = 1 // all zeros, from the tiny values
	}
	return kind, v
}

// queryFor returns a query of size values: random, a stored vector or its
// opposite, or a tiny or a huge one.
func queryFor(r *rand.Rand, size int, vecs map[string][]float32) []float32 {
	stored := make([]string, 0, len(vecs))
	for key := range vecs {
		stored = append(stored, key)
	}
	slices.Sort(stored)
	q := make([]float32, size)
	switch r.IntN(5) {
	case 0, 1:
		if len(stored) > 0 {
			copy(q, vecs[stored[r.IntN(len(stored))]])
			if r.IntN(2) == 0 {
				for i := range q {
					q[i] = -q[i]
				}
			}
			return q
		}
		fallthrough
	case 2:
		for i := range q {
			q[i] = float32(r.NormFloat64())
		}
	case 3:
		for i := range q {
			q[i] = math.Float32frombits(uint32(1 + r.IntN(100)))
		}
	case 4:
		for i := range q {
			q[i] = math.MaxFloat32 * (r.Float32() - 0.5)
		}
	}
	if rules.Vector(q) != nil {
		q[0] = 1
	}
	return q
}

// bruteForce is the search written plainly: every vector that lets lets
// through, each distance worked out with vecmath as the store must, sorted
// by distance and then key, and the first k. plain holds each vector's
// distance worked out in float64 arithmetic adding in order, as
// conformance's reference does.
func bruteForce(vecs map[string][]float32, q []float32, k int, lets func(string) bool) (exact []store.Hit, plain map[string]float64) {
	exact, plain = []store.Hit{}, map[string]float64{}
	for key, v := range vecs {
		if !lets(key) {
			continue
		}
		exact = append(exact, store.Hit{Key: key, Distance: vecmath.Distance(vecmath.Dot(v, q), vecmath.Norm(v), vecmath.Norm(q))})
		var dot, nv, nq float64
		for i := range v {
			dot += float64(v[i]) * float64(q[i])
			nv += float64(v[i]) * float64(v[i])
			nq += float64(q[i]) * float64(q[i])
		}
		plain[key] = min(max(1-dot/(math.Sqrt(nv)*math.Sqrt(nq)), 0), 2)
	}
	slices.SortFunc(exact, func(a, b store.Hit) int {
		return cmp.Or(cmp.Compare(a.Distance, b.Distance), strings.Compare(a.Key, b.Key))
	})
	return exact[:min(k, len(exact))], plain
}

// TestNearestFilter: the Filter is called once for each record with a
// vector, and for no other record, with the Record Get gives, its Vec the
// record's slot. It's called before the distance, so it can rule a record
// out before its dot product. An error from it stops the search at once,
// and comes back as it is. It isn't called by a search that fails a check,
// or by one of a table with no vectors.
func TestNearestFilter(t *testing.T) {
	s := store.New()
	for i := range 40 {
		f := fields{"n": i}
		if i%4 != 3 {
			f["vec"] = []float32{float32(i + 1), float32(i % 5), -1}
		}
		ok(t, putGo(s, fmt.Sprintf("docs:%02d", i), f))
	}
	must[[]format.Change](t)(s.Delete(nil, "docs:04"))
	ok(t, putGo(s, "docs:05", fields{"vec": nil}))
	ok(t, putGo(s, "plain:1", fields{"n": 1}))
	withVectors := 40 - 10 - 2
	seen := map[string]int{}
	keep := func(rec store.Record) (bool, error) {
		seen[rec.Key]++
		got := must[store.Record](t)(s.Get(rec.Key))
		if len(rec.Vec) == 0 || &rec.Vec[0] != &got.Vec[0] || cap(rec.Vec) != len(rec.Vec) || !slices.Equal(rec.Fields, got.Fields) {
			t.Fatalf("the filter was handed %v, where Get gives %v", rec, got)
		}
		return rec.Field(0).Int()%2 == 0, nil
	}
	hits := must[[]store.Hit](t)(s.Nearest("docs", []float32{1, 1, 1}, store.MaxK, keep))
	if len(seen) != withVectors {
		t.Fatalf("the filter saw %d records, of the %d with vectors: %v", len(seen), withVectors, seen)
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("the filter saw %s %d times", key, n)
		}
	}
	even := 0
	for key := range seen {
		if n, _ := strconv.Atoi(key[len("docs:"):]); n%2 == 0 {
			even++
		}
	}
	for _, h := range hits {
		if n, _ := strconv.Atoi(h.Key[len("docs:"):]); n%2 != 0 {
			t.Fatalf("the search gave %s, which the filter ruled out", h.Key)
		}
	}
	if len(hits) != even || even != 19 {
		t.Fatalf("the search gave %d hits, of the %d the filter let through: %v", len(hits), even, hits)
	}
	// With a k that only a few records make, it's called for each record
	// all the same.
	calls := 0
	count := func(store.Record) (bool, error) { calls++; return true, nil }
	if hits := must[[]store.Hit](t)(s.Nearest("docs", []float32{1, 1, 1}, 3, count)); len(hits) != 3 || calls != withVectors {
		t.Fatalf("a search for 3 called the filter %d times, of the %d records with vectors, and gave %v", calls, withVectors, hits)
	}
	errStop := errors.New("stop here")
	calls = 0
	_, err := s.Nearest("docs", []float32{1, 1, 1}, 5, func(store.Record) (bool, error) { calls++; return true, errStop })
	if err != errStop || calls != 1 {
		t.Fatalf("a filter that failed gave %v after %d calls", err, calls)
	}
	calls = 0
	for _, c := range []struct {
		table string
		q     []float32
		k     int
	}{{"plain", []float32{1}, 1}, {"docs", []float32{1, 1}, 1}, {"docs", []float32{1, 1, 1}, 0}, {"nosuch", []float32{1}, 1}, {"Docs", []float32{1}, 1}} {
		s.Nearest(c.table, c.q, c.k, count)
	}
	if calls != 0 {
		t.Fatalf("searches that had nothing to compare called the filter %d times", calls)
	}
}
