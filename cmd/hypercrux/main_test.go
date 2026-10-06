// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux"
)

// hc runs the command in-process and returns its exit code and output.
func hc(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errs strings.Builder
	code := run(args, strings.NewReader(stdin), &out, &errs)
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

func TestEveryCommand(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notes.db")
	expect(t, 0, "hypercrux "+hypercrux.Version, "version")
	expect(t, 0, "Usage:", "help")
	expect(t, 2, "Usage:", []string{}...)

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
	out = expect(t, 0, `{"key":"docs:1"`, "scan", f, "docs:")
	if strings.Count(out, "\n") != 3 || strings.Contains(out, "vec") {
		t.Fatalf("scan output:\n%s", out)
	}
	out = expect(t, 0, `"vec":[0.9,0.1,0]`, "scan", f, "docs:", "--vec", "--limit", "1")
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("scan --limit 1:\n%s", out)
	}
	expect(t, 0, `{"key":"docs:3"`, "scan", f, "docs:", "--after", "docs:2")

	out = expect(t, 0, "customer:42 -owns-> docs:2", "neighbours", f, "customer:42")
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

	out = expect(t, 0, "docs:1", "nearest", f, "docs", "[1, 0, 0]", "-k", "2")
	if !strings.HasPrefix(out, "0.0061  docs:1\n0.0369  docs:3\n") {
		t.Fatalf("nearest:\n%s", out)
	}
	out = expect(t, 0, "docs:3", "nearest", f, "docs", "docs:1", "--where", "status = 'open'")
	if strings.Contains(out, "docs:1") || strings.Contains(out, "docs:2") {
		t.Fatalf("nearest to a record should leave it out and keep the filter:\n%s", out)
	}
	expect(t, 0, `"key":"docs:1"`, "nearest", f, "docs", "[1,0,0]", "--json", "-k=1")

	out = expect(t, 0, "docs:3  Q3 budget", "sql", f,
		`SELECT d.key, d.title, d.vec FROM json_each(walk('customer:42', 2)) w JOIN docs d ON d.key = w.value
		 WHERE d.status = ? ORDER BY distance(d.vec, ?) LIMIT 10`, "open", "[1,0,0]")
	if !strings.Contains(out, "vector(3)") || !strings.HasPrefix(out, "key     title      vec\ndocs:1") {
		t.Fatalf("sql:\n%s", out)
	}
	expect(t, 0, `{"key":"docs:2","vec":[0.1,0.9,0.1]}`, "sql", f, "SELECT key, vec FROM docs WHERE key = ?", "docs:2", "--json")
	expect(t, 0, "1 rows changed", "sql", f, "UPDATE docs SET status = 'archived' WHERE key = 'docs:2'")
	expect(t, 0, "-1", "sql", f, "SELECT ?", "-1")
	expect(t, 1, "keys in table docs are text that starts with docs:", "sql", f, "INSERT INTO docs (key) VALUES ('x:1')")

	expect(t, 0, "unlinked customer:42 -*-> docs:2", "unlink", f, "customer:42", "*", "docs:2")
	expect(t, 1, "not found", "unlink", f, "customer:42", "*", "docs:2")
	expect(t, 0, "deleted docs:1", "delete", f, "docs:1")
	expect(t, 1, "not found: docs:1", "get", f, "docs:1")
	expect(t, 0, "ok: 2 tables, 3 records, 0 links, 2 vectors", "check", f)

	// SQL-first: create a table, adopt it, use it.
	expect(t, 0, "0 rows changed", "sql", f, "CREATE TABLE notes (key TEXT PRIMARY KEY, body TEXT)")
	expect(t, 0, "1 rows changed", "sql", f, "INSERT INTO notes VALUES ('notes:1', 'hello')")
	expect(t, 0, "notes is a record table with 1 records", "adopt", f, "notes")
	expect(t, 0, "notes:1 -about-> docs:3", "link", f, "notes:1", "about", "docs:3")

	// Mistakes.
	expect(t, 1, "no such file", "get", filepath.Join(t.TempDir(), "typo.db"), "docs:1")
	expect(t, 1, "has no table", "put", f, "nocolon", `{}`)
	expect(t, 1, "must be a JSON object", "put", f, "docs:9", `[1, 2]`)
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

func TestOtherSQLiteFilesAreLeftAlone(t *testing.T) {
	f := filepath.Join(t.TempDir(), "app.db")
	plain, err := sql.Open("sqlite3", f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec(`CREATE TABLE settings (k TEXT, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	expect(t, 1, "isn't a HyperCrux file", "get", f, "docs:1")
	expect(t, 1, "isn't a HyperCrux file", "check", f)
	expect(t, 0, "is ready", "init", f)
	expect(t, 0, "ok: 0 tables", "check", f)
}

func TestCheckReportsProblems(t *testing.T) {
	f := filepath.Join(t.TempDir(), "broken.db")
	expect(t, 0, "docs:1", "put", f, "docs:1", `{"a": 1}`)
	expect(t, 0, "docs:2", "put", f, "docs:2", `{"a": 2}`)
	expect(t, 0, "docs:1 -x-> docs:2", "link", f, "docs:1", "x", "docs:2")
	expect(t, 0, "0 rows changed", "sql", f, "DROP TRIGGER hc_docs_delete")
	expect(t, 0, "1 rows changed", "sql", f, "DELETE FROM docs WHERE key = 'docs:2'")
	out := expect(t, 1, "problems in", "check", f)
	if !strings.Contains(out, "missing its trigger hc_docs_delete") || !strings.Contains(out, "no row in docs: docs:2") {
		t.Fatalf("check output:\n%s", out)
	}
}
