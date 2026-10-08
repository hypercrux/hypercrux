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
// after it, as BETA.md's "Transactions" says. The same reads through the
// transaction see its changes.
func TestCallsThroughTheDatabaseInsideUpdate(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, db.Update(func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:1", hc.Fields{"n": 1, "vec": hc.Vector{1, 0}}))
		ok(t, tx.Put("docs:2", hc.Fields{"n": 2, "vec": hc.Vector{0, 1}}))
		ok(t, tx.Put("other:1", nil))
		return tx.Link("docs:1", "cites", "docs:2")
	}))
	writes := func(when string) {
		for name, err := range map[string]error{
			"Put":    db.Put("docs:3", hc.Fields{"n": 3}),
			"Delete": db.Delete("docs:1"),
			"Drop":   db.Drop("other"),
			"Link":   db.Link("docs:2", "cites", "docs:1"),
			"Unlink": db.Unlink("docs:1", "", "docs:2"),
			"Update": db.Update(func(*hc.Tx) error { return nil }),
			"Close":  db.Close(),
		} {
			if !errors.Is(err, hc.ErrInsideUpdate) {
				t.Errorf("%s: db.%s gave %v", when, name, err)
			}
		}
	}
	// reads makes each read through h, and says what each gave.
	reads := func(h betaCalls, check func() (hc.Report, error)) map[string]string {
		f, err := h.Get("docs:1")
		got := map[string]string{"Get": fmt.Sprint(f["n"], " ", err)}
		recs, err := h.Scan("docs:", "", 0)
		got["Scan"] = fmt.Sprint(len(recs), " ", err)
		links, err := h.Neighbours("docs:1", hc.Both, "")
		got["Neighbours"] = fmt.Sprint(links, " ", err)
		steps, err := h.Walk("docs:1", hc.Out, "", 2)
		got["Walk"] = fmt.Sprint(steps, " ", err)
		hits, err := h.Nearest("docs", hc.Vector{0, 1}, 1, "")
		got["Nearest"] = fmt.Sprint(len(hits) > 0 && hits[0].Key == "docs:2", " ", err)
		if check != nil {
			rep, err := check()
			got["Check"] = fmt.Sprint(rep.Records, " ", rep.Links, " ", err)
		}
		return got
	}
	committed := reads(db, db.Check)
	within(t, func() {
		ok(t, db.Update(func(tx *hc.Tx) error {
			writes("before the first change")
			for name, got := range reads(db, db.Check) {
				if got != committed[name] {
					t.Errorf("before the first change, db.%s gave %s, where %s was committed", name, got, committed[name])
				}
			}
			ok(t, tx.Put("docs:1", hc.Fields{"n": 10, "vec": hc.Vector{0, 1}}))
			ok(t, tx.Put("docs:3", hc.Fields{"n": 3}))
			ok(t, tx.Link("docs:2", "cites", "docs:3"))
			writes("after the first change")
			for name, got := range reads(db, db.Check) {
				if !strings.Contains(got, hc.ErrInsideUpdate.Error()) {
					t.Errorf("after the first change, db.%s gave %s", name, got)
				}
			}
			want := map[string]string{
				"Get":        "10 <nil>",
				"Scan":       "3 <nil>",
				"Neighbours": "[docs:1 -cites-> docs:2] <nil>",
				"Walk":       "[{docs:2 1} {docs:3 2}] <nil>",
				"Nearest":    "false <nil>", // docs:1 ties with docs:2 now, and comes first by key
			}
			for name, got := range reads(tx, nil) {
				if got != want[name] {
					t.Errorf("tx.%s gave %s, where %s is wanted", name, got, want[name])
				}
			}
			return nil
		}))
	})
	if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(10) {
		t.Fatalf("after the Update, docs:1 gives %v, %v", f, err)
	}
	for _, key := range []string{"docs:1", "other:1"} {
		if _, err := db.Get(key); err != nil {
			t.Fatalf("a write through db inside the Update took %s: %v", key, err)
		}
	}
	if links, err := db.Neighbours("docs:1", hc.Both, ""); err != nil || len(links) != 1 {
		t.Fatalf("a link or an unlink through db inside the Update went in: %v, %v", links, err)
	}
}

// TestReadersWaitFromTheFirstChange reads from other goroutines during an
// Update, with each of the reads through the database. Before the Update's
// first change each read goes ahead and sees what was committed, and from
// then on each waits until the commit, and sees it.
func TestReadersWaitFromTheFirstChange(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"n": 1, "vec": hc.Vector{1, 0}}))
	ok(t, db.Put("docs:2", hc.Fields{"n": 2, "vec": hc.Vector{0.6, 0.8}}))
	reads := map[string]func() string{
		"Get": func() string {
			f, err := db.Get("docs:1")
			return fmt.Sprint(f["n"], err)
		},
		"Scan": func() string {
			recs, err := db.Scan("docs:", "", 0)
			return fmt.Sprint(len(recs), err)
		},
		"Neighbours": func() string {
			links, err := db.Neighbours("docs:1", hc.Out, "")
			return fmt.Sprint(len(links), err)
		},
		"Walk": func() string {
			steps, err := db.Walk("docs:2", hc.In, "", 1)
			return fmt.Sprint(len(steps), err)
		},
		"Nearest": func() string {
			hits, err := db.Nearest("docs", hc.Vector{0, 1}, 1, "")
			if err != nil || len(hits) == 0 {
				return fmt.Sprint(hits, err)
			}
			return hits[0].Key
		},
		"Check": func() string {
			rep, err := db.Check()
			return fmt.Sprint(rep.Records, rep.Links, err)
		},
	}
	start := func() map[string]chan string {
		out := map[string]chan string{}
		for name, read := range reads {
			c := make(chan string, 1)
			go func() { c <- read() }()
			out[name] = c
		}
		return out
	}
	before := map[string]string{"Get": "1 <nil>", "Scan": "2 <nil>", "Neighbours": "0 <nil>", "Walk": "0 <nil>", "Nearest": "docs:2", "Check": "2 0 <nil>"}
	after := map[string]string{"Get": "10 <nil>", "Scan": "3 <nil>", "Neighbours": "1 <nil>", "Walk": "1 <nil>", "Nearest": "docs:1", "Check": "3 1 <nil>"}
	var waiting map[string]chan string
	ok(t, db.Update(func(tx *hc.Tx) error {
		for name, c := range start() {
			if got := <-c; got != before[name] {
				t.Errorf("before the first change, db.%s gave %s, where %s was committed", name, got, before[name])
			}
		}
		ok(t, tx.Put("docs:1", hc.Fields{"n": 10, "vec": hc.Vector{0, 1}}))
		ok(t, tx.Put("docs:3", nil))
		ok(t, tx.Link("docs:1", "cites", "docs:2"))
		waiting = start()
		time.Sleep(50 * time.Millisecond)
		for name, c := range waiting {
			select {
			case got := <-c:
				t.Errorf("after the first change, db.%s went ahead and gave %s", name, got)
				delete(waiting, name)
			default:
			}
		}
		return nil
	}))
	for name, c := range waiting {
		if got := <-c; got != after[name] {
			t.Errorf("db.%s, which waited for the commit, gave %s, where %s is wanted", name, got, after[name])
		}
	}
}

// TestCallsAfterTheEnd checks the calls that come too late: on a
// transaction once its Update has returned, on a database once it's
// closed, and on a DB or Tx that nothing opened. Each fails with ErrClosed,
// before it checks its arguments, and Close itself does nothing the second
// time.
func TestCallsAfterTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, err := hc.Open(path)
	ok(t, err)
	var kept *hc.Tx
	ok(t, db.Update(func(tx *hc.Tx) error {
		kept = tx
		return tx.Put("docs:1", hc.Fields{"n": 1})
	}))
	// Every argument breaks a rule, so a call that checked them first would
	// give ErrInvalid.
	calls := func(h betaCalls) map[string]error {
		return map[string]error{
			"Get":        errOf(h.Get("Bad")),
			"Put":        h.Put("Bad key", nil),
			"Delete":     h.Delete("Bad"),
			"Scan":       errOf(h.Scan("Bad", "", -1)),
			"Drop":       h.Drop("Bad"),
			"Link":       h.Link("Bad", "", "Bad"),
			"Unlink":     h.Unlink("Bad", "", "Bad"),
			"Neighbours": errOf(h.Neighbours("Bad", 7, "")),
			"Walk":       errOf(h.Walk("Bad", 7, "", 0)),
			"Nearest":    errOf(h.Nearest("Bad", nil, 0, "")),
		}
	}
	for _, tx := range []*hc.Tx{kept, new(hc.Tx)} {
		for name, err := range calls(tx) {
			if !errors.Is(err, hc.ErrClosed) || errors.Is(err, hc.ErrInvalid) || errors.Is(err, hc.ErrNotFound) {
				t.Errorf("tx.%s on an ended transaction gave %v", name, err)
			}
		}
	}
	ok(t, db.Close())
	ok(t, db.Close())
	for _, d := range []*hc.DB{db, new(hc.DB)} {
		all := calls(d)
		all["Update"] = d.Update(func(*hc.Tx) error { return nil })
		all["Check"] = errOf(d.Check())
		for name, err := range all {
			if !errors.Is(err, hc.ErrClosed) || errors.Is(err, hc.ErrInvalid) || errors.Is(err, hc.ErrNotFound) {
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
// each with its own log and copy. Each handle's reads see the other's
// commits before its next read, since every read through a DB follows the
// file first (F9). Each Update catches up with the other's commits under
// the write lock before its function runs, so read-modify-write cycles from
// both lose nothing, and the lock keeps the two apart.
func TestTwoHandlesOnOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	a := open(t, path)
	b, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{Wait: 300 * time.Millisecond})
	ok(t, err)
	t.Cleanup(func() { b.Close() })

	ok(t, a.Put("docs:1", hc.Fields{"n": 1}))
	if f, err := b.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("b's next read after a's commit gives %v, %v", f, err)
	}
	ok(t, b.Update(func(tx *hc.Tx) error {
		if f, err := tx.Get("docs:1"); err != nil || f["n"] != int64(1) {
			t.Errorf("b's Update didn't catch up with a's commit: %v, %v", f, err)
		}
		return tx.Put("docs:2", hc.Fields{"n": 2})
	}))
	if f, err := a.Get("docs:2"); err != nil || f["n"] != int64(2) {
		t.Errorf("a's next read after b's commit gives %v, %v", f, err)
	}
	for _, h := range []*hc.DB{a, b} {
		if rep, err := h.Check(); err != nil || rep.Records != 2 {
			t.Errorf("a handle's Check gives %+v, %v after both commits", rep, err)
		}
	}

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
