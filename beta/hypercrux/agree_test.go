// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// TestTheBetaAgrees0x runs the same random calls on 0.x and on the Beta,
// through their Go APIs, and now and then closes the Beta and opens its
// file again. The calls are every one the Beta has: puts, gets, deletes,
// scans, links, unlinks, reads of links, walks, searches without a filter
// and drops, on their own and inside Updates. Every call must give the same
// answer on both: the same error, with the same message and of the same
// kind, or the same results, with every number's bits, and nil where 0.x
// gives nil. A search's distances must agree within
// conformance.DistanceBound, and its keys come in the same order but where
// two distances fall within it, since 0.x adds in another order. After each
// reopen, and at the end, everything the workload can name must read the
// same on both: every key, with its links both ways and a walk from it,
// every table's scan, a search of every table, and Check's counts.
//
// Two things may differ. The message for one field in two spellings, since
// 0.x reports whichever of the two its map gave first. And the case of a
// field's name, in a table where a Put that failed inside an Update that
// went on left new fields in 0.x, spelt its way, and nothing in the Beta,
// as TestAFailedPutLeavesNoField pins: the workload notes such a Put from
// 0.x's error, and compares that table's field names regardless of case
// until the table is dropped.
//
// An Update holds one to five calls through its transaction, and commits,
// returns an error or panics. A call in it that fails doesn't end it, as
// with 0.x. 0.x is the judge throughout: of the rules and their order of
// errors, of the conversions from Go values, and of what a reopen must
// read back.
//
// The counts at the end, over every seed, show that each call came up with
// each of its answers, and that the reads reached something: scans that
// found records, walks that went two links, and searches with hits.
func TestTheBetaAgrees0x(t *testing.T) {
	steps, seeds := 5000, 3
	if testing.Short() {
		steps, seeds = 1000, 2
	}
	outcomes, seen := map[string]int{}, map[string]int{}
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) { agree0x(t, uint64(seed), steps, outcomes, seen) })
	}
	if t.Failed() {
		return
	}
	all := steps * seeds
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "delete ok",
		"delete not found", "scan ok", "scan invalid", "link ok", "link invalid", "link not found", "unlink ok",
		"unlink not found", "neighbours ok", "neighbours invalid", "neighbours not found", "walk ok", "walk invalid",
		"walk not found", "nearest ok", "nearest invalid", "nearest not found", "drop ok", "drop invalid",
		"drop not found", "update ok", "update error", "reopen ok"} {
		if outcomes[o] < all/1000 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], all, outcomes)
		}
	}
	for o, least := range map[string]int{"scans that found records": all / 30, "walks that went two links": all / 300,
		"walks after a reopen that went two links": all / 50, "searches with hits": all / 50,
		"searches of an emptied table": 1} {
		if seen[o] < least {
			t.Errorf("%q came up %d times in %d steps: %v", o, seen[o], all, seen)
		}
	}
	t.Logf("%d steps from %d seeds; outcomes: %v", all, seeds, outcomes)
	t.Logf("reads that reached something: %v", seen)
}

// TestAFailedPutLeavesNoField pins a difference from 0.x that
// beta/README.md lists: a Put that fails changes nothing, even inside an
// Update that goes on to commit, so a field that only the failed Put named
// isn't there, and a later Put spells it its own way. 0.x adds a Put's new
// fields before it checks the vector's size, and keeps them when the check
// fails, spelt as the failed Put spelt them, so there the later Put takes
// that spelling. For this reason TestTheBetaAgrees0x compares field names
// regardless of case in a table where such a Put failed, and the
// differential harness compares them regardless of case throughout.
func TestAFailedPutLeavesNoField(t *testing.T) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	defer z.Close()
	b := open(t, filepath.Join(dir, "beta.hcx"))
	for _, h := range []handle{zeroOn{z}, betaOn{b}} {
		ok(t, h.put("docs:1", goFields{"vec": vec{1, 2}}))
	}
	failed := call{op: "put", key: "docs:1", f: goFields{"Title": "x", "vec": vec{1, 2, 3}}}
	later := call{op: "put", key: "docs:1", f: goFields{"title": "y"}}
	var zSaid, bSaid []result
	ok(t, runUpdate(func(fn func(h handle) error) error {
		return z.Update(func(tx *zx.Tx) error { return fn(zeroOn{tx}) })
	}, []call{failed, later}, 2, &zSaid))
	ok(t, runUpdate(func(fn func(h handle) error) error {
		return b.Update(func(tx *hc.Tx) error { return fn(betaOn{tx}) })
	}, []call{failed, later}, 2, &bSaid))
	for i := range zSaid {
		same(t, i, "inside the Update", bSaid[i], zSaid[i])
	}
	if !errors.Is(zSaid[0].err, zx.ErrInvalid) {
		t.Fatalf("0.x's Put of a vector of the wrong size gave %v", zSaid[0].err)
	}
	zf, err := z.Get("docs:1")
	ok(t, err)
	bf, err := b.Get("docs:1")
	ok(t, err)
	if zf["Title"] != "y" || len(zf) != 2 {
		t.Errorf("0.x gives %v, so it no longer keeps a failed Put's spelling, and beta/README.md's difference can go", show(zf))
	}
	if bf["title"] != "y" || len(bf) != 2 {
		t.Errorf("the Beta gives %v, where the failed Put should have left no field", show(bf))
	}
}

// mix says how often each kind of step comes up, out of 100. A reopen and
// an Update are steps of their own, and the rest can be calls inside an
// Update too.
var mix = []struct {
	op   string
	upTo int
}{
	{"reopen", 2}, {"update", 12}, {"put", 36}, {"get", 44}, {"delete", 49}, {"scan", 57}, {"link", 73},
	{"unlink", 78}, {"neighbours", 85}, {"walk", 92}, {"nearest", 99}, {"drop", 100},
}

// inUpdate is where the calls an Update can hold start in mix.
const inUpdate = 12

func agree0x(t *testing.T, seed uint64, steps int, outcomes, seen map[string]int) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	path := filepath.Join(dir, "beta.hcx")
	b, err := hc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if b != nil {
			b.Close()
		}
	}()

	g := &workload{r: rand.New(rand.NewPCG(seed, 0x61)), sizes: map[string]int{}, seen: seen, respelt: map[string]bool{}}
	note := func(what string, err error) { outcomes[what+" "+kindOf(err)]++ }
	for step := 0; step < steps; step++ {
		switch op := g.op(g.r.IntN(100)); op {
		case "reopen":
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if b, err = hc.Open(path); err != nil {
				t.Fatalf("step %d: reopening: %v", step, err)
			}
			g.everything(t, step, z, b)
			note("reopen", nil)
		case "update":
			ending := g.r.IntN(4) // 0: an error, 1: a panic, else a commit
			calls := g.update()
			var zSaid, bSaid []result
			zErr := runUpdate(func(fn func(h handle) error) error {
				return z.Update(func(tx *zx.Tx) error { return fn(zeroOn{tx}) })
			}, calls, ending, &zSaid)
			bErr := runUpdate(func(fn func(h handle) error) error {
				return b.Update(func(tx *hc.Tx) error { return fn(betaOn{tx}) })
			}, calls, ending, &bSaid)
			desc := fmt.Sprintf("update %v, ending %d", calls, ending)
			sameErr(t, step, desc, bErr, zErr)
			respelt := maps.Clone(g.respelt)
			for i := range zSaid {
				g.respell(t, step, fmt.Sprintf("%s: call %d", desc, i), calls[i], bSaid[i], zSaid[i], respelt)
				g.saw(calls[i], zSaid[i])
				if zErr == nil {
					g.learn(calls[i], zSaid[i].err)
				}
			}
			if zErr == nil {
				g.respelt = respelt
			}
			g.sizesFrom(z)
			note("update", zErr)
		default:
			cl := g.call(op)
			zr, br := do(zeroOn{z}, cl), do(betaOn{b}, cl)
			g.respell(t, step, cl.String(), cl, br, zr, g.respelt)
			g.saw(cl, zr)
			g.learn(cl, zr.err)
			g.sizesFrom(z)
			note(op, zr.err)
		}
	}
	b.Close()
	if b, err = hc.Open(path); err != nil {
		t.Fatal(err)
	}
	g.everything(t, steps, z, b)
}

// workload makes the random calls, with a rough idea of each table's vector
// size and of the links there are, learnt from 0.x, so that most vectors
// fit and most reads of links find some.
type workload struct {
	r     *rand.Rand
	sizes map[string]int
	keys  []string // keys 0.x has put, some of them gone since
	links []hc.Link
	seen  map[string]int // what the reads found, for the counts at the end
	// respelt holds the tables where a Put that failed inside an Update
	// that committed has left new fields in 0.x, and nothing in the Beta.
	// Their fields' names compare regardless of case, until the table is
	// dropped.
	respelt map[string]bool
}

var (
	tables    = []string{"docs", "people", "t_1"}
	ids       = []string{"1", "2", "3", "4", "é", "a b", "x/y"}
	names     = []string{"title", "Title", "TITLE", "n", "N", "score", "_x", "Zeta", "zeta", "a1", "vec", "Vec", "VEC"}
	badNames  = []string{"key", "Rowid", "1st", "has space", "oid"}
	badKeys   = []string{"nocolon", "docs:", "Docs:1", "docs:\x00", "hc_x:1", ":1", "docs:\xff"}
	linkTypes = []string{"cites", "owns", "x", "é ✓", "a b", "a\x00b", "\n", strings.Repeat("ü", 200)}
	// badTypes break the rules for a link's type. One that starts with a
	// zero byte is left out: 0.x refuses it with a plain error, and the Beta
	// with ErrInvalid, as beta/README.md says.
	badTypes  = []string{"", strings.Repeat("ü", 201), "\xff", "a\xffb"}
	badTables = []string{"Docs", "hc_x", "sqlite_x", "", "a-b", "7up", "nosuch"}
)

type status string
type blob []byte

func (g *workload) pick(list []string) string { return list[g.r.IntN(len(list))] }

func (g *workload) one(in int) bool { return g.r.IntN(in) == 0 }

func (g *workload) key() string {
	if g.one(20) {
		return g.pick(badKeys)
	}
	return g.pick(tables) + ":" + g.pick(ids)
}

// op is the kind of step that w, from 0 to 99, picks in mix.
func (g *workload) op(w int) string {
	for _, m := range mix {
		if w < m.upTo {
			return m.op
		}
	}
	panic("mix doesn't reach 100")
}

// value returns a Go value for a field other than vec: of every type Put
// takes, awkward ones among them, and now and then one it refuses.
func (g *workload) value() any {
	when := time.Date(2026, 10, 8, 9, 30, 0, 123456789, time.FixedZone("IDT", 3*3600))
	n := 7
	var nilInt *int
	values := []any{
		"Q3 plan", "", "é ✓ 😀", "a\x00b", 12, -1, int64(math.MaxInt64), int64(math.MinInt64), int8(-8), uint32(math.MaxUint32),
		uint64(math.MaxInt64), 0.75, math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64, float32(0.1),
		true, false, []byte{0, 1, 255}, []byte{}, nil, nilInt, &n, []any{"a", 1.5}, map[string]any{"k": 1},
		map[int]string{1: "a"}, struct{ X, Y int }{1, 2}, []string{"a"}, when, status("open"), blob{1, 2},
		json.Number("42"), json.Number("4.5"),
		// Refused by 0.x's rules.
		"\xff", math.NaN(), math.Inf(-1), uint64(math.MaxUint64), []float32{1}, make(chan int),
		map[string]any{"f": func() {}},
	}
	if g.one(3) {
		return values[g.r.IntN(len(values))]
	}
	return values[g.r.IntN(len(values)-7)]
}

// floats returns n random values, now and then with an awkward one among
// them, and now and then the same values each time, so searches meet ties.
func (g *workload) floats(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(g.r.NormFloat64())
	}
	if g.one(8) {
		v[g.r.IntN(n)] = []float32{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, math.MaxFloat32}[g.r.IntN(3)]
	}
	if g.one(10) {
		for i := range v {
			v[i] = 0.5
		}
	}
	return v
}

// vector returns a value for vec: mostly a vector of the table's size, in
// any of the forms Put takes, and now and then the wrong size, all zeros,
// holding NaN, or something that isn't a vector at all.
func (g *workload) vector(tbl string) any {
	n := g.sizes[tbl]
	if n == 0 || g.one(12) {
		n = 1 + g.r.IntN(4)
	}
	v := g.floats(n)
	switch g.r.IntN(16) {
	case 0:
		return nil
	case 1:
		return make([]float32, n)
	case 2:
		v[0] = float32(math.NaN())
	case 3:
		f := make([]float64, n)
		for i := range f {
			f[i] = float64(v[i])
		}
		return f
	case 4:
		b, _ := json.Marshal(v)
		return string(b)
	case 5:
		a := make([]any, n)
		for i := range a {
			a[i] = float64(v[i])
		}
		return a
	case 6:
		return 5
	case 7:
		return vecPtr(v)
	case 8:
		return "not a vector"
	case 9:
		return v
	}
	return vec(v)
}

// vec stands for a vector of the package's own type, a Vector on each
// side, and vecPtr for a pointer to one.
type vec []float32
type vecPtr []float32

// goFields are a put's fields, with vec and vecPtr standing in for each
// package's Vector.
type goFields map[string]any

func (f goFields) zero() zx.Fields {
	if f == nil {
		return nil
	}
	out := zx.Fields{}
	for k, v := range f {
		switch x := v.(type) {
		case vec:
			v = zx.Vector(x)
		case vecPtr:
			p := zx.Vector(x)
			v = &p
		}
		out[k] = v
	}
	return out
}

func (f goFields) beta() hc.Fields {
	if f == nil {
		return nil
	}
	out := hc.Fields{}
	for k, v := range f {
		switch x := v.(type) {
		case vec:
			v = hc.Vector(x)
		case vecPtr:
			p := hc.Vector(x)
			v = &p
		}
		out[k] = v
	}
	return out
}

// put returns a key and fields to put there: zero to four fields, with
// names in mixed case. A put holds at most one name that breaks the rules,
// or else one field in two spellings, since 0.x checks the names in its
// map's order and reports whichever problem it meets first.
func (g *workload) put() (string, goFields) {
	key := g.key()
	tbl, _, _ := strings.Cut(key, ":")
	if g.one(15) {
		return key, nil
	}
	f := goFields{}
	bad, clash := false, false
	for n := g.r.IntN(5); n > 0; n-- {
		name := g.pick(names)
		if !bad && !clash && g.one(30) {
			name, bad = g.pick(badNames), true
		}
		for have := range f {
			if strings.EqualFold(have, name) && have != name {
				if bad {
					name = have
				}
				clash = clash || !bad
			}
		}
		if strings.EqualFold(name, "vec") {
			f[name] = g.vector(tbl)
		} else {
			f[name] = g.value()
		}
	}
	return key, f
}

// held is the key at one end of a link the workload has made, most of the
// time, so reads of links and walks find some.
func (g *workload) held() string {
	if len(g.links) > 0 && !g.one(4) {
		l := g.links[g.r.IntN(len(g.links))]
		if g.one(2) {
			return l.To
		}
		return l.From
	}
	return g.put0()
}

// put0 is a key 0.x has put, most of the time, so most links join records
// that exist.
func (g *workload) put0() string {
	if len(g.keys) > 0 && !g.one(5) {
		return g.keys[g.r.IntN(len(g.keys))]
	}
	return g.key()
}

func (g *workload) linkType() string {
	if g.one(20) {
		return g.pick(badTypes)
	}
	return g.pick(linkTypes)
}

// maybeType is a type to read or follow links by: every type, a type some
// link has, or now and then one no link can have.
func (g *workload) maybeType() string {
	switch g.r.IntN(8) {
	case 0, 1, 2, 3:
		return ""
	case 4:
		return g.pick(badTypes[1:])
	}
	if len(g.links) > 0 {
		return g.links[g.r.IntN(len(g.links))].Type
	}
	return g.pick(linkTypes)
}

// direction is mostly one of the three, and now and then a number that
// isn't one.
func (g *workload) direction() int {
	if g.one(25) {
		return []int{3, -1, 100}[g.r.IntN(3)]
	}
	return g.r.IntN(3)
}

func (g *workload) table() string {
	if g.one(12) {
		return g.pick(badTables)
	}
	return g.pick(tables)
}

// query is a search's query for a table: mostly a vector of its size, and
// now and then one of another size, all zeros, holding NaN, or empty.
func (g *workload) query(tbl string) []float32 {
	n := g.sizes[tbl]
	if n == 0 || g.one(15) {
		n = 1 + g.r.IntN(4)
	}
	v := g.floats(n)
	switch g.r.IntN(30) {
	case 0:
		return make([]float32, n)
	case 1:
		v[n-1] = float32(math.Inf(1))
	case 2:
		return nil
	}
	return v
}

// call is one call, on its own or inside an Update.
type call struct {
	op           string
	key, to, typ string // the key, or Scan's prefix and Drop's or Nearest's table; Link's and Unlink's to; a link type
	after        string
	n, dir       int // Scan's limit, Walk's depth or Nearest's k; a direction
	f            goFields
	q            []float32
	where        string
	args         []any
}

func (cl call) String() string {
	switch cl.op {
	case "put":
		return fmt.Sprintf("put %q %v", cl.key, cl.f)
	case "scan":
		return fmt.Sprintf("scan %q after %q limit %d", cl.key, cl.after, cl.n)
	case "link", "unlink":
		return fmt.Sprintf("%s %q -%q-> %q", cl.op, cl.key, cl.typ, cl.to)
	case "neighbours":
		return fmt.Sprintf("neighbours %q %d %q", cl.key, cl.dir, cl.typ)
	case "walk":
		return fmt.Sprintf("walk %q %d %q depth %d", cl.key, cl.dir, cl.typ, cl.n)
	case "nearest":
		return fmt.Sprintf("nearest %q %v k %d where %q %v", cl.key, cl.q, cl.n, cl.where, cl.args)
	}
	return fmt.Sprintf("%s %q", cl.op, cl.key)
}

// call makes a call of the kind op.
func (g *workload) call(op string) call {
	switch op {
	case "put":
		key, f := g.put()
		return call{op: op, key: key, f: f}
	case "scan":
		cl := call{op: op, key: g.pick(tables) + ":"}
		switch g.r.IntN(12) {
		case 0:
			cl.key = g.pick([]string{"docs", "", "Docs:", "hc_x:", ":", "a-b:", "nosuch:"})
		case 1, 2:
			cl.key += g.pick([]string{"1", "a", "x/", "\xc3", "é", " ", "zz"})
		}
		switch g.r.IntN(6) {
		case 0, 1:
			cl.after = g.key()
		case 2:
			cl.after = g.pick([]string{cl.key, "zzz", "\xff", "docs:2", "a"})
		}
		if !g.one(3) {
			cl.n = 1 + g.r.IntN(4)
		}
		if g.one(25) {
			cl.n = -1 - g.r.IntN(2)
		}
		return cl
	case "link":
		cl := call{op: op, key: g.put0(), typ: g.linkType(), to: g.put0()}
		if len(g.links) > 0 {
			l := g.links[g.r.IntN(len(g.links))]
			switch g.r.IntN(6) {
			case 0: // the same two records, maybe another type
				cl.key, cl.to = l.From, l.To
			case 1, 2: // a chain, for walks of several links
				cl.key = l.To
			}
		}
		return cl
	case "unlink":
		if len(g.links) > 0 && !g.one(4) {
			l := g.links[g.r.IntN(len(g.links))]
			if g.one(3) {
				l.Type = ""
			}
			return call{op: op, key: l.From, typ: l.Type, to: l.To}
		}
		return call{op: op, key: g.key(), typ: g.maybeType(), to: g.key()}
	case "neighbours":
		return call{op: op, key: g.held(), dir: g.direction(), typ: g.maybeType()}
	case "walk":
		cl := call{op: op, key: g.held(), dir: g.direction(), typ: g.maybeType(), n: 1 + g.r.IntN(4)}
		if g.one(20) {
			cl.n = []int{0, -1, hc.MaxDepth, hc.MaxDepth + 1}[g.r.IntN(4)]
		}
		return cl
	case "nearest":
		tbl := g.table()
		cl := call{op: op, key: tbl, q: g.query(tbl), n: 1 + g.r.IntN(6)}
		if g.one(20) {
			cl.n = []int{0, -1, hc.MaxK, hc.MaxK + 1}[g.r.IntN(4)]
		}
		switch g.r.IntN(10) {
		case 0: // no filter, said with spaces
			cl.where = g.pick([]string{" ", "\t\n"})
		case 1: // arguments without a filter, which neither engine uses
			cl.args = []any{1, "open"}
		}
		return cl
	case "drop":
		switch g.r.IntN(6) {
		case 0:
			return call{op: op, key: g.pick(badTables)}
		case 1:
			return call{op: op, key: g.pick([]string{"nosuch", "t_2", "gone"})}
		}
		return call{op: op, key: g.pick(tables)}
	}
	return call{op: op, key: g.key()} // get and delete
}

func (g *workload) update() []call {
	calls := make([]call, 1+g.r.IntN(5))
	for i := range calls {
		calls[i] = g.call(g.op(inUpdate + g.r.IntN(100-inUpdate)))
	}
	return calls
}

// learn notes the keys and links a call has made, from 0.x's answer, so
// later calls find them. It keeps the last 40 of each.
func (g *workload) learn(cl call, err error) {
	if err != nil {
		return
	}
	switch cl.op {
	case "put":
		g.keys = append(g.keys, cl.key)
		if len(g.keys) > 40 {
			g.keys = g.keys[1:]
		}
	case "link":
		g.links = append(g.links, hc.Link{From: cl.key, Type: cl.typ, To: cl.to})
		if len(g.links) > 40 {
			g.links = g.links[1:]
		}
	}
}

// respell compares a call's results, with field names regardless of case
// in a table of respelt, and then notes in respelt what the call did to
// that: 0.x keeps the new fields of a Put that fails its vector's size
// check, inside an Update, and a drop takes them away. respelt is the
// workload's own for a call on its own, which can't leave fields, since
// such a call is an Update that rolls back when it fails, and a copy of it
// for a call inside an Update, which becomes the workload's if the Update
// commits.
func (g *workload) respell(t *testing.T, step int, what string, cl call, beta, zero result, respelt map[string]bool) {
	t.Helper()
	tbl := ""
	switch cl.op {
	case "get", "put", "scan":
		tbl, _, _ = strings.Cut(cl.key, ":")
	case "drop":
		tbl = cl.key
	}
	if sameOrRespelt(t, step, what, beta, zero, respelt[tbl]) {
		g.seen["field names a failed Put spelt in 0.x"]++
	}
	switch {
	case cl.op == "drop" && zero.err == nil:
		delete(respelt, tbl)
	case cl.op == "put" && zero.err != nil && strings.Contains(zero.err.Error(), " holds vectors of "):
		respelt[tbl] = true
	}
}

// saw counts what a read found on 0.x, for the counts at the end, which
// show the reads reaching something.
func (g *workload) saw(cl call, r result) {
	if r.err != nil {
		return
	}
	switch {
	case cl.op == "scan" && strings.HasPrefix(r.text, "ok [\""):
		g.seen["scans that found records"]++
	case cl.op == "walk" && strings.Contains(r.text, "@2"):
		g.seen["walks that went two links"]++
	case cl.op == "nearest" && len(r.hits) > 0:
		g.seen["searches with hits"]++
	case cl.op == "nearest" && r.hits != nil:
		g.seen["searches of an emptied table"]++
	}
}

// sizesFrom notes each table's vector size from 0.x, so later vectors
// mostly fit.
func (g *workload) sizesFrom(z *zx.DB) {
	for _, tbl := range tables {
		var dims *int64
		if z.QueryRow(`SELECT dims FROM hc_tables WHERE name = ?`, tbl).Scan(&dims) == nil && dims != nil {
			g.sizes[tbl] = int(*dims)
		} else {
			delete(g.sizes, tbl)
		}
	}
}

// everything compares what the workload can name on both databases: every
// key, with its links both ways and a walk from it, every table's scan, a
// search of every table with a vector size, and Check's counts. It notes
// what the reads found as it goes.
func (g *workload) everything(t *testing.T, step int, z *zx.DB, b *hc.DB) {
	t.Helper()
	zh, bh := zeroOn{z}, betaOn{b}
	var calls []call
	for _, tbl := range tables {
		calls = append(calls, call{op: "scan", key: tbl + ":"})
		for _, id := range ids {
			key := tbl + ":" + id
			calls = append(calls, call{op: "get", key: key}, call{op: "neighbours", key: key, dir: int(hc.Both)},
				call{op: "walk", key: key, dir: int(hc.Both), n: hc.MaxDepth})
		}
		if n := g.sizes[tbl]; n > 0 {
			q := make([]float32, n)
			q[0] = 1
			calls = append(calls, call{op: "nearest", key: tbl, q: q, n: 30})
		}
	}
	for _, cl := range calls {
		zr := do(zh, cl)
		g.respell(t, step, "after a reopen, "+cl.String(), cl, do(bh, cl), zr, g.respelt)
		if cl.op == "walk" && strings.Contains(zr.text, "@2") {
			g.seen["walks after a reopen that went two links"]++
		}
	}
	zr, zErr := z.Check()
	br, bErr := b.Check()
	if zErr != nil || bErr != nil || !zr.OK() || !br.OK() ||
		br.Tables != zr.Tables || br.Records != zr.Records || br.Links != zr.Links || br.Vectors != zr.Vectors {
		t.Fatalf("step %d: after a reopen, Check gives %+v, %v on the Beta, and %+v, %v on 0.x", step, br, bErr, zr, zErr)
	}
}

// kindOf names an error by what a caller can test.
func kindOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, hc.ErrNotFound) || errors.Is(err, zx.ErrNotFound):
		return "not found"
	case errors.Is(err, hc.ErrInvalid) || errors.Is(err, zx.ErrInvalid):
		return "invalid"
	}
	return "error"
}

// describeErr shows an error with its kind, and with its message, apart
// from 0.x's message for one field in two spellings, whose name comes from
// its map's order.
func describeErr(err error) string {
	if err == nil {
		return "ok"
	}
	if strings.Contains(err.Error(), "differ only in case") {
		return kindOf(err) + ": a field in two spellings"
	}
	return kindOf(err) + ": " + err.Error()
}

func sameErr(t *testing.T, step int, what string, beta, zero error) {
	t.Helper()
	if describeErr(beta) != describeErr(zero) {
		t.Fatalf("step %d: %s: the Beta gives %v, and 0.x %v", step, what, beta, zero)
	}
}

// show writes fields with their types, and numbers with their bits, so
// that -0 and 0, or a whole number and a real, never look the same. Each
// package's Vector shows the same way. A nil map shows as nil and an empty
// one as {}, which 0.x gives for a record with no fields.
func show[F ~map[string]any](f F) string {
	if f == nil {
		return "nil"
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range f {
			if !yield(k) {
				return
			}
		}
	})
	parts := make([]string, len(names))
	for i, n := range names {
		var s string
		switch x := f[n].(type) {
		case float64:
			s = fmt.Sprintf("float64 %016x", math.Float64bits(x))
		case int64:
			s = fmt.Sprintf("int64 %d", x)
		case string:
			s = fmt.Sprintf("string %q", x)
		case []byte:
			s = fmt.Sprintf("[]byte %x (nil %v)", x, x == nil)
		case hc.Vector:
			s = "Vector " + bitsOf(x)
		case zx.Vector:
			s = "Vector " + bitsOf(x)
		default:
			s = fmt.Sprintf("%T %#v", x, x)
		}
		parts[i] = n + ": " + s
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func bitsOf[V ~[]float32](v V) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%08x", math.Float32bits(x))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
