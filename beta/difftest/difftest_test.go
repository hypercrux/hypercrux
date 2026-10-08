// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package difftest

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/betax"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
)

// scratch is a folder for the databases. /dev/shm keeps thousands of
// short-lived files off the disk; surviving a crash is for the file tasks'
// own tests, not this one.
func scratch(t *testing.T) string {
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		if dir, err := os.MkdirTemp("/dev/shm", "hypercrux-difftest-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(dir) })
			return dir
		}
	}
	return t.TempDir()
}

// sequences is how many to run: HYPERCRUX_DIFF_SEQUENCES when it's set, for
// long runs, and otherwise fewer in short mode.
func sequences(t *testing.T) int {
	if s := os.Getenv("HYPERCRUX_DIFF_SEQUENCES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("HYPERCRUX_DIFF_SEQUENCES is %q, not a whole number from 1", s)
		}
		return n
	}
	if testing.Short() {
		return 300
	}
	return 2000
}

// check runs Find, leaving out the kinds of step in leave, and on a
// failure saves the shrunk sequence into testdata, where TestSavedSequences
// replays it from then on.
func check(t *testing.T, a, b c.Engine, seed uint64, leave ...string) {
	t.Helper()
	n := sequences(t)
	f, err := Find(a, b, Options{Sequences: n, MaxLength: 80, Seed: seed, Dir: scratch(t), Leave: leave})
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		path, err := Save("testdata", f)
		if err != nil {
			t.Logf("couldn't save it: %v", err)
		}
		t.Fatalf("%s\nsaved as %s", f, path)
	}
	t.Logf("%s and %s agreed on %d random sequences", a.Name(), b.Name(), n)
}

func TestZeroxAgainstItself(t *testing.T) {
	check(t, zerox.Engine{}, zerox.Engine{}, 1)
}

// betaLeaves are the kinds of step the Beta can't take yet: SQL, and
// Nearest's filters, which are SQL too. Both wait for task G4.
var betaLeaves = []string{"sql", "where"}

// TestZeroxAgainstTheBeta runs 0.x against the Beta, 0.x being the judge,
// on sequences without the steps in betaLeaves.
func TestZeroxAgainstTheBeta(t *testing.T) {
	check(t, zerox.Engine{}, betax.Engine{}, 1, betaLeaves...)
}

// TestSavedSequences replays every sequence saved in testdata, with 0.x
// against the Beta. A saved one stays as a test after the difference it
// found is fixed. A sequence with a step the Beta can't take yet replays
// with 0.x against itself until task G4.
func TestSavedSequences(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := scratch(t)
	waiting := 0
	for _, path := range files {
		f, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		var b c.Engine = betax.Engine{}
		if takes(f.Ops, betaLeaves) {
			b = zerox.Engine{}
			waiting++
		}
		m, err := Replay(zerox.Engine{}, b, f.Ops, dir)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if m != nil {
			t.Errorf("%s still fails, with 0.x against %s:\n%v", path, b.Name(), m)
		}
	}
	t.Logf("replayed %d saved sequences, %d of them with 0.x against itself until task G4", len(files), waiting)
}

// takes reports whether a sequence has a step of one of the kinds named,
// counting the steps inside an Update and "where" for Nearest's filters,
// as Generate leaves them out.
func takes(ops []Op, kinds []string) bool {
	for _, op := range ops {
		for _, k := range kinds {
			if op.Kind == k || k == "where" && op.Where != "" {
				return true
			}
		}
		if takes(op.Ops, kinds) {
			return true
		}
	}
	return false
}

// TestABrokenEngineIsCaught runs 0.x against copies of itself with one
// thing each done wrong. The harness has to catch every one, and shrink
// what it found to a short sequence that still shows the difference.
func TestABrokenEngineIsCaught(t *testing.T) {
	for _, how := range breakages {
		t.Run(how, func(t *testing.T) {
			bad := broken{how: how}
			dir := scratch(t)
			const budget = 300
			f, err := Find(zerox.Engine{}, bad, Options{Sequences: budget, MaxLength: 80, Seed: 1000, Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if f == nil {
				t.Fatalf("not caught in %d sequences", budget)
			}
			if len(f.Ops) > 10 {
				t.Errorf("shrunk only to %d steps:\n%s", len(f.Ops), f)
			}
			path, err := Save(t.TempDir(), f)
			if err != nil {
				t.Fatal(err)
			}
			g, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			m, err := Replay(zerox.Engine{}, bad, g.Ops, dir)
			if err != nil || m == nil {
				t.Fatalf("saved and loaded, the sequence no longer fails: %v", err)
			}
			again, err := Replay(zerox.Engine{}, zerox.Engine{}, g.Ops, dir)
			if err != nil || again != nil {
				t.Fatalf("the shrunk sequence fails on 0.x against itself too: %v %v", err, again)
			}
			t.Logf("caught at seed %d, shrunk to %d steps: %s", f.Seed, len(f.Ops), m.Detail)
		})
	}
}

func TestValuesComeBackExactly(t *testing.T) {
	for _, v := range []any{nil, true, int64(math.MinInt64), math.Copysign(0, -1), math.NaN(), math.Inf(-1),
		math.SmallestNonzeroFloat64, "", "é😀\x00", "\xff\xfe", []byte{}, []byte{0, 255},
		c.Vector{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, float32(math.NaN())}} {
		b, err := json.Marshal(Value{v})
		if err != nil {
			t.Fatal(err)
		}
		var back Value
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if show(back.V) != show(v) || reflect.TypeOf(back.V) != reflect.TypeOf(v) {
			t.Errorf("%s came back as %s, from %s", show(v), show(back.V), b)
		}
	}
}

func TestKindsCanBeLeftOut(t *testing.T) {
	for _, op := range Generate(3, 2000, "sql", "where", "update") {
		if op.Kind == "sql" || op.Kind == "update" || op.Where != "" {
			t.Fatalf("a step that should have been left out: %s", opJSON(op))
		}
	}
	seen := map[string]bool{}
	for _, op := range Generate(3, 2000) {
		seen[op.Kind] = true
		seen["where"] = seen["where"] || op.Where != ""
	}
	for _, k := range Kinds {
		if !seen[k] {
			t.Errorf("2,000 steps had no %s", k)
		}
	}
}

func TestSequencesAreRepeatable(t *testing.T) {
	a, _ := json.Marshal(Generate(7, 200))
	b, _ := json.Marshal(Generate(7, 200))
	if string(a) != string(b) {
		t.Fatal("the same seed gave two sequences")
	}
	var ops []Op
	if err := json.Unmarshal(a, &ops); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(ops); string(again) != string(a) {
		t.Fatal("a sequence changes when saved and loaded")
	}
}
