// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// Opening 100,000 records with 384-value vectors (I1), against BETA.md's
// target of 250 ms for a compacted file, from the log of the 20 commits that
// built the file and from that log compacted. beta/bench's
// Open_100k_384dims times the compacted file too, since G5, and its tests
// hold it to 0.1's benchmarks one for one, so the log is timed here beside
// it, as I1 timed it.
//
// The file holds what beta/bench builds: docs:0 to docs:99999, each with grp,
// from 0 to 9, and a vector of 384 values drawn from the same seed, 5,000
// records a commit. Those commits only add records, so they make nothing
// dead, and the public package compacts such a file only at the limit on
// commits (C1). The log is built with a MinGrowth no file reaches all the
// same, as it was when the file was compacted each time it doubled (G5), so
// it keeps its 20 commits whatever the rule. The compacted file is a copy of
// the log, compacted by Compact.
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
// makes it: n records with grp and a vector of 384 values, 5,000 a commit,
// with no compaction after a commit, so the file is the log of its commits.
func buildOpeningLog(tb testing.TB, path string, n int) {
	tb.Helper()
	db, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{MinGrowth: math.MaxInt64})
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

// compactCopy copies the database at from to to, beside it under another
// name first, compacts the copy with Compact, and moves it into place.
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
	db, err := hc.Open(part)
	if err != nil {
		tb.Fatal(err)
	}
	if err := db.Compact(); err != nil {
		tb.Fatal(err)
	}
	if err := db.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := os.Rename(part, to); err != nil {
		tb.Fatal(err)
	}
}
