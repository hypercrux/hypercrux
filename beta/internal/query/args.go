// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"database/sql/driver"
	"fmt"
	"math"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// timeLayout is the text go-sqlite3 binds for a time.Time, in the time's
// own zone.
const timeLayout = "2006-01-02 15:04:05.999999999-07:00"

// Arg makes v the value of a ? mark, as SQL.md's "Arguments" table has it:
// what database/sql makes of v for a driver, then what go-sqlite3 binds.
// So a NaN gives NULL, a nil []byte NULL and an empty one empty bytes, a
// time.Time its text in timeLayout, and a driver.Valuer, such as the
// public package's Vector, what its Value method gives. Named types and
// pointers go through driver.DefaultParameterConverter as database/sql
// sends them, and so do the types it refuses, with its error. One of its
// quirks comes along too: a uint beyond 2^63 - 1 wraps around to a
// negative integer, where a uint64 that big is an error.
func Arg(v any) (value.Value, error) {
	if x, ok := bound(v); ok {
		return x, nil
	}
	dv, err := driver.DefaultParameterConverter.ConvertValue(v)
	if err != nil {
		return value.Value{}, err
	}
	if x, ok := bound(dv); ok {
		return x, nil
	}
	// A decimal, which database/sql hands on and go-sqlite3 can't bind.
	return value.Value{}, fmt.Errorf("unsupported type %T", dv)
}

// bound is what go-sqlite3 binds for one of database/sql's driver values.
func bound(v any) (value.Value, bool) {
	switch x := v.(type) {
	case nil:
		return value.Value{}, true
	case int64:
		return value.Int(x), true
	case int:
		return value.Int(int64(x)), true
	case float64:
		if math.IsNaN(x) {
			return value.Value{}, true
		}
		return value.Real(x), true
	case bool:
		return boolValue(x), true
	case string:
		return value.Text(x), true
	case []byte:
		if x == nil {
			return value.Value{}, true
		}
		return value.Bytes(string(x)), true
	case time.Time:
		if plant == "query/time-in-utc" {
			x = x.UTC()
		}
		return value.Text(x.Format(timeLayout)), true
	}
	return value.Value{}, false
}
