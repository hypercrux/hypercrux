// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
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
	return ""
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

// TestTheModel runs random puts, gets, deletes and drops on the store and
// on the model, and checks that they agree on every answer, that a write
// that fails changes nothing, and that every write's changes, applied to
// a second store, give a copy of the first.
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
	m := &model{tables: map[string]*modelTable{}, records: map[string]map[string]value.Value{}}
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x51)), m: m}
	var changes []format.Change
	outcomes := map[string]int{}
	for step := 0; step < steps; step++ {
		before := len(changes)
		var desc, want, got string
		var err error
		switch w := g.r.IntN(100); {
		case w < 55:
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
		case w < 75:
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
		case w < 95:
			key, bad := g.key()
			desc = "delete " + key
			changes, err = s.Delete(changes, key)
			want, got = m.delete(key, bad), kindOf(err)
			if err == nil && !slices.EqualFunc(changes[before:], []format.Change{{Op: format.Delete, Key: key}}, equalChange) {
				t.Fatalf("step %d: %s gave the changes %v", step, desc, changes[before:])
			}
		default:
			name, bad := pick(g, modelTables), false
			if g.one(5) {
				name, bad = pick(g, badTables), true
			}
			desc = "drop " + name
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
		if err != nil || step%25 == 0 {
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
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "delete ok",
		"delete invalid", "delete not found", "drop ok", "drop invalid", "drop not found"} {
		if outcomes[o] < steps/1000 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], steps, outcomes)
		}
	}
	if testing.Verbose() && seed == 1 {
		t.Logf("outcomes: %v", outcomes)
	}
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
}

// checkInvariants checks what the store's layout promises: each table's
// index and vector field agree with its list, and each record's fields
// are in order of place, without nulls or the vector, inside its table's
// list, and its vector has the table's size.
func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	for name, tb := range s.tables {
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
	}
}

// dump writes out the whole store in order, so two stores can be compared
// as text.
func dump(s *Store) string {
	var b strings.Builder
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tb := s.tables[name]
		fmt.Fprintf(&b, "table %s %q vec %d size %d\n", name, tb.fields, tb.vec, tb.size)
	}
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := s.records[key]
		fmt.Fprintf(&b, "%q:", key)
		for _, f := range r.fields {
			fmt.Fprintf(&b, " %d=%v", f.Index, f.Value)
		}
		if r.vec != nil {
			fmt.Fprintf(&b, " vec=%v", value.Vector(r.vec))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
