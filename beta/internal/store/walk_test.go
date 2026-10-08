// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// TestWalksAgreeWithAReference walks random graphs with the store's Walk
// and with referenceWalk, which works from the set of links alone, and the
// two must give the same steps in the same order. Neighbours must give the
// model's links. The graphs come in five shapes: sparse, dense, a chain
// with a few links off it, a hub that every record links to and from, and
// a ring, with up to three tables whose keys sort across each other's, up
// to three types, links from records to themselves and links of several
// types between two records. Each graph is walked from many records, in
// every direction, with every type, no type, and types no link has, to
// depths from 1 to 32. Then records are deleted, links taken out and a
// table dropped, and the graph is walked again; then the same happens
// inside a transaction, walked through the transaction, and again after it
// rolls back; and last, the graph is walked in a store that loaded the
// first one's snapshot.
func TestWalksAgreeWithAReference(t *testing.T) {
	seeds := 150
	if testing.Short() {
		seeds = 30
	}
	outcomes := map[string]int{}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { walkGraph(t, seed, outcomes) })
	}
	for _, o := range []string{"sparse", "dense", "chain", "hub", "ring", "walk", "walk found some", "walk to depth 10 or more",
		"walk with a type", "walk through a transaction", "walk after a rollback", "walk of a loaded snapshot",
		"a list in blocks"} {
		if outcomes[o] == 0 {
			t.Errorf("%q never came up: %v", o, outcomes)
		}
	}
	if testing.Verbose() {
		t.Logf("outcomes: %v", outcomes)
	}
}

func walkGraph(t *testing.T, seed uint64, outcomes map[string]int) {
	r := rand.New(rand.NewPCG(seed, 0x56))
	tables := []string{"a", "a2", "b"}[:1+r.IntN(3)]
	types := []string{"x", "Y", "y"}[:1+r.IntN(3)]
	shape := []string{"sparse", "dense", "chain", "hub", "ring"}[seed%5]
	outcomes[shape]++
	n := 1 + r.IntN(60)
	if shape == "hub" || shape == "chain" {
		n = 50 + r.IntN(400)
	}
	keys := make([]string, n)
	for i := range keys {
		keys[i] = tables[r.IntN(len(tables))] + ":" + strconv.Itoa(i)
	}
	s, m := New(), newModel()
	for _, k := range keys {
		if _, err := s.Put(nil, k, nil); err != nil {
			t.Fatal(err)
		}
		m.put(k, nil, false)
	}
	link := func(from, typ, to string) {
		t.Helper()
		changes, err := s.Link(nil, from, typ, to)
		if err != nil {
			t.Fatalf("Link(%s, %s, %s): %v", from, typ, to, err)
		}
		if _, added := m.link(linkArgs{from: from, typ: typ, to: to}); added != (len(changes) == 1) {
			t.Fatalf("Link(%s, %s, %s) gave %v, where the model added it: %v", from, typ, to, changes, added)
		}
	}
	key := func() string { return keys[r.IntN(n)] }
	typ := func() string { return types[r.IntN(len(types))] }
	switch shape {
	case "sparse":
		for range n / 2 {
			link(key(), typ(), key())
		}
	case "dense":
		for range 3 * n {
			link(key(), typ(), key())
		}
	case "chain":
		for i := 1; i < n; i++ {
			link(keys[i-1], typ(), keys[i])
		}
		for range n / 10 {
			link(key(), typ(), key())
		}
	case "hub":
		hub := keys[0]
		for _, k := range keys {
			link(k, typ(), hub)
			if r.IntN(2) == 0 {
				link(hub, typ(), k)
			}
		}
		for range n / 4 {
			link(key(), typ(), key())
		}
	case "ring":
		for i := range keys {
			link(keys[i], typ(), keys[(i+1)%n])
			if r.IntN(3) == 0 {
				link(keys[i], typ(), keys[i]) // a link to itself
			}
		}
	}
	for _, rec := range s.records {
		if len(rec.out.blocks) > 1 || len(rec.in.blocks) > 1 {
			outcomes["a list in blocks"]++
		}
	}
	walks := func(rd Reader, m *model, keys []string, what string) {
		t.Helper()
		for range 25 {
			start := keys[r.IntN(len(keys))]
			dir := Direction(r.IntN(3))
			typ := ""
			switch r.IntN(4) {
			case 0:
				typ = types[r.IntN(len(types))]
				outcomes["walk with a type"]++
			case 1:
				typ = "z" // no link has it
			}
			depth := 1 + r.IntN(4)
			if r.IntN(4) == 0 {
				depth = 1 + r.IntN(MaxDepth)
			}
			got, err := rd.Walk(start, dir, typ, depth)
			want, kind := m.walk(start, false, dir, typ, depth)
			if kindOf(err) != kind || !slices.Equal(got, want) || got != nil && len(got) == 0 {
				t.Fatalf("%s: Walk(%s, %v, %q, %d) gave %v, %v, where the reference gives %v, %q", what, start, dir, typ, depth, got, err, want, kind)
			}
			outcomes[what]++
			if len(got) > 0 {
				outcomes["walk found some"]++
				if got[len(got)-1].Depth >= 10 {
					outcomes["walk to depth 10 or more"]++
				}
			}
			links, err := rd.Neighbours(start, dir, typ)
			wantLinks, kind := m.neighbours(start, false, dir, typ)
			if kindOf(err) != kind || !slices.Equal(links, wantLinks) {
				t.Fatalf("%s: Neighbours(%s, %v, %q) gave %v, %v, where the model gives %v, %q", what, start, dir, typ, links, err, wantLinks, kind)
			}
		}
	}
	live := func(m *model) []string { return slices.Sorted(maps.Keys(m.records)) }
	walks(s, m, keys, "walk")

	// Records deleted, links taken out and a table dropped.
	changes := func(s *Store, m *model, w writer) {
		t.Helper()
		for range 1 + n/10 {
			k := keys[r.IntN(n)]
			if m.records[k] == nil {
				continue
			}
			if err := w.Delete(k); err != nil {
				t.Fatal(err)
			}
			m.delete(k, false)
		}
		for _, l := range m.sortedLinks() {
			if r.IntN(8) == 0 {
				typ := l.Type
				if r.IntN(3) == 0 {
					typ = ""
				}
				// An unlink of every type may have taken l already.
				want, _ := m.unlink(l.From, typ, l.To)
				if err := w.Unlink(l.From, typ, l.To); kindOf(err) != want {
					t.Fatalf("Unlink(%s, %q, %s) gave %v, where the model says %q", l.From, typ, l.To, err, want)
				}
			}
		}
		if len(tables) > 1 && r.IntN(2) == 0 {
			tbl := tables[r.IntN(len(tables))]
			if m.tables[tbl] != nil {
				if err := w.Drop(tbl); err != nil {
					t.Fatal(err)
				}
				m.drop(tbl, false)
			}
		}
		checkInvariants(t, s)
		compareAll(t, s, m)
	}
	changes(s, m, directWriter{s})
	if len(m.records) == 0 {
		return
	}
	walks(s, m, live(m), "walk")

	// The same inside a transaction, which a rollback takes back.
	before := dump(s)
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	inTx := m.clone()
	changes(s, inTx, tx)
	if len(inTx.records) > 0 {
		walks(tx, inTx, live(inTx), "walk through a transaction")
	}
	tx.Rollback()
	if dump(s) != before {
		t.Fatal("the rollback didn't put the store back as it was")
	}
	checkInvariants(t, s)
	walks(s, m, live(m), "walk after a rollback")

	// A store that loads the snapshot walks as this one does.
	c := New()
	var snap []format.Change
	for ch := range s.Snapshot() {
		ch.Names, ch.Fields = slices.Clone(ch.Names), slices.Clone(ch.Fields)
		snap = append(snap, ch)
	}
	if err := c.LoadBatch(1, snap); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, c)
	walks(c, m, live(m), "walk of a loaded snapshot")
}

// TestWalkTimings times walks one, two and three links out of 100,000
// records with 5 random links out each, of one type, as 0.1's benchmark
// builds its graph, and prints the times, for G2 to check against BETA.md's
// targets of 3 µs one link out and 40 µs three links out. Each walk starts
// from a record picked at random, as 0.1's benchmark picks it. It also
// times loading the links through LoadBatch, as opening a compacted file
// does, and reads of a record's links, and gives the memory the links take,
// from the heap's size before and after. It checks the results against the
// reference walk for a few of the walks, and nothing about the times, which
// depend on the machine, and on what else runs on it.
//
// The times show only with -v, so it takes 100,000 records only then, and
// without -short. Otherwise it takes 10,000, which checks the same results
// in a tenth of the time, since every job that runs the store's tests, the
// one for planted bugs among them, runs it.
func TestWalkTimings(t *testing.T) {
	n, walks := 10_000, 500
	if testing.Verbose() && !testing.Short() {
		n, walks = 100_000, 5_000
	}
	r := rand.New(rand.NewPCG(uint64(n), 6))
	s := New()
	batch := []format.Change{{Op: format.CreateTable, Table: "node", Names: []string{"n"}}}
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "node:" + strconv.Itoa(i)
		batch = append(batch, format.Change{Op: format.Put, Key: keys[i]})
	}
	if err := s.LoadBatch(1, batch); err != nil {
		t.Fatal(err)
	}
	var links []format.Change
	set := map[Link]bool{}
	for i := range n {
		for range 5 {
			l := Link{keys[i], "to", keys[r.IntN(n)]}
			if !set[l] { // 0.1 makes them with INSERT OR IGNORE
				set[l] = true
				links = append(links, format.Change{Op: format.Link, Key: l.From, Type: l.Type, To: l.To})
			}
		}
	}
	// In the snapshot's order, as a compacted file holds them.
	slices.SortFunc(links, func(a, b format.Change) int {
		return cmp.Or(strings.Compare(a.Key, b.Key), strings.Compare(a.Type, b.Type), strings.Compare(a.To, b.To))
	})
	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	start := time.Now()
	if err := s.LoadBatch(2, links); err != nil {
		t.Fatal(err)
	}
	load := time.Since(start)
	after := heap()
	runtime.KeepAlive(links)
	t.Logf("%d records, %d links of one type, in a compacted file's order: loaded in %v, %.0f ns a link", n, len(links), load.Round(time.Millisecond),
		float64(load.Nanoseconds())/float64(len(links)))
	t.Logf("the links' halves take %.1f MB, %.1f bytes a link, besides the 48 bytes in every record for its two lists",
		float64(after-before)/1e6, float64(after-before)/float64(len(links)))

	for _, depth := range []int{1, 2, 3} {
		starts := make([]string, walks)
		for i := range starts {
			starts[i] = keys[r.IntN(n)]
		}
		reached := 0
		start := time.Now()
		for _, k := range starts {
			steps, err := s.Walk(k, Out, "", depth)
			if err != nil {
				t.Fatal(err)
			}
			reached += len(steps)
		}
		d := time.Since(start)
		t.Logf("a walk %s out: %.2f µs, reaching %.1f records", []string{"", "one link", "two links", "three links"}[depth],
			float64(d.Nanoseconds())/float64(walks)/1e3, float64(reached)/float64(walks))
		for _, k := range starts[:3] {
			got, _ := s.Walk(k, Out, "", depth)
			if want := referenceWalk(set, k, Out, "", depth); !slices.Equal(got, want) {
				t.Fatalf("Walk(%s, out, %d) gave %v, where the reference gives %v", k, depth, got, want)
			}
		}
	}
	start = time.Now()
	found := 0
	for i := range walks {
		l, err := s.Neighbours(keys[(i*7919)%n], Both, "")
		if err != nil {
			t.Fatal(err)
		}
		found += len(l)
	}
	d := time.Since(start)
	t.Logf("a record's links both ways: %.2f µs, %.1f links", float64(d.Nanoseconds())/float64(walks)/1e3, float64(found)/float64(walks))
	checkInvariants(t, s)
}
