// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// The benchmarks share files built once per run: tables of random vectors
// of several sizes, and a graph of 100,000 records with 5 links each.

var (
	fixtureDir  string
	fixtureOnce sync.Once
	fixtureMu   sync.Mutex
	fixtures    = map[string]string{}
)

func cleanupFixtures() {
	if fixtureDir != "" {
		os.RemoveAll(fixtureDir)
	}
}

// fixture returns the path of a file built by build, building it the first
// time it is asked for.
func fixture(b *testing.B, name string, build func(db *DB) error) string {
	b.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hypercrux-bench-")
		if err != nil {
			panic(err)
		}
		fixtureDir = dir
	})
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if p, ok := fixtures[name]; ok {
		return p
	}
	p := filepath.Join(fixtureDir, name+".db")
	db, err := Open(p)
	if err != nil {
		b.Fatal(err)
	}
	if err := build(db); err != nil {
		b.Fatal(err)
	}
	db.Close()
	fixtures[name] = p
	return p
}

// vectorTable fills table docs with n records holding random vectors of dims
// values, and a field group from 0 to 9.
func vectorTable(n, dims int) func(db *DB) error {
	return func(db *DB) error {
		r := rand.New(rand.NewPCG(uint64(n), uint64(dims)))
		for start := 0; start < n; start += 5000 {
			err := db.Update(func(tx *Tx) error {
				for i := start; i < min(start+5000, n); i++ {
					if err := tx.Put("docs:"+strconv.Itoa(i), Fields{"grp": i % 10, "vec": randomVector(r, dims)}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func benchNearest(b *testing.B, n, dims int, where string, args ...any) {
	path := fixture(b, fmt.Sprintf("vec-%d-%d", n, dims), vectorTable(n, dims))
	db, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(3, 4))
	q := randomVector(r, dims)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := db.Nearest("docs", q, 10, where, args...)
		if err != nil || len(hits) != 10 {
			b.Fatalf("%v %d", err, len(hits))
		}
	}
}

func BenchmarkNearest_1k_384dims(b *testing.B)   { benchNearest(b, 1000, 384, "") }
func BenchmarkNearest_10k_384dims(b *testing.B)  { benchNearest(b, 10000, 384, "") }
func BenchmarkNearest_100k_384dims(b *testing.B) { benchNearest(b, 100000, 384, "") }
func BenchmarkNearest_100k_384dims_tenthFiltered(b *testing.B) {
	benchNearest(b, 100000, 384, "grp = ?", 3)
}
func BenchmarkNearest_10k_1536dims(b *testing.B) { benchNearest(b, 10000, 1536, "") }

// graph builds n records with 5 random outgoing links each.
func graph(n int) func(db *DB) error {
	return func(db *DB) error {
		r := rand.New(rand.NewPCG(uint64(n), 6))
		for start := 0; start < n; start += 10000 {
			err := db.Update(func(tx *Tx) error {
				for i := start; i < min(start+10000, n); i++ {
					if err := tx.Put("node:"+strconv.Itoa(i), Fields{"n": i}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return db.Update(func(tx *Tx) error {
			stmt, err := tx.tx.Prepare(`INSERT OR IGNORE INTO hc_links (src, type, dst) VALUES (?, 'to', ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for i := 0; i < n; i++ {
				for j := 0; j < 5; j++ {
					if _, err := stmt.Exec("node:"+strconv.Itoa(i), "node:"+strconv.Itoa(r.IntN(n))); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
}

func benchWalk(b *testing.B, depth int) {
	path := fixture(b, "graph-100k", graph(100000))
	db, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(8, 9))
	b.ResetTimer()
	total := 0
	for i := 0; i < b.N; i++ {
		steps, err := db.Walk("node:"+strconv.Itoa(r.IntN(100000)), Out, "", depth)
		if err != nil {
			b.Fatal(err)
		}
		total += len(steps)
	}
	b.ReportMetric(float64(total)/float64(b.N), "records/walk")
}

func BenchmarkWalk_100k_depth1(b *testing.B) { benchWalk(b, 1) }
func BenchmarkWalk_100k_depth2(b *testing.B) { benchWalk(b, 2) }
func BenchmarkWalk_100k_depth3(b *testing.B) { benchWalk(b, 3) }

// BenchmarkPut is one committed transaction per record, flushed to disk.
func BenchmarkPut(b *testing.B) {
	db, _ := openTemp(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Put("docs:"+strconv.Itoa(i), Fields{"title": "a title", "status": "open", "n": i}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPut_384dims adds a 384-value vector to each record.
func BenchmarkPut_384dims(b *testing.B) {
	db, _ := openTemp(b)
	r := rand.New(rand.NewPCG(1, 1))
	vecs := make([]Vector, 256)
	for i := range vecs {
		vecs[i] = randomVector(r, 384)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Put("docs:"+strconv.Itoa(i), Fields{"title": "a title", "vec": vecs[i%256]}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutBatch puts 1,000 records with 384-value vectors per
// transaction, and reports the time per record.
func BenchmarkPutBatch_384dims(b *testing.B) {
	db, _ := openTemp(b)
	r := rand.New(rand.NewPCG(1, 1))
	vecs := make([]Vector, 256)
	for i := range vecs {
		vecs[i] = randomVector(r, 384)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i += 1000 {
		err := db.Update(func(tx *Tx) error {
			for j := i; j < min(i+1000, b.N); j++ {
				if err := tx.Put("docs:"+strconv.Itoa(j), Fields{"title": "a title", "vec": vecs[j%256]}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	path := fixture(b, "vec-10000-384", vectorTable(10000, 384))
	db, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Get("docs:" + strconv.Itoa(i%10000)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLink is one committed transaction per link.
func BenchmarkLink(b *testing.B) {
	db, _ := openTemp(b)
	ok(b, db.Update(func(tx *Tx) error {
		for i := 0; i < 1000; i++ {
			if err := tx.Put("node:"+strconv.Itoa(i), nil); err != nil {
				return err
			}
		}
		return nil
	}))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Link("node:"+strconv.Itoa(i%1000), "to"+strconv.Itoa(i/1000), "node:"+strconv.Itoa((i*7+1)%1000)); err != nil {
			b.Fatal(err)
		}
	}
}
