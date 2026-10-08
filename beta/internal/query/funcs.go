// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The text and number functions of SQL.md's "Functions", each SQLite's
// routine of the same name in sqlite3-binding.c, and coalesce() and
// ifnull(), which SQLite's code generator works out itself. A call works
// out all its arguments, in order, and then the function, apart from
// coalesce() and ifnull(), which stop at the first argument that isn't
// NULL.

// call compiles a call. The parser has checked the function's name and its
// number of arguments.
func (c *compiler) call(e *Call) (Eval, error) {
	name := e.Func()
	args := make([]Eval, len(e.Args))
	for i, a := range e.Args {
		var err error
		if args[i], err = c.expr(a); err != nil {
			return nil, err
		}
	}
	switch {
	case e.Aggregate(), name == "walk", name == "date", name == "datetime":
		if c.scope == nil {
			return nil, fail(e, "%s() needs the statement's planner", name)
		}
		return c.scope.Call(e, args)
	}
	switch name {
	case "coalesce", "ifnull":
		return coalesce(args), nil
	case "abs":
		return one(args[0], func(v value.Value) (value.Value, error) { return absValue(e, v) }), nil
	case "length":
		return one(args[0], lengthValue), nil
	case "lower":
		return one(args[0], func(v value.Value) (value.Value, error) { return mapBytes(v, lowerByte), nil }), nil
	case "upper":
		return one(args[0], func(v value.Value) (value.Value, error) { return mapBytes(v, upperByte), nil }), nil
	case "typeof":
		return one(args[0], typeofValue), nil
	case "instr":
		return two(args[0], args[1], instrValue), nil
	case "nullif":
		return two(args[0], args[1], nullifValue), nil
	case "max", "min":
		pick := minMax(name == "max")
		if len(args) == 2 {
			return two(args[0], args[1], func(a, b value.Value) (value.Value, error) {
				return pick([]value.Value{a, b}), nil
			}), nil
		}
		return many(args, func(vs []value.Value) (value.Value, error) { return pick(vs), nil }), nil
	case "replace":
		return three(args[0], args[1], args[2], func(x, y, z value.Value) (value.Value, error) {
			return replaceValue(e, x, y, z)
		}), nil
	case "round":
		if len(args) == 1 {
			return one(args[0], func(x value.Value) (value.Value, error) { return roundValue(x, value.Int(0), false), nil }), nil
		}
		return two(args[0], args[1], func(x, n value.Value) (value.Value, error) { return roundValue(x, n, true), nil }), nil
	case "substr":
		if len(args) == 2 {
			return two(args[0], args[1], func(x, y value.Value) (value.Value, error) { return substrValue(x, y, null, false), nil }), nil
		}
		return three(args[0], args[1], args[2], func(x, y, z value.Value) (value.Value, error) {
			return substrValue(x, y, z, true), nil
		}), nil
	case "trim":
		if len(args) == 1 {
			return one(args[0], func(x value.Value) (value.Value, error) { return trimValue(x, null, false), nil }), nil
		}
		return two(args[0], args[1], func(x, y value.Value) (value.Value, error) { return trimValue(x, y, true), nil }), nil
	case "distance":
		var ca, cb vecCache
		return two(args[0], args[1], func(a, b value.Value) (value.Value, error) {
			return distanceValue(e, a, b, &ca, &cb)
		}), nil
	case "vector":
		return one(args[0], func(v value.Value) (value.Value, error) { return vectorValue(e, v) }), nil
	}
	return nil, fail(e, "no such function: %s", e.Name.Name)
}

func one(a Eval, fn func(value.Value) (value.Value, error)) Eval {
	return func(f *Frame) (value.Value, error) {
		x, err := a(f)
		if err != nil {
			return null, err
		}
		return fn(x)
	}
}

func two(a, b Eval, fn func(x, y value.Value) (value.Value, error)) Eval {
	return func(f *Frame) (value.Value, error) {
		x, err := a(f)
		if err != nil {
			return null, err
		}
		y, err := b(f)
		if err != nil {
			return null, err
		}
		return fn(x, y)
	}
}

func three(a, b, c Eval, fn func(x, y, z value.Value) (value.Value, error)) Eval {
	return func(f *Frame) (value.Value, error) {
		x, err := a(f)
		if err != nil {
			return null, err
		}
		y, err := b(f)
		if err != nil {
			return null, err
		}
		z, err := c(f)
		if err != nil {
			return null, err
		}
		return fn(x, y, z)
	}
}

func many(args []Eval, fn func([]value.Value) (value.Value, error)) Eval {
	return func(f *Frame) (value.Value, error) {
		vs := make([]value.Value, len(args))
		for i, a := range args {
			var err error
			if vs[i], err = a(f); err != nil {
				return null, err
			}
		}
		return fn(vs)
	}
}

// coalesce gives the first argument that isn't NULL, without working out
// the ones after it, or the last, which is NULL.
func coalesce(args []Eval) Eval {
	return func(f *Frame) (value.Value, error) {
		var v value.Value
		for _, a := range args {
			var err error
			if v, err = a(f); err != nil || !v.IsNull() {
				return v, err
			}
		}
		return v, nil
	}
}

// absValue is absFunc: NULL for NULL, a whole number's absolute value, an
// error for the smallest one, and for anything else the absolute value of
// it read as a real, which leaves -0.0 as it is.
func absValue(e *Call, v value.Value) (value.Value, error) {
	switch kind(v) {
	case value.KindInt:
		i := v.Int()
		if i < 0 {
			if i == -1<<63 {
				return null, fail(e, "integer overflow")
			}
			i = -i
		}
		return value.Int(i), nil
	case value.KindNull:
		return null, nil
	}
	r := realOf(v)
	if r < 0 {
		r = -r
	}
	return value.Real(r), nil
}

// skipChar is SQLITE_SKIP_UTF8: the byte after the character that starts
// at i. A byte from 0xc0 up takes every continuation byte after it.
func skipChar(s string, i int) int {
	c := s[i]
	i++
	if c >= 0xc0 {
		for i < len(s) && s[i]&0xc0 == 0x80 {
			i++
		}
	}
	return i
}

// charsBeforeNUL counts the characters of s before its first NUL byte, as
// SQLITE_SKIP_UTF8 steps through them.
func charsBeforeNUL(s string) int {
	n := 0
	for i := 0; i < len(s) && s[i] != 0; i = skipChar(s, i) {
		n++
	}
	return n
}

// lengthValue is lengthFunc: the bytes of bytes, the characters of text
// before its first NUL byte, and the length of a number's text.
func lengthValue(v value.Value) (value.Value, error) {
	switch kind(v) {
	case value.KindBytes:
		return value.Int(int64(len(v.Raw()))), nil
	case value.KindInt, value.KindReal:
		s, _ := textOf(v)
		return value.Int(int64(len(s))), nil
	case value.KindText:
		return value.Int(int64(charsBeforeNUL(v.Raw()))), nil
	}
	return null, nil
}

func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func upperByte(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// mapBytes is lowerFunc and upperFunc: v as text, every byte through to,
// which changes only ASCII letters. NULL stays NULL.
func mapBytes(v value.Value, to func(byte) byte) value.Value {
	s, ok := textOf(v)
	if !ok {
		return null
	}
	b := []byte(s)
	for i, c := range b {
		b[i] = to(c)
	}
	return value.Text(string(b))
}

// typeofValue is typeofFunc.
func typeofValue(v value.Value) (value.Value, error) {
	switch kind(v) {
	case value.KindNull:
		return value.Text("null"), nil
	case value.KindInt:
		return value.Text("integer"), nil
	case value.KindReal:
		return value.Text("real"), nil
	case value.KindText:
		return value.Text("text"), nil
	}
	return value.Text("blob"), nil
}

// instrValue is instrFunc: where y first comes in x, counting from 1, or 0.
// It counts bytes when both are bytes, and characters otherwise, taking
// numbers and bytes as text. An empty y gives 1.
func instrValue(x, y value.Value) (value.Value, error) {
	kx, ky := kind(x), kind(y)
	if kx == value.KindNull || ky == value.KindNull {
		return null, nil
	}
	hay, _ := textOf(x)
	needle, _ := textOf(y)
	n := 1
	if len(needle) > 0 {
		isText := kx != value.KindBytes || ky != value.KindBytes
		h, left := 0, len(hay)
		for len(needle) <= left && (hay[h] != needle[0] || hay[h:h+len(needle)] != needle) {
			n++
			for {
				left--
				h++
				if !isText || h >= len(hay) || hay[h]&0xc0 != 0x80 {
					break
				}
			}
		}
		if len(needle) > left {
			n = 0
		}
	}
	return value.Int(int64(n)), nil
}

// nullifValue is nullifFunc: NULL when x and y are equal in the order of
// compare, two NULLs included, and x otherwise.
func nullifValue(x, y value.Value) (value.Value, error) {
	if compare(x, y) != 0 {
		return x, nil
	}
	return null, nil
}

// minMax is minmaxFunc: NULL when any argument is NULL, and otherwise the
// largest for max() or the smallest for min(), given back as it is. On a
// tie, max() keeps the earliest of the tied arguments, and min() the
// latest.
func minMax(isMax bool) func(vs []value.Value) value.Value {
	return func(vs []value.Value) value.Value {
		if vs[0].IsNull() {
			return null
		}
		best := 0
		for i := 1; i < len(vs); i++ {
			if vs[i].IsNull() {
				return null
			}
			c := compare(vs[best], vs[i])
			if isMax && c < 0 || !isMax && (c >= 0 && plant != "query/min-keeps-earliest" || c > 0) {
				best = i
			}
		}
		return vs[best]
	}
}

// replaceValue is replaceFunc: x as text with every y replaced by z, byte
// for byte. NULL when x or y is NULL; x as text when y is empty or starts
// with a NUL byte, even when z is NULL; NULL when z is NULL.
func replaceValue(e *Call, x, y, z value.Value) (value.Value, error) {
	s, ok := textOf(x)
	if !ok {
		return null, nil
	}
	p, ok := textOf(y)
	if !ok {
		return null, nil
	}
	if p == "" || p[0] == 0 {
		return value.Text(s), nil
	}
	r, ok := textOf(z)
	if !ok {
		return null, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	size := int64(len(s)) + 1
	i := 0
	for ; i <= len(s)-len(p); i++ {
		if s[i] != p[0] || s[i:i+len(p)] != p {
			b.WriteByte(s[i])
			continue
		}
		if len(r) > len(p) {
			size += int64(len(r) - len(p))
			if size-1 > maxLength {
				return null, fail(e, "string or blob too big")
			}
		}
		b.WriteString(r)
		i += len(p) - 1
	}
	b.WriteString(s[i:])
	return value.Text(b.String()), nil
}

// roundValue is roundFunc: x read as a real, rounded to n places, with n
// clamped to 0 to 30 and 0 when it's left out. A real more than 2^52 in
// size stays as it is. With no places, a half is added or taken away and
// the result cut to an integer, so halves go away from zero; with places,
// the rounding is that of value.RealPlaces read back.
func roundValue(x, n value.Value, hasN bool) value.Value {
	places := int64(0)
	if hasN {
		if n.IsNull() {
			return null
		}
		places = min(max(intOf(n), 0), 30)
	}
	if x.IsNull() {
		return null
	}
	r := realOf(x)
	switch {
	case r < -4503599627370496.0 || r > 4503599627370496.0:
	case places == 0:
		h := 0.5
		if r < 0 {
			h = -0.5
		}
		r = float64(int64(float64(r + h)))
	default:
		r, _ = value.ParseReal(value.RealPlaces(r, int(places)))
	}
	return value.Real(r)
}

// substrValue is substrFunc: z characters of x's text, or z bytes of bytes,
// from position y, counting from 1, and to the end without z. A negative y
// counts from the end, a negative z takes the characters before y, and y of
// 0 takes one fewer than y of 1 would. Text stops at a NUL byte. NULL when
// x, y or z is NULL, and when x is empty bytes.
func substrValue(x, y, z value.Value, hasZ bool) value.Value {
	isBytes := kind(x) == value.KindBytes
	p1 := intOf(y)
	var s string
	var size int64
	if isBytes {
		s = x.Raw()
		if s == "" {
			return null
		}
		size = int64(len(s))
	} else {
		var ok bool
		if s, ok = textOf(x); !ok {
			return null
		}
		if p1 < 0 {
			size = int64(charsBeforeNUL(s))
		}
	}
	var p2 int64
	if hasZ {
		p2 = intOf(z)
		if p2 == 0 && z.IsNull() {
			return null
		}
	} else {
		p2 = maxLength
	}
	if p1 == 0 && y.IsNull() {
		return null
	}
	switch {
	case p1 < 0:
		p1 += size
		if p1 < 0 {
			if p2 < 0 {
				p2 = 0
			} else {
				p2 += p1
			}
			p1 = 0
		}
	case p1 > 0:
		p1--
	case p2 > 0 && plant != "query/substr-zero-is-one":
		p2--
	}
	if p2 < 0 {
		if p2 < -p1 {
			p2 = p1
		} else {
			p2 = -p2
		}
		p1 -= p2
	}
	if !isBytes {
		i := 0
		for ; i < len(s) && s[i] != 0 && p1 > 0; p1-- {
			i = skipChar(s, i)
		}
		j := i
		for ; j < len(s) && s[j] != 0 && p2 > 0; p2-- {
			j = skipChar(s, j)
		}
		return value.Text(s[i:j])
	}
	if p1 >= size {
		p1, p2 = 0, 0
	} else if p2 > size-p1 {
		p2 = size - p1
	}
	return value.Bytes(s[p1 : p1+p2])
}

// trimValue is trimFunc: x as text with any run of y's characters taken
// off both ends, and spaces when y is left out. y's characters are those
// before its first NUL byte. NULL when x or y is NULL.
func trimValue(x, y value.Value, hasY bool) value.Value {
	if x.IsNull() {
		return null
	}
	s, _ := textOf(x)
	set := []string{" "}
	if hasY {
		cs, ok := textOf(y)
		if !ok {
			return null
		}
		set = set[:0]
		for i := 0; i < len(cs) && cs[i] != 0; {
			j := skipChar(cs, i)
			set = append(set, cs[i:j])
			i = j
		}
	}
	if len(set) > 0 {
	left:
		for len(s) > 0 {
			for _, c := range set {
				if strings.HasPrefix(s, c) {
					s = s[len(c):]
					continue left
				}
			}
			break
		}
	right:
		for len(s) > 0 {
			for _, c := range set {
				if strings.HasSuffix(s, c) {
					s = s[:len(s)-len(c)]
					continue right
				}
			}
			break
		}
	}
	return value.Text(s)
}
