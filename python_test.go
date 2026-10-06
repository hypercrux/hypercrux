// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"errors"
	"math"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These tests write and read a HyperCrux file from another SQLite: Python's
// sqlite3 module, through examples/python/hcfile.py, which uses plain SQL
// and nothing from HyperCrux.

func python(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	return py
}

// hcfile runs hcfile.py and returns its output. wantFail says the command
// should fail, with stderr containing that text.
func hcfile(t *testing.T, py string, wantFail string, args ...string) string {
	t.Helper()
	cmd := exec.Command(py, append([]string{"-I", "-B", filepath.Join("examples", "python", "hcfile.py")}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if wantFail != "" {
		if err == nil || !strings.Contains(stderr.String(), wantFail) {
			t.Fatalf("hcfile.py %v should have failed with %q: err=%v stderr=%s", args, wantFail, err, stderr.String())
		}
		return ""
	}
	if err != nil {
		t.Fatalf("hcfile.py %v: %v: %s", args, err, stderr.String())
	}
	return stdout.String()
}

func TestPythonFollowsTheRules(t *testing.T) {
	py := python(t)
	db, path := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"title": "one", "vec": Vector{1, 0, 0}}))
	ok(t, db.Put("docs:2", Fields{"title": "two", "vec": Vector{0, 1, 0}}))
	ok(t, db.Put("customer:42", Fields{"name": "Dana"}))
	ok(t, db.Link("customer:42", "owns", "docs:1"))

	// Python writes a record with a vector, and links, with plain SQL.
	hcfile(t, py, "", "put", path, "docs:py", `{"title": "from python", "vec": [0.5, 0.5, 0]}`)
	hcfile(t, py, "", "link", path, "docs:py", "cites", "docs:1")
	hcfile(t, py, "", "link", path, "customer:42", "owns", "docs:py")
	// The triggers stop Python where they stop Go.
	hcfile(t, py, "keys in table docs are text that starts with docs:", "sql", path, `INSERT INTO docs (key) VALUES ('notes:1')`)
	hcfile(t, py, "a link to a key that does not exist", "link", path, "docs:py", "cites", "docs:404")
	hcfile(t, py, "vec must be a blob of float32 values", "put", path, "docs:bad", `{"vec": [1, 2]}`)
	hcfile(t, py, "a key never changes", "sql", path, `UPDATE docs SET key = 'docs:9' WHERE key = 'docs:2'`)
	// Deleting from Python takes the record's links with it.
	hcfile(t, py, "", "delete", path, "docs:1")

	f := must[Fields](t)(db.Get("docs:py"))
	if f["title"] != "from python" || !vecEqual(f["vec"].(Vector), Vector{0.5, 0.5, 0}) {
		t.Fatalf("Python's record reads back as %v", f)
	}
	links := must[[]Link](t)(db.Neighbours("customer:42", Out, ""))
	if len(links) != 1 || links[0].To != "docs:py" {
		t.Fatalf("customer:42 links after Python's delete: %v", links)
	}
	if links := must[[]Link](t)(db.Neighbours("docs:py", Out, "")); len(links) != 0 {
		t.Fatalf("docs:py still links to the deleted docs:1: %v", links)
	}
	if _, err := db.Get("docs:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("docs:1 after Python deleted it: %v", err)
	}
	hits := must[[]Hit](t)(db.Nearest("docs", Vector{0.5, 0.5, 0}, 1, ""))
	if len(hits) != 1 || hits[0].Key != "docs:py" {
		t.Fatalf("Nearest: %v", hits)
	}
	checkOK(t, db)
}

func vecEqual(a, b Vector) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPythonAgreesOnNearest compares HyperCrux's search with Python reading
// the raw vectors and comparing them itself.
func TestPythonAgreesOnNearest(t *testing.T) {
	py := python(t)
	db, path := openTemp(t)
	r := rand.New(rand.NewPCG(21, 22))
	ok(t, db.Update(func(tx *Tx) error {
		for i := 0; i < 500; i++ {
			if err := tx.Put("docs:"+strconv.Itoa(i), Fields{"vec": randomVector(r, 64)}); err != nil {
				return err
			}
		}
		return nil
	}))
	for trial := 0; trial < 5; trial++ {
		q := randomVector(r, 64)
		hits := must[[]Hit](t)(db.Nearest("docs", q, 10, ""))
		out := hcfile(t, py, "", "nearest", path, "docs", q.String(), "10")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != len(hits) {
			t.Fatalf("Python found %d, HyperCrux %d", len(lines), len(hits))
		}
		for i, line := range lines {
			d, key, _ := strings.Cut(line, " ")
			pd, err := strconv.ParseFloat(d, 64)
			if err != nil || key != hits[i].Key || math.Abs(pd-hits[i].Distance) > 1e-9 {
				t.Fatalf("result %d: Python %s %s, HyperCrux %s %v", i, d, key, hits[i].Key, hits[i].Distance)
			}
		}
	}
}

// TestPythonAndGoWriteAtOnce has Python and Go write records and links to
// the same file at the same time.
func TestPythonAndGoWriteAtOnce(t *testing.T) {
	py := python(t)
	db, path := openTemp(t)
	ok(t, db.Put("hub:0", nil))
	ok(t, db.Put("py:seed", Fields{"i": -1}))
	const n = 300
	var wg sync.WaitGroup
	var goErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n && goErr == nil; i++ {
			key := "go:" + strconv.Itoa(i)
			goErr = db.Update(func(tx *Tx) error {
				if err := tx.Put(key, Fields{"i": i, "vec": Vector{float32(i + 1), 1}}); err != nil {
					return err
				}
				return tx.Link(key, "hub", "hub:0")
			})
		}
	}()
	script := `
import sys
sys.path.insert(0, sys.argv[1])
from hcfile import put, link
for i in range(int(sys.argv[3])):
    key = "py:%d" % i
    put(sys.argv[2], key, {"i": i})
    link(sys.argv[2], key, "hub", "hub:0")
`
	cmd := exec.Command(py, "-I", "-B", "-c", script, filepath.Join("examples", "python"), path, strconv.Itoa(n))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wg.Wait()
	if goErr != nil {
		t.Fatal(goErr)
	}
	links := must[[]Link](t)(db.Neighbours("hub:0", In, "hub"))
	if len(links) != 2*n {
		t.Fatalf("%d links into the hub, want %d", len(links), 2*n)
	}
	rep := checkOK(t, db)
	if rep.Records != 2*n+2 {
		t.Fatalf("%d records, want %d", rep.Records, 2*n+2)
	}
}
