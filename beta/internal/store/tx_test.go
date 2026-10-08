// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// TestTransactions is S2's closing test. Random transactions run on the
// store and on the model, with random rollbacks: of whole transactions, of
// statements taken back to a mark as SQL's are, and of commits whose write
// to the log fails. Inside them go random puts, links, unlinks, deletes,
// drops and creates, many of which fail and must leave the transaction as
// it was, and reads through the transaction, which must see its changes:
// gets, scans, a cursor kept open across the transaction's later steps and
// the statements it takes back, which must give no more once the
// transaction ends, reads of links and walks. Every answer must agree with
// the model's. After each transaction the store must hold what the model
// holds, and so must a replica that applies each committed change list.
// Readers run alongside the whole time, getting, scanning and walking
// everything, and every read must see a state that a commit left. CI runs
// it under the race detector too.
func TestTransactions(t *testing.T) {
	seeds, txs := 12, 300
	if testing.Short() {
		seeds, txs = 3, 150
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runTransactions(t, seed, txs) })
	}
}

// readers is how many goroutines read alongside the transactions.
const readers = 2

func runTransactions(t *testing.T, seed uint64, n int) {
	s, replica := New(), New()
	committed := newModel()
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x52)), m: committed, side: rand.New(rand.NewPCG(seed, 0x62))}
	seen := &states{seen: map[string]bool{dump(s): true}}
	stop := make(chan struct{})
	done := make(chan readResult, readers)
	for range readers {
		go func() { done <- readAlongside(s, seen, stop) }()
	}
	defer func() {
		close(stop)
		for range readers {
			r := <-done
			if r.err != nil {
				t.Error(r.err)
			}
			if testing.Verbose() && seed == 1 {
				t.Logf("a reader made %d reads and saw %d states", r.reads, len(r.states))
			}
		}
	}()
	outcomes := map[string]int{}
	for range n {
		committed = runTransaction(t, g, s, replica, committed, seen, outcomes)
		compareAll(t, s, committed)
		if a, b := dump(s), dump(replica); a != b {
			t.Fatalf("the replica differs:\nstore:\n%s\nreplica:\n%s", a, b)
		}
		if err := s.Outside(); err != nil {
			t.Fatalf("Outside after the transaction ended: %v", err)
		}
	}
	checkInvariants(t, replica)
	// Every kind of step and ending has to come up, or the test tests less
	// than it says.
	for _, o := range []string{"put ok", "put invalid", "delete ok", "delete invalid", "delete not found", "drop ok",
		"drop invalid", "drop not found", "create ok", "create invalid", "link ok", "link invalid", "link not found",
		"link already there", "link applied", "unlink ok", "unlink not found", "unlink applied", "get ok", "get invalid",
		"get not found", "scan ok", "scan invalid", "live scan", "live scan moved", "live scan ended",
		"live scan at the end", "live scan across a statement rolled back", "neighbours ok", "neighbours found some",
		"walk ok", "walk found some", "nearest ok", "nearest invalid", "nearest found some", "nearest filtered",
		"statement rolled back", "read before the first change", "commit", "commit failed", "rollback"} {
		if outcomes[o] < max(1, n/150) {
			t.Errorf("%q came up %d times in %d transactions: %v", o, outcomes[o], n, outcomes)
		}
	}
	if testing.Verbose() && seed == 1 {
		t.Logf("outcomes: %v", outcomes)
	}
}

// runTransaction runs one random transaction, and returns the model of what
// the store holds once it has ended.
func runTransaction(t *testing.T, g *gen, s, replica *Store, committed *model, seen *states, outcomes map[string]int) *model {
	t.Helper()
	at := dump(s)
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// A check that fails ends the test inside the transaction, and the
	// readers wait for it to end before they can stop.
	defer tx.Rollback()
	work := committed.clone()
	changed := false   // the transaction has made a change, so it holds the copy's lock
	var live *liveScan // a cursor through the transaction, open across its steps
	defer func() {
		// Once the transaction has ended, the cursor gives no more. A check
		// that fails leaves it open, and then there's nothing to check.
		if live != nil && tx.done {
			if rec, more := live.c.Next(); more {
				t.Errorf("%s gave %s after the transaction ended", live.desc, rec.Key)
			}
			outcomes["live scan at the end"]++
		}
	}()
	for st := g.r.IntN(5); st >= 0; st-- {
		// A statement of one to three steps, as SQL's writes make them.
		mark := tx.Mark()
		stmt := work.clone()
		g.m = stmt
		failed := false
		for steps := 1 + g.r.IntN(3); steps > 0; steps-- {
			if live != nil && g.one(2) && !live.pull(t, tx, stmt, outcomes) {
				outcomes["live scan ended"]++
				live = nil
			}
			if g.one(3) {
				txRead(t, g, tx, stmt, &live, outcomes)
				continue
			}
			err := txWrite(t, g, s, tx, stmt, outcomes)
			changed = changed || err == nil
			failed = failed || err != nil
		}
		if side := g.aside(); side.one(2) {
			txSearch(t, side, tx, stmt, outcomes)
		}
		// A statement with a write that failed is taken back half the time,
		// as SQL's are, and now and then one that worked is too. Otherwise
		// its writes that worked stay, and the transaction carries on.
		if failed && g.one(2) || g.one(8) {
			tx.RollbackTo(mark)
			outcomes["statement rolled back"]++
			if live != nil {
				outcomes["live scan across a statement rolled back"]++
			}
			if len(tx.changes) != mark.changes {
				t.Fatalf("RollbackTo left %d changes, where the mark had %d", len(tx.changes), mark.changes)
			}
			compareAll(t, s, work)
			if side := g.aside(); side.one(2) {
				txSearch(t, side, tx, work, outcomes)
			}
		} else {
			work = stmt
			if g.one(4) {
				compareAll(t, s, work)
			}
		}
		g.m = work
		if g.one(4) {
			checkInside(t, s, changed, at, outcomes)
		}
	}
	list := slices.Clone(tx.changes)
	switch end := g.r.IntN(10); {
	case end < 6:
		var handed []format.Change
		called := false
		err := tx.Commit(func(changes []format.Change) error {
			called, handed = true, changes
			seen.add(dump(s)) // before readers can see it
			return nil
		})
		if err != nil || called != (len(list) > 0) || !slices.EqualFunc(handed, list, equalChange) {
			t.Fatalf("Commit gave %v, called write: %v, and handed it %v, where the transaction made %v", err, called, handed, list)
		}
		applyTo(t, replica, handed)
		outcomes["commit"]++
		return work
	case end < 8:
		errLog := errors.New("the log couldn't write the batch")
		err := tx.Commit(func([]format.Change) error { return errLog })
		if len(list) == 0 {
			if err != nil {
				t.Fatalf("a commit with no changes, and so no write, gave %v", err)
			}
			return work
		}
		if !errors.Is(err, errLog) {
			t.Fatalf("a commit whose write failed gave %v", err)
		}
		outcomes["commit failed"]++
	default:
		tx.Rollback()
		outcomes["rollback"]++
	}
	if err := tx.Put("docs:1", nil); !errors.Is(err, errs.ErrClosed) {
		t.Fatalf("a put through a transaction that has ended gave %v", err)
	}
	return committed
}

// txWrite makes one random write through the transaction and on the model,
// checks that they agree, and that the write added the right changes to
// the change list, or none when it failed. It returns the write's error.
func txWrite(t *testing.T, g *gen, s *Store, tx *Tx, m *model, outcomes map[string]int) error {
	t.Helper()
	before := len(tx.changes)
	var desc, want string
	var err error
	var wantChanges []format.Change
	checkChanges := true // whether wantChanges are the changes the write should give
	switch w := g.r.IntN(100); {
	case w < 45:
		key, fields, bad := g.put()
		desc = fmt.Sprintf("put %q %v", key, fields)
		_, existed := s.tables[tableOfKey(key+":")]
		err = tx.Put(key, fields)
		want = m.put(key, fields, bad)
		if err == nil {
			checkPutChanges(t, s, tx.changes[before:], key, existed)
		}
		checkChanges = false
	case w < 60:
		a := g.link()
		desc = fmt.Sprintf("link %q %q %q", a.from, a.typ, a.to)
		c := format.Change{Op: format.Link, Key: a.from, Type: a.typ, To: a.to}
		var added bool
		if g.one(4) {
			// A Link change for a link that's there breaks FORMAT.md's
			// rules, where Link adds nothing.
			err = tx.Apply(c)
			want, added = m.link(a)
			if want == "" && !added {
				want = "invalid"
			}
			outcomes["link applied"]++
		} else {
			err = tx.Link(a.from, a.typ, a.to)
			want, added = m.link(a)
			if want == "" && !added {
				outcomes["link already there"]++
			}
		}
		if added {
			wantChanges = []format.Change{c}
		}
	case w < 66:
		from, typ, to := g.unlinkArgs()
		desc = fmt.Sprintf("unlink %q %q %q", from, typ, to)
		if l := (Link{from, typ, to}); typ != "" && m.links[l] && g.one(2) {
			// A link that's there, as an Unlink change: the change needs a
			// type, and its keys and type have to keep the rules, so only
			// the model's links are certain to.
			err = tx.Apply(format.Change{Op: format.Unlink, Key: from, Type: typ, To: to})
			outcomes["unlink applied"]++
		} else {
			err = tx.Unlink(from, typ, to)
		}
		var types []string
		want, types = m.unlink(from, typ, to)
		for _, typ := range types {
			wantChanges = append(wantChanges, format.Change{Op: format.Unlink, Key: from, Type: typ, To: to})
		}
	case w < 82:
		key, bad := g.key()
		desc = "delete " + key
		if c := (format.Change{Op: format.Delete, Key: key}); g.one(3) {
			err = tx.Apply(c)
		} else {
			err = tx.Delete(key)
		}
		want = m.delete(key, bad)
		wantChanges = []format.Change{{Op: format.Delete, Key: key}}
	case w < 93:
		name, bad := pick(g, txTables), false
		if g.one(5) {
			name, bad = pick(g, badTables), true
		}
		desc = "drop " + name
		if c := (format.Change{Op: format.Drop, Table: name}); g.one(3) {
			err = tx.Apply(c)
		} else {
			err = tx.Drop(name)
		}
		want = m.drop(name, bad)
		wantChanges = []format.Change{{Op: format.Drop, Table: name}}
	default:
		c := g.create()
		desc = c.String()
		err = tx.Apply(c)
		want = m.create(c)
		wantChanges = []format.Change{c}
	}
	got := kindOf(err)
	if got != want {
		t.Fatalf("%s: got %q (%v), the model says %q", desc, got, err, want)
	}
	outcomes[strings.Fields(desc)[0]+" "+cmp.Or(got, "ok")]++
	switch {
	case err != nil && len(tx.changes) != before:
		t.Fatalf("%s failed and still gave the changes %v", desc, tx.changes[before:])
	case err == nil && checkChanges && !slices.EqualFunc(tx.changes[before:], wantChanges, equalChange):
		t.Fatalf("%s gave the changes %v, where it should give %v", desc, tx.changes[before:], wantChanges)
	}
	return err
}

// txRead makes one random read through the transaction, which must see the
// transaction's own changes: half the time a get, otherwise a scan, or the
// opening of the live cursor when there's none. The steps that follow pull
// records from that one.
func txRead(t *testing.T, g *gen, tx *Tx, m *model, live **liveScan, outcomes map[string]int) {
	t.Helper()
	switch g.r.IntN(6) {
	case 0, 1:
		txGet(t, g, tx, m, outcomes)
	case 4, 5:
		readLinks(t, g, tx, m, outcomes)
	case 2:
		prefix, after, bad := g.scanArgs()
		desc := fmt.Sprintf("scan %q %q through the transaction", prefix, after)
		c, err := tx.Scan(prefix, after)
		want := ""
		if bad {
			want = "invalid"
		}
		if got := kindOf(err); got != want {
			t.Fatalf("%s: got %q (%v), the model says %q", desc, got, err, want)
		}
		if err == nil {
			stop := -1
			if g.one(4) {
				stop = g.r.IntN(4)
			}
			checkScan(t, tx, m, c, desc, m.scan(prefix, after), stop)
		}
		outcomes["scan "+cmp.Or(want, "ok")]++
	default:
		if *live != nil {
			txGet(t, g, tx, m, outcomes)
			return
		}
		prefix := pick(g, modelTables) + ":"
		after := ""
		if g.one(3) {
			after = prefix + pick(g, idStarts)
		}
		c, err := tx.Scan(prefix, after)
		if err != nil {
			t.Fatal(err)
		}
		*live = &liveScan{c: c, desc: fmt.Sprintf("the live scan %q %q through the transaction", prefix, after), prefix: prefix, after: after}
		outcomes["live scan"]++
	}
}

// txSearch makes a random search through the transaction, which must see
// the transaction's own changes and give what the model gives. Half the
// searches are of a table with vectors, with a filter that lets some
// through. Its choices are g's, which the caller makes aside.
func txSearch(t *testing.T, g *gen, tx *Tx, m *model, outcomes map[string]int) {
	t.Helper()
	a := g.nearest()
	if g.one(2) {
		for _, name := range modelTables {
			if v := g.heldVector(name); v != nil {
				a = nearestArgs{table: name, q: g.values(len(v)), k: 1 + g.r.IntN(6), filter: pick(g, []string{"", "odd", "whole"})}
				break
			}
		}
	}
	hits, err := searchModel(t, tx, m, a)
	outcomes["nearest "+cmp.Or(kindOf(err), "ok")]++
	noteSearch(a, hits, err, outcomes)
}

// readLinks reads the links of a random key through r, a transaction or
// the store, or walks from it, which must give what the model gives.
func readLinks(t *testing.T, g *gen, r Reader, m *model, outcomes map[string]int) {
	t.Helper()
	key, bad := g.linked()
	if g.one(2) {
		dir, typ := g.direction(), g.readType()
		links, err := r.Neighbours(key, dir, typ)
		want, kind := m.neighbours(key, bad, dir, typ)
		if got := kindOf(err); got != kind || err == nil && (!slices.Equal(links, want) || links != nil && len(links) == 0) {
			t.Fatalf("neighbours %q %v %q gave %v, %v, where the model gives %v, %q", key, dir, typ, links, err, want, kind)
		}
		outcomes["neighbours "+cmp.Or(kind, "ok")]++
		if len(links) > 0 {
			outcomes["neighbours found some"]++
		}
		return
	}
	dir, typ, depth := g.direction(), g.readType(), g.depth()
	steps, err := r.Walk(key, dir, typ, depth)
	want, kind := m.walk(key, bad, dir, typ, depth)
	if got := kindOf(err); got != kind || err == nil && (!slices.Equal(steps, want) || steps != nil && len(steps) == 0) {
		t.Fatalf("walk %q %v %q %d gave %v, %v, where the model gives %v, %q", key, dir, typ, depth, steps, err, want, kind)
	}
	outcomes["walk "+cmp.Or(kind, "ok")]++
	if len(steps) > 0 {
		outcomes["walk found some"]++
	}
}

// txGet reads a random key through the transaction, which must see the
// transaction's own changes.
func txGet(t *testing.T, g *gen, tx *Tx, m *model, outcomes map[string]int) {
	t.Helper()
	key, bad := g.key()
	r, err := tx.Get(key)
	want := ""
	switch {
	case bad:
		want = "invalid"
	case m.records[key] == nil:
		want = "not found"
	}
	got := kindOf(err)
	if got != want {
		t.Fatalf("get %s through the transaction: got %q (%v), the model says %q", key, got, err, want)
	}
	if err == nil {
		compareRecord(t, tx, m, r)
	}
	outcomes["get "+cmp.Or(got, "ok")]++
}

// checkInside makes calls through the store, as calls through the database
// inside Update would, on the goroutine running the transaction. Outside
// must say so at any time. A read goes ahead before the first change, and
// sees the store as the last commit left it. Begin, and a read after the
// first change, are TestInsideUpdate's: it can catch a call that waits for
// itself without hanging.
func checkInside(t *testing.T, s *Store, changed bool, committed string, outcomes map[string]int) {
	t.Helper()
	if err := s.Outside(); !errors.Is(err, errs.ErrInsideUpdate) {
		t.Fatalf("Outside on the transaction's goroutine gave %v", err)
	}
	if changed {
		return
	}
	var d string
	if err := s.Read(func(Reader) error { d = dump(s); return nil }); err != nil {
		t.Fatalf("a read through the store before the transaction's first change gave %v", err)
	}
	if d != committed {
		t.Fatalf("a read through the store before the transaction's first change saw\n%s\nwhere the last commit left\n%s", d, committed)
	}
	outcomes["read before the first change"]++
}

// applyTo applies a committed change list to the replica, in a transaction
// of its own, as a process following the log will.
func applyTo(t *testing.T, replica *Store, changes []format.Change) {
	t.Helper()
	tx, err := replica.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if err := tx.Apply(c); err != nil {
			t.Fatalf("the replica refused %v: %v", c, err)
		}
	}
	if err := tx.Commit(nil); err != nil {
		t.Fatal(err)
	}
}

// readAlongside reads the whole store, over and over, until stop closes.
// Each read must see a state that a commit left. Inside it, every record
// must come through Get, and each table's records through a scan of the
// whole table, in byte order of key. Every fourth read, each record's links
// both ways must come through Neighbours, each to a record in the store,
// and a walk one link out of it must reach the records at their other ends,
// and a search of each table with a vector size must find every vector its
// scan gives, as a search written plainly finds them.
func readAlongside(s *Store, seen *states, stop <-chan struct{}) readResult {
	res := readResult{states: map[string]bool{}}
	for ; ; res.reads++ {
		select {
		case <-stop:
			if res.reads == 0 {
				res.err = errors.New("a reader never got to read")
			}
			return res
		default:
		}
		var d string
		err := s.Read(func(r Reader) error {
			d = dump(s)
			counts := map[string]int{}
			for key, rec := range s.records {
				if _, err := r.Get(key); err != nil {
					return err
				}
				counts[rec.table.name]++
				if res.reads%4 != 0 {
					continue
				}
				links, err := r.Neighbours(key, Both, "")
				if err != nil {
					return err
				}
				steps, err := r.Walk(key, Both, "", 1)
				if err != nil {
					return err
				}
				ends := map[string]bool{}
				for _, l := range links {
					if s.records[l.From] == nil || s.records[l.To] == nil || l.From != key && l.To != key {
						return fmt.Errorf("Neighbours(%s) gave %v", key, l)
					}
					ends[l.From], ends[l.To] = true, true
				}
				delete(ends, key)
				if len(steps) != len(ends) {
					return fmt.Errorf("a walk from %s gave %v, where its links are %v", key, steps, links)
				}
			}
			for name := range s.tables {
				c, err := r.Scan(name+":", "")
				if err != nil {
					return err
				}
				n, last := 0, ""
				for rec, more := c.Next(); more; rec, more = c.Next() {
					if n > 0 && rec.Key <= last || s.records[rec.Key] == nil || s.records[rec.Key].table != s.tables[name] {
						return fmt.Errorf("a scan of %s gave %s after %q", name, rec.Key, last)
					}
					n, last = n+1, rec.Key
				}
				if n != counts[name] {
					return fmt.Errorf("a scan of %s gave %d records, of the %d it holds", name, n, counts[name])
				}
				if res.reads%4 == 0 {
					if err := searchAlongside(r, name); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			res.err = fmt.Errorf("a read beside the transactions gave %v", err)
			return res
		}
		if !seen.has(d) {
			res.err = fmt.Errorf("a read beside the transactions saw a state that no commit left:\n%s", d)
			return res
		}
		res.states[d] = true
	}
}

// searchAlongside searches the table called name through r for every
// vector it holds, and checks the hits against the vectors a scan of the
// table gives, searched plainly.
func searchAlongside(r Reader, name string) error {
	tb, _ := r.Table(name)
	if tb.Size == 0 {
		return nil
	}
	q := make([]float32, tb.Size)
	for i := range q {
		q[i] = float32(i%3) - 0.5
	}
	hits, err := r.Nearest(name, q, MaxK, nil)
	if err != nil {
		return err
	}
	want := []Hit{}
	c, err := r.Scan(name+":", "")
	if err != nil {
		return err
	}
	for rec, more := c.Next(); more; rec, more = c.Next() {
		if rec.Vec != nil {
			want = append(want, Hit{rec.Key, vecmath.Distance(vecmath.Dot(rec.Vec, q), vecmath.Norm(q), vecmath.Norm(rec.Vec))})
		}
	}
	slices.SortFunc(want, func(x, y Hit) int { return cmp.Or(cmp.Compare(x.Distance, y.Distance), strings.Compare(x.Key, y.Key)) })
	if !sameHits(hits, want) {
		return fmt.Errorf("a search of %s gave %v, where its vectors give %v", name, hits, want)
	}
	return nil
}

// readResult is what a reader alongside the transactions did.
type readResult struct {
	reads  int
	states map[string]bool // the states it saw, as dump writes them
	err    error
}

// states are the states that commits have left, as dump writes them. A
// reader may see any of them, and nothing else.
type states struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (st *states) add(d string) {
	st.mu.Lock()
	st.seen[d] = true
	st.mu.Unlock()
}

func (st *states) has(d string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.seen[d]
}

func newModel() *model {
	return &model{tables: map[string]*modelTable{}, records: map[string]map[string]value.Value{}, links: map[Link]bool{}}
}

// clone copies the model, for a transaction or a statement to work on.
func (m *model) clone() *model {
	c := newModel()
	for name, t := range m.tables {
		c.tables[name] = &modelTable{fields: slices.Clone(t.fields), size: t.size}
	}
	for key, rec := range m.records {
		c.records[key] = maps.Clone(rec)
	}
	c.links, c.order = maps.Clone(m.links), slices.Clone(m.order)
	return c
}

// create applies a CreateTable to the model, and returns the kind of error
// it should give.
func (m *model) create(c format.Change) string {
	if m.tables[c.Table] != nil {
		return "invalid"
	}
	m.tables[c.Table] = &modelTable{fields: slices.Clone(c.Names), size: c.Size}
	return ""
}

// txTables are the tables that drops and creates pick from: the model
// test's, and one that only a create makes.
var txTables = append(slices.Clone(modelTables), "made")

// create returns a CreateTable for one of txTables, with up to three
// fields, among which the vector field may have a size.
func (g *gen) create() format.Change {
	c := format.Change{Op: format.CreateTable, Table: pick(g, txTables)}
	for n := g.r.IntN(4); n > 0; n-- {
		name := pick(g, modelNames)
		if !slices.ContainsFunc(c.Names, func(have string) bool { return rules.SameName(have, name) }) {
			c.Names = append(c.Names, name)
		}
	}
	if slices.ContainsFunc(c.Names, rules.IsVec) && g.one(2) {
		c.Size = 1 + g.r.IntN(4)
	}
	return c
}
