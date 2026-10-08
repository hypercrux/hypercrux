// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	beta "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// These tests keep the benchmarks a mirror of 0.1's without running any of
// them, so they take no time in go test ./...

// opening is the one benchmark here that 0.1 hasn't got.
const opening = "BenchmarkOpen_100k_384dims"

// benchmarks gives the names of the benchmarks declared in the Go files
// that match pattern, with the statements of each one's body.
func benchmarks(t *testing.T, pattern string) map[string][]ast.Stmt {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files match %s: %v", pattern, err)
	}
	found := map[string][]ast.Stmt{}
	for _, name := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Benchmark") {
				found[fn.Name.Name] = fn.Body.List
			}
		}
	}
	return found
}

// TestEveryBenchmarkRunsOnBoth checks that every one of 0.1's benchmarks
// has one here by the same name, with only the opening one beside them,
// and that each is a call to both, which runs it on 0.x and on the Beta, or
// skips it on the Beta when later names it. Every name in later and in
// Targets has to be one of them.
func TestEveryBenchmarkRunsOnBoth(t *testing.T) {
	ours := benchmarks(t, "*_test.go")
	theirs := benchmarks(t, filepath.Join("..", "..", "bench_test.go"))
	for name := range theirs {
		if _, ok := ours[name]; !ok {
			t.Errorf("0.1's %s has no benchmark here", name)
		}
	}
	for name, body := range ours {
		if _, ok := theirs[name]; !ok && name != opening {
			t.Errorf("%s isn't one of 0.1's benchmarks", name)
		}
		if !callsBoth(body) {
			t.Errorf("%s isn't a call to both, so it may not run on both engines", name)
		}
	}
	for name, w := range later {
		if _, ok := ours["Benchmark"+name]; !ok {
			t.Errorf("later names %s, which isn't a benchmark here", name)
		}
		if !regexp.MustCompile(`^[A-Z][0-9]$`).MatchString(w.task) || len(w.calls) == 0 {
			t.Errorf("later's entry for %s names task %q and calls %q", name, w.task, w.calls)
		}
		for _, c := range w.calls {
			if _, ok := probes[c]; !ok {
				t.Errorf("later's entry for %s names %q, which TestTheSkipsStillWait can't try", name, c)
			}
		}
	}
	for name := range Targets {
		if _, ok := ours["Benchmark"+name]; !ok {
			t.Errorf("Targets names %s, which isn't a benchmark here", name)
		}
	}
}

// callsBoth reports whether body is the one statement both(b, ...).
func callsBoth(body []ast.Stmt) bool {
	if len(body) != 1 {
		return false
	}
	e, ok := body[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	c, ok := e.X.(*ast.CallExpr)
	if !ok || len(c.Args) != 2 {
		return false
	}
	f, ok := c.Fun.(*ast.Ident)
	return ok && f.Name == "both"
}

// probes are the Beta's calls that later's entries wait for, each made on
// a database holding docs:1 and docs:2, with vectors.
var probes = map[string]func(db *beta.DB) error{
	"Link": func(db *beta.DB) error { return db.Link("docs:1", "to", "docs:2") },
	"Walk": func(db *beta.DB) error {
		_, err := db.Walk("docs:1", beta.Out, "", 1)
		return err
	},
	"Nearest": func(db *beta.DB) error {
		_, err := db.Nearest("docs", beta.Vector{1, 0}, 1, "")
		return err
	},
	"Nearest with a filter": func(db *beta.DB) error {
		_, err := db.Nearest("docs", beta.Vector{1, 0}, 1, "grp = ?", 3)
		return err
	},
}

// TestTheSkipsStillWait tries each call later says a benchmark waits for,
// on the Beta. Each entry needs at least one of its calls to fail as a stub
// does, naming the entry's task, and none naming another. Once the task
// makes its calls work, this fails, so the task takes the entry out of
// later and the benchmark runs on the Beta.
func TestTheSkipsStillWait(t *testing.T) {
	db, err := beta.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *beta.Tx) error {
		if err := tx.Put("docs:1", beta.Fields{"grp": 3, "vec": beta.Vector{1, 0}}); err != nil {
			return err
		}
		return tx.Put("docs:2", beta.Fields{"grp": 4, "vec": beta.Vector{0, 1}})
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := regexp.MustCompile(`until task ([A-Z][0-9])$`)
	waitsFor := map[string]string{} // the task each call's stub names, or ""
	errs := map[string]error{}
	for c, probe := range probes {
		err := probe(db)
		errs[c] = err
		if m := stub.FindStringSubmatch(errString(err)); m != nil && errors.Is(err, errors.ErrUnsupported) {
			waitsFor[c] = m[1]
		}
	}
	for name, w := range later {
		waiting := false
		for _, c := range w.calls {
			switch waitsFor[c] {
			case w.task:
				waiting = true
			case "":
			default:
				t.Errorf("later has %s on the Beta waiting for task %s, but %s waits for task %s", name, w.task, c, waitsFor[c])
			}
		}
		if !waiting {
			for _, c := range w.calls {
				t.Logf("%s on the Beta gave %v", c, errs[c])
			}
			t.Errorf("%s on the Beta no longer waits for task %s: take it out of later in bench_test.go, so it runs", name, w.task)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// targetRows gives the benchmarks that time each row of BETA.md's Targets
// table, by the row's first cell. A row with two targets, such as Walk's,
// names a benchmark for each.
var targetRows = map[string][]string{
	"Nearest 10, 100,000 vectors of 384 values":                                  {"Nearest_100k_384dims"},
	"Nearest 10, 100,000 of 384, a tenth passing a filter on an unindexed field": {"Nearest_100k_384dims_tenthFiltered"},
	"Nearest 10, 10,000 vectors of 1,536 values":                                 {"Nearest_10k_1536dims"},
	"Get by key":                    {"Get"},
	"Walk 1 link out / 3 links out": {"Walk_100k_depth1", "Walk_100k_depth3"},
	"Put with a 384-value vector, 1,000 per transaction":                  {"PutBatch_384dims"},
	"Put one record, committed":                                           {"Put"},
	"Open a compacted database of 100,000 records with 384-value vectors": {"Open_100k_384dims"},
}

// TestTheTargetsAreBETAs reads the Targets table in BETA.md and checks that
// Targets holds each of its targets, and nothing else.
func TestTheTargetsAreBETAs(t *testing.T) {
	plan, err := os.ReadFile(filepath.Join("..", "..", "BETA.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(plan), "\n## Targets\n")
	if !ok {
		t.Fatal("BETA.md has no Targets section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	seen := map[string]bool{}
	rows := 0
	for line := range strings.Lines(section) {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 4 || cells[0] == "Operation" || cells[0] == "---" {
			continue
		}
		rows++
		names, ok := targetRows[cells[0]]
		if !ok {
			t.Errorf("BETA.md has a target for %q, which targetRows doesn't give a benchmark", cells[0])
			continue
		}
		parts := strings.Split(cells[2], " / ")
		if len(parts) != len(names) {
			t.Errorf("BETA.md gives %q for %q, and targetRows names %q", cells[2], cells[0], names)
			continue
		}
		for i, part := range parts {
			want := time.Duration(0)
			if part != "no slower than 0.1" {
				want, err = time.ParseDuration(strings.ReplaceAll(part, " ", ""))
				if err != nil {
					t.Errorf("BETA.md's target %q for %q: %v", part, cells[0], err)
					continue
				}
			}
			if got, ok := Targets[names[i]]; !ok || got != want {
				t.Errorf("Targets gives %s %v, and BETA.md %q", names[i], got, part)
			}
			seen[names[i]] = true
		}
	}
	if rows != len(targetRows) {
		t.Errorf("BETA.md's Targets table has %d rows, and targetRows %d", rows, len(targetRows))
	}
	for name := range Targets {
		if !seen[name] {
			t.Errorf("Targets gives %s a target that BETA.md doesn't", name)
		}
	}
}
