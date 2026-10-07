// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmdtest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bin is the hypercrux binary under test.
var bin string

func TestMain(m *testing.M) {
	bin = os.Getenv("HYPERCRUX_BIN")
	cleanup := func() {}
	if bin == "" {
		dir, err := os.MkdirTemp("", "hypercrux-cmdtest")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cleanup = func() { os.RemoveAll(dir) }
		bin = filepath.Join(dir, "hypercrux")
		build := exec.Command("go", "build", "-o", bin, "github.com/hypercrux/hypercrux/cmd/hypercrux")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building 0.x's command:", err)
			cleanup()
			os.Exit(1)
		}
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// hc runs the binary and returns its exit code and output.
func hc(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running %s: %v", bin, err)
		}
		code = exit.ExitCode()
	}
	return code, out.String(), errs.String()
}

func expect(t *testing.T, wantCode int, wantOut string, args ...string) string {
	t.Helper()
	code, out, errs := hc(t, "", args...)
	if code != wantCode || !strings.Contains(out+errs, wantOut) {
		t.Fatalf("hypercrux %s\nexit %d (want %d)\nstdout: %s\nstderr: %s\nwanted: %q", strings.Join(args, " "), code, wantCode, out, errs, wantOut)
	}
	return out
}

// sections are the parts of TestCommands, in the order they run.
var sections = []struct {
	name string
	fn   func(t *testing.T, f string)
}{
	{"VersionAndHelp", versionAndHelp},
	{"PutGetAndLink", putGetAndLink},
	{"Scan", scan},
	{"NeighboursAndWalk", neighboursAndWalk},
	{"Nearest", nearest},
	{"SQL", sqlStatements},
	{"SQLTriggers", sqlTriggers},
	{"UnlinkDeleteAndCheck", unlinkDeleteAndCheck},
	{"AdoptAndDrop", adoptAndDrop},
	{"ExportAndImport", exportAndImport},
	{"Mistakes", mistakes},
}

// TestCommands is 0.x's TestEveryCommand in sections, run in order on one
// file. Sections leave the file as later ones expect, so any of them can be
// skipped without breaking the rest.
func TestCommands(t *testing.T) {
	skip := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("HYPERCRUX_CMD_SKIP"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			skip[s] = true
		}
	}
	f := filepath.Join(t.TempDir(), "notes.db")
	known := map[string]bool{}
	for _, s := range sections {
		known[s.name] = true
	}
	for name := range skip {
		if !known[name] {
			t.Errorf("HYPERCRUX_CMD_SKIP names %q, which isn't a section", name)
		}
	}
	t.Logf("binary: %s", bin)
	for _, s := range sections {
		t.Run(s.name, func(t *testing.T) {
			if skip[s.name] {
				t.Skip("skipped by HYPERCRUX_CMD_SKIP")
			}
			s.fn(t, f)
		})
	}
}

func versionAndHelp(t *testing.T, f string) {
	expect(t, 0, "hypercrux ", "version")
	expect(t, 0, "Usage:", "help")
	expect(t, 2, "Usage:", []string{}...)
}

func putGetAndLink(t *testing.T, f string) {
	expect(t, 0, "customer:42", "put", f, "customer:42", `{"name": "Dana"}`)
	expect(t, 0, "docs:1", "put", f, "docs:1", `{"title": "Q3 plan", "status": "open", "vec": [0.9, 0.1, 0]}`)
	expect(t, 0, "docs:2", "put", f, "docs:2", `{"title": "Hiring notes", "status": "done", "vec": [0.1, 0.9, 0.1]}`)
	if code, out, errs := hc(t, `{"title": "Q3 budget", "status": "open", "vec": [0.8, 0.2, 0.1], "meta": {"pages": 3}}`, "put", f, "docs:3"); code != 0 {
		t.Fatalf("put from stdin: %d %s %s", code, out, errs)
	}
	expect(t, 0, "customer:42 -owns-> docs:1", "link", f, "customer:42", "owns", "docs:1")
	expect(t, 0, "customer:42 -owns-> docs:2", "link", f, "customer:42", "owns", "docs:2")
	expect(t, 0, "docs:1 -cites-> docs:3", "link", f, "docs:1", "cites", "docs:3")

	out := expect(t, 0, `"title": "Q3 budget"`, "get", f, "docs:3")
	for _, want := range []string{`"key": "docs:3"`, `"vec": [0.8,0.2,0.1]`, `"meta": "{\"pages\":3}"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("get output lacks %s:\n%s", want, out)
		}
	}
}

func scan(t *testing.T, f string) {
	out := expect(t, 0, `{"key":"docs:1"`, "scan", f, "docs:")
	if strings.Count(out, "\n") != 3 || strings.Contains(out, "vec") {
		t.Fatalf("scan output:\n%s", out)
	}
	out = expect(t, 0, `"vec":[0.9,0.1,0]`, "scan", f, "docs:", "--vec", "--limit", "1")
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("scan --limit 1:\n%s", out)
	}
	expect(t, 0, `{"key":"docs:3"`, "scan", f, "docs:", "--after", "docs:2")
}

func neighboursAndWalk(t *testing.T, f string) {
	out := expect(t, 0, "customer:42 -owns-> docs:2", "neighbours", f, "customer:42")
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("neighbours:\n%s", out)
	}
	expect(t, 0, `{"from":"customer:42","to":"docs:1","type":"owns"}`, "neighbours", f, "docs:1", "--in", "--json")
	out = expect(t, 0, "2  docs:3", "walk", f, "customer:42", "2")
	if !strings.HasPrefix(out, "1  docs:1\n1  docs:2\n") {
		t.Fatalf("walk:\n%s", out)
	}
	expect(t, 0, `{"depth":1,"key":"customer:42"}`, "walk", f, "docs:1", "1", "--in", "--json")
	expect(t, 0, "1  docs:1", "walk", f, "docs:3", "1", "--both", "--type", "cites")
}

func nearest(t *testing.T, f string) {
	out := expect(t, 0, "docs:1", "nearest", f, "docs", "[1, 0, 0]", "-k", "2")
	if !strings.HasPrefix(out, "0.0061  docs:1\n0.0369  docs:3\n") {
		t.Fatalf("nearest:\n%s", out)
	}
	out = expect(t, 0, "docs:3", "nearest", f, "docs", "docs:1", "--where", "status = 'open'")
	if strings.Contains(out, "docs:1") || strings.Contains(out, "docs:2") {
		t.Fatalf("nearest to a record should leave it out and keep the filter:\n%s", out)
	}
	expect(t, 0, `"key":"docs:1"`, "nearest", f, "docs", "[1,0,0]", "--json", "-k=1")
}

func sqlStatements(t *testing.T, f string) {
	out := expect(t, 0, "docs:3  Q3 budget", "sql", f,
		`SELECT d.key, d.title, d.vec FROM json_each(walk('customer:42', 2)) w JOIN docs d ON d.key = w.value
		 WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT 10`, "open", "[1,0,0]")
	if !strings.Contains(out, "vector(3)") || !strings.HasPrefix(out, "key     title      vec\ndocs:1") {
		t.Fatalf("sql:\n%s", out)
	}
	expect(t, 0, `{"key":"docs:2","vec":[0.1,0.9,0.1]}`, "sql", "--json", f, "SELECT key, vec FROM docs WHERE key = ?", "docs:2")
	expect(t, 0, "1 rows changed", "sql", f, "UPDATE docs SET status = 'archived' WHERE key = 'docs:2'")
	expect(t, 0, "0 rows changed", "sql", f, "UPDATE docs SET status = 'x' WHERE key = 'docs:404'")
	expect(t, 0, "-1", "sql", f, "SELECT ?", "-1")
	// After the statement, everything is an argument, dashes and all.
	expect(t, 0, "-draft-", "sql", f, "SELECT ?", "-draft-")
	expect(t, 0, "--json", "sql", f, "SELECT ?", "--json")
	// Numbers that wouldn't read back the same stay text.
	expect(t, 0, "docs:3", "put", f, "docs:3", `{"phone": null}`)
	expect(t, 0, "1 rows changed", "sql", f, "UPDATE docs SET phone = ? WHERE key = 'docs:3'", "0501234567")
	expect(t, 0, "docs:3", "sql", f, "SELECT key FROM docs WHERE phone = ?", "0501234567")
	// Comments are fine, more than one statement isn't.
	expect(t, 0, "docs:3", "sql", f, "-- the budget\nSELECT key FROM docs WHERE phone = '0501234567'")
	expect(t, 0, "docs:3", "sql", f, "/* the budget */ SELECT key FROM docs WHERE phone = '0501234567'; -- done")
	expect(t, 2, "one statement at a time", "sql", f, "SELECT 1; DROP TABLE docs")
	expect(t, 2, "one statement at a time", "sql", f, "SELECT 'a;b'; SELECT 2")
	expect(t, 0, "a;b", "sql", f, "SELECT 'a;b' AS x")
	expect(t, 2, "needs a statement", "sql", f, " -- nothing")
	expect(t, 1, "keys in table docs are text that starts with docs:", "sql", f, "INSERT INTO docs (key) VALUES ('x:1')")
}

// Triggers are SQLite's; the statement holding semicolons must still count
// as one.
func sqlTriggers(t *testing.T, f string) {
	expect(t, 0, "0 rows changed", "sql", f, "CREATE TRIGGER t1 AFTER INSERT ON docs BEGIN SELECT 1; SELECT 2; END")
	expect(t, 0, "0 rows changed", "sql", f, "DROP TRIGGER t1")
	expect(t, 0, "docs:1", "get", f, "docs:1") // the table survived
}

func unlinkDeleteAndCheck(t *testing.T, f string) {
	expect(t, 0, "unlinked customer:42 -*-> docs:2", "unlink", f, "customer:42", "*", "docs:2")
	expect(t, 1, "not found", "unlink", f, "customer:42", "*", "docs:2")
	expect(t, 0, "deleted docs:1", "delete", f, "docs:1")
	expect(t, 1, "not found: docs:1", "get", f, "docs:1")
	expect(t, 0, "ok: 2 tables, 3 records, 0 links, 2 vectors", "check", f)
}

// SQL first: create a table with plain SQL, adopt it, use it. 0.x only.
func adoptAndDrop(t *testing.T, f string) {
	expect(t, 0, "0 rows changed", "sql", f, "CREATE TABLE notes (key TEXT PRIMARY KEY, body TEXT)")
	expect(t, 0, "1 rows changed", "sql", f, "INSERT INTO notes VALUES ('notes:1', 'hello')")
	expect(t, 0, "notes is a record table with 1 records", "adopt", f, "notes")
	expect(t, 0, "notes:1 -about-> docs:3", "link", f, "notes:1", "about", "docs:3")
	expect(t, 0, "dropped notes", "drop", f, "notes")
	expect(t, 0, "", "neighbours", f, "docs:3")
	expect(t, 1, "no record table notes", "drop", f, "notes")
}

// Export, import into a new file, and export again: the two exports match
// byte for byte. Added with 0.x's export and import, in 0.2.0.
func exportAndImport(t *testing.T, f string) {
	code, first, errs := hc(t, "", "export", f)
	if code != 0 || !strings.HasPrefix(first, `{"hypercrux":"export","version":1}`+"\n") || !strings.Contains(first, `{"end":`) {
		t.Fatalf("export: exit %d\n%s\n%s", code, first, errs)
	}
	dir := t.TempDir()
	copied := filepath.Join(dir, "copy.db")
	if code, out, errs := hc(t, first, "import", copied); code != 0 || !strings.Contains(out, "imported ") {
		t.Fatalf("import: exit %d\n%s\n%s", code, out, errs)
	}
	if code, second, errs := hc(t, "", "export", copied); code != 0 || second != first {
		t.Fatalf("the second export differs: exit %d\n%s\n---\n%s\n%s", code, first, second, errs)
	}
	expect(t, 0, "ok:", "check", copied)
	if code, out, errs := hc(t, first, "import", f); code != 1 || !strings.Contains(out+errs, "already holds record tables") {
		t.Fatalf("import into a file with records: exit %d\n%s\n%s", code, out, errs)
	}
	bad := filepath.Join(dir, "bad.db")
	if code, out, errs := hc(t, first[:len(first)/2], "import", bad); code != 1 || !strings.Contains(out+errs, "not a valid HyperCrux export") {
		t.Fatalf("import of half an export: exit %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatalf("a failed import left %s behind: %v", bad, err)
	}
	expect(t, 1, "no such file", "export", filepath.Join(dir, "typo.db"))
}

func mistakes(t *testing.T, f string) {
	expect(t, 1, "no such file", "get", filepath.Join(t.TempDir(), "typo.db"), "docs:1")
	expect(t, 1, "has no table", "put", f, "nocolon", `{}`)
	expect(t, 1, "must be a JSON object", "put", f, "docs:9", `[1, 2]`)
	expect(t, 1, "nothing after it", "put", f, "docs:9", `{"a": 1} {"b": 2}`)
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	expect(t, 1, "only zeros", "put", fresh, "docs:1", `{"vec": [0, 0]}`)
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("a failed put left %s behind: %v", fresh, err)
	}
	expect(t, 1, "JSON has key", "put", f, "docs:9", `{"key": "docs:8"}`)
	expect(t, 1, "holds vectors of 3 values", "put", f, "docs:9", `{"vec": [1, 2]}`)
	expect(t, 1, "not found: docs:404", "link", f, "docs:3", "x", "docs:404")
	expect(t, 2, "unknown command", "frobnicate", f)
	expect(t, 2, "unknown option", "get", f, "docs:3", "--colour")
	expect(t, 2, "expected FILE KEY", "get", f)
	expect(t, 2, "DEPTH is a whole number", "walk", f, "docs:3", "two")
	expect(t, 1, "depth is from 1 to 32", "walk", f, "docs:3", "40")
	expect(t, 2, "-k takes a whole number", "nearest", f, "docs", "[1,0,0]", "-k", "0")
	expect(t, 1, "has no vector", "nearest", f, "docs", "customer:42")
	expect(t, 1, "a vector is a JSON array", "nearest", f, "docs", "[1, oops]")
}
