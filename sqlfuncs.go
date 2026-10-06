// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// registerFunctions adds HyperCrux's SQL functions to a new connection:
//
//	distance(a, b)              cosine distance between two vectors
//	vector(json)                a JSON array of numbers as a stored vector
//	walk(key, depth [, type [, direction]])
//	                            the keys reachable by links, as a JSON array
//
// Vectors can be given as stored blobs or as JSON arrays.
func registerFunctions(c *sqlite3.SQLiteConn) error {
	if err := c.RegisterFunc("distance", newDistanceFunc(), true); err != nil {
		return err
	}
	if err := c.RegisterFunc("vector", sqlVector, true); err != nil {
		return err
	}
	return c.RegisterFunc("walk", newWalkFunc(c), false)
}

// null reports whether a function argument is SQL NULL. go-sqlite3 passes
// NULL as a nil []byte.
func null(x any) bool {
	if b, ok := x.([]byte); ok {
		return b == nil
	}
	return x == nil
}

// vectorArg turns a function argument into stored vector bytes. JSON text is
// parsed once and remembered, because a query compares the same query
// vector with every row.
type vectorArg struct {
	text  string
	bytes []byte
}

func (va *vectorArg) get(x any) ([]byte, error) {
	switch v := x.(type) {
	case []byte:
		return v, nil
	case string:
		if va.bytes != nil && va.text == v {
			return va.bytes, nil
		}
		vec, err := ParseVector(v)
		if err != nil {
			return nil, err
		}
		va.text, va.bytes = v, vec.Bytes()
		return va.bytes, nil
	}
	return nil, fmt.Errorf("hypercrux: a vector is a blob or a JSON array, not %T", x)
}

func newDistanceFunc() func(a, b any) (any, error) {
	var va, vb vectorArg
	return func(a, b any) (any, error) {
		if null(a) || null(b) {
			return nil, nil
		}
		ab, err := va.get(a)
		if err != nil {
			return nil, err
		}
		bb, err := vb.get(b)
		if err != nil {
			return nil, err
		}
		return cosine(ab, bb)
	}
}

func sqlVector(x any) (any, error) {
	if null(x) {
		return nil, nil
	}
	switch v := x.(type) {
	case string:
		vec, err := ParseVector(v)
		if err != nil {
			return nil, err
		}
		return vec.Bytes(), nil
	case []byte:
		vec, err := DecodeVector(v)
		if err != nil {
			return nil, err
		}
		if err := checkVector(vec); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, fmt.Errorf("hypercrux: vector() takes a JSON array, not %T", x)
}

// newWalkFunc returns walk() for one connection. It runs the walk query on
// that same connection, so inside a transaction it sees the transaction's
// own changes.
func newWalkFunc(c *sqlite3.SQLiteConn) func(args ...any) (any, error) {
	return func(args ...any) (any, error) {
		if len(args) < 2 || len(args) > 4 {
			return nil, fmt.Errorf("hypercrux: walk(key, depth [, type [, direction]]) takes 2 to 4 arguments")
		}
		key, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("hypercrux: walk() needs a key as text")
		}
		depth, ok := args[1].(int64)
		if !ok {
			return nil, fmt.Errorf("hypercrux: walk() needs a whole number of links")
		}
		typ := ""
		if len(args) > 2 && !null(args[2]) {
			if typ, ok = args[2].(string); !ok {
				return nil, fmt.Errorf("hypercrux: walk() takes the link type as text")
			}
		}
		dir := Out
		if len(args) > 3 && !null(args[3]) {
			s, ok := args[3].(string)
			if !ok {
				return nil, fmt.Errorf("hypercrux: walk() takes the direction as text: out, in or both")
			}
			var err error
			if dir, err = ParseDirection(strings.ToLower(s)); err != nil {
				return nil, err
			}
		}
		query, err := walkSQL(dir, int(depth))
		if err != nil {
			return nil, err
		}
		rows, err := c.Query(query, []driver.Value{key, depth, typ})
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		keys := []string{}
		dest := make([]driver.Value, 2)
		for {
			err := rows.Next(dest)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			switch k := dest[0].(type) {
			case string:
				keys = append(keys, k)
			case []byte:
				keys = append(keys, string(k))
			}
		}
		b, err := json.Marshal(keys)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
}
