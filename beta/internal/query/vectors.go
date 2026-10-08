// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// distance() and vector(), HyperCrux's own functions (SQL.md, "HyperCrux's
// functions"), as 0.x registers them with SQLite, in sqlfuncs.go and
// vectors.go at the repository's root. A vector is bytes, its float32
// values 4 bytes each, little-endian, or text holding a JSON array of
// numbers. The dot products are vecmath's, as the store's Nearest works
// them out, so a search and ORDER BY distance() give the same bits.

// funcError makes the error distance() or vector() gives.
func funcError(e *Call, format string, args ...any) error {
	return &FuncError{Pos: e.At, Func: e.Func(), Msg: fmt.Sprintf(format, args...)}
}

// invalid turns one of rules' errors, which wrap errs.ErrInvalid, into the
// FuncError 0.x gives for it, with its words.
func invalid(e *Call, err error) error {
	return &FuncError{Pos: e.At, Func: e.Func(), Msg: strings.TrimPrefix(err.Error(), "hypercrux: ")}
}

// parseVector is 0.x's ParseVector: text read by encoding/json into float64
// values, each made a float32, and checked as a vector.
func parseVector(e *Call, s string) ([]float32, error) {
	var f []float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, funcError(e, "invalid: a vector is a JSON array of numbers: %v", err)
	}
	vals := make([]float32, len(f))
	for i, x := range f {
		vals[i] = float32(x)
	}
	if err := rules.Vector(vals); err != nil {
		return nil, invalid(e, err)
	}
	return vals, nil
}

// vectorBits gives float32 values as the bytes of a vector.
func vectorBits(vals []float32) string {
	b := make([]byte, 4*len(vals))
	for i, x := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return string(b)
}

// decodeVector puts the float32 values of a vector's bytes into dst, which
// has room for them all.
func decodeVector(dst []float32, raw string) []float32 {
	for i := range dst {
		j := 4 * i
		dst[i] = math.Float32frombits(uint32(raw[j]) | uint32(raw[j+1])<<8 | uint32(raw[j+2])<<16 | uint32(raw[j+3])<<24)
	}
	return dst
}

// vectorValue is 0.x's vector(): NULL for NULL; for text, a JSON array of
// numbers, as bytes; for bytes, whole float32 values, given back unchanged.
// Either way it needs 1 to 65,536 values, all finite and not all zero.
func vectorValue(e *Call, v value.Value) (value.Value, error) {
	switch kind(v) {
	case value.KindNull:
		return null, nil
	case value.KindText:
		vals, err := parseVector(e, v.Raw())
		if err != nil {
			return null, err
		}
		return value.Bytes(vectorBits(vals)), nil
	case value.KindBytes:
		raw := v.Raw()
		if len(raw) == 0 || len(raw)%4 != 0 {
			return null, funcError(e, "a vector blob of %d bytes isn't whole float32 values", len(raw))
		}
		if err := rules.Vector(decodeVector(make([]float32, len(raw)/4), raw)); err != nil {
			return null, invalid(e, err)
		}
		return value.Bytes(raw), nil
	}
	return null, funcError(e, "vector() takes a JSON array, not %s", goType(v))
}

// goType names a value's type as 0.x's messages do, by the Go type
// go-sqlite3 hands a function.
func goType(v value.Value) string {
	if kind(v) == value.KindInt {
		return "int64"
	}
	return "float64"
}

// A vecCache keeps the last text one side of a distance() call read, with
// its vector, since a query compares the same text with every row, as 0.x's
// vectorArg keeps it. Rows on several goroutines may share one.
type vecCache struct {
	last atomic.Pointer[cachedVector]
}

type cachedVector struct {
	text string
	vals []float32
}

// floats lends buffers for the values of a side given as bytes.
var floats = sync.Pool{New: func() any { return new([]float32) }}

// vectorSide reads a side of distance() as 0.x's vectorArg does: bytes as
// they are, as long as they're whole float32 values, and text as vector()
// reads it. done gives back what it lent.
func vectorSide(e *Call, v value.Value, cache *vecCache) (vals []float32, done func(), err error) {
	switch kind(v) {
	case value.KindBytes:
		raw := v.Raw()
		buf := floats.Get().(*[]float32)
		if cap(*buf) < len(raw)/4 {
			*buf = make([]float32, len(raw)/4)
		}
		return decodeVector((*buf)[:len(raw)/4], raw), func() { floats.Put(buf) }, nil
	case value.KindText:
		s := v.Raw()
		if c := cache.last.Load(); c != nil && c.text == s {
			return c.vals, func() {}, nil
		}
		vals, err := parseVector(e, s)
		if err != nil {
			return nil, nil, err
		}
		cache.last.Store(&cachedVector{text: s, vals: vals})
		return vals, func() {}, nil
	}
	return nil, nil, funcError(e, "a vector is a blob or a JSON array, not %s", goType(v))
}

// distanceValue is 0.x's distance(): NULL when either side is NULL, and
// otherwise the cosine distance, from 0 to 2, and 1 when either side is
// bytes whose values are all zero. Sides of different lengths, bytes that
// aren't whole float32 values, an empty side and a side holding NaN or
// infinity are errors, as anything but bytes and text is.
func distanceValue(e *Call, a, b value.Value, ca, cb *vecCache) (value.Value, error) {
	if a.IsNull() || b.IsNull() {
		return null, nil
	}
	x, doneX, err := vectorSide(e, a, ca)
	if err != nil {
		return null, err
	}
	defer doneX()
	y, doneY, err := vectorSide(e, b, cb)
	if err != nil {
		return null, err
	}
	defer doneY()
	// 0.x checks the bytes: the lengths first, then whole float32 values.
	na, nb := 4*len(x), 4*len(y)
	if kind(a) == value.KindBytes {
		na = len(a.Raw())
	}
	if kind(b) == value.KindBytes {
		nb = len(b.Raw())
	}
	if na != nb {
		return null, funcError(e, "distance between vectors of %d and %d values", na/4, nb/4)
	}
	if na == 0 || na%4 != 0 {
		return null, funcError(e, "a vector blob of %d bytes isn't whole float32 values", na)
	}
	xx, yy := vecmath.Dot(x, x), vecmath.Dot(y, y)
	if math.IsNaN(xx) || math.IsInf(xx, 0) || math.IsNaN(yy) || math.IsInf(yy, 0) {
		return null, funcError(e, "a vector holds a value that isn't a finite number")
	}
	return value.Real(vecmath.Distance(vecmath.Dot(x, y), math.Sqrt(xx), math.Sqrt(yy))), nil
}
