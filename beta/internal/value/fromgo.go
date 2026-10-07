// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

// FromGo converts v, given for the field named field, into a Value, the
// way 0.x's Put converts the values of a Fields map:
//
//   - nil: null. A nil pointer is null too, in any field but the vector
//     field.
//   - A string: text, which must be valid UTF-8.
//   - A bool: the whole number 1 or 0.
//   - Any integer type: a whole number. A uint, uint64 or uintptr beyond
//     the largest int64 is refused.
//   - A float32 or float64: a real number, refused when it's NaN or
//     infinite. A float32 becomes the float64 of the same value.
//   - A json.Number: a whole number when it reads as an int64, or else a
//     real number.
//   - A []byte: bytes, copied.
//   - A time.Time: text, in RFC 3339 with nanoseconds, in UTC.
//   - A map, slice, array or struct: text holding its JSON, as
//     encoding/json writes it.
//   - A named type is converted as the type it's based on, and a pointer
//     as what it points to.
//   - In the vector field, whose name matches "vec" regardless of case: a
//     []float32, the public package's Vector, a []float64, a []any of
//     float64s and json.Numbers, or a string holding a JSON array of
//     numbers. Each gives a vector, which must have 1 to 65,536 finite
//     values that aren't all zero. Anything else is refused there,
//     pointers and other named types included, as in 0.x.
//   - In any other field, a []float32, a []float64 or the public
//     package's Vector is refused, since it holds a vector.
//
// What it refuses gives an error that wraps errs.ErrInvalid, with 0.x's
// wording. The field's name is for that wording and for finding the
// vector field. Checking the name itself is the caller's job, with
// rules.Field.
//
// The public package's Vector is a named type based on []float32. This
// package can't import the public package, so FromGo knows the type by
// its package's path and its name.
func FromGo(field string, v any) (Value, error) {
	if rules.IsVec(field) && v != nil {
		vec, err := toVector(v)
		if err != nil {
			return Value{}, err
		}
		if err := rules.Vector(vec); err != nil {
			return Value{}, err
		}
		return Vector(vec), nil
	}
	switch x := v.(type) {
	case nil:
		return Value{}, nil
	case string:
		if err := rules.Text(field, x); err != nil {
			return Value{}, err
		}
		return Text(x), nil
	case bool:
		if x {
			return Int(1), nil
		}
		return Int(0), nil
	case int:
		return Int(int64(x)), nil
	case int8:
		return Int(int64(x)), nil
	case int16:
		return Int(int64(x)), nil
	case int32:
		return Int(int64(x)), nil
	case int64:
		return Int(x), nil
	case uint:
		return fromUint(field, uint64(x))
	case uint8:
		return Int(int64(x)), nil
	case uint16:
		return Int(int64(x)), nil
	case uint32:
		return Int(int64(x)), nil
	case uint64:
		return fromUint(field, x)
	case float32:
		return fromFloat(field, float64(x))
	case float64:
		return fromFloat(field, x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return Int(i), nil
		}
		f, err := x.Float64()
		if err != nil {
			return Value{}, fmt.Errorf("%w: field %s: %v", errs.ErrInvalid, field, err)
		}
		return fromFloat(field, f)
	case []byte:
		return Bytes(string(x)), nil
	case time.Time:
		return Text(x.UTC().Format(time.RFC3339Nano)), nil
	case []float32, []float64:
		return Value{}, rules.VectorElsewhere(field)
	case map[string]any, []any:
		return fromJSON(field, x)
	}
	// Named types, such as type Status string, and other maps, slices and
	// structs.
	rv := reflect.ValueOf(v)
	if isPublicVector(rv.Type()) {
		return Value{}, rules.VectorElsewhere(field)
	}
	switch rv.Kind() {
	case reflect.String:
		return FromGo(field, rv.String())
	case reflect.Bool:
		return FromGo(field, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Int(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fromUint(field, rv.Uint())
	case reflect.Float32, reflect.Float64:
		return fromFloat(field, rv.Float())
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		return fromJSON(field, v)
	case reflect.Pointer:
		if rv.IsNil() {
			return Value{}, nil
		}
		return FromGo(field, rv.Elem().Interface())
	}
	return Value{}, fmt.Errorf("%w: field %s has a %T, which HyperCrux doesn't store", errs.ErrInvalid, field, v)
}

func fromUint(field string, x uint64) (Value, error) {
	if x > 1<<63-1 {
		return Value{}, fmt.Errorf("%w: field %s is %d, beyond SQLite's integers", errs.ErrInvalid, field, x)
	}
	return Int(int64(x)), nil
}

func fromFloat(field string, f float64) (Value, error) {
	if err := rules.Real(field, f); err != nil {
		return Value{}, err
	}
	return Real(f), nil
}

// fromJSON stores a map, slice, array or struct as JSON text, which
// encoding/json always writes in valid UTF-8.
func fromJSON(field string, v any) (Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Value{}, fmt.Errorf("%w: field %s: %v", errs.ErrInvalid, field, err)
	}
	return Text(string(b)), nil
}

// toVector is 0.x's: it reads what the vector field takes, and leaves the
// checks to rules.Vector.
func toVector(x any) ([]float32, error) {
	switch v := x.(type) {
	case []float32:
		return v, nil
	case []float64:
		out := make([]float32, len(v))
		for i, f := range v {
			out[i] = float32(f)
		}
		return out, nil
	case []any:
		out := make([]float32, len(v))
		for i, e := range v {
			switch n := e.(type) {
			case float64:
				out[i] = float32(n)
			case json.Number:
				f, err := n.Float64()
				if err != nil {
					return nil, fmt.Errorf("%w: vec value %d: %v", errs.ErrInvalid, i, err)
				}
				out[i] = float32(f)
			default:
				return nil, fmt.Errorf("%w: vec value %d is a %T, not a number", errs.ErrInvalid, i, e)
			}
		}
		return out, nil
	case string:
		var f []float64
		if err := json.Unmarshal([]byte(v), &f); err != nil {
			return nil, fmt.Errorf("%w: a vector is a JSON array of numbers: %v", errs.ErrInvalid, err)
		}
		out := make([]float32, len(f))
		for i, x := range f {
			out[i] = float32(x)
		}
		return out, nil
	}
	if isPublicVector(reflect.TypeOf(x)) {
		return reflect.ValueOf(x).Convert(float32s).Interface().([]float32), nil
	}
	return nil, fmt.Errorf("%w: vec takes a Vector, not a %T", errs.ErrInvalid, x)
}

// publicPackage is the path of the Beta's public package, whose Vector
// FromGo takes as a []float32.
const publicPackage = "github.com/hypercrux/hypercrux/beta/hypercrux"

var float32s = reflect.TypeOf([]float32(nil))

// isPublicVector reports whether t is the public package's Vector.
func isPublicVector(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem() == float32s.Elem() && t.Name() == "Vector" && t.PkgPath() == publicPackage
}
