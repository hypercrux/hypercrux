// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Export and import (task G6), from 0.x's TestExportImportRoundTrip,
// TestExportKeepsWhatTablesRemember and TestImportRefuses, the parts that
// don't look inside SQLite. RoundTrip runs on one engine and Across on two.
// They aren't among Tests yet, since beta/SQL.md's named tests have to list
// every test there, so each adapter's tests call them.

// emptyExport is what a database with nothing in it exports as.
const emptyExport = `{"hypercrux":"export","version":1}` + "\n" + `{"end":{"tables":0,"records":0,"links":0}}` + "\n"

// RoundTrip sends a database with awkward values through Export, Import
// into a new file and Export again, on one engine. The two exports are the
// same bytes, the new database reads back the same key by key, with each
// record's links both ways, and Check counts the same. A table keeps its
// vector size with no vectors left. Import refuses a database that holds
// record tables, even empty ones, an export cut short, and a record or a
// link there twice, each with the engine's ErrInvalid, and keeps nothing
// then. A database with nothing in it exports as two lines.
func RoundTrip(t *testing.T, e Engine) {
	t.Logf("engine: %s", e.Name())
	empty, _ := openTemp(t, e)
	if got := exportString(t, empty); got != emptyExport {
		t.Fatalf("an empty database exports as\n%s", got)
	}
	a, _ := openTemp(t, e)
	keys := fill(t, e, a, 1)
	first := exportString(t, a)
	b, _ := openTemp(t, e)
	ok(t, b.Import(strings.NewReader(first)))
	if second := exportString(t, b); second != first {
		t.Fatalf("the second export differs:\n%s\n---\n%s", first, second)
	}
	sameIn(t, a, b, keys)

	// The vector size of a table with no vectors left came through.
	wantErr(t, b.Put("sized:2", Fields{"vec": Vector{1, 2}}), e.ErrInvalid())
	ok(t, b.Put("sized:2", Fields{"vec": Vector{1, 2, 3, 4}}))

	refused := func(db DB, in, want string) {
		t.Helper()
		before := exportString(t, db)
		err := db.Import(strings.NewReader(in))
		wantErr(t, err, e.ErrInvalid())
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Import gave %v, where an error with %q is wanted", err, want)
		}
		if after := exportString(t, db); after != before {
			t.Fatalf("a refused import changed the database from\n%s\nto\n%s", before, after)
		}
	}
	// Only into a database without record tables.
	refused(b, first, "already holds record tables")
	c, _ := openTemp(t, e)
	ok(t, c.Put("docs:1", nil))
	ok(t, c.Delete("docs:1"))
	refused(c, first, "already holds record tables")
	ok(t, c.Drop("docs"))
	ok(t, c.Import(strings.NewReader(first)))
	if got := exportString(t, c); got != first {
		t.Fatalf("an import into a database whose tables were dropped exports as\n%s\nwhere\n%s\nis wanted", got, first)
	}

	// An export cut short anywhere, and one that breaks a rule late on.
	d, _ := openTemp(t, e)
	for _, cut := range []int{0, 10, len(first) / 3, len(first) / 2, len(first) - 2} {
		refused(d, first[:cut], "not a valid HyperCrux export")
	}
	ok(t, d.Import(strings.NewReader(first[:len(first)-1]))) // the last line break can go
	d, _ = openTemp(t, e)
	lines := strings.SplitAfter(first, "\n")
	end := len(lines) - 2 // the end line; the last element is empty
	twice := strings.Join(slices.Concat(lines[:end], []string{lines[end-1]}, lines[end:]), "")
	refused(d, twice, "is there twice")
	var lastRecord int
	for i, l := range lines {
		if strings.HasPrefix(l, `{"key":`) {
			lastRecord = i
		}
	}
	twice = strings.Join(slices.Concat(lines[:lastRecord+1], []string{lines[lastRecord]}, lines[lastRecord+1:]), "")
	refused(d, twice, "is there twice")
	missing := strings.Join(slices.Concat(lines[:end], []string{`{"from":"docs:1","type":"x","to":"docs:nothing"}` + "\n"}, lines[end:]), "")
	refused(d, missing, "a link to a key that does not exist")
	if rep := checkOK(t, d); rep.Tables+rep.Records+rep.Links != 0 {
		t.Fatalf("refused imports kept %+v", rep)
	}
}

// Across moves a database from one engine to another and back: filled
// through from's API, exported, imported into to, exported, and imported
// back into from. Every export is byte for byte the first, and each
// database reads back the same, key by key, with each record's links both
// ways.
func Across(t *testing.T, from, to Engine) {
	t.Logf("from %s to %s", from.Name(), to.Name())
	a, _ := openTemp(t, from)
	keys := fill(t, from, a, 2)
	first := exportString(t, a)
	b, _ := openTemp(t, to)
	ok(t, b.Import(strings.NewReader(first)))
	second := exportString(t, b)
	if second != first {
		t.Fatalf("%s exports what %s exported as\n%s\nwhere %s's export is\n%s", to.Name(), from.Name(), second, from.Name(), first)
	}
	c, _ := openTemp(t, from)
	ok(t, c.Import(strings.NewReader(second)))
	if third := exportString(t, c); third != first {
		t.Fatalf("back in %s, the export is\n%s\nwhere the first was\n%s", from.Name(), third, first)
	}
	sameIn(t, a, b, keys)
	sameIn(t, a, c, keys)
}

func exportString(t testing.TB, db DB) string {
	t.Helper()
	var b strings.Builder
	ok(t, db.Export(&b))
	return b.String()
}

// sameIn checks that b reads back what a holds: every key the same, with
// its fields' types and bits and its links both ways, and Check's counts.
func sameIn(t testing.TB, a, b DB, keys []string) {
	t.Helper()
	for _, k := range keys {
		fa, errA := a.Get(k)
		fb, errB := b.Get(k)
		if showFields(fa) != showFields(fb) || fmt.Sprint(errA) != fmt.Sprint(errB) {
			t.Fatalf("Get(%q) gives %s, %v, where the first database gives %s, %v", k, showFields(fb), errB, showFields(fa), errA)
		}
		la, errA := a.Neighbours(k, Both, "")
		lb, errB := b.Neighbours(k, Both, "")
		if fmt.Sprintf("%q %v", la, errA) != fmt.Sprintf("%q %v", lb, errB) {
			t.Fatalf("Neighbours(%q, both) gives %q, %v, where the first database gives %q, %v", k, lb, errB, la, errA)
		}
	}
	ra, rb := checkOK(t, a), checkOK(t, b)
	if ra.Tables != rb.Tables || ra.Records != rb.Records || ra.Links != rb.Links || ra.Vectors != rb.Vectors {
		t.Fatalf("Check counts %+v, where the first database has %+v", rb, ra)
	}
}

// showFields writes fields with their types, and numbers with their bits,
// so that -0 and 0, or text and bytes, never look the same.
func showFields(f Fields) string {
	if f == nil {
		return "nil"
	}
	var parts []string
	for _, n := range slices.Sorted(maps.Keys(f)) {
		var v string
		switch x := f[n].(type) {
		case float64:
			v = fmt.Sprintf("float64 %016x", math.Float64bits(x))
		case Vector:
			bits := make([]string, len(x))
			for i, y := range x {
				bits[i] = fmt.Sprintf("%08x", math.Float32bits(y))
			}
			v = "Vector [" + strings.Join(bits, " ") + "]"
		default:
			v = fmt.Sprintf("%T %#v", x, x)
		}
		parts = append(parts, n+": "+v)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// fill puts awkward values into db through Put, Link, Delete and Drop, and
// returns every key it named. There are -0, subnormals and the extremes,
// text with zero bytes, escapes and every length of UTF-8, empty bytes,
// vectors with awkward values, fields that join a table out of byte order,
// a table that keeps its vector size with no vectors left, an empty table,
// a vector field without a size, links of awkward types, keys whose order
// differs from their tables', and random records and links from seed.
func fill(t *testing.T, e Engine, db DB, seed uint64) []string {
	t.Helper()
	var keys []string
	put := func(key string, f Fields) {
		t.Helper()
		ok(t, db.Put(key, f))
		keys = append(keys, key)
	}
	put("docs:1", Fields{"zeta": 1})
	put("docs:1", Fields{"alpha": int64(math.MinInt64), "vec": Vector{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, math.MaxFloat32}})
	put("docs:2", Fields{"Mid": "a\x00b é 😀 \U0010ffff", "neg": math.Copysign(0, -1), "tiny": math.SmallestNonzeroFloat64,
		"huge": -math.MaxFloat64, "raw": []byte{0, 1, 255}, "none": []byte{}, "max": int64(math.MaxInt64), "whole": 1e21})
	put("docs:3", Fields{"vec": Vector{1, 0, 1e-7}, "title": "\n\r\t\"\\\x1f\x7f ", "n": 0.1})
	put("sized:1", Fields{"vec": Vector{1, 2, 3, 4}})
	ok(t, db.Delete("sized:1"))
	put("empty:1", Fields{"a": 1})
	ok(t, db.Delete("empty:1"))
	put("gone:1", Fields{"vec": Vector{1}})
	ok(t, db.Drop("gone"))
	put("users2:zed", nil)
	put("users:ann", Fields{"vec": nil, "b": 2})
	for _, l := range []Link{
		{"users:ann", "owns", "docs:1"}, {"users2:zed", "owns", "docs:1"}, {"docs:1", "é é", "docs:2"},
		{"docs:1", strings.Repeat("é", 200), "docs:1"}, {"docs:2", "a\x00b", "docs:3"}, {"docs:3", "😀", "users:ann"},
	} {
		ok(t, db.Link(l.From, l.Type, l.To))
	}

	r := rand.New(rand.NewPCG(seed, 6))
	names := []string{"title", "Score", "body", "n", "when_"}
	var rnd []string
	for i := 0; i < 60; i++ {
		key := "rnd:" + strconv.Itoa(r.IntN(40))
		f := Fields{}
		for n := r.IntN(4); n > 0; n-- {
			var v any
			switch r.IntN(5) {
			case 0:
				v = int64(r.Uint64())
			case 1:
				for {
					if x := math.Float64frombits(r.Uint64()); !math.IsNaN(x) && !math.IsInf(x, 0) {
						v = x
						break
					}
				}
			case 2:
				v = strings.Repeat("ü", r.IntN(5))
			case 3:
				v = []byte(strconv.Itoa(r.IntN(1000)))
			}
			f[names[r.IntN(len(names))]] = v
		}
		if r.IntN(2) == 0 {
			f["vec"] = Vector{float32(r.NormFloat64()), float32(r.NormFloat64()), 1}
		}
		put(key, f)
		rnd = append(rnd, key)
	}
	for i := 0; i < 40; i++ {
		ok(t, db.Link(rnd[r.IntN(len(rnd))], []string{"cites", "owns"}[r.IntN(2)], rnd[r.IntN(len(rnd))]))
	}
	for i := 0; i < 5; i++ {
		if err := db.Delete(rnd[r.IntN(len(rnd))]); err != nil {
			wantErr(t, err, e.ErrNotFound())
		}
	}
	return keys
}
