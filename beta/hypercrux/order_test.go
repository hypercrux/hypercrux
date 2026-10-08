// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// TestTheCallsCheckIn0xsOrder makes Scan, Link, Unlink, Neighbours, Walk,
// Nearest and Drop with arguments that break several rules at once, so
// that the order of the checks shows, and with arguments at the edges of
// what each call gives: nil for none, an empty list, a record with no
// fields, a search of a table with no vector size and of one whose vectors
// have all gone, and Nearest's filter given as spaces, with arguments it
// doesn't use. Each call goes first through a transaction, in an Update
// that then rolls back, and then through the database, on 0.x and on the
// Beta, and must give 0.x's answer: the same error, with the same message
// and of the same kind, or the same results, nil where 0.x gives nil.
func TestTheCallsCheckIn0xsOrder(t *testing.T) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	b := open(t, filepath.Join(dir, "beta.hcx"))

	setUp := []call{
		{op: "put", key: "docs:1", f: goFields{"title": "one", "n": 1, "vec": vec{1, 0}}},
		{op: "put", key: "docs:2", f: goFields{"vec": vec{0, 1}}},
		{op: "put", key: "docs:3", f: goFields{"raw": []byte{0, 1}}},
		{op: "put", key: "people:1"},
		{op: "put", key: "people:2", f: goFields{"name": "Sam"}},
		{op: "put", key: "emptied:1", f: goFields{"vec": vec{1, 2, 3}}},
		{op: "delete", key: "emptied:1"},
		{op: "put", key: "plain:1", f: goFields{"a": 1}},
		{op: "link", key: "docs:1", typ: "cites", to: "docs:2"},
		{op: "link", key: "docs:1", typ: "owns", to: "people:1"},
		{op: "link", key: "docs:2", typ: "cites", to: "docs:3"},
		{op: "link", key: "docs:3", typ: "cites", to: "docs:3"},
		{op: "link", key: "people:1", typ: "knows", to: "docs:1"},
	}
	for _, cl := range setUp {
		zr, br := do(zeroOn{z}, cl), do(betaOn{b}, cl)
		if zr.err != nil {
			t.Fatalf("setting up, %s: %v", cl, zr.err)
		}
		same(t, 0, "setting up, "+cl.String(), br, zr)
	}

	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	long := strings.Repeat("ü", 200)
	cases := []call{
		// Scan: the prefix's colon, then its table, then the limit.
		{op: "scan", key: "docs", n: -1},
		{op: "scan", key: "", n: -1},
		{op: "scan", key: "Docs:", n: -1},
		{op: "scan", key: "hc_x:", n: -1},
		{op: "scan", key: "sqlite_x:", n: -1},
		{op: "scan", key: "a-b:x", n: -1},
		{op: "scan", key: ":", n: -1},
		{op: "scan", key: "docs:", after: "Bad", n: -1},
		{op: "scan", key: "nosuch:", n: -1},
		{op: "scan", key: "nosuch:"},
		{op: "scan", key: "docs:"},
		{op: "scan", key: "docs:", n: 2},
		{op: "scan", key: "docs:", after: "docs:1", n: 1},
		{op: "scan", key: "docs:", after: "docs:3"},
		{op: "scan", key: "docs:", after: "a"},
		{op: "scan", key: "docs:", after: "zzz"},
		{op: "scan", key: "docs:2"},
		{op: "scan", key: "people:"},
		{op: "scan", key: "emptied:"},
		{op: "scan", key: "docs:\xff"},
		// Neighbours: the key, then the direction; the type isn't checked.
		{op: "neighbours", key: "Bad", dir: 7},
		{op: "neighbours", key: "docs:404", dir: 7},
		{op: "neighbours", key: "docs:1", dir: 7},
		{op: "neighbours", key: "docs:1", dir: -1},
		{op: "neighbours", key: "docs:1", dir: int(hc.Out), typ: strings.Repeat("x", 300)},
		{op: "neighbours", key: "docs:1", dir: int(hc.Out), typ: "\xff"},
		{op: "neighbours", key: "docs:1", dir: int(hc.Both)},
		{op: "neighbours", key: "docs:3", dir: int(hc.Both)},
		{op: "neighbours", key: "docs:1", dir: int(hc.In), typ: "knows"},
		{op: "neighbours", key: "people:2", dir: int(hc.Both)},
		// Walk: the depth, then the direction, then the key.
		{op: "walk", key: "Bad", dir: 7, n: 0},
		{op: "walk", key: "Bad", dir: 7, n: hc.MaxDepth + 1},
		{op: "walk", key: "Bad", dir: 7, n: -1},
		{op: "walk", key: "Bad", dir: 7, n: 1},
		{op: "walk", key: "Bad", dir: int(hc.Out), n: 1},
		{op: "walk", key: "docs:404", dir: int(hc.Out), n: 1},
		{op: "walk", key: "people:2", dir: int(hc.Both), n: hc.MaxDepth},
		{op: "walk", key: "docs:1", dir: int(hc.Both), n: hc.MaxDepth},
		{op: "walk", key: "docs:1", dir: int(hc.Out), typ: "cites", n: 2},
		{op: "walk", key: "docs:1", dir: int(hc.In), n: 1},
		{op: "walk", key: "docs:1", dir: int(hc.Out), typ: "\xff", n: 3},
		// Nearest: the table's name, the query, k, the table, then the
		// query's size.
		{op: "nearest", key: "Bad", n: 0},
		{op: "nearest", key: "docs", n: 0},
		{op: "nearest", key: "docs", q: []float32{nan}, n: 0},
		{op: "nearest", key: "docs", q: []float32{0, 0}, n: 0},
		{op: "nearest", key: "docs", q: []float32{inf, 1}, n: 0},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: 0},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: -1},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: hc.MaxK + 1},
		{op: "nearest", key: "nosuch", q: []float32{1}, n: 0},
		{op: "nearest", key: "nosuch", q: []float32{1}, n: 1},
		{op: "nearest", key: "plain", q: []float32{1}, n: 1},
		{op: "nearest", key: "docs", q: []float32{1, 2, 3}, n: 1},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: 5},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: hc.MaxK},
		{op: "nearest", key: "emptied", q: []float32{1, 2, 3}, n: 3},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: 1, where: " "},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: 1, where: "\t\n"},
		{op: "nearest", key: "docs", q: []float32{1, 2}, n: 2, args: []any{1, "open"}},
		// Link: the type, then from, then to.
		{op: "link", key: "Bad", typ: "", to: "docs:404"},
		{op: "link", key: "Bad", typ: "\xff", to: "Bad"},
		{op: "link", key: "Bad", typ: long + "ü", to: "Bad"},
		{op: "link", key: "Bad", typ: "t", to: "Bad2"},
		{op: "link", key: "docs:404", typ: "t", to: "Bad"},
		{op: "link", key: "docs:1", typ: "t", to: "Bad"},
		{op: "link", key: "docs:1", typ: "t", to: "docs:404"},
		{op: "link", key: "docs:1", typ: "cites", to: "docs:2"},
		{op: "link", key: "docs:2", typ: "new", to: "docs:1"},
		{op: "link", key: "docs:1", typ: long, to: "docs:1"},
		{op: "link", key: "people:2", typ: "a\x00b", to: "docs:3"},
		{op: "neighbours", key: "docs:1", dir: int(hc.Both)},
		// Unlink checks nothing, and anything wrong is a link not found.
		{op: "unlink", key: "Bad", typ: "", to: "Bad2"},
		{op: "unlink", key: "Bad", typ: "\xff", to: "docs:1"},
		{op: "unlink", key: "docs:1", typ: "", to: "docs:404"},
		{op: "unlink", key: "docs:1", typ: "nosuch", to: "docs:2"},
		{op: "unlink", key: "docs:1", typ: "cites", to: "docs:2"},
		{op: "unlink", key: "docs:1", typ: "cites", to: "docs:2"},
		{op: "unlink", key: "docs:1", typ: "", to: "docs:1"},
		{op: "neighbours", key: "docs:1", dir: int(hc.Both)},
		// Drop: the name, then the table; links into the table go with it.
		{op: "drop", key: ""},
		{op: "drop", key: "Bad"},
		{op: "drop", key: "hc_x"},
		{op: "drop", key: "nosuch"},
		{op: "drop", key: "emptied"},
		{op: "drop", key: "emptied"},
		{op: "nearest", key: "emptied", q: []float32{1, 2}, n: 1},
		{op: "drop", key: "people"},
		{op: "neighbours", key: "docs:1", dir: int(hc.Both)},
		{op: "walk", key: "docs:3", dir: int(hc.Both), n: hc.MaxDepth},
		{op: "scan", key: "people:"},
	}
	kinds := map[string]bool{}
	for i, cl := range cases {
		// Through a transaction, in an Update that rolls back, so the call
		// through the database next meets the same state.
		var zr, br result
		z.Update(func(tx *zx.Tx) error { zr = do(zeroOn{tx}, cl); return errRollback })
		b.Update(func(tx *hc.Tx) error { br = do(betaOn{tx}, cl); return errRollback })
		same(t, i, "through a transaction, "+cl.String(), br, zr)
		zr, br = do(zeroOn{z}, cl), do(betaOn{b}, cl)
		same(t, i, "through the database, "+cl.String(), br, zr)
		kinds[fmt.Sprint(cl.op, " ", kindOf(zr.err))] = true
		if testing.Verbose() {
			t.Logf("%s: %s", cl, zr.text)
		}
	}
	// The cases reach each kind of answer.
	for _, want := range []string{"scan ok", "scan invalid", "neighbours ok", "neighbours invalid", "neighbours not found",
		"walk ok", "walk invalid", "walk not found", "nearest ok", "nearest invalid", "nearest not found", "link ok",
		"link invalid", "link not found", "unlink not found", "drop invalid", "drop not found"} {
		if !kinds[want] {
			t.Errorf("no case gives %q now: %v", want, kinds)
		}
	}
}
