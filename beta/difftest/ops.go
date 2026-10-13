// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package difftest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"unicode/utf8"

	c "github.com/hypercrux/hypercrux/beta/conformance"
)

// Op is one step of a sequence: a call on the database, or an Update
// holding several. A sequence is plain data, so a failing one can be saved
// as JSON and replayed.
type Op struct {
	Kind   string  `json:"op"`
	Key    string  `json:"key,omitempty"` // the key, or Scan's prefix
	To     string  `json:"to,omitempty"`
	Type   string  `json:"type,omitempty"`
	Table  string  `json:"table,omitempty"`
	After  string  `json:"after,omitempty"`
	N      int     `json:"n,omitempty"`   // Scan's limit, Nearest's k or Walk's depth
	Dir    int     `json:"dir,omitempty"` // 0 out, 1 in, 2 both
	Fields []Field `json:"fields,omitempty"`
	Vec    *Value  `json:"vec,omitempty"` // Nearest's query
	Where  string  `json:"where,omitempty"`
	SQL    string  `json:"sql,omitempty"`
	Args   []Value `json:"args,omitempty"` // for Where and SQL
	Ops    []Op    `json:"ops,omitempty"`  // the steps of an Update
	Fail   bool    `json:"fail,omitempty"` // the Update returns an error and rolls back
}

// Field is one field of a Put.
type Field struct {
	Name  string `json:"name"`
	Value Value  `json:"value"`
}

// Value is a field's value or an argument: nil, a bool, an int64, a
// float64, a string, a []byte or a conformance.Vector. In JSON it carries
// its type, floats are written by their bits, and text that isn't UTF-8 is
// written in base64, so every value comes back exactly.
type Value struct{ V any }

func (v Value) MarshalJSON() ([]byte, error) {
	var m map[string]any
	switch x := v.V.(type) {
	case nil:
		m = map[string]any{"null": true}
	case bool:
		m = map[string]any{"bool": x}
	case int64:
		m = map[string]any{"int": strconv.FormatInt(x, 10)}
	case float64:
		m = map[string]any{"real": fmt.Sprintf("%016x", math.Float64bits(x))}
	case string:
		if utf8.ValidString(x) {
			m = map[string]any{"text": x}
		} else {
			m = map[string]any{"text64": base64.StdEncoding.EncodeToString([]byte(x))}
		}
	case []byte:
		m = map[string]any{"bytes": base64.StdEncoding.EncodeToString(x)}
	case c.Vector:
		bits := make([]string, len(x))
		for i, f := range x {
			bits[i] = fmt.Sprintf("%08x", math.Float32bits(f))
		}
		m = map[string]any{"vector": bits}
	default:
		return nil, fmt.Errorf("difftest: a %T can't be saved", v.V)
	}
	return json.Marshal(m)
}

func (v *Value) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(m) != 1 {
		return fmt.Errorf("difftest: a value has one member, not %d", len(m))
	}
	for k, raw := range m {
		var s string
		var err error
		switch k {
		case "null":
			v.V = nil
		case "bool":
			var x bool
			err = json.Unmarshal(raw, &x)
			v.V = x
		case "int":
			if err = json.Unmarshal(raw, &s); err == nil {
				v.V, err = strconv.ParseInt(s, 10, 64)
			}
		case "real":
			if err = json.Unmarshal(raw, &s); err == nil {
				var bits uint64
				bits, err = strconv.ParseUint(s, 16, 64)
				v.V = math.Float64frombits(bits)
			}
		case "text":
			err = json.Unmarshal(raw, &s)
			v.V = s
		case "text64", "bytes":
			if err = json.Unmarshal(raw, &s); err == nil {
				var data []byte
				data, err = base64.StdEncoding.DecodeString(s)
				if k == "bytes" {
					v.V = append([]byte{}, data...)
				} else {
					v.V = string(data)
				}
			}
		case "vector":
			var bits []string
			if err = json.Unmarshal(raw, &bits); err == nil {
				vec := make(c.Vector, len(bits))
				for i, s := range bits {
					var u uint64
					if u, err = strconv.ParseUint(s, 16, 32); err != nil {
						break
					}
					vec[i] = math.Float32frombits(uint32(u))
				}
				v.V = vec
			}
		default:
			err = fmt.Errorf("difftest: unknown kind of value %q", k)
		}
		if err != nil {
			return fmt.Errorf("difftest: value %s: %w", b, err)
		}
	}
	return nil
}

// What the generator picks from. A few names and ids keep records, links
// and fields running into each other, and the odd ones test the rules.
var (
	tables    = []string{"docs", "people", "t_1"}
	badTables = []string{"Docs", "hc_x", "", "a-b"}
	ids       = []string{"1", "2", "3", "4", "5", "6", "7", "8", "é", "a b", "😀", "2026-10-07", "x/y", " "}
	badKeys   = []string{"nocolon", "docs:", "Docs:1", "docs:a\x00b", ":1", "hc_x:1", "docs:" + strings.Repeat("k", 1100)}
	names     = []string{"title", "n", "score", "data", "Mixed", "vec", "vec", "vec"}
	badNames  = []string{"key", "rowid", "bad name", "a-b"}
	linkTypes = []string{"owns", "cites", "x", "é", "a b"}
	badTypes  = []string{"", strings.Repeat("t", 201)}
	ints      = []int64{0, 1, -1, 42, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1}
	reals     = []float64{0, math.Copysign(0, -1), 1, -1.5, 0.1, 1e21, 1e-7, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, 2.2250738585072014e-308, 123456789}
	texts = []string{"", "a", "Q3 plan", "é", "😀", "a\x00b", "\n\t\x01", "שלום", `{"x":1}`, "null", strings.Repeat("y", 300)}
	f32s  = []float32{float32(math.Copysign(0, -1)), math.MaxFloat32 / 4, math.SmallestNonzeroFloat32, 1.17549435e-38, 1e-7, 0.1, 1}
)

// statements is the plain SQL a sequence runs, each with its arguments.
// Every query orders its rows, so two engines can be compared row by row.
// The last is the crux query, in 0.x's form: a walk joined to a table,
// filtered on a field, and ordered by distance.
//
// Two of the Beta's differences from 0.x on purpose are kept out (SQL.md,
// "Where the Beta differs from 0.x"). The values an UPDATE writes are never
// an infinite real or text that isn't UTF-8, which 0.x stores and the Beta
// refuses as Put does (sqlValue). And the sum adds whole numbers of a
// modest size only, since the Beta adds a table's rows in key order and
// 0.x in the order it put them, which can change a real sum's last bits,
// or whether it overflows, at the extremes.
var statements = []struct {
	sql  string
	args func(g *gen) []Value
}{
	{`SELECT key, title, n FROM docs ORDER BY key`, nil},
	{`SELECT count(*), sum(n) FROM people WHERE typeof(n) = 'integer' AND n BETWEEN -1000000000 AND 1000000000`, nil},
	{`UPDATE docs SET n = ? WHERE key = ?`, func(g *gen) []Value { return []Value{{g.sqlValue()}, {g.key()}} }},
	{`DELETE FROM people WHERE key = ?`, func(g *gen) []Value { return []Value{{g.key()}} }},
	{`INSERT INTO people (key) VALUES (?)`, func(g *gen) []Value { return []Value{{g.key()}} }},
	{`SELECT d.key FROM json_each(walk(?, 2)) w JOIN docs d ON d.key = w.value ORDER BY d.key`,
		func(g *gen) []Value { return []Value{{g.key()}} }},
	{`SELECT key FROM t_1 WHERE vec IS NOT NULL ORDER BY distance(vec, ?), key LIMIT 3`,
		func(g *gen) []Value { return []Value{{g.vector("t_1")}} }},
	{cruxSQL, func(g *gen) []Value { return g.cruxArgs(g.cruxStart()) }},
}

// cruxSQL is the crux query, in 0.x's form.
const cruxSQL = `SELECT d.key, d.title FROM json_each(walk(?, ?)) w JOIN docs d ON d.key = w.value WHERE d.n > ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?`

// sqlValue is a value for an UPDATE to write: one of value's, with the
// values Put refuses that 0.x's SQL stores, an infinite real and text that
// isn't UTF-8, made NULL. A NaN stays, since SQL takes it as NULL in both.
func (g *gen) sqlValue() any {
	v := g.value()
	if f, ok := v.(float64); ok && math.IsInf(f, 0) || v == "\xff" {
		return nil
	}
	return v
}

// cruxStart is a start for the crux query: a record that links to a record
// of docs, most of the time, as far as the generator knows.
func (g *gen) cruxStart() string {
	start := g.linked()
	var toDocs []string
	for _, l := range g.links {
		if strings.HasPrefix(l.To, "docs:") {
			toDocs = append(toDocs, l.From)
		}
	}
	if len(toDocs) > 0 && !g.one(4) {
		start = g.pick(toDocs)
	}
	return start
}

// cruxArgs gives the crux query's arguments: the start, a depth from 1 to 4,
// a whole number for d.n to pass, a query vector for docs, and a LIMIT, now
// and then 0 or -1, which means no limit.
func (g *gen) cruxArgs(start string) []Value {
	q := g.vector("docs")
	limit := int64(1 + g.r.IntN(6))
	if g.one(10) {
		limit = []int64{0, -1}[g.r.IntN(2)]
	}
	return []Value{{start}, {int64(1 + g.r.IntN(4))}, {int64(g.r.IntN(20))}, {q}, {limit}}
}

// gen makes random sequences. It keeps a rough idea of what the database
// holds, so most steps are calls that can succeed, but it never looks at
// the database: the same seed always gives the same sequence.
type gen struct {
	r      *rand.Rand
	keys   []string
	links  []c.Link
	dims   map[string]int
	fields map[string]map[string]bool // the fields good Puts have given each table, in lower case
	leave  map[string]bool
}

// Kinds are the kinds of step, plus "where", which stands for Nearest's
// filters. Generate can leave any of them out, such as "sql" and "where"
// for an engine that has no SQL yet.
var Kinds = []string{"put", "get", "delete", "scan", "link", "unlink", "neighbours", "walk", "nearest", "drop", "update", "sql", "where"}

// Generate makes a sequence of n steps from seed, without the kinds named
// in leave.
func Generate(seed uint64, n int, leave ...string) []Op {
	g := newGen(seed, leave...)
	ops := make([]Op, n)
	for i := range ops {
		ops[i] = g.op(true)
	}
	return ops
}

// newGen makes the generator Generate uses for seed.
func newGen(seed uint64, leave ...string) *gen {
	g := &gen{r: rand.New(rand.NewPCG(seed, 0x6466)), dims: map[string]int{}, fields: map[string]map[string]bool{}, leave: map[string]bool{}}
	for _, k := range leave {
		g.leave[k] = true
	}
	return g
}

func (g *gen) pick(list []string) string { return list[g.r.IntN(len(list))] }

func (g *gen) one(in int) bool { return g.r.IntN(in) == 0 }

func (g *gen) table() string {
	if g.one(30) {
		return g.pick(badTables)
	}
	return g.pick(tables)
}

func (g *gen) key() string {
	switch {
	case g.one(40):
		return g.pick(badKeys)
	case len(g.keys) > 0 && g.r.IntN(10) < 6:
		return g.keys[g.r.IntN(len(g.keys))]
	}
	return g.pick(tables) + ":" + g.pick(ids)
}

// held is a key the sequence has put, most of the time.
func (g *gen) held() string {
	if len(g.keys) > 0 && !g.one(6) {
		return g.keys[g.r.IntN(len(g.keys))]
	}
	return g.key()
}

// recent is one of the last few keys put, most of the time, so links
// gather around a few records and walks have somewhere to go.
func (g *gen) recent() string {
	if n := len(g.keys); n > 1 && !g.one(4) {
		return g.keys[n-1-g.r.IntN(min(n, 6))]
	}
	return g.held()
}

// linked is a key with a link out of it, most of the time, for walks.
func (g *gen) linked() string {
	if len(g.links) > 0 && !g.one(4) {
		return g.links[g.r.IntN(len(g.links))].From
	}
	return g.key()
}

func (g *gen) linkType() string {
	if g.one(25) {
		return g.pick(badTypes)
	}
	return g.pick(linkTypes)
}

func (g *gen) maybeType() string {
	if g.one(2) {
		return ""
	}
	return g.pick(linkTypes)
}

// value is a field's value. A few of them are values Put refuses.
func (g *gen) value() any {
	switch g.r.IntN(12) {
	case 0, 1:
		return g.pick(texts)
	case 2:
		return "text " + strconv.Itoa(g.r.IntN(100))
	case 3, 4:
		return ints[g.r.IntN(len(ints))]
	case 5:
		return int64(g.r.IntN(20))
	case 6, 7:
		return reals[g.r.IntN(len(reals))]
	case 8:
		b := make([]byte, g.r.IntN(6))
		for i := range b {
			b[i] = byte(g.r.Uint32())
		}
		return b
	case 9:
		return g.one(2)
	case 10:
		if g.one(4) { // values Put refuses
			return []any{math.Inf(1), math.NaN(), "\xff"}[g.r.IntN(3)]
		}
		return nil
	}
	return nil
}

// vector makes a vector for a table: mostly of the size the generator
// thinks the table has, sometimes of another size, all zeros, or with NaN.
// A table whose size the generator doesn't know takes a vector of any
// size, since its first vector sets it.
func (g *gen) vector(tbl string) c.Vector {
	dims := g.dims[tbl]
	if dims == 0 {
		dims = 1 + g.r.IntN(4)
	}
	switch g.r.IntN(40) {
	case 0, 1:
		return g.floats(dims + 1)
	case 2:
		return make(c.Vector, dims)
	case 3:
		v := g.floats(dims)
		v[0] = float32(math.NaN())
		return v
	}
	return g.floats(dims)
}

// sound reports whether Put takes v as a vector, its size apart: its values
// are finite and not all zero, with -0 counting as zero.
func sound(v c.Vector) bool {
	zero := true
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
		zero = zero && x == 0
	}
	return len(v) > 0 && !zero
}

func (g *gen) floats(dims int) c.Vector {
	v := make(c.Vector, dims)
	for i := range v {
		if g.one(5) {
			v[i] = f32s[g.r.IntN(len(f32s))]
		} else {
			v[i] = float32(g.r.NormFloat64())
		}
	}
	if g.one(10) { // the same vector each time, for ties in Nearest
		v[0] = 1
		for i := 1; i < len(v); i++ {
			v[i] = 0.5
		}
	}
	allZero := true
	for _, x := range v {
		allZero = allZero && x == 0
	}
	if allZero {
		v[0] = 1
	}
	return v
}

// weights says how often each kind of step comes up, out of 100.
var weights = []struct {
	kind string
	upTo int
}{
	{"put", 26}, {"get", 32}, {"delete", 35}, {"scan", 40}, {"link", 60}, {"unlink", 64},
	{"neighbours", 68}, {"walk", 76}, {"nearest", 84}, {"drop", 85}, {"update", 93}, {"sql", 100},
}

func (g *gen) op(top bool) Op {
	for {
		w, kind := g.r.IntN(100), ""
		for _, k := range weights {
			if w < k.upTo {
				kind = k.kind
				break
			}
		}
		if g.leave[kind] || kind == "update" && !top {
			continue
		}
		switch kind {
		case "put":
			return g.put()
		case "get":
			return Op{Kind: "get", Key: g.key()}
		case "delete":
			key := g.key()
			g.forget(key)
			return Op{Kind: "delete", Key: key}
		case "scan":
			return g.scan()
		case "link":
			return g.link()
		case "unlink":
			return g.unlink()
		case "neighbours":
			return Op{Kind: "neighbours", Key: g.linked(), Dir: g.r.IntN(3), Type: g.maybeType()}
		case "walk":
			return g.walk()
		case "nearest":
			return g.nearest()
		case "drop":
			return g.drop()
		case "update":
			return g.update()
		}
		return g.sql(top)
	}
}

// put makes a Put. The generator remembers the key, and the table's vector
// size, only when it's a Put the rules allow, so that later steps mostly
// find what they look for, and every table's size it knows is the size the
// table has.
//
// A Put whose vector has another size than the table's, and nothing else
// wrong with it, names no other field. 0.x checks a vector's size once it
// has added the fields the table hasn't got, and keeps them when the Put
// fails inside an Update that goes on to commit, where the Beta keeps
// nothing, so SQL that names such a field would differ, as SQL.md says
// under "Where the Beta differs from 0.x".
func (g *gen) put() Op {
	key := g.key()
	op := Op{Kind: "put", Key: key}
	tbl, _, _ := strings.Cut(key, ":")
	good := true
	for _, k := range badKeys {
		good = good && k != key
	}
	for n := g.r.IntN(5); n > 0; n-- {
		name := g.pick(names)
		switch {
		case g.one(40):
			name = g.pick(badNames)
			good = false
		case g.one(30):
			name = "Title" // the same field as title, in another case
		}
		var v any
		if name == "vec" && tbl == "people" && !g.one(6) {
			name = "title" // vectors gather in docs and t_1, so searches find several
		}
		if name == "vec" {
			switch {
			case !g.one(8):
				v = g.vector(tbl)
			case g.one(6): // a vector written as JSON text, which Put reads as one of 2 values
				v = "[1, 2]"
			}
		} else {
			v = g.value()
		}
		op.Fields = append(op.Fields, Field{Name: name, Value: Value{v}})
	}
	// A field named twice in one Put keeps the last value, as a map does, so
	// the rules see only that one, but title and Title together break a
	// rule.
	spellings := map[string]string{}
	last := map[string]int{} // the last field of each name
	for i, f := range op.Fields {
		l := strings.ToLower(f.Name)
		if s, ok := spellings[l]; ok && s != f.Name {
			good = false
		}
		spellings[l] = f.Name
		last[f.Name] = i
	}
	size := 0 // the size of the vector the Put gives, or 0 for none
	for name, i := range last {
		switch v := op.Fields[i].Value.V; {
		case name != "vec":
			if f, isFloat := v.(float64); isFloat && (math.IsNaN(f) || math.IsInf(f, 0)) || v == "\xff" {
				good = false
			}
		case v == "[1, 2]":
			size = 2
		default:
			if vec, isVec := v.(c.Vector); isVec && sound(vec) {
				size = len(vec)
			} else if isVec {
				good = false
			}
		}
	}
	if size > 0 && g.dims[tbl] > 0 && size != g.dims[tbl] {
		op.Fields = []Field{op.Fields[last["vec"]]}
		good = false
	}
	if good {
		g.learn(key, op.Fields, size)
	}
	return op
}

// learn notes a Put the rules allow: its key, the fields it gives the table,
// and the table's vector size, when it gives the table its first vector, of
// size values.
func (g *gen) learn(key string, fields []Field, size int) {
	tbl, _, _ := strings.Cut(key, ":")
	g.keys = append(g.keys, key)
	if size > 0 && g.dims[tbl] == 0 {
		g.dims[tbl] = size
	}
	if g.fields[tbl] == nil {
		g.fields[tbl] = map[string]bool{}
	}
	for _, f := range fields {
		g.fields[tbl][strings.ToLower(f.Name)] = true
	}
}

func (g *gen) forget(key string) {
	kept := g.keys[:0]
	for _, k := range g.keys {
		if k != key {
			kept = append(kept, k)
		}
	}
	g.keys = kept
}

func (g *gen) scan() Op {
	prefix := g.table() + ":"
	if g.one(4) {
		prefix += g.pick([]string{"1", "2", "a", "x", "é"})
	}
	if g.one(25) {
		prefix = g.pick([]string{"nocolon", "Docs:", ""})
	}
	op := Op{Kind: "scan", Key: prefix}
	if g.one(3) {
		op.After = g.key()
	}
	if !g.one(2) {
		op.N = 1 + g.r.IntN(4)
	}
	if g.one(40) {
		op.N = -1
	}
	return op
}

func (g *gen) link() Op {
	if len(g.links) > 0 && g.one(3) { // the same two records, another type
		l := g.links[g.r.IntN(len(g.links))]
		return Op{Kind: "link", Key: l.From, To: l.To, Type: g.linkType()}
	}
	l := c.Link{From: g.recent(), Type: g.linkType(), To: g.recent()}
	if len(g.links) > 0 && g.one(3) { // chains, for walks of several links
		l.From = g.links[g.r.IntN(len(g.links))].To
	}
	g.links = append(g.links, l)
	return Op{Kind: "link", Key: l.From, To: l.To, Type: l.Type}
}

func (g *gen) unlink() Op {
	if len(g.links) > 0 && !g.one(4) {
		l := g.links[g.r.IntN(len(g.links))]
		if g.one(3) {
			l.Type = ""
		}
		return Op{Kind: "unlink", Key: l.From, To: l.To, Type: l.Type}
	}
	return Op{Kind: "unlink", Key: g.key(), To: g.key(), Type: g.maybeType()}
}

func (g *gen) walk() Op {
	depth := 1 + g.r.IntN(4)
	if g.one(20) {
		depth = []int{0, -1, c.MaxDepth + 1}[g.r.IntN(3)]
	}
	op := Op{Kind: "walk", Key: g.linked(), Dir: g.r.IntN(3), N: depth}
	if g.one(4) && len(g.links) > 0 { // a type some link has
		op.Type = g.links[g.r.IntN(len(g.links))].Type
	}
	return op
}

func (g *gen) nearest() Op {
	tbl := g.table()
	var withVectors []string
	for _, t := range tables {
		if g.dims[t] > 0 {
			withVectors = append(withVectors, t)
		}
	}
	if len(withVectors) > 0 && !g.one(5) {
		tbl = g.pick(withVectors)
	}
	op := Op{Kind: "nearest", Table: tbl, N: 2 + g.r.IntN(4)}
	if g.one(5) {
		op.N = 1
	}
	q := g.vector(tbl)
	op.Vec = &Value{q}
	if g.one(20) {
		op.N = []int{0, -1, 10001}[g.r.IntN(3)]
	}
	if g.leave["where"] {
		return op
	}
	switch g.r.IntN(6) {
	case 0:
		op.Where, op.Args = "n > ?", []Value{{int64(g.r.IntN(20))}}
	case 1:
		op.Where, op.Args = "title = ?", []Value{{g.pick(texts)}}
	case 2:
		op.Where = "n IS NOT NULL"
	}
	return op
}

func (g *gen) drop() Op {
	tbl := g.table()
	g.dims[tbl] = 0
	delete(g.fields, tbl)
	kept := g.keys[:0]
	for _, k := range g.keys {
		if !strings.HasPrefix(k, tbl+":") {
			kept = append(kept, k)
		}
	}
	g.keys = kept
	return Op{Kind: "drop", Table: tbl}
}

func (g *gen) update() Op {
	op := Op{Kind: "update", Fail: g.one(3)}
	back := g.remember()
	for n := 1 + g.r.IntN(5); n > 0; n-- {
		op.Ops = append(op.Ops, g.op(false))
	}
	if op.Fail { // what it did is undone, so forget it
		back()
	}
	return op
}

// remember returns a func that puts back what the generator knows now, for
// an Update that rolls back.
func (g *gen) remember() func() {
	keys, links := append([]string{}, g.keys...), append([]c.Link{}, g.links...)
	dims := maps.Clone(g.dims)
	fields := map[string]map[string]bool{}
	for t, f := range g.fields {
		fields[t] = maps.Clone(f)
	}
	return func() { g.keys, g.links, g.dims, g.fields = keys, links, dims, fields }
}

// cruxUpdate makes an Update that gives the crux query something to find
// and runs it there, through the transaction: puts of one to three records
// of docs, each with a title, an n and a vector of docs's size, links to
// them from a record the sequence has put, most of the time, and the query
// from that record. A third of them roll back, as other Updates do.
func (g *gen) cruxUpdate() Op {
	op := Op{Kind: "update", Fail: g.one(3)}
	back := g.remember()
	from := g.held()
	size := g.dims["docs"]
	if size == 0 {
		size = 1 + g.r.IntN(4)
	}
	for n := 1 + g.r.IntN(3); n > 0; n-- {
		put := Op{Kind: "put", Key: "docs:" + g.pick(ids), Fields: []Field{
			{Name: "title", Value: Value{g.pick(texts)}},
			{Name: "n", Value: Value{int64(g.r.IntN(30))}},
			{Name: "vec", Value: Value{g.floats(size)}},
		}}
		g.learn(put.Key, put.Fields, size)
		link := Op{Kind: "link", Key: from, To: put.Key, Type: g.pick(linkTypes)}
		g.links = append(g.links, c.Link{From: link.Key, Type: link.Type, To: link.To})
		op.Ops = append(op.Ops, put, link)
	}
	op.Ops = append(op.Ops, Op{Kind: "sql", SQL: cruxSQL, Args: g.cruxArgs(from)})
	if op.Fail {
		back()
	}
	return op
}

// cruxReady reports whether docs has what the crux query reads, as far as
// the generator knows: a vector size, and the fields title and n.
func (g *gen) cruxReady() bool {
	f := g.fields["docs"]
	return g.dims["docs"] > 0 && f["title"] && f["n"]
}

// sql picks one of statements. The crux query, the last of them, comes half
// the time as an Update of its own that gives it something to find
// (cruxUpdate), when it isn't a step inside another Update. Otherwise it
// comes once docs has a vector size and the fields title and n, as far as
// the generator knows, and one time in ten when it hasn't, so that it meets
// the errors of a table or field that isn't there too.
func (g *gen) sql(top bool) Op {
	s := statements[g.r.IntN(len(statements))]
	if s.sql == cruxSQL {
		switch {
		case top && !g.leave["update"] && g.one(2):
			return g.cruxUpdate()
		case !g.cruxReady() && !g.one(10):
			s = statements[g.r.IntN(len(statements)-1)]
		}
	}
	op := Op{Kind: "sql", SQL: s.sql}
	if s.args != nil {
		op.Args = s.args(g)
	}
	return op
}
