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
)

// TestTransactions is S2's closing test. Random transactions run on the
// store and on the model, with random rollbacks: of whole transactions, of
// statements taken back to a mark as SQL's are, and of commits whose write
// to the log fails. Inside them go random puts, deletes, drops and creates,
// many of which fail and must leave the transaction as it was, and reads
// through the transaction, which must see its changes. Every answer must
// agree with the model's. After each transaction the store must hold what
// the model holds, and so must a replica that applies each committed change
// list. Readers run alongside the whole time, and every read must see a
// state that a commit left. CI runs it under the race detector too.
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
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x52)), m: committed}
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
		"drop invalid", "drop not found", "create ok", "create invalid", "get ok", "get invalid", "get not found",
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
	changed := false // the transaction has made a change, so it holds the copy's lock
	for st := g.r.IntN(5); st >= 0; st-- {
		// A statement of one to three steps, as SQL's writes make them.
		mark := tx.Mark()
		stmt := work.clone()
		g.m = stmt
		failed := false
		for steps := 1 + g.r.IntN(3); steps > 0; steps-- {
			if g.one(5) {
				txGet(t, g, tx, stmt, outcomes)
				continue
			}
			err := txWrite(t, g, s, tx, stmt, outcomes)
			changed = changed || err == nil
			failed = failed || err != nil
		}
		// A statement with a write that failed is taken back half the time,
		// as SQL's are, and now and then one that worked is too. Otherwise
		// its writes that worked stay, and the transaction carries on.
		if failed && g.one(2) || g.one(8) {
			tx.RollbackTo(mark)
			outcomes["statement rolled back"]++
			if len(tx.changes) != mark.changes {
				t.Fatalf("RollbackTo left %d changes, where the mark had %d", len(tx.changes), mark.changes)
			}
			compareAll(t, s, work)
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
	switch w := g.r.IntN(100); {
	case w < 60:
		key, fields, bad := g.put()
		desc = fmt.Sprintf("put %q %v", key, fields)
		_, existed := s.tables[tableOfKey(key+":")]
		err = tx.Put(key, fields)
		want = m.put(key, fields, bad)
		if err == nil {
			checkPutChanges(t, s, tx.changes[before:], key, existed)
		}
	case w < 80:
		key, bad := g.key()
		desc = "delete " + key
		if c := (format.Change{Op: format.Delete, Key: key}); g.one(3) {
			err = tx.Apply(c)
		} else {
			err = tx.Delete(key)
		}
		want = m.delete(key, bad)
		wantChanges = []format.Change{{Op: format.Delete, Key: key}}
	case w < 92:
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
	case err == nil && wantChanges != nil && !slices.EqualFunc(tx.changes[before:], wantChanges, equalChange):
		t.Fatalf("%s gave the changes %v", desc, tx.changes[before:])
	}
	return err
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
// Each read must see a state that a commit left.
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
			for key := range s.records {
				if _, err := r.Get(key); err != nil {
					return err
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
	return &model{tables: map[string]*modelTable{}, records: map[string]map[string]value.Value{}}
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
