// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value_test

import (
	"bytes"
	"math"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

func TestTheSixKinds(t *testing.T) {
	for _, c := range []struct {
		v    value.Value
		kind value.Kind
		name string
		show string
	}{
		{value.Value{}, value.KindNull, "null", "null"},
		{value.Null(), value.KindNull, "null", "null"},
		{value.Int(-7), value.KindInt, "int", "int -7"},
		{value.Real(0.5), value.KindReal, "real", "real 0.5 (3fe0000000000000)"},
		{value.Text("é\x00"), value.KindText, "text", `text "é\x00"`},
		{value.Bytes("\x00\xff"), value.KindBytes, "bytes", "bytes 00ff"},
		{value.Vector([]float32{1, -0.5}), value.KindVector, "vector", "vector [1 (3f800000), -0.5 (bf000000)]"},
	} {
		if c.v.Kind() != c.kind || c.kind.String() != c.name {
			t.Errorf("%v has kind %v, want %v", c.v, c.v.Kind(), c.name)
		}
		if c.v.IsNull() != (c.kind == value.KindNull) {
			t.Errorf("%v: IsNull() = %v", c.v, c.v.IsNull())
		}
		if got := c.v.String(); got != c.show {
			t.Errorf("String() = %q, want %q", got, c.show)
		}
	}
	if got := value.Kind(9).String(); got != "Kind(9)" {
		t.Errorf("Kind(9).String() = %q", got)
	}
}

func TestBitsComeBack(t *testing.T) {
	for _, bits := range []uint64{
		0,                  // 0
		1 << 63,            // -0
		1,                  // the smallest subnormal
		0x000fffffffffffff, // the largest subnormal
		0x7fefffffffffffff, // the largest float64
		0xffefffffffffffff, // the most negative
		0x7ff8000000000001, // a NaN with a payload, which SQL may make and the file never holds
	} {
		v := value.Real(math.Float64frombits(bits))
		if got := math.Float64bits(v.Real()); got != bits {
			t.Errorf("Real with bits %016x came back as %016x", bits, got)
		}
	}
	for _, i := range []int64{0, -1, math.MinInt64, math.MaxInt64} {
		if got := value.Int(i).Int(); got != i {
			t.Errorf("Int(%d) came back as %d", i, got)
		}
	}
	vec := []float32{
		math.Float32frombits(1 << 31),    // -0
		math.Float32frombits(1),          // the smallest subnormal
		math.Float32frombits(0x807fffff), // the most negative subnormal
		math.Float32frombits(0x7f7fffff), // the largest float32
		math.Float32frombits(0x3f800001), // one ulp past 1
		math.Float32frombits(0x3dcccccd), // 0.1
		math.Float32frombits(0x7fc00001), // a NaN with a payload
	}
	v := value.Vector(vec)
	if v.Dims() != len(vec) {
		t.Fatalf("Dims() = %d, want %d", v.Dims(), len(vec))
	}
	same := func(a, b []float32) bool {
		return slices.EqualFunc(a, b, func(x, y float32) bool { return math.Float32bits(x) == math.Float32bits(y) })
	}
	if got := v.Vector(); !same(got, vec) {
		t.Errorf("Vector() = %v, want %v", got, vec)
	}
	if got := v.AppendVector([]float32{9}); !same(got, append([]float32{9}, vec...)) {
		t.Errorf("AppendVector gave %v", got)
	}
	if got := value.VectorBits(v.Raw()); got != v {
		t.Errorf("VectorBits(Raw()) = %v, want %v", got, v)
	}
}

// TestVectorsAreStoredAsTheyAreWritten checks that a vector's bits are laid
// out as FORMAT.md writes them and as 0.x stores a vector in a blob.
func TestVectorsAreStoredAsTheyAreWritten(t *testing.T) {
	vec := []float32{0.12, -0.8, 1e-40, 3}
	want := conformance.Vector(vec).Bytes()
	if got := value.Vector(vec).Raw(); got != string(want) {
		t.Errorf("Raw() = % x, want % x", got, want)
	}
	if got := value.Vector(nil); got.Dims() != 0 || got.Raw() != "" {
		t.Errorf("an empty vector holds %v", got)
	}
}

// TestIdentity checks what == means for Values.
func TestIdentity(t *testing.T) {
	equal := []struct{ a, b value.Value }{
		{value.Value{}, value.Null()},
		{value.Int(3), value.Int(3)},
		{value.Real(0.25), value.Real(0.25)},
		{value.Text("a"), value.Text("a")},
		{value.Bytes("a"), value.Bytes("a")},
		{value.Vector([]float32{1, 2}), value.Vector([]float32{1, 2})},
		{value.Vector([]float32{1, 2}), value.VectorBits("\x00\x00\x80\x3f\x00\x00\x00\x40")},
	}
	for _, c := range equal {
		if c.a != c.b {
			t.Errorf("%v != %v", c.a, c.b)
		}
	}
	differ := []struct{ a, b value.Value }{
		{value.Real(0), value.Real(math.Copysign(0, -1))},
		{value.Text("a"), value.Bytes("a")},
		{value.Int(1), value.Real(1)},
		{value.Int(0), value.Null()},
		{value.Text(""), value.Null()},
		{value.Bytes(""), value.Text("")},
		{value.Vector([]float32{0}), value.Vector([]float32{float32(math.Copysign(0, -1))})},
		{value.Vector([]float32{1}), value.Bytes("\x00\x00\x80\x3f")},
	}
	for _, c := range differ {
		if c.a == c.b {
			t.Errorf("%v == %v", c.a, c.b)
		}
	}
}

func TestGo(t *testing.T) {
	if got := value.Null().Go(); got != nil {
		t.Errorf("null gives %#v", got)
	}
	if got, ok := value.Int(5).Go().(int64); !ok || got != 5 {
		t.Errorf("Int(5) gives %#v", value.Int(5).Go())
	}
	if got, ok := value.Real(2.5).Go().(float64); !ok || got != 2.5 {
		t.Errorf("Real(2.5) gives %#v", value.Real(2.5).Go())
	}
	if got, ok := value.Text("x").Go().(string); !ok || got != "x" {
		t.Errorf("Text gives %#v", value.Text("x").Go())
	}
	b := value.Bytes("\x01\x02")
	got, ok := b.Go().([]byte)
	if !ok || !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("Bytes gives %#v", b.Go())
	}
	got[0] = 9 // a copy, so the value doesn't change
	if b.Raw() != "\x01\x02" {
		t.Errorf("changing what Go returned changed the value to %v", b)
	}
	if vec, ok := value.Vector([]float32{1, 2}).Go().([]float32); !ok || !slices.Equal(vec, []float32{1, 2}) {
		t.Errorf("Vector gives %#v", value.Vector([]float32{1, 2}).Go())
	}
}

func TestAccessorsPanicOnTheWrongKind(t *testing.T) {
	calls := map[string]func(value.Value){
		"Int":          func(v value.Value) { v.Int() },
		"Real":         func(v value.Value) { v.Real() },
		"Text":         func(v value.Value) { v.Text() },
		"Bytes":        func(v value.Value) { v.Bytes() },
		"Raw":          func(v value.Value) { v.Raw() },
		"Dims":         func(v value.Value) { v.Dims() },
		"Vector":       func(v value.Value) { v.Vector() },
		"AppendVector": func(v value.Value) { v.AppendVector(nil) },
	}
	for name, call := range calls {
		func() {
			defer func() {
				if p := recover(); p == nil || !strings.Contains(p.(string), name) {
					t.Errorf("%s on null panicked with %v", name, p)
				}
			}()
			call(value.Null())
		}()
	}
	defer func() {
		if recover() == nil {
			t.Error("VectorBits took 3 bytes")
		}
	}()
	value.VectorBits("abc")
}

// TestAValueIsSmall pins the size the type's comment gives, since the
// store and SQL's rows hold a great many values.
func TestAValueIsSmall(t *testing.T) {
	if n := unsafe.Sizeof(value.Value{}); n != 32 {
		t.Errorf("a Value takes %d bytes, where its comment says 32", n)
	}
}
