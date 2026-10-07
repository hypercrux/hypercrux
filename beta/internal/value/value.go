// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is one of the six kinds of value, in the order of FORMAT.md's table.
// The order says nothing about how SQL sorts values, which is Q1's, by the
// rules P5 sets.
type Kind uint8

const (
	KindNull   Kind = iota // n in the file
	KindInt                // i: a whole number, an int64
	KindReal               // r: a real number, a float64
	KindText               // t
	KindBytes              // b
	KindVector             // v: float32 values
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindInt:
		return "int"
	case KindReal:
		return "real"
	case KindText:
		return "text"
	case KindBytes:
		return "bytes"
	case KindVector:
		return "vector"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Value is one value of any of the six kinds. The zero Value is null.
//
// A Value holds exactly what FORMAT.md stores. A real number keeps its
// bits, so -0 and subnormal numbers come back as they went in. A vector
// keeps the bits of each float32 too, held as FORMAT.md writes them: 4
// bytes a value, little-endian, which is also how 0.x stores a vector in a
// blob. Text and bytes are separate kinds, as in 0.x and its export, even
// when they hold the same bytes. Text may hold bytes that aren't UTF-8,
// since SQL can make such text; the rules that keep stored text valid are
// S1's.
//
// A Value can't be changed once it's made, so it can be copied and shared
// freely. It takes 32 bytes, and making a null or a number allocates
// nothing. Two Values are == when they have the same kind and the same
// bits: -0 and 0 differ, and so do text and bytes holding the same bytes.
// That's identity, for checking that values come through unchanged. SQL
// compares and converts values by the rules P5 sets, which Q1 writes on
// top of this type.
//
// The accessors for a kind's contents panic when the Value is of another
// kind, as an index out of range does: callers look at Kind first.
//
// 0.x's Go values map onto Values as FromGo describes, and Go gives them
// back as 0.x's Get does.
//
// The store keeps a record's vector in its table's vector array (V1): the
// record holds the number of its slot there, and a read hands out that
// slot's float32 values without copying them. A Value of kind KindVector
// carries a vector in a change list and in SQL.
type Value struct {
	kind Kind
	n    uint64 // an Int's bits, or a Real's
	s    string // a Text's or a Bytes' bytes, or a Vector's values as their bits, 4 bytes each, little-endian
}

// Null returns the null value, which is also the zero Value.
func Null() Value { return Value{} }

// Int returns a whole number.
func Int(i int64) Value { return Value{kind: KindInt, n: uint64(i)} }

// Real returns a real number, keeping its bits.
func Real(f float64) Value { return Value{kind: KindReal, n: math.Float64bits(f)} }

// Text returns text holding s.
func Text(s string) Value { return Value{kind: KindText, s: s} }

// Bytes returns bytes holding the bytes of s. A caller with a []byte
// converts it with string(b), which copies it once.
func Bytes(s string) Value { return Value{kind: KindBytes, s: s} }

// Vector returns a vector holding a copy of v's values, keeping their bits.
func Vector(v []float32) Value {
	b := make([]byte, 0, 4*len(v))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(x))
	}
	return Value{kind: KindVector, s: string(b)}
}

// VectorBits returns a vector whose values' bits are in bits, 4 bytes a
// value, little-endian, as FORMAT.md stores them and Raw returns them. It
// panics when the length of bits isn't a multiple of 4.
func VectorBits(bits string) Value {
	if len(bits)%4 != 0 {
		panic(fmt.Sprintf("value: VectorBits given %d bytes, which isn't a multiple of 4", len(bits)))
	}
	return Value{kind: KindVector, s: bits}
}

// Kind returns the value's kind.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether the value is null.
func (v Value) IsNull() bool { return v.kind == KindNull }

func (v Value) must(k Kind, method string) {
	if v.kind != k {
		panic(fmt.Sprintf("value: %s called on a value of kind %s", method, v.kind))
	}
}

// Int returns a whole number's value.
func (v Value) Int() int64 {
	v.must(KindInt, "Int")
	return int64(v.n)
}

// Real returns a real number's value, with its bits.
func (v Value) Real() float64 {
	v.must(KindReal, "Real")
	return math.Float64frombits(v.n)
}

// Text returns text's contents.
func (v Value) Text() string {
	v.must(KindText, "Text")
	return v.s
}

// Bytes returns a copy of the bytes a bytes value holds.
func (v Value) Bytes() []byte {
	v.must(KindBytes, "Bytes")
	return []byte(v.s)
}

// Raw returns what a text, bytes or vector value holds, as a string,
// without copying it: a vector's is its values' bits, 4 bytes a value,
// little-endian. It panics for the other kinds.
func (v Value) Raw() string {
	switch v.kind {
	case KindText, KindBytes, KindVector:
		return v.s
	}
	panic(fmt.Sprintf("value: Raw called on a value of kind %s", v.kind))
}

// Dims returns how many values a vector has.
func (v Value) Dims() int {
	v.must(KindVector, "Dims")
	return len(v.s) / 4
}

// Vector returns a vector's values in a new slice.
func (v Value) Vector() []float32 {
	v.must(KindVector, "Vector")
	return v.AppendVector(make([]float32, 0, len(v.s)/4))
}

// AppendVector appends a vector's values to dst and returns the result,
// as V1 does to put a vector into its table's array.
func (v Value) AppendVector(dst []float32) []float32 {
	v.must(KindVector, "AppendVector")
	s := v.s
	for i := 0; i+4 <= len(s); i += 4 {
		bits := uint32(s[i]) | uint32(s[i+1])<<8 | uint32(s[i+2])<<16 | uint32(s[i+3])<<24
		dst = append(dst, math.Float32frombits(bits))
	}
	return dst
}

// Go returns the value as 0.x's Get gives it: nil, an int64, a float64, a
// string, a new []byte, or a new []float32 for a vector, which the public
// package hands out as its Vector type.
func (v Value) Go() any {
	switch v.kind {
	case KindInt:
		return v.Int()
	case KindReal:
		return v.Real()
	case KindText:
		return v.s
	case KindBytes:
		return v.Bytes()
	case KindVector:
		return v.Vector()
	}
	return nil
}

// String shows the value with its kind, and reals and vector values with
// their bits, as the differential harness shows values, so that -0 and 0,
// or text and bytes, never look the same in a test's output.
func (v Value) String() string {
	switch v.kind {
	case KindNull:
		return "null"
	case KindInt:
		return "int " + strconv.FormatInt(int64(v.n), 10)
	case KindReal:
		return fmt.Sprintf("real %v (%016x)", math.Float64frombits(v.n), v.n)
	case KindText:
		return "text " + strconv.Quote(v.s)
	case KindBytes:
		return "bytes " + hex.EncodeToString([]byte(v.s))
	case KindVector:
		f := v.Vector()
		parts := make([]string, len(f))
		for i, x := range f {
			parts[i] = fmt.Sprintf("%v (%08x)", x, math.Float32bits(x))
		}
		return "vector [" + strings.Join(parts, ", ") + "]"
	}
	return v.kind.String()
}
