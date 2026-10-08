// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	zero "github.com/hypercrux/hypercrux"
	beta "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The two engines, each through its public package: 0.x from the root of
// the repository, as it is now, and the Beta from beta/hypercrux. A
// benchmark is written once, against engine, db and tx, and runs on both.
// Each call goes straight to the package's own, with the package's own
// Fields and Vector. A map[string]any becomes Fields and a []float32 a
// Vector without a copy, since those are their types underneath, so the
// work timed is the package's, as in 0.1's benchmarks.

// The engines' names, which end each sub-benchmark's name.
const (
	zeroName = "0.x"
	betaName = "Beta"
)

// An engine is one of the two packages.
type engine struct {
	name string
	open func(path string) (db, error)
	// vector boxes v as the package's Vector, for the field vec, as 0.1's
	// benchmarks put a Vector in Fields.
	vector func(v []float32) any
	// inMemory says the database is read into memory as it opens, and held
	// there until it's closed. The Beta does that; 0.x reads the file as it
	// goes.
	inMemory bool
}

var engines = []engine{
	{name: zeroName, open: openZero, vector: func(v []float32) any { return zero.Vector(v) }},
	{name: betaName, open: openBeta, vector: func(v []float32) any { return beta.Vector(v) }, inMemory: true},
}

// db is an open database, with the calls the benchmarks make.
type db interface {
	update(fn func(t tx) error) error
	put(key string, f map[string]any) error
	get(key string) error
	link(from, typ, to string) error
	// walk follows links out of key, of every type, and gives the number of
	// records it reached.
	walk(key string, depth int) (int, error)
	// nearest gives the number of hits.
	nearest(table string, q []float32, k int, where string, args ...any) (int, error)
	close() error
}

// tx is a transaction inside update.
type tx interface {
	put(key string, f map[string]any) error
	link(from, typ, to string) error
}

// compacter is a db that can rewrite its file with only the live data: the
// Beta's, once Compact works (task G5).
type compacter interface {
	compact() error
}

// 0.x.

type zeroDB struct{ db *zero.DB }

type zeroTx struct{ tx *zero.Tx }

func openZero(path string) (db, error) {
	d, err := zero.Open(path)
	if err != nil {
		return nil, err
	}
	return zeroDB{d}, nil
}

func (d zeroDB) update(fn func(t tx) error) error {
	return d.db.Update(func(t *zero.Tx) error { return fn(zeroTx{t}) })
}

func (d zeroDB) put(key string, f map[string]any) error { return d.db.Put(key, f) }

func (d zeroDB) get(key string) error {
	_, err := d.db.Get(key)
	return err
}

func (d zeroDB) link(from, typ, to string) error { return d.db.Link(from, typ, to) }

func (d zeroDB) walk(key string, depth int) (int, error) {
	steps, err := d.db.Walk(key, zero.Out, "", depth)
	return len(steps), err
}

func (d zeroDB) nearest(table string, q []float32, k int, where string, args ...any) (int, error) {
	hits, err := d.db.Nearest(table, q, k, where, args...)
	return len(hits), err
}

func (d zeroDB) close() error { return d.db.Close() }

func (t zeroTx) put(key string, f map[string]any) error { return t.tx.Put(key, f) }

func (t zeroTx) link(from, typ, to string) error { return t.tx.Link(from, typ, to) }

// The Beta.

type betaDB struct{ db *beta.DB }

type betaTx struct{ tx *beta.Tx }

func openBeta(path string) (db, error) {
	d, err := beta.Open(path)
	if err != nil {
		return nil, err
	}
	return betaDB{d}, nil
}

func (d betaDB) update(fn func(t tx) error) error {
	return d.db.Update(func(t *beta.Tx) error { return fn(betaTx{t}) })
}

func (d betaDB) put(key string, f map[string]any) error { return d.db.Put(key, f) }

func (d betaDB) get(key string) error {
	_, err := d.db.Get(key)
	return err
}

func (d betaDB) link(from, typ, to string) error { return d.db.Link(from, typ, to) }

func (d betaDB) walk(key string, depth int) (int, error) {
	steps, err := d.db.Walk(key, beta.Out, "", depth)
	return len(steps), err
}

func (d betaDB) nearest(table string, q []float32, k int, where string, args ...any) (int, error) {
	hits, err := d.db.Nearest(table, q, k, where, args...)
	return len(hits), err
}

func (d betaDB) compact() error { return d.db.Compact() }

func (d betaDB) close() error { return d.db.Close() }

func (t betaTx) put(key string, f map[string]any) error { return t.tx.Put(key, f) }

func (t betaTx) link(from, typ, to string) error { return t.tx.Link(from, typ, to) }
