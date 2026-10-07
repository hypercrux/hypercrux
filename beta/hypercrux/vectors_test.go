// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The Beta's vector helpers are ported from 0.x, and these tests hold them
// to 0.x's own functions, run on the same inputs in the same binary: the
// same values bit for bit, the same error texts and the same kinds of
// error.

// sameVector reports whether two vectors hold the same values, bit for bit,
// and are both nil or both not.
func sameVector(a hc.Vector, b zx.Vector) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float32bits(a[i]) != math.Float32bits(b[i]) {
			return false
		}
	}
	return true
}

// sameError reports whether the Beta's error and 0.x's read the same and
// are of the same kind.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error() &&
		errors.Is(a, hc.ErrInvalid) == errors.Is(b, zx.ErrInvalid) &&
		errors.Is(a, hc.ErrNotFound) == errors.Is(b, zx.ErrNotFound)
}

// junk is what a random array sometimes holds in place of a number.
var junk = []string{"NaN", "Infinity", "-Infinity", "null", "true", `"1"`, "[1]", "{}", "", "1e", "-", "0x10", "+1", ".5", "01", "1e400"}

// randomNumber writes a number as JSON might hold it, across float32's
// range and past it at both ends.
func randomNumber(r *rand.Rand) string {
	switch r.IntN(10) {
	case 0:
		return strconv.Itoa(r.IntN(2001) - 1000)
	case 1:
		return "-0"
	case 2:
		return "0"
	case 3:
		return junk[r.IntN(len(junk))]
	}
	x := r.NormFloat64() * math.Pow(10, float64(r.IntN(101)-50))
	format := "geEf"[r.IntN(4)]
	return strconv.FormatFloat(x, format, r.IntN(20)-1, 64)
}

// randomArray writes a JSON array of numbers, now and then cut short.
func randomArray(r *rand.Rand) string {
	parts := make([]string, r.IntN(6))
	for i := range parts {
		parts[i] = randomNumber(r)
	}
	s := "[" + strings.Join(parts, []string{",", ", ", " ,\n"}[r.IntN(3)]) + "]"
	if r.IntN(15) == 0 {
		s = s[:r.IntN(len(s)+1)]
	}
	return s
}

func repeat(n int, s string) string {
	return "[" + strings.TrimSuffix(strings.Repeat(s+",", n), ",") + "]"
}

func TestParseVectorIs0xs(t *testing.T) {
	inputs := []string{
		"[0.5, 1e-3, -2]", "[1]", "[1,2,3]", " [ 1 , 2 ] ", "[1E+2, 2e-2]", "[1]\n",
		"[]", "null", "", " ", "[", "[0]", "[0, 0]", "[-0]", "[0, -0, 1]",
		"[1e39]", "[-1e39]", "[3.4028235e38]", "[3.4028236e38]", "[1e-46]", "[1e-46, 1]", "[1.4e-45]",
		"[1e400]", "[1, NaN]", "[1, Infinity]", "[1, null]", "[null]", "[true]", `["1"]`, "{}", "[1,]",
		"[1] x", "[[1]]", "1", `"[1]"`, "[0x10]", "[01]", "[+1]", "[.5]", "[5.]", "[1e]", "[-]", "[ 1]",
		"[123456789012345678901234567890]", "[0.1, 0.2, 0.30000000000000004]", "[1.0000000596046448]",
		repeat(hc.MaxDims, "1"), repeat(hc.MaxDims+1, "1"), repeat(hc.MaxDims, "0"),
	}
	r := rand.New(rand.NewPCG(4, 1))
	for range 3000 {
		inputs = append(inputs, randomArray(r))
	}
	for range 300 {
		b := make([]byte, r.IntN(12))
		for i := range b {
			b[i] = "[]0123456789.,-+eE nul"[r.IntN(22)]
		}
		inputs = append(inputs, string(b))
	}
	ok, refused := 0, 0
	for _, s := range inputs {
		got, gotErr := hc.ParseVector(s)
		want, wantErr := zx.ParseVector(s)
		if !sameVector(got, want) || !sameError(gotErr, wantErr) {
			t.Errorf("ParseVector(%.60q) = %v, %v; 0.x gives %v, %v", s, got, gotErr, want, wantErr)
		}
		if wantErr == nil {
			ok++
		} else {
			refused++
		}
	}
	if ok < 500 || refused < 500 {
		t.Errorf("the inputs give %d vectors and %d refusals, too few of one to compare", ok, refused)
	}

	// From 0.x's TestVectors.
	if v, err := hc.ParseVector("[0.5, 1e-3, -2]"); err != nil || !reflect.DeepEqual(v, hc.Vector{0.5, 0.001, -2}) {
		t.Fatalf("ParseVector: %v %v", v, err)
	}
}

func TestDecodeVectorIs0xs(t *testing.T) {
	inputs := [][]byte{nil, {}, {1}, {1, 2}, {1, 2, 3}, {1, 2, 3, 4, 5}, make([]byte, 4), make([]byte, 8)}
	for _, bits := range []uint32{0x7fc00001, 0xffc00000, 0x7f800000, 0xff800000, 0x00000001, 0x80000000, 0x7f7fffff, 0x3f800000} {
		inputs = append(inputs, hc.Vector{math.Float32frombits(bits)}.Bytes())
	}
	r := rand.New(rand.NewPCG(4, 2))
	for range 2000 {
		b := make([]byte, r.IntN(41))
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		inputs = append(inputs, b)
	}
	for _, b := range inputs {
		got, gotErr := hc.DecodeVector(b)
		want, wantErr := zx.DecodeVector(b)
		if !sameVector(got, want) || !sameError(gotErr, wantErr) {
			t.Errorf("DecodeVector(%x) = %v, %v; 0.x gives %v, %v", b, got, gotErr, want, wantErr)
		}
		if gotErr == nil && !bytes.Equal(got.Bytes(), b) {
			t.Errorf("DecodeVector(%x) gives %v, whose Bytes are %x", b, got, got.Bytes())
		}
	}
}

func TestVectorMethodsAre0xs(t *testing.T) {
	vecs := []hc.Vector{
		nil, {}, {0.5, 1}, {0}, {float32(math.Copysign(0, -1))}, {float32(math.NaN())}, {float32(math.Inf(1))},
		{float32(math.Inf(-1)), 1}, {math.SmallestNonzeroFloat32}, {math.MaxFloat32, -math.MaxFloat32}, {0.1},
		{1e-7}, {1e-6}, {1e21}, {1e20}, {123456789}, {0.5, -0.25, 1},
	}
	r := rand.New(rand.NewPCG(4, 3))
	for range 2000 {
		v := make(hc.Vector, r.IntN(9))
		for i := range v {
			if r.IntN(2) == 0 {
				v[i] = math.Float32frombits(r.Uint32())
			} else {
				v[i] = float32(r.NormFloat64() * math.Pow(10, float64(r.IntN(61)-30)))
			}
		}
		vecs = append(vecs, v)
	}
	for _, v := range vecs {
		z := zx.Vector(v)
		if got, want := v.String(), z.String(); got != want {
			t.Errorf("%#v.String() = %q; 0.x gives %q", v, got, want)
		}
		if got, want := v.Bytes(), z.Bytes(); !bytes.Equal(got, want) || (got == nil) != (want == nil) {
			t.Errorf("%#v.Bytes() = %x; 0.x gives %x", v, got, want)
		}
		got, gotErr := v.Value()
		want, wantErr := z.Value()
		if !reflect.DeepEqual(got, want) || !sameError(gotErr, wantErr) {
			t.Errorf("%#v.Value() = %v, %v; 0.x gives %v, %v", v, got, gotErr, want, wantErr)
		}
	}

	// From 0.x's TestVectors.
	if hc.Vector([]float32{0.5, 1}).String() != "[0.5,1]" {
		t.Fatal("Vector.String")
	}
}
