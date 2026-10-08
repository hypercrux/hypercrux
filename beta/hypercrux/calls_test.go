// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// within runs fn on a goroutine of its own and fails the test if it takes
// longer than 10 seconds, so a call that waits for itself fails instead of
// hanging the test.
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a call inside an Update waited for its own Update")
	}
}

// TestCallsThroughTheDatabaseInsideUpdate makes every call through the
// database inside its own Update. A write, Close and a nested Update fail at
// once with ErrInsideUpdate, before the first change and after it. A read
// goes ahead before the first change, seeing what was committed, and fails
// after it, as BETA.md's "Transactions" says.
func TestCallsThroughTheDatabaseInsideUpdate(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	writes := func(when string) {
		for name, err := range map[string]error{
			"Put":    db.Put("docs:2", hc.Fields{"n": 2}),
			"Delete": db.Delete("docs:1"),
			"Update": db.Update(func(*hc.Tx) error { return nil }),
			"Close":  db.Close(),
		} {
			if !errors.Is(err, hc.ErrInsideUpdate) {
				t.Errorf("%s: db.%s gave %v", when, name, err)
			}
		}
	}
	within(t, func() {
		ok(t, db.Update(func(tx *hc.Tx) error {
			writes("before the first change")
			if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(1) {
				t.Errorf("before the first change, db.Get gave %v, %v", f, err)
			}
			if _, err := db.Check(); err != nil {
				t.Errorf("before the first change, db.Check gave %v", err)
			}
			ok(t, tx.Put("docs:1", hc.Fields{"n": 10}))
			writes("after the first change")
			if _, err := db.Get("docs:1"); !errors.Is(err, hc.ErrInsideUpdate) {
				t.Errorf("after the first change, db.Get gave %v", err)
			}
			if _, err := db.Check(); !errors.Is(err, hc.ErrInsideUpdate) {
				t.Errorf("after the first change, db.Check gave %v", err)
			}
			if f, err := tx.Get("docs:1"); err != nil || f["n"] != int64(10) {
				t.Errorf("tx.Get gave %v, %v", f, err)
			}
			return nil
		}))
	})
	if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(10) {
		t.Fatalf("after the Update, docs:1 gives %v, %v", f, err)
	}
	if _, err := db.Get("docs:2"); !errors.Is(err, hc.ErrNotFound) {
		t.Fatalf("a Put through db inside the Update went in: %v", err)
	}
}

// TestReadersWaitFromTheFirstChange reads from another goroutine during an
// Update: before its first change the read goes ahead and sees what was
// committed, and from then on it waits until the commit, and sees it.
func TestReadersWaitFromTheFirstChange(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	read := func() chan any {
		c := make(chan any, 1)
		go func() {
			f, err := db.Get("docs:1")
			if err != nil {
				c <- err
				return
			}
			c <- f["n"]
		}()
		return c
	}
	var waiting chan any
	ok(t, db.Update(func(tx *hc.Tx) error {
		if n := <-read(); n != int64(1) {
			t.Errorf("before the first change, a read gave %v", n)
		}
		ok(t, tx.Put("docs:1", hc.Fields{"n": 2}))
		waiting = read()
		select {
		case n := <-waiting:
			t.Errorf("after the first change, a read went ahead and gave %v", n)
		case <-time.After(50 * time.Millisecond):
		}
		return nil
	}))
	if n := <-waiting; n != int64(2) {
		t.Fatalf("the read that waited for the commit gave %v", n)
	}
}

// TestCallsAfterTheEnd checks the calls that come too late: on a
// transaction once its Update has returned, on a database once it's
// closed, and on a DB or Tx that nothing opened. Each fails with ErrClosed,
// and Close itself does nothing the second time.
func TestCallsAfterTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, err := hc.Open(path)
	ok(t, err)
	var kept *hc.Tx
	ok(t, db.Update(func(tx *hc.Tx) error {
		kept = tx
		return tx.Put("docs:1", hc.Fields{"n": 1})
	}))
	for _, tx := range []*hc.Tx{kept, new(hc.Tx)} {
		for name, err := range map[string]error{
			"Get":    errOf(tx.Get("docs:1")),
			"Put":    tx.Put("Bad key", nil),
			"Delete": tx.Delete("docs:1"),
		} {
			wantErr(t, err, hc.ErrClosed)
			if name == "Put" && errors.Is(err, hc.ErrInvalid) {
				t.Errorf("tx.Put on an ended transaction checked its key: %v", err)
			}
		}
	}
	ok(t, db.Close())
	ok(t, db.Close())
	for _, d := range []*hc.DB{db, new(hc.DB)} {
		for name, err := range map[string]error{
			"Get":    errOf(d.Get("docs:1")),
			"Put":    d.Put("docs:1", nil),
			"Delete": d.Delete("docs:1"),
			"Update": d.Update(func(*hc.Tx) error { return nil }),
			"Check":  errOf(d.Check()),
		} {
			if !errors.Is(err, hc.ErrClosed) {
				t.Errorf("db.%s on a closed database gave %v", name, err)
			}
		}
		ok(t, d.Close())
	}
	if db.Path() != path {
		t.Errorf("a closed database's path is %q", db.Path())
	}
}

// TestCloseWaitsForAnUpdate closes a database while an Update on another
// goroutine is under way. Close returns once the Update has committed, and
// the commit is in the file.
func TestCloseWaitsForAnUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, err := hc.Open(path)
	ok(t, err)
	inside, release, updated := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		updated <- db.Update(func(tx *hc.Tx) error {
			if err := tx.Put("docs:1", hc.Fields{"n": 1}); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while an Update was under way", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	ok(t, <-updated)
	ok(t, <-closed)
	if f, err := open(t, path).Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Fatalf("the Update that Close waited for gives %v, %v after a reopen", f, err)
	}
}

// TestTwoHandlesOnOneFile opens one file twice, as two processes would,
// each with its own log and copy, as far as G1 goes: each Update catches
// up with the other's commits under the write lock before its function
// runs, so read-modify-write cycles from both lose nothing, and the lock
// keeps the two apart. A handle's reads outside an Update see the other's
// commits only once an Update of its own has caught up; following the
// other handle's commits as they come is F6's.
func TestTwoHandlesOnOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	a := open(t, path)
	b, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{Wait: 300 * time.Millisecond})
	ok(t, err)
	t.Cleanup(func() { b.Close() })

	ok(t, a.Put("docs:1", hc.Fields{"n": 1}))
	ok(t, b.Update(func(tx *hc.Tx) error {
		if f, err := tx.Get("docs:1"); err != nil || f["n"] != int64(1) {
			t.Errorf("b's Update didn't catch up with a's commit: %v, %v", f, err)
		}
		return tx.Put("docs:2", hc.Fields{"n": 2})
	}))
	if f, err := b.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("after its Update, b's copy has %v, %v for a's commit", f, err)
	}
	ok(t, a.Update(func(tx *hc.Tx) error {
		_, err := tx.Get("docs:2")
		return err
	}))

	// The lock keeps them apart: while a's Update runs, b waits, and gives
	// up after its wait.
	inside, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- a.Update(func(tx *hc.Tx) error {
			close(inside)
			<-release
			return tx.Put("docs:3", hc.Fields{"n": 3})
		})
	}()
	<-inside
	wantErr(t, b.Put("docs:4", hc.Fields{"n": 4}), hc.ErrLockTimeout)
	close(release)
	ok(t, <-done)
	ok(t, b.Put("docs:4", hc.Fields{"n": 4}))

	// Goroutines on both handles add one to a counter, each in an Update
	// that reads it first.
	goroutines, adds := 3, 15
	if testing.Short() {
		adds = 5
	}
	ok(t, a.Put("count:1", hc.Fields{"n": 0}))
	var wg sync.WaitGroup
	var mu sync.Mutex
	committed := [2]int{} // by handle
	errs := make(chan error, 2*goroutines)
	for g := range 2 * goroutines {
		db := []*hc.DB{a, b}[g%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range adds {
				err := db.Update(func(tx *hc.Tx) error {
					f, err := tx.Get("count:1")
					if err != nil {
						return err
					}
					if err := tx.Put("count:1", hc.Fields{"n": f["n"].(int64) + 1}); err != nil {
						return err
					}
					return tx.Put(fmt.Sprintf("add:%d-%d", g, i), nil)
				})
				if errors.Is(err, hc.ErrLockTimeout) {
					continue // b's short wait ran out while a's goroutines took turns
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				committed[g%2]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if committed[0] == 0 || committed[1] == 0 {
		t.Fatalf("the two handles committed %d and %d adds, so they didn't take turns", committed[0], committed[1])
	}
	// Each handle catches up with the other's last commits in an Update
	// with no changes, which commits nothing.
	ok(t, a.Update(func(*hc.Tx) error { return nil }))
	ok(t, b.Update(func(*hc.Tx) error { return nil }))
	c := open(t, path)
	rep, err := c.Check()
	ok(t, err)
	count, err := c.Get("count:1")
	ok(t, err)
	// Every add that committed is one record and one step of the counter.
	if adds := committed[0] + committed[1]; count["n"] != int64(adds) || rep.Records != adds+5 {
		t.Fatalf("the counter is %v, and the file holds %d records, after %d adds committed: an Update lost another's commit",
			count["n"], rep.Records, adds)
	}
	for _, db := range []*hc.DB{a, b} {
		if f, err := db.Get("count:1"); err != nil || f["n"] != count["n"] {
			t.Errorf("a handle's copy has the counter at %v, %v, and the file at %v", f, err, count["n"])
		}
	}
}

// TestTheVectorGoesThroughFromGo puts the package's own Vector, which
// value.FromGo knows only by its path and name, into the vector field and
// into another field, and a pointer to one, as 0.x's tests do with 0.x's
// Vector. Get gives a Vector back, of its own: changing it changes nothing
// in the database, and nor does changing the one given to Put.
func TestTheVectorGoesThroughFromGo(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "test.hcx"))
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()

	v := hc.Vector{0.5, -0.25, 1}
	ok(t, db.Put("docs:1", hc.Fields{"vec": v}))
	v[0] = 99
	f, err := db.Get("docs:1")
	ok(t, err)
	got, isVector := f["vec"].(hc.Vector)
	if !isVector || show(f) != show(hc.Fields{"vec": hc.Vector{0.5, -0.25, 1}}) {
		t.Fatalf("Get gave %#v for the vector put", f["vec"])
	}
	got[1] = 99
	if f, _ := db.Get("docs:1"); show(f) != show(hc.Fields{"vec": hc.Vector{0.5, -0.25, 1}}) {
		t.Fatalf("changing what Get gave changed the database: %v", f)
	}
	ok(t, db.Put("imgs:1", hc.Fields{"VEC": hc.Vector{1, 2}}))
	if f, _ := db.Get("imgs:1"); f["VEC"] == nil {
		t.Fatalf("a vector field spelt VEC comes back as %v", f)
	}

	pv, pz := hc.Vector{1}, zx.Vector{1}
	for _, c := range []struct {
		name       string
		beta, zero any
	}{
		{"emb", hc.Vector{1, 2}, zx.Vector{1, 2}},
		{"emb", &pv, &pz},
		{"vec", &pv, &pz},
		{"vec", []hc.Vector{{1}}, []zx.Vector{{1}}},
	} {
		bErr := db.Put("other:1", hc.Fields{c.name: c.beta})
		zErr := z.Put("other:1", zx.Fields{c.name: c.zero})
		if zErr == nil || describeErr(bErr) != describeErr(zErr) {
			t.Errorf("Put(%s: %T) gives %v, and 0.x %v", c.name, c.beta, bErr, zErr)
		}
	}
}

// TestTableOfIs0xs holds TableOf to 0.x's on fixed and random keys: the
// same table, or the same error, of the same kind.
func TestTableOfIs0xs(t *testing.T) {
	keys := []string{"", "docs", "docs:", ":7", "Docs:7", "7docs:1", "hc_keys:1", "sqlite_master:1", "hc:1",
		"my-docs:1", "docs:\x00", "docs:\xff", "docs:a:b", "docs:" + strings.Repeat("x", hc.MaxKeyLen-5),
		"docs:" + strings.Repeat("x", hc.MaxKeyLen-4), "a:é", strings.Repeat("a", 63) + ":1", strings.Repeat("a", 64) + ":1"}
	r := rand.New(rand.NewPCG(5, 1))
	parts := []string{"a", "z", "A", "0", "_", ":", "-", " ", "\x00", "\xff", "é", "hc_", "sqlite_", "docs"}
	for range 3000 {
		var b strings.Builder
		for n := r.IntN(6); n > 0; n-- {
			b.WriteString(parts[r.IntN(len(parts))])
		}
		keys = append(keys, b.String())
	}
	for _, k := range keys {
		got, gotErr := hc.TableOf(k)
		want, wantErr := zx.TableOf(k)
		if got != want || !sameError(gotErr, wantErr) {
			t.Errorf("TableOf(%q) = %q, %v; 0.x gives %q, %v", k, got, gotErr, want, wantErr)
		}
	}
}

// TestCheckCounts checks the counts Check gives until G7 adds the rest: a
// table stays once its records have gone, and a record counts as having a
// vector until its vector is cleared.
func TestCheckCounts(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	count := func(want hc.Report) {
		t.Helper()
		if rep, err := db.Check(); err != nil || !rep.OK() || rep.Tables != want.Tables || rep.Records != want.Records ||
			rep.Links != want.Links || rep.Vectors != want.Vectors {
			t.Fatalf("Check gave %+v, %v, where %+v is wanted", rep, err, want)
		}
	}
	count(hc.Report{})
	ok(t, db.Put("docs:1", hc.Fields{"vec": hc.Vector{1, 2}}))
	ok(t, db.Put("docs:2", hc.Fields{"vec": hc.Vector{2, 1}, "n": 1}))
	ok(t, db.Put("people:1", nil))
	count(hc.Report{Tables: 2, Records: 3, Vectors: 2})
	ok(t, db.Put("docs:2", hc.Fields{"vec": nil}))
	ok(t, db.Delete("people:1"))
	count(hc.Report{Tables: 2, Records: 2, Vectors: 1})
}
