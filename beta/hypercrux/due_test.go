// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// When the compaction after a commit comes (C1), through the public
// package: a database that only grows, one whose commits write over its data
// and delete it, and every handle on a file counting what its writer counts,
// in this process and in another. The log's TestEveryProcessTakesTheSameChoice
// holds each choice to the rule commit by commit; these hold Update to it.

// pending is what Due weighs for db, as db last read the file, with the
// file's size, which is where the log ends when nobody is writing.
type pending struct {
	dead    int64
	commits uint64
}

func (p pending) String() string {
	return fmt.Sprintf("%d bytes dead, commits past the compacted part %d", p.dead, p.commits)
}

// weighs is db's pending, once a read through it has followed the file:
// a Get, which follows before it looks for its key.
func weighs(t *testing.T, db *hc.DB) pending {
	t.Helper()
	if _, err := db.Get("nosuch:1"); !errors.Is(err, hc.ErrNotFound) {
		t.Fatalf("a read to follow the file gave %v", err)
	}
	dead, commits := hc.Pending(db)
	return pending{dead, commits}
}

// dueBy is C1's rule, worked out by the test, for a file of size bytes, with
// floor and limit in place of a megabyte and 100,000.
func dueBy(p pending, size, floor int64, limit uint64) bool {
	return p.commits >= limit || p.dead >= floor && 2*p.dead >= size
}

// growing is commit i's work in TestADatabaseThatOnlyGrows: three new
// records with a text and a vector, each linked to the record of the commit
// before it, and a new record in another table linked to them.
func growing(tx *hc.Tx, i int) error {
	who := "people:" + strconv.Itoa(i)
	if err := tx.Put(who, hc.Fields{"name": "writer " + strconv.Itoa(i)}); err != nil {
		return err
	}
	for j := range 3 {
		key := fmt.Sprintf("docs:%d-%d", i, j)
		v := make(hc.Vector, 16)
		for k := range v {
			v[k] = float32(i*16 + j + k + 1)
		}
		if err := tx.Put(key, hc.Fields{"text": strings.Repeat(string(rune('a'+j)), 200+i), "n": i, "vec": v}); err != nil {
			return err
		}
		if err := tx.Link(who, "wrote", key); err != nil {
			return err
		}
		if i > 1 {
			if err := tx.Link(key, "after", fmt.Sprintf("docs:%d-%d", i-1, j)); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestADatabaseThatOnlyGrows is the second part of C1's closing test: a
// database whose commits only add records and links counts nothing dead, so
// the compaction after a commit comes only at the limit on commits.
//
// With a limit of 25 commits, and the floor for dead bytes at a single byte,
// so that one dead byte with half the file would be enough, the DB compacts
// after commits 25, 50 and 75 and after no other, and it and a DB that
// follows the file through its reads count nothing dead after every commit.
// With the defaults, commits of 1,000 records with 384-value vectors, as
// beta/bench's batched put makes them, grow the file to 32 MB, or 8 MB with
// -short, without a compaction, where the rule before C1 compacted at each
// doubling past a megabyte.
func TestADatabaseThatOnlyGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grows.hcx")
	db, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{MaxCommits: 25, MinGrowth: 1})
	ok(t, err)
	defer db.Close()
	follower := open(t, path)
	for i := 1; i <= 80; i++ {
		ok(t, db.Update(func(tx *hc.Tx) error { return growing(tx, i) }))
		want := pending{0, uint64(i % 25)}
		if got := weighs(t, db); got != want {
			t.Fatalf("after commit %d the DB weighs %v, where %v is wanted", i, got, want)
		}
		if got := weighs(t, follower); got != want {
			t.Fatalf("after commit %d the follower weighs %v, where %v is wanted", i, got, want)
		}
		if gen := header(t, fsys.OS{}, path).Gen; gen != uint64(1+i/25) {
			t.Fatalf("after commit %d the file has generation %d, where compactions come after commits 25, 50 and 75 alone", i, gen)
		}
	}

	batches := 20
	if testing.Short() {
		batches = 5
	}
	big := filepath.Join(t.TempDir(), "batches.hcx")
	db2 := open(t, big)
	r := rand.New(rand.NewPCG(1, 384))
	for i := range batches {
		ok(t, db2.Update(func(tx *hc.Tx) error {
			for j := range 1000 {
				v := make(hc.Vector, 384)
				for k := range v {
					v[k] = float32(r.NormFloat64())
				}
				if err := tx.Put("docs:"+strconv.Itoa(i*1000+j), hc.Fields{"title": "a title", "vec": v}); err != nil {
					return err
				}
			}
			return nil
		}))
		if got := weighs(t, db2); got != (pending{0, uint64(i + 1)}) {
			t.Fatalf("after batch %d the DB weighs %v", i+1, got)
		}
	}
	if h := header(t, fsys.OS{}, big); h.Gen != 1 || fileSize(t, big) < int64(batches)*1500000 {
		t.Errorf("after %d batches of 1,000 records the file has generation %d and %d bytes", batches, h.Gen, fileSize(t, big))
	}
}

// writingOver is commit i's work in TestADatabaseThatWritesOver: two of 20
// records written over with 50 KB of text, a third deleted with its links
// and put back, and links among them.
func writingOver(tx *hc.Tx, i int) error {
	text := func(k int) string { return strings.Repeat(string(rune('a'+(i+k)%26)), 50<<10) }
	for k, n := range []int{i % 20, (i + 7) % 20} {
		if err := tx.Put("rec:"+strconv.Itoa(n), hc.Fields{"text": text(k), "n": i}); err != nil {
			return err
		}
	}
	gone := "rec:" + strconv.Itoa((i+13)%20)
	if _, err := tx.Get(gone); err == nil {
		if err := tx.Delete(gone); err != nil {
			return err
		}
	}
	if err := tx.Put(gone, hc.Fields{"text": text(2)}); err != nil {
		return err
	}
	return tx.Link(gone, "next", "rec:"+strconv.Itoa(i%20))
}

// TestADatabaseThatWritesOver is the third part of C1's closing test: a
// database whose commits write over its records and delete them, opened with
// the defaults, is compacted after a commit once its commits since the last
// compaction have made a megabyte dead and half the file, and only then.
// Each commit writes over two of 20 records of 50 KB, deletes a third with
// its links and puts it back. Inside each compaction, through the snapshot
// it writes, what the DB weighs holds to the rule, the commit that set it off
// among it; after each commit that no compaction follows, it doesn't. The
// file is never more than twice its live data and a megabyte and a commit,
// and it's compacted at least four times, or twice with -short.
func TestADatabaseThatWritesOver(t *testing.T) {
	commits, least := 60, 4
	if testing.Short() {
		commits, least = 30, 2
	}
	path := filepath.Join(t.TempDir(), "over.hcx")
	db := open(t, path)
	compactions := 0
	defer hc.UseSnapshot(func(r store.Reader) iter.Seq[format.Change] {
		dead, n := hc.Pending(db)
		if p, size := (pending{dead, n}), fileSize(t, path); !dueBy(p, size, 1<<20, 100000) || p.dead < 1<<20 {
			t.Errorf("a compaction came after a commit with %v in a file of %d bytes", p, size)
		}
		compactions++
		return r.(*store.Store).Compacted()
	})()
	for i := 1; i <= commits; i++ {
		before := compactions
		ok(t, db.Update(func(tx *hc.Tx) error { return writingOver(tx, i) }))
		p, size := weighs(t, db), fileSize(t, path)
		switch {
		case compactions > before && p != (pending{}):
			t.Fatalf("after commit %d and its compaction, the DB weighs %v", i, p)
		case compactions == before && dueBy(p, size, 1<<20, 100000):
			t.Fatalf("no compaction came after commit %d, with %v in a file of %d bytes", i, p, size)
		case size > 2*(20*(50<<10)+(64<<10))+1<<20+3*(51<<10):
			t.Fatalf("after commit %d the file is %d bytes, more than twice its live data and a megabyte and a commit", i, size)
		}
	}
	t.Logf("%d commits made %d compactions", commits, compactions)
	if compactions < least {
		t.Errorf("%d commits made %d compactions, where %d are wanted", commits, compactions, least)
	}
}

// The handles' agreement through the package.

// agreeFloor and agreeLimit are the rule of the writer in
// TestEveryHandleCountsAlike: due once 4,000 bytes are dead and half the
// file, or 40 commits follow the compacted part.
const (
	agreeFloor = 4000
	agreeLimit = 40
)

// TestEveryHandleCountsAlike: one DB makes random Updates, of the kinds the
// log's closing test makes, through the package, and compacts by itself once
// C1's rule says so, with a floor of 4,000 bytes and a limit of 40 commits.
// Some of its Updates fail after their changes, and some hold an SQL UPDATE
// that writes over a record and then fails, which the transaction takes back
// to the statement's mark. After every Update, every handle weighs what the
// writer weighs: a DB that follows the file through its reads, reloading it
// after a compaction, one opened afresh, and in a helper process, one that
// follows and, every fifth commit, one opened afresh there. Inside each
// compaction, through the snapshot it writes, what the writer weighs holds to
// the rule, and the follower, a DB opened then and the helper's follower
// weigh the same; after each Update that no compaction follows, the rule
// doesn't hold. So a follower and a process that reloads after another's
// compaction count what the writer counts.
func TestEveryHandleCountsAlike(t *testing.T) {
	commits := 300
	if testing.Short() {
		commits = 150
	}
	path := filepath.Join(t.TempDir(), "agree.hcx")
	db, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{MinGrowth: agreeFloor, MaxCommits: agreeLimit})
	ok(t, err)
	defer db.Close()
	follower := open(t, path)
	h := startCounter(t, path)
	agree := func(when string, want pending) {
		t.Helper()
		if got := weighs(t, follower); got != want {
			t.Fatalf("%s, the follower weighs %v, where the writer weighs %v", when, got, want)
		}
		fresh, err := hc.Open(path)
		ok(t, err)
		got := pending{}
		got.dead, got.commits = hc.Pending(fresh)
		ok(t, fresh.Close())
		if got != want {
			t.Fatalf("%s, a DB opened afresh weighs %v, where the writer weighs %v", when, got, want)
		}
		if got := h.ask(t, "follow"); got != want.String() {
			t.Fatalf("%s, the helper's follower weighs %s, where the writer weighs %v", when, got, want)
		}
	}
	kinds := map[string]int{}
	i := 0
	defer hc.UseSnapshot(func(r store.Reader) iter.Seq[format.Change] {
		dead, n := hc.Pending(db)
		p, size := pending{dead, n}, fileSize(t, path)
		if !dueBy(p, size, agreeFloor, agreeLimit) {
			t.Errorf("commit %d: a compaction came with %v in a file of %d bytes", i, p, size)
		}
		if p.commits >= agreeLimit {
			kinds["compacted at the limit on commits"]++
		} else {
			kinds["compacted by dead bytes"]++
		}
		agree(fmt.Sprintf("in the compaction after commit %d", i), p)
		return r.(*store.Store).Compacted()
	})()
	rng := rand.New(rand.NewPCG(2, 0xc1))
	for i = 1; i <= commits; i++ {
		gen := header(t, fsys.OS{}, path).Gen
		err := db.Update(func(tx *hc.Tx) error {
			if err := agreeOps(rng, tx, i, kinds); err != nil {
				return err
			}
			if rng.IntN(15) == 0 {
				kinds["Update failed"]++
				return errFailed
			}
			return nil
		})
		if err != nil && !errors.Is(err, errFailed) {
			t.Fatalf("commit %d: %v", i, err)
		}
		p, size := weighs(t, db), fileSize(t, path)
		if header(t, fsys.OS{}, path).Gen == gen && dueBy(p, size, agreeFloor, agreeLimit) {
			t.Fatalf("commit %d: no compaction came, with %v in a file of %d bytes", i, p, size)
		}
		agree(fmt.Sprintf("after commit %d", i), p)
		if i%5 == 0 {
			if got := h.ask(t, "open"); got != p.String() {
				t.Fatalf("after commit %d, a DB the helper opens afresh weighs %s, where the writer weighs %v", i, got, p)
			}
		}
	}
	t.Logf("kinds: %v", kinds)
	for _, k := range []string{"compacted at the limit on commits", "compacted by dead bytes", "Update failed", "statement taken back"} {
		if kinds[k] == 0 {
			t.Errorf("%q never came up: %v", k, kinds)
		}
	}
	h.end(t)
}

// errFailed is the error an Update of TestEveryHandleCountsAlike fails with,
// once it has made its changes.
var errFailed = fmt.Errorf("the Update's own error")

// agreeOps is commit i's work in TestEveryHandleCountsAlike, through the
// transaction, chosen by r, as the log's closingOps chooses: phases of 50
// commits take turns, writing over the data, with deletes, links, removed
// links and a dropped table, and then only adding to it. Now and then an SQL
// UPDATE writes over a:0 and then fails at the next record, whose key it
// would change, and is taken back.
func agreeOps(r *rand.Rand, tx *hc.Tx, i int, kinds map[string]int) error {
	exists := func(key string) bool { _, err := tx.Get(key); return err == nil }
	text := func(n int) string {
		b := make([]byte, n)
		for j := range b {
			b[j] = 'a' + byte(r.IntN(26))
		}
		return string(b)
	}
	aKey := func() string { return "a:" + strconv.Itoa(r.IntN(12)) }
	anyKey := func() string {
		switch r.IntN(3) {
		case 0:
			return "v:" + strconv.Itoa(r.IntN(6))
		case 1:
			return "tmp:" + strconv.Itoa(r.IntN(4))
		}
		return aKey()
	}
	types := []string{"near", "far"}
	if !exists("a:0") {
		if err := tx.Put("a:0", hc.Fields{"n": 0}); err != nil {
			return err
		}
	}
	adding := (i/50)%2 == 1
	if !adding && i%50 == 0 {
		if err := tx.Drop("g"); err != nil && !errors.Is(err, hc.ErrNotFound) {
			return err
		}
	}
	for n := 1 + r.IntN(4); n > 0; n-- {
		if adding {
			key := fmt.Sprintf("g:%d-%d", i, n)
			if err := tx.Put(key, hc.Fields{"s": text(10 + r.IntN(30))}); err != nil {
				return err
			}
			if err := tx.Link(key, "to", "a:0"); err != nil {
				return err
			}
			continue
		}
		var err error
		switch k := r.IntN(20); {
		case k < 10:
			f := hc.Fields{"s": text(r.IntN(600))}
			if r.IntN(2) == 0 {
				f["n"] = r.Int64()
			}
			if r.IntN(8) == 0 {
				f["x"] = nil
			}
			err = tx.Put(aKey(), f)
		case k < 12:
			var vec any
			if r.IntN(5) != 0 {
				v := make(hc.Vector, 8)
				for j := range v {
					v[j] = float32(r.NormFloat64())
				}
				v[0] += 3
				vec = v
			}
			err = tx.Put("v:"+strconv.Itoa(r.IntN(6)), hc.Fields{"t": text(r.IntN(30)), "vec": vec})
		case k < 13:
			if key := anyKey(); key != "a:0" && exists(key) {
				err = tx.Delete(key)
			}
		case k < 15:
			if from, to := anyKey(), anyKey(); exists(from) && exists(to) {
				err = tx.Link(from, types[r.IntN(2)], to)
			}
		case k < 17:
			from := anyKey()
			if !exists(from) {
				break
			}
			links, e := tx.Neighbours(from, hc.Out, "")
			if e != nil || len(links) == 0 {
				err = e
				break
			}
			l := links[r.IntN(len(links))]
			if r.IntN(2) == 0 {
				l.Type = ""
			}
			err = tx.Unlink(l.From, l.Type, l.To)
		case k < 18:
			key := "tmp:" + strconv.Itoa(r.IntN(4))
			if err = tx.Put(key, hc.Fields{"n": i}); err == nil {
				err = tx.Link(key, "near", key)
			}
		case k < 19:
			if _, e := tx.Exec("UPDATE a SET key = 'a:0', s = ?", text(300)); e == nil {
				break // a:0 is the table's only record
			}
			kinds["statement taken back"]++
		default:
			if e := tx.Drop("tmp"); e != nil && !errors.Is(e, hc.ErrNotFound) {
				err = e
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// counter is the helper process of TestEveryHandleCountsAlike, with pipes
// to its standard input and output.
type counter struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// startCounter starts the helper role count on the database at path, and
// waits for it to say it's open. It's killed when the test ends, if it
// hasn't ended by then.
func startCounter(t *testing.T, path string) *counter {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HYPERCRUX_BETA_ROLE=count", "HYPERCRUX_BETA_PATH="+path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	ok(t, err)
	out, err := cmd.StdoutPipe()
	ok(t, err)
	ok(t, cmd.Start())
	c := &counter{cmd: cmd, in: in, out: bufio.NewReader(out)}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	if s := c.line(t); s != "open" {
		t.Fatalf("the helper said %q", s)
	}
	return c
}

// line reads a line the helper prints.
func (c *counter) line(t *testing.T) string {
	t.Helper()
	s, err := c.out.ReadString('\n')
	if err != nil {
		t.Fatalf("reading from the helper: %v", err)
	}
	return strings.TrimSuffix(s, "\n")
}

// ask sends the helper a line and returns the line it prints back.
func (c *counter) ask(t *testing.T, line string) string {
	t.Helper()
	if _, err := fmt.Fprintln(c.in, line); err != nil {
		t.Fatal(err)
	}
	return c.line(t)
}

// end closes the helper's standard input, which ends it, and waits for it to
// exit.
func (c *counter) end(t *testing.T) {
	t.Helper()
	c.in.Close()
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("the helper failed: %v", err)
	}
}

// countHelper is the helper role count: it opens the database at path,
// prints "open", and for each line on its standard input prints what Due
// weighs: for "follow", once a read through its DB has followed the file,
// and for "open", of a DB it opens afresh.
func countHelper(path string) error {
	db, err := hc.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Println("open")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var p pending
		switch in.Text() {
		case "follow":
			if _, err := db.Get("nosuch:1"); !errors.Is(err, hc.ErrNotFound) {
				return fmt.Errorf("a read to follow the file gave %v", err)
			}
			p.dead, p.commits = hc.Pending(db)
		case "open":
			fresh, err := hc.Open(path)
			if err != nil {
				return err
			}
			p.dead, p.commits = hc.Pending(fresh)
			if err := fresh.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("no command %q", in.Text())
		}
		fmt.Println(p.String())
	}
	return in.Err()
}
