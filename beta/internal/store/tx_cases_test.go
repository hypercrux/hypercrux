// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// patience is long enough that only a broken store runs out of it, even on
// a busy machine. moment is how long a read that should wait gets to go
// ahead, which only a broken store uses.
const (
	patience = 10 * time.Second
	moment   = 20 * time.Millisecond
)

// readersWait reports whether a read would wait now: whether a transaction
// holds the copy's lock, or is waiting for it.
func readersWait(s *Store) bool {
	if s.mu.TryRLock() {
		s.mu.RUnlock()
		return false
	}
	return true
}

// waitUntil waits until cond holds, and fails the test if it doesn't within
// patience.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(patience); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after %v", what, patience)
		}
	}
}

// recv takes the next value from ch, and fails the test if none comes
// within patience.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(patience):
		t.Fatalf("%s: still waiting after %v", what, patience)
		panic("unreachable")
	}
}

// stillWaiting fails the test if ch gives a value within a moment.
func stillWaiting[T any](t *testing.T, what string, ch <-chan T) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s went ahead, with %v", what, v)
	case <-time.After(moment):
	}
}

// result is what a read of docs:1's field n gave.
type result struct {
	n   value.Value
	err error
}

func (r result) String() string { return fmt.Sprintf("n = %v, error %v", r.n, r.err) }

// readN reads docs:1's field n through Read.
func readN(s *Store) result {
	var n value.Value
	err := s.Read(func(r Reader) error {
		rec, err := r.Get("docs:1")
		if err != nil {
			return err
		}
		tb, _ := r.Table("docs")
		n = rec.Field(tb.Find("n"))
		return nil
	})
	return result{n, err}
}

// readLater starts readN on a goroutine of its own.
func readLater(s *Store) <-chan result {
	got := make(chan result, 1)
	go func() { got <- readN(s) }()
	return got
}

func wantN(t *testing.T, r result, n int64) {
	t.Helper()
	if r.err != nil || r.n != value.Int(n) {
		t.Fatalf("a read gave %v, want n = %d", r, n)
	}
}

func setN(tx *Tx, n int64) error {
	return tx.Put("docs:1", []format.Field{{Name: "n", Value: value.Int(n)}})
}

// newStoreN returns a store holding docs:1 with n = 1.
func newStoreN(t *testing.T) *Store {
	t.Helper()
	s := New()
	if _, err := s.Put(nil, "docs:1", []format.Field{{Name: "n", Value: value.Int(1)}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// begin begins a transaction on the test's goroutine, and rolls it back
// when the test ends, which does nothing to a transaction that has ended.
// So a check that fails while the transaction is open frees the goroutines
// waiting for it.
func begin(t *testing.T, s *Store) *Tx {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tx.Rollback)
	return tx
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestReadersWaitOnlyAfterTheFirstChange: a transaction holds up no reader
// until its first change, and writes that failed don't count as one. From
// the first change until the transaction ends, readers wait, and then see
// what it left.
func TestReadersWaitOnlyAfterTheFirstChange(t *testing.T) {
	s := newStoreN(t)
	for n, end := range map[int64]string{2: "commit", 3: "rollback"} {
		tx := begin(t, s)
		if readersWait(s) {
			t.Fatal("readers wait for a transaction that has made no change")
		}
		wantN(t, recv(t, "a read before the first change", readLater(s)), 1)
		if err := tx.Put("Docs:1", nil); !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("a put with a bad key gave %v", err)
		}
		if err := tx.Delete("docs:404"); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("a delete of a missing record gave %v", err)
		}
		if readersWait(s) {
			t.Fatal("readers wait for a transaction whose writes failed and changed nothing")
		}
		wantN(t, recv(t, "a read after writes that failed", readLater(s)), 1)

		ok(t, setN(tx, n))
		if !readersWait(s) {
			t.Fatal("readers don't wait after the transaction's first change")
		}
		got := readLater(s)
		stillWaiting(t, "a read after the first change", got)
		// A statement taken back keeps the lock, as more changes do.
		m := tx.Mark()
		ok(t, setN(tx, 10))
		tx.RollbackTo(m)
		if !readersWait(s) {
			t.Fatal("readers don't wait after RollbackTo")
		}
		stillWaiting(t, "a read after RollbackTo", got)

		want := n
		if end == "commit" {
			ok(t, tx.Commit(nil))
		} else {
			tx.Rollback()
			want = 1
		}
		wantN(t, recv(t, "a read that waited for the "+end, got), want)
		if readersWait(s) {
			t.Fatalf("readers wait after the %s", end)
		}
		if _, err := s.Put(nil, "docs:1", []format.Field{{Name: "n", Value: value.Int(1)}}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAWaitingWriterGoesFirst: a transaction's first change waits for the
// reads under way, and a read that comes while it waits goes after it,
// since Go's RWMutex lets a waiting writer in ahead of new readers. So a
// stream of reads can't hold a transaction off.
func TestAWaitingWriterGoesFirst(t *testing.T) {
	s := newStoreN(t)
	inRead, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- s.Read(func(Reader) error {
			close(inRead)
			<-release
			return nil
		})
	}()
	recv(t, "the long read", inRead)
	committed := make(chan error, 1)
	go func() {
		tx, err := s.Begin()
		if err == nil {
			err = setN(tx, 2) // waits for the long read
		}
		if err == nil {
			err = tx.Commit(nil)
		}
		committed <- err
	}()
	waitUntil(t, "the transaction's first change waits for the long read", func() bool { return readersWait(s) })
	stillWaiting(t, "the first change, under a read", committed)
	got := readLater(s)
	stillWaiting(t, "a read that came after the transaction", got)
	close(release)
	ok(t, recv(t, "the long read", readDone))
	ok(t, recv(t, "the transaction", committed))
	wantN(t, recv(t, "the read that came after the transaction", got), 2)
}

// TestReadersWaitThroughTheHandOff: Commit keeps the copy locked while it
// hands the change list on, so no reader sees the changes before write has
// put them in the file, nor ever when write fails.
func TestReadersWaitThroughTheHandOff(t *testing.T) {
	s := newStoreN(t)
	errLog := errors.New("the log couldn't write the batch")
	for _, fails := range []bool{false, true} {
		inWrite, release := make(chan struct{}), make(chan struct{})
		committed := make(chan error, 1)
		go func() {
			tx, err := s.Begin()
			if err == nil {
				err = setN(tx, 7)
			}
			if err == nil {
				err = tx.Commit(func([]format.Change) error {
					close(inWrite)
					<-release
					if fails {
						return errLog
					}
					return nil
				})
			}
			committed <- err
		}()
		recv(t, "the hand-off", inWrite)
		if !readersWait(s) {
			t.Fatal("readers can read while Commit hands the change list on")
		}
		got := readLater(s)
		stillWaiting(t, "a read during the hand-off", got)
		close(release)
		err := recv(t, "the commit", committed)
		want := int64(7)
		if fails {
			if !errors.Is(err, errLog) {
				t.Fatalf("a commit whose write failed gave %v", err)
			}
			want = 1
		} else {
			ok(t, err)
		}
		wantN(t, recv(t, "the read that waited", got), want)
		if _, err := s.Put(nil, "docs:1", []format.Field{{Name: "n", Value: value.Int(1)}}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInsideUpdate is the check behind errs.ErrInsideUpdate. On the
// goroutine running a transaction, a call through the store that would wait
// for that transaction fails at once. Outside, which Update calls before
// the write lock, and Begin fail at any time, and Read fails after the
// first change. Other goroutines wait instead, and the transaction's own
// goroutine is outside again once it ends. The transaction runs on a
// goroutine of its own, so a call that waits for itself fails the test
// instead of hanging it.
func TestInsideUpdate(t *testing.T) {
	s := newStoreN(t)
	if err := s.Outside(); err != nil {
		t.Fatalf("Outside with no transaction open: %v", err)
	}
	paused, carryOn := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			tx, err := s.Begin()
			if err != nil {
				return err
			}
			if err := s.Outside(); !errors.Is(err, errs.ErrInsideUpdate) {
				return fmt.Errorf("Outside inside the transaction gave %v", err)
			}
			if _, err := s.Begin(); !errors.Is(err, errs.ErrInsideUpdate) {
				return fmt.Errorf("Begin inside the transaction gave %v", err)
			}
			if r := readN(s); r.err != nil || r.n != value.Int(1) {
				return fmt.Errorf("a read before the first change gave %v", r)
			}
			if err := setN(tx, 2); err != nil {
				return err
			}
			if r := readN(s); !errors.Is(r.err, errs.ErrInsideUpdate) {
				return fmt.Errorf("a read after the first change gave %v", r)
			}
			if err := s.Outside(); !errors.Is(err, errs.ErrInsideUpdate) {
				return fmt.Errorf("Outside after the first change gave %v", err)
			}
			if _, err := s.Begin(); !errors.Is(err, errs.ErrInsideUpdate) {
				return fmt.Errorf("Begin after the first change gave %v", err)
			}
			rec, err := tx.Get("docs:1")
			if err != nil || rec.Field(0) != value.Int(2) {
				return fmt.Errorf("the transaction read %v, %v", rec, err)
			}
			paused <- struct{}{}
			<-carryOn
			if err := tx.Commit(nil); err != nil {
				return err
			}
			if err := s.Outside(); err != nil {
				return fmt.Errorf("Outside after the commit gave %v", err)
			}
			if r := readN(s); r.err != nil || r.n != value.Int(2) {
				return fmt.Errorf("a read after the commit gave %v", r)
			}
			tx, err = s.Begin()
			if err != nil {
				return fmt.Errorf("Begin after the commit gave %v", err)
			}
			tx.Rollback()
			return nil
		}()
	}()
	select {
	case <-paused:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(patience):
		t.Fatal("a call inside the transaction waited for the transaction")
	}
	// Another goroutine isn't inside: it waits for the transaction.
	if err := s.Outside(); err != nil {
		t.Fatalf("Outside on another goroutine gave %v", err)
	}
	began := make(chan error, 1)
	go func() {
		tx, err := s.Begin()
		if err == nil {
			tx.Rollback()
		}
		began <- err
	}()
	got := readLater(s)
	stillWaiting(t, "a second Begin", began)
	stillWaiting(t, "a read from another goroutine", got)
	close(carryOn)
	ok(t, recv(t, "the transaction", done))
	ok(t, recv(t, "the second Begin", began))
	wantN(t, recv(t, "the read from another goroutine", got), 2)
}

// TestInsideUpdateWhoeverChanges: the check goes by the goroutine that
// began the transaction, so a read through the store on that goroutine
// fails even when the first change came from a goroutine the transaction
// handed the Tx to.
func TestInsideUpdateWhoeverChanges(t *testing.T) {
	s := newStoreN(t)
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			tx, err := s.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			changed := make(chan error)
			go func() { changed <- setN(tx, 2) }()
			if err := <-changed; err != nil {
				return err
			}
			if r := readN(s); !errors.Is(r.err, errs.ErrInsideUpdate) {
				return fmt.Errorf("a read after a change from another goroutine gave %v", r)
			}
			return nil
		}()
	}()
	ok(t, recv(t, "the transaction", done))
	wantN(t, readN(s), 1)
}

// TestOneTransactionAtATime: transactions from several goroutines take
// turns, each adding 1 to n, so none is lost. Each holds the store for a
// couple of milliseconds, which puts Go's mutex into the mode where it
// hands the store straight to the transaction waiting longest. That's when
// a transaction ending could clear the number of the one that begins next,
// so each checks that it's still inside after its wait.
func TestOneTransactionAtATime(t *testing.T) {
	s := newStoreN(t)
	const workers, each = 3, 15
	done := make(chan error, workers)
	for range workers {
		go func() {
			done <- func() error {
				for range each {
					tx, err := s.Begin()
					if err != nil {
						return err
					}
					rec, err := tx.Get("docs:1")
					if err != nil {
						tx.Rollback()
						return err
					}
					time.Sleep(2 * time.Millisecond)
					if err := s.Outside(); !errors.Is(err, errs.ErrInsideUpdate) {
						tx.Rollback()
						return fmt.Errorf("a transaction found itself outside after a wait: %v", err)
					}
					if err := setN(tx, rec.Field(0).Int()+1); err != nil {
						tx.Rollback()
						return err
					}
					if err := tx.Commit(nil); err != nil {
						return err
					}
				}
				return nil
			}()
		}()
	}
	for range workers {
		ok(t, recv(t, "the transactions", done))
	}
	wantN(t, readN(s), 1+workers*each)
}

// TestGoroutineNumbers reads the number in runtime.Stack's first line, and
// finds none in a line that doesn't start as Go's do.
func TestGoroutineNumbers(t *testing.T) {
	for line, want := range map[string]int64{
		"goroutine 1 [running]:\nmain.main()\n":                    1,
		"goroutine 18 [running]:":                                  18,
		"goroutine 4611686018427387903 [chan receive (synctest)]:": 1<<62 - 1,
		"goroutine 7": 7,
	} {
		if got, ok := goroutineNumber([]byte(line)); !ok || got != want {
			t.Errorf("goroutineNumber(%q) = %d, %v, want %d", line, got, ok, want)
		}
	}
	for _, line := range []string{"", "goroutine", "goroutine ", "goroutine  1 [running]:", "goroutine x [running]:",
		"goroutine 0 [running]:", "goroutine -1 [running]:", "Goroutine 1 [running]:", "goroutine 12x [running]:",
		"goroutine 99999999999999999999 [running]:", " goroutine 1 [running]:"} {
		if got, ok := goroutineNumber([]byte(line)); ok {
			t.Errorf("goroutineNumber(%q) = %d, want none", line, got)
		}
	}
	// A goroutine keeps its number, and no two have the same.
	me := goroutine()
	if again := goroutine(); again != me {
		t.Fatalf("one goroutine got %d, then %d", me, again)
	}
	seen := map[int64]bool{me: true}
	numbers := make(chan int64)
	for range 20 {
		go func() { numbers <- goroutine() }()
	}
	for range 20 {
		n := <-numbers
		if seen[n] {
			t.Fatalf("two goroutines got %d", n)
		}
		seen[n] = true
	}
}

// writer is what TestUndoEachChange writes through.
type writer interface {
	Put(key string, fields []format.Field) error
	Delete(key string) error
	Drop(name string) error
	Link(from, typ, to string) error
	Unlink(from, typ, to string) error
	Apply(c format.Change) error
}

// TestUndoEachChange takes back each kind of change that S1.md lists, alone
// and together. A put can change a record's fields, add fields to its
// table, create the record or the table, set the table's vector size, or
// clear a vector. A delete takes a record out, and a drop a table with its
// records. Pairs make the same record or table twice, or one record twice.
// A link adds a link, an unlink takes one or several out, and a delete or a
// drop takes out every link of the records it takes, both ways: links
// between two of them, from a record to itself, and to and from records
// that stay. After a rollback the store must be as it was, and after a
// commit as it was before that rollback.
func TestUndoEachChange(t *testing.T) {
	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	vec := func(x ...float32) value.Value { return value.Vector(x) }
	cases := []struct {
		name  string
		write func(w writer) error
	}{
		{"a put that changes fields", func(w writer) error {
			return w.Put("docs:1", []format.Field{f("title", value.Text("z")), f("n", value.Null())})
		}},
		{"a put that adds fields", func(w writer) error {
			return w.Put("docs:2", []format.Field{f("Alpha", value.Int(1)), f("zeta", value.Text("z"))})
		}},
		{"a put that creates a record", func(w writer) error {
			return w.Put("docs:3", []format.Field{f("title", value.Text("c"))})
		}},
		{"a put that creates a table", func(w writer) error {
			return w.Put("notes:1", []format.Field{f("body", value.Text("x")), f("vec", vec(1, 2, 3))})
		}},
		{"a put that sets the vector size", func(w writer) error {
			return w.Put("pics:1", []format.Field{f("VEC", vec(1))})
		}},
		{"a put that clears a vector", func(w writer) error { return w.Put("docs:1", []format.Field{f("vec", value.Null())}) }},
		{"a delete", func(w writer) error { return w.Delete("docs:1") }},
		{"a drop", func(w writer) error { return w.Drop("docs") }},
		{"a drop of a table with no records", func(w writer) error { return w.Drop("empty") }},
		{"an applied create table and put", func(w writer) error {
			if err := w.Apply(format.Change{Op: format.CreateTable, Table: "made", Size: 2, Names: []string{"a", "vec"}}); err != nil {
				return err
			}
			return w.Apply(format.Change{Op: format.Put, Key: "made:1", Fields: []format.Field{f("a", value.Int(1)), f("vec", vec(1, 2))}})
		}},
		{"a delete and a new record with the same key", func(w writer) error {
			if err := w.Delete("docs:1"); err != nil {
				return err
			}
			return w.Put("docs:1", []format.Field{f("n", value.Int(2))})
		}},
		{"a drop and a new table with the same name", func(w writer) error {
			if err := w.Drop("docs"); err != nil {
				return err
			}
			return w.Put("docs:1", []format.Field{f("other", value.Int(1)), f("Vec", vec(1, 2, 3))})
		}},
		{"two puts on one record", func(w writer) error {
			if err := w.Put("docs:2", []format.Field{f("n", value.Int(1)), f("new", value.Int(1))}); err != nil {
				return err
			}
			return w.Put("docs:2", []format.Field{f("n", value.Int(2)), f("title", value.Null()), f("vec", vec(0, 1))})
		}},
		{"a new table, dropped and made again", func(w writer) error {
			if err := w.Put("notes:1", []format.Field{f("vec", vec(1))}); err != nil {
				return err
			}
			if err := w.Drop("notes"); err != nil {
				return err
			}
			return w.Put("notes:2", []format.Field{f("vec", vec(1, 2))})
		}},
		{"a link", func(w writer) error { return w.Link("docs:2", "new", "pics:1") }},
		{"a link there already, and a link from a record to itself", func(w writer) error {
			if err := w.Link("docs:1", "cites", "docs:2"); err != nil {
				return err
			}
			return w.Link("pics:1", "self", "pics:1")
		}},
		{"an unlink", func(w writer) error { return w.Unlink("docs:1", "cites", "docs:2") }},
		{"an unlink of every type", func(w writer) error {
			if err := w.Link("docs:1", "also", "docs:2"); err != nil {
				return err
			}
			return w.Unlink("docs:1", "", "docs:2")
		}},
		{"a delete of a record with links both ways and to itself", func(w writer) error { return w.Delete("docs:1") }},
		{"a drop of a table with links inside it, into it and out of it", func(w writer) error { return w.Drop("docs") }},
		{"a delete, and new links to a new record with the same key", func(w writer) error {
			if err := w.Delete("docs:1"); err != nil {
				return err
			}
			if err := w.Put("docs:1", nil); err != nil {
				return err
			}
			if err := w.Link("docs:1", "cites", "docs:2"); err != nil {
				return err
			}
			return w.Link("pics:1", "shows", "docs:1")
		}},
		{"two deletes of records linked to each other", func(w writer) error {
			if err := w.Delete("docs:2"); err != nil {
				return err
			}
			return w.Delete("docs:1")
		}},
		{"a drop, and links to a new table of the same name", func(w writer) error {
			if err := w.Drop("docs"); err != nil {
				return err
			}
			if err := w.Put("docs:1", nil); err != nil {
				return err
			}
			return w.Link("pics:1", "shows", "docs:1")
		}},
		{"an applied link and unlink", func(w writer) error {
			if err := w.Apply(format.Change{Op: format.Link, Key: "pics:1", Type: "new", To: "docs:2"}); err != nil {
				return err
			}
			return w.Apply(format.Change{Op: format.Unlink, Key: "docs:2", Type: "in", To: "pics:1"})
		}},
		{"many links into one record, and its delete", func(w writer) error {
			for i := range 3 * halfMax {
				key := fmt.Sprintf("many:%d", i*7919%(3*halfMax))
				if err := w.Put(key, nil); err != nil {
					return err
				}
				if err := w.Link(key, "to", "pics:1"); err != nil {
					return err
				}
				if err := w.Link("pics:1", "from", key); err != nil {
					return err
				}
			}
			return w.Delete("pics:1")
		}},
	}
	all := func(w writer) error {
		for _, c := range cases[:6] { // the puts: the rest undo what they do
			if err := c.write(w); err != nil {
				return err
			}
		}
		return nil
	}
	cases = append(cases, struct {
		name  string
		write func(w writer) error
	}{"every put at once", all})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := undoStore(t)
			before := dump(s)
			tx := begin(t, s)
			ok(t, c.write(tx))
			after := dump(s)
			if after == before {
				t.Fatal("the writes changed nothing")
			}
			tx.Rollback()
			checkInvariants(t, s)
			if got := dump(s); got != before {
				t.Fatalf("after the rollback the store holds\n%s\nwhere it held\n%s", got, before)
			}
			tx = begin(t, s)
			ok(t, c.write(tx))
			ok(t, tx.Commit(nil))
			checkInvariants(t, s)
			if got := dump(s); got != after {
				t.Fatalf("after the commit the store holds\n%s\nwhere the writes left\n%s", got, after)
			}
		})
	}
}

// undoStore returns the store TestUndoEachChange starts from: docs with two
// records and a vector size of 2, pics with a vector field but no size yet,
// and a table with no records. docs:1 and docs:2 link to each other, docs:1
// links to itself, pics:1 links to docs:1, and docs:2 to pics:1.
func undoStore(t *testing.T) *Store {
	t.Helper()
	s := New()
	link := func(from, typ, to string) format.Change {
		return format.Change{Op: format.Link, Key: from, Type: typ, To: to}
	}
	for _, c := range []format.Change{
		{Op: format.CreateTable, Table: "docs"},
		{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "n", Value: value.Int(1)}, {Name: "title", Value: value.Text("a")},
			{Name: "vec", Value: value.Vector([]float32{1, 0})}}},
		{Op: format.Put, Key: "docs:2", Fields: []format.Field{{Name: "title", Value: value.Text("b")}}},
		{Op: format.CreateTable, Table: "pics", Names: []string{"Vec"}},
		{Op: format.Put, Key: "pics:1"},
		{Op: format.CreateTable, Table: "empty"},
		link("docs:1", "cites", "docs:2"),
		link("docs:2", "cites", "docs:1"),
		link("docs:1", "self", "docs:1"),
		link("pics:1", "shows", "docs:1"),
		link("docs:2", "in", "pics:1"),
	} {
		ok(t, s.Apply(c))
	}
	return s
}

// TestARollbackLeavesReadsAlone: a table's field list cut back by a
// rollback has no room past its end, so a field added later never lands in
// the array that an earlier read handed out. That keeps S1's promise that
// what a read hands out stays as it was.
func TestARollbackLeavesReadsAlone(t *testing.T) {
	s := undoStore(t)
	tx := begin(t, s)
	ok(t, tx.Put("docs:2", []format.Field{{Name: "Alpha", Value: value.Int(1)}, {Name: "zeta", Value: value.Int(2)}}))
	tb, _ := tx.Table("docs")
	read := slices.Clone(tb.Fields)
	tx.Rollback()
	tx = begin(t, s)
	ok(t, tx.Put("docs:2", []format.Field{{Name: "beta", Value: value.Int(3)}}))
	ok(t, tx.Commit(nil))
	if !slices.Equal(tb.Fields, read) {
		t.Fatalf("a read gave the fields %q, which became %q", read, tb.Fields)
	}
	if now, _ := s.Table("docs"); !slices.Equal(now.Fields, []string{"n", "title", "vec", "beta"}) {
		t.Fatalf("docs has the fields %q", now.Fields)
	}
}

// TestAFailedWriteCarriesOn is P5's note for S2 and Q6: a write that fails
// inside a transaction undoes its own changes only, and the transaction
// carries on. So does a statement taken back to its mark.
func TestAFailedWriteCarriesOn(t *testing.T) {
	s := newStoreN(t)
	tx := begin(t, s)
	ok(t, setN(tx, 2))
	before := slices.Clone(tx.changes)
	for _, err := range []error{
		tx.Put("docs:1", []format.Field{{Name: "n", Value: value.Real(1)}, {Name: "key", Value: value.Int(1)}}),
		tx.Put("docs:1", []format.Field{{Name: "vec", Value: value.Text("[1]")}}),
		tx.Delete("docs:2"),
		tx.Drop("nosuch"),
		tx.Apply(format.Change{Op: format.CreateTable, Table: "docs"}),
	} {
		if err == nil {
			t.Fatal("a write that should fail worked")
		}
	}
	if !slices.EqualFunc(tx.changes, before, equalChange) {
		t.Fatalf("writes that failed left the changes %v", tx.changes)
	}
	// A statement of three writes, the last of which fails, is taken back.
	m := tx.Mark()
	ok(t, tx.Put("docs:2", nil))
	ok(t, setN(tx, 3))
	if err := tx.Put("docs:3", []format.Field{{Name: "N", Value: value.Int(1)}, {Name: "n", Value: value.Int(2)}}); err == nil {
		t.Fatal("a put of one field twice worked")
	}
	tx.RollbackTo(m)
	if rec, err := tx.Get("docs:1"); err != nil || rec.Field(0) != value.Int(2) {
		t.Fatalf("after RollbackTo, docs:1 is %v, %v", rec, err)
	}
	if _, err := tx.Get("docs:2"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("after RollbackTo, docs:2 gives %v", err)
	}
	if !slices.EqualFunc(tx.changes, before, equalChange) {
		t.Fatalf("after RollbackTo the changes are %v", tx.changes)
	}
	// A mark past the transaction's present point is a mistake.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("RollbackTo a mark past the present point didn't panic")
			}
		}()
		tx.RollbackTo(Mark{undo: len(tx.undo) + 1, changes: len(tx.changes)})
	}()
	// The transaction carries on, and commits what it kept.
	ok(t, tx.Put("docs:4", nil))
	var handed []format.Change
	ok(t, tx.Commit(func(c []format.Change) error { handed = c; return nil }))
	want := append(before, format.Change{Op: format.Put, Key: "docs:4"})
	if !slices.EqualFunc(handed, want, equalChange) {
		t.Fatalf("the commit handed on %v, want %v", handed, want)
	}
	wantN(t, readN(s), 2)
	if _, err := s.Get("docs:4"); err != nil {
		t.Fatal(err)
	}
}

// TestCommit: write is called only for a transaction with changes, and
// gets the change list. A write that fails or panics rolls the transaction
// back and frees the store, and the list handed on is write's to keep.
func TestCommit(t *testing.T) {
	s := newStoreN(t)
	tx := begin(t, s)
	if _, err := tx.Get("docs:1"); err != nil {
		t.Fatal(err)
	}
	ok(t, tx.Commit(func([]format.Change) error { return errors.New("write was called with no changes") }))

	tx = begin(t, s)
	ok(t, setN(tx, 2))
	var kept []format.Change
	ok(t, tx.Commit(func(c []format.Change) error { kept = c; return nil }))
	want := []format.Change{{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "n", Value: value.Int(2)}}}}
	if !slices.EqualFunc(kept, want, equalChange) {
		t.Fatalf("Commit handed on %v", kept)
	}

	errLog := errors.New("the log failed")
	tx = begin(t, s)
	ok(t, setN(tx, 3))
	if err := tx.Commit(func([]format.Change) error { return errLog }); !errors.Is(err, errLog) {
		t.Fatalf("a commit whose write failed gave %v", err)
	}
	wantN(t, readN(s), 2)
	if err := tx.Commit(nil); !errors.Is(err, errs.ErrClosed) {
		t.Fatalf("a commit after a failed one gave %v", err)
	}

	tx = begin(t, s)
	ok(t, setN(tx, 4))
	func() {
		defer func() {
			if p := recover(); p != "the codec broke" {
				t.Errorf("a write that panicked gave the panic %v", p)
			}
		}()
		tx.Commit(func([]format.Change) error { panic("the codec broke") })
	}()
	if readersWait(s) {
		t.Fatal("a commit whose write panicked kept the copy's lock")
	}
	wantN(t, readN(s), 2)

	// The store is free for the next transaction, and the list handed on
	// earlier is as it was.
	tx = begin(t, s)
	ok(t, setN(tx, 5))
	ok(t, tx.Commit(nil))
	wantN(t, readN(s), 5)
	if !slices.EqualFunc(kept, want, equalChange) {
		t.Fatalf("a later transaction changed the list handed on to %v", kept)
	}
}

// TestATransactionThatHasEnded: once Commit or Rollback has ended a
// transaction, its methods fail with ErrClosed, a cursor it gave gives no
// more records, and Rollback and RollbackTo do nothing. While it's open,
// the read that a later task writes says so, and its snapshot holds its
// changes.
func TestATransactionThatHasEnded(t *testing.T) {
	s := newStoreN(t)
	tx := begin(t, s)
	m := tx.Mark()
	for _, err := range readsToCome(tx) {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("a read for a later task gave %v", err)
		}
	}
	ok(t, setN(tx, 2))
	var snap []format.Change
	for c := range tx.Snapshot() {
		snap = append(snap, c)
	}
	want := []format.Change{
		{Op: format.CreateTable, Table: "docs", Names: []string{"n"}},
		{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "n", Value: value.Int(2)}}},
	}
	if !slices.EqualFunc(snap, want, equalChange) {
		t.Fatalf("the transaction's snapshot gave %v", snap)
	}
	for i, end := range []func(){func() { ok(t, tx.Commit(nil)) }, func() { tx.Rollback() }} {
		if i > 0 {
			tx = begin(t, s)
			ok(t, setN(tx, 3))
			m = tx.Mark()
		}
		cur, err := tx.Scan("docs:", "")
		ok(t, err)
		end()
		if rec, more := cur.Next(); more {
			t.Errorf("a cursor gave %v after its transaction ended", rec.Key)
		}
		closed := append(readsToCome(tx),
			tx.Put("docs:1", nil), tx.Delete("docs:1"), tx.Drop("docs"), tx.Apply(format.Change{Op: format.Delete, Key: "docs:1"}),
			tx.Link("docs:1", "x", "docs:1"), tx.Unlink("docs:1", "", "docs:1"), tx.Commit(nil))
		_, err = tx.Get("docs:1")
		closed = append(closed, err)
		_, err = tx.Scan("docs:", "")
		closed = append(closed, err)
		_, err = tx.Neighbours("docs:1", Out, "")
		closed = append(closed, err)
		_, err = tx.Walk("docs:1", Out, "", 1)
		closed = append(closed, err)
		for j, err := range closed {
			if !errors.Is(err, errs.ErrClosed) {
				t.Errorf("call %d after the end gave %v", j, err)
			}
		}
		if _, found := tx.Table("docs"); found {
			t.Error("Table found a table after the end")
		}
		tx.Rollback()
		tx.RollbackTo(m)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("ranging over Snapshot after the end didn't panic")
				}
			}()
			for range tx.Snapshot() {
			}
		}()
		wantN(t, readN(s), 2)
	}
}

// readsToCome makes the transaction's reads that later tasks write.
func readsToCome(tx *Tx) []error {
	_, nearest := tx.Nearest("docs", []float32{1}, 1, nil)
	return []error{nearest}
}

// TestDirectWritesWaitTheirTurn: the store's own writes, which have it to
// themselves, panic while a transaction is open.
func TestDirectWritesWaitTheirTurn(t *testing.T) {
	s := newStoreN(t)
	tx := begin(t, s)
	for name, write := range map[string]func(){
		"Put":    func() { s.Put(nil, "docs:2", nil) },
		"Delete": func() { s.Delete(nil, "docs:1") },
		"Drop":   func() { s.Drop(nil, "docs") },
		"Link":   func() { s.Link(nil, "docs:1", "x", "docs:1") },
		"Unlink": func() { s.Unlink(nil, "docs:1", "", "docs:1") },
		"Apply":  func() { s.Apply(format.Change{Op: format.Delete, Key: "docs:1"}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s on the store worked while a transaction was open", name)
				}
			}()
			write()
		}()
	}
	tx.Rollback()
	if _, err := s.Put(nil, "docs:2", nil); err != nil {
		t.Fatal(err)
	}
}
