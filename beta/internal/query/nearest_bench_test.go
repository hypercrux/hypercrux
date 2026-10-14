// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// benchStore is a store of n records with vectors of dims random values, a
// title and a group from 0 to 9, as beta/bench's tables of vectors have
// them, put straight into the store with no file.
func benchStore(b *testing.B, n, dims int) *store.Store {
	b.Helper()
	s := store.New()
	r := rand.New(rand.NewPCG(1, 2))
	vec := make([]float32, dims)
	for i := 0; i < n; i++ {
		for j := range vec {
			vec[j] = r.Float32()*2 - 1
		}
		fields := []format.Field{
			{Name: "grp", Value: value.Int(int64(i % 10))},
			{Name: "title", Value: value.Text("title " + strconv.Itoa(i))},
			{Name: "vec", Value: value.Vector(vec)},
		}
		if _, err := s.Put(nil, "docs:"+strconv.Itoa(i), fields); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

// BenchmarkNearestThroughAFilter is a search of 100,000 records with
// 384-value vectors, a tenth of them through a filter, grp = 3, as G4.md
// timed it: SQL's own nearest search, the plan ORDER BY distance() with vec
// IS NOT NULL and a LIMIT gets, and Nearest's with the same filter, as the
// public package makes it with NearestFilter.
func BenchmarkNearestThroughAFilter(b *testing.B) {
	s := benchStore(b, 100000, 384)
	q := make([]float32, 384)
	for i := range q {
		q[i] = float32(i%7) - 3
	}
	b.Run("SQL", func(b *testing.B) {
		st, err := Parse("SELECT key FROM docs WHERE grp = ? AND vec IS NOT NULL ORDER BY distance(vec, ?) LIMIT 10")
		if err != nil {
			b.Fatal(err)
		}
		sel := st.(*Select)
		args := []value.Value{value.Int(3), value.Bytes(value.Vector(q).Raw())}
		for i := 0; i < b.N; i++ {
			p, err := Prepare(s, sel)
			if err != nil || p.Shape() != ShapeNearest {
				b.Fatalf("%v, %v", p, err)
			}
			rows, err := p.Run(&Frame{Args: args})
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil || n != 10 {
				b.Fatalf("%d rows, %v", n, err)
			}
			rows.Close()
		}
	})
	b.Run("Nearest", func(b *testing.B) {
		cond, params, err := ParseCondition("grp = ?")
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			keep, _, err := NearestFilter(s, "docs", cond, params, &Frame{Args: []value.Value{value.Int(3)}})
			if err != nil {
				b.Fatal(err)
			}
			hits, err := s.Nearest("docs", q, 10, keep)
			if err != nil || len(hits) != 10 {
				b.Fatalf("%d hits, %v", len(hits), err)
			}
		}
	})
}
