// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestChangeLists is S3's closing test: store A's change lists applied to
// store B, and A's snapshot loaded into store C, both give back A.
//
// Random transactions run on A, as in S2's test: puts, links, unlinks,
// deletes, drops and creates, many of which fail, statements taken back to
// their marks, commits whose write fails, and rollbacks. Deletes and drops
// take links out both ways, which B and C have to work out for themselves
// from the Delete or the Drop alone. Each change list a commit hands
// on is encoded with format.AppendBatch and decoded again, as the log
// writes a batch and reads it, and must come back the same. B opens once a
// fifth of the transactions have run, taking the batches committed by then
// with LoadBatch, as a process opening the file does. Then it takes each
// new batch with ApplyBatch, as a process following the log does, while
// readers read B and must only ever see a state that a commit on A left. A
// quarter of the time B is first handed the batch with a change put in that
// breaks the rules, which must be damage naming the batch and the change,
// and leave B as it was.
//
// Every 20 transactions, A's snapshot is checked against FORMAT.md's order
// and the model, cut into batches as compaction cuts a compacted part,
// encoded and decoded, and loaded into an empty store C, with LoadBatch or
// ApplyBatch. Then C, and B once it follows the log, must hold what A holds,
// through the read API and through their snapshots.
func TestChangeLists(t *testing.T) {
	seeds, txs := 12, 300
	if testing.Short() {
		seeds, txs = 3, 150
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runChangeLists(t, seed, txs) })
	}
}

func runChangeLists(t *testing.T, seed uint64, n int) {
	a, b := New(), New()
	committed := newModel()
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x53)), m: committed}
	seen := &states{seen: map[string]bool{dump(a): true}}
	outcomes := map[string]int{}
	var opening [][]format.Change // the batches in the file when B opens it
	following := false
	stop := make(chan struct{})
	done := make(chan readResult, readers)
	defer func() {
		if !following {
			return
		}
		close(stop)
		for range readers {
			r := <-done
			if r.err != nil {
				t.Error(r.err)
			}
			if testing.Verbose() && seed == 1 {
				t.Logf("a reader of B made %d reads and saw %d states", r.reads, len(r.states))
			}
		}
	}()
	seq := uint64(0)
	for i := range n {
		var list []format.Change
		committed, list = changeListTx(t, g, a, committed, seen, outcomes)
		if list != nil {
			seq++
			batch := writable(t, 1, seq, list)
			if following {
				follow(t, g, b, seq, batch, outcomes)
			} else {
				opening = append(opening, batch)
			}
		}
		if !following && i+1 >= n/5 {
			for k, batch := range opening {
				if err := b.LoadBatch(uint64(k+1), batch); err != nil {
					t.Fatalf("B couldn't load batch %d: %v", k+1, err)
				}
				outcomes["batch loaded"]++
			}
			following = true
			for range readers {
				go func() { done <- readAlongside(b, seen, stop) }()
			}
		}
		if i%20 == 19 || i == n-1 {
			follower := b
			if !following {
				follower = nil
			}
			checkpoint(t, g, a, follower, committed, outcomes)
		}
	}
	// Every kind of step and ending has to come up, or the test tests less
	// than it says.
	for _, o := range []string{"put ok", "put invalid", "delete ok", "delete not found", "drop ok", "drop not found",
		"create ok", "create invalid", "link ok", "link already there", "unlink ok", "statement rolled back", "commit",
		"commit failed", "rollback", "batch loaded", "batch applied", "damage found", "damage in a link",
		"snapshot loaded", "snapshot applied", "snapshot with links"} {
		if outcomes[o] < max(1, n/150) {
			t.Errorf("%q came up %d times in %d transactions: %v", o, outcomes[o], n, outcomes)
		}
	}
	if testing.Verbose() && seed == 1 {
		t.Logf("outcomes: %v", outcomes)
	}
}

// changeListTx runs one random transaction on s, as S2's runTransaction
// does, with writes that fail and statements taken back to their marks,
// and ends it with a commit, a commit whose write fails, or a rollback. It
// returns the model of what s holds once the transaction has ended, and
// the change list its commit handed on, or nil when it handed none on.
// seen gets each state a commit leaves, before anyone can read it.
func changeListTx(t *testing.T, g *gen, s *Store, committed *model, seen *states, outcomes map[string]int) (*model, []format.Change) {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	work := committed.clone()
	for st := g.r.IntN(5); st >= 0; st-- {
		mark := tx.Mark()
		stmt := work.clone()
		g.m = stmt
		failed := false
		for steps := 1 + g.r.IntN(3); steps > 0; steps-- {
			if g.one(6) {
				txGet(t, g, tx, stmt, outcomes)
				continue
			}
			if err := txWrite(t, g, s, tx, stmt, outcomes); err != nil {
				failed = true
			}
		}
		if failed && g.one(2) || g.one(8) {
			tx.RollbackTo(mark)
			outcomes["statement rolled back"]++
		} else {
			work = stmt
		}
		g.m = work
	}
	list := slices.Clone(tx.changes)
	switch end := g.r.IntN(10); {
	case end < 7:
		var handed []format.Change
		called := false
		err := tx.Commit(func(changes []format.Change) error {
			called, handed = true, changes
			seen.add(dump(s))
			return nil
		})
		if err != nil || called != (len(list) > 0) || !slices.EqualFunc(handed, list, equalChange) {
			t.Fatalf("Commit gave %v, called write: %v, and handed it %v, where the transaction made %v", err, called, handed, list)
		}
		if called {
			outcomes["commit"]++
		}
		return work, handed
	case end < 8:
		errLog := errors.New("the log couldn't write the batch")
		err := tx.Commit(func(changes []format.Change) error {
			writable(t, 1, 1, changes) // what the log would have written
			return errLog
		})
		if len(list) == 0 {
			if err != nil {
				t.Fatalf("a commit with no changes, and so no write, gave %v", err)
			}
			return work, nil
		}
		if !errors.Is(err, errLog) {
			t.Fatalf("a commit whose write failed gave %v", err)
		}
		outcomes["commit failed"]++
	default:
		tx.Rollback()
		outcomes["rollback"]++
	}
	return committed, nil
}

// writable encodes changes as the log writes a batch, with
// format.AppendBatch, then decodes the batch as the log reads one. The
// codec must take the changes and give them back the same. It returns what
// it decoded, which is what the log hands a store.
func writable(t *testing.T, gen, seq uint64, changes []format.Change) []format.Change {
	t.Helper()
	b, sum, err := format.AppendBatch(nil, gen, seq, changes)
	if err != nil {
		t.Fatalf("AppendBatch refused batch %d: %v\nits changes: %v", seq, err, changes)
	}
	bt, err := format.DecodeBatch(b, gen, seq)
	if err != nil {
		t.Fatalf("DecodeBatch refused batch %d, as AppendBatch wrote it: %v", seq, err)
	}
	if bt.Seq != seq || bt.Sum != sum || bt.Length != len(b) || !slices.EqualFunc(bt.Changes, changes, equalChange) {
		t.Fatalf("batch %d came back as %+v, from the changes %v", seq, bt, changes)
	}
	return bt.Changes
}

// follow hands B a committed batch, as a process following the log takes
// it. A quarter of the time it hands B a damaged copy first, which B must
// refuse whole.
func follow(t *testing.T, g *gen, b *Store, seq uint64, batch []format.Change, outcomes map[string]int) {
	t.Helper()
	if g.one(4) {
		bad, at := damaged(g, batch)
		bad = writable(t, 1, seq, bad) // a batch that counts: only the store can find the damage
		before := dumpRead(b)
		err := b.ApplyBatch(seq, bad)
		var d *errs.Damage
		if !errors.As(err, &d) || d.Batch != seq || !strings.HasPrefix(d.Reason, fmt.Sprintf("change %d: ", at)) ||
			errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("batch %d, with change %d breaking the rules, gave %v\nits changes: %v", seq, at, err, bad)
		}
		if after := dumpRead(b); after != before {
			t.Fatalf("a damaged batch changed B from\n%s\nto\n%s", before, after)
		}
		outcomes["damage found"]++
		if op := bad[at-1].Op; op == format.Link || op == format.Unlink {
			outcomes["damage in a link"]++
		}
	}
	if err := b.ApplyBatch(seq, batch); err != nil {
		t.Fatalf("B couldn't apply batch %d: %v", seq, err)
	}
	outcomes["batch applied"]++
}

// damaged returns a copy of batch with a change put in at a random place,
// which breaks the rules for the state the changes before it leave, and
// that change's number, from 1. The codec writes such a change, since only
// the state shows what's wrong with it.
func damaged(g *gen, batch []format.Change) ([]format.Change, int) {
	at := g.r.IntN(len(batch) + 1)
	var bad format.Change
	if at > 0 && batch[at-1].Op != format.Put && g.one(2) {
		// The same create, delete, drop, link or unlink again, straight
		// after it: a table made twice, a record or a table that's gone, a
		// link added twice, or a link taken out twice.
		bad = batch[at-1]
	} else {
		bad = pick(g, []format.Change{
			{Op: format.Put, Key: "nosuch:1"},
			{Op: format.Delete, Key: "nosuch:1"},
			{Op: format.Drop, Table: "nosuch"},
			{Op: format.Link, Key: "nosuch:1", Type: "x", To: "nosuch:2"},
			{Op: format.Unlink, Key: "nosuch:1", Type: "x", To: "nosuch:2"},
		})
	}
	return slices.Insert(slices.Clone(batch), at, bad), at + 1
}

// checkpoint checks A's snapshot, loads it into an empty store C as opening
// a compacted file does, and checks that C, and B when it isn't nil, hold
// what A holds.
func checkpoint(t *testing.T, g *gen, a, b *Store, m *model, outcomes map[string]int) {
	t.Helper()
	snap := snapshotOf(a)
	checkSnapshot(t, m, snap)
	if len(m.links) > 0 {
		outcomes["snapshot with links"]++
	}
	c := New()
	how := "snapshot loaded"
	if g.one(2) {
		how = "snapshot applied"
	}
	for i, batch := range cut(g, snap) {
		seq := uint64(i + 1)
		decoded := writable(t, 2, seq, batch) // a compacted file's batches have generation 2 or later
		var err error
		if how == "snapshot loaded" {
			err = c.LoadBatch(seq, decoded)
		} else {
			err = c.ApplyBatch(seq, decoded)
		}
		if err != nil {
			t.Fatalf("C couldn't take batch %d of A's snapshot: %v", seq, err)
		}
	}
	outcomes[how]++
	sameStores(t, "C", a, c)
	if b != nil {
		sameStores(t, "B", a, b)
	}
}

// cut splits a snapshot into batches of one to eight changes, as compaction
// splits a compacted part where it likes, keeping each change whole.
func cut(g *gen, changes []format.Change) [][]format.Change {
	var out [][]format.Change
	for len(changes) > 0 {
		n := min(1+g.r.IntN(8), len(changes))
		out = append(out, changes[:n:n])
		changes = changes[n:]
	}
	return out
}

// snapshotOf returns s's snapshot, taken inside Read as compaction takes
// it, with each change's slices copied out, since the snapshot uses a
// Put's Fields again.
func snapshotOf(s *Store) []format.Change {
	var out []format.Change
	s.Read(func(r Reader) error {
		out = collect(r)
		return nil
	})
	return out
}

func collect(r Reader) []format.Change {
	var out []format.Change
	for c := range r.Snapshot() {
		c.Names, c.Fields = slices.Clone(c.Names), slices.Clone(c.Fields)
		out = append(out, c)
	}
	return out
}

// dumpRead is dump inside Read, for a store that others read.
func dumpRead(s *Store) string {
	var d string
	s.Read(func(Reader) error {
		d = dump(s)
		return nil
	})
	return d
}

// checkSnapshot checks a snapshot against FORMAT.md's order for a
// compacted part, and against the model of what the store holds. Every
// table comes in byte order of name, as a CreateTable with its vector size
// and its whole field list, and then a Put for each of its records in byte
// order of key. A Put carries every field that holds a value, the vector
// among them, spelt as the table spells it, in byte order of name, and no
// nulls. Then every link comes, as a Link, in byte order of the key it's
// from, then its type, then the key it's to.
func checkSnapshot(t *testing.T, m *model, snap []format.Change) {
	t.Helper()
	var table *modelTable
	var name, last string // the table whose records come now, and the last key
	tables, puts, links := 0, 0, 0
	var lastLink *format.Change
	for i, c := range snap {
		if lastLink != nil && c.Op != format.Link {
			t.Fatalf("the snapshot gives %v after the link %v", c, lastLink)
		}
		switch c.Op {
		case format.Link:
			if l := (Link{c.Key, c.Type, c.To}); !m.links[l] {
				t.Fatalf("the snapshot gives %v, which isn't one of the model's links", c)
			}
			if lastLink != nil && cmp.Or(strings.Compare(lastLink.Key, c.Key), strings.Compare(lastLink.Type, c.Type),
				strings.Compare(lastLink.To, c.To)) >= 0 {
				t.Fatalf("the snapshot gives %v after %v", c, lastLink)
			}
			lastLink = &snap[i]
			links++
		case format.CreateTable:
			mt := m.tables[c.Table]
			if mt == nil || tables > 0 && c.Table <= name || !slices.Equal(c.Names, mt.fields) || c.Size != mt.size {
				t.Fatalf("the snapshot gives %v after table %q, where the model has %+v", c, name, mt)
			}
			table, name, last = mt, c.Table, ""
			tables++
		case format.Put:
			rec := m.records[c.Key]
			if table == nil || tableOfKey(c.Key) != name || c.Key <= last || rec == nil || len(c.Fields) != len(rec) {
				t.Fatalf("the snapshot gives %v in table %q after %q, where the model has %v", c, name, last, rec)
			}
			for i, f := range c.Fields {
				if i > 0 && c.Fields[i-1].Name >= f.Name || !slices.Contains(table.fields, f.Name) || f.Value.IsNull() ||
					rec[strings.ToLower(f.Name)] != f.Value {
					t.Fatalf("the snapshot gives %v, where the model has %v, in a table with the fields %q", c, rec, table.fields)
				}
			}
			last = c.Key
			puts++
		default:
			t.Fatalf("the snapshot gives %v", c)
		}
	}
	if tables != len(m.tables) || puts != len(m.records) || links != len(m.links) {
		t.Fatalf("the snapshot gives %d tables, %d records and %d links, and the model has %d, %d and %d",
			tables, puts, links, len(m.tables), len(m.records), len(m.links))
	}
}

// sameStores checks that got holds what want holds. Through the read API,
// every table and key the workload can name must give the same answer from
// both, its links and a walk from it included, and so must a scan of each
// table and the snapshot. Then the whole of both must match, each table's
// keys and each record's lists of links included, so nothing the read API
// can't see differs either.
func sameStores(t *testing.T, name string, want, got *Store) {
	t.Helper()
	tables := append(append(slices.Clone(txTables), badTables...), "nosuch")
	var keys []string
	for _, tbl := range tables {
		for _, id := range modelIDs {
			keys = append(keys, tbl+":"+id)
		}
	}
	keys = append(keys, badKeys...)
	err := want.Read(func(w Reader) error {
		return got.Read(func(g Reader) error {
			for _, tbl := range tables {
				wt, wok := w.Table(tbl)
				gt, gok := g.Table(tbl)
				if wok != gok || wt.Name != gt.Name || !slices.Equal(wt.Fields, gt.Fields) || wt.Vec != gt.Vec || wt.Size != gt.Size {
					return fmt.Errorf("table %s is %+v, %v, where it should be %+v, %v", tbl, gt, gok, wt, wok)
				}
			}
			for _, key := range keys {
				wr, werr := w.Get(key)
				gr, gerr := g.Get(key)
				if errText(werr) != errText(gerr) || !sameRecord(wr, gr) {
					return fmt.Errorf("Get(%q) gives %+v, %v, where it should give %+v, %v", key, gr, gerr, wr, werr)
				}
				wl, werr := w.Neighbours(key, Both, "")
				gl, gerr := g.Neighbours(key, Both, "")
				if errText(werr) != errText(gerr) || !slices.Equal(wl, gl) {
					return fmt.Errorf("Neighbours(%q) gives %v, %v, where it should give %v, %v", key, gl, gerr, wl, werr)
				}
				ws, werr := w.Walk(key, Both, "", 3)
				gs, gerr := g.Walk(key, Both, "", 3)
				if errText(werr) != errText(gerr) || !slices.Equal(ws, gs) {
					return fmt.Errorf("Walk(%q) gives %v, %v, where it should give %v, %v", key, gs, gerr, ws, werr)
				}
			}
			for _, tbl := range tables {
				ws, werr := scanAll(w, tbl+":")
				gs, gerr := scanAll(g, tbl+":")
				if errText(werr) != errText(gerr) || !slices.EqualFunc(ws, gs, sameRecord) {
					return fmt.Errorf("a scan of %s gives %+v, %v, where it should give %+v, %v", tbl, gs, gerr, ws, werr)
				}
			}
			if ws, gs := collect(w), collect(g); !slices.EqualFunc(ws, gs, equalChange) {
				return fmt.Errorf("the snapshot is\n%v\nwhere it should be\n%v", gs, ws)
			}
			if a, b := dump(want), dump(got); a != b {
				return fmt.Errorf("the store holds\n%s\nwhere it should hold\n%s", b, a)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("%s doesn't give back A: %v", name, err)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// scanAll returns every record a scan of prefix gives, in order.
func scanAll(r Reader, prefix string) ([]Record, error) {
	c, err := r.Scan(prefix, "")
	if err != nil {
		return nil, err
	}
	var out []Record
	for rec, more := c.Next(); more; rec, more = c.Next() {
		out = append(out, rec)
	}
	return out, nil
}

// sameRecord compares two records bit for bit, the vectors' values
// included, where -0 and 0 differ.
func sameRecord(a, b Record) bool {
	return a.Key == b.Key && slices.Equal(a.Fields, b.Fields) &&
		slices.EqualFunc(a.Vec, b.Vec, func(x, y float32) bool { return math.Float32bits(x) == math.Float32bits(y) }) &&
		(a.Vec == nil) == (b.Vec == nil)
}

// TestApplyBatch applies batches whole or not at all. Each batch below
// makes two changes, breaks a rule at its third, and has a fourth after
// that. ApplyBatch must report damage of kind "error", naming the batch and
// the third change, leave the store as it was with readers free, and then
// take the batch without its bad change.
func TestApplyBatch(t *testing.T) {
	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	put := func(key string, fields ...format.Field) format.Change {
		return format.Change{Op: format.Put, Key: key, Fields: fields}
	}
	for _, c := range []struct {
		bad format.Change
		why string // in the damage's reason
	}{
		{format.Change{Op: format.CreateTable, Table: "docs"}, "table docs is created when it exists already"},
		{put("notes:1"), "a put of notes:1 into table notes, which doesn't exist"},
		{put("docs:1", f("Title", value.Text("x"))), "spells table docs's field title as Title"},
		{put("docs:1", f("title", value.Text("x")), f("n", value.Int(1))), "out of byte order"},
		{put("docs:1", f("vec", value.Vector([]float32{1, 2, 3}))), "table docs holds vectors of 2 values, and docs:1 has 3"},
		{put("docs:1", f("title", value.Text("\xff"))), "isn't valid UTF-8"},
		{format.Change{Op: format.Delete, Key: "docs:2"}, "not found: docs:2"}, // the second change deleted it
		{format.Change{Op: format.Drop, Table: "nosuch"}, "not found: no record table nosuch"},
		{format.Change{Op: format.Delete, Key: "docs:1", Table: "docs"}, "a delete change that sets Table, which it doesn't use"},
		{format.Change{Op: format.Link, Key: "docs:1", Type: "cites", To: "docs:2"}, "not found: docs:2"},
		{format.Change{Op: format.Link, Key: "pics:1", Type: "shows", To: "docs:1"}, "pics:1 -shows-> docs:1 is added when it's there already"},
		{format.Change{Op: format.Link, Key: "docs:3", Type: "", To: "docs:1"}, `link type "": use 1 to 200 characters`},
		{format.Change{Op: format.Unlink, Key: "docs:2", Type: "cites", To: "docs:1"}, "no link docs:2 -cites-> docs:1 to remove"},
		{format.Change{Op: 99}, "a change of kind Op(99)"},
	} {
		s := undoStore(t)
		before := dump(s)
		batch := []format.Change{
			put("docs:3", f("title", value.Text("c"))),
			{Op: format.Delete, Key: "docs:2"},
			c.bad,
			{Op: format.Drop, Table: "empty"},
		}
		err := s.ApplyBatch(7, batch)
		var d *errs.Damage
		if !errors.As(err, &d) || d.Batch != 7 || d.Path != "" || d.Offset != 0 || !strings.HasPrefix(d.Reason, "change 3: ") ||
			!strings.Contains(d.Reason, c.why) {
			t.Errorf("a batch with %v gave %v", c.bad, err)
		}
		if !errors.Is(err, errs.ErrDamaged) || errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) ||
			errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("a batch with %v gave %v, which isn't of kind error", c.bad, err)
		}
		if got := dump(s); got != before {
			t.Fatalf("a batch with %v changed the store to\n%s\nfrom\n%s", c.bad, got, before)
		}
		if readersWait(s) {
			t.Fatalf("a batch with %v left the copy locked", c.bad)
		}
		ok(t, s.ApplyBatch(7, slices.Delete(batch, 2, 3)))
		if _, err := s.Get("docs:3"); err != nil {
			t.Fatalf("after the good batch, docs:3 gives %v", err)
		}
		if _, err := s.Get("docs:2"); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("after the good batch, docs:2 gives %v", err)
		}
		if _, found := s.Table("empty"); found {
			t.Fatal("after the good batch, the table empty is there")
		}
	}
	s := undoStore(t)
	before := dump(s)
	var d *errs.Damage
	if err := s.ApplyBatch(8, nil); !errors.As(err, &d) || d.Batch != 8 || d.Reason != "a batch with no changes" {
		t.Errorf("a batch with no changes gave %v", err)
	}
	if dump(s) != before {
		t.Error("a batch with no changes changed the store")
	}
}

// TestApplyBatchTakesItsTurn: ApplyBatch waits for a transaction that's
// open, as Begin does, and goes in once it ends. On the goroutine running
// that transaction, it would wait for itself, so it fails at once with
// errs.ErrInsideUpdate, unwrapped. That transaction runs on a goroutine of
// its own, so a check that doesn't work fails the test instead of hanging
// it.
func TestApplyBatchTakesItsTurn(t *testing.T) {
	s := newStoreN(t)
	inside, committed := make(chan error, 1), make(chan error, 1)
	carryOn := make(chan struct{})
	go func() {
		tx, err := s.Begin()
		if err == nil {
			defer tx.Rollback()
			err = setN(tx, 2)
		}
		if err == nil {
			if err = s.ApplyBatch(1, []format.Change{{Op: format.Put, Key: "docs:2"}}); err == errs.ErrInsideUpdate {
				err = nil
			} else {
				err = fmt.Errorf("ApplyBatch inside the transaction gave %v", err)
			}
		}
		inside <- err
		if err != nil {
			return
		}
		<-carryOn
		committed <- tx.Commit(nil)
	}()
	ok(t, recv(t, "ApplyBatch inside the transaction", inside))
	applied := make(chan error, 1)
	go func() {
		applied <- s.ApplyBatch(2, []format.Change{{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "n", Value: value.Int(3)}}}})
	}()
	stillWaiting(t, "ApplyBatch beside an open transaction", applied)
	close(carryOn)
	ok(t, recv(t, "the transaction's commit", committed))
	ok(t, recv(t, "ApplyBatch after the transaction", applied))
	wantN(t, readN(s), 3)
}

// TestLoadBatch: on opening, a batch goes straight into the store. One
// that breaks a rule is damage, as in ApplyBatch, and leaves the changes
// before it in the store, which the open that fails throws away.
func TestLoadBatch(t *testing.T) {
	s := undoStore(t)
	good := []format.Change{
		{Op: format.Put, Key: "docs:3", Fields: []format.Field{{Name: "title", Value: value.Text("c")}}},
		{Op: format.Delete, Key: "docs:2"},
	}
	err := s.LoadBatch(4, append(slices.Clone(good), format.Change{Op: format.Drop, Table: "nosuch"}))
	var d *errs.Damage
	if !errors.As(err, &d) || d.Batch != 4 || d.Reason != "change 3: hypercrux: not found: no record table nosuch" {
		t.Fatalf("a batch whose third change fails gave %v", err)
	}
	if _, err := s.Get("docs:3"); err != nil {
		t.Errorf("the changes before the one that failed aren't there: docs:3 gives %v", err)
	}
	s = undoStore(t)
	ok(t, s.LoadBatch(4, good))
	if _, err := s.Get("docs:2"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("after the batch, docs:2 gives %v", err)
	}
	if err := s.LoadBatch(5, []format.Change{}); !errors.As(err, &d) || d.Batch != 5 || d.Reason != "a batch with no changes" {
		t.Errorf("a batch with no changes gave %v", err)
	}
	tx := begin(t, s)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("LoadBatch worked while a transaction was open")
			}
		}()
		s.LoadBatch(6, good)
	}()
	tx.Rollback()
}

// TestFits: text and bytes fit the format up to 4,294,967,295 bytes. A test
// can't make a longer value, so fits is tested alone. A put of text and
// bytes too long for a u16 length goes through the codec as well.
func TestFits(t *testing.T) {
	ok(t, fits("t", math.MaxUint32))
	err := fits("t", math.MaxUint32+1)
	if !errors.Is(err, errs.ErrInvalid) || err.Error() != "hypercrux: invalid: field t holds 4294967296 bytes, and a value holds at most 4294967295" {
		t.Errorf("fits gave %v", err)
	}
	s := New()
	long := strings.Repeat("é", 1<<16)
	changes, err := s.Put(nil, "docs:1", []format.Field{{Name: "b", Value: value.Bytes(long)}, {Name: "t", Value: value.Text(long)}})
	ok(t, err)
	writable(t, 1, 1, changes)
}

// TestTheChangesOfEachWrite pins, for each of the writes S1.md lists, the
// change list a commit hands on, and that the codec writes it.
func TestTheChangesOfEachWrite(t *testing.T) {
	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	vec := func(x ...float32) value.Value { return value.Vector(x) }
	for _, c := range []struct {
		name  string
		write func(tx *Tx) error
		want  []format.Change
	}{
		{"a put into a new table", func(tx *Tx) error {
			return tx.Put("notes:1", []format.Field{f("body", value.Text("x")), f("Vec", vec(1, 2, 3))})
		}, []format.Change{
			{Op: format.CreateTable, Table: "notes"},
			{Op: format.Put, Key: "notes:1", Fields: []format.Field{f("Vec", vec(1, 2, 3)), f("body", value.Text("x"))}},
		}},
		{"a put spelt otherwise", func(tx *Tx) error {
			return tx.Put("docs:2", []format.Field{f("TITLE", value.Text("z")), f("VEC", value.Null()), f("Alpha", value.Int(1))})
		}, []format.Change{
			{Op: format.Put, Key: "docs:2", Fields: []format.Field{f("Alpha", value.Int(1)), f("title", value.Text("z")), f("vec", value.Null())}},
		}},
		{"a put of nothing", func(tx *Tx) error { return tx.Put("docs:1", nil) },
			[]format.Change{{Op: format.Put, Key: "docs:1"}}},
		{"a delete and a put again", func(tx *Tx) error {
			if err := tx.Delete("docs:1"); err != nil {
				return err
			}
			return tx.Put("docs:1", []format.Field{f("n", value.Int(2))})
		}, []format.Change{
			{Op: format.Delete, Key: "docs:1"},
			{Op: format.Put, Key: "docs:1", Fields: []format.Field{f("n", value.Int(2))}},
		}},
		{"a drop and a put into a table of the same name", func(tx *Tx) error {
			if err := tx.Drop("docs"); err != nil {
				return err
			}
			return tx.Put("docs:1", []format.Field{f("other", value.Int(1))})
		}, []format.Change{
			{Op: format.Drop, Table: "docs"},
			{Op: format.CreateTable, Table: "docs"},
			{Op: format.Put, Key: "docs:1", Fields: []format.Field{f("other", value.Int(1))}},
		}},
		{"an applied create of a whole table, and a put into it", func(tx *Tx) error {
			if err := tx.Apply(format.Change{Op: format.CreateTable, Table: "made", Size: 2, Names: []string{"z", "vec", "a"}}); err != nil {
				return err
			}
			return tx.Put("made:1", []format.Field{f("A", value.Int(1)), f("Z", value.Int(2))})
		}, []format.Change{
			{Op: format.CreateTable, Table: "made", Size: 2, Names: []string{"z", "vec", "a"}},
			{Op: format.Put, Key: "made:1", Fields: []format.Field{f("a", value.Int(1)), f("z", value.Int(2))}},
		}},
		{"links, one of them there already", func(tx *Tx) error {
			for _, l := range []Link{{"docs:2", "new", "pics:1"}, {"docs:1", "cites", "docs:2"}, {"pics:1", "self", "pics:1"}} {
				if err := tx.Link(l.From, l.Type, l.To); err != nil {
					return err
				}
			}
			return nil
		}, []format.Change{
			{Op: format.Link, Key: "docs:2", Type: "new", To: "pics:1"},
			{Op: format.Link, Key: "pics:1", Type: "self", To: "pics:1"},
		}},
		{"an unlink of every type, in byte order of type", func(tx *Tx) error {
			if err := tx.Link("docs:1", "also", "docs:2"); err != nil {
				return err
			}
			if err := tx.Link("docs:1", "Zed", "docs:2"); err != nil {
				return err
			}
			return tx.Unlink("docs:1", "", "docs:2")
		}, []format.Change{
			{Op: format.Link, Key: "docs:1", Type: "also", To: "docs:2"},
			{Op: format.Link, Key: "docs:1", Type: "Zed", To: "docs:2"},
			{Op: format.Unlink, Key: "docs:1", Type: "Zed", To: "docs:2"},
			{Op: format.Unlink, Key: "docs:1", Type: "also", To: "docs:2"},
			{Op: format.Unlink, Key: "docs:1", Type: "cites", To: "docs:2"},
		}},
		{"an unlink, and the link again", func(tx *Tx) error {
			if err := tx.Unlink("docs:1", "self", "docs:1"); err != nil {
				return err
			}
			return tx.Link("docs:1", "self", "docs:1")
		}, []format.Change{
			{Op: format.Unlink, Key: "docs:1", Type: "self", To: "docs:1"},
			{Op: format.Link, Key: "docs:1", Type: "self", To: "docs:1"},
		}},
		{"a delete of a record with links, which go without changes of their own", func(tx *Tx) error { return tx.Delete("docs:1") },
			[]format.Change{{Op: format.Delete, Key: "docs:1"}}},
		{"a drop of a table with links, which go without changes of their own", func(tx *Tx) error { return tx.Drop("docs") },
			[]format.Change{{Op: format.Drop, Table: "docs"}}},
		{"links and unlinks that fail", func(tx *Tx) error {
			if tx.Link("docs:1", "", "docs:2") == nil || tx.Link("docs:1", "x", "docs:404") == nil || tx.Unlink("docs:1", "x", "docs:2") == nil ||
				tx.Unlink("docs:404", "", "docs:1") == nil {
				return errors.New("a link or an unlink that should fail worked")
			}
			return tx.Link("docs:2", "x", "docs:1")
		}, []format.Change{{Op: format.Link, Key: "docs:2", Type: "x", To: "docs:1"}}},
		{"writes that fail, and a statement taken back", func(tx *Tx) error {
			if tx.Delete("docs:404") == nil || tx.Drop("nosuch") == nil || tx.Put("docs:1", []format.Field{f("key", value.Int(1))}) == nil {
				return errors.New("a write that should fail worked")
			}
			m := tx.Mark()
			if err := tx.Put("docs:5", nil); err != nil {
				return err
			}
			tx.RollbackTo(m)
			return tx.Delete("docs:2")
		}, []format.Change{{Op: format.Delete, Key: "docs:2"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := undoStore(t)
			tx := begin(t, s)
			ok(t, c.write(tx))
			var handed []format.Change
			ok(t, tx.Commit(func(changes []format.Change) error { handed = changes; return nil }))
			if !slices.EqualFunc(handed, c.want, equalChange) {
				t.Fatalf("the commit handed on\n%v\nwhere it should have handed on\n%v", handed, c.want)
			}
			writable(t, 1, 1, handed)
		})
	}
	// A transaction that changes nothing hands nothing on, so the log
	// writes no batch.
	s := undoStore(t)
	tx := begin(t, s)
	m := tx.Mark()
	ok(t, tx.Put("docs:9", nil))
	tx.RollbackTo(m)
	if tx.Delete("docs:404") == nil {
		t.Fatal("a delete of a record that isn't there worked")
	}
	ok(t, tx.Commit(func([]format.Change) error { return errors.New("write was called with no changes") }))
}
