// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// When a compaction is due (C1): Due's thresholds one by one, with the real
// store as the Target, which counts the dead bytes; the count starting
// again at a compaction; a Target that doesn't count; opening a log at the
// limit on commits; and the closing test, in which every process takes the
// same choice from the same file.

// choice is what Due weighs and what it says, as one Log sees the file,
// holding the write lock.
type choice struct {
	due     bool
	dead    int64
	commits uint64
	end     int64
}

func (c choice) String() string {
	return fmt.Sprintf("due %v, %d bytes dead, commits past the compacted part %d, the log ending at %d", c.due, c.dead, c.commits, c.end)
}

// chosen is l's choice. l holds the write lock.
func chosen(l *Log) choice { return choice{l.Due(), l.Dead(), l.Commits(), l.end} }

// rule is C1's rule, worked out by the test: due once floor bytes are dead
// and half the file, or once limit commits follow the compacted part.
func rule(c choice, floor int64, limit uint64) bool {
	return c.commits >= limit || c.dead >= floor && 2*c.dead >= c.end
}

// putOver commits a put of key with the field s holding n letters on d, as
// Update makes a commit, and returns d's choice straight after it, under
// the same lock, as Update asks for it.
func putOver(t *testing.T, d *db, key string, n int) choice {
	t.Helper()
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer d.l.Unlock()
	tx, err := d.c.s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := tx.Put(key, []format.Field{{Name: "s", Value: value.Text(strings.Repeat("x", n))}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(d.l.Append); err != nil {
		t.Fatal(err)
	}
	return chosen(d.l)
}

// overSize is what a put of key with the field s holding n letters makes
// dead when it writes over one of the same size: the earlier put's key and
// count of fields, and its value with the field's name.
func overSize(key string, n int) int64 { return 1 + 2 + int64(len(key)) + 2 + 2 + 1 + 1 + 4 + int64(n) }

// untilDue puts key with n letters again and again on d, checking after each
// commit that Due says what the rule says for floor and limit, until it says
// a compaction is due, and returns how many commits that took and the choice.
func untilDue(t *testing.T, d *db, key string, n int, floor int64, limit uint64) (int, choice) {
	t.Helper()
	for i := 1; i <= 1000; i++ {
		c := putOver(t, d, key, n)
		if c.due != rule(c, floor, limit) {
			t.Fatalf("commit %d: %v, where the rule says %v", i, c, !c.due)
		}
		if c.due {
			return i, c
		}
	}
	t.Fatalf("no compaction came due in 1,000 commits of %s", key)
	return 0, choice{}
}

// TestDue: a compaction is due once the batches since the compacted part
// have made half the file dead and a megabyte, and not before.
//
//   - In generation 1, a record written over 100 KB at a time is half dead
//     by its third commit, and the megabyte decides: due at the twelfth,
//     with exactly 11 of its puts dead.
//   - Six records of 300 KB and that one, compacted, then one of them
//     written over: a megabyte is dead by the fourth commit, and half the
//     file decides, later, at the first commit that makes half of it dead.
//   - After a compaction that failed before the switch, the next isn't due,
//     though the rule says it is, until the file has grown by as much again
//     as it had since its last compaction.
//
// Due is false without the lock, and for a stuck Log.
func TestDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	n, c := untilDue(t, d, "a:1", 100<<10, minDead, maxCommits)
	if n != 12 || c.dead != 11*overSize("a:1", 100<<10) {
		t.Errorf("in generation 1, due at commit %d, with %v, where commit 12 is wanted, with 11 puts dead", n, c)
	}
	if d.l.Due() {
		t.Error("due without the lock")
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		putOver(t, d, "b:"+strconv.Itoa(i), 300<<10)
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	start := putOver(t, d, "c:1", 1)
	if start.dead != 0 || start.commits != 1 {
		t.Fatalf("after a compaction and one new record, %v", start)
	}
	// Each commit over b:0 makes one of its puts dead, and adds a batch of
	// that put, with its head, its checksum and its marker. Half the file is
	// dead at the first commit j where 2j puts reach the file's end.
	over, step := overSize("b:0", 300<<10), overSize("b:0", 300<<10)+format.BatchHeadSize+4+format.MarkerSize
	half := 1
	for 2*int64(half)*over < start.end+int64(half)*step {
		half++
	}
	if half <= 4 {
		t.Fatalf("half the file is dead after %d commits, where the test wants more than four", half)
	}
	for i := 1; ; i++ {
		c := putOver(t, d, "b:0", 300<<10)
		if c.due != rule(c, minDead, maxCommits) || c.dead != int64(i)*over {
			t.Fatalf("writing over b:0, commit %d: %v, where the rule says %v, with %d bytes dead", i, c, !c.due, int64(i)*over)
		}
		if i == 4 && (c.due || c.dead < minDead) {
			t.Fatalf("writing over b:0, commit 4: %v, where a megabyte is dead and half the file isn't", c)
		}
		if c.due {
			if i != half {
				t.Errorf("writing over b:0, due at commit %d, where half the file is dead at commit %d", i, half)
			}
			break
		}
	}

	// A compaction that fails holds the next off until the file has grown as
	// much again.
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	trap.set(failNth("sync db.compact", 1))
	err := d.compactLocked()
	trap.set(nil)
	if err == nil {
		t.Fatal("the compaction worked with its sync failing")
	}
	retry := d.l.end + (d.l.end - int64(d.l.hdr.CompactedEnd))
	d.l.Unlock()
	for {
		c := putOver(t, d, "b:0", 300<<10)
		switch {
		case c.end < retry && c.due:
			t.Fatalf("due at %v, before the file has grown to %d after a failed compaction", c, retry)
		case c.end >= retry && !c.due:
			t.Fatalf("not due at %v, once the file has grown to %d after a failed compaction", c, retry)
		}
		if c.due {
			break
		}
	}

	// A stuck Log is never due.
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	d.l.stuck = fmt.Errorf("%w: a test's", errs.ErrStuck)
	due := d.l.Due()
	d.l.stuck = nil
	d.l.Unlock()
	if due {
		t.Error("a stuck Log said a compaction is due")
	}
}

// TestMinGrowthSetsWhenACompactionIsDue: with Options.MinGrowth set, a
// compaction is due once that many bytes are dead, in place of a megabyte,
// as long as half the file is: a record of 300 letters written over is due
// at its eighth commit in generation 1, and at the seventh after a
// compaction, which holds the record as it stands.
func TestMinGrowthSetsWhenACompactionIsDue(t *testing.T) {
	const growth = 2000
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{MinGrowth: growth})
	if n, c := untilDue(t, d, "a:1", 300, growth, maxCommits); n != 8 || c.dead != 7*overSize("a:1", 300) {
		t.Errorf("in generation 1, due at commit %d, with %v, where the eighth is wanted", n, c)
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	if n, c := untilDue(t, d, "a:1", 300, growth, maxCommits); n != 7 || c.dead != 7*overSize("a:1", 300) {
		t.Errorf("after a compaction, due at commit %d, with %v, where the seventh is wanted", n, c)
	}
}

// TestTheLimitOnCommits: a database that only grows makes nothing dead, so a
// compaction is due only once Options.MaxCommits commits follow the
// compacted part: at the fifth commit, and after the compaction at the fifth
// past it, counted from the compacted part's last batch, whatever the
// compacted part holds. After a compaction that failed at the limit, the
// next is due once the file has grown as much again.
func TestTheLimitOnCommits(t *testing.T) {
	const limit = 5
	path := filepath.Join(t.TempDir(), "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{MaxCommits: limit, MinGrowth: 1})
	next := 0
	grow := func() choice {
		t.Helper()
		next++
		return putOver(t, d, "g:"+strconv.Itoa(next), 2000)
	}
	for round := range 3 {
		for i := uint64(1); i <= limit; i++ {
			c := grow()
			if c.dead != 0 || c.commits != i || c.due != (i == limit) {
				t.Fatalf("round %d, commit %d: %v, where only the fifth commit is due, with nothing dead", round, i, c)
			}
		}
		if err := d.compact(); err != nil {
			t.Fatal(err)
		}
		if d.l.compacted == 0 || d.l.Commits() != 0 {
			t.Fatalf("after the compaction, the compacted part has %d batches and %d commits follow it", d.l.compacted, d.l.Commits())
		}
	}
	for i := uint64(1); i <= limit; i++ {
		grow()
	}
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	if !d.l.Due() {
		t.Fatal("not due at the limit")
	}
	trap.set(failNth("sync db.compact", 1))
	err := d.compactLocked()
	trap.set(nil)
	if err == nil {
		t.Fatal("the compaction worked with its sync failing")
	}
	retry := d.l.end + (d.l.end - int64(d.l.hdr.CompactedEnd))
	d.l.Unlock()
	for {
		c := grow()
		if c.due != (c.end >= retry) {
			t.Fatalf("after a compaction that failed at the limit, %v, where the next is due once the log reaches %d", c, retry)
		}
		if c.due {
			break
		}
	}
}

// TestACompactionStartsTheCountAgain: after a Log's own compaction, its
// count of dead bytes and its commits start from nothing, as they do for a
// Log that opens the compacted file afresh, and for one that had the old file
// open and reloads. The commits after it count the same in all three.
func TestACompactionStartsTheCountAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	follower := openDBT(t, fsys.OS{}, path, Options{})
	fill(t, d, 1, 20)
	if c := lockedChoice(t, d); c.dead == 0 || c.commits != 20 {
		t.Fatalf("20 commits of dead data: %v", c)
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	if c := lockedChoice(t, d); c.dead != 0 || c.commits != 0 {
		t.Fatalf("after its own compaction, the Log counts %v", c)
	}
	fill(t, d, 21, 30)
	want := lockedChoice(t, d)
	if want.dead == 0 {
		t.Fatal("10 commits of dead data after the compaction made nothing dead")
	}
	if got := followedChoice(t, follower); got != want {
		t.Errorf("a Log that reloads counts %v, where the one that compacted counts %v", got, want)
	}
	if got := openedChoice(t, path, Options{}); got != want {
		t.Errorf("a Log opened afresh counts %v, where the one that compacted counts %v", got, want)
	}
}

// lockedChoice is d's choice, holding the write lock for it.
func lockedChoice(t *testing.T, d *db) choice {
	t.Helper()
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer d.l.Unlock()
	return chosen(d.l)
}

// followedChoice is d's choice once it has followed the file, as a process's
// reads follow it, reloading it when another file has taken the path.
func followedChoice(t *testing.T, d *db) choice {
	t.Helper()
	c, err := followed(d)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// followed follows the file through d's Log, as a process's reads do, with
// Reload when Follow asks for it, then takes the write lock for d's choice.
func followed(d *db) (choice, error) {
	for tries := 0; ; tries++ {
		err := d.l.Follow()
		if !errors.Is(err, ErrReplaced) {
			if err != nil {
				return choice{}, err
			}
			break
		}
		if tries == maxTries {
			return choice{}, errors.New("other files kept taking the path")
		}
		if err := d.l.Reload(); err != nil {
			return choice{}, err
		}
	}
	if err := d.l.Lock(); err != nil {
		return choice{}, err
	}
	defer d.l.Unlock()
	return chosen(d.l), nil
}

// openedChoice is the choice of a Log that opens the file afresh, as a
// process started after the commits would.
func openedChoice(t *testing.T, path string, o Options) choice {
	t.Helper()
	c, err := opened(path, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// opened opens the database at path with o, takes the write lock for the
// choice, and closes it.
func opened(path string, o Options) (choice, error) {
	d, err := openDB(fsys.OS{}, path, o)
	if err != nil {
		return choice{}, err
	}
	defer d.l.Close()
	if err := d.l.Lock(); err != nil {
		return choice{}, err
	}
	defer d.l.Unlock()
	return chosen(d.l), nil
}

// TestATargetThatDoesntCount: a Target that isn't a Counter counts nothing
// dead, so commits that write over each other make a compaction due only at
// the limit on commits.
func TestATargetThatDoesntCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	l, _ := openLog(t, path, Options{MaxCommits: 3, MinGrowth: 1})
	for i := uint64(1); i <= 3; i++ {
		if err := l.Lock(); err != nil {
			t.Fatal(err)
		}
		if err := l.Append(randomPut(i)); err != nil {
			t.Fatal(err)
		}
		c := chosen(l)
		l.Unlock()
		if c.dead != 0 || c.due != (i == 3) {
			t.Fatalf("commit %d through a recorder: %v, where only the third is due", i, c)
		}
	}
}

// randomPut is a batch that writes over the record a:1, or makes its table
// first, for a Log whose Target keeps nothing of its own.
func randomPut(i uint64) []format.Change {
	put := format.Change{Op: format.Put, Key: "a:1", Fields: []format.Field{{Name: "s", Value: value.Text(strings.Repeat("y", 500))}}}
	if i == 1 {
		return []format.Change{{Op: format.CreateTable, Table: "a"}, put}
	}
	return []format.Change{put}
}

// TestOpeningTheLimitOnCommits is what settles the limit on commits: a log
// of one commit short of 100,000, each a put of two short fields, opens with
// the store as its Target, as the public package opens one, in about 0.3
// seconds on a two-core cloud machine, which -v shows. It counts 99,999
// commits, makes nothing dead, and isn't due, and the next commit makes a
// compaction due. With -short the log holds 999 commits, at a limit of
// 1,000.
func TestOpeningTheLimitOnCommits(t *testing.T) {
	commits, o := uint64(maxCommits-1), Options{}
	if testing.Short() {
		commits, o = 999, Options{MaxCommits: 1000}
	}
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit([]format.Change{{Op: format.CreateTable, Table: "docs"}})
	for i := uint64(2); i <= commits; i++ {
		w.commit([]format.Change{{Op: format.Put, Key: "docs:" + strconv.FormatUint(i, 10), Fields: []format.Field{
			{Name: "n", Value: value.Int(int64(i))}, {Name: "title", Value: value.Text("a title")}}}})
	}
	w.write(path)
	began := time.Now()
	d, err := openDB(fsys.OS{}, path, o)
	took := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, d.l)
	info, _ := os.Stat(path)
	t.Logf("opening %d commits, %d bytes, took %v", commits, info.Size(), took)
	c := lockedChoice(t, d)
	if c.commits != commits || c.dead != 0 || c.due {
		t.Errorf("a log of %d commits that only grows gives %v", commits, c)
	}
	if c := putOver(t, d, "docs:0", 1); c.commits != commits+1 || !c.due {
		t.Errorf("the commit that reaches the limit gives %v", c)
	}
}

// The closing test.

// closingFloor and closingLimit are the closing test's rule: due once 4,000
// bytes are dead and half the file, or once 40 commits follow the compacted
// part.
const (
	closingFloor = 4000
	closingLimit = 40
)

// closingOptions are the options of every Log in the closing test, in this
// process and in the helper.
var closingOptions = Options{MinGrowth: closingFloor, MaxCommits: closingLimit}

// TestEveryProcessTakesTheSameChoice is C1's closing test, with the public
// package's TestADatabaseThatOnlyGrows and TestADatabaseThatWritesOver.
//
// One Log, the writer, makes random commits through the store: puts that
// make records and write over them, vectors written over and taken away,
// deletes of records with links, links, removed links and drops of a table
// with links inside it and out of it, statements taken back to their marks
// and transactions rolled back. Phases of 60 commits take turns, writing
// over the data and only adding to it, so compactions come both ways. After
// each commit, holding the lock as Update does, the writer takes its choice,
// which has to be what C1's rule says for what it weighs. Then each other
// Log takes its own, holding the lock in turn: one in this process that
// follows the file, reloading it after a compaction; one opened afresh; and
// in a helper process, one that follows the file and, every fifth commit,
// one opened afresh there. Every one of them has to give the writer's
// choice. When a compaction is due, the writer, the follower here or the
// helper's follower makes it, by turns, so the writer moves to a compacted
// file it didn't write too.
//
// After each commit, the writer's count has to be the file's changes less
// the live state, by the count's measure (canonical), worked out from the
// file and the copy apart from the store's count: so it's a function of the
// file, and exact by that measure.
func TestEveryProcessTakesTheSameChoice(t *testing.T) {
	commits := 480
	if testing.Short() {
		commits = 240
	}
	path := filepath.Join(t.TempDir(), "db")
	w := openDBT(t, fsys.OS{}, path, closingOptions)
	b := openDBT(t, fsys.OS{}, path, closingOptions)
	h := start(t, "count", path)
	if s := h.line(t); s != "open" {
		t.Fatalf("the helper said %q", s)
	}
	r := rand.New(rand.NewPCG(1, 0xc1))
	kinds := map[string]int{}
	for i := 1; i <= commits; i++ {
		c, wrote := closingCommit(t, r, w, i)
		if !wrote {
			kinds["nothing written"]++
		}
		if c.due != rule(c, closingFloor, closingLimit) {
			t.Fatalf("commit %d: the writer gives %v, where the rule says %v", i, c, !c.due)
		}
		if live := liveBytes(t, w); c.dead != fileBytesDead(t, path, live) {
			t.Fatalf("commit %d: the writer counts %v, where the file's changes less its live data, %d bytes, make %d", i, c, live, fileBytesDead(t, path, live))
		}
		if got := followedChoice(t, b); got != c {
			t.Fatalf("commit %d: the follower gives %v, where the writer gives %v", i, got, c)
		}
		if got := openedChoice(t, path, closingOptions); got != c {
			t.Fatalf("commit %d: a Log opened afresh gives %v, where the writer gives %v", i, got, c)
		}
		if got := h.ask(t, "follow"); got != c.String() {
			t.Fatalf("commit %d: the helper's follower gives %s, where the writer gives %v", i, got, c)
		}
		if i%5 == 0 {
			if got := h.ask(t, "open"); got != c.String() {
				t.Fatalf("commit %d: a Log the helper opens afresh gives %s, where the writer gives %v", i, got, c)
			}
		}
		if !c.due {
			continue
		}
		switch {
		case c.commits >= closingLimit:
			kinds["by the limit on commits"]++
		default:
			kinds["by dead bytes"]++
		}
		switch i % 3 {
		case 0:
			if err := w.compact(); err != nil {
				t.Fatal(err)
			}
			kinds["by the writer"]++
		case 1:
			if err := b.compact(); err != nil {
				t.Fatal(err)
			}
			kinds["by the follower"]++
		default:
			if got := h.ask(t, "compact"); got != "compacted" {
				t.Fatalf("the helper's compaction: %s", got)
			}
			kinds["by the helper"]++
		}
	}
	t.Logf("compactions and commits: %v", kinds)
	for _, k := range []string{"by the limit on commits", "by dead bytes", "by the writer", "by the follower", "by the helper", "nothing written"} {
		if kinds[k] == 0 {
			t.Errorf("no compaction %s came up: %v", k, kinds)
		}
	}
	h.end(t)
}

// closingCommit makes commit i of the closing test on w, holding the write
// lock, with closingOps, and returns w's choice straight after it, under the
// same lock, as Update takes it, and whether the commit wrote a batch. Now
// and then a statement is taken back to its mark, or the whole transaction
// rolled back, and then its choice is the file's as it was.
func closingCommit(t *testing.T, r *rand.Rand, w *db, i int) (choice, bool) {
	t.Helper()
	if err := w.l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer w.l.Unlock()
	tx, err := w.c.s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := closingOps(r, tx, i); err != nil {
		t.Fatalf("commit %d: %v", i, err)
	}
	if r.IntN(6) == 0 {
		m := tx.Mark()
		if err := closingOps(r, tx, i); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		tx.RollbackTo(m)
	}
	wrote := false
	if r.IntN(15) == 0 {
		tx.Rollback()
	} else if err := tx.Commit(func(changes []format.Change) error {
		wrote = true
		return w.l.Append(changes)
	}); err != nil {
		t.Fatal(err)
	}
	return chosen(w.l), wrote
}

// closingOps is the work of commit i in the closing test, through a
// transaction of the copy, chosen by r. Phases of 60 commits take turns:
// the first writes over the data, and the second only adds to it.
//
//   - Writing over: puts of fifteen records of table a, of one to three of
//     the fields n, s, a text of up to 600 letters, and x, now and then a
//     null; vectors of eight values in table v, written over and taken away;
//     deletes of a and v records, which take their links with them; links
//     among them of three types; removed links, one of a type or all of
//     them between two records; and records of a table tmp, linked among
//     themselves, to themselves and out, which is dropped now and then. The
//     phase's first commit drops table g, which the phase before it added.
//   - Adding: new records of table g, each with a short text and a link to
//     a:0.
//
// The record a:0 is never deleted, so a link always has somewhere to go.
func closingOps(r *rand.Rand, tx *store.Tx, i int) error {
	exists := func(key string) bool { _, err := tx.Get(key); return err == nil }
	text := func(n int) value.Value {
		b := make([]byte, n)
		for j := range b {
			b[j] = 'a' + byte(r.IntN(26))
		}
		return value.Text(string(b))
	}
	aKey := func() string { return "a:" + strconv.Itoa(r.IntN(15)) }
	vKey := func() string { return "v:" + strconv.Itoa(r.IntN(6)) }
	tmpKey := func() string { return "tmp:" + strconv.Itoa(r.IntN(5)) }
	anyKey := func() string {
		switch r.IntN(3) {
		case 0:
			return vKey()
		case 1:
			return tmpKey()
		}
		return aKey()
	}
	if !exists("a:0") {
		if err := tx.Put("a:0", []format.Field{{Name: "n", Value: value.Int(0)}}); err != nil {
			return err
		}
	}
	types := []string{"near", "far", "same"}
	adding := (i/60)%2 == 1
	if _, there := tx.Table("g"); !adding && i%60 == 0 && there {
		if err := tx.Drop("g"); err != nil {
			return err
		}
	}
	for n := 1 + r.IntN(4); n > 0; n-- {
		if adding {
			key := fmt.Sprintf("g:%d-%d", i, n)
			if err := tx.Put(key, []format.Field{{Name: "s", Value: text(10 + r.IntN(30))}}); err != nil {
				return err
			}
			if err := tx.Link(key, pick(r, types), "a:0"); err != nil {
				return err
			}
			continue
		}
		var err error
		switch k := r.IntN(20); {
		case k < 10:
			var fields []format.Field
			if r.IntN(2) == 0 {
				fields = append(fields, format.Field{Name: "n", Value: value.Int(r.Int64())})
			}
			fields = append(fields, format.Field{Name: "s", Value: text(r.IntN(600))})
			if r.IntN(3) == 0 {
				fields = append(fields, format.Field{Name: "x", Value: value.Real(r.Float64())})
			}
			if r.IntN(8) == 0 {
				fields[0].Value = value.Null()
			}
			err = tx.Put(aKey(), fields)
		case k < 12:
			vec := value.Null()
			if r.IntN(5) != 0 {
				v := make([]float32, 8)
				for j := range v {
					v[j] = float32(r.NormFloat64())
				}
				v[0] += 3 // never all zero
				vec = value.Vector(v)
			}
			err = tx.Put(vKey(), []format.Field{{Name: "t", Value: text(r.IntN(30))}, {Name: "vec", Value: vec}})
		case k < 13:
			if key := anyKey(); key != "a:0" && exists(key) {
				err = tx.Delete(key)
			}
		case k < 16:
			if from, to := anyKey(), anyKey(); exists(from) && exists(to) {
				err = tx.Link(from, pick(r, types), to)
			}
		case k < 18:
			from := anyKey()
			if !exists(from) {
				break
			}
			links, e := tx.Neighbours(from, store.Out, "")
			if e != nil || len(links) == 0 {
				err = e
				break
			}
			l := pick(r, links)
			if r.IntN(2) == 0 {
				l.Type = "" // every link from one to the other
			}
			err = tx.Unlink(l.From, l.Type, l.To)
		case k < 19:
			key := tmpKey()
			if err = tx.Put(key, []format.Field{{Name: "n", Value: value.Int(int64(i))}}); err != nil {
				break
			}
			if to := pick(r, []string{tmpKey(), key, "a:0"}); exists(to) {
				err = tx.Link(key, pick(r, types), to)
			}
		default:
			if _, there := tx.Table("tmp"); there && r.IntN(2) == 0 {
				err = tx.Drop("tmp")
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func pick[T any](r *rand.Rand, list []T) T { return list[r.IntN(len(list))] }

// ask sends the helper a line and returns the line it prints back.
func (p *proc) ask(t *testing.T, line string) string {
	t.Helper()
	if _, err := fmt.Fprintln(p.in, line); err != nil {
		t.Fatal(err)
	}
	return p.line(t)
}

// countRole is the helper role count, for the closing test: it opens the
// database at path with closingOptions, prints "open", and then answers
// each line on its standard input. To "follow" it follows the file,
// reloading it when another has taken the path, and prints its choice,
// holding the lock; to "open" it prints the choice of a Log it opens
// afresh; and to "compact" it compacts the database, holding the lock, and
// prints "compacted".
func countRole(path string) error {
	d, err := openDB(fsys.OS{}, path, closingOptions)
	if err != nil {
		return err
	}
	defer d.l.Close()
	fmt.Println("open")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var c choice
		var err error
		switch in.Text() {
		case "follow":
			c, err = followed(d)
		case "open":
			c, err = opened(path, closingOptions)
		case "compact":
			if _, err = followed(d); err == nil {
				err = d.compact()
			}
			if err == nil {
				fmt.Println("compacted")
				continue
			}
		default:
			err = fmt.Errorf("no command %q", in.Text())
		}
		if err != nil {
			return err
		}
		fmt.Println(c.String())
	}
	return in.Err()
}

// liveBytes is what w's copy holds by the count's measure: its snapshot, as
// canonical measures it.
func liveBytes(t *testing.T, w *db) int64 {
	t.Helper()
	var n int64
	w.c.s.Read(func(r store.Reader) error {
		vecs := map[string]string{}
		for c := range r.Snapshot() {
			n += canonical(t, c, vecs)
		}
		return nil
	})
	return n
}

// fileBytesDead is how many bytes of changes in the database file at path
// are dead, by the count's measure: what its changes take, from its first
// batch to the end of its log, less live, what its live data takes.
func fileBytesDead(t *testing.T, path string, live int64) int64 {
	t.Helper()
	b := fileBytes(t, path)
	h, err := format.DecodeHeader(b, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	vecs := map[string]string{}
	off := int64(format.HeaderSize)
	for seq := uint64(1); off < int64(len(b)); seq++ {
		bt, err := format.DecodeBatch(b[off:], h.Gen, seq)
		if err != nil {
			t.Fatalf("batch %d at offset %d: %v", seq, off, err)
		}
		for _, c := range bt.Changes {
			n += canonical(t, c, vecs)
		}
		off += int64(bt.Length) + format.MarkerSize
	}
	return n - live
}

// canonical is what c takes by the count's measure, worked out from the
// codec's own bytes: as format.AppendBatch writes it, the batch's length
// less its head and its checksum, apart from a create, which counts as it
// does with no fields, and a vector block, each of whose vectors counts as
// a put of the vector field writes it. vecs holds each table's vector field
// as its create names it, for the blocks after it.
func canonical(t *testing.T, c format.Change, vecs map[string]string) int64 {
	t.Helper()
	switch c.Op {
	case format.CreateTable:
		for _, name := range c.Names {
			if rules.IsVec(name) {
				vecs[c.Table] = name
			}
		}
		c = format.Change{Op: format.CreateTable, Table: c.Table}
	case format.Vectors:
		var n int64
		for _, v := range c.Vecs {
			n += 2 + int64(len(vecs[c.Table])) + 1 + 4 + int64(len(v.Bits))
		}
		return n
	}
	b, _, err := format.AppendBatch(nil, 1, 1, []format.Change{c})
	if err != nil {
		t.Fatalf("AppendBatch refused %v: %v", c, err)
	}
	return int64(len(b) - format.BatchHeadSize - 4)
}
