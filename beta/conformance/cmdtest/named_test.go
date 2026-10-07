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

// The table of named tests in beta/SQL.md, one row a test: its name, its
// suite, and whether the Beta must pass it or leaves it out, and why.
var namedRow = regexp.MustCompile("^\\| `([A-Za-z]+)` \\| (conformance|command) \\| (must pass|left out: .+) \\|$")

// TestEveryNamedTestExists holds beta/SQL.md's table of named tests to the
// two suites it names: the conformance suite, as conformance.Tests() lists
// it, and the sections of TestCommands. Every row must name a test that
// exists, and every test must have exactly one row, so a test added to either
// suite has to be named in the spec too.
func TestEveryNamedTestExists(t *testing.T) {
	path := filepath.Join("..", "..", "SQL.md")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	exists := map[string]map[string]bool{"conformance": {}, "command": {}}
	for _, name := range conformance.Tests() {
		exists["conformance"][name] = true
	}
	for _, s := range sections {
		exists["command"][s.name] = true
	}

	rows := map[string]map[string]string{"conformance": {}, "command": {}}
	inTable, line, pass, out := false, 0, 0, 0
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
		m := namedRow.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("%s:%d: a row of the named tests that doesn't read as one: %s", path, line, text)
			continue
		}
		name, suite, verdict := m[1], m[2], m[3]
		switch {
		case !exists[suite][name]:
			t.Errorf("%s:%d names %s in the %s suite, which has no such test", path, line, name, suite)
		case rows[suite][name] != "":
			t.Errorf("%s:%d names %s in the %s suite a second time", path, line, name, suite)
		}
		rows[suite][name] = verdict
		if verdict == "must pass" {
			pass++
		} else {
			out++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if pass+out == 0 {
		t.Fatalf("%s has no table under \"## The named tests\"", path)
	}
	for _, suite := range []string{"conformance", "command"} {
		for name := range exists[suite] {
			if rows[suite][name] == "" {
				t.Errorf("the %s suite's test %s has no row in %s's named tests", suite, name, path)
			}
		}
	}
	t.Logf("%s names %d tests the Beta must pass and %d it leaves out", path, pass, out)
}
