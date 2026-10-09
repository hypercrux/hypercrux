// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Walks and the one-record subquery, as the planner's Scope gives them to
// the evaluator (SQL.md, "Walks" and "The one-record subquery"):
//
//   - a walk follows the store's Walk, with SQL.md's rules for its
//     arguments, in 0.x's order and with 0.x's messages. A start that isn't
//     a record's key reaches nothing, where the store's Walk gives an error;
//   - walk() as a function gives the keys as JSON text, and is worked out
//     each time it's called, as 0.x registers it with SQLite as a function
//     that isn't deterministic;
//   - IN over a walk and the one-record subquery are worked out once for
//     each statement, the first time a row reaches them, as SQLite's
//     OP_Once runs a subquery that reads nothing of the row. Their answer,
//     an error included, is kept for every row after.

// walkArgs works out a walk's arguments, which read no field, in order.
func walkArgs(args []Eval, f *Frame) ([]value.Value, error) {
	vals := make([]value.Value, len(args))
	for i, a := range args {
		var err error
		if vals[i], err = a(f); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// walkSteps follows the walk that vals, a walk's 2 to 4 arguments, ask
// for, from r, checking them as 0.x's walk() does and in its order: the
// start is text, the depth an integer, the type NULL or text, the
// direction NULL or text that's out, in, both or empty, in any case, and
// then the depth from 1 to 32. NULL and empty text are every type and out.
// Each refusal is a *FuncError at pos with 0.x's message. A start that
// isn't a record's key, or isn't a valid key at all, reaches nothing.
func walkSteps(r store.Reader, pos int, vals []value.Value) ([]store.Step, error) {
	refuse := func(msg string) error { return &FuncError{Pos: pos, Func: "walk", Msg: msg} }
	if vals[0].Kind() != value.KindText {
		return nil, refuse("walk() needs a key as text")
	}
	if vals[1].Kind() != value.KindInt {
		return nil, refuse("walk() needs a whole number of links")
	}
	typ := ""
	if len(vals) > 2 && !vals[2].IsNull() {
		if vals[2].Kind() != value.KindText {
			return nil, refuse("walk() takes the link type as text")
		}
		typ = vals[2].Raw()
	}
	dir := store.Out
	if len(vals) > 3 && !vals[3].IsNull() {
		if vals[3].Kind() != value.KindText {
			return nil, refuse("walk() takes the direction as text: out, in or both")
		}
		switch s := strings.ToLower(vals[3].Raw()); s {
		case "out", "":
		case "in":
			dir = store.In
		case "both":
			dir = store.Both
		default:
			return nil, refuse("invalid: direction " + strconv.Quote(s) + ": use out, in or both")
		}
	}
	depth := vals[1].Int()
	if depth < 1 || depth > store.MaxDepth {
		return nil, refuse("invalid: depth is from 1 to 32, not " + strconv.FormatInt(depth, 10))
	}
	steps, err := r.Walk(vals[0].Raw(), dir, typ, int(depth))
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) || errors.Is(err, errs.ErrInvalid) {
			// The depth and the direction are good, so it's the start.
			return nil, nil
		}
		return nil, err
	}
	return steps, nil
}

// walkJSON is walk() as a function: the keys as Go's encoding/json writes a
// []string, and [] for none, as 0.x gives them.
func walkJSON(steps []store.Step) value.Value {
	keys := make([]string, len(steps))
	for i, s := range steps {
		keys[i] = s.Key
	}
	b, err := json.Marshal(keys)
	if err != nil {
		panic("query: a walk's keys don't make JSON: " + err.Error())
	}
	return value.Text(string(b))
}

// walkOnce is a walk worked out once for each statement: the keys it
// reaches as text values, for the evaluator's IN over a walk, and as a set,
// for the planner's own.
type walkOnce struct {
	r    store.Reader
	w    *Walk
	args []Eval

	mu   sync.Mutex
	done bool
	keys []value.Value
	set  map[string]struct{}
	err  error
}

// get works out the walk the first time, with f's arguments, and gives the
// same answer every time after.
func (o *walkOnce) get(f *Frame) (*walkOnce, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done {
		o.done = true
		o.err = o.run(f)
	}
	return o, o.err
}

func (o *walkOnce) run(f *Frame) error {
	vals, err := walkArgs(o.args, f)
	if err != nil {
		return err
	}
	steps, err := walkSteps(o.r, o.w.At, vals)
	if err != nil {
		return err
	}
	o.keys = make([]value.Value, len(steps))
	o.set = make(map[string]struct{}, len(steps))
	for i, s := range steps {
		o.keys[i] = value.Text(s.Key)
		o.set[s.Key] = struct{}{}
	}
	return nil
}

// has reports whether the walk reached the key k.
func (o *walkOnce) has(k string) bool {
	_, ok := o.set[k]
	return ok
}

// recordOnce is the one-record subquery worked out once for each
// statement.
type recordOnce struct {
	r     store.Reader
	table store.Table
	field int // the field's place in the table's list, or -1 for key
	key   Eval

	mu    sync.Mutex
	done  bool
	v     value.Value
	found bool
	err   error
}

func (o *recordOnce) get(f *Frame) (value.Value, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done || plant == "query/record-every-time" {
		o.done = true
		o.v, o.found, o.err = o.run(f)
	}
	return o.v, o.found, o.err
}

// run finds the record: its key is the text e gives, in the table, and the
// value is the field's, NULL where the record has none, or the key, or the
// vector as a copy. The subquery's WHERE key = e compares the key, which
// has text affinity in 0.x, with e: only text equal to a key finds it, and
// a key of another table finds nothing.
func (o *recordOnce) run(f *Frame) (value.Value, bool, error) {
	k, err := o.key(f)
	if err != nil {
		return null, false, err
	}
	if k.Kind() != value.KindText || plant != "query/record-of-any-table" && !strings.HasPrefix(k.Raw(), o.table.Name+":") {
		return null, false, nil
	}
	rec, ok, err := getRecord(o.r, k.Raw())
	if err != nil || !ok {
		return null, false, err
	}
	switch {
	case o.field < 0:
		return value.Text(rec.Key), true, nil
	case o.field == o.table.Vec:
		if rec.Vec == nil {
			return null, true, nil
		}
		return value.Vector(rec.Vec), true, nil
	}
	return rec.Field(o.field), true, nil
}

// getRecord reads the record with the key k from r, and reports false when
// there's none, or when k isn't a key at all, as SQL finds no row then.
// Any other error, such as a transaction that has ended, comes back.
func getRecord(r store.Reader, k string) (store.Record, bool, error) {
	rec, err := r.Get(k)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) || errors.Is(err, errs.ErrInvalid) {
			return store.Record{}, false, nil
		}
		return store.Record{}, false, err
	}
	return rec, true, nil
}

// inWalkCond is a WHERE term x IN or NOT IN over a walk, which the planner
// works out with a set of the walk's keys in place of the evaluator's list.
// It gives what the evaluator's inWalk gives: the walk first, then x; 1 when
// x is text equal to a key the walk reached; NULL for a NULL x, or 0 when
// the walk reached nothing; and 0 for anything else. NOT IN gives the
// opposite, with NULL staying NULL, and the term keeps a row when it's 1.
func inWalkCond(o *walkOnce, x Eval, not bool) Cond {
	return func(f *Frame) (bool, error) {
		if _, err := o.get(f); err != nil {
			return false, err
		}
		xv, err := x(f)
		if err != nil {
			return false, err
		}
		t := 0
		switch {
		case xv.IsNull() && len(o.keys) > 0:
			t = 2
			if plant == "query/null-not-in-walk-kept" && not {
				return true, nil
			}
		case xv.Kind() == value.KindText && o.has(xv.Raw()):
			t = 1
		}
		if not && t != 2 {
			t = 1 - t
		}
		return t == 1, nil
	}
}
