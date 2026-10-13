// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmdtest

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance"
)

// A row of the table of named tests in beta/SQL.md: a test's name, its
// suite, and whether the Beta must pass it or leaves it out, and why.
var namedRowText = regexp.MustCompile("^\\| `([A-Za-z]+)` \\| (conformance|command) \\| (must pass|left out: .+) \\|$")

// sqlPath is beta/SQL.md, from this package's folder.
var sqlPath = filepath.Join("..", "..", "SQL.md")

// namedRows are the rows of beta/SQL.md's table of named tests, one a test,
// in the order they come.
type namedRow struct {
	line                 int
	name, suite, verdict string
}

// readNamedTests reads the rows of beta/SQL.md's table of named tests, and
// reports through t a row that doesn't read as one. It fails t when the
// file has no such table.
func readNamedTests(t *testing.T) []namedRow {
	t.Helper()
	f, err := os.Open(sqlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []namedRow
	inTable, line := false, 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.HasPrefix(text, "## ") {
			inTable = text == "## The named tests"
			continue
		}
		if !inTable || !strings.HasPrefix(text, "| `") {
			continue
		}
		m := namedRowText.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("%s:%d: a row of the named tests that doesn't read as one: %s", sqlPath, line, text)
			continue
		}
		rows = append(rows, namedRow{line: line, name: m[1], suite: m[2], verdict: m[3]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no table under \"## The named tests\"", sqlPath)
	}
	return rows
}

// namedTests is beta/SQL.md's table of named tests, for each suite: each
// test's verdict, "must pass" or "left out: " and the reason.
func namedTests(t *testing.T) map[string]map[string]string {
	t.Helper()
	tests := map[string]map[string]string{"conformance": {}, "command": {}}
	for _, r := range readNamedTests(t) {
		tests[r.suite][r.name] = r.verdict
	}
	return tests
}

// TestEveryNamedTestExists holds beta/SQL.md's table of named tests to the
// two suites it names: the conformance suite, as conformance.Tests() lists
// it, and the sections of TestCommands. Every row must name a test that
// exists, and every test must have exactly one row, so a test added to either
// suite has to be named in the spec too. TestCommands leaves out, for the
// Beta, the sections the table leaves out.
func TestEveryNamedTestExists(t *testing.T) {
	exists := map[string]map[string]bool{"conformance": {}, "command": {}}
	for _, name := range conformance.Tests() {
		exists["conformance"][name] = true
	}
	for _, s := range sections {
		exists["command"][s.name] = true
	}

	rows := map[string]map[string]string{"conformance": {}, "command": {}}
	pass, out := 0, 0
	for _, r := range readNamedTests(t) {
		switch {
		case !exists[r.suite][r.name]:
			t.Errorf("%s:%d names %s in the %s suite, which has no such test", sqlPath, r.line, r.name, r.suite)
		case rows[r.suite][r.name] != "":
			t.Errorf("%s:%d names %s in the %s suite a second time", sqlPath, r.line, r.name, r.suite)
		}
		rows[r.suite][r.name] = r.verdict
		if r.verdict == "must pass" {
			pass++
		} else {
			out++
		}
	}
	for _, suite := range []string{"conformance", "command"} {
		for name := range exists[suite] {
			if rows[suite][name] == "" {
				t.Errorf("the %s suite's test %s has no row in %s's named tests", suite, name, sqlPath)
			}
		}
	}
	t.Logf("%s names %d tests the Beta must pass and %d it leaves out", sqlPath, pass, out)
}
