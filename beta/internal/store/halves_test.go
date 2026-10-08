// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// TestLinkLists runs inserts and removes on one record's list of links and
// on a sorted list of places, at sizes where blocks split and join. Halves
// of four types go in at random, then in runs after every half, as a
// compacted file gives a record's links out, before every half, and into
// one narrow range, and come out at random and in runs, down to none. Each
// must report what the sorted list says, and every so often the layout must
// hold, the halves must be the list's, and get, first and ofType must agree
// with the list, for the types there and for types between them.
func TestLinkLists(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 0x55))
	var l links
	var list []linkAt // the places, sorted
	records := map[string]*record{}
	recordOf := func(key string) *record {
		if records[key] == nil {
			records[key] = &record{key: key}
		}
		return records[key]
	}
	types := []linkType{makeType("a"), makeType("b b"), makeType("cites"), makeType("é")}
	splits, joins := 0, 0
	insert := func(typ linkType, key string) {
		at := linkAt{typ.Value(), key}
		i, found := slices.BinarySearchFunc(list, at, compareAt)
		if _, there := l.get(at); there != found {
			t.Fatalf("get(%q) gave %v, with the half there: %v", at, there, found)
		}
		if found {
			return
		}
		blocks := len(l.blocks)
		l.insert(half{typ, recordOf(key)})
		list = slices.Insert(list, i, at)
		if len(l.blocks) > blocks && blocks > 0 && i < len(list)-1 {
			splits++
		}
	}
	remove := func(at linkAt) {
		blocks := len(l.blocks)
		i, found := slices.BinarySearchFunc(list, at, compareAt)
		if got := l.remove(at); got != found {
			t.Fatalf("remove(%q) gave %v, with the half there: %v", at, got, found)
		}
		if found {
			list = slices.Delete(list, i, i+1)
			if len(l.blocks) < blocks && len(list) > 0 {
				joins++
			}
		}
	}
	steps := 0
	check := func(phase string) {
		t.Helper()
		if steps++; steps%97 != 0 && phase != "" {
			return
		}
		if got := checkHalves(t, phase, &l); !slices.Equal(got, list) {
			t.Fatalf("%s: the list holds %d halves, and the sorted list %d", phase, len(got), len(list))
		}
		if l.len() != len(list) {
			t.Fatalf("%s: len gives %d, of %d", phase, l.len(), len(list))
		}
		for _, typ := range []string{"", "a", "a ", "b b", "c", "cites", "citesa", "é", "\xff"} {
			want := slices.IndexFunc(list, func(at linkAt) bool { return at.typ >= typ })
			if want < 0 {
				want = len(list)
			}
			bi, i := l.first(typ)
			got := 0
			for _, b := range l.blocks[:bi] {
				got += len(b)
			}
			if got += i; got != want || bi < len(l.blocks) && i >= len(l.blocks[bi]) || bi == len(l.blocks) && i != 0 {
				t.Fatalf("%s: first(%q) gave block %d place %d, the %dth half, where the sorted list has %d", phase, typ, bi, i, got, want)
			}
			var of []linkAt
			for h := range l.ofType(typ) {
				of = append(of, h.at())
			}
			var wantOf []linkAt
			for _, at := range list {
				if typ == "" || at.typ == typ {
					wantOf = append(wantOf, at)
				}
			}
			if !slices.Equal(of, wantOf) {
				t.Fatalf("%s: ofType(%q) gave %d halves, where the sorted list has %d", phase, typ, len(of), len(wantOf))
			}
		}
	}
	key := func(n int) string { return fmt.Sprintf("t:%05d", r.IntN(n)) }
	for range 5000 {
		insert(types[r.IntN(len(types))], key(3000))
		check("random inserts")
	}
	for range 3000 {
		remove(linkAt{types[r.IntN(len(types))].Value(), key(3000)})
		check("random removes")
	}
	for i := range 1500 { // after every half
		insert(types[len(types)-1], fmt.Sprintf("u:%05d", i))
		check("a run after every half")
	}
	for i := 1500; i > 0; i-- { // before every half
		insert(types[0], fmt.Sprintf("a:%05d", i))
		check("a run before every half")
	}
	for range 1500 { // one narrow range, so the same blocks split again and again
		insert(types[2], fmt.Sprintf("t:01000.%05d", r.IntN(100000)))
		check("a narrow range")
	}
	for len(list) > 600 { // runs of neighbours, so blocks empty and join
		at := r.IntN(len(list))
		for _, place := range slices.Clone(list[at:min(at+r.IntN(300), len(list))]) {
			remove(place)
			check("runs of removes")
		}
	}
	for len(list) > 0 {
		remove(list[r.IntN(len(list))])
		check("removes down to none")
	}
	check("")
	if l.blocks != nil {
		t.Fatalf("with no halves left, the list has %d blocks", len(l.blocks))
	}
	if splits < 20 || joins < 20 {
		t.Fatalf("blocks split %d times and joined %d times, which tests less than it should", splits, joins)
	}
	if testing.Verbose() {
		t.Logf("blocks split %d times and joined %d times", splits, joins)
	}
}

func compareAt(a, b linkAt) int {
	return cmp.Or(strings.Compare(a.typ, b.typ), strings.Compare(a.key, b.key))
}

// checkHalves checks the layout of a list of links: blocks of 1 to halfMax
// halves, in byte order of type and then of key across the blocks, each
// half with a type and a record, and nothing left past a block's length. It
// returns the halves' places, in order.
func checkHalves(t *testing.T, name string, l *links) []linkAt {
	var places []linkAt
	for bi, b := range l.blocks {
		if len(b) == 0 || cap(b) > halfMax {
			t.Fatalf("%s: block %d holds %d halves, with room for %d", name, bi, len(b), cap(b))
		}
		for _, h := range b {
			if h.other == nil || h.typ == (linkType{}) {
				t.Fatalf("%s: block %d holds %+v", name, bi, h)
			}
			at := h.at()
			if n := len(places); n > 0 && compareAt(places[n-1], at) >= 0 {
				t.Fatalf("%s: block %d holds %q after %q", name, bi, at, places[n-1])
			}
			places = append(places, at)
		}
		for _, h := range b[len(b):cap(b)] {
			if h != (half{}) {
				t.Fatalf("%s: block %d keeps %q past its length", name, bi, h.at())
			}
		}
	}
	if l.blocks != nil && len(l.blocks) == 0 {
		t.Fatalf("%s: an empty list of blocks", name)
	}
	return places
}

// TestLinkListBlocks pins how a list's blocks grow, split and join: a block
// of up to 8 halves has just the room it needs, and doubles from there; a
// full block splits into halves; a block that falls below a quarter of
// halfMax halves joins its smaller neighbour when the two hold three
// quarters of a block at most, and otherwise stays as it is.
func TestLinkListBlocks(t *testing.T) {
	typ := makeType("t")
	var l links
	h := func(i int) half { return half{typ, &record{key: fmt.Sprintf("t:%05d", i)}} }
	for i := range smallBlock {
		l.insert(h(i))
		if b := l.blocks[0]; len(l.blocks) != 1 || cap(b) != i+1 {
			t.Fatalf("a list of %d halves has %d blocks, the first with room for %d", i+1, len(l.blocks), cap(b))
		}
	}
	l.insert(h(smallBlock))
	if c := cap(l.blocks[0]); c != 2*smallBlock {
		t.Fatalf("a block past %d halves has room for %d", smallBlock, c)
	}
	for i := smallBlock + 1; i < 4*halfMax; i++ {
		l.insert(h(i))
	}
	sizes := func() []int {
		var n []int
		for _, b := range l.blocks {
			n = append(n, len(b))
		}
		return n
	}
	want := func(what string, n ...int) {
		t.Helper()
		checkHalves(t, what, &l)
		if !slices.Equal(sizes(), n) {
			t.Fatalf("%s: the blocks hold %v halves, where they should hold %v", what, sizes(), n)
		}
	}
	want("halves after every half", halfMax, halfMax, halfMax, halfMax)
	from := func(bi, n int) { // takes n halves out of block bi, from its end
		for range n {
			b := l.blocks[bi]
			l.remove(b[len(b)-1].at())
		}
	}
	q := halfMax / 4
	from(0, halfMax-q)
	want("the first block down to a quarter", q, halfMax, halfMax, halfMax)
	from(0, 1)
	want("the first block below a quarter, beside a full one", q-1, halfMax, halfMax, halfMax)
	from(1, halfMax-2*q-1)
	want("its neighbour cut to half", q-1, 2*q+1, halfMax, halfMax)
	from(0, 1)
	want("the two fit in three quarters", 3*q-1, halfMax, halfMax)
	// A full block splits into halves when a half goes into it.
	l.insert(half{typ, &record{key: "t:00600x"}})
	want("a full block split", 3*q-1, halfMax/2+1, halfMax/2, halfMax)
}

// TestCompareAsKeys: tables' names compare as their records' keys do, each
// name with a colon after it, both ways round.
func TestCompareAsKeys(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"users", "users2", 1}, {"users", "users_x", -1}, {"users", "usersa", -1}, {"a", "ab", -1}, {"a", "a0", 1},
		{"docs", "docs", 0}, {"docs", "notes", -1}, {"b", "a_b", 1},
	} {
		if got := compareAsKeys(c.a, c.b); got != c.want {
			t.Errorf("compareAsKeys(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareAsKeys(c.b, c.a); got != -c.want {
			t.Errorf("compareAsKeys(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
		if want := strings.Compare(c.a+":", c.b+":"); want != c.want {
			t.Fatalf("the case %q, %q is wrong: with colons they compare as %d", c.a, c.b, want)
		}
	}
}
