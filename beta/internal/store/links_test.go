// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// 0.x's cases for links and walks, from its tests and the conformance
// suite, each run on 0.x and on the store side by side, as S1's and S4's
// tests run theirs. A link, an unlink, Neighbours and Walk must give the
// same error from both, with the same message, or the same links or steps
// in the same order. The store's Link and Unlink are what the public
// package's will call, with nothing to convert, so they're called as they
// are.

// pair is 0.x and the store, side by side.
type pair struct {
	t  *testing.T
	db *hc.DB
	s  *store.Store
}

func newPair(t *testing.T) *pair { return &pair{t, openZerox(t), store.New()} }

// put puts a record with no fields into both.
func (p *pair) put(keys ...string) {
	p.t.Helper()
	for _, k := range keys {
		ok(p.t, p.db.Put(k, nil))
		ok(p.t, putGo(p.s, k, nil))
	}
}

// link links on both, which must agree, and returns the store's changes.
func (p *pair) link(from, typ, to string) ([]format.Change, error) {
	p.t.Helper()
	zerr := p.db.Link(from, typ, to)
	changes, err := p.s.Link(nil, from, typ, to)
	sameError(p.t, fmt.Sprintf("Link(%q, %q, %q)", from, typ, to), zerr, err)
	return changes, err
}

// unlink unlinks on both, which must agree, and returns the store's
// changes.
func (p *pair) unlink(from, typ, to string) ([]format.Change, error) {
	p.t.Helper()
	zerr := p.db.Unlink(from, typ, to)
	changes, err := p.s.Unlink(nil, from, typ, to)
	sameError(p.t, fmt.Sprintf("Unlink(%q, %q, %q)", from, typ, to), zerr, err)
	return changes, err
}

func (p *pair) delete(key string) {
	p.t.Helper()
	ok(p.t, p.db.Delete(key))
	must[[]format.Change](p.t)(p.s.Delete(nil, key))
}

func (p *pair) drop(table string) {
	p.t.Helper()
	ok(p.t, p.db.Drop(table))
	must[[]format.Change](p.t)(p.s.Drop(nil, table))
}

// neighbours reads a record's links from both, which must agree, and
// returns them as "from -type-> to" with spaces between.
func (p *pair) neighbours(key string, dir store.Direction, typ string) (string, error) {
	p.t.Helper()
	want, zerr := p.db.Neighbours(key, hc.Direction(dir), typ)
	got, err := p.s.Neighbours(key, dir, typ)
	desc := fmt.Sprintf("Neighbours(%q, %v, %q)", key, dir, typ)
	sameError(p.t, desc, zerr, err)
	var conv []store.Link
	for _, l := range want {
		conv = append(conv, store.Link(l))
	}
	if !reflect.DeepEqual(got, conv) {
		p.t.Fatalf("%s gives %v, and 0.x %v", desc, got, want)
	}
	var out []string
	for _, l := range got {
		out = append(out, l.String())
	}
	return strings.Join(out, ", "), err
}

// walk walks from a record on both, which must agree, and returns the
// steps as "key@depth" with spaces between.
func (p *pair) walk(key string, dir store.Direction, typ string, depth int) (string, error) {
	p.t.Helper()
	want, zerr := p.db.Walk(key, hc.Direction(dir), typ, depth)
	got, err := p.s.Walk(key, dir, typ, depth)
	desc := fmt.Sprintf("Walk(%q, %v, %q, %d)", key, dir, typ, depth)
	sameError(p.t, desc, zerr, err)
	var conv []store.Step
	for _, st := range want {
		conv = append(conv, store.Step(st))
	}
	if !reflect.DeepEqual(got, conv) {
		p.t.Fatalf("%s gives %v, and 0.x %v", desc, got, want)
	}
	var out []string
	for _, st := range got {
		out = append(out, st.Key+"@"+strconv.Itoa(st.Depth))
	}
	return strings.Join(out, " "), err
}

// sameError fails the test unless err, the store's, and zerr, 0.x's, are of
// the same kind with the same message, or both nil.
func sameError(t *testing.T, desc string, zerr, err error) {
	t.Helper()
	if kind(err) != zeroxKind(zerr) || err != nil && err.Error() != zerr.Error() {
		t.Fatalf("%s: the store gives %v, and 0.x %v", desc, err, zerr)
	}
}

// links counts the links in the store's snapshot, as the public package's
// Check counts them.
func links(t *testing.T, s *store.Store) int {
	t.Helper()
	n := 0
	for _, c := range snapshot(t, s) {
		if c.Op == format.Link {
			n++
		}
	}
	return n
}

// TestLinksFollowTheirRecords is 0.x's TestLinksFollowTheirRecords and the
// suite's LinksFollowTheirRecords, run on both. A link that's there already
// changes nothing, and gives no change. Every link of a record goes with
// it when it's deleted. An unlink of a link that isn't there is not found,
// and one with no type takes every link from one record to the other.
func TestLinksFollowTheirRecords(t *testing.T) {
	p := newPair(t)
	p.put("customer:42", "docs:1", "docs:2", "docs:3")
	for _, l := range []store.Link{{"customer:42", "owns", "docs:1"}, {"customer:42", "owns", "docs:1"}, {"customer:42", "owns", "docs:2"},
		{"customer:42", "watches", "docs:1"}, {"docs:1", "cites", "docs:3"}, {"docs:3", "cites", "docs:3"}} {
		_, err := p.link(l.From, l.Type, l.To)
		ok(t, err)
	}
	if changes, _ := p.link("customer:42", "owns", "docs:1"); changes != nil {
		t.Fatalf("a link that's there already gave %v", changes)
	}
	for _, l := range []store.Link{{"customer:42", "owns", "docs:9"}, {"customer:9", "owns", "docs:1"}, {"customer:42", "", "docs:1"},
		{"customer:42", strings.Repeat("x", 201), "docs:1"}} {
		if _, err := p.link(l.From, l.Type, l.To); err == nil {
			t.Fatalf("Link(%v) worked", l)
		}
	}
	for _, c := range []struct {
		key  string
		dir  store.Direction
		typ  string
		want string
	}{
		{"customer:42", store.Out, "", "customer:42 -owns-> docs:1, customer:42 -owns-> docs:2, customer:42 -watches-> docs:1"},
		{"docs:1", store.In, "owns", "customer:42 -owns-> docs:1"},
		{"docs:3", store.Both, "", "docs:1 -cites-> docs:3, docs:3 -cites-> docs:3"},
	} {
		if got := must[string](t)(p.neighbours(c.key, c.dir, c.typ)); got != c.want {
			t.Errorf("Neighbours(%s, %v, %q) = %q, want %q", c.key, c.dir, c.typ, got, c.want)
		}
	}
	_, err := p.neighbours("docs:9", store.Out, "")
	wantErr(t, err, errs.ErrNotFound)

	// Deleting a record takes every link to and from it.
	p.delete("docs:1")
	if got := must[string](t)(p.neighbours("customer:42", store.Both, "")); got != "customer:42 -owns-> docs:2" {
		t.Fatalf("after deleting docs:1: %s", got)
	}
	if got := must[string](t)(p.neighbours("docs:3", store.In, "")); got != "docs:3 -cites-> docs:3" {
		t.Fatalf("docs:3 is still linked from the deleted docs:1: %s", got)
	}

	_, err = p.unlink("customer:42", "watches", "docs:2")
	wantErr(t, err, errs.ErrNotFound)
	if err.Error() != "hypercrux: not found: no link from customer:42 to docs:2" {
		t.Errorf("Unlink gave %q", err)
	}
	_, err = p.link("customer:42", "watches", "docs:2")
	ok(t, err)
	changes, err := p.unlink("customer:42", "", "docs:2") // every type
	ok(t, err)
	want := []format.Change{
		{Op: format.Unlink, Key: "customer:42", Type: "owns", To: "docs:2"},
		{Op: format.Unlink, Key: "customer:42", Type: "watches", To: "docs:2"},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("Unlink with no type gave %v", changes)
	}
	if got := must[string](t)(p.neighbours("customer:42", store.Out, "")); got != "" {
		t.Fatalf("Unlink with no type left %s", got)
	}
	if rep := must[hc.Report](t)(p.db.Check()); rep.Links != 1 || links(t, p.s) != 1 {
		t.Fatalf("0.x counts %d links, and the store's snapshot %d", rep.Links, links(t, p.s))
	}
}

// chain builds 0.x's chain on both: n:a -> n:b -> n:c -> n:d with "next"
// links, a loop n:d -> n:a, and a side link n:a -> n:x of the type "see".
func chain(p *pair) {
	p.t.Helper()
	p.put("n:a", "n:b", "n:c", "n:d", "n:x")
	for _, l := range []store.Link{{"n:a", "next", "n:b"}, {"n:b", "next", "n:c"}, {"n:c", "next", "n:d"}, {"n:d", "next", "n:a"},
		{"n:a", "see", "n:x"}} {
		_, err := p.link(l.From, l.Type, l.To)
		ok(p.t, err)
	}
}

// TestWalk is 0.x's TestWalk and the suite's Walk, run on both, with more
// cases: every depth from 1 to 32 one way and both ways, types that no link
// has or that break the rules, and the checks in 0.x's order.
func TestWalk(t *testing.T) {
	p := newPair(t)
	chain(p)
	for _, c := range []struct {
		key   string
		dir   store.Direction
		typ   string
		depth int
		want  string
	}{
		{"n:a", store.Out, "", 1, "n:b@1 n:x@1"},
		{"n:a", store.Out, "next", 2, "n:b@1 n:c@2"},
		{"n:a", store.Out, "", 10, "n:b@1 n:x@1 n:c@2 n:d@3"}, // the loop back to n:a ends
		{"n:a", store.In, "", 1, "n:d@1"},
		{"n:a", store.In, "", 3, "n:d@1 n:c@2 n:b@3"},
		{"n:c", store.Both, "next", 1, "n:b@1 n:d@1"},
		{"n:x", store.Both, "", 2, "n:a@1 n:b@2 n:d@2"},
		{"n:x", store.Out, "", 5, ""},
		{"n:x", store.In, "see", 32, "n:a@1"},
		{"n:a", store.Out, "nosuch", 3, ""},
		{"n:a", store.Out, strings.Repeat("t", 201), 3, ""},
		{"n:a", store.Out, "", 32, "n:b@1 n:x@1 n:c@2 n:d@3"},
	} {
		if got := must[string](t)(p.walk(c.key, c.dir, c.typ, c.depth)); got != c.want {
			t.Errorf("Walk(%s, %v, %q, %d) = %q, want %q", c.key, c.dir, c.typ, c.depth, got, c.want)
		}
	}
	for depth := 1; depth <= store.MaxDepth; depth++ {
		for _, dir := range []store.Direction{store.Out, store.In, store.Both} {
			for _, key := range []string{"n:a", "n:c", "n:x"} {
				must[string](t)(p.walk(key, dir, "", depth))
				must[string](t)(p.walk(key, dir, "next", depth))
			}
		}
	}
	for _, c := range []struct {
		key   string
		dir   store.Direction
		depth int
		want  error
	}{
		{"n:a", store.Out, 0, errs.ErrInvalid}, {"n:a", store.Out, -1, errs.ErrInvalid}, {"n:a", store.Out, store.MaxDepth + 1, errs.ErrInvalid},
		{"n:zz", store.Out, 1, errs.ErrNotFound}, {"N:a", store.Out, 1, errs.ErrInvalid}, {"n:a", 3, 1, errs.ErrInvalid},
		{"n:zz", 3, 1, errs.ErrInvalid}, {"n:zz", store.Out, 0, errs.ErrInvalid}, {"nocolon", -1, 40, errs.ErrInvalid},
	} {
		_, err := p.walk(c.key, c.dir, "", c.depth)
		wantErr(t, err, c.want)
	}
	// The depth comes before the direction, and both before the key.
	_, err := p.walk("n:zz", 7, "", 0)
	if !strings.Contains(err.Error(), "depth is from 1 to 32, not 0") {
		t.Errorf("a walk with every argument wrong gave %q", err)
	}
}

// TestLinkRules: 0.x's rules for links and their order, and the messages,
// from TestRulesForLongKeysAndLinkTypes and the suite's version of it, with
// more. Link checks the type first, then each key, from before to. Unlink
// checks nothing, and anything wrong is a link that isn't there.
// Neighbours checks the key before the direction, and neither read checks
// the type. A type that starts with a zero byte is where the two differ on
// purpose: 0.x refuses it with a plain error, and the store with
// ErrInvalid.
func TestLinkRules(t *testing.T) {
	p := newPair(t)
	p.put("docs:1", "docs:2")
	for _, typ := range []string{strings.Repeat("ü", 150), strings.Repeat("ü", 200), "a\x00b", "é", "a b", "Owns", "owns", "K"} {
		_, err := p.link("docs:1", typ, "docs:2")
		ok(t, err)
	}
	for _, c := range []struct{ from, typ, to string }{
		{"docs:1", strings.Repeat("ü", 201), "docs:2"}, {"docs:1", "", "docs:2"}, {"docs:1", "\xff", "docs:2"},
		{"Docs:1", "", "docs:2"}, {"Docs:1", "x", "docs:9"}, {"docs:9", "x", "Docs:2"}, {"docs:1", "x", "Docs:2"},
		{"nocolon", "x", "docs:2"}, {"docs:", "x", "docs:2"}, {"docs:1", "x", "docs:\x00"}, {"docs:9", "x", "docs:8"},
		{"docs:1", "x", "docs:9"}, {"hc_x:1", "x", "docs:2"},
	} {
		if _, err := p.link(c.from, c.typ, c.to); err == nil {
			t.Fatalf("Link(%q, %q, %q) worked", c.from, c.typ, c.to)
		}
	}
	_, err := p.s.Link(nil, "docs:1", "\x00x", "docs:2")
	if zerr := p.db.Link("docs:1", "\x00x", "docs:2"); !errors.Is(err, errs.ErrInvalid) || zeroxKind(zerr) != "error" {
		t.Errorf("a type that starts with a zero byte gives %v in the store, and %v in 0.x", err, zerr)
	}
	for _, c := range []struct{ from, typ, to string }{
		{"Docs:1", "owns", "docs:2"}, {"docs:1", "owns", "Docs:2"}, {"docs:1", strings.Repeat("t", 201), "docs:2"},
		{"docs:1", "\xff", "docs:2"}, {"docs:1", "\x00x", "docs:2"}, {"nocolon", "", "x"}, {"docs:2", "", "docs:1"},
		{"docs:9", "", "docs:1"},
	} {
		_, err := p.unlink(c.from, c.typ, c.to)
		wantErr(t, err, errs.ErrNotFound)
	}
	for _, c := range []struct {
		key string
		dir store.Direction
	}{{"docs:9", 3}, {"Docs:1", 3}, {"docs:1", 3}, {"docs:1", -1}, {"nocolon", store.Both}} {
		if _, err := p.neighbours(c.key, c.dir, ""); err == nil {
			t.Fatalf("Neighbours(%q, %v) worked", c.key, c.dir)
		}
	}
	for _, typ := range []string{strings.Repeat("ü", 201), "\xff", "", "OWNS", "\x00x"} {
		must[string](t)(p.neighbours("docs:1", store.Both, typ))
		must[string](t)(p.walk("docs:1", store.Both, typ, 2))
	}
	if rep := must[hc.Report](t)(p.db.Check()); rep.Links != links(t, p.s) {
		t.Fatalf("0.x counts %d links, and the store's snapshot %d", rep.Links, links(t, p.s))
	}
}

// TestNeighboursComeIn0xsOrder: links out by type and then the key they're
// to, links in by type and then the key they're from, and both ways by
// type, from and to, in byte order, which puts capitals before lower case,
// "a b" before "ab", a digit before a colon, users:10 before users:9, and
// é after every ASCII letter. A link from a record to itself comes once
// both ways. More links than a block holds come in the same order.
func TestNeighboursComeIn0xsOrder(t *testing.T) {
	p := newPair(t)
	keys := []string{"users:ann", "users:10", "users:9", "users2:zed", "users_x:1", "a:1", "users:Bob", "usersa:1"}
	p.put(keys...)
	types := []string{"b", "a", "A", "a b", "ab", "é", "Z", "a\x00b"}
	for i, from := range keys {
		for j, to := range keys {
			if (i+j)%3 == 0 || from == "users:ann" || to == "users:ann" {
				_, err := p.link(from, types[(i*7+j)%len(types)], to)
				ok(t, err)
			}
		}
	}
	for _, key := range keys {
		for _, dir := range []store.Direction{store.Out, store.In, store.Both} {
			for _, typ := range append([]string{"", "nosuch"}, types...) {
				must[string](t)(p.neighbours(key, dir, typ))
			}
		}
	}
	// A record with links of three types to and from 300 others, more than a
	// block holds.
	for i := range 300 {
		key := fmt.Sprintf("many:%d", i*7%300)
		p.put(key)
		for _, l := range []store.Link{{key, "to", "users:ann"}, {"users:ann", "back", key}, {key, "a", "users:ann"}} {
			_, err := p.link(l.From, l.Type, l.To)
			ok(t, err)
		}
	}
	for _, dir := range []store.Direction{store.Out, store.In, store.Both} {
		for _, typ := range []string{"", "a", "to", "back", "b"} {
			must[string](t)(p.neighbours("users:ann", dir, typ))
		}
		must[string](t)(p.walk("users:ann", dir, "", 2))
	}
}

// TestDeletesAndDropsTakeLinksBothWays: deleting a record takes every link
// to it and from it, and dropping a table takes every link of its records:
// links between two of them, from a record to itself, from other tables
// into it and from it out to others, as 0.x's triggers take them, from
// TestTableNamesLikeHyperCruxsOwn and TestDropAndAdoptAfterSchemaChanges,
// with more. A table made again with the same name starts with no links.
func TestDeletesAndDropsTakeLinksBothWays(t *testing.T) {
	p := newPair(t)
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		p.put(tbl+":1", tbl+":2")
		_, err := p.link(tbl+":1", "next", tbl+":2")
		ok(t, err)
	}
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		p.delete(tbl + ":2")
		if got := must[string](t)(p.neighbours(tbl+":1", store.Both, "")); got != "" {
			t.Errorf("%s: deleting a record left its link: %s", tbl, got)
		}
	}

	p.put("customer:1", "docs:2", "docs:3", "notes:1", "notes:2")
	for _, l := range []store.Link{
		{"customer:1", "owns", "docs:1"}, {"customer:1", "owns", "docs:2"}, {"customer:1", "owns", "docs:3"},
		{"docs:1", "cites", "docs:2"}, {"docs:2", "cites", "docs:1"}, {"docs:3", "self", "docs:3"},
		{"docs:1", "about", "notes:1"}, {"notes:1", "about", "docs:3"}, {"notes:2", "about", "notes:1"},
		{"customer:1", "reads", "notes:2"}, {"docs:2", "by", "customer:1"},
	} {
		_, err := p.link(l.From, l.Type, l.To)
		ok(t, err)
	}
	everyone := []string{"customer:1", "docs:1", "docs:2", "docs:3", "notes:1", "notes:2"}
	compare := func() {
		t.Helper()
		for _, key := range everyone {
			p.neighbours(key, store.Both, "")
			p.walk(key, store.Both, "", 4)
		}
		if rep := must[hc.Report](t)(p.db.Check()); rep.Links != links(t, p.s) {
			t.Fatalf("0.x counts %d links, and the store's snapshot %d", rep.Links, links(t, p.s))
		}
	}
	compare()
	p.delete("docs:3") // with a link to itself, and links in from two tables
	compare()
	p.drop("docs")
	compare()
	if got := must[string](t)(p.neighbours("customer:1", store.Out, "")); got != "customer:1 -reads-> notes:2" {
		t.Fatalf("after the drop, customer:1 has %s", got)
	}
	p.put("docs:1")
	compare()
	if got := must[string](t)(p.neighbours("docs:1", store.Both, "")); got != "" {
		t.Fatalf("a table made again gave docs:1 the links %s", got)
	}
	p.drop("notes")
	compare()
	if got := must[string](t)(p.neighbours("customer:1", store.Both, "")); got != "" {
		t.Fatalf("after both drops, customer:1 has %s", got)
	}
}

// TestLinksInsideATransaction: links and unlinks through a transaction, in
// 0.x's Update and through the store's Tx alike, show at once to reads of
// links and to walks through the same transaction, and once it rolls back
// none of them do. A delete inside it takes the record's links both ways,
// and the rollback puts them back.
func TestLinksInsideATransaction(t *testing.T) {
	p := newPair(t)
	chain(p)
	tx := must[*store.Tx](t)(p.s.Begin())
	defer tx.Rollback()
	type read struct {
		links string
		steps string
	}
	reads := func(neighbours func(string) ([]store.Link, error), walk func(string) ([]store.Step, error)) []read {
		var out []read
		for _, key := range []string{"n:a", "n:b", "n:c", "n:d", "n:x", "n:y"} {
			var r read
			ls, err := neighbours(key)
			r.links = fmt.Sprint(ls, err)
			st, err := walk(key)
			r.steps = fmt.Sprint(st, err)
			out = append(out, r)
		}
		return out
	}
	var inside []read
	rollBack := errors.New("roll back")
	err := p.db.Update(func(ztx *hc.Tx) error {
		ok(t, ztx.Put("n:y", nil))
		ok(t, ztx.Link("n:y", "next", "n:a"))
		ok(t, ztx.Link("n:x", "see", "n:y"))
		ok(t, ztx.Unlink("n:b", "", "n:c"))
		ok(t, ztx.Delete("n:d"))
		inside = reads(func(key string) ([]store.Link, error) {
			ls, err := ztx.Neighbours(key, hc.Both, "")
			var out []store.Link
			for _, l := range ls {
				out = append(out, store.Link(l))
			}
			return out, err
		}, func(key string) ([]store.Step, error) {
			st, err := ztx.Walk(key, hc.Both, "", 3)
			var out []store.Step
			for _, s := range st {
				out = append(out, store.Step(s))
			}
			return out, err
		})
		return rollBack
	})
	if !errors.Is(err, rollBack) {
		t.Fatal(err)
	}
	before := reads(func(key string) ([]store.Link, error) { return p.s.Neighbours(key, store.Both, "") },
		func(key string) ([]store.Step, error) { return p.s.Walk(key, store.Both, "", 3) })
	ok(t, tx.Put("n:y", nil))
	ok(t, tx.Link("n:y", "next", "n:a"))
	ok(t, tx.Link("n:x", "see", "n:y"))
	ok(t, tx.Unlink("n:b", "", "n:c"))
	ok(t, tx.Delete("n:d"))
	got := reads(func(key string) ([]store.Link, error) { return tx.Neighbours(key, store.Both, "") },
		func(key string) ([]store.Step, error) { return tx.Walk(key, store.Both, "", 3) })
	if !reflect.DeepEqual(got, inside) {
		t.Fatalf("inside the transaction the store gives\n%v\nand 0.x\n%v", got, inside)
	}
	tx.Rollback()
	after := reads(func(key string) ([]store.Link, error) { return p.s.Neighbours(key, store.Both, "") },
		func(key string) ([]store.Step, error) { return p.s.Walk(key, store.Both, "", 3) })
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("after the rollback the store gives\n%v\nwhere it gave\n%v", after, before)
	}
	for _, key := range []string{"n:a", "n:b", "n:c", "n:d", "n:x"} {
		p.neighbours(key, store.Both, "")
		p.walk(key, store.Both, "", 3)
	}
}
