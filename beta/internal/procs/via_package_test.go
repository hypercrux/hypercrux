// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The public package under the harness (I1). Writers and readers open the
// database with hypercrux.Open and go through nothing else: a writer commits
// with Update and compacts with Compact, a reader reads with Scan, Get,
// Neighbours and Check, and the run's Final, Backup and Restore hold the
// write lock through an Update of their own. Update compacts by itself too,
// once a commit leaves the file twice the size its last compaction left it
// at and a megabyte past that (G5), which the long kill test's file reaches.
//
// Each commit touches every handle at once, as 0.x's KilledWritersNeverLeaveAMess
// has its writer do. Commit n, its place in the log, which the writer works
// out inside its Update from the counter, puts the record c:n with the
// commit's text in the field t and a vector made from the text in vec, links
// it to the commit before it with next and to one of ten anchors with group,
// puts a scratch record s:n, deletes s:n-20 when n is a multiple of 7 past
// 20, and moves the counter, meta:n. The first commit puts the anchors. So a
// commit that reached the copy in part would show as a record without its
// vector or its links, a scratch record out of step with the counter, or
// counts that don't add up, which the readers look for at every read and
// Final looks for in every commit. The harness compares commits by their
// text, and a backup's era is the record c:000000000, as in realReloaded
// (reload_test.go).
var (
	packaged           = Workload{Name: "public package", Write: func(w *Writer) error { return packageWrite(w, false) }, Read: packageRead, Final: packageFinal, Backup: packageBackup, Restore: packageRestore}
	packagedCompacting = Workload{Name: "public package, compacting", Write: func(w *Writer) error { return packageWrite(w, true) }, Read: packageRead, Final: packageFinal, Backup: packageBackup, Restore: packageRestore}
)

// The keys a commit puts, besides place(n), the commit's record.
func scratchKey(n int) string { return fmt.Sprintf("s:%09d", n) }
func anchorKey(n int) string  { return "anchor:" + strconv.Itoa(n%10) }

const counterKey = "meta:n"

// scratchGone reports whether commit n's scratch record is gone once commit
// count has been made: commit j deletes s:j-20 when j is a multiple of 7 past
// 20.
func scratchGone(n, count int) bool {
	j := n + 20
	return j <= count && j%7 == 0
}

// scratchLeft is how many scratch records there are once commit count has
// been made.
func scratchLeft(count int) int {
	left := 0
	for n := 1; n <= count; n++ {
		if !scratchGone(n, count) {
			left++
		}
	}
	return left
}

// vecFor is the vector a commit holding text puts in its record: 16 values
// drawn from a seed that the text gives, so a reader can tell the right one
// from a stale or torn one.
func vecFor(text string) hc.Vector {
	h := fnv.New64a()
	h.Write([]byte(text))
	r := rand.New(rand.NewPCG(h.Sum64(), 7))
	v := make(hc.Vector, 16)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	v[0] += 4 // never all zero
	return v
}

// packageCommit is commit n's changes, holding text, inside an Update.
func packageCommit(tx *hc.Tx, n int, text string) error {
	if n == 1 {
		for k := range 10 {
			if err := tx.Put(anchorKey(k), hc.Fields{"name": "anchor " + strconv.Itoa(k)}); err != nil {
				return err
			}
		}
	}
	key := place(int64(n))
	if err := tx.Put(key, hc.Fields{"t": text, "vec": vecFor(text)}); err != nil {
		return err
	}
	if n > 1 {
		if err := tx.Link(key, "next", place(int64(n-1))); err != nil {
			return err
		}
	}
	if err := tx.Link(key, "group", anchorKey(n)); err != nil {
		return err
	}
	if err := tx.Put(scratchKey(n), hc.Fields{"n": n}); err != nil {
		return err
	}
	if n%7 == 0 && n > 20 {
		if err := tx.Delete(scratchKey(n - 20)); err != nil {
			return err
		}
	}
	return tx.Put(counterKey, hc.Fields{"n": n})
}

// handle is what *hc.DB and *hc.Tx have in common that the checks read
// through.
type handle interface {
	Get(key string) (hc.Fields, error)
	Scan(prefix, after string, limit int) ([]hc.Record, error)
	Neighbours(key string, dir hc.Direction, typ string) ([]hc.Link, error)
}

var (
	_ handle = (*hc.DB)(nil)
	_ handle = (*hc.Tx)(nil)
)

// counter reads the counter: the commits made so far, or 0 before the first.
func counter(h handle) (int, error) {
	f, err := h.Get(counterKey)
	if errors.Is(err, hc.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, ok := f["n"].(int64)
	if !ok {
		return 0, fmt.Errorf("the counter holds %v", f)
	}
	return int(n), nil
}

// commitsOf reads the commits' records with one Scan, which reads one point
// in the log: the era, and each commit's text, in order.
func commitsOf(h handle) (era int, commits []string, err error) {
	recs, err := h.Scan("c:", "", 0)
	if err != nil {
		return 0, nil, err
	}
	for _, rec := range recs {
		n, err := strconv.ParseInt(strings.TrimPrefix(rec.Key, "c:"), 10, 64)
		switch {
		case err != nil:
			return 0, nil, fmt.Errorf("a commit's record has the key %s", rec.Key)
		case n == 0:
			e, ok := rec.Fields["era"].(int64)
			if !ok {
				return 0, nil, fmt.Errorf("the era's record holds %v", rec.Fields)
			}
			era = int(e)
		case n != int64(len(commits))+1:
			return 0, nil, fmt.Errorf("a read found commit %d after commit %d", n, len(commits))
		default:
			text, ok := rec.Fields["t"].(string)
			if !ok {
				return 0, nil, fmt.Errorf("%s holds %v", rec.Key, rec.Fields)
			}
			commits = append(commits, text)
		}
	}
	return era, commits, nil
}

// checkCommit checks that the record of commit n, wherever h finds it,
// holds its whole commit: its text's vector, and its two links, or one for
// the first commit. Each call reads one point in the log, so the checks a
// reader makes through the database read the record as some one commit left
// it. When the record isn't there, as after a backup with fewer commits went
// into place, there's nothing to check, and it says so.
func checkCommit(h handle, n int) (found bool, err error) {
	key := place(int64(n))
	f, err := h.Get(key)
	if errors.Is(err, hc.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	text, _ := f["t"].(string)
	vec, _ := f["vec"].(hc.Vector)
	if !slices.Equal(vec, vecFor(text)) {
		return true, fmt.Errorf("%s holds the text %q and the vector %v, which isn't the text's", key, short(text), vec)
	}
	links, err := h.Neighbours(key, hc.Out, "")
	if errors.Is(err, hc.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	want := []hc.Link{{From: key, Type: "group", To: anchorKey(n)}}
	if n > 1 {
		want = append(want, hc.Link{From: key, Type: "next", To: place(int64(n - 1))})
	}
	if !slices.Equal(links, want) {
		return true, fmt.Errorf("%s has the links %v, where its commit made %v", key, links, want)
	}
	return true, nil
}

// checkScratch checks the scratch records with one Scan: the newest is the
// counter's, and the others are the ones the deletes since have left.
func checkScratch(h handle) (count int, err error) {
	recs, err := h.Scan("s:", "", 0)
	if err != nil || len(recs) == 0 {
		return 0, err
	}
	last := recs[len(recs)-1].Key
	count, err = strconv.Atoi(strings.TrimPrefix(last, "s:"))
	if err != nil {
		return 0, fmt.Errorf("a scratch record has the key %s", last)
	}
	var want []string
	for n := 1; n <= count; n++ {
		if !scratchGone(n, count) {
			want = append(want, scratchKey(n))
		}
	}
	got := make([]string, len(recs))
	for i, rec := range recs {
		got[i] = rec.Key
		if n, err := strconv.Atoi(strings.TrimPrefix(rec.Key, "s:")); err != nil || rec.Fields["n"] != int64(n) {
			return 0, fmt.Errorf("%s holds %v", rec.Key, rec.Fields)
		}
	}
	if !slices.Equal(got, want) {
		return 0, fmt.Errorf("after commit %d the scratch records are %d where %d are left: %s", count, len(got), len(want), short(strings.Join(got, " ")))
	}
	return count, nil
}

// checkCounts checks what Check finds against the commits it counts, the
// records with a vector: each commit made two links, the first one, and the
// records are the commits, the scratch records left, the ten anchors and the
// counter, and the era's record when the file came from a backup. Check
// reads one point in the log.
func checkCounts(db *hc.DB) error {
	rep, err := db.Check()
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("Check found problems: %s", strings.Join(rep.Problems, "; "))
	}
	n := rep.Vectors
	links, records := 0, 0
	if n > 0 {
		links, records = 2*n-1, n+scratchLeft(n)+10+1
	}
	if rep.Links != links || (rep.Records != records && rep.Records != records+1) {
		return fmt.Errorf("Check counts %d records, %d links and %d vectors, where %d commits make %d records, or one more with the era's, and %d links",
			rep.Records, rep.Links, rep.Vectors, n, records, links)
	}
	return nil
}

// packageWrite is a writer's work: commits through Update, each telling the
// harness of itself once the writer holds the write lock and has read the
// counter, before it makes any change, and taking a moment over its changes,
// as an Update that computes something would, so that kills land in the
// middle of Updates. With compact, about one commit in 16 is followed by
// Compact. A Compact that leaves the DB stuck ends the writer, as a stuck
// commit does, and the process's end lets go of the lock.
func packageWrite(w *Writer, compact bool) error {
	db, err := hc.Open(w.Path())
	if err != nil {
		return err
	}
	defer db.Close()
	for i := 1; ; i++ {
		letters := make([]byte, 1+w.Rand().IntN(100))
		for j := range letters {
			letters[j] = 'a' + byte(w.Rand().IntN(26))
		}
		text := fmt.Sprintf("writer %d's commit %d: %s", w.ID(), i, letters)
		pause := time.Duration(w.Rand().IntN(1000)) * time.Microsecond
		begun := false
		before := inodeAt(w.Path())
		err := db.Update(func(tx *hc.Tx) error {
			n, err := counter(tx)
			if err != nil {
				return err
			}
			w.Begin(text)
			begun = true
			time.Sleep(pause)
			return packageCommit(tx, n+1, text)
		})
		switch {
		case err == nil:
			w.Done()
		case begun:
			w.Failed(err)
		case !errors.Is(err, hc.ErrLockTimeout):
			return err
		}
		// With one writer and no backups, as in the kill test, another file
		// at the path after a commit is the compaction the commit set off.
		if !compact && err == nil && before != 0 && inodeAt(w.Path()) != before {
			w.Count(commitCompacted)
		}
		if compact && err == nil && w.Rand().IntN(compactEvery) == 0 {
			switch err := db.Compact(); {
			case errors.Is(err, hc.ErrStuck):
				return err
			case err != nil:
				w.Count(compactionFailed)
			default:
				w.Count(writerCompacted)
			}
		}
		time.Sleep(time.Duration(w.Rand().IntN(200)) * time.Microsecond)
	}
}

// commitCompacted is what a writer of the workload that doesn't call
// Compact counts when a commit of its own was followed by a compaction.
const commitCompacted = "commit followed by a compaction"

// inodeAt is the inode number of the file at path, or 0 when it can't be
// read.
func inodeAt(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ino
}

// packageRead is a reader's work: the database opened once with
// hypercrux.Open, and read until the process is killed. Each read is a few
// calls through the database, each of which follows the file and reloads it
// when another file has taken the path: a Scan of the commits, which it
// hands the harness from the first, the checks of the newest commit's
// record and of the scratch records, and Check's counts.
func packageRead(r *Reader) error {
	db, err := hc.Open(r.Path())
	if err != nil {
		return err
	}
	defer db.Close()
	last := 0
	for {
		era, commits, err := commitsOf(db)
		if err != nil {
			return err
		}
		if len(commits) > 0 {
			if _, err := checkCommit(db, len(commits)); err != nil {
				return err
			}
		}
		if _, err := checkScratch(db); err != nil {
			return err
		}
		if err := checkCounts(db); err != nil {
			return err
		}
		if era != last {
			r.Count(dbReadNewEra)
			last = era
		}
		r.Restart(era)
		for k, text := range commits {
			r.Apply(uint64(k+1), text)
		}
		time.Sleep(time.Duration(r.Rand().IntN(2000)) * time.Microsecond)
	}
}

// packageWhole checks, through the transaction tx, one point in the log, that
// every commit is there whole, with its vector and its links, that the
// scratch records and the counter are the last commit's, and returns the
// commits, in order.
func packageWhole(tx *hc.Tx) ([]string, error) {
	_, commits, err := commitsOf(tx)
	if err != nil {
		return nil, err
	}
	for n := 1; n <= len(commits); n++ {
		if found, err := checkCommit(tx, n); err != nil || !found {
			return nil, fmt.Errorf("commit %d of %d: found %v, %v", n, len(commits), found, err)
		}
	}
	count, err := checkScratch(tx)
	if err != nil {
		return nil, err
	}
	c, err := counter(tx)
	if err != nil {
		return nil, err
	}
	if count != len(commits) || c != len(commits) {
		return nil, fmt.Errorf("the file holds %d commits, with scratch records up to %d and the counter at %d", len(commits), count, c)
	}
	return commits, nil
}

// packageFinal reads the database through an Update, holding the write lock
// as the next writer would, so the check of the end of the log has run, and
// checks every commit there (packageWhole), then Check's counts.
func packageFinal(path string) ([]string, error) {
	db, err := hc.Open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var commits []string
	err = db.Update(func(tx *hc.Tx) error {
		var err error
		commits, err = packageWhole(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return commits, checkCounts(db)
}

// packageBackup copies the database at path to to inside an Update, which
// holds the write lock, so the copy ends at a commit, and then puts the era's
// record into the copy, through a DB of its own, since nobody else has it
// open.
func packageBackup(path, to string, era int) ([]string, error) {
	db, err := hc.Open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var commits []string
	err = db.Update(func(tx *hc.Tx) error {
		var err error
		if commits, err = packageWhole(tx); err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o644)
	})
	if err != nil {
		return nil, err
	}
	b, err := hc.Open(to)
	if err != nil {
		return nil, err
	}
	if err := b.Put(place(0), hc.Fields{"era": era}); err != nil {
		b.Close()
		return nil, err
	}
	return commits, b.Close()
}

// packageRestore moves the backup at from into place inside an Update on the
// file at path, which holds that file's write lock, so no compaction is
// between its look at the path and its rename, and returns the commits the
// file held.
func packageRestore(from, path string) ([]string, error) {
	db, err := hc.Open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var gone []string
	err = db.Update(func(tx *hc.Tx) error {
		var err error
		if gone, err = packageWhole(tx); err != nil {
			return err
		}
		return os.Rename(from, path)
	})
	return gone, err
}

// TestKilledWritersThroughThePackage is the 200 SIGKILLs through the public
// package: a writer committing with Update, killed with SIGKILL at a random
// moment in a life of 10 to 100 milliseconds and a new one started in its
// place, until 200 have been killed with a commit under way, between its
// Begin, once the Update holds the write lock, and its end, or 20 with
// -short. A reader follows through hypercrux.Open. The file at the end holds
// every commit a writer saw succeed, and each commit under way at a kill is
// there whole, with its records, its vector, its links and its delete, or not
// at all, and so is every commit the reader saw, at every read. 0.x's
// KilledWritersNeverLeaveAMess, which the conformance suite holds, runs on
// the Beta too since task G4 brought Query.
func TestKilledWritersThroughThePackage(t *testing.T) {
	kills, took := 200, 10*time.Second
	if testing.Short() {
		kills, took = 20, 0
	}
	rep, err := Run(packaged, Options{
		Path:           filepath.Join(t.TempDir(), "db.hcx"),
		Writers:        1,
		Readers:        1,
		Time:           took,
		KilledUnderWay: kills,
		WriterLife:     Span{Min: 10 * time.Millisecond, Max: 100 * time.Millisecond},
		ReaderLife:     Span{Min: 200 * time.Millisecond, Max: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
	if rep.KilledUnderWay < kills {
		t.Errorf("%d writers were killed with a commit under way, where %d are wanted", rep.KilledUnderWay, kills)
	}
	if rep.UnderWayIn == 0 {
		t.Error("no commit under way at a kill was in the file at the end, so no kill came after a commit's batch was written")
	}
	t.Logf("%d of %d writers killed with a commit under way, and %d of those commits in the file at the end", rep.KilledUnderWay, rep.WritersKilled, rep.UnderWayIn)
}

// TestManyProcessesThroughThePackage: two writers committing with Update, and
// two readers that open the database once with hypercrux.Open and read it
// until they're killed, each killed with SIGKILL at a random moment and a new
// one started in its place, for 2 seconds, or half a second with -short.
// Writers compact the database with Compact after about one commit in 16,
// and a writer's slot stays empty for up to 50 milliseconds after its writer
// is killed, so a reader can be the first to find what it left. Meanwhile the run takes a
// backup at random moments, 50 to 250 milliseconds apart, and moves it into
// place as long again after, or further apart in a long run (restoresFor, in
// reload_test.go). Every reader sees every commit once and in order, in each
// era of the log, through the compactions and the backups, the readers agree
// with each other and with the files, every commit a writer saw succeed is in
// the file at the end or was lost with the file a backup replaced, and every
// commit is whole wherever it's read. The run goes on until writers have
// compacted 5 times and readers have read a backup.
func TestManyProcessesThroughThePackage(t *testing.T) {
	rep, err := Run(packagedCompacting, Options{
		Path:       filepath.Join(t.TempDir(), "db.hcx"),
		ReaderLife: Span{Min: 100 * time.Millisecond, Max: time.Second},
		WriterGap:  Span{Min: 0, Max: 50 * time.Millisecond},
		Restores:   restoresFor(Span{Min: 50 * time.Millisecond, Max: 250 * time.Millisecond}),
		Least:      map[string]int{writerCompacted: 5, dbReadNewEra: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}
