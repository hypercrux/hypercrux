// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// The command's own tests, in process, of what the Beta's command does that
// 0.x's doesn't: compact, check's sizes and the damage that stops a database
// opening, init on an empty file, and SQL through Exec and Query. The tests
// of 0.x's command, which the Beta's passes too, are in
// beta/conformance/cmdtest, run from outside against both binaries.

// hc runs the command in process and returns its exit code and output.
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

// generation is the generation of the database file at path.
func generation(t *testing.T, path string) uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.DecodeHeader(b, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return h.Gen
}

// TestVersionAndUsage: the version line gives the package's version and the
// file format's, and no SQLite, which the Beta hasn't got. The usage lists
// compact and leaves adopt out, and adopt is an unknown command, exit 2.
func TestVersionAndUsage(t *testing.T) {
	want := "hypercrux " + hypercrux.Version + ", file format " + strconv.Itoa(hypercrux.FormatVersion) + "\n"
	if out := expect(t, 0, "", "version"); out != want {
		t.Errorf("version prints %q, where %q is wanted", out, want)
	}
	out := expect(t, 0, "Usage:", "help")
	if !strings.Contains(out, "hypercrux compact FILE") || strings.Contains(out, "adopt") {
		t.Errorf("the usage:\n%s", out)
	}
	expect(t, 2, `unknown command "adopt"`, "adopt", filepath.Join(t.TempDir(), "x.db"), "notes")
}

// TestCompact: compact rewrites the file with only the live data, says how
// large the file was and is, and leaves every record as it was, in a file of
// the next generation. A database with a second name, made by a hard link,
// is refused as Open refuses it, since a compaction replaces one name only
// (F7.md). A missing file, or a call without a file or with one argument too
// many, is refused as the other verbs refuse them.
func TestCompact(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notes.db")
	expect(t, 0, "docs:1", "put", f, "docs:1", `{"title": "Q3 plan", "vec": [0.9, 0.1, 0]}`)
	for i := range 20 {
		expect(t, 0, "docs:2", "put", f, "docs:2", `{"n": `+strconv.Itoa(i)+`, "text": "`+strings.Repeat("x", 500)+`"}`)
	}
	expect(t, 0, "docs:1 -cites-> docs:2", "link", f, "docs:1", "cites", "docs:2")
	before := expect(t, 0, "", "export", f)
	out := expect(t, 0, "compacted "+f+" from ", "compact", f)
	m := regexp.MustCompile(`from ([0-9,]+) bytes to ([0-9,]+) bytes\n$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("compact prints %q", out)
	}
	from, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	to, _ := strconv.Atoi(strings.ReplaceAll(m[2], ",", ""))
	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if to >= from || int64(to) != info.Size() || generation(t, f) != 2 {
		t.Errorf("compact went from %d bytes to %d, and the file is %d bytes, of generation %d", from, to, info.Size(), generation(t, f))
	}
	if after := expect(t, 0, "", "export", f); after != before {
		t.Errorf("after compact the export is\n%s\nwhere it was\n%s", after, before)
	}
	expect(t, 0, "ok: 1 table, 2 records, 1 link, 1 vector", "check", f)
	second := f + ".second"
	if err := os.Link(f, second); err != nil {
		t.Fatal(err)
	}
	expect(t, 1, "has 2 names, made by hard links", "compact", f)
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	expect(t, 0, "compacted "+f, "compact", f)
	expect(t, 1, "no such file", "compact", filepath.Join(t.TempDir(), "typo.db"))
	expect(t, 2, "expected FILE after compact", "compact")
	expect(t, 2, "expected FILE", "compact", f, "extra")
}

// TestCheck: check prints 0.x's line, then the sizes on a line of their own.
// Damage that stops the database opening is a problem in the file, in
// Check's words, and exits with 1, as problems Check finds do.
func TestCheck(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notes.db")
	expect(t, 0, "docs:1", "put", f, "docs:1", `{"n": 1}`)
	expect(t, 0, "docs:2", "put", f, "docs:2", `{"n": 2}`)
	expect(t, 0, "people:1", "put", f, "people:1", `{"name": "Ann"}`)
	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	out := expect(t, 0, "ok: 2 tables, 3 records, 0 links, 0 vectors\n", "check", f)
	sizes := regexp.MustCompile(`\nsize: ([0-9,]+) bytes in the file, about ([0-9,]+) bytes in memory\n$`).FindStringSubmatch(out)
	if sizes == nil || strings.ReplaceAll(sizes[1], ",", "") != strconv.FormatInt(info.Size(), 10) || sizes[2] == "0" {
		t.Errorf("check prints\n%s\nfor a file of %d bytes", out, info.Size())
	}

	// A bit changed in batch 2's changes, which the next Open finds.
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	n, err := format.BatchLength(b[format.HeaderSize:], 1, 1, int64(len(b)-format.HeaderSize))
	if err != nil {
		t.Fatal(err)
	}
	second := format.HeaderSize + n + format.MarkerSize
	b[second+30] ^= 0x10
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out = expect(t, 1, "problems in "+f+":\n  - the file is damaged: batch 2 at offset "+strconv.FormatInt(second, 10)+": ", "check", f)
	if strings.Count(out, "\n") != 2 {
		t.Errorf("check of a damaged file prints\n%s", out)
	}
	expect(t, 1, "the file is damaged", "get", f, "docs:1")
}

// TestInit: init makes a new database, or a database of an empty file, as a
// writer does, and opens one that's there. A file that isn't a database is
// refused, and left as it was.
func TestInit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "new.db")
	expect(t, 0, f+" is ready (HyperCrux file format 1)", "init", f)
	expect(t, 0, f+" is ready", "init", f)
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, 0, empty+" is ready", "init", empty)
	if info, err := os.Stat(empty); err != nil || info.Size() != format.HeaderSize {
		t.Errorf("init left the empty file as %v, %v", info, err)
	}
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("some notes, which aren't a database\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, 1, "not a HyperCrux database", "init", other)
	if b, err := os.ReadFile(other); err != nil || string(b) != "some notes, which aren't a database\n" {
		t.Errorf("init changed a file that isn't a database: %q, %v", b, err)
	}
}

// TestSQL: an INSERT, an UPDATE or a DELETE goes through Exec, which gives
// the count of rows it changed, whatever comments come before it, and a
// SELECT through Query. A statement outside the Beta's SQL, and a write that
// breaks a rule, fail with exit 1, and more than one statement with exit 2,
// as in 0.x.
func TestSQL(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notes.db")
	expect(t, 0, "docs:1", "put", f, "docs:1", `{"title": "Q3 plan", "status": "open"}`)
	expect(t, 0, "1 rows changed", "sql", f, "INSERT INTO docs (key, title) VALUES (?, ?)", "docs:2", "Hiring")
	expect(t, 0, "2 rows changed", "sql", f, "-- every one\n/* of them */ update docs SET status = 'done'")
	expect(t, 0, "0 rows changed", "sql", f, "DELETE FROM docs WHERE key = 'docs:404'")
	out := expect(t, 0, "", "sql", f, "SELECT key, title, status FROM docs ORDER BY key")
	if out != "key     title    status\ndocs:1  Q3 plan  done\ndocs:2  Hiring   done\n" {
		t.Errorf("a SELECT prints\n%s", out)
	}
	expect(t, 0, `{"n":2}`, "sql", "--json", f, "SELECT count(*) AS n FROM docs")
	expect(t, 1, "outside the SQL subset", "sql", f, "CREATE TABLE notes (key TEXT PRIMARY KEY)")
	expect(t, 1, "keys in table docs are text that starts with docs:", "sql", f, "INSERT INTO docs (key) VALUES ('x:1')")
	expect(t, 2, "one statement at a time", "sql", f, "SELECT 1; DELETE FROM docs")
	expect(t, 0, "2 rows changed", "sql", f, "DELETE FROM docs; -- all of it")
}

// TestAFailedPutLeavesNothing: a put that fails on a file it created
// removes the new database, and a failure exits with 1.
func TestAFailedPutLeavesNothing(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	expect(t, 1, "only zeros", "put", fresh, "docs:1", `{"vec": [0, 0]}`)
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Errorf("a failed put left %s behind: %v", fresh, err)
	}
	expect(t, 1, "no such file", "get", filepath.Join(t.TempDir(), "x.db"), "docs:1")
}

// TestTheCommandBuildsOnEveryOS: go build ./... works on macOS and Windows,
// where the rest of beta/ builds to empty packages and the command to one
// that says it runs on Linux only (G5.md's note for R2). On Linux the
// command is main.go's.
func TestTheCommandBuildsOnEveryOS(t *testing.T) {
	if command == nil {
		t.Fatal("a Linux build has no command for main to run")
	}
	if testing.Short() {
		t.Skip("building for two other systems takes a few seconds the first time")
	}
	for _, target := range []string{"darwin/arm64", "windows/amd64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "hypercrux"), ".")
		build.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Errorf("go build for %s: %v\n%s", target, err, out)
		}
	}
}
