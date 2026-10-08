// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"math"
	"strconv"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// SQL's rules for values, from SQL.md's "Values": how each kind reads as
// another, how values compare, truth and arithmetic. Each is SQLite
// 3.53.4's routine, named in its comment, as go-sqlite3 v1.14.52's
// sqlite3-binding.c has it.
//
// A value of kind KindVector is a record's vector, which SQL reads as bytes
// (SQL.md, "Vectors"), so these rules take it as bytes wherever they look
// at a value's kind.

// kind returns the kind of value SQL sees: a vector is bytes.
func kind(v value.Value) value.Kind {
	if k := v.Kind(); k != value.KindVector {
		return k
	}
	return value.KindBytes
}

var null = value.Value{}

// boolValue is 1 for true and 0 for false.
func boolValue(b bool) value.Value {
	if b {
		return value.Int(1)
	}
	return value.Int(0)
}

// textOf returns the text SQL reads a value as, as sqlite3_value_text gives
// it: a whole number in decimal, a real as value.RealText writes it, and
// text and bytes as they are. It reports false for NULL.
func textOf(v value.Value) (string, bool) {
	switch kind(v) {
	case value.KindInt:
		return strconv.FormatInt(v.Int(), 10), true
	case value.KindReal:
		if plant == "query/real-text-shortest" {
			return strconv.FormatFloat(v.Real(), 'g', -1, 64), true
		}
		return value.RealText(v.Real()), true
	case value.KindText, value.KindBytes:
		return v.Raw(), true
	}
	return "", false
}

// realOf is sqlite3VdbeRealValue: a value read as a real. Text and bytes
// read as value.ParseReal reads them, and NULL is 0.
func realOf(v value.Value) float64 {
	switch kind(v) {
	case value.KindReal:
		return v.Real()
	case value.KindInt:
		return float64(v.Int())
	case value.KindText, value.KindBytes:
		r, _ := value.ParseReal(v.Raw())
		return r
	}
	return 0
}

// intOf is sqlite3VdbeIntValue: a value read as a whole number. A real is
// cut toward zero and clamped to 64 bits, text and bytes read as
// value.ParseInt reads them, and NULL is 0.
func intOf(v value.Value) int64 {
	switch kind(v) {
	case value.KindInt:
		return v.Int()
	case value.KindReal:
		return realToInt(v.Real())
	case value.KindText, value.KindBytes:
		i, _ := value.ParseInt(v.Raw())
		return i
	}
	return 0
}

// realToInt is sqlite3RealToI64: r cut toward zero, clamped to 64 bits.
func realToInt(r float64) int64 {
	switch {
	case r < -9223372036854774784.0:
		return math.MinInt64
	case r > 9223372036854774784.0:
		return math.MaxInt64
	}
	return int64(r)
}

// realSameAsInt is sqlite3RealSameAsInt: whether r, a real made from the
// whole number i, is i exactly and less than 2^51 in size. Zero counts,
// -0 included.
func realSameAsInt(r float64, i int64) bool {
	return r == 0 || math.Float64bits(r) == math.Float64bits(float64(i)) && -2251799813685248 <= i && i < 2251799813685248
}

// numericType is computeNumericType, how arithmetic reads text and bytes:
// the longest start that reads as a number, after leading spaces, as a
// whole number when it's written as one that fits in 64 bits, and the
// integer 0 when nothing reads. It reports false for a number that's a real,
// whose value arithmetic then takes from realOf.
func numericType(v value.Value) (int64, bool) {
	switch kind(v) {
	case value.KindInt:
		return v.Int(), true
	case value.KindReal:
		return 0, false
	}
	raw := v.Raw()
	_, rc := value.ParseReal(raw)
	if rc&value.RealPoint != 0 {
		return 0, false
	}
	i, c := value.ParseInt(raw)
	if rc <= 0 && c <= 1 || rc > 0 && c == 0 {
		return i, true
	}
	return 0, false
}

// numericAffinity is applyNumericAffinity, without its try for an integer:
// text that all reads as a number, apart from spaces at either end,
// becomes that number, a whole number when it's written as one, and any
// other value stays as it is. Comparisons under a numeric affinity take
// their operands this way.
func numericAffinity(v value.Value) value.Value {
	if v.Kind() != value.KindText {
		return v
	}
	raw := v.Raw()
	r, rc := value.ParseReal(raw)
	if rc <= 0 {
		return v
	}
	if rc&value.RealPoint == 0 {
		if i := realToInt(r); realSameAsInt(r, i) {
			return value.Int(i)
		}
		if i, c := value.ParseInt(raw); c == 0 {
			return value.Int(i)
		}
	}
	return value.Real(r)
}

// numerify is sqlite3VdbeMemNumerify, CAST to NUMERIC of text or bytes:
// the longest start that reads as a number, a whole number when it's
// written as one that fits, or when its value is whole and less than 2^51
// in size, and a real otherwise.
func numerify(raw string) value.Value {
	r, rc := value.ParseReal(raw)
	if rc&value.RealPoint == 0 {
		if i, c := value.ParseInt(raw); c < 2 {
			return value.Int(i)
		}
	}
	if i := realToInt(r); realSameAsInt(r, i) {
		return value.Int(i)
	}
	return value.Real(r)
}

// truth is sqlite3VdbeBooleanValue: 1 for true, 0 for false and 2 for NULL,
// which is unknown. A number is true when it isn't zero, and text and bytes
// are read as a real first, so 'a' is false and '1' is true.
func truth(v value.Value) int {
	switch kind(v) {
	case value.KindInt:
		if v.Int() != 0 {
			return 1
		}
		return 0
	case value.KindNull:
		return 2
	}
	if realOf(v) != 0 {
		return 1
	}
	return 0
}

// truthValue gives a truth back as a value: 1, 0 or NULL.
func truthValue(t int) value.Value {
	if t == 2 {
		return null
	}
	return value.Int(int64(t))
}

// andTruth and orTruth are OP_And's and OP_Or's tables, on truths.
var (
	andTruth = [9]int{0, 0, 0, 0, 1, 2, 0, 2, 2}
	orTruth  = [9]int{0, 1, 2, 1, 1, 1, 2, 1, 2}
)

// compare is sqlite3MemCompare with SQLite's BINARY collation: negative,
// zero or positive as a comes before b, ties with it or comes after it.
// NULL comes first, then numbers by value, then text, then bytes, and text
// and bytes compare byte by byte. Integers and reals compare exactly.
func compare(a, b value.Value) int {
	ka, kb := kind(a), kind(b)
	if ka == value.KindNull || kb == value.KindNull {
		switch {
		case ka == kb:
			return 0
		case ka == value.KindNull:
			return -1
		}
		return 1
	}
	switch {
	case ka == value.KindInt && kb == value.KindInt:
		return cmpInt(a.Int(), b.Int())
	case ka == value.KindReal && kb == value.KindReal:
		x, y := a.Real(), b.Real()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case ka == value.KindInt:
		if kb == value.KindReal {
			return intRealCompare(a.Int(), b.Real())
		}
		return -1
	case ka == value.KindReal:
		if kb == value.KindInt {
			return -intRealCompare(b.Int(), a.Real())
		}
		return -1
	case kb == value.KindInt || kb == value.KindReal:
		return 1
	case ka == value.KindText && kb != value.KindText:
		return -1
	case ka != value.KindText && kb == value.KindText:
		return 1
	}
	return strings.Compare(a.Raw(), b.Raw())
}

func cmpInt(x, y int64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// intRealCompare is sqlite3IntFloatCompare: i against r, exactly, without
// rounding either to the other.
func intRealCompare(i int64, r float64) int {
	switch {
	case math.IsNaN(r):
		return 1
	case r < -9223372036854775808.0:
		return 1
	case r >= 9223372036854775808.0:
		return -1
	}
	if c := cmpInt(i, int64(r)); c != 0 {
		return c
	}
	switch f := float64(i); {
	case f < r:
		return -1
	case f > r:
		return 1
	}
	return 0
}

// An affinity is SQLite's: a column's or a CAST's type, which a comparison
// brings to its operands (SQL.md, "Conversion before comparing").
type affinity byte

const (
	affNone    affinity = 0x40 // SQLITE_AFF_NONE, with no conversion; 0 means the same for an expression
	affBlob    affinity = 'A'  // a field, or CAST to BLOB, which convert nothing
	affText    affinity = 'B'  // CAST to TEXT
	affNumeric affinity = 'C'  // CAST to NUMERIC
	affInteger affinity = 'D'  // CAST to INTEGER
	affReal    affinity = 'E'  // CAST to REAL
)

// compareAffinity is sqlite3CompareAffinity: the affinity a comparison of
// operands with the affinities a and b applies to them. When both have one,
// it's numeric if either is, and none otherwise. When one has, it's that
// one's.
func compareAffinity(a, b affinity) affinity {
	if a > affNone && b > affNone {
		if a >= affNumeric || b >= affNumeric {
			return affNumeric
		}
		return affBlob
	}
	if a <= affNone {
		return b | affNone
	}
	return a | affNone
}

// withAffinity applies a comparison's affinity to its operands, as OP_Eq
// and the other comparisons do: a numeric affinity turns text that reads as
// a number into it, and a text affinity turns numbers into text, each only
// when one of the operands is text. Neither may be NULL.
func withAffinity(a, b value.Value, aff affinity) (value.Value, value.Value) {
	ta, tb := a.Kind() == value.KindText, b.Kind() == value.KindText
	if !ta && !tb {
		return a, b
	}
	switch {
	case aff >= affNumeric:
		return numericAffinity(a), numericAffinity(b)
	case aff == affText:
		if !ta && (a.Kind() == value.KindInt || a.Kind() == value.KindReal) {
			s, _ := textOf(a)
			a = value.Text(s)
		}
		if !tb && (b.Kind() == value.KindInt || b.Kind() == value.KindReal) {
			s, _ := textOf(b)
			b = value.Text(s)
		}
	}
	return a, b
}

// compareWith compares a and b under a comparison's affinity, as OP_Eq and
// the others do, and reports false when either is NULL, which makes the
// comparison NULL.
func compareWith(a, b value.Value, aff affinity) (int, bool) {
	if a.Kind() == value.KindInt && b.Kind() == value.KindInt {
		return cmpInt(a.Int(), b.Int()), true
	}
	if a.IsNull() || b.IsNull() {
		return 0, false
	}
	a, b = withAffinity(a, b, aff)
	return compare(a, b), true
}

// cmpResult turns a comparison's order into the result of op.
func cmpResult(op Op, c int) bool {
	switch op {
	case OpEq, OpIs:
		return c == 0
	case OpNe, OpIsNot:
		return c != 0
	case OpLt:
		return c < 0
	case OpLe:
		return c <= 0
	case OpGt:
		return c > 0
	}
	return c >= 0 // OpGe
}

// compareOp is a comparison's value: 1, 0, or NULL when either side is
// NULL, apart from IS and IS NOT, which take two NULLs as equal and never
// give NULL.
func compareOp(op Op, a, b value.Value, aff affinity) value.Value {
	if op == OpIs || op == OpIsNot {
		if a.IsNull() || b.IsNull() {
			return boolValue((a.IsNull() && b.IsNull()) == (op == OpIs))
		}
	}
	c, ok := compareWith(a, b, aff)
	if !ok {
		return null
	}
	return boolValue(cmpResult(op, c))
}

// Arithmetic, as OP_Add to OP_Remainder work it out (SQL.md,
// "Arithmetic"). Two whole numbers give a whole number, or a real when the
// result won't fit in 64 bits; text and bytes are read by numericType; a
// real on either side works in float64; dividing by zero gives NULL, and so
// does a result that would be NaN. NULL on either side gives NULL.
func arith(op Op, l, r value.Value) value.Value {
	kl, kr := kind(l), kind(r)
	if kl == value.KindInt && kr == value.KindInt {
		return intArith(op, l, r, l.Int(), r.Int())
	}
	if kl == value.KindNull || kr == value.KindNull {
		return null
	}
	a, aInt := numericType(l)
	b, bInt := numericType(r)
	if aInt && bInt {
		return intArith(op, l, r, a, b)
	}
	return realArith(op, l, r)
}

// intArith works out a op b, the whole numbers l and r read as, and goes
// over to realArith on l and r when the result won't fit in 64 bits.
func intArith(op Op, l, r value.Value, a, b int64) value.Value {
	switch op {
	case OpAdd:
		if s := a + b; (s > a) == (b > 0) {
			return value.Int(s)
		}
	case OpSub:
		if s := a - b; (s < a) == (b > 0) {
			return value.Int(s)
		}
	case OpMul:
		if p, ok := mulInt(a, b); ok {
			return value.Int(p)
		}
	case OpDiv:
		if b == 0 {
			return null
		}
		if b != -1 || a != math.MinInt64 {
			if plant == "query/div-rounds-down" && (a%b != 0) && (a < 0) != (b < 0) {
				return value.Int(a/b - 1)
			}
			return value.Int(a / b)
		}
	case OpRem:
		if b == 0 {
			return null
		}
		if b == -1 {
			b = 1
		}
		return value.Int(a % b)
	}
	return realArith(op, l, r)
}

// mulInt multiplies, and reports false when the product won't fit.
func mulInt(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a || a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 {
		return 0, false
	}
	return p, true
}

// realArith works out l op r in float64, with each side read by realOf.
// The remainder reads both as whole numbers and gives a real.
func realArith(op Op, l, r value.Value) value.Value {
	x, y := realOf(l), realOf(r)
	var z float64
	switch op {
	case OpAdd:
		z = float64(x + y)
	case OpSub:
		z = float64(x - y)
	case OpMul:
		z = float64(x * y)
	case OpDiv:
		if y == 0 {
			return null
		}
		z = float64(x / y)
	case OpRem:
		a, b := intOf(l), intOf(r)
		if b == 0 {
			return null
		}
		if b == -1 {
			b = 1
		}
		z = float64(a % b)
	}
	if math.IsNaN(z) && plant != "query/nan-kept" {
		return null
	}
	return value.Real(z)
}

// maxLength is SQLITE_MAX_LENGTH, the longest text or bytes SQLite makes.
const maxLength = 1000000000

// concat is OP_Concat: the text of l followed by the text of r, or NULL when
// either is NULL. It reports false when the result would be longer than
// SQLite's limit.
func concat(l, r value.Value) (value.Value, bool) {
	a, ok := textOf(l)
	if !ok {
		return null, true
	}
	b, ok := textOf(r)
	if !ok {
		return null, true
	}
	if len(a)+len(b) > maxLength {
		return null, false
	}
	return value.Text(a + b), true
}

// cast is sqlite3VdbeMemCast, CAST(v AS to), to is one of Cast's types.
// NULL stays NULL.
func cast(v value.Value, to string) value.Value {
	k := kind(v)
	if k == value.KindNull {
		return v
	}
	switch to {
	case "INTEGER":
		if k == value.KindInt {
			return v
		}
		return value.Int(intOf(v))
	case "REAL":
		if k == value.KindReal {
			return v
		}
		return value.Real(realOf(v))
	case "NUMERIC":
		if k == value.KindInt || k == value.KindReal {
			return v
		}
		return numerify(v.Raw())
	case "TEXT":
		if k == value.KindText {
			return v
		}
		s, _ := textOf(v)
		return value.Text(s)
	}
	// BLOB
	if k == value.KindBytes {
		return v
	}
	s, _ := textOf(v)
	return value.Bytes(s)
}

// castAffinity is sqlite3AffinityType for the types Cast takes.
func castAffinity(to string) affinity {
	switch to {
	case "INTEGER":
		return affInteger
	case "REAL":
		return affReal
	case "NUMERIC":
		return affNumeric
	case "TEXT":
		return affText
	}
	return affBlob
}
