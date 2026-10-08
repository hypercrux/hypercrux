// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// The benchmarks share files built once per run, as 0.1's do: tables of
// random vectors of several sizes, and a graph of 100,000 records with 5
// links each. Each engine gets its own file of each, built by the same calls
// from the same seeds, so the two hold the same records, fields, links and
// vectors.

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

var (
	fixtureDir  string
	fixtureOnce sync.Once
	fixtureMu   sync.Mutex
	fixtures    = map[string]built{}
)

// built is a fixture's file.
type built struct {
	path string
	// note is what the results should say about the file, or "": that the
	// Beta's file is a log of the commits that built it, until Compact
	// works.
	note string
}

// fixture returns the file build makes on engine e, building it the first
// time it's asked for. On the Beta the file is then compacted once Compact
// works (tasks F8 and G5), since BETA.md's target for opening is for a
// compacted file. Until then it's a log of the commits that built it, and
// the fixture's note says so.
func fixture(b *testing.B, e engine, name string, build func(d db) error) built {
	b.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hypercrux-beta-bench-")
		if err != nil {
			panic(err)
		}
		fixtureDir = dir
	})
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if f, ok := fixtures[e.name+"/"+name]; ok {
		return f
	}
	f := built{path: filepath.Join(fixtureDir, e.name+"-"+name+".db")}
	d, err := e.open(f.path)
	if err != nil {
		b.Fatal(err)
	}
	c := &counting{db: d}
	if err := build(c); err != nil {
		d.close()
		b.Fatalf("building %s on %s: %v", name, e.name, err)
	}
	if cd, ok := d.(compacter); ok {
		switch err := cd.compact(); {
		case errors.Is(err, errors.ErrUnsupported):
			f.note = fmt.Sprintf("the file is a log of the %s that built it, until compaction (tasks F8 and G5)", count(c.commits, "commit"))
		case err != nil:
			d.close()
			b.Fatalf("compacting %s on %s: %v", name, e.name, err)
		}
	}
	if err := d.close(); err != nil {
		b.Fatal(err)
	}
	fixtures[e.name+"/"+name] = f
	return f
}

// counting counts the commits made through it.
type counting struct {
	db
	commits int
}

func (c *counting) update(fn func(t tx) error) error {
	c.commits++
	return c.db.update(fn)
}

func (c *counting) put(key string, f map[string]any) error {
	c.commits++
	return c.db.put(key, f)
}

func (c *counting) link(from, typ, to string) error {
	c.commits++
	return c.db.link(from, typ, to)
}

// size is how many records a fixture of n records holds: n, or a hundredth
// of it with -short, but at least 10, which Nearest's searches need.
func size(n int) int {
	if testing.Short() {
		return max(n/100, 10)
	}
	return n
}

// vectorTable fills table docs with n records holding random vectors of dims
// values, and a field grp from 0 to 9, 5,000 records a commit.
func vectorTable(e engine, n, dims int) func(d db) error {
	return func(d db) error {
		r := rand.New(rand.NewPCG(uint64(n), uint64(dims)))
		for start := 0; start < n; start += 5000 {
			err := d.update(func(t tx) error {
				for i := start; i < min(start+5000, n); i++ {
					if err := t.put("docs:"+strconv.Itoa(i), map[string]any{"grp": i % 10, "vec": e.vector(randomVector(r, dims))}); err != nil {
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

// graph builds n records, 10,000 a commit, and then in one commit gives
// each 5 links of the type "to" to records picked at random. A link picked
// twice is added once, and a record can link to itself.
func graph(n int) func(d db) error {
	return func(d db) error {
		r := rand.New(rand.NewPCG(uint64(n), 6))
		for start := 0; start < n; start += 10000 {
			err := d.update(func(t tx) error {
				for i := start; i < min(start+10000, n); i++ {
					if err := t.put("node:"+strconv.Itoa(i), map[string]any{"n": i}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return d.update(func(t tx) error {
			for i := 0; i < n; i++ {
				for j := 0; j < 5; j++ {
					if err := t.link("node:"+strconv.Itoa(i), "to", "node:"+strconv.Itoa(r.IntN(n))); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
}

// randomVector is 0.1's: dims values from a normal distribution.
func randomVector(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// openTemp opens a new database on e, closed and removed once the benchmark
// has run.
func openTemp(b *testing.B, e engine) db {
	b.Helper()
	d, err := e.open(filepath.Join(b.TempDir(), "test.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.close() })
	return d
}

// openFixture opens a fixture's file on e.
func openFixture(b *testing.B, e engine, f built) db {
	b.Helper()
	d, err := e.open(f.path)
	if err != nil {
		b.Fatal(err)
	}
	return d
}
