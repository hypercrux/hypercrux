// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// TestTheStoreAgrees0x runs the same random puts, gets, scans, links,
// unlinks, reads of links, walks, deletes and drops, with Go values, on 0.x
// and on the store, through putGo and scanGo as the public package will.
// After each step both must give the same error, with the same message, or
// the same record, or the same records, links or steps in the same order,
// and each table the same fields in the order 0.x's SELECT * gives its
// columns. Every so often, every record's links both ways must agree too.
// Only the message for a field named twice in two spellings may differ,
// since 0.x reports whichever of the two its map gave first.
//
// Searches come between the steps, with choices of their own, so the steps
// stay as they were before V1: random queries of each table's size and of
// other sizes, now and then one that breaks the rules, a table that isn't
// there, a k outside 1 to 10,000, or a filter on the key, which is SQL for
// 0.x and a Filter for the store. Both must give the same error, or hits
// that agree within conformance.DistanceBound. Every so often, a search of
// every vector in each table must agree too.
func TestTheStoreAgrees0x(t *testing.T) {
	db, err := hc.Open(filepath.Join(t.TempDir(), "agree.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := store.New()
	steps := 3000
	if testing.Short() {
		steps = 600
	}
	r := rand.New(rand.NewPCG(3, 0x51))
	pick := func(list []string) string { return list[r.IntN(len(list))] }
	tables := []string{"docs", "people", "t_1"}
	ids := []string{"1", "2", "3", "4", "é", "a b"}
	names := []string{"title", "Title", "TITLE", "n", "N", "score", "_x", "Zeta", "zeta", "a1", "vec", "Vec", "VEC"}
	badNames := []string{"key", "Rowid", "1st", "has space"}
	badKeys := []string{"nocolon", "docs:", "Docs:1", "docs:\x00", "hc_x:1"}
	when := time.Date(2026, 10, 7, 12, 0, 0, 5, time.FixedZone("IDT", 3*3600))
	values := []any{"Q3 plan", "", "é", "a\x00b", 12, -1, int64(math.MaxInt64), 0.75, math.Copysign(0, -1),
		math.SmallestNonzeroFloat64, true, false, []byte{0, 1, 255}, nil, nil, uint8(7), []any{"a", 1.5},
		map[string]any{"k": 1}, when, status("open"),
		"\xff", math.NaN(), math.Inf(-1), uint64(math.MaxUint64), []float32{1}, make(chan int)}
	sizes := map[string]int{}
	key := func() string {
		if r.IntN(25) == 0 {
			return pick(badKeys)
		}
		return pick(tables) + ":" + pick(ids)
	}
	vector := func(tbl string) any {
		n := sizes[tbl]
		if n == 0 || r.IntN(12) == 0 {
			n = 1 + r.IntN(3)
		}
		v := make([]float32, n)
		for i := range v {
			v[i] = float32(r.NormFloat64())
		}
		switch r.IntN(14) {
		case 0:
			return nil
		case 1:
			return make([]float32, n)
		case 2:
			v[0] = float32(math.NaN())
		case 3:
			f := make([]float64, n)
			for i := range f {
				f[i] = float64(v[i])
			}
			return f
		case 4:
			return "[1, 0.5]"
		case 5:
			return 5
		}
		return v
	}
	linkTypes := []string{"owns", "cites", "Owns", "x", "é", "a b"}
	badLinkTypes := []string{"", strings.Repeat("t", 201), "\xff"}
	linkType := func() string {
		if r.IntN(25) == 0 {
			return pick(badLinkTypes)
		}
		return pick(linkTypes)
	}
	readType := func() string {
		if r.IntN(2) == 0 {
			return ""
		}
		return linkType()
	}
	var made []store.Link // links that worked, which may have gone since
	linked := func() string {
		if len(made) == 0 || r.IntN(3) == 0 {
			return key()
		}
		l := made[r.IntN(len(made))]
		if r.IntN(2) == 0 {
			return l.From
		}
		return l.To
	}
	direction := func() store.Direction {
		if r.IntN(20) == 0 {
			return 3
		}
		return store.Direction(r.IntN(3))
	}
	outcomes := map[string]int{}
	rs := rand.New(rand.NewPCG(3, 0x61)) // the searches' choices
	for step := 0; step < steps; step++ {
		if rs.IntN(6) == 0 {
			searchBoth(t, db, s, rs, tables, sizes, outcomes)
		}
		var desc string
		var zeroxErr, err error
		clash := false
		var touched string
		switch w := r.IntN(100); {
		case w < 45:
			k := key()
			tbl, _, _ := strings.Cut(k, ":")
			f := fields{}
			bad := 0
			for n := r.IntN(4); n > 0; n-- {
				name := pick(names)
				if r.IntN(30) == 0 && bad == 0 { // one bad name at most, since 0.x's map picks which it reports
					name, bad = pick(badNames), 1
				}
				for have := range f {
					clash = clash || strings.EqualFold(have, name) && have != name
				}
				if strings.EqualFold(name, "vec") {
					f[name] = vector(tbl)
				} else {
					f[name] = values[r.IntN(len(values))]
				}
			}
			desc = fmt.Sprintf("put %q %v", k, f)
			zeroxErr, err = db.Put(k, hc.Fields(f)), putGo(s, k, f)
			if zeroxErr == nil {
				if tb, ok := s.Table(tbl); ok {
					sizes[tbl] = tb.Size
				}
			}
			touched = k
		case w < 53:
			k := key()
			desc = "get " + k
			want, e1 := db.Get(k)
			got, e2 := getGo(s, k)
			zeroxErr, err = e1, e2
			if e1 == nil && e2 == nil {
				for name, v := range want {
					if vec, ok := v.(hc.Vector); ok {
						want[name] = []float32(vec)
					}
				}
				if !reflect.DeepEqual(got, fields(want)) {
					t.Fatalf("step %d: %s: the store gives %#v, and 0.x %#v", step, desc, got, want)
				}
			}
		case w < 61:
			prefix := pick(tables) + ":"
			if r.IntN(3) == 0 {
				prefix += pick([]string{"1", "4", "a", "a ", "\xc3", "é", "x"})
			}
			if r.IntN(15) == 0 {
				prefix = pick([]string{"docs", "", "Docs:", "hc_x:", ":", "t-1:"})
			}
			after, limit := "", 0
			if r.IntN(3) == 0 {
				after = key()
			}
			if r.IntN(2) == 0 {
				limit = r.IntN(4)
			}
			if r.IntN(25) == 0 {
				limit = -1
			}
			desc = fmt.Sprintf("scan %q %q %d", prefix, after, limit)
			want, e1 := db.Scan(prefix, after, limit)
			got, e2 := scanGo(s, prefix, after, limit)
			zeroxErr, err = e1, e2
			if e1 == nil && e2 == nil && !reflect.DeepEqual(got, fromZerox(want)) {
				t.Fatalf("step %d: %s: the store gives %q, and 0.x %q:\n%v\n%v", step, desc, keysOf(got), keysOf(fromZerox(want)), got, want)
			}
		case w < 73:
			from, typ, to := linked(), linkType(), linked()
			if r.IntN(4) == 0 {
				to = from
			}
			desc = fmt.Sprintf("link %q %q %q", from, typ, to)
			zeroxErr = db.Link(from, typ, to)
			_, err = s.Link(nil, from, typ, to)
			if zeroxErr == nil {
				made = append(made, store.Link{From: from, Type: typ, To: to})
			}
		case w < 78:
			from, typ, to := key(), readType(), key()
			if len(made) > 0 && r.IntN(4) != 0 {
				l := made[r.IntN(len(made))]
				from, typ, to = l.From, l.Type, l.To
				if r.IntN(3) == 0 {
					typ = ""
				}
			}
			desc = fmt.Sprintf("unlink %q %q %q", from, typ, to)
			zeroxErr = db.Unlink(from, typ, to)
			_, err = s.Unlink(nil, from, typ, to)
		case w < 84:
			k, dir, typ := linked(), direction(), readType()
			desc = fmt.Sprintf("neighbours %q %v %q", k, dir, typ)
			want, e1 := db.Neighbours(k, hc.Direction(dir), typ)
			got, e2 := s.Neighbours(k, dir, typ)
			zeroxErr, err = e1, e2
			if e1 == nil && e2 == nil && !sameLinks(got, want) {
				t.Fatalf("step %d: %s: the store gives %v, and 0.x %v", step, desc, got, want)
			}
			if len(got) > 0 {
				outcomes["neighbours found some"]++
			}
		case w < 90:
			k, dir, typ, depth := linked(), direction(), readType(), 1+r.IntN(4)
			if r.IntN(20) == 0 {
				depth = []int{0, -1, store.MaxDepth + 1}[r.IntN(3)]
			}
			desc = fmt.Sprintf("walk %q %v %q %d", k, dir, typ, depth)
			want, e1 := db.Walk(k, hc.Direction(dir), typ, depth)
			got, e2 := s.Walk(k, dir, typ, depth)
			zeroxErr, err = e1, e2
			if e1 == nil && e2 == nil && !sameSteps(got, want) {
				t.Fatalf("step %d: %s: the store gives %v, and 0.x %v", step, desc, got, want)
			}
			if len(got) > 0 && got[len(got)-1].Depth > 1 {
				outcomes["walk went further than one link"]++
			}
		case w < 98:
			k := key()
			desc = "delete " + k
			zeroxErr = db.Delete(k)
			_, err = s.Delete(nil, k)
		default:
			name := pick(tables)
			if r.IntN(4) == 0 {
				name = pick([]string{"Docs", "nosuch", "hc_x"})
			}
			desc = "drop " + name
			zeroxErr = db.Drop(name)
			_, err = s.Drop(nil, name)
			delete(sizes, name)
		}
		if kind(err) != zeroxKind(zeroxErr) || !clash && err != nil && err.Error() != zeroxErr.Error() {
			t.Fatalf("step %d: %s: the store gives %v, and 0.x %v", step, desc, err, zeroxErr)
		}
		outcomes[strings.Fields(desc)[0]+" "+kind(err)]++
		if touched != "" && zeroxErr == nil {
			agreeOn(t, db, s, touched)
		}
		if step%100 == 0 {
			for _, tbl := range tables {
				agreeOnTable(t, db, s, tbl)
				agreeOnSearch(t, db, s, tbl, sizes[tbl])
			}
			agreeOnLinks(t, db, s, tables, ids)
		}
	}
	for _, tbl := range tables {
		agreeOnTable(t, db, s, tbl)
		agreeOnSearch(t, db, s, tbl, sizes[tbl])
	}
	agreeOnLinks(t, db, s, tables, ids)
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "scan ok", "scan invalid",
		"link ok", "link invalid", "link not found", "unlink ok", "unlink not found", "neighbours ok",
		"neighbours invalid", "neighbours not found", "neighbours found some", "walk ok", "walk invalid",
		"walk not found", "walk went further than one link", "nearest ok", "nearest invalid", "nearest not found",
		"nearest found some", "nearest filtered", "delete ok", "delete not found", "drop ok", "drop invalid",
		"drop not found"} {
		if outcomes[o] < steps/500 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], steps, outcomes)
		}
	}
	if testing.Verbose() {
		t.Logf("outcomes: %v", outcomes)
	}
}

// searchBoth makes one random search on 0.x and on the store, which must
// agree, with its choices from rs. sizes holds each table's vector size as
// the step that last put into it left it, or 0.
func searchBoth(t *testing.T, db *hc.DB, s *store.Store, rs *rand.Rand, tables []string, sizes map[string]int, outcomes map[string]int) {
	t.Helper()
	tbl := tables[rs.IntN(len(tables))]
	if rs.IntN(15) == 0 {
		tbl = []string{"Docs", "nosuch", "hc_x", ""}[rs.IntN(4)]
	}
	n := sizes[tbl]
	if n == 0 || rs.IntN(12) == 0 {
		n = 1 + rs.IntN(3)
	}
	q := make([]float32, n)
	for i := range q {
		q[i] = float32(rs.NormFloat64())
	}
	switch rs.IntN(20) {
	case 0:
		q = make([]float32, n)
	case 1:
		q[0] = float32(math.NaN())
	case 2:
		q = nil
	}
	k := 1 + rs.IntN(6)
	if rs.IntN(15) == 0 {
		k = []int{0, -1, store.MaxK, store.MaxK + 1}[rs.IntN(4)]
	}
	where, before := "", tbl+":"+[]string{"2", "4", "é"}[rs.IntN(3)]
	var keep store.Filter
	var args []any
	if rs.IntN(3) == 0 {
		where, args = "key < ?", []any{before}
		keep = func(r store.Record) (bool, error) { return r.Key < before, nil }
	}
	want, zerr := db.Nearest(tbl, hc.Vector(q), k, where, args...)
	got, err := s.Nearest(tbl, q, k, keep)
	sameSearch(t, fmt.Sprintf("nearest %q %v %d %q", tbl, q, k, where), got, err, want, zerr)
	outcomes["nearest "+kind(err)]++
	if len(got) > 0 {
		outcomes["nearest found some"]++
		if where != "" {
			outcomes["nearest filtered"]++
		}
	}
}

// agreeOnSearch compares a search of every vector in a table, with a query
// of the table's size, on 0.x and on the store.
func agreeOnSearch(t *testing.T, db *hc.DB, s *store.Store, tbl string, size int) {
	t.Helper()
	q := make([]float32, max(size, 1))
	for i := range q {
		q[i] = float32(i%3) - 0.5
	}
	want, zerr := db.Nearest(tbl, hc.Vector(q), store.MaxK, "")
	got, err := s.Nearest(tbl, q, store.MaxK, nil)
	sameSearch(t, fmt.Sprintf("a search of every vector in %s", tbl), got, err, want, zerr)
}

// sameLinks reports whether the store's links are 0.x's, in the same
// order, with nil for none in both.
func sameLinks(got []store.Link, want []hc.Link) bool {
	var conv []store.Link
	for _, l := range want {
		conv = append(conv, store.Link(l))
	}
	return reflect.DeepEqual(got, conv)
}

// sameSteps reports whether the store's steps are 0.x's, in the same
// order, with nil for none in both.
func sameSteps(got []store.Step, want []hc.Step) bool {
	var conv []store.Step
	for _, st := range want {
		conv = append(conv, store.Step(st))
	}
	return reflect.DeepEqual(got, conv)
}

// agreeOnLinks compares every record's links both ways, and the count of
// links in 0.x's Check and the store's snapshot.
func agreeOnLinks(t *testing.T, db *hc.DB, s *store.Store, tables, ids []string) {
	t.Helper()
	for _, tbl := range tables {
		for _, id := range ids {
			k := tbl + ":" + id
			want, e1 := db.Neighbours(k, hc.Both, "")
			got, e2 := s.Neighbours(k, store.Both, "")
			if kind(e2) != zeroxKind(e1) || !sameLinks(got, want) {
				t.Fatalf("%s: the store has the links %v, %v, and 0.x %v, %v", k, got, e2, want, e1)
			}
		}
	}
	if rep := must[hc.Report](t)(db.Check()); rep.Links != links(t, s) {
		t.Fatalf("0.x counts %d links, and the store's snapshot %d", rep.Links, links(t, s))
	}
}

func kind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errs.ErrNotFound):
		return "not found"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	}
	return "error"
}

func zeroxKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, hc.ErrNotFound):
		return "not found"
	case errors.Is(err, hc.ErrInvalid):
		return "invalid"
	}
	return "error"
}

// agreeOn compares one record, and its table's fields.
func agreeOn(t *testing.T, db *hc.DB, s *store.Store, key string) {
	t.Helper()
	want, err := db.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range want {
		if vec, ok := v.(hc.Vector); ok {
			want[name] = []float32(vec)
		}
	}
	got := must[fields](t)(getGo(s, key))
	if !reflect.DeepEqual(got, fields(want)) {
		t.Fatalf("%s: the store holds %#v, and 0.x %#v", key, got, want)
	}
	agreeOnTable(t, db, s, key[:strings.IndexByte(key, ':')])
}

// agreeOnTable compares a table's fields, in order, with the columns of
// 0.x's SELECT *, and the tables' records.
func agreeOnTable(t *testing.T, db *hc.DB, s *store.Store, tbl string) {
	t.Helper()
	tb, ok := s.Table(tbl)
	rows, err := db.Query(`SELECT * FROM "` + tbl + `" ORDER BY key`)
	if err != nil {
		if ok || !strings.Contains(err.Error(), "no such table") {
			t.Fatalf("table %s: the store has %+v, and 0.x gives %v", tbl, tb, err)
		}
		return
	}
	cols, err := rows.Columns()
	var keys []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, vals[0].(string))
	}
	rows.Close()
	if err != nil || !ok || !slices.Equal(cols, append([]string{"key"}, tb.Fields...)) {
		t.Fatalf("table %s: the store has %+v, and 0.x the columns %q (%v)", tbl, tb, cols, err)
	}
	for _, k := range keys {
		if _, err := s.Get(k); err != nil {
			t.Fatalf("table %s: 0.x has %s, and the store gives %v", tbl, k, err)
		}
	}
}
