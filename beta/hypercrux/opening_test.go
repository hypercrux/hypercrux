// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Opening 100,000 records with 384-value vectors (I1), against BETA.md's
// target of 250 ms for a compacted file. beta/bench's Open_100k_384dims
// times the same open from the log of the 20 commits that built the file,
// until Compact works (G5), and its tests hold it to 0.1's benchmarks one for
// one, so the compacted file is timed here, beside the log.
//
// The file is the one beta/bench builds: docs:0 to docs:99999, each with grp,
// from 0 to 9, and a vector of 384 values drawn from the same seed, 5,000
// records a commit. The compacted file is that log compacted as Compact will
// compact it (G5): a copy read from the file, and its snapshot written into
// NAME.compact, which takes the path.
//
// With HYPERCRUX_OPEN_DIR set, the files are built there once and kept, so a
// run with -cpuprofile times the opens alone.

// openingRecords is how many records the files hold: 100,000, or 1,000 with
// -short.
func openingRecords() int {
	if testing.Short() {
		return 1000
	}
	return 100000
}

// BenchmarkOpenFile_100k_384dims times Open and one Get, as beta/bench's
// Open_100k_384dims does, from the log of 20 commits and from the compacted
// file. Close and handing the copy back to the system come between the
// opens, outside the time, so each open builds its copy in fresh memory, as a
// new process does.
func BenchmarkOpenFile_100k_384dims(b *testing.B) {
	n := openingRecords()
	dir := os.Getenv("HYPERCRUX_OPEN_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	log := filepath.Join(dir, "open-"+strconv.Itoa(n)+"-log.hcx")
	compacted := filepath.Join(dir, "open-"+strconv.Itoa(n)+"-compacted.hcx")
	if !exists(b, log) {
		buildOpeningLog(b, log, n)
	}
	if !exists(b, compacted) {
		compactCopy(b, log, compacted)
	}
	for _, f := range []struct{ name, path string }{{"log", log}, {"compacted", compacted}} {
		b.Run(f.name, func(b *testing.B) { timeOpens(b, f.path, n) })
	}
}

// timeOpens times b.N opens of the database at path, each with a Get of one
// of its n records.
func timeOpens(b *testing.B, path string, n int) {
	debug.FreeOSMemory()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db, err := hc.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.Get("docs:" + strconv.Itoa(i*7919%n)); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		debug.FreeOSMemory()
		b.StartTimer()
	}
}

// exists reports whether a file is at path.
func exists(tb testing.TB, path string) bool {
	tb.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		tb.Fatal(err)
	}
	return true
}

// buildOpeningLog makes the database at path as beta/bench's vectorTable
// makes it: n records with grp and a vector of 384 values, 5,000 a commit.
func buildOpeningLog(tb testing.TB, path string, n int) {
	tb.Helper()
	db, err := hc.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(uint64(n), 384))
	for start := 0; start < n; start += 5000 {
		err := db.Update(func(tx *hc.Tx) error {
			for i := start; i < min(start+5000, n); i++ {
				v := make(hc.Vector, 384)
				for j := range v {
					v[j] = float32(r.NormFloat64())
				}
				if err := tx.Put("docs:"+strconv.Itoa(i), hc.Fields{"grp": i % 10, "vec": v}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
}

// openingCopy is a logfile.Target that puts every batch straight into a copy
// of its own, which nothing else reads.
type openingCopy struct{ s *store.Store }

func (c *openingCopy) Apply(seq uint64, changes []format.Change) error {
	return c.s.LoadBatch(seq, changes)
}

func (c *openingCopy) Reset() { c.s = store.New() }

// compactCopy copies the database at from to to, beside it under another
// name first, compacts the copy (compactAt), and moves it into place.
func compactCopy(tb testing.TB, from, to string) {
	tb.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		tb.Fatal(err)
	}
	part := to + ".part"
	if err := os.WriteFile(part, b, 0o644); err != nil {
		tb.Fatal(err)
	}
	compactAt(tb, part)
	if err := os.Rename(part, to); err != nil {
		tb.Fatal(err)
	}
}

// compactAt compacts the database at path as Compact will (G5): it opens the
// file with a copy of its own, takes the write lock, and writes the copy's
// snapshot into NAME.compact, which takes the path.
func compactAt(tb testing.TB, path string) {
	tb.Helper()
	c := &openingCopy{s: store.New()}
	l, err := logfile.Open(fsys.OS{}, path, c, logfile.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	if err := l.Lock(); err != nil {
		tb.Fatal(err)
	}
	defer l.Unlock()
	if err := c.s.Read(func(r store.Reader) error { return l.Compact(r.Snapshot()) }); err != nil {
		tb.Fatal(err)
	}
}
