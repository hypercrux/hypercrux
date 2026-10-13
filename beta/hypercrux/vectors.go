// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Vector is a record's vector, such as an embedding of its text. It's the
// field vec, and holds float32 values. In SQL it's a blob of those values,
// little-endian, four bytes each, as 0.x stores it.
type Vector []float32

// MaxDims is the largest vector HyperCrux accepts.
const MaxDims = rules.MaxDims

// Value gives the vector to SQL as a blob, so a Vector can be passed
// straight to Query or Exec, as in distance(vec, ?).
func (v Vector) Value() (driver.Value, error) { return v.Bytes(), nil }

// Bytes returns the vector as a blob, the way SQL gives one out: each
// value's bits, little-endian, four bytes each.
func (v Vector) Bytes() []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// DecodeVector reads a vector from a blob, as Bytes writes it and SQL gives
// it out. It checks only the blob's length.
func DecodeVector(b []byte) (Vector, error) {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil, fmt.Errorf("a vector blob of %d bytes isn't whole float32 values", len(b))
	}
	v := make(Vector, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

// ParseVector reads a vector written as a JSON array of numbers, such as
// "[0.12, 0.8, 0.05]". Text that isn't such an array is refused with an
// error that wraps ErrInvalid, and so is a vector Put would refuse: no
// values, more than MaxDims of them, a value that isn't a finite float32,
// or only zeros.
func ParseVector(s string) (Vector, error) {
	var f []float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("%w: a vector is a JSON array of numbers: %v", ErrInvalid, err)
	}
	v := make(Vector, len(f))
	for i, x := range f {
		v[i] = float32(x)
	}
	return v, rules.Vector(v)
}

// String returns the vector as a JSON array, or "" when a value isn't
// finite, since JSON has no way to write it.
func (v Vector) String() string {
	b, _ := json.Marshal([]float32(v))
	return string(b)
}

// Hit is one result of Nearest.
type Hit struct {
	Key      string
	Distance float64 // cosine distance, from 0 (same direction) to 2
}

// MaxK is the most results Nearest returns.
const MaxK = store.MaxK

// Nearest returns the k records in table whose vectors are closest to q,
// closest first, by cosine distance, with ties in key order. The search is
// exact: every vector that passes the filter is compared. where is an
// optional condition on the table's fields in the Beta's SQL, such as
// "status = ?", as a WHERE over the table would take it, with one value in
// args for each ? mark. Its errors wrap neither ErrInvalid nor ErrNotFound,
// as a SELECT's don't. Without a filter, args aren't used. k runs from 1 to
// MaxK. A table with no vectors yet gives no hits, and one that doesn't
// exist gives ErrNotFound.
func (db *DB) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	var hits []Hit
	err := db.read(func(r store.Reader) error {
		var err error
		hits, err = nearest(r, table, q, k, where, args, db.moment)
		return err
	})
	return hits, err
}

// Nearest is DB.Nearest inside the transaction.
func (t *Tx) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	stx, err := t.open()
	if err != nil {
		return nil, err
	}
	return nearest(stx, table, q, k, where, args, t.db.moment)
}

// nearest searches r as 0.x's Nearest does, with its checks in its order and
// its errors (SQL.md, "Nearest's filter"):
//
//   - the table's name, the query vector and k, then the table, which has to
//     exist, then its vector size: a table with none yet gives nil, before
//     anything else is looked at, the arguments and the filter included;
//     then the query's size against the table's (searchable);
//   - the arguments, each made a value as a statement's are, with
//     database/sql's words for one it can't take (searchArgs), since 0.x
//     hands them to database/sql with its query, filter or no filter;
//   - the filter, unless where is only spaces: ParseCondition reads it, and
//     NearestFilter plans it as 0.x's query over the table, with the
//     arguments in its ? marks, exactly one for each, and the moment read
//     once for the call. When its terms that read no field rule every record
//     out, there are no hits, and an empty list, as in 0.x;
//   - the store's search, with the filter, which works out the filter's
//     other terms for each record with a vector.
//
// Without a filter the arguments aren't used, as in 0.x, where they fill no
// mark. A search that finds nothing gives an empty list. The hits come in a
// list of their own, and their keys are the records' own strings, which
// never change.
func nearest(r store.Reader, table string, q Vector, k int, where string, args []any, now func() time.Time) ([]Hit, error) {
	if plant == "hypercrux/filter-checks-reordered" {
		if _, _, err := query.ParseCondition(where); err != nil {
			return nil, err
		}
	}
	size, err := searchable(r, table, q, k)
	if err != nil || size == 0 {
		return nil, err
	}
	vals, names, err := searchArgs(args)
	if err != nil {
		return nil, err
	}
	cond, params, err := query.ParseCondition(where)
	if err != nil {
		return nil, err
	}
	var keep store.Filter
	if cond != nil && plant != "hypercrux/filter-ignored" {
		if err := unnamed(names); err != nil {
			return nil, err
		}
		var none bool
		keep, none, err = query.NearestFilter(r, table, cond, params, &query.Frame{Args: vals, Now: now()})
		if err != nil {
			return nil, err
		}
		if none {
			if plant == "hypercrux/filter-none-nil" {
				return nil, nil
			}
			return []Hit{}, nil
		}
	}
	found, err := r.Nearest(table, []float32(q), k, keep)
	if err != nil || found == nil {
		return nil, err
	}
	if len(found) == 0 && plant == "hypercrux/nearest-nil-for-none" {
		return nil, nil
	}
	hits := make([]Hit, len(found))
	for i, h := range found {
		hits[i] = Hit(h)
	}
	return hits, nil
}

// searchable makes 0.x's checks of a search, in 0.x's order and with its
// errors, which are the store's Nearest's too: the table's name, the query
// vector, k, then the table, which has to exist, and the query's size
// against the table's vector size. It gives that size, which is 0 for a
// table with no vectors yet, whatever the query's size.
func searchable(r store.Reader, table string, q Vector, k int) (int, error) {
	if err := rules.Table(table); err != nil {
		return 0, err
	}
	if err := rules.Vector([]float32(q)); err != nil {
		return 0, err
	}
	if k < 1 || k > MaxK {
		return 0, fmt.Errorf("%w: k is from 1 to %d, not %d", ErrInvalid, MaxK, k)
	}
	t, ok := r.Table(table)
	switch {
	case !ok:
		return 0, fmt.Errorf("%w: no record table %s", ErrNotFound, table)
	case t.Size != 0 && t.Size != len(q):
		return 0, fmt.Errorf("%w: table %s holds vectors of %d values, and the query has %d", ErrInvalid, table, t.Size, len(q))
	}
	return t.Size, nil
}

// searchArgs makes a search's arguments values, as database/sql and the
// driver make a statement's (argument): a sql.NamedArg gives its value and
// its name, which has to start with a letter, and a value database/sql or
// go-sqlite3 can't take is refused in database/sql's words, which are
// 0.x's. It gives the names too, for the driver's check, since only a
// filter has marks for the arguments to fill (unnamed).
func searchArgs(args []any) ([]value.Value, []driver.NamedValue, error) {
	vals := make([]value.Value, len(args))
	names := make([]driver.NamedValue, len(args))
	for i, a := range args {
		names[i].Ordinal = i + 1
		if na, ok := a.(sql.NamedArg); ok {
			if r, _ := utf8.DecodeRuneInString(na.Name); na.Name != "" && !unicode.IsLetter(r) {
				return nil, nil, fmt.Errorf("name %q does not begin with a letter", na.Name)
			}
			names[i].Name, a = na.Name, na.Value
		}
		v, err := argument(a)
		if err != nil {
			where := fmt.Sprintf("$%d", i+1)
			if names[i].Name != "" {
				where = fmt.Sprintf("with name %q", names[i].Name)
			}
			return nil, nil, fmt.Errorf("sql: converting argument %s type: %w", where, err)
		}
		vals[i] = v
	}
	return vals, names, nil
}
