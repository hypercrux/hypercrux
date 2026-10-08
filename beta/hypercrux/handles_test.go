// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	c "github.com/hypercrux/hypercrux/beta/conformance"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The calls of both packages through one interface, handle, so a test
// makes each call on 0.x and on the Beta and compares what they give.

// zeroCalls is what 0.x's DB and Tx have in common, and betaCalls the
// Beta's.
type zeroCalls interface {
	Get(key string) (zx.Fields, error)
	Put(key string, f zx.Fields) error
	Delete(key string) error
	Scan(prefix, after string, limit int) ([]zx.Record, error)
	Link(from, typ, to string) error
	Unlink(from, typ, to string) error
	Neighbours(key string, dir zx.Direction, typ string) ([]zx.Link, error)
	Walk(key string, dir zx.Direction, typ string, depth int) ([]zx.Step, error)
	Nearest(table string, q zx.Vector, k int, where string, args ...any) ([]zx.Hit, error)
	Drop(table string) error
}

type betaCalls interface {
	Get(key string) (hc.Fields, error)
	Put(key string, f hc.Fields) error
	Delete(key string) error
	Scan(prefix, after string, limit int) ([]hc.Record, error)
	Link(from, typ, to string) error
	Unlink(from, typ, to string) error
	Neighbours(key string, dir hc.Direction, typ string) ([]hc.Link, error)
	Walk(key string, dir hc.Direction, typ string, depth int) ([]hc.Step, error)
	Nearest(table string, q hc.Vector, k int, where string, args ...any) ([]hc.Hit, error)
	Drop(table string) error
}

var (
	_ zeroCalls = (*zx.DB)(nil)
	_ zeroCalls = (*zx.Tx)(nil)
	_ betaCalls = (*hc.DB)(nil)
	_ betaCalls = (*hc.Tx)(nil)
)

// handle is a DB or a Tx of either package, with what each call gives
// turned into what the tests compare: fields as maps, searches as the
// suite's hits, and the rest as text, which shows nil apart from empty.
type handle interface {
	get(key string) (map[string]any, error)
	put(key string, f goFields) error
	delete(key string) error
	scan(prefix, after string, limit int) ([]keyed, error)
	link(from, typ, to string) error
	unlink(from, typ, to string) error
	neighbours(key string, dir int, typ string) (string, error)
	walk(key string, dir int, typ string, depth int) (string, error)
	nearest(table string, q []float32, k int, where string, args ...any) ([]c.Hit, error)
	drop(table string) error
}

type zeroOn struct{ h zeroCalls }

func (z zeroOn) get(key string) (map[string]any, error) {
	f, err := z.h.Get(key)
	return f, err
}
func (z zeroOn) put(key string, f goFields) error  { return z.h.Put(key, f.zero()) }
func (z zeroOn) delete(key string) error           { return z.h.Delete(key) }
func (z zeroOn) link(from, typ, to string) error   { return z.h.Link(from, typ, to) }
func (z zeroOn) unlink(from, typ, to string) error { return z.h.Unlink(from, typ, to) }
func (z zeroOn) drop(table string) error           { return z.h.Drop(table) }

func (z zeroOn) scan(prefix, after string, limit int) ([]keyed, error) {
	recs, err := z.h.Scan(prefix, after, limit)
	return records(recs, func(r zx.Record) keyed { return keyed{r.Key, r.Fields} }), err
}

func (z zeroOn) neighbours(key string, dir int, typ string) (string, error) {
	links, err := z.h.Neighbours(key, zx.Direction(dir), typ)
	return showList(links, func(l zx.Link) string { return fmt.Sprintf("%q %q %q", l.From, l.Type, l.To) }), err
}

func (z zeroOn) walk(key string, dir int, typ string, depth int) (string, error) {
	steps, err := z.h.Walk(key, zx.Direction(dir), typ, depth)
	return showList(steps, func(s zx.Step) string { return fmt.Sprintf("%q@%d", s.Key, s.Depth) }), err
}

func (z zeroOn) nearest(table string, q []float32, k int, where string, args ...any) ([]c.Hit, error) {
	hits, err := z.h.Nearest(table, zx.Vector(q), k, where, args...)
	return suiteHits(hits, func(h zx.Hit) c.Hit { return c.Hit(h) }), err
}

type betaOn struct{ h betaCalls }

func (b betaOn) get(key string) (map[string]any, error) {
	f, err := b.h.Get(key)
	return f, err
}
func (b betaOn) put(key string, f goFields) error  { return b.h.Put(key, f.beta()) }
func (b betaOn) delete(key string) error           { return b.h.Delete(key) }
func (b betaOn) link(from, typ, to string) error   { return b.h.Link(from, typ, to) }
func (b betaOn) unlink(from, typ, to string) error { return b.h.Unlink(from, typ, to) }
func (b betaOn) drop(table string) error           { return b.h.Drop(table) }

func (b betaOn) scan(prefix, after string, limit int) ([]keyed, error) {
	recs, err := b.h.Scan(prefix, after, limit)
	return records(recs, func(r hc.Record) keyed { return keyed{r.Key, r.Fields} }), err
}

func (b betaOn) neighbours(key string, dir int, typ string) (string, error) {
	links, err := b.h.Neighbours(key, hc.Direction(dir), typ)
	return showList(links, func(l hc.Link) string { return fmt.Sprintf("%q %q %q", l.From, l.Type, l.To) }), err
}

func (b betaOn) walk(key string, dir int, typ string, depth int) (string, error) {
	steps, err := b.h.Walk(key, hc.Direction(dir), typ, depth)
	return showList(steps, func(s hc.Step) string { return fmt.Sprintf("%q@%d", s.Key, s.Depth) }), err
}

func (b betaOn) nearest(table string, q []float32, k int, where string, args ...any) ([]c.Hit, error) {
	hits, err := b.h.Nearest(table, hc.Vector(q), k, where, args...)
	return suiteHits(hits, func(h hc.Hit) c.Hit { return c.Hit(h) }), err
}

// keyed is a record a scan gave, as both packages' records turn into.
type keyed struct {
	key    string
	fields map[string]any
}

// records turns a package's records into keyed ones, keeping nil as nil.
func records[R any](recs []R, conv func(R) keyed) []keyed {
	if recs == nil {
		return nil
	}
	out := make([]keyed, len(recs))
	for i, r := range recs {
		out[i] = conv(r)
	}
	return out
}

// showList writes a list of results, each by one, or nil for a nil list,
// so nil and empty never look the same.
func showList[T any](list []T, one func(T) string) string {
	if list == nil {
		return "nil"
	}
	parts := make([]string, len(list))
	for i, x := range list {
		parts[i] = one(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// suiteHits turns a package's hits into the suite's, keeping nil as nil.
func suiteHits[H any](hits []H, conv func(H) c.Hit) []c.Hit {
	if hits == nil {
		return nil
	}
	out := make([]c.Hit, len(hits))
	for i, h := range hits {
		out[i] = conv(h)
	}
	return out
}

// result is what a call gave: its error, and text that shows the error
// and the results apart from a search's hits, which are compared within
// the bound. folded is the text with every field's name in lower case.
type result struct {
	text, folded string
	hits         []c.Hit
	err          error
}

// do makes the call through h.
func do(h handle, cl call) result {
	var text, folded string
	var hits []c.Hit
	var err error
	switch cl.op {
	case "put":
		err = h.put(cl.key, cl.f)
	case "get":
		var f map[string]any
		f, err = h.get(cl.key)
		text, folded = show(f), show(foldNames(f))
	case "delete":
		err = h.delete(cl.key)
	case "scan":
		var recs []keyed
		recs, err = h.scan(cl.key, cl.after, cl.n)
		text = showList(recs, func(r keyed) string { return fmt.Sprintf("%q %s", r.key, show(r.fields)) })
		folded = showList(recs, func(r keyed) string { return fmt.Sprintf("%q %s", r.key, show(foldNames(r.fields))) })
	case "link":
		err = h.link(cl.key, cl.typ, cl.to)
	case "unlink":
		err = h.unlink(cl.key, cl.typ, cl.to)
	case "neighbours":
		text, err = h.neighbours(cl.key, cl.dir, cl.typ)
	case "walk":
		text, err = h.walk(cl.key, cl.dir, cl.typ, cl.n)
	case "nearest":
		hits, err = h.nearest(cl.key, cl.q, cl.n, cl.where, cl.args...)
		text = fmt.Sprintf("%d hits, nil %v", len(hits), hits == nil)
	case "drop":
		err = h.drop(cl.key)
	default:
		panic("a call of kind " + cl.op)
	}
	if folded == "" {
		folded = text
	}
	return result{describeErr(err) + " " + text, describeErr(err) + " " + folded, hits, err}
}

// foldNames gives the fields with each name in lower case, keeping nil as
// nil. Field names are ASCII, and match regardless of case.
func foldNames(f map[string]any) map[string]any {
	if f == nil {
		return nil
	}
	out := make(map[string]any, len(f))
	for k, v := range f {
		out[strings.ToLower(k)] = v
	}
	return out
}

// same fails the test unless the Beta's result is 0.x's.
func same(t *testing.T, step int, what string, beta, zero result) {
	t.Helper()
	sameOrRespelt(t, step, what, beta, zero, false)
}

// sameOrRespelt fails the test unless the Beta's result is 0.x's, or, when
// spelling is true, unless the two differ only in the case of field names,
// and reports whether they did. That's the difference
// TestAFailedPutLeavesNoField pins: a Put that failed inside an Update
// that went on left its new fields in 0.x, spelt its way, and nothing in
// the Beta.
func sameOrRespelt(t *testing.T, step int, what string, beta, zero result, spelling bool) bool {
	t.Helper()
	if err := c.CompareHits(beta.hits, zero.hits); err != nil {
		t.Fatalf("step %d: %s: the Beta's hits aren't 0.x's: %v", step, what, err)
	}
	switch {
	case beta.text == zero.text:
		return false
	case spelling && beta.folded == zero.folded:
		return true
	}
	t.Fatalf("step %d: %s: the Beta gives\n  %s\nand 0.x\n  %s", step, what, beta.text, zero.text)
	return false
}

var errRollback = errors.New("rolled back on purpose")

// runUpdate makes calls inside one Update through update, noting what each
// gave in said, and ends the function by ending: 0 returns an error, 1
// panics, and anything else commits.
func runUpdate(update func(fn func(h handle) error) error, calls []call, ending int, said *[]result) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	return update(func(h handle) error {
		for _, cl := range calls {
			*said = append(*said, do(h, cl))
		}
		switch ending {
		case 0:
			return errRollback
		case 1:
			panic("on purpose")
		}
		return nil
	})
}
