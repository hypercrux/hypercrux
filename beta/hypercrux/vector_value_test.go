// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestAVectorValueIsFromGos: the way convert takes for a Vector or a
// []float32 in the vector field (vectorValue) gives what value.FromGo gives,
// bit for bit, or the same error, for random vectors of every length up to a
// few hundred and at the ends of the range, and for awkward values: -0,
// subnormals, float32's extremes, NaN, the infinities and all zeros, in
// every place among four running sums. A vector the caller changes after the
// put leaves the value as it was.
func TestAVectorValueIsFromGos(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	awkward := []float32{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, -math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32, 1e-38, 3}
	bad := []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))}
	var cases [][]float32
	for n := 1; n <= 300; n++ {
		v := make([]float32, n)
		for i := range v {
			v[i] = float32(r.NormFloat64())
		}
		cases = append(cases, v)
		w := make([]float32, n)
		for i := range w {
			w[i] = awkward[r.IntN(len(awkward))]
		}
		cases = append(cases, w)
		for _, b := range bad {
			x := append([]float32(nil), v...)
			x[r.IntN(n)] = b
			cases = append(cases, x)
		}
		cases = append(cases, make([]float32, n)) // all zeros
		z := make([]float32, n)
		z[r.IntN(n)] = math.SmallestNonzeroFloat32 // all zeros but one subnormal
		cases = append(cases, z)
	}
	big := make([]float32, rules.MaxDims)
	big[len(big)-1] = math.MaxFloat32
	cases = append(cases, []float32{}, big, make([]float32, rules.MaxDims+1))
	for i, v := range cases {
		want, wantErr := value.FromGo("vec", Vector(v))
		for _, given := range []any{Vector(v), v} {
			var got value.Value
			var err error
			switch g := given.(type) {
			case Vector:
				got, err = vectorValue(g)
			case []float32:
				got, err = vectorValue(g)
			}
			switch {
			case (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error():
				t.Fatalf("case %d, %T of %d values: vectorValue gave the error %v, and FromGo %v", i, given, len(v), err, wantErr)
			case got != want:
				t.Fatalf("case %d, %T of %d values: vectorValue gave other bits than FromGo", i, given, len(v))
			}
		}
	}
	v := []float32{0.5, -0.25, 1}
	got, err := vectorValue(v)
	if err != nil {
		t.Fatal(err)
	}
	v[0] = 99
	if got != value.Vector([]float32{0.5, -0.25, 1}) {
		t.Fatal("changing the vector after vectorValue changed the value")
	}
}

// TestAVectorBufferUsedAgainInAnUpdate: an Update that fills one buffer with
// each record's vector in turn, as a program reading vectors from a stream
// would, puts what the buffer held at each put. The copy holds it, and so
// does the file, which a reopen reads back.
func TestAVectorBufferUsedAgainInAnUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	buf := make(Vector, 8)
	err = db.Update(func(tx *Tx) error {
		for i := range 3 {
			for j := range buf {
				buf[j] = float32(10*i + j + 1)
			}
			if err := tx.Put("docs:"+strconv.Itoa(i), Fields{"vec": buf}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	look := func(db *DB) string {
		var b strings.Builder
		for i := range 3 {
			f, err := db.Get("docs:" + strconv.Itoa(i))
			fmt.Fprintln(&b, f, err)
		}
		return b.String()
	}
	want := "map[vec:[1,2,3,4,5,6,7,8]] <nil>\nmap[vec:[11,12,13,14,15,16,17,18]] <nil>\nmap[vec:[21,22,23,24,25,26,27,28]] <nil>\n"
	if got := look(db); got != want {
		t.Fatalf("the copy holds\n%s\nwhere\n%s\nwent in", got, want)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := look(again); got != want {
		t.Fatalf("a reopen reads\n%s\nwhere\n%s\nwent in", got, want)
	}
}

// BenchmarkVectorValue sets vectorValue beside value.FromGo for a Vector of
// 384 values, as each put of the batched put converts one.
func BenchmarkVectorValue(b *testing.B) {
	v := make(Vector, 384)
	for i := range v {
		v[i] = float32(i%17) - 8
	}
	b.Run("vectorValue", func(b *testing.B) {
		for range b.N {
			if _, err := vectorValue(v); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("FromGo", func(b *testing.B) {
		for range b.N {
			if _, err := value.FromGo("vec", v); err != nil {
				b.Fatal(err)
			}
		}
	})
}
