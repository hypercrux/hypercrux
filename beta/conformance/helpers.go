// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// DistanceBound is how far two engines' distances may differ, since they may
// add the products in a different order.
const DistanceBound = 1e-9

func openTemp(t testing.TB, e Engine) (DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := e.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func must[T any](t testing.TB) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func checkOK(t testing.TB, db DB) Report {
	t.Helper()
	rep := must[Report](t)(db.Check())
	if !rep.OK() {
		t.Fatalf("Check found problems:\n%s", strings.Join(rep.Problems, "\n"))
	}
	return rep
}

// cosineDistance is the reference: one minus the cosine, from 0 to 2, added
// up in plain order in float64, with a zero vector at distance 1.
func cosineDistance(a, b Vector) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 1
	}
	d := 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
	return min(max(d, 0), 2)
}

// bruteNearest compares every vector, the slow and obvious way.
func bruteNearest(vecs map[string]Vector, q Vector, k int, keep func(string) bool) []Hit {
	var hits []Hit
	for key, v := range vecs {
		if keep != nil && !keep(key) {
			continue
		}
		hits = append(hits, Hit{key, cosineDistance(v, q)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Distance != hits[j].Distance {
			return hits[i].Distance < hits[j].Distance
		}
		return hits[i].Key < hits[j].Key
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

func randomVector(r *rand.Rand, dims int) Vector {
	v := make(Vector, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// sameHits compares two result lists. Distances must agree within
// DistanceBound. Keys must come in the same order, except within a run of
// distances that agree within the bound, where either order is right.
func sameHits(t testing.TB, got, want []Hit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range got {
		if math.Abs(got[i].Distance-want[i].Distance) > DistanceBound {
			t.Fatalf("hit %d: got %v, want %v", i, got[i], want[i])
		}
	}
	for i := 0; i < len(want); {
		j := i + 1
		for j < len(want) && want[j].Distance-want[j-1].Distance <= DistanceBound {
			j++
		}
		g, w := keysOf(got[i:j]), keysOf(want[i:j])
		sort.Strings(g)
		sort.Strings(w)
		if strings.Join(g, " ") != strings.Join(w, " ") {
			t.Fatalf("hits %d to %d: got %v, want %v", i, j-1, got[i:j], want[i:j])
		}
		i = j
	}
}

func keysOf(hits []Hit) []string {
	keys := make([]string, len(hits))
	for i, h := range hits {
		keys[i] = h.Key
	}
	return keys
}

// firstColumn returns the first column of every row, joined by spaces.
func firstColumn(t testing.TB, rows *sql.Rows, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals[0]))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " ")
}
