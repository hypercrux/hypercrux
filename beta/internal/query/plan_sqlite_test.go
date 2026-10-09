// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The planner against 0.x through zerox: the plan-shape cases, the crux
// query on fixed random data, which is Q5's closing test, and random
// statements of the planner's shapes. These tests need cgo, as 0.x does.

// zeroArgs makes Go arguments the corpus's, for sqlcorpus.Run.
func zeroArgs(args []any) []difftest.Value {
	vals := make([]difftest.Value, len(args))
	for i, a := range args {
		vals[i] = difftest.Value{V: a}
	}
	return vals
}

// TestThePlanCasesAre0xs holds planCases to 0.x's answers, on the fixture
// with the empty table, each as 0.x runs it.
func TestThePlanCasesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	err := db.Update(func(tx c.Handle) error {
		if err := tx.Put("empty:1", c.Fields{"n": int64(1), "vec": c.Vector{1, 0}}); err != nil {
			return err
		}
		return tx.Delete("empty:1")
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cs := range planCases {
		q := cs.sql
		switch cs.zero {
		case "-":
			continue
		case "":
		default:
			q = cs.zero
		}
		n++
		got := answerText(sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q, Args: zeroArgs(cs.args)}, false))
		if got != cs.want {
			t.Errorf("%s:\n  0.x gives %s\n  the table %s", q, strings.ReplaceAll(got, "\n", " / "), strings.ReplaceAll(cs.want, "\n", " / "))
		}
	}
	t.Logf("%d of %d cases held to 0.x", n, len(planCases))
}

// randomData is a database made at random from a seed, as 0.x and the
// Beta hold it: documents with a title, a status, a number and, mostly, a
// vector, some of them copies or doubles of another's, so distances tie;
// customers; and links of three types between them.
type randomData struct {
	dims  int
	docs  []string // their keys, in key order
	custs []string
	vecs  map[string][]float32
	put   []randomRecord
	links []c.Link
}

type randomRecord struct {
	key    string
	fields c.Fields
}

var statuses = []any{"open", "open", "open", "done", "archived", nil}

func newRandomData(r *rand.Rand, dims, ndocs, ncusts int) *randomData {
	d := &randomData{dims: dims, vecs: map[string][]float32{}}
	for i := 0; i < ndocs; i++ {
		key := fmt.Sprintf("docs:%04d", i)
		f := c.Fields{"title": fmt.Sprintf("doc %d", i), "status": statuses[r.IntN(len(statuses))], "n": int64(r.IntN(50) - 10)}
		if r.IntN(8) != 0 {
			v := make([]float32, dims)
			switch {
			case len(d.docs) > 0 && r.IntN(12) == 0:
				copy(v, d.vecs[d.docs[r.IntN(len(d.docs))]])
				if v[0] == 0 && v[1] == 0 {
					v[0] = 1 // the copy of a vector-less document
				}
			case len(d.docs) > 0 && r.IntN(12) == 0:
				for j, x := range d.vecs[d.docs[r.IntN(len(d.docs))]] {
					v[j] = 2 * x
				}
			}
			if slices.Equal(v, make([]float32, dims)) {
				for j := range v {
					v[j] = float32(r.NormFloat64())
				}
			}
			f["vec"] = c.Vector(v)
			d.vecs[key] = v
		}
		d.docs = append(d.docs, key)
		d.put = append(d.put, randomRecord{key, f})
	}
	for i := 0; i < ncusts; i++ {
		key := fmt.Sprintf("customer:%02d", i)
		d.custs = append(d.custs, key)
		d.put = append(d.put, randomRecord{key, c.Fields{"name": fmt.Sprintf("customer %d", i)}})
	}
	seen := map[c.Link]bool{}
	link := func(l c.Link) {
		if !seen[l] {
			seen[l] = true
			d.links = append(d.links, l)
		}
	}
	for _, cu := range d.custs {
		for j := r.IntN(8); j > 0; j-- {
			link(c.Link{From: cu, Type: "owns", To: d.docs[r.IntN(len(d.docs))]})
		}
		if r.IntN(3) == 0 {
			link(c.Link{From: cu, Type: "knows", To: d.custs[r.IntN(len(d.custs))]})
		}
	}
	for _, doc := range d.docs {
		for j := r.IntN(4); j > 0; j-- {
			link(c.Link{From: doc, Type: "cites", To: d.docs[r.IntN(len(d.docs))]})
		}
	}
	return d
}

// open puts the data into 0.x and into a store, records in key order, so
// 0.x reads its tables in key order too.
func (d *randomData) open(t *testing.T) (zerox.Engine, c.DB, *store.Store) {
	t.Helper()
	e := zerox.Engine{}
	db, err := e.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	s := store.New()
	err = db.Update(func(tx c.Handle) error {
		for _, rec := range d.put {
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
		for _, l := range d.links {
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
	return e, db, s
}

// distanceTo is the cosine distance from a document's vector to q, worked
// out plainly in float64, as a reference for ties.
func (d *randomData) distanceTo(key string, q []float32) float64 {
	v, ok := d.vecs[key]
	if !ok || len(v) != len(q) {
		return math.NaN()
	}
	var dot, na, nb float64
	for i := range v {
		dot += float64(v[i]) * float64(q[i])
		na += float64(v[i]) * float64(v[i])
		nb += float64(q[i]) * float64(q[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return min(max(1-dot/(math.Sqrt(na)*math.Sqrt(nb)), 0), 2)
}

// sameUpToTies reports whether two answers to a query ordered by distance
// alone agree: the same error, or the same rows, apart from the order of
// rows whose distances fall within conformance.DistanceBound of each other,
// whose order 0.x leaves unsettled. Where such a run of rows is cut short
// by the LIMIT, the two may keep different rows of it. key is the column
// that holds a document's key.
func sameUpToTies(want, got *sqlcorpus.Answer, dist func(key string) float64, key int, limit int) (exact, ok bool) {
	if sqlcorpus.Same(want, got, false) {
		return true, true
	}
	if want.Error != "" || got.Error != "" || len(want.Rows) != len(got.Rows) || !slices.Equal(want.Columns, got.Columns) {
		return false, false
	}
	keyOf := func(row []difftest.Value) string { s, _ := row[key].V.(string); return s }
	rowText := func(row []difftest.Value) string { return fmt.Sprint(row) }
	for i := 0; i < len(want.Rows); {
		j := i + 1
		for j < len(want.Rows) && math.Abs(dist(keyOf(want.Rows[j]))-dist(keyOf(want.Rows[j-1]))) <= c.DistanceBound {
			j++
		}
		for k := i; k < j; k++ {
			if math.Abs(dist(keyOf(want.Rows[k]))-dist(keyOf(got.Rows[k]))) > c.DistanceBound {
				return false, false
			}
		}
		if j < len(want.Rows) || len(want.Rows) < limit {
			w, g := make([]string, 0, j-i), make([]string, 0, j-i)
			for k := i; k < j; k++ {
				w, g = append(w, rowText(want.Rows[k])), append(g, rowText(got.Rows[k]))
			}
			slices.Sort(w)
			slices.Sort(g)
			if !slices.Equal(w, g) {
				return false, false
			}
		}
		i = j
	}
	return false, true
}

// cruxForms are the crux query as the README has it, with its walk, its
// filter, its query vector and its LIMIT as ? marks, and the same through
// the Beta's walk as a table and with the table first, each with the form
// 0.x runs.
var cruxForms = []struct{ beta, zero string }{
	{"SELECT d.key, d.title FROM json_each(walk(?, ?)) w JOIN docs d ON d.key = w.value WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?", ""},
	{"SELECT d.key, d.title FROM walk(?, ?) w JOIN docs d ON d.key = w.key WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?",
		"SELECT d.key, d.title FROM json_each(walk(?, ?)) w JOIN docs d ON d.key = w.value WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?"},
	{"SELECT d.key, d.title FROM docs d JOIN json_each(walk(?, ?)) w ON w.value = d.key WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?", ""},
}

// readmeCrux is the crux query as the README writes it, with the walk's
// start and depth put in as literals.
const readmeCrux = `
SELECT d.key, d.title
FROM json_each(walk('%s', %d)) w
JOIN docs d ON d.key = w.value
WHERE d.status = 'open' AND d.vec IS NOT NULL
ORDER BY distance(d.vec, ?)
LIMIT 10`

// cruxQuestion makes the query vector: random, a document's own, or
// another document's doubled, given as a Vector, which binds as bytes, or
// as JSON text, and now and then one of another size.
func cruxQuestion(r *rand.Rand, d *randomData) ([]float32, any) {
	q := make([]float32, d.dims)
	if k := d.docs[r.IntN(len(d.docs))]; r.IntN(4) == 0 && d.vecs[k] != nil {
		copy(q, d.vecs[k])
	} else {
		for i := range q {
			q[i] = float32(r.NormFloat64())
		}
	}
	if r.IntN(40) == 0 {
		q = append(q, 1)
	}
	if r.IntN(3) == 0 {
		parts := make([]string, len(q))
		for i, x := range q {
			parts[i] = fmt.Sprint(x)
		}
		return q, "[" + strings.Join(parts, ", ") + "]"
	}
	return q, c.Vector(q)
}

// TestTheCruxQueryMatches0x is Q5's closing test: the crux query, the
// README's one statement across all four handles, on fixed random data in
// 0.x and in a store, with random walks, filters, query vectors and
// limits, gives 0.x's answer, rows in the same order apart from ties in
// distance, errors by kind.
func TestTheCruxQueryMatches0x(t *testing.T) {
	queries := 1500
	if testing.Short() {
		queries = 300
	}
	cruxAs0x(t, 5, queries)
}

// cruxAs0x runs the crux query queries times on each of two databases
// made from seed.
func cruxAs0x(t *testing.T, seed uint64, queries int) {
	r := rand.New(rand.NewPCG(seed, 0x5135))
	counts := map[string]int{}
	for _, dims := range []int{3, 16} {
		d := newRandomData(r, dims, 400, 40)
		e, db, s := d.open(t)
		bad := 0
		for i := 0; i < queries; i++ {
			form := cruxForms[r.IntN(len(cruxForms))]
			start := d.custs[r.IntN(len(d.custs))]
			switch r.IntN(20) {
			case 0:
				start = "customer:99"
			case 1:
				start = d.docs[r.IntN(len(d.docs))]
			}
			depth := 1 + r.IntN(4)
			if r.IntN(50) == 0 {
				depth = 0
			}
			status := statuses[r.IntN(len(statuses)-1)]
			q, qArg := cruxQuestion(r, d)
			limit := 1 + r.IntN(20)
			switch r.IntN(20) {
			case 0:
				limit = 0
			case 1:
				limit = -1
			}
			args := []any{start, int64(depth), status, qArg, int64(limit)}
			zero := form.zero
			if zero == "" {
				zero = form.beta
			}
			if r.IntN(4) == 0 {
				// The README's own text, which has the rest as literals.
				form.beta = fmt.Sprintf(readmeCrux, start, depth)
				zero, args, limit = form.beta, []any{qArg}, 10
			}
			want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: zero, Args: zeroArgs(args)}, false)
			var got *sqlcorpus.Answer
			err := s.Read(func(rd store.Reader) error {
				got = planQuery(rd, form.beta, planArgs(args), planNow)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if limit < 0 {
				limit = math.MaxInt
			}
			exact, ok := sameUpToTies(want, got, func(k string) float64 { return d.distanceTo(k, q) }, 0, limit)
			switch {
			case !ok:
				bad++
				if bad <= 10 {
					t.Errorf("%s %v\n  0.x  %s\n  Beta %s", form.beta, args, answerText(want), answerText(got))
				}
			case want.Error != "":
				counts["errors"]++
			case len(want.Rows) == 0:
				counts["no rows"]++
			case exact:
				counts["the same rows"]++
			default:
				counts["the same rows but for ties"]++
			}
			counts["rows in all"] += len(want.Rows)
		}
		db.Close()
		if bad > 0 {
			t.Errorf("%d of %d differ, with vectors of %d values", bad, queries, dims)
		}
	}
	t.Logf("%d crux queries: %v", 2*queries, counts)
}

// twoForms writes a statement as the Beta takes it and as 0.x runs it, side
// by side, with the arguments of its ? marks once.
type twoForms struct {
	beta, zero strings.Builder
	args       []any
}

func (f *twoForms) both(s ...string) *twoForms {
	for _, x := range s {
		f.beta.WriteString(x)
		f.zero.WriteString(x)
	}
	return f
}

// each writes one text for the Beta and another for 0.x.
func (f *twoForms) each(beta, zero string) *twoForms {
	f.beta.WriteString(beta)
	f.zero.WriteString(zero)
	return f
}

func (f *twoForms) arg(v any) *twoForms {
	f.both("?")
	f.args = append(f.args, v)
	return f
}

// plannedGen makes random statements of the planner's shapes over the data
// newRandomData makes, with ties settled for 0.x as the Beta settles them:
// by key over a table, and by the walk's order, json_each's key, over a
// walk.
type plannedGen struct {
	r *rand.Rand
	d *randomData
}

func (g *plannedGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

func (g *plannedGen) start() any {
	switch g.r.IntN(12) {
	case 0:
		return "customer:99"
	case 1:
		return g.d.docs[g.r.IntN(len(g.d.docs))]
	case 2:
		return int64(7)
	}
	return g.d.custs[g.r.IntN(len(g.d.custs))]
}

// walk writes walk(...) with random arguments, and now and then, when
// refused is set, arguments it refuses.
func (g *plannedGen) walk(f *twoForms, refused bool) {
	start := g.start()
	if _, ok := start.(string); !ok && !refused {
		start = g.d.custs[0]
	}
	f.both("walk(").arg(start).both(", ")
	switch n := g.r.IntN(30); {
	case n == 0 && refused:
		f.arg(int64(0))
	case n == 1 && refused:
		f.arg(2.0)
	default:
		f.arg(int64(1 + g.r.IntN(3)))
	}
	if g.r.IntN(2) == 0 {
		f.both(", ").arg([]any{nil, "owns", "cites", "knows", "", "nothing"}[g.r.IntN(6)])
		if g.r.IntN(2) == 0 {
			dirs := []any{nil, "out", "in", "both", "IN", "up"}
			if !refused {
				dirs = dirs[:5]
			}
			f.both(", ").arg(dirs[g.r.IntN(len(dirs))])
		}
	}
	f.both(")")
}

// question is a query vector: a random one, a document's own, given as
// bytes or JSON text, and now and then zeros, one of another size, NULL or
// a document's vector through the one-record subquery.
func (g *plannedGen) question(f *twoForms) {
	switch g.r.IntN(12) {
	case 0:
		f.both("(SELECT vec FROM docs WHERE key = ").arg(g.d.docs[g.r.IntN(len(g.d.docs))]).both(")")
		return
	case 1:
		f.arg(c.Vector(make([]float32, g.d.dims)))
		return
	case 2:
		f.arg(c.Vector(make([]float32, g.d.dims+1)))
		return
	case 3:
		f.both("NULL")
		return
	}
	_, q := cruxQuestion(g.r, g.d)
	f.arg(q)
}

// cond is a condition that never fails, on the table of a join if there's
// one, as d. Its walks take arguments they never refuse: where a statement
// has more than one, or other terms, 0.x may work out its terms in another
// order, or read the table first when a term on its key names records, and
// skip a walk that would fail, as SQL.md allows.
func (g *plannedGen) cond(f *twoForms, d string, inWalks bool) {
	n := g.r.IntN(9)
	if n == 6 && !inWalks {
		n = 8
	}
	switch n {
	case 0:
		f.both(d + "status = ").arg(statuses[g.r.IntN(len(statuses)-1)])
	case 1:
		f.both(d + "n > ").arg(int64(g.r.IntN(40) - 10))
	case 2:
		f.both(d + "title LIKE 'doc 1%'")
	case 3:
		f.both(d + "key > ").arg(g.d.docs[g.r.IntN(len(g.d.docs))])
	case 4:
		f.both(d + "status IS NULL")
	case 5:
		f.both(d + "n IS NOT NULL")
	case 6:
		f.both(d + "key ")
		if g.r.IntN(2) == 0 {
			f.both("NOT ")
		}
		f.both("IN (").each("SELECT key FROM ", "SELECT value FROM json_each(")
		g.walk(f, false)
		f.each(")", "))")
	case 7:
		f.both(d + "n IN (").arg(int64(g.r.IntN(10))).both(", ").arg(int64(g.r.IntN(10))).both(", 3)")
	default:
		f.both(d + "vec IS NOT NULL")
	}
}

// where writes a WHERE of up to three conditions, or none, with must
// among them when it's given. most caps the count.
func (g *plannedGen) where(f *twoForms, d string, inWalks bool, most int, must ...string) {
	n := min(g.r.IntN(4), most)
	if len(must) > 0 && n == 0 {
		n = 1
	}
	if n == 0 {
		return
	}
	at := -1
	if len(must) > 0 {
		at = g.r.IntN(n)
	}
	f.both(" WHERE ")
	for i := 0; i < n; i++ {
		if i > 0 {
			f.both(" AND ")
		}
		if i == at {
			f.both(must[0])
		} else {
			g.cond(f, d, inWalks)
		}
	}
}

func (g *plannedGen) limit(f *twoForms) {
	if g.r.IntN(5) == 0 {
		return
	}
	f.both(" LIMIT ").arg(int64(g.r.IntN(15) - 1))
	if g.r.IntN(4) == 0 {
		f.both(" OFFSET ").arg(int64(g.r.IntN(5)))
	}
}

// cap3 is the most conditions a join's WHERE gets: one when its walk may
// be refused, since SQLite's constant propagation can make two terms on one
// field a constant that rules every row out before the walk runs, and
// three otherwise.
func cap3(refused bool) int {
	if refused {
		return 1
	}
	return 3
}

// statement makes one random statement, and says whether its reals may
// differ by conformance.DistanceBound.
func (g *plannedGen) statement() (f *twoForms, close bool) {
	f = &twoForms{}
	switch g.r.IntN(9) {
	case 0, 1: // a nearest search, or the sort that stands in for it
		f.both("SELECT key, title")
		if g.r.IntN(3) == 0 {
			f.both(", distance(vec, ")
			g.question(f)
			f.both(") AS dd")
			close = true
		}
		f.both(" FROM docs")
		g.where(f, "", true, 3, "vec IS NOT NULL")
		f.both(" ORDER BY distance(vec, ")
		g.question(f)
		f.both(")").each("", ", key")
		f.both(" LIMIT ").arg(int64(g.r.IntN(15) - 1))
		if g.r.IntN(3) == 0 {
			f.both(" OFFSET ").arg(int64(g.r.IntN(5)))
		}
	case 2: // ORDER BY distance() over rows with and without a vector
		f.both("SELECT key FROM docs")
		g.where(f, "", true, 3)
		f.both(" ORDER BY distance(vec, ")
		g.question(f)
		f.both(")"+g.pick("", " DESC")).each("", ", key")
		g.limit(f)
	case 3: // the walk as a table
		f.each("SELECT key FROM ", "SELECT value AS key FROM json_each(")
		g.walk(f, true)
		f.each("", ")")
		if g.r.IntN(2) == 0 {
			f.both(" WHERE ").each("key ", "value ").both(g.pick("> 'docs:0200'", "< 'docs:0100'", "LIKE 'customer%'"))
		}
		f.both(" ORDER BY key" + g.pick("", " DESC"))
		g.limit(f)
	case 4: // a walk join
		f.both("SELECT d.key, d.n, d.status FROM ")
		refused := g.r.IntN(2) == 0
		if g.r.IntN(3) == 0 {
			f.each("", "json_each(")
			g.walk(f, refused)
			f.each(" w JOIN docs d ON d.key = w.key", ") w JOIN docs d ON d.key = w.value")
		} else {
			f.both("json_each(")
			g.walk(f, refused)
			f.both(") w JOIN docs d ON d.key = w.value")
		}
		g.where(f, "d.", !refused, cap3(refused))
		f.both(" ORDER BY ")
		switch g.r.IntN(3) {
		case 0:
			f.both("d.n" + g.pick("", " DESC"))
		case 1:
			f.both("distance(d.vec, ")
			g.question(f)
			f.both(")")
		default:
			f.both("d.status, d.title DESC")
		}
		f.each("", ", w.key")
		g.limit(f)
	case 5: // IN and NOT IN over walks
		// A walk that may be refused gets one other condition at most, as
		// in a join (cap3).
		refused := g.r.IntN(2) == 0
		before, after := g.r.IntN(3) == 0, g.r.IntN(3) == 0
		if refused && before {
			after = false
		}
		f.both("SELECT key, n FROM docs WHERE ")
		if before {
			g.cond(f, "", true)
			f.both(" AND ")
		}
		f.both("key "+g.pick("", "NOT ")+"IN (").each("SELECT key FROM ", "SELECT value FROM json_each(")
		g.walk(f, refused)
		f.each(")", "))")
		if after {
			f.both(" AND ")
			g.cond(f, "", true)
		}
		f.both(" ORDER BY " + g.pick("key", "key DESC", "n, key", "n DESC, key"))
		g.limit(f)
	case 6: // the one-record subquery
		k := func() any { return g.d.docs[g.r.IntN(len(g.d.docs))] }
		switch g.r.IntN(3) {
		case 0:
			f.both("SELECT key FROM docs WHERE n > (SELECT n FROM docs WHERE key = ").arg(k()).both(") ORDER BY key LIMIT 10")
		case 1:
			f.both("SELECT (SELECT title FROM docs WHERE key = ").arg(k()).both("), (SELECT n FROM docs WHERE key = ").arg(k()).both(")")
		default:
			f.both("SELECT key, title FROM docs WHERE key = (SELECT key FROM docs WHERE key = ").arg(k()).both(")")
		}
	case 7: // aggregates over a walk join
		f.both("SELECT count(*), sum(d.n), min(d.title), max(d.n), count(d.vec) FROM json_each(")
		refused := g.r.IntN(2) == 0
		g.walk(f, refused)
		f.both(") w JOIN docs d ON d.key = w.value")
		g.where(f, "d.", !refused, cap3(refused))
	default: // walk() as a function
		f.both("SELECT key, ")
		g.walk(f, true)
		f.both(" FROM docs WHERE key < ").arg(g.d.docs[g.r.IntN(len(g.d.docs))]).both(" ORDER BY key DESC LIMIT 3")
	}
	return f, close
}

// TestRandomPlannedQueriesAs0xHasThem runs random statements of the
// planner's shapes on the same random data in 0.x and through the planner:
// nearest searches and the sorts that stand in for them, ORDER BY distance()
// over rows without a vector, walks as tables, walk joins, IN and NOT IN
// over walks, the one-record subquery, aggregates over a join and walk() as
// a function, with walks and query vectors refused now and then.
func TestRandomPlannedQueriesAs0xHasThem(t *testing.T) {
	queries := 3000
	if testing.Short() {
		queries = 600
	}
	plannedAs0x(t, 6, queries)
}

// plannedAs0x runs queries random statements on each of two databases made
// from seed, and reports how many differ from 0.x's answers.
func plannedAs0x(t *testing.T, seed uint64, queries int) {
	r := rand.New(rand.NewPCG(seed, 0x5135))
	counts := map[string]int{}
	for _, dims := range []int{3, 16} {
		d := newRandomData(r, dims, 300, 30)
		e, db, s := d.open(t)
		g := &plannedGen{r: r, d: d}
		bad := 0
		for i := 0; i < queries; i++ {
			f, close := g.statement()
			beta, zero := f.beta.String(), f.zero.String()
			want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: zero, Args: zeroArgs(f.args)}, false)
			var got *sqlcorpus.Answer
			var shape Shape
			err := s.Read(func(rd store.Reader) error {
				got = planQuery(rd, beta, planArgs(f.args), planNow)
				if st, err := Parse(beta); err == nil {
					if p, err := Prepare(rd, st.(*Select)); err == nil {
						shape = p.Shape()
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !sameAnswer(want, got, close && dims > 3) {
				bad++
				if bad <= 10 {
					t.Errorf("%s %v\n  0.x  %s %s\n  Beta %s %s", beta, f.args, answerText(want), want.Message, answerText(got), got.Message)
				}
			}
			switch {
			case want.Error != "":
				counts[string(shape)+", errors"]++
			case len(want.Rows) == 0:
				counts[string(shape)+", no rows"]++
			default:
				counts[string(shape)]++
			}
		}
		db.Close()
		if bad > 0 {
			t.Errorf("%d of %d differ, with vectors of %d values", bad, queries, dims)
		}
	}
	t.Logf("%d statements: %v", 2*queries, counts)
}

// TestNearestFiltersAs0xHasThem runs Nearest with random filters on the
// same random data in 0.x and through plannedNearest, which takes its
// filter from NearestFilter: up to three conditions, among them names it
// can't find, constant ones, ones that fail for one record, the one-record
// subquery and IN over a walk; random queries and k, now and then ones
// Nearest refuses. The hits must agree as conformance.CompareHits has it,
// and the errors by kind.
func TestNearestFiltersAs0xHasThem(t *testing.T) {
	searches := 1500
	if testing.Short() {
		searches = 300
	}
	nearestFiltersAs0x(t, 7, searches)
}

// nearestFiltersAs0x runs searches random searches on each of two
// databases made from seed.
func nearestFiltersAs0x(t *testing.T, seed uint64, searches int) {
	r := rand.New(rand.NewPCG(seed, 0x5135))
	counts := map[string]int{}
	for _, dims := range []int{3, 16} {
		d := newRandomData(r, dims, 300, 30)
		e, db, s := d.open(t)
		g := &plannedGen{r: r, d: d}
		bad := 0
		for i := 0; i < searches; i++ {
			// A condition that fails goes alone: beside others that rule its
			// record out, 0.x may check them in another order, or skip the
			// record by its key, as SQL.md allows.
			f := &twoForms{}
			switch n := r.IntN(4); {
			case r.IntN(8) == 0:
				f.both("abs(-9223372036854775807 - (key = ").arg(d.docs[r.IntN(len(d.docs))]).both("))")
			case r.IntN(16) == 0:
				f.both("abs(-9223372036854775808)")
			default:
				for j := 0; j < n; j++ {
					if j > 0 {
						f.both(" AND ")
					}
					switch r.IntN(12) {
					case 0:
						f.both(g.pick("1 = 1", "0", "nosuch = 1"))
					case 1:
						f.both("? IS NULL")
						f.args = append(f.args, []any{nil, int64(1)}[r.IntN(2)])
					case 2:
						f.both("n > (SELECT n FROM docs WHERE key = ").arg(d.docs[r.IntN(len(d.docs))]).both(")")
					default:
						g.cond(f, "", true)
					}
				}
			}
			q := make([]float32, dims)
			for j := range q {
				q[j] = float32(r.NormFloat64())
			}
			switch r.IntN(30) {
			case 0:
				q = make([]float32, dims)
			case 1:
				q = append(q, 1)
			}
			k := 1 + r.IntN(12)
			if r.IntN(30) == 0 {
				k = []int{0, -1, store.MaxK + 1}[r.IntN(3)]
			}
			want, werr := db.Nearest("docs", c.Vector(q), k, f.zero.String(), f.args...)
			var got []store.Hit
			var gerr error
			if err := s.Read(func(rd store.Reader) error {
				got, gerr = plannedNearest(rd, "docs", q, k, f.beta.String(), planArgs(f.args), planNow)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			wk, gk := difftest.Kind(e, werr), betaKind(gerr)
			why := ""
			switch {
			case wk != gk:
				why = fmt.Sprintf("0.x %s %v, Beta %s %v", wk, werr, gk, gerr)
			case werr == nil:
				hits := make([]c.Hit, len(got))
				for j, h := range got {
					hits[j] = c.Hit{Key: h.Key, Distance: h.Distance}
				}
				// zerox hands an empty list on as nil, so only the hits count.
				if err := c.CompareHits(hits, want); err != nil {
					why = err.Error()
				}
			}
			if why != "" {
				bad++
				if bad <= 10 {
					t.Errorf("Nearest(docs, k %d, %q, %v): %s", k, f.beta.String(), f.args, why)
				}
			}
			switch {
			case werr != nil:
				counts["refused by "+wk]++
			case len(want) == 0:
				counts["no hits"]++
			default:
				counts["hits"]++
			}
		}
		db.Close()
		if bad > 0 {
			t.Errorf("%d of %d differ, with vectors of %d values", bad, searches, dims)
		}
	}
	t.Logf("%d searches: %v", 2*searches, counts)
}
