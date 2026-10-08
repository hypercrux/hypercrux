// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// The benchmarks are 0.1's, by name, from bench_test.go at the root of the
// repository, with the same data and the same calls. Each runs on both
// engines, 0.x first, as sub-benchmarks named for the engine.

// both runs a benchmark on each engine in turn, so one run gives the two
// side by side. A benchmark that later names is skipped on the Beta, with
// the task that makes it run.
func both(b *testing.B, bench func(b *testing.B, e engine)) {
	name := strings.TrimPrefix(b.Name(), "Benchmark")
	for _, e := range engines {
		b.Run(e.name, func(b *testing.B) {
			if w, ok := later[name]; ok && e.name == betaName {
				b.Skip(w.reason())
			}
			bench(b, e)
		})
	}
}

// later names the benchmarks the Beta can't run yet, by 0.1's name, each
// with the task that makes it run and the Beta's calls that wait for that
// task. TestTheSkipsStillWait fails once those calls work, so the task that
// makes them work takes its lines out, and the results fill in.
var later = map[string]wait{
	"Nearest_1k_384dims":                 {"G2", []string{"Nearest"}},
	"Nearest_10k_384dims":                {"G2", []string{"Nearest"}},
	"Nearest_100k_384dims":               {"G2", []string{"Nearest"}},
	"Nearest_100k_384dims_tenthFiltered": {"G4", []string{"Nearest with a filter"}},
	"Nearest_10k_1536dims":               {"G2", []string{"Nearest"}},
	"Walk_100k_depth1":                   {"G2", []string{"Link", "Walk"}},
	"Walk_100k_depth2":                   {"G2", []string{"Link", "Walk"}},
	"Walk_100k_depth3":                   {"G2", []string{"Link", "Walk"}},
	"Link":                               {"G2", []string{"Link"}},
}

// wait is what a benchmark on the Beta waits for.
type wait struct {
	task  string
	calls []string
}

func (w wait) reason() string {
	return fmt.Sprintf("waits for task %s, which brings %s", w.task, strings.Join(w.calls, " and "))
}

// benchNearest finds the 10 records closest to a random vector among n with
// vectors of dims values, all of them or those that pass where.
func benchNearest(n, dims int, where string, args ...any) func(b *testing.B, e engine) {
	return func(b *testing.B, e engine) {
		n := size(n)
		f := fixture(b, e, fmt.Sprintf("vec-%d-%d", n, dims), vectorTable(e, n, dims))
		d := openFixture(b, e, f)
		defer d.close()
		r := rand.New(rand.NewPCG(3, 4))
		q := randomVector(r, dims)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			hits, err := d.nearest("docs", q, 10, where, args...)
			if err != nil || hits != 10 {
				b.Fatalf("%v %d", err, hits)
			}
		}
	}
}

func BenchmarkNearest_1k_384dims(b *testing.B)   { both(b, benchNearest(1000, 384, "")) }
func BenchmarkNearest_10k_384dims(b *testing.B)  { both(b, benchNearest(10000, 384, "")) }
func BenchmarkNearest_100k_384dims(b *testing.B) { both(b, benchNearest(100000, 384, "")) }
func BenchmarkNearest_100k_384dims_tenthFiltered(b *testing.B) {
	both(b, benchNearest(100000, 384, "grp = ?", 3))
}
func BenchmarkNearest_10k_1536dims(b *testing.B) { both(b, benchNearest(10000, 1536, "")) }

// benchWalk follows every link out of a record picked at random, depth links
// deep, in the graph of 100,000 records with 5 links each.
func benchWalk(depth int) func(b *testing.B, e engine) {
	return func(b *testing.B, e engine) {
		n := size(100000)
		f := fixture(b, e, fmt.Sprintf("graph-%d", n), graph(n))
		d := openFixture(b, e, f)
		defer d.close()
		r := rand.New(rand.NewPCG(8, 9))
		b.ResetTimer()
		total := 0
		for i := 0; i < b.N; i++ {
			steps, err := d.walk("node:"+strconv.Itoa(r.IntN(n)), depth)
			if err != nil {
				b.Fatal(err)
			}
			total += steps
		}
		b.ReportMetric(float64(total)/float64(b.N), "records/walk")
	}
}

func BenchmarkWalk_100k_depth1(b *testing.B) { both(b, benchWalk(1)) }
func BenchmarkWalk_100k_depth2(b *testing.B) { both(b, benchWalk(2)) }
func BenchmarkWalk_100k_depth3(b *testing.B) { both(b, benchWalk(3)) }

// BenchmarkPut is one committed transaction per record, synced to disk.
func BenchmarkPut(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		d := openTemp(b, e)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := d.put("docs:"+strconv.Itoa(i), map[string]any{"title": "a title", "status": "open", "n": i}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// vectors are 256 random vectors of 384 values, which the puts take in
// turn.
func vectors() [][]float32 {
	r := rand.New(rand.NewPCG(1, 1))
	vecs := make([][]float32, 256)
	for i := range vecs {
		vecs[i] = randomVector(r, 384)
	}
	return vecs
}

// BenchmarkPut_384dims adds a 384-value vector to each record.
func BenchmarkPut_384dims(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		d := openTemp(b, e)
		vecs := vectors()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := d.put("docs:"+strconv.Itoa(i), map[string]any{"title": "a title", "vec": e.vector(vecs[i%256])}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPutBatch_384dims puts 1,000 records with 384-value vectors per
// transaction, and reports the time per record.
func BenchmarkPutBatch_384dims(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		d := openTemp(b, e)
		vecs := vectors()
		b.ResetTimer()
		for i := 0; i < b.N; i += 1000 {
			err := d.update(func(t tx) error {
				for j := i; j < min(i+1000, b.N); j++ {
					if err := t.put("docs:"+strconv.Itoa(j), map[string]any{"title": "a title", "vec": e.vector(vecs[j%256])}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGet reads the records of a table of 10,000 with 384-value
// vectors in turn.
func BenchmarkGet(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		n := size(10000)
		f := fixture(b, e, fmt.Sprintf("vec-%d-384", n), vectorTable(e, n, 384))
		d := openFixture(b, e, f)
		defer d.close()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := d.get("docs:" + strconv.Itoa(i%n)); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkLink is one committed transaction per link.
func BenchmarkLink(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		d := openTemp(b, e)
		err := d.update(func(t tx) error {
			for i := 0; i < 1000; i++ {
				if err := t.put("node:"+strconv.Itoa(i), nil); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := d.link("node:"+strconv.Itoa(i%1000), "to"+strconv.Itoa(i/1000), "node:"+strconv.Itoa((i*7+1)%1000)); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkOpen_100k_384dims opens the table of 100,000 records with
// 384-value vectors that Nearest_100k_384dims searches, and reads one record
// from it, the one target 0.1 has no figure for. The Beta reads the whole
// file into memory as it opens; 0.x opens the SQLite file and reads what the
// Get needs.
//
// Closing the database comes between iterations, outside the time, and on
// the Beta so does handing its copy back to the system, so each open starts
// with fresh memory, as a new process does. Until compaction (tasks F8 and
// G5), the Beta's file is a log of the 20 commits that built it, and the
// benchmark logs a line saying so, which the results give beside its time.
func BenchmarkOpen_100k_384dims(b *testing.B) {
	both(b, func(b *testing.B, e engine) {
		n := size(100000)
		f := fixture(b, e, fmt.Sprintf("vec-%d-384", n), vectorTable(e, n, 384))
		if f.note != "" {
			b.Log(f.note)
		}
		if e.inMemory {
			debug.FreeOSMemory()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			d, err := e.open(f.path)
			if err != nil {
				b.Fatal(err)
			}
			if err := d.get("docs:" + strconv.Itoa(i%n)); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			if err := d.close(); err != nil {
				b.Fatal(err)
			}
			if e.inMemory {
				debug.FreeOSMemory()
			}
			b.StartTimer()
		}
	})
}
