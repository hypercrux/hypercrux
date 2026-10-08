// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The real log through compactions and backups moved into place (F9). The
// file a reader reads is now and then another one, a compaction's or a
// backup's, and after a compaction its batches aren't the commits. So each
// commit puts a record of its own, keyed by its place in the log, which the
// writer works out from its copy once it holds the write lock: c:000000001
// for the first, and so on, with the commit's text in the field t. A reader
// works out the commits from the store's state, record by record, whatever
// batches brought them. A backup's era is a record of its own, c:000000000,
// with the field era, which the run's Backup puts into the copy before it's
// moved into place, and every compaction after carries.
//
//   - Writers commit through the real log, as realWrite does, on a slow disk,
//     with a copy in the store, as the public package keeps one, and after
//     about one commit in 16 they compact the database under the same lock,
//     as G5 will once Due says so.
//   - Readers open the database once and follow it until they're killed.
//     Half of them, by their numbers, follow through the log itself, calling
//     Follow, and Reload when Follow asks for it, with the store as their
//     Target. The other half follow through the public package, whose every
//     read follows the file and reloads it: each of their reads is a Scan of
//     the commits' table.
//   - The run takes a backup now and then, holding the write lock, marks it
//     with its era, and moves it into place a while later, holding the lock
//     of the file there, so that no compaction is between its look at the
//     path and its rename.
var realReloaded = Workload{Name: "real log reloaded", Write: reloadWrite, Read: reloadRead, Final: reloadFinal, Backup: reloadBackup, Restore: reloadRestore}

// compactEvery is about how many commits a writer makes for each compaction.
const compactEvery = 16

// What the workload's processes count.
const (
	writerCompacted   = "writer compacted the database"
	compactionFailed  = "compaction failed, and the database carried on in its old file"
	logReloadedSame   = "log follower reloaded a file of the era it read"
	logReloadedNewEra = "log follower reloaded a backup of a later era"
	dbReadNewEra      = "package follower read a backup of a later era"
)

// glue is the public package's target in small, a Reloader: while Open reads
// the file, and from a Reset until Loaded, nobody else can reach the copy, so
// each batch goes straight in, and otherwise through a transaction of its
// own.
type glue struct {
	s                *store.Store
	opening, loading bool
	resets           int
}

func (g *glue) Apply(seq uint64, changes []format.Change) error {
	if g.opening || g.loading {
		return g.s.LoadBatch(seq, changes)
	}
	return g.s.ApplyBatch(seq, changes)
}

func (g *glue) Reset() {
	g.s = store.New()
	g.loading = true
	g.resets++
}

func (g *glue) Loaded(error) { g.loading = false }

// copyOf is a database opened as the public package opens one, in small: its
// log and its copy.
type copyOf struct {
	l *logfile.Log
	g *glue
}

func openCopy(files fsys.FS, path string, o logfile.Options) (*copyOf, error) {
	g := &glue{s: store.New(), opening: true}
	l, err := logfile.Open(files, path, g, o)
	if err != nil {
		return nil, err
	}
	g.opening = false
	return &copyOf{l: l, g: g}, nil
}

// commit is the public package's Update in small: the write lock, the copy's
// transaction running fn, Commit with the log's Append, and when compact is
// true, a compaction under the same lock from a read of the copy, as G5's
// will be after a commit; then the lock goes. It returns the commit's error,
// and the compaction's.
func (d *copyOf) commit(fn func(tx *store.Tx) error, compact bool) (err, cerr error) {
	if err := d.l.Lock(); err != nil {
		return err, nil
	}
	defer func() {
		if e := d.l.Unlock(); err == nil {
			err = e
		}
	}()
	tx, err := d.g.s.Begin()
	if err != nil {
		return err, nil
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err, nil
	}
	if err := tx.Commit(d.l.Append); err != nil {
		return err, nil
	}
	if compact {
		cerr = d.g.s.Read(func(r store.Reader) error { return d.l.Compact(r.Snapshot()) })
	}
	return nil, cerr
}

// place is the key of the record that commit n puts, or of the era's record
// for 0.
func place(n int64) string { return fmt.Sprintf("c:%09d", n) }

// stateOf works out from a copy its era and its commits, in order, from the
// records each commit puts, keyed by its place in the log.
func stateOf(s *store.Store) (era int, commits []string, err error) {
	err = s.Read(func(r store.Reader) error {
		c, err := r.Scan("c:", "")
		if err != nil {
			return err
		}
		t, _ := r.Table("c")
		text, ofEra := t.Find("t"), t.Find("era")
		for {
			rec, more := c.Next()
			if !more {
				return nil
			}
			n, err := strconv.ParseInt(strings.TrimPrefix(rec.Key, "c:"), 10, 64)
			switch {
			case err != nil:
				return fmt.Errorf("a commit's record has the key %s", rec.Key)
			case n == 0:
				era = int(rec.Field(ofEra).Int())
			case n != int64(len(commits))+1:
				return fmt.Errorf("the copy holds commit %d after commit %d", n, len(commits))
			default:
				commits = append(commits, rec.Field(text).Text())
			}
		}
	})
	return era, commits, err
}

// reloadWrite is a writer's work: commits, each putting the record of its
// place in the log, and a count of them, and now and then a compaction,
// until the process is killed.
func reloadWrite(w *Writer) error {
	d, err := openCopy(slowSync{fsys.OS{}}, w.Path(), logfile.Options{})
	if err != nil {
		return err
	}
	defer d.l.Close()
	for n := 1; ; n++ {
		letters := make([]byte, 1+w.Rand().IntN(200))
		for i := range letters {
			letters[i] = 'a' + byte(w.Rand().IntN(26))
		}
		text := fmt.Sprintf("writer %d's commit %d: %s", w.ID(), n, letters)
		begun := false
		compact := w.Rand().IntN(compactEvery) == 0
		err, cerr := d.commit(func(tx *store.Tx) error {
			var count int64
			if rec, err := tx.Get("meta:n"); err == nil {
				count = rec.Field(0).Int()
			} else if !errors.Is(err, errs.ErrNotFound) {
				return err
			}
			w.Begin(text)
			begun = true
			if err := tx.Put(place(count+1), []format.Field{{Name: "t", Value: value.Text(text)}}); err != nil {
				return err
			}
			return tx.Put("meta:n", []format.Field{{Name: "n", Value: value.Int(count + 1)}})
		}, compact)
		switch {
		case err == nil:
			w.Done()
		case begun:
			w.Failed(err)
		case !errors.Is(err, errs.ErrLockTimeout):
			return err
		}
		switch {
		case errors.Is(cerr, errs.ErrStuck):
			return cerr
		case cerr != nil:
			w.Count(compactionFailed)
		case compact && err == nil:
			w.Count(writerCompacted)
		}
		time.Sleep(time.Duration(w.Rand().IntN(1000)) * time.Microsecond)
	}
}

// reloadRead is a reader's work: it follows the database through the log
// itself, or through the public package, by its number.
func reloadRead(r *Reader) error {
	if r.ID()%2 == 0 {
		return followThroughTheLog(r)
	}
	return followThroughThePackage(r)
}

// teller tells a reader what a copy holds: from the first commit after a
// reload, or in a new era, and otherwise the commits since it last told.
type teller struct {
	era    int
	told   int // the commits it has told in the era
	resets int // the copy's resets when it last told
}

func (tl *teller) tell(r *Reader, g *glue) error {
	era, commits, err := stateOf(g.s)
	if err != nil {
		return err
	}
	if g.resets != tl.resets || era != tl.era || len(commits) < tl.told {
		switch {
		case era != tl.era:
			r.Count(logReloadedNewEra)
		case g.resets != tl.resets:
			r.Count(logReloadedSame)
		}
		r.Restart(era)
		tl.era, tl.told, tl.resets = era, 0, g.resets
	}
	for k := tl.told; k < len(commits); k++ {
		r.Apply(uint64(k+1), commits[k])
	}
	tl.told = len(commits)
	return nil
}

// followThroughTheLog opens the database once with the store as its Target,
// and follows it until the process is killed: Follow, and Reload when Follow
// asks for it, then what the copy holds.
func followThroughTheLog(r *Reader) error {
	g := &glue{s: store.New(), opening: true}
	l, err := logfile.Open(fsys.OS{}, r.Path(), g, logfile.Options{})
	if err != nil {
		return err
	}
	g.opening = false
	defer l.Close()
	tl := &teller{resets: g.resets}
	era, _, err := stateOf(g.s)
	if err != nil {
		return err
	}
	r.Restart(era)
	tl.era = era
	for {
		if err := tl.tell(r, g); err != nil {
			return err
		}
		time.Sleep(time.Duration(r.Rand().IntN(2000)) * time.Microsecond)
		err := l.Follow()
		if errors.Is(err, logfile.ErrReplaced) {
			err = l.Reload()
		}
		if err != nil {
			return err
		}
	}
}

// followThroughThePackage opens the database once through the public
// package, and reads it until the process is killed: each read is a Scan of
// the commits' table, which follows the file and reloads it as it has to, and
// it tells the reader what the read found, from the first commit.
func followThroughThePackage(r *Reader) error {
	db, err := hc.Open(r.Path())
	if err != nil {
		return err
	}
	defer db.Close()
	last := 0
	for {
		recs, err := db.Scan("c:", "", 0)
		if err != nil {
			return err
		}
		era := 0
		var commits []string
		for _, rec := range recs {
			n, err := strconv.ParseInt(strings.TrimPrefix(rec.Key, "c:"), 10, 64)
			switch {
			case err != nil:
				return fmt.Errorf("a commit's record has the key %s", rec.Key)
			case n == 0:
				era = int(rec.Fields["era"].(int64))
			case n != int64(len(commits))+1:
				return fmt.Errorf("a read found commit %d after commit %d", n, len(commits))
			default:
				commits = append(commits, rec.Fields["t"].(string))
			}
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

// reloadFinal reads the database holding the write lock, as the next writer
// would, once its check of the end of the log has run, and works out its
// commits from the copy.
func reloadFinal(path string) ([]string, error) {
	d, err := openCopy(fsys.OS{}, path, logfile.Options{Wait: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	defer d.l.Close()
	if err := d.l.Lock(); err != nil {
		return nil, err
	}
	_, commits, err := stateOf(d.g.s)
	if e := d.l.Unlock(); err == nil {
		err = e
	}
	return commits, err
}

// reloadBackup copies the database at path to to, holding the write lock,
// so the copy ends at a commit, and then puts the era's record into the
// copy, which nobody else has open.
func reloadBackup(path, to string, era int) ([]string, error) {
	d, err := openCopy(fsys.OS{}, path, logfile.Options{Wait: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	defer d.l.Close()
	if err := d.l.Lock(); err != nil {
		return nil, err
	}
	_, commits, err := stateOf(d.g.s)
	if err == nil {
		var b []byte
		if b, err = os.ReadFile(path); err == nil {
			err = os.WriteFile(to, b, 0o644)
		}
	}
	if e := d.l.Unlock(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	c, err := openCopy(fsys.OS{}, to, logfile.Options{})
	if err != nil {
		return nil, err
	}
	defer c.l.Close()
	err, _ = c.commit(func(tx *store.Tx) error {
		return tx.Put(place(0), []format.Field{{Name: "era", Value: value.Int(int64(era))}})
	}, false)
	return commits, err
}

// reloadRestore moves the backup at from into place at path, holding the
// write lock of the file there, so that no compaction is between its look at
// the path and its rename, which would put the old data back over the
// backup, and returns the commits that file held.
func reloadRestore(from, path string) ([]string, error) {
	d, err := openCopy(fsys.OS{}, path, logfile.Options{Wait: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	defer d.l.Close()
	if err := d.l.Lock(); err != nil {
		return nil, err
	}
	_, gone, err := stateOf(d.g.s)
	if err == nil {
		err = os.Rename(from, path)
	}
	if e := d.l.Unlock(); err == nil {
		err = e
	}
	return gone, err
}

// realReloadedSlowly is realReloaded with a Restore that waits a fifth of a
// second after its rename before it returns, so that followers read the new
// era, and report it, before the run hears the restore is done.
var realReloadedSlowly = Workload{Name: "real log reloaded, slowly", Write: reloadWrite, Read: reloadRead, Final: reloadFinal, Backup: reloadBackup,
	Restore: func(from, path string) ([]string, error) {
		gone, err := reloadRestore(from, path)
		time.Sleep(200 * time.Millisecond)
		return gone, err
	}}

// TestAnEraCanBeReadBeforeItsRestoreReturns: a reader's report of a new era
// can come before the workload's Restore has returned, since the backup is in
// place from its rename on. The run begins the era before the backup goes
// into place, so the report finds it begun.
func TestAnEraCanBeReadBeforeItsRestoreReturns(t *testing.T) {
	rep, err := Run(realReloadedSlowly, Options{
		Path:       filepath.Join(t.TempDir(), "db.hcx"),
		Time:       300 * time.Millisecond,
		ReaderLife: Span{Min: 500 * time.Millisecond, Max: time.Second},
		Restores:   Span{Min: 20 * time.Millisecond, Max: 50 * time.Millisecond},
		Least:      map[string]int{logReloadedNewEra: 1, dbReadNewEra: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}

// TestFollowersSeeEveryCommitThroughReloads is F9's closing test: two writers
// committing through the real log on a slow disk, compacting the database
// after about one commit in 16, and two followers that open it once and
// follow it, half through the log and half through the public package, each
// killed with SIGKILL at a random moment and a new one started in its place,
// for 2 seconds, or half a second with -short. Followers live 100
// milliseconds to a second. Meanwhile the run takes a backup at random
// moments, 50 to 250 milliseconds apart, and moves it into place as long
// again after. Every follower sees every commit once and in order, in each
// era of the log, through compactions and the backups, the followers agree
// with each other and with the files, every commit a writer saw succeed is
// in the file at the end or was lost with the file a backup replaced, and
// every commit under way when its writer was killed is there whole or not at
// all. The run goes on until both kinds of follower have reloaded a backup,
// followers through the log have reloaded compacted files, and writers have
// compacted.
func TestFollowersSeeEveryCommitThroughReloads(t *testing.T) {
	rep, err := Run(realReloaded, Options{
		Path:       filepath.Join(t.TempDir(), "db.hcx"),
		ReaderLife: Span{Min: 100 * time.Millisecond, Max: time.Second},
		Restores:   Span{Min: 50 * time.Millisecond, Max: 250 * time.Millisecond},
		Least:      map[string]int{writerCompacted: 5, logReloadedSame: 3, logReloadedNewEra: 1, dbReadNewEra: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}
