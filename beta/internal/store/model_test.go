// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The model is the plainest thing that does the job: maps, with field
// names matched by strings.ToLower and new fields appended in sorted
// order, written apart from the store's code.
type model struct {
	tables  map[string]*modelTable
	records map[string]map[string]value.Value // by key, then by field name in lower case; nulls aren't there
	links   map[Link]bool                     // every link, each once
	order   []Link                            // the same links, in the order they came, for a generator to pick from
}

type modelTable struct {
	fields []string // as first spelt, in order
	size   int
}

func tableOfKey(key string) string { return key[:strings.IndexByte(key, ':')] }

// put returns the kind of error a put should give, "" when it should work,
// and applies the put when it should. bad says the generator put a value,
// a name or a key into it that breaks a rule whatever the state.
func (m *model) put(key string, fields []format.Field, bad bool) string {
	if bad {
		return "invalid"
	}
	tbl := tableOfKey(key)
	t := m.tables[tbl]
	for _, f := range fields {
		if f.Value.Kind() == value.KindVector && t != nil && t.size != 0 && f.Value.Dims() != t.size {
			return "invalid"
		}
	}
	if t == nil {
		t = &modelTable{}
		m.tables[tbl] = t
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.Name
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.ContainsFunc(t.fields, func(have string) bool { return strings.ToLower(have) == strings.ToLower(name) }) {
			t.fields = append(t.fields, name)
		}
	}
	rec := m.records[key]
	if rec == nil {
		rec = map[string]value.Value{}
		m.records[key] = rec
	}
	for _, f := range fields {
		l := strings.ToLower(f.Name)
		if f.Value.IsNull() {
			delete(rec, l)
		} else {
			rec[l] = f.Value
		}
		if f.Value.Kind() == value.KindVector && t.size == 0 {
			t.size = f.Value.Dims()
		}
	}
	return ""
}

func (m *model) delete(key string, bad bool) string {
	switch {
	case bad:
		return "invalid"
	case m.records[key] == nil:
		return "not found"
	}
	delete(m.records, key)
	for l := range m.links {
		if l.From == key || l.To == key {
			delete(m.links, l)
		}
	}
	m.reorder()
	return ""
}

func (m *model) drop(name string, bad bool) string {
	switch {
	case bad:
		return "invalid"
	case m.tables[name] == nil:
		return "not found"
	}
	delete(m.tables, name)
	for key := range m.records {
		if tableOfKey(key) == name {
			delete(m.records, key)
		}
	}
	for l := range m.links {
		if tableOfKey(l.From) == name || tableOfKey(l.To) == name {
			delete(m.links, l)
		}
	}
	m.reorder()
	return ""
}

// reorder takes the links that have gone out of order.
func (m *model) reorder() {
	m.order = slices.DeleteFunc(m.order, func(l Link) bool { return !m.links[l] })
}

// linkArgs are a link's arguments, with which of them break the rules
// whatever the state, as the generator made them.
type linkArgs struct {
	from, typ, to           string
	badFrom, badType, badTo bool
}

// link returns the kind of error a link should give, and adds the link when
// it should work and isn't there yet, which added reports. The checks come
// in 0.x's order: the type, then from, then to.
func (m *model) link(a linkArgs) (kind string, added bool) {
	switch {
	case a.badType, a.badFrom:
		return "invalid", false
	case m.records[a.from] == nil:
		return "not found", false
	case a.badTo:
		return "invalid", false
	case m.records[a.to] == nil:
		return "not found", false
	}
	l := Link{a.from, a.typ, a.to}
	if m.links[l] {
		return "", false
	}
	m.links[l] = true
	m.order = append(m.order, l)
	return "", true
}

// unlink returns the kind of error an unlink should give, and the types of
// the links it should take out, in byte order, which it takes out.
func (m *model) unlink(from, typ, to string) (string, []string) {
	var types []string
	for l := range m.links {
		if l.From == from && l.To == to && (typ == "" || l.Type == typ) {
			types = append(types, l.Type)
		}
	}
	if len(types) == 0 {
		return "not found", nil
	}
	sort.Strings(types)
	for _, typ := range types {
		delete(m.links, Link{from, typ, to})
	}
	m.reorder()
	return "", types
}

// neighbours returns the links Neighbours should give, in 0.x's order, and
// the kind of error. The checks come in 0.x's order: the key, then the
// direction.
func (m *model) neighbours(key string, bad bool, dir Direction, typ string) ([]Link, string) {
	switch {
	case bad:
		return nil, "invalid"
	case m.records[key] == nil:
		return nil, "not found"
	case dir != Out && dir != In && dir != Both:
		return nil, "invalid"
	}
	var out []Link
	for l := range m.links {
		if (typ == "" || l.Type == typ) && (dir != In && l.From == key || dir != Out && l.To == key) {
			out = append(out, l)
		}
	}
	// 0.x's orders: out by type and the key it's to, in by type and the
	// key it's from, both ways by type, from and to. One key is the
	// record's in the first two, so all three are by type, from and to.
	slices.SortFunc(out, func(a, b Link) int {
		return cmp.Or(strings.Compare(a.Type, b.Type), strings.Compare(a.From, b.From), strings.Compare(a.To, b.To))
	})
	return out, ""
}

// walk returns the steps Walk should give, and the kind of error. The
// checks come in 0.x's order: the depth, the direction, then the key.
func (m *model) walk(key string, bad bool, dir Direction, typ string, depth int) ([]Step, string) {
	switch {
	case depth < 1 || depth > 32:
		return nil, "invalid"
	case dir != Out && dir != In && dir != Both:
		return nil, "invalid"
	case bad:
		return nil, "invalid"
	case m.records[key] == nil:
		return nil, "not found"
	}
	return referenceWalk(m.links, key, dir, typ, depth), ""
}

// referenceWalk is a walk written plainly, from a set of links alone: every
// key within depth links of start, along links of the type typ, or of every
// type when it's "", in the direction dir, with the fewest links it takes,
// nearest first and then by key, and without start. It goes over every link
// once for each depth, and a key gets depth d when a link reaches it from a
// key at d-1 and it has no depth yet. It gives nil when nothing is in
// reach, as 0.x does.
func referenceWalk(links map[Link]bool, start string, dir Direction, typ string, depth int) []Step {
	dist := map[string]int{start: 0}
	reach := func(from, to string, d int) {
		if at, ok := dist[from]; ok && at == d-1 {
			if _, ok := dist[to]; !ok {
				dist[to] = d
			}
		}
	}
	for d := 1; d <= depth; d++ {
		for l := range links {
			if typ != "" && l.Type != typ {
				continue
			}
			if dir == Out || dir == Both {
				reach(l.From, l.To, d)
			}
			if dir == In || dir == Both {
				reach(l.To, l.From, d)
			}
		}
	}
	var steps []Step
	for key, d := range dist {
		if key != start {
			steps = append(steps, Step{key, d})
		}
	}
	slices.SortFunc(steps, func(a, b Step) int { return cmp.Or(cmp.Compare(a.Depth, b.Depth), strings.Compare(a.Key, b.Key)) })
	return steps
}

// sortedLinks returns the model's links in byte order of the key each is
// from, then type, then the key it's to.
func (m *model) sortedLinks() []Link {
	out := slices.Collect(maps.Keys(m.links))
	slices.SortFunc(out, func(a, b Link) int {
		return cmp.Or(strings.Compare(a.From, b.From), strings.Compare(a.Type, b.Type), strings.Compare(a.To, b.To))
	})
	return out
}

// scan returns the keys a scan gives: every key that starts with prefix
// and comes after after, sorted.
func (m *model) scan(prefix, after string) []string {
	var keys []string
	for key := range m.records {
		if strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func kindOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errs.ErrNotFound):
		return "not found"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	}
	return "error: " + err.Error()
}

// What the generator picks from: a few tables, ids and names, so records
// and fields run into each other, names in mixed case, and a few of each
// that break the rules.
var (
	modelTables = []string{"docs", "people", "t_1"}
	badTables   = []string{"Docs", "hc_x", "", "a-b", "sqlite_x"}
	modelIDs    = []string{"1", "2", "3", "4", "5", "6", "é", "a b", "x:y"}
	badKeys     = []string{"nocolon", "docs:", "Docs:1", "docs:a\x00b", ":1", "hc_x:1", "docs:\xff", "docs:" + strings.Repeat("k", 1100)}
	modelNames  = []string{"title", "Title", "TITLE", "n", "N", "score", "data", "_x", "a1", "A1", "zeta", "Zeta", "vec", "Vec", "VEC"}
	badNames    = []string{"key", "Key", "rowid", "OID", "_rowid_", "has space", "1st", "", "é"}
	goodValues  = []value.Value{value.Int(0), value.Int(1), value.Int(-1), value.Int(math.MaxInt64), value.Int(math.MinInt64),
		value.Real(0), value.Real(math.Copysign(0, -1)), value.Real(1.5), value.Real(math.MaxFloat64), value.Real(math.SmallestNonzeroFloat64),
		value.Text(""), value.Text("a"), value.Text("é"), value.Text("a\x00b"), value.Bytes(""), value.Bytes("\x00\xff")}
	badValues = []value.Value{value.Text("\xff"), value.Real(math.NaN()), value.Real(math.Inf(1)), value.Real(math.Inf(-1)),
		value.Vector([]float32{1})}
	awkwardFloats = []float32{float32(math.Copysign(0, -1)), math.Float32frombits(1), math.MaxFloat32 / 4, 0.1, 1}
	// A scan's prefix is a table's name, a colon, and one of idStarts,
	// which hold the starts of modelIDs, a byte that starts é alone, and
	// ones no key has. afters are what a scan can start after besides keys.
	idStarts    = []string{"", "", "", "1", "5", "a", "a ", "x", "x:", "\xc3", "é", "\xff", "zz"}
	afters      = []string{"a", "docs", "docs;", "people:", "t_1:\xff", "zzz", "docs:\x00", "\xff"}
	badPrefixes = []string{"", "docs", "nocolon", "Docs:", "hc_x:", "sqlite_x:1", ":", ":1", "a-b:", "1docs:"}
	// Link types: case counts in them, they can hold any UTF-8 but a zero
	// byte at the start, and they run to 200 characters.
	linkTypes    = []string{"owns", "cites", "Owns", "x", "é", "a b", "a\x00b", strings.Repeat("ü", 200)}
	badLinkTypes = []string{"", strings.Repeat("t", 201), "\xff", "\x00x"}
)

type gen struct {
	r *rand.Rand
	m *model
}

func (g *gen) one(in int) bool { return g.r.IntN(in) == 0 }

func pick[T any](g *gen, list []T) T { return list[g.r.IntN(len(list))] }

// key returns a key and whether it breaks the rules.
func (g *gen) key() (string, bool) {
	if g.one(30) {
		return pick(g, badKeys), true
	}
	return pick(g, modelTables) + ":" + pick(g, modelIDs), false
}

// vector returns a value for the vector field, mostly a vector of the
// size the model's table has, and whether it breaks a rule whatever the
// state. A vector of another size is the model's to judge.
func (g *gen) vector(tbl string) (value.Value, bool) {
	size := 1 + g.r.IntN(4)
	if t := g.m.tables[tbl]; t != nil && t.size != 0 && !g.one(15) {
		size = t.size
	}
	v := make([]float32, size)
	for i := range v {
		if g.one(4) {
			v[i] = pick(g, awkwardFloats)
		} else {
			v[i] = float32(g.r.NormFloat64())
		}
	}
	v[g.r.IntN(size)] = 1 // never all zero, unless made so below
	switch g.r.IntN(30) {
	case 0, 1, 2:
		return value.Null(), false
	case 3:
		return value.Vector(make([]float32, size)), true
	case 4:
		v[0] = float32(math.NaN())
		return value.Vector(v), true
	case 5:
		return value.Vector(nil), true
	case 6:
		return value.Text("[1, 2]"), true
	}
	return value.Vector(v), false
}

// put returns a put's key and fields, and whether something in it breaks
// a rule whatever the state.
func (g *gen) put() (string, []format.Field, bool) {
	key, bad := g.key()
	var fields []format.Field
	for n := g.r.IntN(5); n > 0; n-- {
		name, fieldBad := pick(g, modelNames), false
		if g.one(40) {
			name, fieldBad = pick(g, badNames), true
		}
		var v value.Value
		switch {
		case rules.IsVec(name):
			var vecBad bool
			v, vecBad = g.vector(tableOfKey(key + ":"))
			fieldBad = fieldBad || vecBad
		case g.one(3):
			v = value.Null() // a new field with a null still joins the table
		case g.one(40):
			v, fieldBad = pick(g, badValues), true
		default:
			v = pick(g, goodValues)
		}
		if slices.ContainsFunc(fields, func(f format.Field) bool { return strings.ToLower(f.Name) == strings.ToLower(name) }) {
			if !g.one(10) {
				continue // the same field twice is a mistake to make only now and then
			}
			fieldBad = true
		}
		fields = append(fields, format.Field{Name: name, Value: v})
		bad = bad || fieldBad
	}
	return key, fields, bad
}

// linkType returns a link type, and whether it breaks the rules.
func (g *gen) linkType() (string, bool) {
	if g.one(25) {
		return pick(g, badLinkTypes), true
	}
	return pick(g, linkTypes), false
}

// readType returns a type for a read: "" for every type half the time, and
// otherwise mostly a type one of the model's links has, and now and then one
// that breaks the rules, which follows no links.
func (g *gen) readType() string {
	switch {
	case g.one(2):
		return ""
	case len(g.m.links) > 0 && !g.one(3):
		return pick(g, g.m.order).Type
	}
	typ, _ := g.linkType()
	return typ
}

// direction returns a direction, and now and then a number that isn't one.
func (g *gen) direction() Direction {
	if g.one(15) {
		return pick(g, []Direction{3, -1})
	}
	return Direction(g.r.IntN(3))
}

// depth returns a walk's depth, mostly 1 to 4, now and then the most, and
// now and then one outside the rules.
func (g *gen) depth() int {
	switch {
	case g.one(25):
		return pick(g, []int{0, -1, MaxDepth + 1})
	case g.one(10):
		return MaxDepth
	}
	return 1 + g.r.IntN(4)
}

// held returns the key of a record the model holds, most of the time, and
// otherwise a key as key gives one, and whether it breaks the rules.
func (g *gen) held() (string, bool) {
	if len(g.m.records) == 0 || g.one(4) {
		return g.key()
	}
	keys := slices.Sorted(maps.Keys(g.m.records))
	return pick(g, keys), false
}

// linked returns a key at one end of one of the model's links, most of the
// time, and otherwise a key as held or key gives one, and whether it breaks
// the rules.
func (g *gen) linked() (string, bool) {
	if g.one(10) {
		return g.key()
	}
	if len(g.m.links) == 0 || g.one(4) {
		return g.held()
	}
	l := pick(g, g.m.order)
	if g.one(2) {
		return l.From, false
	}
	return l.To, false
}

// link returns a link's arguments: two keys, either of which may break the
// rules or have no record, though most are records the model holds, and a
// type. A quarter of the links go from a record to itself, some join two
// records the model links already, with another type, and some are links
// the model has already.
func (g *gen) link() linkArgs {
	var a linkArgs
	a.from, a.badFrom = g.held()
	a.to, a.badTo = g.held()
	a.typ, a.badType = g.linkType()
	switch {
	case g.one(4):
		a.to, a.badTo = a.from, a.badFrom
	case g.one(3) && len(g.m.links) > 0:
		l := pick(g, g.m.order)
		a.from, a.to, a.badFrom, a.badTo = l.From, l.To, false, false
		if g.one(3) {
			a.typ, a.badType = l.Type, false
		}
	}
	return a
}

// unlinkArgs returns an unlink's arguments: mostly a link the model has,
// with its type or with "" for every type, and otherwise any two keys and
// any type.
func (g *gen) unlinkArgs() (from, typ, to string) {
	if len(g.m.links) > 0 && !g.one(4) {
		l := pick(g, g.m.order)
		if g.one(3) {
			l.Type = ""
		}
		return l.From, l.Type, l.To
	}
	from, _ = g.key()
	to, _ = g.key()
	return from, g.readType(), to
}

// scanArgs returns a scan's prefix and after, and whether the prefix breaks
// the rules. after is "" half the time, and otherwise a key, which may be
// in another table or break the rules itself, a string inside the prefix,
// the prefix itself, or something no key is.
func (g *gen) scanArgs() (prefix, after string, bad bool) {
	if g.one(12) {
		prefix, bad = pick(g, badPrefixes), true
	} else {
		prefix = pick(g, modelTables) + ":" + pick(g, idStarts)
	}
	switch g.r.IntN(8) {
	case 4:
		after, _ = g.key()
	case 5:
		after = prefix + pick(g, idStarts)
	case 6:
		after = prefix
	case 7:
		after = pick(g, afters)
	}
	return prefix, after, bad
}

// checkScan pulls records from a cursor and checks them against want, the
// keys the model's scan gives, in order, and each record against the
// model's. It stops after stop records, or at the end when stop is -1, and
// then a cursor that has given false must keep giving it.
func checkScan(t *testing.T, r Reader, m *model, c Cursor, desc string, want []string, stop int) {
	t.Helper()
	for i := 0; i != stop; i++ {
		rec, more := c.Next()
		if !more {
			if i != len(want) {
				t.Fatalf("%s gave %d records, where the model gives %q", desc, i, want)
			}
			if rec, again := c.Next(); again {
				t.Fatalf("%s gave false, then %s", desc, rec.Key)
			}
			return
		}
		if i >= len(want) || rec.Key != want[i] {
			t.Fatalf("%s gave %s as record %d, where the model gives %q", desc, rec.Key, i, want)
		}
		compareRecord(t, r, m, rec)
	}
}

// liveScan is a cursor kept open while other steps change the store. Each
// record it gives must be the first in the model as the model is then that
// starts with its prefix and comes after the last one it gave.
type liveScan struct {
	c      Cursor
	desc   string
	prefix string
	after  string // the last key it gave, or the after it began with
	given  int
}

// pull takes one record from the cursor and checks it, and reports false
// once the cursor has no more. It counts the pulls where the cursor has to
// find its place again, since the store has changed under it.
func (l *liveScan) pull(t *testing.T, r Reader, m *model, outcomes map[string]int) bool {
	t.Helper()
	if c := l.c.(*cursor); c.epoch != c.s.epoch && !c.done {
		outcomes["live scan moved"]++
	}
	want := m.scan(l.prefix, l.after)
	rec, more := l.c.Next()
	if more != (len(want) > 0) || more && rec.Key != want[0] {
		t.Fatalf("%s, after %d records, the last %q, gave %q, %v, where the model gives %q", l.desc, l.given, l.after, rec.Key, more, want)
	}
	if !more {
		return false
	}
	compareRecord(t, r, m, rec)
	l.after = rec.Key
	l.given++
	return true
}

// TestTheModel runs random puts, gets, scans, links, unlinks, reads of
// links, walks, deletes and drops on the store and on the model, and checks
// that they agree on every answer, that a write that fails changes nothing,
// and that every write's changes, applied to a second store, give a copy of
// the first. A scan is pulled to its end or stopped part of the way. Now and
// then a cursor stays open over the steps that follow, and gives a record
// every few steps, which must be the next in the model as the steps between
// have left it. Deletes and drops take the links of their records both
// ways, which the model does by looking at every link.
func TestTheModel(t *testing.T) {
	seeds, steps := 20, 4000
	if testing.Short() {
		seeds, steps = 4, 2000
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runModel(t, seed, steps) })
	}
}

func runModel(t *testing.T, seed uint64, steps int) {
	s, replica := New(), New()
	m := newModel()
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x51)), m: m}
	var changes []format.Change
	outcomes := map[string]int{}
	var live, ended *liveScan // a cursor open over the steps, and the last one that ended
	for step := 0; step < steps; step++ {
		if live != nil && g.one(3) && !live.pull(t, s, m, outcomes) {
			outcomes["live scan ended"]++
			live, ended = nil, live
		}
		if ended != nil && g.one(5) {
			// A cursor that has given false keeps giving it, whatever the
			// steps since have put after its place.
			if rec, more := ended.c.Next(); more {
				t.Fatalf("step %d: %s gave false, and then %s", step, ended.desc, rec.Key)
			}
			outcomes["ended scan still ended"]++
		}
		before := len(changes)
		var desc, want, got string
		var err error
		switch w := g.r.IntN(100); {
		case w < 34:
			key, fields, bad := g.put()
			given := slices.Clone(fields)
			desc = fmt.Sprintf("put %q %v", key, fields)
			_, existed := s.tables[tableOfKey(key+":")]
			changes, err = s.Put(changes, key, fields)
			want, got = m.put(key, fields, bad), kindOf(err)
			if !slices.Equal(given, fields) {
				t.Fatalf("step %d: %s changed its fields to %v", step, desc, fields)
			}
			if err == nil {
				checkPutChanges(t, s, changes[before:], key, existed)
			}
		case w < 42:
			key, bad := g.key()
			desc = "get " + key
			r, err := s.Get(key)
			want, got = "", kindOf(err)
			switch {
			case bad:
				want = "invalid"
			case m.records[key] == nil:
				want = "not found"
			}
			if err == nil && want == "" {
				compareRecord(t, s, m, r)
			}
		case w < 49:
			prefix, after, bad := g.scanArgs()
			desc = fmt.Sprintf("scan %q %q", prefix, after)
			var c Cursor
			c, err = s.Scan(prefix, after)
			want, got = "", kindOf(err)
			if bad {
				want = "invalid"
			}
			if err != nil || want != "" {
				break
			}
			if live == nil && g.one(4) {
				live = &liveScan{c: c, desc: "the live " + desc, prefix: prefix, after: after}
				outcomes["live scan"]++
				break
			}
			stop := -1
			if g.one(4) {
				stop = g.r.IntN(4)
			}
			checkScan(t, s, m, c, desc, m.scan(prefix, after), stop)
		case w < 69:
			a := g.link()
			desc = fmt.Sprintf("link %q %q %q", a.from, a.typ, a.to)
			changes, err = s.Link(changes, a.from, a.typ, a.to)
			var added bool
			want, added = m.link(a)
			got = kindOf(err)
			if err == nil && got == want {
				var wantChanges []format.Change
				if added {
					wantChanges = []format.Change{{Op: format.Link, Key: a.from, Type: a.typ, To: a.to}}
				} else {
					outcomes["link already there"]++
				}
				if !slices.EqualFunc(changes[before:], wantChanges, equalChange) {
					t.Fatalf("step %d: %s gave the changes %v", step, desc, changes[before:])
				}
			}
		case w < 74:
			from, typ, to := g.unlinkArgs()
			desc = fmt.Sprintf("unlink %q %q %q", from, typ, to)
			changes, err = s.Unlink(changes, from, typ, to)
			var types []string
			want, types = m.unlink(from, typ, to)
			got = kindOf(err)
			if err == nil && got == want {
				var wantChanges []format.Change
				for _, typ := range types {
					wantChanges = append(wantChanges, format.Change{Op: format.Unlink, Key: from, Type: typ, To: to})
				}
				if !slices.EqualFunc(changes[before:], wantChanges, equalChange) {
					t.Fatalf("step %d: %s gave the changes %v", step, desc, changes[before:])
				}
				if len(types) > 1 {
					outcomes["unlink of several types"]++
				}
			}
		case w < 80:
			key, bad := g.linked()
			dir, typ := g.direction(), g.readType()
			desc = fmt.Sprintf("neighbours %q %v %q", key, dir, typ)
			var links []Link
			links, err = s.Neighbours(key, dir, typ)
			wantLinks, kind := m.neighbours(key, bad, dir, typ)
			want, got = kind, kindOf(err)
			if err == nil && kind == "" && (!slices.Equal(links, wantLinks) || links != nil && len(links) == 0) {
				t.Fatalf("step %d: %s gave %v, where the model gives %v", step, desc, links, wantLinks)
			}
			if len(links) > 0 {
				outcomes["neighbours found some"]++
			}
		case w < 88:
			key, bad := g.linked()
			dir, typ, depth := g.direction(), g.readType(), g.depth()
			if g.one(3) {
				dir = Both
			}
			desc = fmt.Sprintf("walk %q %v %q %d", key, dir, typ, depth)
			var steps []Step
			steps, err = s.Walk(key, dir, typ, depth)
			wantSteps, kind := m.walk(key, bad, dir, typ, depth)
			want, got = kind, kindOf(err)
			if err == nil && kind == "" && (!slices.Equal(steps, wantSteps) || steps != nil && len(steps) == 0) {
				t.Fatalf("step %d: %s gave %v, where the model gives %v", step, desc, steps, wantSteps)
			}
			if len(steps) > 0 && steps[len(steps)-1].Depth > 1 {
				outcomes["walk went further than one link"]++
			}
		case w < 97:
			key, bad := g.key()
			desc = "delete " + key
			if linked(m, func(k string) bool { return k == key }) {
				outcomes["delete with links"]++
			}
			changes, err = s.Delete(changes, key)
			want, got = m.delete(key, bad), kindOf(err)
			if err == nil && !slices.EqualFunc(changes[before:], []format.Change{{Op: format.Delete, Key: key}}, equalChange) {
				t.Fatalf("step %d: %s gave the changes %v", step, desc, changes[before:])
			}
		default:
			name, bad := pick(g, modelTables), false
			switch {
			case g.one(5):
				name, bad = pick(g, badTables), true
			case g.one(8):
				name = "nosuch"
			}
			desc = "drop " + name
			if linked(m, func(k string) bool { return tableOfKey(k) == name }) {
				outcomes["drop with links"]++
			}
			changes, err = s.Drop(changes, name)
			want, got = m.drop(name, bad), kindOf(err)
			if err == nil && !slices.EqualFunc(changes[before:], []format.Change{{Op: format.Drop, Table: name}}, equalChange) {
				t.Fatalf("step %d: %s gave the changes %v", step, desc, changes[before:])
			}
		}
		if got != want {
			t.Fatalf("step %d: %s: got %q (%v), the model says %q", step, desc, got, err, want)
		}
		outcomes[strings.Fields(desc)[0]+" "+cmp.Or(got, "ok")]++
		if err != nil && len(changes) != before {
			t.Fatalf("step %d: %s failed and still gave the changes %v", step, desc, changes[before:])
		}
		for _, c := range changes[before:] {
			if err := replica.Apply(c); err != nil {
				t.Fatalf("step %d: %s: the replica refused %v: %v", step, desc, c, err)
			}
		}
		// A write that fails must have changed nothing. A read changes
		// nothing anyway, so a failed one needs no look at the whole store.
		read := slices.Contains([]string{"get", "scan", "neighbours", "walk"}, strings.Fields(desc)[0])
		if err != nil && !read || step%25 == 0 {
			compareAll(t, s, m)
			if a, b := dump(s), dump(replica); a != b {
				t.Fatalf("step %d, after %s, the replica differs:\nstore:\n%s\nreplica:\n%s", step, desc, a, b)
			}
		}
	}
	compareAll(t, s, m)
	checkInvariants(t, replica)
	// Every kind of step has to come up, working and failing, or the test
	// tests less than it says.
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "scan ok", "scan invalid",
		"live scan", "live scan moved", "live scan ended", "ended scan still ended", "link ok", "link invalid",
		"link not found", "link already there", "unlink ok", "unlink not found", "unlink of several types",
		"neighbours ok", "neighbours invalid", "neighbours not found", "neighbours found some", "walk ok", "walk invalid",
		"walk not found", "walk went further than one link", "delete ok", "delete invalid", "delete not found",
		"delete with links", "drop ok", "drop invalid", "drop not found", "drop with links"} {
		if outcomes[o] < steps/1000 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], steps, outcomes)
		}
	}
	if testing.Verbose() && seed == 1 {
		t.Logf("outcomes: %v", outcomes)
	}
}

// linked reports whether the model has a link from or to a key that is
// says yes to.
func linked(m *model, is func(key string) bool) bool {
	for l := range m.links {
		if is(l.From) || is(l.To) {
			return true
		}
	}
	return false
}

// checkPutChanges checks the changes a put gave: a CreateTable with no
// fields and size 0 first when the put created the table, then the Put,
// with its fields in byte order of name, each spelt as the table spells
// it.
func checkPutChanges(t *testing.T, s *Store, got []format.Change, key string, existed bool) {
	t.Helper()
	tbl := tableOfKey(key)
	if !existed {
		if len(got) == 0 || !equalChange(got[0], format.Change{Op: format.CreateTable, Table: tbl}) {
			t.Fatalf("a put that created table %s gave %v", tbl, got)
		}
		got = got[1:]
	}
	if len(got) != 1 || got[0].Op != format.Put || got[0].Key != key {
		t.Fatalf("a put of %s gave %v", key, got)
	}
	shape, _ := s.Table(tbl)
	for i, f := range got[0].Fields {
		if i > 0 && got[0].Fields[i-1].Name >= f.Name {
			t.Fatalf("the put of %s isn't in byte order of name: %v", key, got[0])
		}
		if !slices.Contains(shape.Fields, f.Name) {
			t.Fatalf("the put of %s names %s, which table %s spells otherwise: %v", key, f.Name, tbl, shape.Fields)
		}
	}
}

func equalChange(a, b format.Change) bool {
	return a.Op == b.Op && a.Table == b.Table && a.Size == b.Size && slices.Equal(a.Names, b.Names) && a.Key == b.Key &&
		slices.Equal(a.Fields, b.Fields) && a.Type == b.Type && a.To == b.To
}

// compareRecord checks one record read through s, the store or a
// transaction, against the model's.
func compareRecord(t *testing.T, s Reader, m *model, r Record) {
	t.Helper()
	tbl := tableOfKey(r.Key)
	shape, ok := s.Table(tbl)
	if !ok {
		t.Fatalf("%s has no table", r.Key)
	}
	got := map[string]value.Value{}
	for _, f := range r.Fields {
		got[strings.ToLower(shape.Fields[f.Index])] = f.Value
	}
	if r.Vec != nil {
		got[strings.ToLower(shape.Fields[shape.Vec])] = value.Vector(r.Vec)
	}
	want := m.records[r.Key]
	if len(got) != len(want) {
		t.Fatalf("%s holds %v, and the model %v", r.Key, got, want)
	}
	for name, v := range want {
		if got[name] != v {
			t.Fatalf("%s holds %v, and the model %v", r.Key, got, want)
		}
	}
}

// compareAll checks every table and record of the store against the model.
func compareAll(t *testing.T, s *Store, m *model) {
	t.Helper()
	checkInvariants(t, s)
	if len(s.tables) != len(m.tables) || len(s.records) != len(m.records) {
		t.Fatalf("the store holds %d tables and %d records, the model %d and %d", len(s.tables), len(s.records), len(m.tables), len(m.records))
	}
	for name, mt := range m.tables {
		shape, ok := s.Table(name)
		if !ok {
			t.Fatalf("the store lacks table %s", name)
		}
		vec := slices.IndexFunc(mt.fields, func(f string) bool { return strings.ToLower(f) == "vec" })
		if !slices.Equal(shape.Fields, mt.fields) || shape.Vec != vec || shape.Size != mt.size {
			t.Fatalf("table %s is %+v, and the model's has the fields %q, the vector field at %d and size %d",
				name, shape, mt.fields, vec, mt.size)
		}
	}
	for key := range m.records {
		r, err := s.Get(key)
		if err != nil {
			t.Fatalf("Get(%s): %v", key, err)
		}
		compareRecord(t, s, m, r)
	}
	// Every link the records hold out of them is the model's, and the model
	// has no more. checkInvariants has checked that the links in mirror
	// them.
	n := 0
	for key, r := range s.records {
		for h := range r.out.ofType("") {
			if l := (Link{key, h.typ.Value(), h.other.key}); !m.links[l] {
				t.Fatalf("the store has the link %v, which the model hasn't", l)
			}
			n++
		}
	}
	if n != len(m.links) {
		t.Fatalf("the store holds %d links, and the model %d: %v", n, len(m.links), m.sortedLinks())
	}
}

// checkInvariants checks what the store's layout promises: each table's
// index and vector field agree with its list, and each record's fields
// are in order of place, without nulls or the vector, inside its table's
// list, and its vector has the table's size. Each table's keys hold its
// records' keys, and only those, in blocks as checkOrder checks them. Each
// record's links both ways are as checkLinks checks them, so each link has
// its two halves, and every half points at a record in the store.
func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	held := 0 // keys in the tables' orders
	for name, tb := range s.tables {
		checkOrder(t, name, &tb.keys)
		for r := range tb.keys.records {
			if r.table != tb || s.records[r.key] != r {
				t.Fatalf("table %s's keys hold %s, which the hash table has as %v", name, r.key, s.records[r.key])
			}
		}
		held += tb.keys.n
		if tb.name != name || len(tb.index) != len(tb.fields) {
			t.Fatalf("table %s is %+v", name, tb)
		}
		vec := -1
		for p, f := range tb.fields {
			if tb.index[rules.Fold(f)] != p {
				t.Fatalf("table %s's index has %s at %d, where its list has it at %d", name, f, tb.index[rules.Fold(f)], p)
			}
			if rules.IsVec(f) {
				vec = p
			}
		}
		if tb.vec != vec || tb.size < 0 || tb.size > 0 && vec < 0 {
			t.Fatalf("table %s has the vector field at %d and size %d, with the fields %q", name, tb.vec, tb.size, tb.fields)
		}
	}
	for key, r := range s.records {
		tb := s.tables[tableOfKey(key)]
		if r.key != key || r.table != tb || tb == nil {
			t.Fatalf("record %s is filed under %s, in table %v", r.key, key, r.table)
		}
		for i, f := range r.fields {
			if i > 0 && r.fields[i-1].Index >= f.Index || f.Index < 0 || f.Index >= len(tb.fields) || f.Index == tb.vec || f.Value.IsNull() {
				t.Fatalf("record %s has the fields %v, in a table with the fields %q", key, r.fields, tb.fields)
			}
		}
		if r.vec != nil && len(r.vec) != tb.size {
			t.Fatalf("record %s has a vector of %d values, in a table of size %d", key, len(r.vec), tb.size)
		}
		checkLinks(t, s, r, true)
		checkLinks(t, s, r, false)
	}
	if held != len(s.records) {
		t.Fatalf("the tables' keys hold %d keys, and the hash table %d records", held, len(s.records))
	}
}

// checkLinks checks one of a record's lists, its links out when out is
// true and its links in otherwise: the layout, as checkHalves checks it,
// and each half. Its type keeps the rules, and is the handle the unique
// package gives its text. It points at a record in the store, whose other
// list holds the half's mirror, with the same handle. It returns how many
// halves the list holds.
func checkLinks(t *testing.T, s *Store, r *record, out bool) int {
	l, way := &r.in, "in"
	if out {
		l, way = &r.out, "out"
	}
	count := len(checkHalves(t, r.key+"'s links "+way, l))
	for h := range l.ofType("") {
		if s.records[h.other.key] != h.other {
			t.Fatalf("%s's links %s point at %s, which isn't in the store", r.key, way, h.other.key)
		}
		if rules.LinkType(h.typ.Value()) != nil || h.typ != makeType(h.typ.Value()) {
			t.Fatalf("%s's links %s hold the type %q, as a handle of its own", r.key, way, h.typ.Value())
		}
		mirror := &h.other.out
		if out {
			mirror = &h.other.in
		}
		if m, ok := mirror.get(linkAt{h.typ.Value(), r.key}); !ok || m.other != r || m.typ != h.typ {
			t.Fatalf("%s's links %s hold %q %s, and the other end hasn't got its half", r.key, way, h.typ.Value(), h.other.key)
		}
	}
	return count
}

// checkOrder checks the layout of one table's keys: blocks of 1 to
// blockMax keys, in byte order across the blocks, each with the record
// whose key it is, n counting them, and nothing left past a block's length.
func checkOrder(t *testing.T, name string, o *keyOrder) {
	t.Helper()
	count, last := 0, ""
	for bi, b := range o.blocks {
		if len(b) == 0 || cap(b) > blockMax {
			t.Fatalf("table %s's block %d holds %d keys, with room for %d", name, bi, len(b), cap(b))
		}
		for _, e := range b {
			if count > 0 && e.key <= last || e.r == nil || e.r.key != e.key {
				t.Fatalf("table %s's block %d holds %q after %q, with the record %v", name, bi, e.key, last, e.r)
			}
			last = e.key
			count++
		}
		for _, e := range b[len(b):cap(b)] {
			if e != (entry{}) {
				t.Fatalf("table %s's block %d keeps %q past its length", name, bi, e.key)
			}
		}
	}
	if count != o.n || o.blocks != nil && len(o.blocks) == 0 {
		t.Fatalf("table %s's keys count %d, and hold %d in %d blocks", name, o.n, count, len(o.blocks))
	}
}

// dump writes out the whole store in order, so two stores can be compared
// as text. Each table's line ends with its keys, in the order it keeps them.
func dump(s *Store) string {
	var b strings.Builder
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tb := s.tables[name]
		var keys []string
		for r := range tb.keys.records {
			keys = append(keys, r.key)
		}
		fmt.Fprintf(&b, "table %s %q vec %d size %d keys %q\n", name, tb.fields, tb.vec, tb.size, keys)
	}
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Each record's line is written by hand, without fmt, since the tests
	// that compare stores write thousands of them: its key, each field's
	// place and value, its vector's bits, and each of its links out and in
	// as its type and the key at the other end, each after its length.
	for _, key := range keys {
		r := s.records[key]
		b.WriteString(strconv.Quote(key))
		b.WriteByte(':')
		for _, f := range r.fields {
			b.WriteByte(' ')
			b.WriteString(strconv.Itoa(f.Index))
			b.WriteByte('=')
			dumpValue(&b, f.Value)
		}
		if r.vec != nil {
			b.WriteString(" vec=")
			for _, x := range r.vec {
				b.WriteString(strconv.FormatUint(uint64(math.Float32bits(x)), 16))
				b.WriteByte(',')
			}
		}
		for _, l := range []*links{&r.out, &r.in} {
			b.WriteString(" |")
			for h := range l.ofType("") {
				for _, x := range []string{h.typ.Value(), h.other.key} {
					b.WriteString(strconv.Itoa(len(x)))
					b.WriteByte(':')
					b.WriteString(x)
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// dumpValue writes a value for dump: its kind's letter and its bits, so -0
// and 0, or text and bytes, never look the same.
func dumpValue(b *strings.Builder, v value.Value) {
	switch v.Kind() {
	case value.KindInt:
		b.WriteByte('i')
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case value.KindReal:
		b.WriteByte('r')
		b.WriteString(strconv.FormatUint(math.Float64bits(v.Real()), 16))
	case value.KindText:
		b.WriteByte('t')
		b.WriteString(strconv.Quote(v.Text()))
	case value.KindBytes:
		b.WriteByte('b')
		b.WriteString(hex.EncodeToString([]byte(v.Raw())))
	default:
		b.WriteString(v.String())
	}
}
