// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// A test of its own for each rule in SQL.md's "Values" and each function,
// as a table of expressions and what they give. eval_sqlite_test.go, built
// with cgo, holds every table to 0.x's answers too, so each table is
// SQLite's behaviour as well as the evaluator's.

// A ruleCase is an expression of the subset without fields, its
// arguments, and what it gives: an int or an int64, a float64 to the bit,
// a string for text, a []byte for bytes, nil for NULL, or a failure.
type ruleCase struct {
	expr string
	want any
	args []any
}

// A failure is an error a ruleCase gives.
type failure string

const (
	anError    failure = "an *Error"    // integer overflow, LIKE's errors and the like
	aFuncError failure = "a *FuncError" // distance() or vector() refusing its arguments
)

var negZero = math.Copysign(0, -1)

// overflow is an operand that raises an error when it's worked out, to show
// which parts of an expression are.
const overflow = "abs(-9223372036854775808)"

func r(expr string, want any, args ...any) ruleCase { return ruleCase{expr, want, args} }

// withE puts overflow into an expression in place of each E standing
// alone, so the E of LIKE stays.
func withE(expr string, want any) ruleCase {
	return ruleCase{loneE.ReplaceAllLiteralString(expr, overflow), want, nil}
}

var loneE = regexp.MustCompile(`\bE\b`)

var rulesKinds = []ruleCase{
	r("typeof(NULL)", "null"), r("typeof(1)", "integer"), r("typeof(1.5)", "real"), r("typeof('a')", "text"),
	r("typeof(x'00')", "blob"), r("typeof(9223372036854775808)", "real"), r("typeof(-9223372036854775808)", "integer"),
	r("CAST(x'ff' AS TEXT)", "\xff"), r("typeof(CAST(x'ff' AS TEXT))", "text"),
	r("-0.0", negZero), r("0.0", 0.0), r("1e999", math.Inf(1)), r("-1e999", math.Inf(-1)),
	r("1e999 - 1e999", nil), r("1e999 * 0", nil), r("1e999 / 1e999", nil), r("0 * -1e999", nil),
	r("x''", []byte{}), r("''", ""),
}

var rulesArguments = []ruleCase{
	r("? - ?", 7, int64(10), int64(3)), r("typeof(?)", "null", math.NaN()), r("? IS NULL", 1, math.NaN()),
	r("? || ''", "\xff\xfe", "\xff\xfe"), r("length(?)", 1, "a\x00b"), r("?", "a\x00b", "a\x00b"),
	r("typeof(?)", "blob", []byte{}), r("length(?)", 0, []byte{}), r("?", nil, nil), r("?", 1, true),
	r("?", 0, false), r("?", negZero, negZero), r("?", math.Inf(-1), math.Inf(-1)), r("? = 5", 0, "5"),
	r("? = 5", 1, int64(5)), r("? = 5", 1, 5.0), r("typeof(?)", "real", 5e-324),
}

var rulesComparing = []ruleCase{
	r("NULL = NULL", nil), r("1 < NULL", nil), r("NULL <> 1", nil), r("NULL IS NULL", 1),
	r("1 < 'a'", 1), r("'a' < x'00'", 1), r("x'00' > 99", 1), r("1.5 < '0'", 1), r("NULL < -1e999", nil),
	r("9007199254740993 > 9007199254740992.0", 1), r("9007199254740993 = 9007199254740992.0", 0),
	r("9007199254740992 = 9007199254740992.0", 1), r("-0.0 = 0", 1), r("-0.0 < 0", 0),
	r("9223372036854775807 < 9223372036854775808.0", 1), r("-9223372036854775808 = -9223372036854775808.0", 1),
	r("-9223372036854775808 > -9223372036854777856.0", 1), r("1 = 1.0", 1), r("2 > 1.5", 1),
	r("'A' < 'a'", 1), r("'ab' > 'a'", 1), r("'' < 'a'", 1), r("'é' > 'z'", 1), r("'a' = 'a'", 1),
	r("x'0001' < x'01'", 1), r("x'00' < x'0000'", 1), r("x'41' = 'A'", 0), r("'1' = 1", 0),
	r("max(1, 'a')", "a"), r("min(x'00', 'z')", "z"), r("nullif(1, 1.0)", nil),
}

var rulesConversion = []ruleCase{
	r("CAST(5 AS INTEGER) = '5'", 1), r("CAST(5 AS INTEGER) = '5x'", 0), r("CAST(5 AS INTEGER) = ' 5 '", 1),
	r("'5' = CAST(5 AS INTEGER)", 1), r("CAST(5 AS REAL) = '5.0'", 1), r("CAST(5 AS NUMERIC) = '5e0'", 1),
	r("CAST(5 AS INTEGER) = '5.0'", 1), r("CAST(5 AS INTEGER) < '10'", 1), r("CAST(5 AS INTEGER) = x'35'", 0),
	r("CAST('5' AS TEXT) = 5", 1), r("5 = CAST('5' AS TEXT)", 1), r("CAST(1.5 AS TEXT) = 1.5", 1),
	r("CAST('1e0' AS TEXT) = 1", 0), r("CAST('5' AS TEXT) = CAST(5 AS TEXT)", 1),
	r("CAST('5' AS BLOB) = 5", 0), r("CAST('5' AS BLOB) = '5'", 0), r("CAST('5' AS BLOB) = x'35'", 1),
	r("CAST('5' AS TEXT) = CAST(5 AS INTEGER)", 1), r("CAST(1 AS TEXT) = CAST(1 AS BLOB)", 0),
	r("CAST('5' AS BLOB) = CAST(5 AS INTEGER)", 0), r("'5' = 5", 0), r("? = 5", 0, "5"),
	r("+CAST(5 AS INTEGER) = '5'", 0), r("(CAST(5 AS INTEGER)) = '5'", 1), r("((CAST(5 AS INTEGER))) = '5'", 1),
	r("CAST(5 AS INTEGER) IN ('5', 6)", 1), r("5 IN (CAST('5' AS TEXT), 6)", 0), r("CAST(5 AS TEXT) IN (5, 6)", 1),
	r("CAST(5 AS INTEGER) IN ('5')", 1), r("5 IN (CAST('5' AS TEXT))", 0), r("CAST(5 AS TEXT) IN (5, 6, 7)", 1),
	r("CAST(5 AS INTEGER) IN ('5.0', 6, 7)", 1), r("'1' BETWEEN CAST(1 AS INTEGER) AND 2", 0),
	r("CAST(1 AS INTEGER) BETWEEN '1' AND '1'", 1), r("nullif(CAST(5 AS INTEGER), '5')", 5),
	r("CAST(5 AS INTEGER) IS '5'", 1), r("CAST(5 AS INTEGER) IS NOT '5'", 0),
}

var rulesTruth = []ruleCase{
	r("NOT NULL", nil), r("NOT 0", 1), r("NOT 1", 0), r("NOT 'a'", 1), r("NOT '1'", 0), r("NOT 0.5", 0),
	r("NOT -0.0", 1), r("NOT x'31'", 0), r("NOT ' 2'", 0), r("NOT '0.0'", 1), r("NOT x''", 1), r("NOT 1e999", 0),
	r("(1=1) AND NULL", nil), r("(1=0) AND NULL", 0), r("NULL AND (1=0)", 0), r("(1=1) AND (2=2)", 1),
	r("(1=1) OR NULL", 1), r("NULL OR (1=0)", nil), r("(1=0) OR (2=3)", 0), r("NULL OR NULL", nil),
	r("'a' AND (1=1)", 0), r("'1' OR (1=0)", 1), r("typeof(1 < 2)", "integer"), r("2 AND 3", 1),
	r("typeof('a' LIKE 'a')", "integer"), r("typeof(1 IN (1, 2))", "integer"), r("typeof(NOT 5)", "integer"),
}

var rulesArithmetic = []ruleCase{
	r("2 + 3", 5), r("2 - 3", -1), r("2 * 3", 6), r("9223372036854775807 + 1", 9223372036854775808.0),
	r("-9223372036854775808 - 1", -9223372036854775808.0), r("4611686018427387904 * 2", 9223372036854775808.0),
	r("3037000500 * 3037000500", 9223372037000250000.0), r("-9223372036854775807 - 1", math.MinInt64),
	r("-7 / 2", -3), r("7 / -2", -3), r("7 / 2", 3), r("-9223372036854775808 / -1", 9223372036854775808.0),
	r("-7 % 2", -1), r("7 % -2", 1), r("-9223372036854775808 % -1", 0), r("5.5 % 2", 1.0), r("5 % 2.5", 1.0),
	r("5 % 0.5", nil), r("1e300 % 10", 7.0), r("-5.5 % 2", -1.0), r("7.0 / 2", 3.5), r("1 / 3.0", 1.0/3),
	r("1 / 0", nil), r("1 % 0", nil), r("1.0 / 0", nil), r("1 / 0.0", nil), r("1 / -0.0", nil), r("0.0 / 0.0", nil),
	r("NULL + 1", nil), r("1 * NULL", nil), r("NULL / 0", nil),
	r("'12' + 1", 13), r("'12.5' + 1", 13.5), r("'1e3' + 0", 1000.0), r("'12abc' + 1", 13), r("'abc' + 1", 1),
	r("'0x10' + 0", 0), r("' 12 ' + 0", 12), r("'-0' * 1.0", negZero), r("'-0' + 0", 0),
	r("'9223372036854775808' + 0", 9223372036854775808.0), r("x'3132' + 0", 12), r("x'' + 1", 1),
	r("'1.5e' + 0", 1.5), r("'12' * '2'", 24), r("CAST(x'313200' AS TEXT) + 0", 12.0),
	r("-'5'", -5), r("-'abc'", 0), r("-'1.5'", -1.5), r("-NULL", nil), r("-x'31'", -1), r("- -1", 1),
	r("-(-9223372036854775808)", 9223372036854775808.0), r("-(0.0)", negZero), r("-(1 - 1.0)", 0.0),
	r("+'5'", "5"), r("+x'00'", []byte{0}), r("typeof(+'5')", "text"),
	r("-9223372036854775808", math.MinInt64), r("-9223372036854775809", -9223372036854775809.0),
	r("9223372036854775808", 9223372036854775808.0), r("-0", 0), r("--1", anError),
}

var rulesNumbersAsText = []ruleCase{
	r("CAST(0.1 AS TEXT)", "0.1"), r("CAST(0.1 + 0.2 AS TEXT)", "0.30000000000000004"),
	r("CAST(5e-324 AS TEXT)", "4.9406564584124654e-324"), r("CAST(1.0 AS TEXT)", "1.0"),
	r("CAST(100.0 AS TEXT)", "100.0"), r("CAST(1e21 AS TEXT)", "1.0e+21"), r("CAST(1e-7 AS TEXT)", "1.0e-07"),
	r("CAST(1e-5 AS TEXT)", "1.0e-05"), r("CAST(1e-4 AS TEXT)", "0.0001"), r("CAST(1e16 AS TEXT)", "10000000000000000.0"),
	r("CAST(1e17 AS TEXT)", "1.0e+17"), r("CAST(-0.0 AS TEXT)", "0.0"), r("CAST(1e999 AS TEXT)", "Inf"),
	r("CAST(-1e999 AS TEXT)", "-Inf"), r("CAST(49.47 AS TEXT)", "49.47"), r("CAST(123 AS TEXT)", "123"),
	r("CAST(-9223372036854775808 AS TEXT)", "-9223372036854775808"), r("CAST(1e100 AS TEXT)", "1.0e+100"),
	r("CAST(1.7976931348623157e308 AS TEXT)", "1.7976931348623157e+308"), r("CAST(2.5e-3 AS TEXT)", "0.0025"),
	r("CAST(123456789.125 AS TEXT)", "123456789.125"), r("CAST(0.3 AS TEXT)", "0.3"),
	r("CAST(9007199254740993.0 AS TEXT)", "9007199254740992.0"), r("1.5 || ''", "1.5"), r("length(1.5)", 3),
	r("length(-0.0)", 3), r("upper(1e21)", "1.0E+21"), r("lower(1e999)", "inf"), r("1e999 LIKE 'inf'", 1),
	r("CAST(CAST(0.1 + 0.2 AS TEXT) AS REAL) = 0.1 + 0.2", 1),
}

var rulesTextAsNumbers = []ruleCase{
	// Comparisons against a numeric CAST: all of it, apart from spaces.
	r("CAST(1 AS INTEGER) = '1.0'", 1), r("CAST(1 AS INTEGER) = '1abc'", 0), r("CAST(1 AS INTEGER) = ' 1 '", 1),
	r("CAST(1 AS INTEGER) = '0x1'", 0), r("CAST(1 AS INTEGER) = '1e0'", 1), r("CAST(1 AS INTEGER) = '.'", 0),
	// CAST to INTEGER, and integer arguments.
	r("CAST('12abc' AS INTEGER)", 12), r("CAST('  -12' AS INTEGER)", -12), r("CAST('1e3' AS INTEGER)", 1),
	r("CAST('99999999999999999999' AS INTEGER)", math.MaxInt64), r("CAST('-99999999999999999999' AS INTEGER)", math.MinInt64),
	r("CAST('9223372036854775808' AS INTEGER)", math.MaxInt64), r("CAST('-9223372036854775808' AS INTEGER)", math.MinInt64),
	r("CAST('abc' AS INTEGER)", 0), r("CAST(x'3132' AS INTEGER)", 12), r("CAST('+7' AS INTEGER)", 7),
	r("CAST('- 7' AS INTEGER)", 0), r("CAST('0012' AS INTEGER)", 12), r("substr('abcdef', '2')", "bcdef"),
	r("substr('abcdef', ' 3 ')", "cdef"), r("round(2.5, '0')", 3.0),
	// CAST to REAL, truth, and real arguments.
	r("CAST('1.5x' AS REAL)", 1.5), r("CAST('abc' AS REAL)", 0.0), r("CAST('1e999' AS REAL)", math.Inf(1)),
	r("CAST('3500000000000000.2500001' AS REAL)", 3500000000000000.0), r("CAST('-0' AS REAL)", negZero),
	r("CAST(' .5 ' AS REAL)", 0.5), r("CAST('5.' AS REAL)", 5.0), r("CAST('1e+' AS REAL)", 1.0), r("CAST('-' AS REAL)", 0.0),
	r("CAST(CAST(x'313200' AS TEXT) AS REAL)", 12.0), r("CAST('1e-400' AS REAL)", 0.0), r("abs('-2')", 2.0),
	r("round('2.5')", 3.0), r("NOT '0.0'", 1), r("CAST(x'2d31' AS REAL)", -1.0),
	// CAST to NUMERIC.
	r("CAST('12' AS NUMERIC)", 12), r("CAST('12.0' AS NUMERIC)", 12), r("CAST('12.5' AS NUMERIC)", 12.5),
	r("CAST('1e3' AS NUMERIC)", 1000), r("CAST('2251799813685247.0' AS NUMERIC)", 2251799813685247),
	r("CAST('2251799813685248.0' AS NUMERIC)", 2251799813685248.0), r("CAST('9223372036854775808' AS NUMERIC)", 9223372036854775808.0),
	r("CAST('9223372036854775807' AS NUMERIC)", math.MaxInt64), r("CAST('12abc' AS NUMERIC)", 12),
	r("CAST('abc' AS NUMERIC)", 0), r("CAST(x'' AS NUMERIC)", 0), r("CAST('-0.0' AS NUMERIC)", 0),
	r("CAST(' 7 ' AS NUMERIC)", 7), r("CAST('1.5e1' AS NUMERIC)", 15), r("CAST(x'312e35' AS NUMERIC)", 1.5),
}

var rulesCast = []ruleCase{
	r("CAST(NULL AS TEXT)", nil), r("CAST(NULL AS INTEGER)", nil), r("CAST(2.0 AS NUMERIC)", 2.0),
	r("CAST(2 AS NUMERIC)", 2), r("CAST(2.7 AS INTEGER)", 2), r("CAST(-2.7 AS INTEGER)", -2),
	r("CAST(1e999 AS INTEGER)", math.MaxInt64), r("CAST(-1e999 AS INTEGER)", math.MinInt64),
	r("CAST(9223372036854775807.0 AS INTEGER)", math.MaxInt64), r("CAST(-0.0 AS INTEGER)", 0),
	r("CAST(x'41' AS TEXT)", "A"), r("CAST('A' AS BLOB)", []byte("A")), r("CAST(1.5 AS BLOB)", []byte("1.5")),
	r("CAST(12 AS BLOB)", []byte("12")), r("CAST(12 AS REAL)", 12.0), r("CAST(x'00' AS BLOB)", []byte{0}),
	r("CAST(1.5 AS REAL)", 1.5), r("CAST('a' AS TEXT)", "a"), r("cast(1 as integer)", 1), r("CAST(1 AS text)", "1"),
}

var rulesConcatenation = []ruleCase{
	r("NULL || 'a'", nil), r("'a' || NULL", nil), r("1 || 2", "12"), r("1.0 || ''", "1.0"),
	r("x'41' || x'42'", "AB"), r("typeof(x'41' || x'42')", "text"), r("'a' || 1e21", "a1.0e+21"),
	r("'a' || x'00' || 'b'", "a\x00b"), r("-0.0 || ''", "0.0"), r("'' || ''", ""),
}

var rulesLike = []ruleCase{
	r("'q3 plan' LIKE 'Q3%'", 1), r("'é' LIKE 'É%'", 0), r("'É' LIKE 'é'", 0), r("'abc' LIKE 'a_c'", 1),
	r("'abc' LIKE 'A_C'", 1), r(`'a%c' LIKE 'a\%c' ESCAPE '\'`, 1), r(`'abc' LIKE 'a\%c' ESCAPE '\'`, 0),
	r(`'a_c' LIKE 'a\_c' ESCAPE '\'`, 1), r(`'A' LIKE '\a' ESCAPE '\'`, 1), r("'a%' LIKE 'a%%' ESCAPE '%'", 1),
	r("'ab' LIKE 'a%%' ESCAPE '%'", 0), r("'_' LIKE '__' ESCAPE '_'", 1), r("'aé' LIKE 'aéé' ESCAPE 'é'", 1),
	r("NULL LIKE 'a'", nil), r("'a' LIKE NULL", nil), r("'a' LIKE 'a' ESCAPE NULL", nil),
	r("'a' LIKE 'a' ESCAPE 'ab'", anError), r("'a' LIKE 'a' ESCAPE ''", anError), r("NULL LIKE 'a' ESCAPE 'ab'", anError),
	r("NULL LIKE ?", anError, strings.Repeat("a", 50001)), r("'a' LIKE ?", 0, strings.Repeat("a", 50000)),
	r("CAST(x'610062' AS TEXT) LIKE 'a'", 1), r("'a' LIKE CAST(x'610062' AS TEXT)", 1), r("'ab' LIKE 'a'", 0),
	r("1.5 LIKE '1.5'", 1), r("15 LIKE '1_'", 1), r("x'41' LIKE 'a'", 1), r("'a' NOT LIKE 'b'", 1),
	r("NULL NOT LIKE 'a'", nil), r("'' LIKE '%'", 1), r("'abc' LIKE '%%c'", 1), r("'' LIKE '_'", 0),
	r("'😀' LIKE '_'", 1), r("'é' LIKE '_'", 1), r("'abc' LIKE '%b%'", 1), r("'abc' LIKE '%B'", 0),
	r("'aXbXc' LIKE '%x%x%'", 1), r("'a' LIKE 'a' ESCAPE 'a'", 0), r("'a' LIKE 'aa' ESCAPE 'a'", 1),
	r("'a' LIKE 'a' ESCAPE CAST(x'21003f' AS TEXT)", 1), r("'é' LIKE CAST(x'c3' AS TEXT)", 0),
	r("'abc' LIKE 'a' || '%'", 1), r("'a' LIKE '%_%_'", 0), r("'ab' LIKE '%_%_'", 1),
}

var rulesInBetweenIs = []ruleCase{
	r("1 IN (1, 2)", 1), r("3 IN (1, 2)", 0), r("3 IN (1, NULL)", nil), r("NULL IN (1, 2)", nil),
	r("1 IN (NULL, 1)", 1), r("1 IN ()", 0), r("NULL IN ()", 0), r("NULL NOT IN ()", 1), r("1 NOT IN (1, 2)", 0),
	r("3 NOT IN (1, NULL)", nil), r("3 NOT IN (1, 2)", 1), r("1 IN (1.0, 2, 3)", 1), r("'a' IN ('A', 'b', 'c')", 0),
	r("x'00' IN (x'00', 1, 2)", 1), r("3 IN (1, 2, NULL)", nil), r("1 IN (1)", 1), r("NULL IN (NULL)", nil),
	r("2 BETWEEN 1 AND 3", 1), r("NULL BETWEEN 1 AND 3", nil), r("0 BETWEEN NULL AND 3", nil),
	r("5 BETWEEN NULL AND 3", 0), r("2 NOT BETWEEN 1 AND 3", 0), r("5 NOT BETWEEN NULL AND 3", 1),
	r("'b' BETWEEN 'a' AND 'c'", 1), r("2 BETWEEN 3 AND 1", 0), r("1.5 BETWEEN 1 AND 2", 1),
	r("NULL IS NULL", 1), r("1 IS NULL", 0), r("NULL IS 1", 0), r("1 IS 1", 1), r("1 IS NOT 1", 0),
	r("NULL IS NOT NULL", 0), r("'a' IS 'a'", 1), r("1 IS 1.0", 1), r("x'00' IS NOT NULL", 1),
	// IS with an empty IN list on the right is SQLite's IS TRUE or IS FALSE,
	// unless the list's left side calls a function.
	r("0.5 IS (1 NOT IN ())", 1), r("'a' IS (1 IN ())", 1), r("NULL IS (1 NOT IN ())", 0),
	r("NULL IS NOT (1 IN ())", 1), r("12 IS NOT (1 NOT IN ())", 0), r("12 IS NOT (abs(1) NOT IN ())", 1),
	r("2 IS (abs(1) NOT IN ())", 0), r("2 IS (1 NOT IN ())", 1),
}

var rulesWorkedOut = []ruleCase{
	withE("0 AND E", 0), withE("E AND 0", 0), withE("1 OR E", 1), withE("E OR 1", 1), withE("(0) AND E", 0),
	withE("00 AND E", 0), withE("2147483647 OR E", 1), withE("(0 AND E) OR 1", 1),
	withE("-0 AND E", anError), withE("+1 OR E", anError), withE("2147483648 OR E", anError),
	withE("1 AND E", anError), withE("0 OR E", anError), withE("(1=1) OR E", anError), withE("NULL AND E", anError),
	withE("0.0 AND E", anError), withE("'0' AND E", anError), withE("(0 AND 1) OR E", anError),
	withE("(E OR 1) AND 0", 0), withE("0 AND (1 OR E)", 0), withE("(1 IN ()) AND E", 0), withE("(1 NOT IN ()) OR E", 1),
	withE("NOT (1 IN ()) OR E", anError), withE("E AND ('a' IS NULL)", 0), withE("E OR (1 IS NOT NULL)", 1),
	withE("E AND (NULL IS NULL)", anError), withE("E AND (1 IS NOT NULL)", anError), withE("E AND (-5 IS NULL)", 0),
	withE("((1.5 IS NULL) IS NOT NULL) OR E", 1), withE("E AND (1 IS 2)", anError),
	withE("coalesce(1, E)", 1), withE("ifnull(1, E)", 1), withE("coalesce(NULL, 2, E)", 2), withE("coalesce(NULL, E)", anError),
	withE("1 IN (1, E)", 1), withE("1 IN (2, E)", anError), withE("NULL IN (1, E)", anError), withE("E IN ()", 0),
	withE("E NOT IN ()", 1), withE("1 IN (1, 2, E)", anError), withE("1 IN (E)", anError), withE("nullif(1, E)", anError),
	withE("1 BETWEEN 2 AND E", anError), withE("max(1, NULL, E)", anError), withE("E IS NULL", anError),
	withE("E LIKE vector('oops')", aFuncError), withE("vector('oops') LIKE E", anError),
	withE("1 IN (vector('oops'), 2, E)", aFuncError), withE("E IN (vector('oops'), 2, 3)", aFuncError),
	withE("E IN (vector('oops'), 2)", anError), withE("E + vector('oops')", anError),
	withE("E BETWEEN vector('oops') AND 1", anError), withE("CAST(E AS TEXT) IS (vector('x') IN ())", anError),
}

var rulesFunctions = []ruleCase{
	r("abs(-5)", 5), r("abs(5)", 5), r("abs(NULL)", nil), r("abs(-9223372036854775808)", anError),
	r("abs(-9223372036854775807)", math.MaxInt64), r("abs(-1.5)", 1.5), r("abs(-0.0)", negZero), r("abs('-2')", 2.0),
	r("abs('abc')", 0.0), r("abs(x'2d33')", 3.0), r("abs(-1e999)", math.Inf(1)),
	r("coalesce(NULL, NULL, 3)", 3), r("coalesce(NULL, NULL)", nil), r("coalesce(1, 2)", 1), r("ifnull(NULL, 'a')", "a"),
	r("ifnull(1, 2)", 1), r("ifnull(NULL, NULL)", nil),
	r("instr('abcabc', 'ca')", 3), r("instr('abc', '')", 1), r("instr('abc', 'd')", 0), r("instr(NULL, 'a')", nil),
	r("instr('a', NULL)", nil), r("instr('éa', 'a')", 2), r("instr(x'c3a961', x'61')", 3), r("instr(12345, 34)", 3),
	r("instr(x'616263', 'c')", 3), r("instr('', '')", 1), r("instr('a', 'ab')", 0), r("instr('é', x'a9')", 0),
	r("instr(CAST(x'610062' AS TEXT), 'b')", 3), r("instr(1.5, '.')", 2), r("instr('aé😀b', 'b')", 4),
	r("length('abc')", 3), r("length('é')", 1), r("length(x'00ff')", 2), r("length(1.5)", 3), r("length(12)", 2),
	r("length(NULL)", nil), r("length(CAST(x'610062' AS TEXT))", 1), r("length('😀')", 1),
	r("length(CAST(x'80' AS TEXT))", 1), r("length(CAST(x'c3' AS TEXT))", 1), r("length(CAST(x'c380808080' AS TEXT))", 1),
	r("length('')", 0), r("length(x'')", 0), r("length(-9223372036854775808)", 20),
	r("upper('abcé')", "ABCé"), r("lower('ÉA')", "Éa"), r("upper(NULL)", nil), r("lower(x'41')", "a"),
	r("typeof(lower(x'41'))", "text"), r("upper(CAST(x'610062' AS TEXT))", "A\x00B"), r("lower(12)", "12"),
	r("max(1, 2.5, '0')", "0"), r("min(1, 2.5)", 1), r("min(1, 1.0)", 1.0), r("max(1, 1.0)", 1), r("max(1, NULL)", nil),
	r("min(NULL, 1)", nil), r("max(x'00', 'z')", []byte{0}), r("min(2, 1, 3)", 1), r("max(1.0, 1, 1)", 1.0),
	r("min(1, 1.0, 1)", 1), r("max(-0.0, 0)", negZero), r("min(-0.0, 0)", 0), r("min('a', 'A')", "A"),
	r("nullif(1, 1)", nil), r("nullif(1, 1.0)", nil), r("nullif(1, '1')", 1), r("nullif(NULL, NULL)", nil),
	r("nullif(NULL, 1)", nil), r("nullif(1, NULL)", 1), r("nullif('a', 'A')", "a"), r("nullif(-0.0, 0)", nil),
	r("replace('abcabc', 'bc', 'X')", "aXaX"), r("replace('abc', '', 'X')", "abc"), r("replace('abc', '', NULL)", "abc"),
	r("replace('abc', 'b', NULL)", nil), r("replace(NULL, 'a', 'b')", nil), r("replace('abc', NULL, 'b')", nil),
	r("replace(123, 2, 9)", "193"), r("replace('aaa', 'aa', 'b')", "ba"), r("replace('abc', CAST(x'0061' AS TEXT), 'z')", "abc"),
	r("replace(1.5, '.', ',')", "1,5"), r("replace(x'616263', 'b', 'X')", "aXc"), r("replace('a', 'abc', 'X')", "a"),
	r("replace('abc', 'abc', '')", ""), r("typeof(replace(x'61', 'z', 'y'))", "text"),
	r("round(2.5)", 3.0), r("round(-2.5)", -3.0), r("round(-0.4)", 0.0), r("round(-0.0)", 0.0), r("round(2.675, 2)", 2.67),
	r("round(NULL)", nil), r("round(1.5, NULL)", nil), r("round(NULL, 1)", nil), r("round(1.5, -1)", 2.0),
	r("round(4503599627370497.0)", 4503599627370497.0), r("round('2.5')", 3.0), r("typeof(round(2))", "real"),
	r("round(0.49999999999999994)", 1.0), r("round(1.23456789, 3)", 1.235), r("round(1e999)", math.Inf(1)),
	r("round(123.456, 30)", 123.456), r("round(123.456, 31)", 123.456), r("round(0.000001234, 7)", 0.0000012),
	r("round(-1.5, 0)", -2.0), r("round(1.15, 1)", 1.1), r("round(9223372036854775807)", 9223372036854775807.0),
	r("substr('abcdef', 2)", "bcdef"), r("substr('abcdef', 2, 3)", "bcd"), r("substr('abcdef', -2)", "ef"),
	r("substr('abcdef', -3, 2)", "de"), r("substr('abcdef', 3, -2)", "ab"), r("substr('abcdef', 0, 2)", "a"),
	r("substr('abcdef', 0)", "abcdef"), r("substr('abcdef', 1, 0)", ""), r("substr('éa', 1, 1)", "é"),
	r("substr(x'010203', 2)", []byte{2, 3}), r("substr(x'', 1)", nil), r("substr(NULL, 1)", nil),
	r("substr('abc', NULL)", nil), r("substr('abc', 1, NULL)", nil), r("substr(CAST(x'61006263' AS TEXT), 1)", "a"),
	r("substr(12345, 2, 2)", "23"), r("substr('abc', 2.9)", "bc"), r("substr('abc', -9223372036854775808, 2)", ""),
	r("substr('abc', 9223372036854775807)", ""), r("substr('abcdef', -10, 7)", "abc"), r("substr('abcdef', 2, -5)", "a"),
	r("substr(x'010203', -1)", []byte{3}), r("substr(x'010203', 0, 2)", []byte{1}), r("substr(x'010203', 5)", []byte{}),
	r("substr('abc', 0, -1)", ""), r("substr('😀bc', 2)", "bc"), r("typeof(substr(x'0102', 1))", "blob"),
	r("trim('  a  ')", "a"), r("trim('xxaxx', 'x')", "a"), r("trim('abcba', 'ab')", "c"), r("trim(NULL)", nil),
	r("trim('a', NULL)", nil), r("trim('  a  ', '')", "  a  "), r("trim('éaé', 'é')", "a"), r("trim(123, 1)", "23"),
	r("trim(x'206120', ' ')", "a"), r("trim('aaa', 'a')", ""), r("trim(1.5, '5')", "1."),
	r("trim('éaé', CAST(x'c3' AS TEXT))", "\xa9a\xc3\xa9"), r("trim('abc', CAST(x'610063' AS TEXT))", "bc"),
	r("typeof(NULL)", "null"), r("typeof(-0.0)", "real"),
	r("distance('[1, 0]', '[0, 1]')", 1.0), r("distance('[1, 0]', '[1, 0]')", 0.0), r("distance('[1, 0]', '[-1, 0]')", 2.0),
	r("distance(NULL, '[1]')", nil), r("distance('[1]', NULL)", nil), r("distance('[1, 0]', '[1, 0, 0]')", aFuncError),
	r("distance('[0, 0]', '[1, 0]')", aFuncError), r("distance(x'00000000', '[1]')", 1.0),
	r("distance(x'00000080', '[1]')", 1.0), r("distance('[1, 0]', 'oops')", aFuncError), r("distance(1, '[1]')", aFuncError),
	r("distance(x'0000c07f', x'0000803f')", aFuncError), r("distance(x'0000807f', x'0000803f')", aFuncError),
	r("distance(x'00', x'00')", aFuncError), r("distance(x'', x'')", aFuncError), r("distance(x'0000803f', x'')", aFuncError),
	r("distance('[3, 4]', '[4, 3]')", 0.040000000000000036), r("distance(vector('[1, 2]'), '[2, 4]')", 2.220446049250313e-16),
	r("distance(NULL, 1)", nil), r("distance('[1]', 1.5)", aFuncError),
	r("vector('[1, 0.5, -2]')", []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x3f, 0, 0, 0, 0xc0}), r("typeof(vector('[1]'))", "blob"),
	r("length(vector('[1, 2]'))", 8), r("vector('[]')", aFuncError), r("vector('oops')", aFuncError),
	r("vector('[1e39]')", aFuncError), r("vector('[1e-46]')", aFuncError), r("vector(NULL)", nil),
	r("vector(x'0000803f')", []byte{0, 0, 0x80, 0x3f}), r("vector(x'00')", aFuncError), r("vector(1)", aFuncError),
	r("vector('[null, 1]')", []byte{0, 0, 0, 0, 0, 0, 0x80, 0x3f}), r("vector(x'00000000')", aFuncError),
	r("vector(x'0000c07f')", aFuncError), r("vector('[0.1]')", []byte{0xcd, 0xcc, 0xcc, 0x3d}), r("vector(2.5)", aFuncError),
	r("vector(' [1] ')", []byte{0, 0, 0x80, 0x3f}), r("vector('[1]x')", aFuncError),
}

// ruleTables names each table for its test, and for the test against 0.x.
var ruleTables = map[string][]ruleCase{
	"Kinds": rulesKinds, "Arguments": rulesArguments, "Comparing": rulesComparing,
	"Conversion before comparing": rulesConversion, "Truth": rulesTruth, "Arithmetic": rulesArithmetic,
	"Numbers as text": rulesNumbersAsText, "Text as numbers": rulesTextAsNumbers, "CAST": rulesCast,
	"Concatenation": rulesConcatenation, "LIKE": rulesLike, "IN, BETWEEN and IS": rulesInBetweenIs,
	"What gets worked out": rulesWorkedOut, "Functions": rulesFunctions,
}

// evalExpr parses SELECT expr and works out its one result column.
func evalExpr(expr string, args []any) (value.Value, error) {
	s, err := Parse("SELECT " + expr)
	if err != nil {
		return value.Value{}, err
	}
	ev, err := Compile(s.(*Select).Results[0].Expr, nil)
	if err != nil {
		return value.Value{}, err
	}
	vs := make([]value.Value, len(args))
	for i, a := range args {
		vs[i] = argValue(a)
	}
	return ev(&Frame{Args: vs})
}

// sameAs reports whether a value or an error is what a ruleCase wants, and
// says what it is when it isn't.
func sameAs(v value.Value, err error, want any) (bool, string) {
	got := v.String()
	if err != nil {
		var pe *Error
		var fe *FuncError
		switch {
		case errors.As(err, &fe):
			got = string(aFuncError) + ": " + err.Error()
		case errors.As(err, &pe):
			got = string(anError) + ": " + err.Error()
		default:
			got = "another error: " + err.Error()
		}
		f, ok := want.(failure)
		return ok && strings.HasPrefix(got, string(f)+":"), got
	}
	switch w := want.(type) {
	case nil:
		return v.IsNull(), got
	case int:
		return v.Kind() == value.KindInt && v.Int() == int64(w), got
	case int64:
		return v.Kind() == value.KindInt && v.Int() == w, got
	case float64:
		return v.Kind() == value.KindReal && math.Float64bits(v.Real()) == math.Float64bits(w), got
	case string:
		return v.Kind() == value.KindText && v.Raw() == w, got
	case []byte:
		return kind(v) == value.KindBytes && v.Raw() == string(w), got
	}
	return false, got
}

func checkRules(t *testing.T, name string) {
	t.Helper()
	cases := ruleTables[name]
	if len(cases) == 0 {
		t.Fatalf("no table %q", name)
	}
	for _, c := range cases {
		v, err := evalExpr(c.expr, c.args)
		if ok, got := sameAs(v, err, c.want); !ok {
			t.Errorf("%s %v: got %s, want %#v", c.expr, c.args, got, c.want)
		}
	}
}

func TestKinds(t *testing.T)                     { checkRules(t, "Kinds") }
func TestArguments(t *testing.T)                 { checkRules(t, "Arguments") }
func TestComparing(t *testing.T)                 { checkRules(t, "Comparing") }
func TestConversionBeforeComparing(t *testing.T) { checkRules(t, "Conversion before comparing") }
func TestTruth(t *testing.T)                     { checkRules(t, "Truth") }
func TestArithmetic(t *testing.T)                { checkRules(t, "Arithmetic") }
func TestNumbersAsText(t *testing.T)             { checkRules(t, "Numbers as text") }
func TestTextAsNumbers(t *testing.T)             { checkRules(t, "Text as numbers") }
func TestCast(t *testing.T)                      { checkRules(t, "CAST") }
func TestConcatenation(t *testing.T)             { checkRules(t, "Concatenation") }
func TestLike(t *testing.T)                      { checkRules(t, "LIKE") }
func TestInBetweenAndIs(t *testing.T)            { checkRules(t, "IN, BETWEEN and IS") }
func TestWhatGetsWorkedOut(t *testing.T)         { checkRules(t, "What gets worked out") }
func TestFunctions(t *testing.T)                 { checkRules(t, "Functions") }

// TestEvalsRunOnSeveralGoroutines works out one compiled expression on
// several goroutines at once, each with a Frame of its own, as the
// operators may, and each gets what it gets alone. distance() keeps the
// text it read last and borrows buffers, which -race checks here.
func TestEvalsRunOnSeveralGoroutines(t *testing.T) {
	s, err := Parse("SELECT distance(?, '[1, 2, 3]') + distance(?, ?) + length(? || 'x') + (? LIKE 'a%')")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Compile(s.(*Select).Results[0].Expr, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(i int) *Frame {
		v := []float32{float32(i), 1, float32(i % 3)}
		texts := []string{"[1, 0, 0]", "[0, 1, 0]", "[2, 2, 1]"}
		return &Frame{Args: []value.Value{
			value.Bytes(vectorBits(v)), value.Text(texts[i%3]), value.Text(texts[(i+1)%3]),
			value.Text(strings.Repeat("a", i%5)), value.Text([]string{"abc", "b"}[i%2]),
		}}
	}
	want := make([]value.Value, 64)
	for i := range want {
		if want[i], err = ev(frame(i)); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func() {
			for round := 0; round < 50; round++ {
				for i := range want {
					v, err := ev(frame(i))
					if err != nil || v != want[i] {
						errs <- fmt.Errorf("frame %d: got %s, %v; alone it gives %s", i, v, err, want[i])
						return
					}
				}
			}
			errs <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
