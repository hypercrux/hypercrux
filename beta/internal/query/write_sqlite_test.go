// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// Writes against 0.x through zerox: the write cases, each as 0.x gives it,
// and random writes with the reads after each, a run of them inside one
// transaction in each engine. These tests need cgo, as 0.x does.

// TestTheWriteCasesAre0xs holds writeCases to 0.x's answers on the
// fixture: the same count or error kind, and the same rows after, apart
// from the cases where the Beta differs, where 0.x gives zeroWant and
// zeroRows.
func TestTheWriteCasesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	differ := 0
	for _, wc := range writeCases {
		ans := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: wc.sql, Args: zeroArgs(wc.args), After: wc.after}, false)
		got, rows := writeResult(ans), ""
		if ans.After != nil {
			rows = answerText(ans.After)
		}
		want, wantRows := wc.want, wc.rows
		if wc.zeroWant != "" || wc.zeroRows != "" {
			differ++
			if wc.zeroWant != "" {
				want = wc.zeroWant
			}
			if wc.zeroRows != "" {
				wantRows = wc.zeroRows
			}
		}
		if got != want || rows != wantRows {
			t.Errorf("%s %v\n  0.x gives %s, then %s\n  the case %s, then %s", wc.sql, wc.args, got, strings.ReplaceAll(rows, "\n", " / "),
				want, strings.ReplaceAll(wantRows, "\n", " / "))
		}
	}
	t.Logf("%d write cases held to 0.x, %d of them where the Beta differs", len(writeCases), differ)
}

// zeroRead runs a query through a 0.x handle and gives its answer as the
// corpus has it, as sqlcorpus.Run reads one.
func zeroRead(e c.Engine, h c.Handle, q string, args []any) *sqlcorpus.Answer {
	failed := func(err error) *sqlcorpus.Answer {
		return &sqlcorpus.Answer{Error: difftest.Kind(e, err), Message: err.Error()}
	}
	rows, err := h.Query(q, args...)
	if err != nil {
		return failed(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return failed(err)
	}
	ans := &sqlcorpus.Answer{Columns: cols}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return failed(err)
		}
		row := make([]difftest.Value, len(vals))
		for i, v := range vals {
			row[i] = difftest.Value{V: v}
		}
		ans.Rows = append(ans.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	return ans
}

// zeroExec runs a write through a 0.x handle and gives its count of
// changed rows, or its error's kind.
func zeroExec(e c.Engine, h c.Handle, q string, args []any) *sqlcorpus.Answer {
	res, err := h.Exec(q, args...)
	if err != nil {
		return &sqlcorpus.Answer{Error: difftest.Kind(e, err), Message: err.Error()}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return &sqlcorpus.Answer{Error: difftest.Kind(e, err), Message: err.Error()}
	}
	return &sqlcorpus.Answer{Changed: &n}
}

// writeShape is a table of the random writes' data, with its fields, each
// of a sort: text, a number, bytes or the vector.
type writeShape struct {
	name   string
	fields []string
	sorts  []string
}

var writeShapes = []writeShape{
	{"docs", []string{"title", "status", "n", "score", "vec", "data"}, []string{"text", "text", "number", "number", "vec", "bytes"}},
	{"people", []string{"name", "age"}, []string{"text", "number"}},
	{"notes", []string{"n", "vec"}, []string{"number", "vec"}},
}

// number is the first field of the sort number.
func (t writeShape) number() string { return t.fields[slices.Index(t.sorts, "number")] }

// writeGen makes random writes over writeShapes. It keeps clear of what
// SQL.md has the Beta do apart from 0.x: values Put refuses and 0.x stores,
// such as infinite reals, text that isn't valid UTF-8 and vectors of -0
// alone, so bytes and vectors never reach a field of text or a number; a
// one-record subquery in an UPDATE's SET that reads a field the SET
// changes, which 0.x reads as changed; and orders of work 0.x's planner may
// choose, so a WHERE term that can fail stands alone or beside a term on
// the key that names the records. New keys come in key order after every
// key there is, so 0.x's rowids are in key order and its scans read the
// records in key order too.
type writeGen struct {
	r    *rand.Rand
	next int
	keys map[string][]string // each table's keys, in order
	args []any
}

// arg adds an argument, and gives its mark.
func (g *writeGen) arg(v any) string {
	g.args = append(g.args, v)
	return "?"
}

func (g *writeGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

// key gives a key of the table that's there, or now and then one that
// isn't.
func (g *writeGen) key(table string) string {
	ks := g.keys[table]
	if len(ks) == 0 || g.r.IntN(8) == 0 {
		return fmt.Sprintf("%s:%04d", table, 9000+g.r.IntN(10))
	}
	return ks[g.r.IntN(len(ks))]
}

// newKey gives a new key of the table, after every key there is.
func (g *writeGen) newKey(table string) string {
	g.next++
	return fmt.Sprintf("%s:%04d", table, 1000+g.next)
}

// vector gives a vector of three values mostly, now and then of two, or of
// zeros.
func (g *writeGen) vector() c.Vector {
	n := 3
	if g.r.IntN(6) == 0 {
		n = 2
	}
	v := make(c.Vector, n)
	if g.r.IntN(15) != 0 {
		for i := range v {
			v[i] = float32(g.r.IntN(9) - 4)
		}
		if v[0] == 0 {
			v[0] = 1
		}
	}
	return v
}

// literal gives a value of a sort that reads no field.
func (g *writeGen) literal(sort string) string {
	switch sort {
	case "text":
		switch g.r.IntN(8) {
		case 0:
			return "NULL"
		case 1:
			return g.arg(g.pick("x", "open", "OPEN", "é"))
		}
		return g.pick("'open'", "'done'", "'Q3 plan'", "'é'", "''", "'12'")
	case "number":
		switch g.r.IntN(8) {
		case 0:
			return "NULL"
		case 1:
			return g.arg(int64(g.r.IntN(20) - 5))
		case 2:
			return g.arg(float64(g.r.IntN(9)) / 4)
		}
		return g.pick("0", "1", "-7", "42", "2.5", "'5'")
	case "bytes":
		if g.r.IntN(6) == 0 {
			return g.arg([]byte{1, 2, 3, 4})
		}
		return g.pick("x''", "x'000102'", "x'0000803f0000803f0000803f'", "x'000000000000000000000000'", "NULL")
	}
	switch g.r.IntN(10) {
	case 0:
		return "NULL"
	case 1:
		return "vector('[1, 2, 3]')"
	case 2:
		return "x'0000803f0000803f0000803f'"
	case 3:
		return "'[1, 2, 3]'"
	case 4:
		return "x'0000803f0000803f'"
	case 5:
		return "vector(" + g.arg("[0, 2, 1]") + ")"
	}
	return g.arg(g.vector())
}

// failing gives an expression that fails on some rows or every row: vector()
// on text that isn't a JSON array, abs() of the smallest integer, a walk
// that's refused, or distance() between vectors of two sizes. Outside an
// INSERT it reads the record's fields, so it fails for some records, and
// where it doesn't it gives a number or text.
func (g *writeGen) failing(t writeShape, insert bool) string {
	if insert {
		switch g.r.IntN(5) {
		case 0:
			return "abs(-9223372036854775807 - " + g.arg(int64(g.r.IntN(2))) + ")"
		}
		return g.pick("vector('x')", "abs(-9223372036854775808)", "walk('people:0001', 0)", "distance(x'0000803f', x'0000803f0000803f')")
	}
	n := t.number()
	switch g.r.IntN(4) {
	case 0:
		return "abs(-9223372036854775808 + (" + n + " < " + g.arg(int64(g.r.IntN(12)-2)) + "))"
	case 1:
		return "length(vector(coalesce(" + n + ", '[1, 2, 3]')))"
	case 2:
		return "walk(key, coalesce(" + n + ", 1))"
	}
	return "abs(-9223372036854775807 - (key = " + g.arg(g.key(t.name)) + "))"
}

// value gives a value for a field of the given sort. In an UPDATE it may
// read the record's fields, and its one-record subquery reads a field that
// the UPDATE doesn't set, or another table's.
func (g *writeGen) value(t writeShape, sort string, insert bool, set []string) string {
	if insert && g.r.IntN(20) == 0 || !insert && g.r.IntN(8) == 0 {
		return g.failing(t, insert)
	}
	if g.r.IntN(8) == 0 {
		var choices [][2]string
		for _, o := range writeShapes {
			for i, f := range o.fields {
				if o.sorts[i] == sort && (o.name != t.name || insert || !slices.Contains(set, f)) {
					choices = append(choices, [2]string{f, o.name})
				}
			}
		}
		if len(choices) > 0 {
			ch := choices[g.r.IntN(len(choices))]
			return "(SELECT " + ch[0] + " FROM " + ch[1] + " WHERE key = " + g.arg(g.key(ch[1])) + ")"
		}
	}
	if insert || g.r.IntN(3) == 0 {
		return g.literal(sort)
	}
	own := ""
	for i, f := range t.fields {
		if t.sorts[i] == sort && (own == "" || g.r.IntN(2) == 0) {
			own = f
		}
	}
	if own == "" {
		return g.literal(sort)
	}
	switch sort {
	case "text":
		return g.pick(own, "upper("+own+")", own+" || '!'", "substr("+own+", 1, 2)", "coalesce("+own+", 'none')", "walk(key, 1)")
	case "number":
		return g.pick(own, own+" + 1", own+" * 2", "-"+own, "coalesce("+own+", 0) - 3", own+" / 2", "length(key)")
	case "bytes":
		return g.pick(own, "x'0a0b'")
	}
	if t.name == "docs" && g.r.IntN(3) == 0 {
		return "data"
	}
	return g.pick(own, "vector('[0, 1, 0]')")
}

// where gives a WHERE, or none.
func (g *writeGen) where(t writeShape) string {
	n := t.number()
	safe := func() string {
		switch g.r.IntN(10) {
		case 0:
			return n + " > " + g.arg(int64(g.r.IntN(12)-2))
		case 1:
			return n + " IS NULL"
		case 2:
			return "key >= " + g.arg(g.key(t.name))
		case 3:
			return "key IN (SELECT value FROM json_each(walk(" + g.arg(g.key(g.pick("people", "docs"))) + ", 2, NULL, 'both')))"
		case 4:
			return "(SELECT age FROM people WHERE key = " + g.arg(g.key("people")) + ") IS NOT NULL"
		case 5:
			return "walk(key, 1) <> '[]'"
		case 6:
			return n + " BETWEEN 0 AND 10"
		case 7:
			return "key <> " + g.arg(g.key(t.name))
		}
		return n + " % 2 = 0"
	}
	lookup := func() string {
		if g.r.IntN(2) == 0 {
			return "key = " + g.arg(g.key(t.name))
		}
		k1 := g.arg(g.key(t.name))
		k2 := g.arg(g.key(t.name))
		return "key IN (" + k1 + ", " + k2 + ", " + g.arg(g.key(t.name)) + ")"
	}
	switch g.r.IntN(8) {
	case 0:
		return ""
	case 1:
		return " WHERE " + g.failing(t, false)
	case 2:
		l := lookup()
		return " WHERE " + l + " AND " + g.failing(t, false)
	case 3:
		return " WHERE " + lookup()
	case 4:
		a := safe()
		return " WHERE " + a + " AND " + safe()
	}
	return " WHERE " + safe()
}

// statement gives a random INSERT, UPDATE or DELETE and its arguments, in
// the order of their marks. A table that's grown small gets INSERTs, so
// every table keeps records to update and delete.
func (g *writeGen) statement() (string, []any) {
	g.args = nil
	t := writeShapes[g.r.IntN(len(writeShapes))]
	if len(g.keys[t.name]) < 8 && g.r.IntN(3) != 0 {
		return g.insert(t), g.args
	}
	switch g.r.IntN(8) {
	case 0, 1, 2:
		return g.insert(t), g.args
	case 3, 4, 5, 6:
		return g.update(t), g.args
	}
	if g.r.IntN(3) != 0 {
		// Most DELETEs name their records.
		if g.r.IntN(2) == 0 {
			return "DELETE FROM " + t.name + " WHERE key = " + g.arg(g.key(t.name)), g.args
		}
		k1 := g.arg(g.key(t.name))
		return "DELETE FROM " + t.name + " WHERE key IN (" + k1 + ", " + g.arg(g.key(t.name)) + ")", g.args
	}
	return "DELETE FROM " + t.name + g.where(t), g.args
}

// insert gives an INSERT of key, mostly, and some of the table's fields,
// in any order, in one to three rows.
func (g *writeGen) insert(t writeShape) string {
	type col struct{ name, sort string }
	var cols []col
	if g.r.IntN(25) != 0 {
		cols = append(cols, col{"key", "key"})
	}
	for i, f := range t.fields {
		if g.r.IntN(2) == 0 {
			cols = append(cols, col{f, t.sorts[i]})
		}
	}
	if len(cols) == 0 {
		cols = append(cols, col{t.fields[0], t.sorts[0]})
	}
	g.r.Shuffle(len(cols), func(i, j int) { cols[i], cols[j] = cols[j], cols[i] })
	names := make([]string, len(cols))
	for i, cl := range cols {
		names[i] = cl.name
	}
	var rows []string
	var given []string
	for n := 1 + g.r.IntN(3); n > 0; n-- {
		vals := make([]string, len(cols))
		for i, cl := range cols {
			if cl.name != "key" {
				vals[i] = g.value(t, cl.sort, true, nil)
				continue
			}
			// A key that goes in is a new one, after every key there is.
			// The others are refused.
			switch g.r.IntN(30) {
			case 0:
				if ks := g.keys[t.name]; len(ks) > 0 {
					vals[i] = "'" + ks[g.r.IntN(len(ks))] + "'"
				} else {
					vals[i] = "NULL"
				}
			case 1:
				if len(given) > 0 {
					vals[i] = g.arg(given[0])
				} else {
					vals[i] = "NULL"
				}
			case 2:
				other := writeShapes[g.r.IntN(len(writeShapes))].name
				if other == t.name {
					other = "zz"
				}
				vals[i] = "'" + g.key(other) + "'"
			case 3:
				vals[i] = g.pick("NULL", "7", "'"+t.name+":'", "x'00'")
			case 4:
				vals[i] = g.failing(t, true)
			default:
				k := g.newKey(t.name)
				given = append(given, k)
				vals[i] = g.arg(k)
			}
		}
		rows = append(rows, "("+strings.Join(vals, ", ")+")")
	}
	return "INSERT INTO " + t.name + " (" + strings.Join(names, ", ") + ") VALUES " + strings.Join(rows, ", ")
}

// update gives an UPDATE of one to three fields, key and vec among them
// now and then.
func (g *writeGen) update(t writeShape) string {
	var set, sorts []string
	for i, f := range t.fields {
		if g.r.IntN(3) == 0 {
			set, sorts = append(set, f), append(sorts, t.sorts[i])
		}
	}
	if g.r.IntN(10) == 0 {
		set, sorts = append(set, "key"), append(sorts, "key")
	}
	if len(set) == 0 {
		i := g.r.IntN(len(t.fields))
		set, sorts = append(set, t.fields[i]), append(sorts, t.sorts[i])
	}
	var parts []string
	for _, i := range g.r.Perm(len(set)) {
		v := ""
		if set[i] == "key" {
			if g.r.IntN(5) == 0 {
				v = g.arg(g.key(t.name))
			} else {
				v = g.pick("key", "key", "upper(key)", "NULL")
			}
		} else {
			v = g.value(t, sorts[i], false, set)
		}
		parts = append(parts, set[i]+" = "+v)
	}
	return "UPDATE " + t.name + " SET " + strings.Join(parts, ", ") + g.where(t)
}

// writeData is the data the random writes start from, put into 0.x and a
// store in key order, so 0.x's rowids follow key order.
func writeData(r *rand.Rand) (puts []randomRecord, links []c.Link) {
	titles := []any{"Q3 plan", "notes", "é accents", "", "12", nil}
	for i := 0; i < 24; i++ {
		f := c.Fields{"title": titles[r.IntN(len(titles))], "n": int64(r.IntN(14) - 3)}
		if r.IntN(3) != 0 {
			f["status"] = []any{"open", "done", "OPEN"}[r.IntN(3)]
		}
		if r.IntN(3) != 0 {
			f["score"] = float64(r.IntN(40)) / 8
		}
		if r.IntN(5) == 0 {
			f["n"] = "5"
		}
		if r.IntN(4) != 0 {
			f["vec"] = c.Vector{float32(1 + r.IntN(3)), float32(r.IntN(3)), float32(r.IntN(3) - 1)}
		}
		if r.IntN(4) == 0 {
			f["data"] = []any{[]byte{0, 1, 2}, c.Vector{1, 2, 3}.Bytes(), []byte{}}[r.IntN(3)]
		}
		puts = append(puts, randomRecord{fmt.Sprintf("docs:%04d", i), f})
	}
	for i := 0; i < 8; i++ {
		f := c.Fields{"name": fmt.Sprintf("person %d", i)}
		if r.IntN(4) != 0 {
			f["age"] = int64(20 + r.IntN(50))
		}
		puts = append(puts, randomRecord{fmt.Sprintf("people:%04d", i), f})
	}
	for i := 0; i < 5; i++ {
		puts = append(puts, randomRecord{fmt.Sprintf("notes:%04d", i), c.Fields{"n": int64(i), "vec": nil}})
	}
	for i := 0; i < 8; i++ {
		for j := r.IntN(4); j > 0; j-- {
			links = append(links, c.Link{From: fmt.Sprintf("people:%04d", i), Type: "owns", To: fmt.Sprintf("docs:%04d", r.IntN(24))})
		}
	}
	for i := 0; i < 24; i++ {
		if r.IntN(2) == 0 {
			links = append(links, c.Link{From: fmt.Sprintf("docs:%04d", i), Type: "cites", To: fmt.Sprintf("docs:%04d", r.IntN(24))})
		}
	}
	slices.SortFunc(links, func(a, b c.Link) int { return strings.Compare(a.From+" "+a.To, b.From+" "+b.To) })
	links = slices.CompactFunc(links, func(a, b c.Link) bool { return a == b })
	return puts, links
}

// writeReads are the reads after each write: every table whole, and the
// links that walks follow.
var writeReads = []string{
	"SELECT * FROM docs ORDER BY key",
	"SELECT * FROM people ORDER BY key",
	"SELECT * FROM notes ORDER BY key",
	"SELECT key, walk(key, 1, NULL, 'both') FROM people ORDER BY key",
}

// TestRandomWritesAs0x runs random writes, each followed by writeReads,
// through 0.x and through Exec on a store, a run of them inside one
// transaction in each engine, and every answer must be the same: the count
// or the error's kind of each write, and the rows of each read. A failed
// write leaves each transaction as it was, so the runs go on side by side.
func TestRandomWritesAs0x(t *testing.T) {
	runs, writes := 4, 400
	if testing.Short() {
		runs, writes = 2, 150
	}
	counts := map[string]int{}
	for run := 0; run < runs; run++ {
		r := rand.New(rand.NewPCG(uint64(run), 0x5136))
		puts, links := writeData(r)
		e := zerox.Engine{}
		db, err := e.Open(filepath.Join(t.TempDir(), "db"))
		if err != nil {
			t.Fatal(err)
		}
		s := store.New()
		err = db.Update(func(tx c.Handle) error {
			for _, rec := range puts {
				if err := tx.Put(rec.key, rec.fields); err != nil {
					return err
				}
				fields, err := storeFields(rec.fields)
				if err != nil {
					return err
				}
				if _, err := s.Put(nil, rec.key, fields); err != nil {
					return err
				}
			}
			for _, l := range links {
				if err := tx.Link(l.From, l.Type, l.To); err != nil {
					return err
				}
				if _, err := s.Link(nil, l.From, l.Type, l.To); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		bad := 0
		btx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		g := &writeGen{r: r, keys: map[string][]string{}}
		err = db.Update(func(ztx c.Handle) error {
			for i := 0; i < writes && bad < 10; i++ {
				for _, sh := range writeShapes {
					g.keys[sh.name] = tableKeys(t, btx, sh.name)
				}
				q, args := g.statement()
				if _, err := Parse(q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				want := zeroExec(e, ztx, q, args)
				got := execWrite(btx, q, argValuesOf(args), corpusNow)
				switch {
				case want.Error != "":
					counts[want.Error]++
				case *want.Changed == 0:
					counts["no rows"]++
				default:
					counts[strings.Fields(q)[0]]++
				}
				if !sqlcorpus.Same(want, got, false) {
					bad++
					t.Errorf("run %d, write %d: %s %v\n  0.x  %s\n  Beta %s", run, i, q, args, writeAnswerText(want), writeAnswerText(got))
				}
				for _, rq := range writeReads {
					want, got := zeroRead(e, ztx, rq, nil), planQuery(btx, rq, nil, corpusNow)
					if !sqlcorpus.Same(want, got, false) {
						bad++
						t.Errorf("run %d, after write %d: %s %v\n  then %s\n  0.x  %s\n  Beta %s", run, i, q, args, rq, writeAnswerText(want), writeAnswerText(got))
						break
					}
				}
			}
			return nil
		})
		btx.Rollback()
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%v", counts)
}

// tableKeys gives a table's keys in order.
func tableKeys(t *testing.T, r store.Reader, table string) []string {
	t.Helper()
	cur, err := r.Scan(table+":", "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for rec, ok := cur.Next(); ok; rec, ok = cur.Next() {
		keys = append(keys, rec.Key)
	}
	return keys
}

// argValuesOf makes Go arguments values, as Arg does.
func argValuesOf(args []any) []value.Value {
	vals := make([]value.Value, len(args))
	for i, a := range args {
		vals[i] = argValue(a)
	}
	return vals
}
