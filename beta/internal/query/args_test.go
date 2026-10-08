// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Named types and Valuers for SQL.md's "Arguments" table, which go through
// database/sql's conversion.
type (
	argText   string
	argInt    int16
	argUint   uint
	argReal   float32
	argBytes  []byte
	argBool   bool
	argFailer struct{}
)

func (argFailer) Value() (driver.Value, error) { return nil, errors.New("argFailer refuses") }

var (
	argZone = time.FixedZone("", 5*3600+1800)
	argTime = time.Date(2026, 10, 8, 9, 5, 7, 120000000, argZone)
	argFive = 5
)

// argCases are Go values and the SQL values SQL.md's "Arguments" table
// makes of them, with a nil want for an error. eval_sqlite_test.go holds
// them to what 0.x binds.
var argCases = []struct {
	v    any
	want *value.Value
}{
	{nil, &value.Value{}},
	{int(-7), ptr(value.Int(-7))},
	{int8(-128), ptr(value.Int(-128))},
	{int16(300), ptr(value.Int(300))},
	{int32(math.MinInt32), ptr(value.Int(math.MinInt32))},
	{int64(math.MinInt64), ptr(value.Int(math.MinInt64))},
	{uint8(255), ptr(value.Int(255))},
	{uint16(65535), ptr(value.Int(65535))},
	{uint32(math.MaxUint32), ptr(value.Int(math.MaxUint32))},
	{uint(math.MaxInt64), ptr(value.Int(math.MaxInt64))},
	{uint64(math.MaxInt64), ptr(value.Int(math.MaxInt64))},
	{uint64(1 << 63), nil},
	{uint(1 << 63), ptr(value.Int(math.MinInt64))}, // database/sql's wrap-around
	{true, ptr(value.Int(1))},
	{false, ptr(value.Int(0))},
	{2.5, ptr(value.Real(2.5))},
	{math.Copysign(0, -1), ptr(value.Real(math.Copysign(0, -1)))},
	{math.Inf(1), ptr(value.Real(math.Inf(1)))},
	{math.NaN(), &value.Value{}},
	{float32(0.1), ptr(value.Real(float64(float32(0.1))))},
	{float32(math.NaN()), &value.Value{}},
	{"", ptr(value.Text(""))},
	{"a\x00b", ptr(value.Text("a\x00b"))},
	{"\xff", ptr(value.Text("\xff"))},
	{[]byte(nil), &value.Value{}},
	{[]byte{}, ptr(value.Bytes(""))},
	{[]byte{0, 1}, ptr(value.Bytes("\x00\x01"))},
	{argTime, ptr(value.Text("2026-10-08 09:05:07.12+05:30"))},
	{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ptr(value.Text("2026-01-02 03:04:05+00:00"))},
	{time.Time{}, ptr(value.Text("0001-01-01 00:00:00+00:00"))},
	{argText("t"), ptr(value.Text("t"))},
	{argInt(-3), ptr(value.Int(-3))},
	{argUint(9), ptr(value.Int(9))},
	{argReal(1.5), ptr(value.Real(1.5))},
	{argBytes("b"), ptr(value.Bytes("b"))},
	{argBool(true), ptr(value.Int(1))},
	{json.Number("12"), ptr(value.Text("12"))},
	{&argFive, ptr(value.Int(5))},
	{(*int)(nil), &value.Value{}},
	{c.Vector{1, 0.5}, ptr(value.Bytes("\x00\x00\x80\x3f\x00\x00\x00\x3f"))},
	{sql.NullString{}, &value.Value{}},
	{sql.NullInt64{Int64: 4, Valid: true}, ptr(value.Int(4))},
	{argFailer{}, nil},
	{struct{}{}, nil},
	{map[string]int{}, nil},
	{[]int{1}, nil},
	{complex(1, 2), nil},
}

func ptr(v value.Value) *value.Value { return &v }

func TestArgumentsConvert(t *testing.T) {
	for _, cs := range argCases {
		got, err := Arg(cs.v)
		switch {
		case cs.want == nil && err == nil:
			t.Errorf("Arg(%#v) gives %s, and SQL.md has an error", cs.v, got)
		case cs.want == nil:
		case err != nil:
			t.Errorf("Arg(%#v): %v", cs.v, err)
		case got != *cs.want:
			t.Errorf("Arg(%#v) gives %s, and SQL.md has %s", cs.v, got, *cs.want)
		}
	}
}
