// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestKeyOrder runs inserts and removes on a table's keys and on a sorted
// list, at sizes where blocks split and join. Keys go in at random, then in
// runs after every key, as a compacted file gives them, before every key,
// and into one narrow range, and come out at random and in runs, down to
// none. Some inserts are of keys already there, and some removes of keys
// that aren't. Each must report what the list says, and every so often the
// layout must hold, the keys must be the list's, and find must give the
// list's place for keys and for strings between them, at and past them.
func TestKeyOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 0x54))
	var o keyOrder
	var list []string // the keys, sorted
	splits, joins := 0, 0
	insert := func(key string) {
		blocks := len(o.blocks)
		i, found := slices.BinarySearch(list, key)
		if got := o.insert(&record{key: key}); got == found {
			t.Fatalf("insert(%q) gave %v, with the key there: %v", key, got, found)
		}
		if !found {
			list = slices.Insert(list, i, key)
			if len(o.blocks) > blocks && blocks > 0 && i < len(list)-1 {
				splits++
			}
		}
	}
	remove := func(key string) {
		blocks := len(o.blocks)
		i, found := slices.BinarySearch(list, key)
		if got := o.remove(key); got != found {
			t.Fatalf("remove(%q) gave %v, with the key there: %v", key, got, found)
		}
		if found {
			list = slices.Delete(list, i, i+1)
			if len(o.blocks) < blocks && o.n > 0 {
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
		checkOrder(t, "t", &o)
		var got []string
		for rec := range o.records {
			got = append(got, rec.key)
		}
		if !slices.Equal(got, list) {
			t.Fatalf("%s: the order holds %d keys, and the list %d: %q", phase, len(got), len(list), got)
		}
		for range 20 {
			checkFind(t, &o, list, probe(r, list))
		}
		// Each block's first key, inserted again with the hint on the
		// block before it, is there already.
		for j := 1; j < len(o.blocks); j++ {
			o.hint = j - 1
			if key := o.blocks[j][0].key; o.insert(&record{key: key}) {
				t.Fatalf("%s: block %d's first key %q went in again", phase, j, key)
			}
		}
	}
	id := func(n int) string { return fmt.Sprintf("t:%05d", r.IntN(n)) }
	for range 4000 {
		insert(id(8000))
		check("random inserts")
	}
	for range 3000 {
		remove(id(8000))
		check("random removes")
	}
	for i := range 1500 { // after every key
		insert(fmt.Sprintf("t:9%05d", i))
		check("a run after every key")
	}
	for i := 1500; i > 0; i-- { // before every key
		insert(fmt.Sprintf("t:!%05d", i))
		check("a run before every key")
	}
	for range 1500 { // one narrow range, so the same blocks split again and again
		insert(fmt.Sprintf("t:04000.%05d", r.IntN(100000)))
		check("a narrow range")
	}
	for len(list) > 600 { // runs of neighbours, so blocks empty and join
		at := r.IntN(len(list))
		for _, key := range slices.Clone(list[at:min(at+r.IntN(300), len(list))]) {
			remove(key)
			check("runs of removes")
		}
	}
	for len(list) > 0 {
		remove(list[r.IntN(len(list))])
		check("removes down to none")
	}
	check("")
	if o.blocks != nil || o.n != 0 {
		t.Fatalf("with no keys left, the order has %d blocks and counts %d keys", len(o.blocks), o.n)
	}
	if splits < 20 || joins < 20 {
		t.Fatalf("blocks split %d times and joined %d times, which tests less than it should", splits, joins)
	}
	if testing.Verbose() {
		t.Logf("blocks split %d times and joined %d times", splits, joins)
	}
}

// TestKeyOrderJoins pins when blocks join: a block that falls below a
// quarter of blockMax keys joins its smaller neighbour when the two hold
// three quarters of a block at most, and otherwise stays as it is, and a
// block that a run after every key fills is full.
func TestKeyOrderJoins(t *testing.T) {
	var o keyOrder
	for i := range 4 * blockMax {
		o.insert(&record{key: fmt.Sprintf("t:%05d", i)})
	}
	sizes := func() []int {
		var n []int
		for _, b := range o.blocks {
			n = append(n, len(b))
		}
		return n
	}
	want := func(what string, n ...int) {
		t.Helper()
		checkOrder(t, "t", &o)
		if !slices.Equal(sizes(), n) {
			t.Fatalf("%s: the blocks hold %v keys, where they should hold %v", what, sizes(), n)
		}
	}
	q := blockMax / 4
	want("keys after every key", blockMax, blockMax, blockMax, blockMax)
	from := func(bi, n int) { // takes n keys out of block bi, from its end
		for range n {
			b := o.blocks[bi]
			o.remove(b[len(b)-1].key)
		}
	}
	from(0, blockMax-q)
	want("the first block down to a quarter", q, blockMax, blockMax, blockMax)
	from(0, 1)
	want("the first block below a quarter, beside a full one", q-1, blockMax, blockMax, blockMax)
	from(1, blockMax-2*q-1)
	want("its neighbour cut to half", q-1, 2*q+1, blockMax, blockMax)
	from(0, 1)
	want("the two fit in three quarters", 3*q-1, blockMax, blockMax)
	// A block in the middle joins the smaller of its two neighbours.
	from(0, 3*q-1-3*q/2)
	from(2, blockMax-5*q/4)
	want("both neighbours of block 1 cut", 3*q/2, blockMax, 5*q/4)
	from(1, blockMax-q)
	want("block 1 down to a quarter", 3*q/2, q, 5*q/4)
	from(1, 1)
	want("block 1 joined block 2, the smaller", 3*q/2, q-1+5*q/4)
}

// probe returns a string to find: a key, one just before or after a key, a
// key cut short, or one before or after every key.
func probe(r *rand.Rand, list []string) string {
	if len(list) == 0 || r.IntN(10) == 0 {
		return []string{"", "t:", "t;", "u", "\xff"}[r.IntN(5)]
	}
	key := list[r.IntN(len(list))]
	switch r.IntN(4) {
	case 0:
		return key + "\x00"
	case 1:
		return key[:len(key)-1]
	case 2:
		return key[:len(key)-1] + string(key[len(key)-1]-1) + "\xff"
	}
	return key
}

// checkFind checks find's place for key, at it and past it, against the
// sorted list.
func checkFind(t *testing.T, o *keyOrder, list []string, key string) {
	t.Helper()
	for _, past := range []bool{false, true} {
		want := sort.SearchStrings(list, key)
		if past && want < len(list) && list[want] == key {
			want++
		}
		bi, i := o.find(key, past)
		switch {
		case bi == len(o.blocks):
			if i != 0 || want != len(list) {
				t.Fatalf("find(%q, %v) gave place %d past the end, where the list has %d of %d", key, past, i, want, len(list))
			}
		case bi > len(o.blocks) || i >= len(o.blocks[bi]):
			t.Fatalf("find(%q, %v) gave block %d place %d, of %d blocks", key, past, bi, i, len(o.blocks))
		case want == len(list) || o.blocks[bi][i].key != list[want]:
			t.Fatalf("find(%q, %v) gave %q, where the list has %d of %d", key, past, o.blocks[bi][i].key, want, len(list))
		}
	}
}

// TestUndoManyKeys: a transaction that takes enough keys out of a table
// and puts enough in to join and split its blocks, through deletes, puts, a
// drop and a table made again, and a statement taken back, puts back each
// table's keys when it rolls back, and keeps them when it commits. A cursor
// through the transaction stays open the whole time, and each record it
// gives must be the next in the model as the transaction has left it.
func TestUndoManyKeys(t *testing.T) {
	s, m := New(), newModel()
	r := rand.New(rand.NewPCG(5, 0x54))
	put := func(w writer, m *model, key string) {
		t.Helper()
		f := []format.Field{{Name: "n", Value: value.Int(int64(len(key)))}}
		ok(t, w.Put(key, f))
		m.put(key, f, false)
	}
	direct := directWriter{s}
	for _, i := range r.Perm(3000) {
		put(direct, m, "docs:"+strconv.Itoa(i))
	}
	for i := range 500 {
		put(direct, m, "notes:"+strconv.Itoa(i))
	}
	before := dump(s)
	outcomes := map[string]int{}
	for _, end := range []string{"rollback", "commit"} {
		tx := begin(t, s)
		work := m.clone()
		live := &liveScan{c: must(tx.Scan("docs:", "")), desc: "a scan of docs through the transaction", prefix: "docs:"}
		pull := func(n int) {
			for range n {
				if !live.pull(t, tx, work, outcomes) {
					t.Fatalf("%s ended with %d records given", live.desc, live.given)
				}
			}
		}
		pull(10)
		for _, i := range r.Perm(3000)[:2000] {
			key := "docs:" + strconv.Itoa(i)
			ok(t, tx.Delete(key))
			work.delete(key, false)
			if r.IntN(50) == 0 {
				pull(1)
			}
		}
		for i := range 2000 {
			put(tx, work, fmt.Sprintf("docs:%d.%d", r.IntN(3000), i))
			if r.IntN(50) == 0 {
				pull(1)
			}
		}
		mark, kept := tx.Mark(), work.clone()
		for i := range 500 {
			put(tx, work, fmt.Sprintf("docs:5.%d", i))
		}
		ok(t, tx.Delete("docs:5.0"))
		tx.RollbackTo(mark)
		work = kept
		pull(5)
		ok(t, tx.Drop("notes"))
		work.drop("notes", false)
		for i := range 300 {
			put(tx, work, "notes:x"+strconv.Itoa(i))
		}
		checkInvariants(t, s)
		compareAll(t, s, work)
		pull(5)
		checkScan(t, tx, work, live.c, live.desc, work.scan(live.prefix, live.after), -1)
		after := dump(s)
		if end == "rollback" {
			tx.Rollback()
			checkInvariants(t, s)
			if got := dump(s); got != before {
				t.Fatalf("after the rollback the store holds\n%.2000s\nwhere it held\n%.2000s", got, before)
			}
			compareAll(t, s, m)
			continue
		}
		ok(t, tx.Commit(nil))
		checkInvariants(t, s)
		if got := dump(s); got != after {
			t.Fatal("after the commit the store differs from what the transaction left")
		}
		compareAll(t, s, work)
	}
	if outcomes["live scan moved"] < 50 {
		t.Fatalf("the cursor found its place again %d times", outcomes["live scan moved"])
	}
}

// directWriter writes straight to a store, as the writer interface does
// for a transaction.
type directWriter struct{ s *Store }

func (w directWriter) Put(key string, fields []format.Field) error {
	_, err := w.s.Put(nil, key, fields)
	return err
}
func (w directWriter) Delete(key string) error     { _, err := w.s.Delete(nil, key); return err }
func (w directWriter) Drop(name string) error      { _, err := w.s.Drop(nil, name); return err }
func (w directWriter) Apply(c format.Change) error { return w.s.Apply(c) }
func (w directWriter) Link(from, typ, to string) error {
	_, err := w.s.Link(nil, from, typ, to)
	return err
}
func (w directWriter) Unlink(from, typ, to string) error {
	_, err := w.s.Unlink(nil, from, typ, to)
	return err
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestKeyOrderTimings times a table's keys at the size opening has to load
// within its target, 100,000 keys, or 20,000 with -short, and prints the
// times with -v. It times the keys going in in byte order, as a compacted
// file brings them; in the order of their numbers, docs:0, docs:1 and so
// on, as a program counting up puts them and the log then brings them; and
// in a random order. Then it times a scan of them all, and the keys going
// out in a random order. Last, it times the store loading as many records
// of two fields each through LoadBatch, as opening does, in byte order and
// in a random order, which puts the keys among everything else a put does.
// It checks the results, and nothing about the times, which depend on the
// machine. It runs once, as every test does.
func TestKeyOrderTimings(t *testing.T) {
	n := 100_000
	if testing.Short() {
		n = 20_000
	}
	r := rand.New(rand.NewPCG(6, 0x54))
	counted := make([]*record, n)
	for i := range counted {
		counted[i] = &record{key: "docs:" + strconv.Itoa(i)}
	}
	sorted := slices.Clone(counted)
	slices.SortFunc(sorted, func(a, b *record) int { return strings.Compare(a.key, b.key) })
	shuffled := slices.Clone(sorted)
	r.Shuffle(n, func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	each := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / float64(n) }

	var random, inOrder, byNumber keyOrder
	start := time.Now()
	for _, rec := range sorted {
		inOrder.insert(rec)
	}
	dInOrder := time.Since(start)
	start = time.Now()
	for _, rec := range counted {
		byNumber.insert(rec)
	}
	dByNumber := time.Since(start)
	start = time.Now()
	for _, rec := range shuffled {
		random.insert(rec)
	}
	dRandom := time.Since(start)
	start = time.Now()
	scanned := 0
	for rec := range random.records {
		if rec != sorted[scanned] {
			t.Fatalf("record %d of the scan is %s, where it should be %s", scanned, rec.key, sorted[scanned].key)
		}
		scanned++
	}
	dScan := time.Since(start)
	checkOrder(t, "random", &random)
	checkOrder(t, "in order", &inOrder)
	checkOrder(t, "by number", &byNumber)
	if scanned != n || inOrder.n != n || byNumber.n != n {
		t.Fatalf("the orders hold %d, %d and %d keys, of %d", random.n, inOrder.n, byNumber.n, n)
	}
	blocksRandom, blocksInOrder, blocksByNumber := len(random.blocks), len(inOrder.blocks), len(byNumber.blocks)
	r.Shuffle(n, func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	start = time.Now()
	for _, rec := range shuffled {
		random.remove(rec.key)
	}
	dRemove := time.Since(start)
	if random.n != 0 || random.blocks != nil {
		t.Fatalf("after every remove, the order holds %d keys in %d blocks", random.n, len(random.blocks))
	}
	t.Logf("%d keys in byte order: %v, %.0f ns a key, into %d blocks", n, dInOrder.Round(time.Microsecond), each(dInOrder), blocksInOrder)
	t.Logf("%d keys in the order of their numbers: %v, %.0f ns a key, into %d blocks", n, dByNumber.Round(time.Microsecond), each(dByNumber), blocksByNumber)
	t.Logf("%d keys in a random order: %v, %.0f ns a key, into %d blocks", n, dRandom.Round(time.Microsecond), each(dRandom), blocksRandom)
	t.Logf("a scan of %d keys: %v, %.1f ns a key", n, dScan.Round(time.Microsecond), each(dScan))
	t.Logf("%d keys out in a random order: %v, %.0f ns a key", n, dRemove.Round(time.Microsecond), each(dRemove))

	for _, order := range []string{"byte order", "a random order"} {
		recs := sorted
		if order != "byte order" {
			recs = shuffled
		}
		batch := []format.Change{{Op: format.CreateTable, Table: "docs", Names: []string{"n", "title"}}}
		for i, rec := range recs {
			batch = append(batch, format.Change{Op: format.Put, Key: rec.key, Fields: []format.Field{
				{Name: "n", Value: value.Int(int64(i))}, {Name: "title", Value: value.Text("a title")},
			}})
		}
		s := New()
		start := time.Now()
		ok(t, s.LoadBatch(1, batch))
		d := time.Since(start)
		if tb := s.tables["docs"]; tb.keys.n != n || len(s.records) != n {
			t.Fatalf("the store loaded %d records and %d keys, of %d", len(s.records), tb.keys.n, n)
		}
		t.Logf("the store loads %d records in %s: %v, %.0f ns a record", n, order, d.Round(time.Microsecond), each(d))
	}
}
