// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value_test

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux"
	"github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

type (
	status   string
	flag     bool
	count    uint64
	temp     float64
	myFloat  float32
	myFloats []float32
	myTime   time.Time
	blob     []byte
)

func fromGo(t *testing.T, field string, v any) value.Value {
	t.Helper()
	got, err := value.FromGo(field, v)
	if err != nil {
		t.Fatalf("FromGo(%s, %#v): %v", field, v, err)
	}
	return got
}

// TestFromGoRoundTrip has the values of 0.x's PutAndGetRoundTrip, and what
// 0.x's Get gives back for each.
func TestFromGoRoundTrip(t *testing.T) {
	when := time.Date(2026, 10, 6, 9, 30, 0, 0, time.FixedZone("IDT", 3*3600))
	for _, c := range []struct {
		field string
		v     any
		kind  value.Kind
		want  any
	}{
		{"title", "Q3 plan", value.KindText, "Q3 plan"},
		{"pages", 12, value.KindInt, int64(12)},
		{"score", 0.75, value.KindReal, 0.75},
		{"draft", true, value.KindInt, int64(1)},
		{"raw", []byte{0, 1, 255}, value.KindBytes, []byte{0, 1, 255}},
		{"due", when, value.KindText, "2026-10-06T06:30:00Z"},
		{"tags", []any{"plan", "q3"}, value.KindText, `["plan","q3"]`},
		{"meta", map[string]any{"owner": "dana"}, value.KindText, `{"owner":"dana"}`},
		{"small", uint8(7), value.KindInt, int64(7)},
		{"vec", []float32{0.5, -0.25, 1}, value.KindVector, []float32{0.5, -0.25, 1}},
	} {
		got := fromGo(t, c.field, c.v)
		if got.Kind() != c.kind || !reflect.DeepEqual(got.Go(), c.want) {
			t.Errorf("FromGo(%s, %#v) = %v, want %#v", c.field, c.v, got, c.want)
		}
	}
}

// TestFromGoMoreFieldTypes has the values of 0.x's MoreFieldTypes.
func TestFromGoMoreFieldTypes(t *testing.T) {
	var nilInt *int
	n := 7
	for _, c := range []struct {
		field string
		v     any
		want  any
	}{
		{"status", status("open"), "open"},
		{"tags", []string{"a", "b"}, `["a","b"]`},
		{"meta", map[string]string{"k": "v"}, `{"k":"v"}`},
		{"point", struct{ X, Y int }{1, 2}, `{"X":1,"Y":2}`},
		{"missing", nilInt, nil},
		{"count", &n, int64(7)},
	} {
		if got := fromGo(t, c.field, c.v); !reflect.DeepEqual(got.Go(), c.want) {
			t.Errorf("FromGo(%s, %#v) = %v, want %#v", c.field, c.v, got, c.want)
		}
	}
}

// TestFromGoRefuses has the values 0.x's KeyAndFieldRules refuses, a
// []float32 standing in for 0.x's Vector.
func TestFromGoRefuses(t *testing.T) {
	for _, c := range []struct {
		field string
		v     any
	}{
		{"a", math.NaN()}, {"a", math.Inf(1)}, {"a", uint64(math.MaxUint64)}, {"a", make(chan int)}, {"a", func() {}},
		{"a", "\xff"}, {"emb", []float32{1}}, {"vec", "not a vector"}, {"vec", []float32{}}, {"vec", []float32{0, 0}},
		{"vec", []float32{1, float32(math.NaN())}},
	} {
		if _, err := value.FromGo(c.field, c.v); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("FromGo(%s, %#v) = %v, want ErrInvalid", c.field, c.v, err)
		}
	}
}

// TestFromGoCopies checks that bytes are copied, so changing the slice
// afterwards leaves the value alone.
func TestFromGoCopies(t *testing.T) {
	b := []byte{1, 2}
	v := fromGo(t, "raw", b)
	b[0] = 9
	if v.Raw() != "\x01\x02" {
		t.Errorf("the value changed with its slice: %v", v)
	}
	f := []float32{1, 2}
	vec := fromGo(t, "vec", f)
	f[0] = 9
	if got := vec.Vector(); got[0] != 1 {
		t.Errorf("the vector changed with its slice: %v", vec)
	}
}

// TestFromGoKeepsBits checks that reals and vector values keep their bits.
func TestFromGoKeepsBits(t *testing.T) {
	for _, bits := range []uint64{1 << 63, 1, 0x000fffffffffffff, 0x7fefffffffffffff} {
		f := math.Float64frombits(bits)
		if got := math.Float64bits(fromGo(t, "a", f).Real()); got != bits {
			t.Errorf("%016x came back as %016x", bits, got)
		}
	}
	vec := []float32{math.Float32frombits(1 << 31), math.Float32frombits(1), 1}
	if got := fromGo(t, "Vec", vec).Raw(); got != value.Vector(vec).Raw() {
		t.Errorf("the vector's bits changed: % x", got)
	}
}

// TestFromGoAgrees0x puts each value into 0.x and converts it with FromGo:
// both must refuse it with the same message, or 0.x's Get must give back
// what Go gives for the Value. 0.x's own Vector can't take part, since
// FromGo knows the Beta's Vector instead, so []float32 stands in for it.
func TestFromGoAgrees0x(t *testing.T) {
	db, err := hc.Open(filepath.Join(t.TempDir(), "values.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var nilInt *int
	n := 7
	pn := &n
	s := status("open")
	when := time.Date(2026, 10, 6, 9, 30, 0, 123456789, time.FixedZone("IDT", 3*3600))
	cases := []struct {
		field string
		v     any
	}{
		{"title", "Q3 plan"}, {"t", ""}, {"t", "a\x00b"}, {"t", "é😀"}, {"t", "\xff"}, {"t", "a\xc3"},
		{"pages", 12}, {"i", int8(-3)}, {"i", int16(-300)}, {"i", int32(-70000)}, {"i", int64(math.MinInt64)},
		{"u", uint(7)}, {"u", uint16(65535)}, {"u", uint32(math.MaxUint32)}, {"u", uint64(math.MaxInt64)},
		{"u", uint64(math.MaxInt64) + 1}, {"u", uint(math.MaxUint64)}, {"u", uintptr(5)}, {"u", count(math.MaxUint64)},
		{"score", 0.75}, {"r", math.Copysign(0, -1)}, {"r", math.SmallestNonzeroFloat64}, {"r", math.MaxFloat64},
		{"r", math.NaN()}, {"r", math.Inf(1)}, {"r", math.Inf(-1)}, {"r", float32(0.1)}, {"r", math.Float32frombits(1)},
		{"r", temp(1.5)}, {"r", temp(math.NaN())}, {"r", myFloat(2.5)},
		{"draft", true}, {"off", false}, {"f", flag(true)},
		{"raw", []byte{0, 1, 255}}, {"raw", []byte{}}, {"raw", blob{1, 2}}, {"raw", json.RawMessage(`{"x":1}`)},
		{"due", when}, {"due", &when}, {"due", myTime(when)},
		{"n", json.Number("12")}, {"n", json.Number("-9223372036854775809")}, {"n", json.Number("1.5")},
		{"n", json.Number("1e400")}, {"n", json.Number("x")}, {"n", json.Number("-0")},
		{"tags", []any{"plan", "q3"}}, {"tags", []string{"a", "b"}}, {"tags", []string{"\xff"}}, {"tags", []any{math.NaN()}},
		{"tags", []any(nil)}, {"tags", []int(nil)}, {"tags", [0]int{}}, {"tags", [2]float32{1, 2}}, {"tags", []myFloat{1}},
		{"meta", map[string]any{"owner": "dana"}}, {"meta", map[string]string{"k": "\xff"}}, {"meta", map[string]any(nil)},
		{"meta", map[string]any{"x": math.Inf(1)}}, {"meta", map[int]string{1: "a"}},
		{"point", struct{ X, Y int }{1, 2}}, {"point", struct{}{}}, {"point", struct{ C chan int }{}},
		{"status", s}, {"status", &s}, {"missing", nilInt}, {"count", pn}, {"count", &pn},
		{"a", make(chan int)}, {"a", func() {}}, {"a", complex(1, 2)},
		{"emb", []float32{1}}, {"emb", []float64{1}}, {"emb", []float32{}}, {"emb", &[]float32{1}},
		{"emb", myFloats{1}}, {"emb", conformance.Vector{1}},
		{"vec", nil}, {"vec", []float32{0.5, -0.25, 1}}, {"Vec", []float32{1, 2}}, {"VEC", []float32{3}},
		{"vec", []float64{0.1, -2}}, {"vec", []float64{1e300}}, {"vec", []float64{1e-50, 0}},
		{"vec", []any{1.0, json.Number("2")}}, {"vec", []any{1, 2}}, {"vec", []any{json.Number("1e400")}},
		{"vec", []any{json.Number("x")}}, {"vec", []any{"1"}}, {"vec", "[1, 2]"}, {"vec", "[1e400]"},
		{"vec", "null"}, {"vec", "not a vector"}, {"vec", ""},
		{"vec", []float32{}}, {"vec", []float32(nil)}, {"vec", []float32{0, 0}}, {"vec", []float32{1, float32(math.NaN())}},
		{"vec", []float32{float32(math.Inf(1))}}, {"vec", []float32{float32(math.Copysign(0, -1))}},
		{"vec", (*[]float32)(nil)}, {"vec", &[]float32{1}}, {"vec", nilInt}, {"vec", myFloats{1}},
		{"vec", conformance.Vector{1}}, {"vec", []byte{0, 0, 0x80, 0x3f}}, {"vec", 5}, {"vec", "\xff"},
	}
	for i, c := range cases {
		key := "t" + strconv.Itoa(i) + ":1" // a table each, so no vector size gets in the way
		zeroxErr := db.Put(key, hc.Fields{c.field: c.v})
		got, err := value.FromGo(c.field, c.v)
		if (err == nil) != (zeroxErr == nil) || err != nil && err.Error() != zeroxErr.Error() {
			t.Errorf("%s %#v: FromGo gives %v, %v; 0.x gives %v", c.field, c.v, got, err, zeroxErr)
			continue
		}
		if err != nil {
			if !errors.Is(err, errs.ErrInvalid) || !errors.Is(zeroxErr, hc.ErrInvalid) {
				t.Errorf("%s %#v: FromGo gives %v and 0.x %v, which should both be invalid", c.field, c.v, err, zeroxErr)
			}
			continue
		}
		f, err := db.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		want := f[c.field]
		if vec, ok := want.(hc.Vector); ok {
			want = []float32(vec)
		}
		if g := got.Go(); !reflect.DeepEqual(g, want) {
			t.Errorf("%s %#v: FromGo gives %#v, and 0.x's Get %#v", c.field, c.v, g, want)
		} else if r, ok := g.(float64); ok && math.Float64bits(r) != math.Float64bits(want.(float64)) {
			t.Errorf("%s %#v: FromGo gives the bits %016x, and 0.x's Get %016x", c.field, c.v, math.Float64bits(r), math.Float64bits(want.(float64)))
		}
	}
}
