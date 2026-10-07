// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package difftest

import (
	"math"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
)

// breakages are the ways broken gets 0.x wrong, each a slip an engine could
// plausibly make.
var breakages = []string{
	"unlink takes every type", // Unlink with a type removes the links of every type
	"nearest drops one",       // Nearest leaves out its last hit
	"scan stops early",        // Scan with a limit returns one record fewer
	"negative zero",           // Put stores -0 as 0
	"walk too short",          // Walk leaves out the furthest record
	"rollback keeps puts",     // a Put inside an Update survives its rollback
}

// broken is 0.x with one thing done wrong.
type broken struct {
	zerox.Engine
	how string
}

func (b broken) Name() string { return "0.x with " + b.how }

func (b broken) Open(path string) (c.DB, error) {
	db, err := b.Engine.Open(path)
	if err != nil {
		return nil, err
	}
	return brokenDB{db, brokenHandle{db, b.how, nil}}, nil
}

type brokenHandle struct {
	c.Handle
	how string
	db  c.DB // set inside an Update, for the rollback breakage
}

func (h brokenHandle) Unlink(from, typ, to string) error {
	if h.how == "unlink takes every type" {
		typ = ""
	}
	return h.Handle.Unlink(from, typ, to)
}

func (h brokenHandle) Nearest(table string, q c.Vector, k int, where string, args ...any) ([]c.Hit, error) {
	hits, err := h.Handle.Nearest(table, q, k, where, args...)
	if h.how == "nearest drops one" && len(hits) > 1 {
		hits = hits[:len(hits)-1]
	}
	return hits, err
}

func (h brokenHandle) Scan(prefix, after string, limit int) ([]c.Record, error) {
	if h.how == "scan stops early" && limit > 1 {
		limit--
	}
	return h.Handle.Scan(prefix, after, limit)
}

func (h brokenHandle) Put(key string, f c.Fields) error {
	if h.how == "negative zero" {
		g := c.Fields{}
		for k, v := range f {
			if x, ok := v.(float64); ok && x == 0 && math.Signbit(x) {
				v = 0.0
			}
			g[k] = v
		}
		f = g
	}
	if h.how == "rollback keeps puts" && h.db != nil {
		// Written straight to the database, outside the transaction, after
		// it ends: the transaction holds the write lock until then.
		pending = append(pending, func() { h.db.Put(key, f) })
	}
	return h.Handle.Put(key, f)
}

func (h brokenHandle) Walk(key string, d c.Direction, typ string, depth int) ([]c.Step, error) {
	steps, err := h.Handle.Walk(key, d, typ, depth)
	if h.how == "walk too short" && len(steps) > 1 {
		steps = steps[:len(steps)-1]
	}
	return steps, err
}

// pending holds the writes the rollback breakage makes after an Update.
// Tests in this package don't run in parallel, so one list will do.
var pending []func()

type brokenDB struct {
	c.DB
	h brokenHandle
}

func (d brokenDB) Unlink(from, typ, to string) error { return d.h.Unlink(from, typ, to) }
func (d brokenDB) Nearest(table string, q c.Vector, k int, where string, args ...any) ([]c.Hit, error) {
	return d.h.Nearest(table, q, k, where, args...)
}
func (d brokenDB) Scan(prefix, after string, limit int) ([]c.Record, error) {
	return d.h.Scan(prefix, after, limit)
}
func (d brokenDB) Put(key string, f c.Fields) error { return d.h.Put(key, f) }
func (d brokenDB) Walk(key string, dir c.Direction, typ string, depth int) ([]c.Step, error) {
	return d.h.Walk(key, dir, typ, depth)
}

func (d brokenDB) Update(fn func(tx c.Handle) error) error {
	pending = nil
	err := d.DB.Update(func(tx c.Handle) error {
		return fn(brokenHandle{tx, d.h.how, d.DB})
	})
	if err != nil {
		for _, w := range pending {
			w()
		}
	}
	pending = nil
	return err
}
