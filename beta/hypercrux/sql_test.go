// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
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

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The database/sql driver: values out and arguments in against 0.x's,
// SQL().Begin refused, the transaction carried in each statement's context,
// errors of 0.x's kinds, and rows copied out before Query returns. Running
// SQL is task G4's, so these tests hand the driver an engine of their own
// with hc.UseEngine, which reads and writes the store as G4's will.

// table is a query.Rows over rows the test holds. It hands every row out
// in one buffer, which it fills again for the next row, as an operator
// may, and notes when it's closed.
type table struct {
	rows   [][]value.Value
	err    error // what Err gives once the rows have run out
	buf    []value.Value
	at     int
	closed bool
}

func (t *table) Next() bool {
	if t.closed || t.at == len(t.rows) {
		return false
	}
	t.buf = append(t.buf[:0], t.rows[t.at]...)
	t.at++
	return true
}

func (t *table) Row() []value.Value { return t.buf }

func (t *table) Err() error {
	if t.at == len(t.rows) {
		return t.err
	}
	return nil
}

func (t *table) Close() { t.closed = true }

// fieldN reads the field n of the record key from r, or null when the
// record has no value there.
func fieldN(r store.Reader, key string) (value.Value, error) {
	rec, err := r.Get(key)
	if err != nil {
		return value.Value{}, err
	}
	tbl, _ := r.Table(key[:strings.IndexByte(key, ':')])
	return rec.Field(slices.Index(tbl.Fields, "n")), nil
}

// lazyN is a query.Rows that reads the field n of each of its keys from r
// as Next asks for it, as a scan over the store does.
type lazyN struct {
	r      store.Reader
	keys   []string
	row    []value.Value
	err    error
	closed bool
}

func (l *lazyN) Next() bool {
	if l.closed || l.err != nil || len(l.keys) == 0 {
		return false
	}
	v, err := fieldN(l.r, l.keys[0])
	l.keys = l.keys[1:]
	if err != nil {
		l.err = err
		return false
	}
	l.row = append(l.row[:0], v)
	return true
}

func (l *lazyN) Row() []value.Value { return l.row }
func (l *lazyN) Err() error         { return l.err }
func (l *lazyN) Close()             { l.closed = true }

// describe writes what v holds, with its type, every number's bits and
// every byte, and nil apart from empty, so that what a scan gives on 0.x
// and on the Beta compares as text.
func describe(v reflect.Value) string {
	if !v.IsValid() {
		return "nothing"
	}
	t := v.Type()
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return t.String() + "(nil)"
		}
		return t.String() + "(" + describe(v.Elem()) + ")"
	case reflect.Float32:
		return fmt.Sprintf("%s %v (%08x)", t, v.Float(), math.Float32bits(float32(v.Float())))
	case reflect.Float64:
		return fmt.Sprintf("%s %v (%016x)", t, v.Float(), math.Float64bits(v.Float()))
	case reflect.Complex64, reflect.Complex128:
		c := v.Complex()
		return fmt.Sprintf("%s %v (%016x %016x)", t, c, math.Float64bits(real(c)), math.Float64bits(imag(c)))
	case reflect.String:
		return fmt.Sprintf("%s %q", t, v.String())
	case reflect.Slice:
		if v.IsNil() {
			return t.String() + "(nil)"
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return fmt.Sprintf("%s %x", t, v.Bytes())
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = describe(v.Index(i))
		}
		return t.String() + "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		if v.IsNil() {
			return t.String() + "(nil)"
		}
		return fmt.Sprintf("%s of %d", t, v.Len())
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() {
			tm := v.Interface().(time.Time)
			return fmt.Sprintf("time.Time %s in %s", tm.Format(time.RFC3339Nano), tm.Location())
		}
		var parts []string
		for i := range t.NumField() {
			if t.Field(i).IsExported() {
				parts = append(parts, t.Field(i).Name+": "+describe(v.Field(i)))
			}
		}
		return t.String() + "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprintf("%s %v", t, v.Interface())
}

// describeAny describes x as an `any` holds it, type and all.
func describeAny(x any) string { return describe(reflect.ValueOf(&x).Elem()) }

// goOf gives a value as database/sql would hand it to a *any, so that the
// value a statement was given compares with what 0.x gives back for it.
func goOf(v value.Value) any {
	switch v.Kind() {
	case value.KindInt:
		return v.Int()
	case value.KindReal:
		return v.Real()
	case value.KindText:
		return v.Text()
	case value.KindBytes:
		return v.Bytes()
	case value.KindVector:
		return "a vector, which no argument gives: " + v.String()
	}
	return nil
}

// Named types for the destinations and the arguments.
type (
	myString  string
	myInt     int
	myInt8    int8
	myUint16  uint16
	myFloat32 float32
	myFloat   float64
	myBool    bool
	myBytes   []byte
)

// recorder is a sql.Scanner that notes what database/sql hands it.
type recorder struct{ Got string }

func (r *recorder) Scan(src any) error {
	r.Got = describeAny(src)
	return nil
}

// dest makes a destination for a scan, a new one each time, for 0.x or for
// the Beta, which differ only for each package's own Vector.
type dest struct {
	name string
	make func(beta bool) any
}

func newOf[T any]() func(bool) any { return func(bool) any { return new(T) } }

// dests are the destinations database/sql scans into: every type its
// conversions name, the sql.Null types, pointers to pointers, named types
// of each kind, a Scanner, and types it refuses, with a destination that
// isn't a pointer and a nil pointer.
var dests = []dest{
	{"string", newOf[string]()},
	{"[]byte", newOf[[]byte]()},
	{"sql.RawBytes", newOf[sql.RawBytes]()},
	{"bool", newOf[bool]()},
	{"any", newOf[any]()},
	{"int", newOf[int]()},
	{"int8", newOf[int8]()},
	{"int16", newOf[int16]()},
	{"int32", newOf[int32]()},
	{"int64", newOf[int64]()},
	{"uint", newOf[uint]()},
	{"uint8", newOf[uint8]()},
	{"uint16", newOf[uint16]()},
	{"uint32", newOf[uint32]()},
	{"uint64", newOf[uint64]()},
	{"uintptr", newOf[uintptr]()},
	{"float32", newOf[float32]()},
	{"float64", newOf[float64]()},
	{"complex128", newOf[complex128]()},
	{"time.Time", newOf[time.Time]()},
	{"sql.NullString", newOf[sql.NullString]()},
	{"sql.NullInt64", newOf[sql.NullInt64]()},
	{"sql.NullInt32", newOf[sql.NullInt32]()},
	{"sql.NullInt16", newOf[sql.NullInt16]()},
	{"sql.NullByte", newOf[sql.NullByte]()},
	{"sql.NullFloat64", newOf[sql.NullFloat64]()},
	{"sql.NullBool", newOf[sql.NullBool]()},
	{"sql.NullTime", newOf[sql.NullTime]()},
	{"sql.Null[string]", newOf[sql.Null[string]]()},
	{"sql.Null[int64]", newOf[sql.Null[int64]]()},
	{"sql.Null[int8]", newOf[sql.Null[int8]]()},
	{"sql.Null[uint64]", newOf[sql.Null[uint64]]()},
	{"sql.Null[float64]", newOf[sql.Null[float64]]()},
	{"sql.Null[float32]", newOf[sql.Null[float32]]()},
	{"sql.Null[[]byte]", newOf[sql.Null[[]byte]]()},
	{"sql.Null[bool]", newOf[sql.Null[bool]]()},
	{"sql.Null[any]", newOf[sql.Null[any]]()},
	{"sql.Null[time.Time]", newOf[sql.Null[time.Time]]()},
	{"*string", newOf[*string]()},
	{"*int64", newOf[*int64]()},
	{"*float64", newOf[*float64]()},
	{"*[]byte", newOf[*[]byte]()},
	{"*bool", newOf[*bool]()},
	{"*any", newOf[*any]()},
	{"myString", newOf[myString]()},
	{"myInt", newOf[myInt]()},
	{"myInt8", newOf[myInt8]()},
	{"myUint16", newOf[myUint16]()},
	{"myFloat32", newOf[myFloat32]()},
	{"myFloat", newOf[myFloat]()},
	{"myBool", newOf[myBool]()},
	{"myBytes", newOf[myBytes]()},
	{"json.RawMessage", newOf[json.RawMessage]()},
	{"Vector", func(beta bool) any {
		if beta {
			return new(hc.Vector)
		}
		return new(zx.Vector)
	}},
	{"a sql.Scanner", newOf[recorder]()},
	{"a struct", newOf[struct{ X int }]()},
	{"[]string", newOf[[]string]()},
	{"map[string]any", newOf[map[string]any]()},
	{"an int that isn't a pointer", func(bool) any { return 0 }},
	{"a nil *int", func(bool) any { return (*int)(nil) }},
}

// outValue is a value the Beta's engine gives, and an argument from which
// 0.x's SELECT ? gives the same value.
type outValue struct {
	beta value.Value
	zero any
}

// outValues are the kinds of value the Beta gives, near every edge a
// destination has: whole numbers at the ends of each integer type, reals
// with -0, subnormals, infinities and float32's ends, text that reads as
// numbers and booleans or nearly does, with zero bytes and bytes that
// aren't UTF-8, bytes, and vectors, which go out as their bytes.
func outValues() []outValue {
	vs := []outValue{{value.Null(), nil}}
	for _, n := range []int64{0, 1, -1, 2, 127, 128, -128, -129, 255, 256, 32767, 32768, -32768, -32769, 65535, 65536,
		math.MaxInt32, math.MaxInt32 + 1, math.MinInt32, math.MinInt32 - 1, math.MaxUint32, math.MaxUint32 + 1,
		1 << 53, 1<<53 + 1, math.MaxInt64, math.MinInt64} {
		vs = append(vs, outValue{value.Int(n), n})
	}
	for _, f := range []float64{0, math.Copysign(0, -1), 1, -1, 0.5, 1.5, -2.5, 0.1, 3, 255, 256, 1e15, 1e16, 1e20, 1e21,
		1 << 53, 1 << 63, -(1 << 63), 1e-7, 1e-5, 5e-324, 2.2250738585072014e-308, math.MaxFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.MaxFloat32, 3.5e38, 1e-45, 1e-46} {
		vs = append(vs, outValue{value.Real(f), f})
	}
	for _, s := range []string{"", "a", "hello, world", "0", "1", "-1", "2", "12", " 12", "12 ", "+12", "1.5", "-0",
		"-0.0", "1e3", "0x10", "1_000", "true", "false", "t", "f", "TRUE", "True", "yes", "9223372036854775807",
		"9223372036854775808", "-9223372036854775809", "18446744073709551615", "18446744073709551616", "NaN", "Inf",
		"-Inf", "infinity", "3.4028235e38", "1e39", "\x00", "a\x00b", "\xff\xfe", "é", "日本語", "2026-10-08 12:00:00",
		strings.Repeat("x", 1000)} {
		vs = append(vs, outValue{value.Text(s), s})
	}
	for _, s := range []string{"", "\x00", "\x01\x02\xff", "123", "-5", "1.5", "true", "\xff\xfe", "abc"} {
		vs = append(vs, outValue{value.Bytes(s), []byte(s)})
	}
	r := rand.New(rand.NewPCG(3, 4))
	long := make([]float32, 384)
	for i := range long {
		long[i] = r.Float32()*2 - 1
	}
	for _, v := range [][]float32{{1, 2}, {float32(math.Copysign(0, -1)), 1}, {0.5, -0.25, 1},
		{math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32}, {1}, long} {
		vs = append(vs, outValue{value.Vector(v), zx.Vector(v)})
	}
	return vs
}

// scanOne reads the one row of a query's rows into dest through
// Rows.Scan, and describes dest straight after the scan, while a
// sql.RawBytes is still good. It gives the first error of Query, Next,
// Scan and Close.
func scanOne(rows *sql.Rows, err error, dest any) (string, error) {
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no row")
	}
	err = rows.Scan(dest)
	got := describe(reflect.ValueOf(dest))
	if err != nil {
		return got, err
	}
	return got, rows.Close()
}

// TestValuesScanAs0xs scans every kind of value the Beta gives (NULL,
// integers, reals, text, bytes and vectors, the extremes and -0 among them)
// into every kind of destination database/sql takes, through Rows.Scan and
// through Row.Scan, on the Beta and on 0.x side by side. The Beta's value
// comes through the driver from the test's engine, and 0.x's from SELECT ?
// with an argument that makes the same value. Each scan must give the same
// on both: the same result, bit for bit, or the same error, word for word,
// with the column's name.
func TestValuesScanAs0xs(t *testing.T) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	b := open(t, filepath.Join(dir, "beta.hcx"))
	var current value.Value
	hc.UseEngine(b, hc.Engine{Query: func(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
		return []string{"v"}, &table{rows: [][]value.Value{{current}}}, nil
	}})
	counts := map[value.Kind]*[2]int{} // by kind: scans that gave a value, and scans refused
	for _, v := range outValues() {
		current = v.beta
		for _, d := range dests {
			for _, through := range []string{"Rows.Scan", "Row.Scan"} {
				zd, bd := d.make(false), d.make(true)
				var zGot, bGot string
				var zErr, bErr error
				if through == "Rows.Scan" {
					rows, err := z.Query("SELECT ? AS v", v.zero)
					zGot, zErr = scanOne(rows, err, zd)
					rows, err = b.Query("SELECT 1 AS v")
					bGot, bErr = scanOne(rows, err, bd)
				} else {
					zErr = z.QueryRow("SELECT ? AS v", v.zero).Scan(zd)
					zGot = describe(reflect.ValueOf(zd))
					bErr = b.QueryRow("SELECT 1 AS v").Scan(bd)
					bGot = describe(reflect.ValueOf(bd))
				}
				if (bErr == nil) != (zErr == nil) || fmt.Sprint(bErr) != fmt.Sprint(zErr) || bGot != zGot {
					t.Errorf("%s into %s through %s:\nthe Beta gives %s, %v\nand 0.x      %s, %v", v.beta, d.name, through, bGot, bErr, zGot, zErr)
				}
				c := counts[v.beta.Kind()]
				if c == nil {
					c = new([2]int)
					counts[v.beta.Kind()] = c
				}
				if zErr == nil {
					c[0]++
				} else {
					c[1]++
				}
			}
		}
	}
	for _, k := range []value.Kind{value.KindNull, value.KindInt, value.KindReal, value.KindText, value.KindBytes, value.KindVector} {
		if c := counts[k]; c == nil || c[0] < 10 || c[1] < 10 {
			t.Errorf("%s: %v scans gave a value and were refused, where at least 10 of each are wanted", k, c)
		}
	}
	for k, c := range counts {
		t.Logf("%s: %d scans gave a value and %d were refused", k, c[0], c[1])
	}
}

// arg is an argument for both engines: beta and zero differ only for each
// package's own Vector.
type arg struct {
	name       string
	beta, zero any
}

// Valuers and a decimal, for the arguments.
type (
	failingValuer struct{}
	intValuer     struct{}
	valueValuer   struct{ s string }
	decimal       struct{}
)

func (failingValuer) Value() (driver.Value, error) { return nil, errors.New("no value today") }
func (intValuer) Value() (driver.Value, error)     { return 5, nil } // an int, which isn't a driver.Value
func (v valueValuer) Value() (driver.Value, error) { return v.s, nil }

// Decompose makes decimal one of the types database/sql passes on as they
// are, for a driver that takes decimals, which neither engine does.
func (decimal) Decompose([]byte) (byte, bool, []byte, int32) { return 0, false, []byte{1}, 0 }

// arguments are the Go values SQL.md's "Arguments" lists, each kind at its
// edges, with the types it refuses. now is the time to give, with its
// monotonic reading and the local zone. A uint of 2^63 or more wraps round
// to a negative integer, as database/sql converts it, where a uint64 that
// size is refused, on both engines.
func arguments(now time.Time) []arg {
	same := func(name string, v any) arg { return arg{name, v, v} }
	i, s, bs := 7, "pointed at", []byte{4, 5}
	ps := &s
	tm := time.Date(2026, 10, 8, 13, 14, 15, 123456789, time.UTC)
	pv, pz := hc.Vector{3, 4}, zx.Vector{3, 4}
	return []arg{
		same("nil", nil),
		same("int", 0), same("int", -5), same("int", math.MaxInt), same("int", math.MinInt),
		same("int8", int8(-128)), same("int8", int8(127)), same("int16", int16(math.MinInt16)),
		same("int32", int32(math.MinInt32)), same("int64", int64(math.MaxInt64)), same("int64", int64(math.MinInt64)),
		same("uint8", uint8(255)), same("uint16", uint16(math.MaxUint16)), same("uint32", uint32(math.MaxUint32)),
		same("uint", uint(math.MaxInt64)), same("uint", uint(1<<63)), same("uint64", uint64(math.MaxInt64)),
		same("uint64", uint64(1<<63)), same("uint64", uint64(math.MaxUint64)), same("uintptr", uintptr(5)),
		same("bool", true), same("bool", false),
		same("float64", 0.0), same("float64", math.Copysign(0, -1)), same("float64", 1.5), same("float64", 0.1),
		same("float64", 5e-324), same("float64", math.MaxFloat64), same("float64", math.Inf(1)),
		same("float64", math.Inf(-1)), same("float64", math.NaN()),
		same("float64", math.Float64frombits(0x7ff8000000000001)), same("float64", math.Float64frombits(0xfff8000000000000)),
		same("float32", float32(0.1)), same("float32", float32(math.Copysign(0, -1))), same("float32", float32(math.MaxFloat32)),
		same("float32", float32(math.SmallestNonzeroFloat32)), same("float32", float32(math.Inf(1))),
		same("float32", float32(math.NaN())),
		same("string", ""), same("string", "a"), same("string", "\x00"), same("string", "\xff"), same("string", "é\nline"),
		same("[]byte", []byte(nil)), same("[]byte", []byte{}), same("[]byte", []byte{0, 1, 255}),
		same("time.Time", tm), same("time.Time", tm.In(time.FixedZone("India", 5*3600+30*60))),
		same("time.Time", tm.In(time.FixedZone("Pacific", -8*3600))), same("time.Time", tm.In(time.FixedZone("odd", 3661))),
		same("time.Time", time.Time{}), same("time.Time", time.Unix(0, 0).UTC()),
		same("time.Time", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)),
		same("time.Time", time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)), same("time.Time", now),
		{"Vector", hc.Vector{1, 2}, zx.Vector{1, 2}},
		{"Vector", hc.Vector{float32(math.Copysign(0, -1)), float32(math.NaN())}, zx.Vector{float32(math.Copysign(0, -1)), float32(math.NaN())}},
		{"Vector", hc.Vector(nil), zx.Vector(nil)},
		{"*Vector", &pv, &pz},
		{"*Vector", (*hc.Vector)(nil), (*zx.Vector)(nil)},
		same("sql.NullString", sql.NullString{}), same("sql.NullString", sql.NullString{String: "x", Valid: true}),
		same("sql.NullInt64", sql.NullInt64{Int64: -9, Valid: true}),
		same("sql.NullFloat64", sql.NullFloat64{Float64: math.NaN(), Valid: true}),
		same("sql.NullBool", sql.NullBool{Bool: true, Valid: true}), same("sql.NullTime", sql.NullTime{Time: tm, Valid: true}),
		same("sql.Null[string]", sql.Null[string]{V: "y", Valid: true}), same("sql.Null[int8]", sql.Null[int8]{V: -1, Valid: true}),
		same("a Valuer that fails", failingValuer{}), same("a Valuer that gives an int", intValuer{}),
		same("a Valuer", valueValuer{"from Value"}), same("a nil pointer to a Valuer", (*valueValuer)(nil)),
		same("myInt8", myInt8(-3)), same("myString", myString("s")), same("myBytes", myBytes{1, 2}),
		same("myBytes", myBytes(nil)), same("myFloat32", myFloat32(0.5)), same("myBool", myBool(true)),
		same("myUint16", myUint16(7)),
		same("*int", &i), same("*int", (*int)(nil)), same("**string", &ps), same("*[]byte", &bs), same("*time.Time", &tm),
		same("chan", make(chan int)), same("func", func() {}), same("map", map[string]int{}),
		same("struct", struct{ X int }{}), same("[]float32", []float32{1}), same("[]int", []int{1}),
		same("[2]byte", [2]byte{}), same("complex128", complex(1, 2)), same("decimal", decimal{}),
	}
}

// TestArgumentsAre0xs gives both engines each Go value SQL.md's
// "Arguments" lists, and checks the value the Beta's driver hands its
// engine against what 0.x stores for it, in a field through UPDATE and
// back through SELECT ?, which must agree with each other. The Beta's
// engine gives the value back as its row, so the round trip must give
// 0.x's too. A type 0.x refuses, the Beta refuses with the same message,
// which database/sql writes for both, before the statement reaches the
// engine. A decimal is refused by both, in words of their own.
func TestArgumentsAre0xs(t *testing.T) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	ok(t, z.Put("args:1", zx.Fields{"v": nil}))
	b := open(t, filepath.Join(dir, "beta.hcx"))
	var bound []value.Value
	hc.UseEngine(b, hc.Engine{Query: func(_ store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error) {
		bound = slices.Clone(args)
		cols := make([]string, len(args))
		for i := range cols {
			cols[i] = "?"
		}
		return cols, &table{rows: [][]value.Value{args}}, nil
	}})

	taken, refused := 0, 0
	for _, a := range arguments(time.Now()) {
		var zv, stored any
		zErr := z.QueryRow("SELECT ?", a.zero).Scan(&zv)
		_, sErr := z.Exec("UPDATE args SET v = ? WHERE key = 'args:1'", a.zero)
		if sErr == nil {
			ok(t, z.QueryRow("SELECT v FROM args WHERE key = 'args:1'").Scan(&stored))
		}
		want := describeAny(zv)
		if (zErr == nil) != (sErr == nil) || zErr == nil && describeAny(stored) != want {
			t.Fatalf("%s %#v: 0.x gives %s, %v through SELECT ? and stores %s, %v", a.name, a.zero, want, zErr, describeAny(stored), sErr)
		}

		bound = nil
		var bv any
		bErr := b.QueryRow("SELECT ?", a.beta).Scan(&bv)
		if zErr != nil {
			refused++
			switch {
			case bErr == nil:
				t.Errorf("%s %#v: the Beta takes it as %s, and 0.x refuses it: %v", a.name, a.beta, describeAny(bv), zErr)
			case a.name != "decimal" && bErr.Error() != zErr.Error():
				t.Errorf("%s %#v: the Beta refuses it with %q, and 0.x with %q", a.name, a.beta, bErr, zErr)
			case kindOf(bErr) != "error" || kindOf(zErr) != "error":
				t.Errorf("%s %#v: the Beta refuses it with %v, of kind %s, and 0.x with %v, of kind %s", a.name, a.beta, bErr, kindOf(bErr), zErr, kindOf(zErr))
			case bound != nil:
				t.Errorf("%s %#v: an argument refused reached the engine as %v", a.name, a.beta, bound)
			}
			continue
		}
		if bErr != nil || len(bound) != 1 {
			t.Errorf("%s %#v: the Beta gives %v and binds %v, where 0.x gives %s", a.name, a.beta, bErr, bound, want)
			continue
		}
		if got := describeAny(goOf(bound[0])); got != want {
			t.Errorf("%s %#v: the Beta binds %s, and 0.x stores %s", a.name, a.beta, got, want)
		}
		if got := describeAny(bv); got != want {
			t.Errorf("%s %#v: the Beta's round trip gives %s, and 0.x's %s", a.name, a.beta, got, want)
		}
		taken++
	}
	if taken < 50 || refused < 10 {
		t.Errorf("%d arguments were taken and %d refused", taken, refused)
	}
	t.Logf("%d arguments taken and %d refused, as 0.x takes and refuses them", taken, refused)

	// An argument with a name has no mark to fill, where 0.x leaves the
	// mark NULL.
	bound = nil
	var v any = "unchanged"
	err = b.QueryRow("SELECT ?", sql.Named("x", 1)).Scan(&v)
	if err == nil || !strings.Contains(err.Error(), "the argument named x has no mark to fill") || kindOf(err) != "error" || bound != nil {
		t.Errorf("a named argument gives %v, and the engine got %v", err, bound)
	}
	if err := z.QueryRow("SELECT ?", sql.Named("x", 1)).Scan(&v); err != nil || v != nil {
		t.Errorf("0.x gives %#v, %v for a named argument, and the Beta's difference may have gone", v, err)
	}

	// A statement takes exactly as many arguments as ? marks. 0.x ignores
	// extra ones, and refuses too few in words of its own.
	for _, c := range []struct {
		q          string
		args       []any
		want       string
		zeroRefuse bool
	}{
		{"SELECT ?, ?", []any{1}, "hypercrux: the statement takes 2 arguments and was given 1", true},
		{"SELECT ?", nil, "hypercrux: the statement takes 1 argument and was given 0", true},
		{"SELECT ?", []any{1, 2}, "hypercrux: the statement takes 1 argument and was given 2", false},
		{"SELECT 1", []any{1}, "hypercrux: the statement takes 0 arguments and was given 1", false},
	} {
		bound = nil
		_, bErr := b.Query(c.q, c.args...)
		_, zErr := z.Query(c.q, c.args...)
		if bErr == nil || bErr.Error() != c.want || kindOf(bErr) != "error" || bound != nil {
			t.Errorf("%s with %v: the Beta gives %v, where %q is wanted", c.q, c.args, bErr, c.want)
		}
		if (zErr != nil) != c.zeroRefuse || zErr != nil && kindOf(zErr) != "error" {
			t.Errorf("%s with %v: 0.x gives %v", c.q, c.args, zErr)
		}
	}
	// A prepared statement's count is database/sql's to check, on both.
	bs, err := b.SQL().Prepare("SELECT ?")
	ok(t, err)
	defer bs.Close()
	zs, err := z.SQL().Prepare("SELECT ?")
	ok(t, err)
	defer zs.Close()
	for _, n := range []int{0, 2} {
		_, bErr := bs.Query(make([]any, n)...)
		_, zErr := zs.Query(make([]any, n)...)
		if want := fmt.Sprintf("sql: expected 1 arguments, got %d", n); bErr == nil || zErr == nil || bErr.Error() != want || zErr.Error() != want {
			t.Errorf("a prepared statement given %d arguments: the Beta gives %v, and 0.x %v", n, bErr, zErr)
		}
	}
	var n int64
	if err := bs.QueryRow(int8(5)).Scan(&n); err != nil || n != 5 {
		t.Errorf("a prepared statement given its one argument gives %d, %v", n, err)
	}
}

// TestSQLBeginIsRefused checks that SQL().Begin returns an error, by every
// way database/sql has to begin, and that the connection goes back to the
// pool, so a handle of one connection carries on. 0.x's Begin starts a
// transaction, which the Beta leaves to Update.
func TestSQLBeginIsRefused(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "beta.hcx"))
	hc.UseEngine(db, hc.Engine{Query: func(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
		return []string{"v"}, &table{rows: [][]value.Value{{value.Int(1)}}}, nil
	}})
	h := db.SQL()
	h.SetMaxOpenConns(1) // so a connection a refusal kept would hold up what follows
	ctx := context.Background()
	refusals := map[string]error{}
	within(t, func() {
		_, refusals["Begin"] = h.Begin()
		_, refusals["BeginTx"] = h.BeginTx(ctx, nil)
		_, refusals["BeginTx with LevelSerializable and ReadOnly"] = h.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
		c, err := h.Conn(ctx)
		ok(t, err)
		_, refusals["Conn.BeginTx"] = c.BeginTx(ctx, nil)
		ok(t, c.Close())
		var n int
		if err := h.QueryRow("SELECT 1 AS v").Scan(&n); err != nil || n != 1 {
			t.Errorf("after the refusals, a query on the one connection gives %d, %v", n, err)
		}
	})
	want := "hypercrux: SQL().Begin is an unsupported operation in the Beta: a transaction goes through Update"
	for name, err := range refusals {
		if err == nil || err.Error() != want || !errors.Is(err, errors.ErrUnsupported) || kindOf(err) != "error" {
			t.Errorf("%s gives %v, where %q is wanted", name, err, want)
		}
	}
	if len(refusals) != 4 {
		t.Errorf("%d ways to begin were tried", len(refusals))
	}

	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	ztx, err := z.SQL().Begin()
	ok(t, err)
	ok(t, ztx.Rollback())
}

// useNEngine has db run a SELECT as a read of docs:1's field n from the
// Reader the driver hands the engine, and an UPDATE as a put of docs:1's n,
// from its one argument, in the transaction the driver hands it. A DELETE
// puts docs:1's n as -1 and then fails, as a statement does when it breaks
// a rule after its first change.
func useNEngine(db *hc.DB) {
	hc.UseEngine(db, hc.Engine{
		Query: func(r store.Reader, _ *query.Select, _ []value.Value) ([]string, query.Rows, error) {
			v, err := fieldN(r, "docs:1")
			if err != nil {
				return nil, nil, err
			}
			return []string{"n"}, &table{rows: [][]value.Value{{v}}}, nil
		},
		Write: func(tx *store.Tx, s query.Statement, args []value.Value) (int64, error) {
			n := value.Int(-1)
			if len(args) > 0 {
				n = args[0]
			}
			if err := tx.Put("docs:1", []format.Field{{Name: "n", Value: n}}); err != nil {
				return 0, err
			}
			if _, isDelete := s.(*query.Delete); isDelete {
				return 0, fmt.Errorf("%w: a rule broken after the first change", hc.ErrInvalid)
			}
			return 1, nil
		},
	})
}

// readN reads docs:1's n through a Query, as useNEngine gives it, or says
// what went wrong.
func readN(q func(string, ...any) (*sql.Rows, error)) string {
	rows, err := q("SELECT n FROM docs WHERE key = 'docs:1'")
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n any
		if err := rows.Scan(&n); err != nil {
			return "error: " + err.Error()
		}
		got = append(got, fmt.Sprint(n))
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	return strings.Join(got, " ")
}

// TestATxsStatementsRunInItsUpdate carries the transaction in each
// statement's context, as far as G3 can go before G4 runs SQL: a Tx's
// Query, QueryRow and Exec reach the engine with the Update's own
// transaction, so they see its changes, and what Exec writes commits with
// the Update, or goes when the Update rolls back. A statement that fails
// takes back only its own changes, and the Update carries on. Through the
// database, inside the Update, a read goes ahead until the first change
// and then fails at once, a write fails before and after, and a read from
// another goroutine waits for the commit, through the methods and SQL()
// alike. A transaction once its Update has returned refuses every call.
func TestATxsStatementsRunInItsUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beta.hcx")
	db := open(t, path)
	useNEngine(db)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	expect := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s gives %s, where %s is wanted", what, got, want)
		}
	}
	expect("db.Query", readN(db.Query), "1")

	// Through the database, a write is an Update of its own.
	res, err := db.Exec("UPDATE docs SET n = ?", 2)
	ok(t, err)
	if n, err := res.RowsAffected(); n != 1 || err != nil {
		t.Errorf("RowsAffected gives %d, %v", n, err)
	}
	expect("db.Query after db.Exec", readN(db.Query), "2")
	_, err = db.Exec("DELETE FROM docs")
	wantErr(t, err, hc.ErrInvalid)
	expect("db.Query after a db.Exec that failed", readN(db.Query), "2")

	var kept *hc.Tx
	var waiting chan string // a read on another goroutine, once it's started
	inside := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, hc.ErrInsideUpdate) {
			t.Errorf("inside the Update, %s gives %v", what, err)
		}
	}
	within(t, func() {
		err := db.Update(func(tx *hc.Tx) error {
			kept = tx
			expect("db.Query before the first change", readN(db.Query), "2")
			expect("db.SQL().Query before the first change", readN(db.SQL().Query), "2")
			expect("tx.Query before the first change", readN(tx.Query), "2")
			_, err := db.Exec("UPDATE docs SET n = ?", 9)
			inside("db.Exec before the first change", err)
			_, err = db.SQL().Exec("UPDATE docs SET n = ?", 9)
			inside("db.SQL().Exec before the first change", err)

			res, err := tx.Exec("UPDATE docs SET n = ?", 3)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); n != 1 || err != nil {
				t.Errorf("tx.Exec's RowsAffected gives %d, %v", n, err)
			}
			expect("tx.Query after tx.Exec", readN(tx.Query), "3")
			var n int
			if err := tx.QueryRow("SELECT n FROM docs WHERE key = ?", "docs:1").Scan(&n); err != nil || n != 3 {
				t.Errorf("tx.QueryRow gives %d, %v", n, err)
			}
			if f, err := tx.Get("docs:1"); err != nil || f["n"] != int64(3) {
				t.Errorf("tx.Get gives %v, %v after tx.Exec", f, err)
			}

			// After the first change, through the database.
			_, err = db.Query("SELECT n FROM docs")
			inside("db.Query", err)
			inside("db.QueryRow", db.QueryRow("SELECT n FROM docs").Scan(&n))
			_, err = db.Exec("UPDATE docs SET n = ?", 9)
			inside("db.Exec", err)
			_, err = db.SQL().Query("SELECT n FROM docs")
			inside("db.SQL().Query", err)
			_, err = db.SQL().Exec("UPDATE docs SET n = ?", 9)
			inside("db.SQL().Exec", err)
			waiting = make(chan string, 1)
			go func() { waiting <- readN(db.Query) }()
			select {
			case got := <-waiting:
				t.Errorf("on another goroutine, db.Query went ahead after the first change, and gave %s", got)
				waiting <- got
			case <-time.After(50 * time.Millisecond):
			}

			// A statement that fails takes back what it changed, and only
			// that.
			if err := tx.Put("docs:2", hc.Fields{"n": 20}); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM docs"); !errors.Is(err, hc.ErrInvalid) {
				t.Errorf("a tx.Exec that breaks a rule gives %v", err)
			}
			expect("tx.Query after a tx.Exec that failed", readN(tx.Query), "3")
			if f, err := tx.Get("docs:2"); err != nil || f["n"] != int64(20) {
				t.Errorf("a tx.Exec that failed took docs:2 with it: %v, %v", f, err)
			}
			return nil
		})
		if err != nil {
			t.Errorf("the Update gives %v", err)
		}
	})
	if waiting != nil {
		select {
		case got := <-waiting:
			expect("db.Query that waited for the commit", got, "3")
		case <-time.After(10 * time.Second):
			t.Error("a db.Query that waited for the commit is still waiting")
		}
	}
	if t.Failed() {
		return
	}
	expect("db.Query after the commit", readN(db.Query), "3")
	for name, err := range map[string]error{
		"Query":    errOf(kept.Query("SELECT n FROM docs")),
		"QueryRow": kept.QueryRow("SELECT n FROM docs").Scan(new(int)),
		"Exec":     errOf(kept.Exec("UPDATE docs SET n = ?", 4)),
	} {
		if !errors.Is(err, hc.ErrClosed) {
			t.Errorf("tx.%s once its Update returned gives %v", name, err)
		}
	}

	// An Update that rolls back takes its statements' changes with it.
	err = db.Update(func(tx *hc.Tx) error {
		if _, err := tx.Exec("UPDATE docs SET n = ?", 4); err != nil {
			return err
		}
		expect("tx.Query in the Update that rolls back", readN(tx.Query), "4")
		return errRollback
	})
	wantErr(t, err, errRollback)
	expect("db.Query after the rollback", readN(db.Query), "3")

	// What the statements committed is in the file.
	ok(t, db.Close())
	again := open(t, path)
	for key, want := range map[string]int64{"docs:1": 3, "docs:2": 20} {
		if f, err := again.Get(key); err != nil || f["n"] != want {
			t.Errorf("after a reopen, %s gives %v, %v, where n is %d", key, f, err, want)
		}
	}
}

// TestAPoolOfOneHoldsUpNoStatement limits SQL()'s pool to one connection,
// as some programs do with 0.x, and holds that connection with rows left
// open. Then it runs a tx.Exec for each row of a tx.Query, as 0.x runs both
// on its transaction's one connection, and a db.Query while another's rows
// are open, which 0.x's pool of one would wait for. The package's methods
// don't draw on SQL()'s pool, so none of them waits.
func TestAPoolOfOneHoldsUpNoStatement(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "beta.hcx"))
	useNEngine(db)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	db.SQL().SetMaxOpenConns(1)
	held, err := db.SQL().Query("SELECT n FROM docs")
	ok(t, err)
	defer held.Close()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	ok(t, z.Put("docs:1", zx.Fields{"n": 1}))
	z.SQL().SetMaxOpenConns(1)

	// loop runs a tx.Exec for each row of a tx.Query, with the rows open.
	loop := func(query func(string, ...any) (*sql.Rows, error), exec func(string, ...any) (sql.Result, error)) error {
		rows, err := query("SELECT n FROM docs WHERE key = 'docs:1'")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n int64
			if err := rows.Scan(&n); err != nil {
				return err
			}
			if _, err := exec("UPDATE docs SET n = ? WHERE key = 'docs:1'", n+1); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	within(t, func() {
		if err := z.Update(func(tx *zx.Tx) error { return loop(tx.Query, tx.Exec) }); err != nil {
			t.Errorf("on 0.x, the loop gives %v", err)
		}
		if err := db.Update(func(tx *hc.Tx) error { return loop(tx.Query, tx.Exec) }); err != nil {
			t.Errorf("on the Beta, the loop gives %v", err)
		}
		open, err := db.Query("SELECT n FROM docs")
		if err != nil {
			t.Errorf("db.Query gives %v", err)
			return
		}
		defer open.Close()
		if got := readN(db.Query); got != "2" {
			t.Errorf("a db.Query while another's rows are open gives %s", got)
		}
	})
	var n int64
	ok(t, z.QueryRow("SELECT n FROM docs WHERE key = 'docs:1'").Scan(&n))
	if f, err := db.Get("docs:1"); err != nil || f["n"] != n {
		t.Errorf("after the loop, the Beta's docs:1 gives %v, %v, and 0.x's n is %d", f, err, n)
	}
}

// TestSQLErrorsHave0xsKinds gives the same broken rule through every way a
// statement goes in, on 0.x and on the Beta, whose engine gives the error
// task G4's will: a key that isn't text, in an INSERT. Through Exec, on the
// database and on a transaction, both give it wrapping ErrInvalid, with the
// same text. Through SQL(), both give it wrapping nothing, with 0.x's text
// there. Through Query and QueryRow the Beta's kind follows the statement,
// so it's invalid, where 0.x's is a plain error (SQL.md, "Where the Beta
// differs from 0.x"). A syntax error is of kind "error" everywhere, with
// SQLite's words on both, and an error that wraps ErrNotFound, which SQL
// never gives, wraps nothing through SQL() either.
func TestSQLErrorsHave0xsKinds(t *testing.T) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	b := open(t, filepath.Join(dir, "beta.hcx"))
	ok(t, z.Put("docs:1", nil))
	ok(t, b.Put("docs:1", nil))
	notFound := fmt.Errorf("%w: docs:9", hc.ErrNotFound)
	hc.UseEngine(b, hc.Engine{
		Query: func(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
			return nil, nil, notFound
		},
		Write: func(*store.Tx, query.Statement, []value.Value) (int64, error) {
			return 0, fmt.Errorf("%w: keys in table docs are text that starts with docs:, up to 1024 bytes", hc.ErrInvalid)
		},
	})

	// sqlCalls is what a DB, a Tx and SQL()'s handle have in common, on
	// both engines. through runs a statement with Exec, Query and QueryRow
	// on one of them, and ways runs it through every way in on one engine,
	// giving each way's error by its name.
	type sqlCalls interface {
		Exec(string, ...any) (sql.Result, error)
		Query(string, ...any) (*sql.Rows, error)
		QueryRow(string, ...any) *sql.Row
	}
	through := func(h sqlCalls, q string) map[string]error {
		var rowsErr error
		if rows, err := h.Query(q); err != nil {
			rowsErr = err
		} else {
			rows.Close()
		}
		return map[string]error{
			"Exec":     errOf(h.Exec(q)),
			"Query":    rowsErr,
			"QueryRow": h.QueryRow(q).Scan(new(any)),
		}
	}
	ways := func(db sqlCalls, update func(func(sqlCalls)) error, sqlDB *sql.DB, q string) map[string]error {
		got := map[string]error{}
		for name, err := range through(db, q) {
			got["DB."+name] = err
		}
		ok(t, update(func(tx sqlCalls) {
			for name, err := range through(tx, q) {
				got["Tx."+name] = err
			}
		}))
		for name, err := range through(sqlDB, q) {
			got["SQL()."+name] = err
		}
		st, err := sqlDB.Prepare(q)
		if err != nil {
			got["SQL().Prepare"] = err
		} else {
			got["SQL().Prepare, then Exec"] = errOf(st.Exec())
			st.Close()
		}
		return got
	}
	zeroWays := func(q string) map[string]error {
		return ways(z, func(fn func(sqlCalls)) error {
			return z.Update(func(tx *zx.Tx) error { fn(tx); return nil })
		}, z.SQL(), q)
	}
	betaWays := func(q string) map[string]error {
		return ways(b, func(fn func(sqlCalls)) error {
			return b.Update(func(tx *hc.Tx) error { fn(tx); return nil })
		}, b.SQL(), q)
	}

	// The broken rule.
	const bad = "INSERT INTO docs (key) VALUES (7)"
	zGot, bGot := zeroWays(bad), betaWays(bad)
	if len(zGot) != 10 || len(bGot) != 10 {
		t.Fatalf("0.x went %d ways, and the Beta %d", len(zGot), len(bGot))
	}
	for way, zErr := range zGot {
		bErr := bGot[way]
		if zErr == nil || bErr == nil {
			t.Errorf("%s: the Beta gives %v, and 0.x %v", way, bErr, zErr)
			continue
		}
		switch {
		case strings.HasPrefix(way, "SQL()") || strings.HasSuffix(way, "Exec"):
			if kindOf(bErr) != kindOf(zErr) || bErr.Error() != zErr.Error() {
				t.Errorf("%s: the Beta gives %v, of kind %s, and 0.x %v, of kind %s", way, bErr, kindOf(bErr), zErr, kindOf(zErr))
			}
		case kindOf(bErr) != "invalid" || kindOf(zErr) != "error" || bErr.Error() != "hypercrux: invalid: "+strings.TrimPrefix(zErr.Error(), "hypercrux: "):
			t.Errorf("%s: the Beta gives %v, of kind %s, and 0.x %v, of kind %s", way, bErr, kindOf(bErr), zErr, kindOf(zErr))
		}
	}
	if kindOf(bGot["SQL().Exec"]) != "error" || kindOf(bGot["DB.Exec"]) != "invalid" {
		t.Errorf("through SQL(), the Beta gives %v, and through Exec %v", bGot["SQL().Exec"], bGot["DB.Exec"])
	}

	// Syntax errors.
	for _, q := range []string{"SELEC 1", "SELECT 1 +", "SELECT (1", "UPDATE"} {
		zGot, bGot := zeroWays(q), betaWays(q)
		for way, zErr := range zGot {
			if bErr := bGot[way]; zErr == nil || bErr == nil || kindOf(bErr) != "error" || kindOf(zErr) != "error" || bErr.Error() != zErr.Error() {
				t.Errorf("%s through %s: the Beta gives %v, and 0.x %v", q, way, bErr, zErr)
			}
		}
	}

	// Not found, which the engine shouldn't give, wraps nothing through
	// SQL().
	for way, err := range betaWays("SELECT n FROM docs") {
		viaSQL := strings.HasPrefix(way, "SQL()")
		if err == nil || err.Error() != notFound.Error() || errors.Is(err, hc.ErrNotFound) == viaSQL {
			t.Errorf("not found through %s gives %v, of kind %s", way, err, kindOf(err))
		}
	}
}

// TestExecGivesItsCounts checks Exec's result: RowsAffected is the engine's
// count of changed rows for a write and 0 for a SELECT, through the
// database, a transaction and SQL(), and LastInsertId returns an error,
// since records have no row numbers. A write through Query gives no
// columns and no rows, as 0.x's does.
func TestExecGivesItsCounts(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "beta.hcx"))
	hc.UseEngine(db, hc.Engine{
		Query: func(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
			return []string{"v"}, &table{rows: [][]value.Value{{value.Int(1)}, {value.Int(2)}}}, nil
		},
		Write: func(_ *store.Tx, _ query.Statement, args []value.Value) (int64, error) { return args[0].Int(), nil },
	})
	results := map[string]func(string, ...any) (sql.Result, error){"DB.Exec": db.Exec, "SQL().Exec": db.SQL().Exec}
	ok(t, db.Update(func(tx *hc.Tx) error {
		for _, c := range []struct {
			q    string
			args []any
			want int64
		}{
			{"UPDATE docs SET n = 1 WHERE n > ?", []any{3}, 3},
			{"DELETE FROM docs WHERE n > ?", []any{0}, 0},
			{"SELECT n FROM docs", nil, 0},
		} {
			res, err := tx.Exec(c.q, c.args...)
			ok(t, err)
			if n, err := res.RowsAffected(); n != c.want || err != nil {
				t.Errorf("tx.Exec(%q): RowsAffected gives %d, %v", c.q, n, err)
			}
		}
		return nil
	}))
	for name, exec := range results {
		for q, want := range map[string]int64{"INSERT INTO docs (key) VALUES (?), (?)": 2, "SELECT 1": 0} {
			args := []any{}
			if want > 0 {
				args = []any{int64(want), "docs:2"}
			}
			res, err := exec(q, args...)
			ok(t, err)
			if n, err := res.RowsAffected(); n != want || err != nil {
				t.Errorf("%s(%q): RowsAffected gives %d, %v", name, q, n, err)
			}
			id, err := res.LastInsertId()
			if want := "hypercrux: LastInsertId is an unsupported operation in the Beta: records have no row numbers, and a key names each one"; id != 0 || err == nil ||
				err.Error() != want || !errors.Is(err, errors.ErrUnsupported) || kindOf(err) != "error" {
				t.Errorf("%s(%q): LastInsertId gives %d, %v", name, q, id, err)
			}
		}
	}

	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	ok(t, z.Put("docs:1", zx.Fields{"n": 1}))
	queries := map[string]func(string, ...any) (*sql.Rows, error){"the Beta": db.Query, "0.x": z.Query}
	for name, query := range queries {
		rows, err := query("UPDATE docs SET n = ? WHERE key = 'docs:1'", int64(1))
		ok(t, err)
		cols, err := rows.Columns()
		if cols != nil || err != nil || rows.Next() || rows.Err() != nil {
			t.Errorf("on %s, a write through Query gives the columns %#v, %v, and a row or %v", name, cols, err, rows.Err())
		}
		ok(t, rows.Close())
	}
}

// TestRowsAreCopiedOut checks that what Query hands out holds nothing of
// the store's, and was all read before Query returned. A caller that writes
// into the sql.RawBytes a scan gave changes neither the database's bytes
// nor the vector the engine gave. Rows the engine hands over in one buffer
// come out each as it was. The engine's rows were read to the end and
// closed before Query returned, so rows left open hold up no Update, and
// go on giving what the database held when the query ran, even once it's
// closed. A transaction's rows do the same once its Update has ended. An
// error from the engine's rows, or a row of the wrong width, gives no
// rows.
func TestRowsAreCopiedOut(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "beta.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"n": 1, "b": []byte{1, 2, 3}, "vec": hc.Vector{1, 2}}))
	ok(t, db.Put("docs:2", hc.Fields{"n": 2}))
	ok(t, db.Put("docs:3", hc.Fields{"n": 3}))

	// Writing into what a scan gave.
	var handed value.Value
	hc.UseEngine(db, hc.Engine{Query: func(r store.Reader, _ *query.Select, _ []value.Value) ([]string, query.Rows, error) {
		rec, err := r.Get("docs:1")
		if err != nil {
			return nil, nil, err
		}
		tbl, _ := r.Table("docs")
		handed = value.Vector(rec.Vec)
		return []string{"b", "vec"}, &table{rows: [][]value.Value{{rec.Field(slices.Index(tbl.Fields, "b")), handed}}}, nil
	}})
	rows, err := db.Query("SELECT b, vec FROM docs WHERE key = 'docs:1'")
	ok(t, err)
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	var rb, rv sql.RawBytes
	ok(t, rows.Scan(&rb, &rv))
	if !slices.Equal(rb, []byte{1, 2, 3}) || !slices.Equal(rv, hc.Vector{1, 2}.Bytes()) {
		t.Fatalf("the scan gave %x and %x", rb, rv)
	}
	for i := range rb {
		rb[i] = 0xee
	}
	for i := range rv {
		rv[i] = 0xee
	}
	ok(t, rows.Close())
	if f, err := db.Get("docs:1"); err != nil || show(f) != show(hc.Fields{"n": int64(1), "b": []byte{1, 2, 3}, "vec": hc.Vector{1, 2}}) {
		t.Errorf("writing into what a scan gave changed the database: %s, %v", show(f), err)
	}
	if handed.Raw() != string(hc.Vector{1, 2}.Bytes()) {
		t.Errorf("writing into what a scan gave changed the engine's vector: %s", handed)
	}

	// Rows in one buffer, read as the engine went, and closed.
	var given []query.Rows
	hc.UseEngine(db, hc.Engine{Query: func(r store.Reader, s *query.Select, _ []value.Value) ([]string, query.Rows, error) {
		var rs query.Rows = &lazyN{r: r, keys: []string{"docs:1", "docs:2", "docs:3"}}
		if s.Where == nil {
			rs = &table{rows: [][]value.Value{{value.Int(10)}, {value.Text("twenty")}, {value.Real(30)}}}
		}
		given = append(given, rs)
		return []string{"n"}, rs, nil
	}})
	all := func(rows *sql.Rows) string {
		var got []string
		for rows.Next() {
			var v any
			ok(t, rows.Scan(&v))
			got = append(got, describeAny(v))
		}
		ok(t, rows.Err())
		return strings.Join(got, ", ")
	}
	rows, err = db.Query("SELECT n FROM docs")
	ok(t, err)
	if got, want := all(rows), "interface {}(int64 10), interface {}(string \"twenty\"), interface {}(float64 30 (403e000000000000))"; got != want {
		t.Errorf("rows the engine gave in one buffer come out as %s, where %s is wanted", got, want)
	}
	const lazy = "SELECT n FROM docs WHERE n > 0"
	const before = "interface {}(int64 1), interface {}(int64 2), interface {}(int64 3)"
	open1, err := db.Query(lazy)
	ok(t, err)
	if l := given[len(given)-1].(*lazyN); !l.closed || len(l.keys) != 0 {
		t.Errorf("Query returned with the engine's rows open, or with %d keys still to read", len(l.keys))
	}
	within(t, func() { ok(t, db.Put("docs:2", hc.Fields{"n": 22})) })
	var open2 *sql.Rows
	ok(t, db.Update(func(tx *hc.Tx) error {
		var err error
		open2, err = tx.Query(lazy)
		if err != nil {
			return err
		}
		return tx.Put("docs:3", hc.Fields{"n": 33})
	}))
	ok(t, db.Put("docs:1", hc.Fields{"n": 11}))
	ok(t, db.Close())
	if got := all(open1); got != before {
		t.Errorf("rows left open give %s, where %s, from when the query ran, is wanted", got, before)
	}
	if got, want := all(open2), "interface {}(int64 1), interface {}(int64 22), interface {}(int64 3)"; got != want {
		t.Errorf("a transaction's rows left open give %s, where %s, from when the query ran, is wanted", got, want)
	}

	// Errors.
	db = open(t, filepath.Join(t.TempDir(), "beta.hcx"))
	broken := errors.New("the engine's rows broke")
	for name, rs := range map[string]*table{
		"an error after two rows": {rows: [][]value.Value{{value.Int(1)}, {value.Int(2)}}, err: broken},
		"a row of two values":     {rows: [][]value.Value{{value.Int(1)}, {value.Int(1), value.Int(2)}}},
	} {
		hc.UseEngine(db, hc.Engine{Query: func(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
			return []string{"n"}, rs, nil
		}})
		rows, err := db.Query("SELECT n FROM docs")
		if err == nil {
			rows.Close()
		}
		if want := "hypercrux: a row of 2 values for 1 columns"; err == nil || name == "an error after two rows" && err != broken ||
			name == "a row of two values" && err.Error() != want {
			t.Errorf("%s: Query gives %v", name, err)
		}
		if !rs.closed {
			t.Errorf("%s: the engine's rows were left open", name)
		}
	}
}

// TestSQLCallsAfterTheEnd makes the SQL calls too late: on a transaction
// once its Update has returned, and on one nothing made, and on a database
// once it's closed, and on one nothing opened. Each fails with ErrClosed
// before it looks at the statement or its arguments, which here would
// give errors of their own. A closed database's SQL() is the handle it had,
// closed, and a database nothing opened gives a closed one too, so every
// call through them gets database/sql's error for a closed handle, and
// setting one up doesn't panic.
func TestSQLCallsAfterTheEnd(t *testing.T) {
	db, err := hc.Open(filepath.Join(t.TempDir(), "beta.hcx"))
	ok(t, err)
	var kept *hc.Tx
	ok(t, db.Update(func(tx *hc.Tx) error { kept = tx; return nil }))
	bad := make(chan int) // an argument database/sql refuses
	calls := func(exec func(string, ...any) (sql.Result, error), query func(string, ...any) (*sql.Rows, error), row func(string, ...any) *sql.Row) map[string]error {
		return map[string]error{
			"Exec":     errOf(exec("SELEC", bad)),
			"Query":    errOf(query("SELEC", bad)),
			"QueryRow": row("SELEC", bad).Scan(new(int)),
		}
	}
	closed := func(what string, all map[string]error) {
		t.Helper()
		for name, err := range all {
			if !errors.Is(err, hc.ErrClosed) || errors.Is(err, hc.ErrInvalid) || errors.Is(err, hc.ErrNotFound) {
				t.Errorf("%s.%s gives %v", what, name, err)
			}
		}
	}
	closed("an ended Tx", calls(kept.Exec, kept.Query, kept.QueryRow))
	tx := new(hc.Tx)
	closed("a Tx nothing made", calls(tx.Exec, tx.Query, tx.QueryRow))
	h := db.SQL()
	ok(t, db.Close())
	closed("a closed DB", calls(db.Exec, db.Query, db.QueryRow))
	never := new(hc.DB)
	closed("a DB nothing opened", calls(never.Exec, never.Query, never.QueryRow))
	if db.SQL() != h {
		t.Error("a closed database's SQL gives another handle")
	}
	for what, s := range map[string]*sql.DB{"a closed DB": db.SQL(), "a DB nothing opened": never.SQL()} {
		s.SetMaxOpenConns(1)
		_, beginErr := s.Begin()
		for name, err := range map[string]error{"Ping": s.Ping(), "Exec": errOf(s.Exec("DELETE FROM docs")),
			"Query": errOf(s.Query("SELECT 1")), "Begin": beginErr} {
			if err == nil || err.Error() != "sql: database is closed" {
				t.Errorf("%s's SQL().%s gives %v", what, name, err)
			}
		}
	}
}
