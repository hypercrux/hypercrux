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

// TestTheStoreAgrees0x runs the same random puts, gets, deletes and drops,
// with Go values, on 0.x and on the store, through putGo as the public
// package will. After each step both must give the same error, with the
// same message, or the same record, and each table the same fields in the
// order 0.x's SELECT * gives its columns. Only the message for a field
// named twice in two spellings may differ, since 0.x reports whichever of
// the two its map gave first.
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
	outcomes := map[string]int{}
	for step := 0; step < steps; step++ {
		var desc string
		var zeroxErr, err error
		clash := false
		var touched string
		switch w := r.IntN(100); {
		case w < 60:
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
		case w < 75:
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
		case w < 95:
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
			}
		}
	}
	for _, tbl := range tables {
		agreeOnTable(t, db, s, tbl)
	}
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "delete ok",
		"delete not found", "drop ok", "drop invalid", "drop not found"} {
		if outcomes[o] < steps/500 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], steps, outcomes)
		}
	}
	if testing.Verbose() {
		t.Logf("outcomes: %v", outcomes)
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
