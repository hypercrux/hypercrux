// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sqlcorpus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/difftest"
)

// Case is one entry in the corpus: a statement, its arguments, whether it's
// inside the Beta's subset, and 0.x's answer.
type Case struct {
	ID    string           `json:"id"`
	Kind  string           `json:"kind"` // statement, expression, date or now
	SQL   string           `json:"sql"`
	Args  []difftest.Value `json:"args,omitempty"`
	Via   string           `json:"via,omitempty"`   // what 0.x runs instead, for a form only the Beta has
	After string           `json:"after,omitempty"` // a query run after a write, before it's rolled back
	In    bool             `json:"in"`              // inside the Beta's subset, as beta/SQL.md settles it
	Close bool             `json:"close,omitempty"` // reals may differ by conformance.DistanceBound
	Note  string           `json:"note,omitempty"`  // for a statement outside the subset, why
	// Answer is 0.x's answer. A "now" case has none, since the date moves:
	// it's compared with a reference engine's answer at the time.
	Answer *Answer `json:"answer,omitempty"`
}

// Answer is what a statement gave back.
type Answer struct {
	Error   string             `json:"error,omitempty"`   // the error's kind, as difftest.Kind names it
	Message string             `json:"message,omitempty"` // 0.x's error message, for reference; never compared
	Columns []string           `json:"columns,omitempty"`
	Rows    [][]difftest.Value `json:"rows,omitempty"`
	Changed *int64             `json:"changed,omitempty"` // the rows a write changed
	After   *Answer            `json:"after,omitempty"`   // the case's After query, inside the write's transaction
}

// errRollback undoes a write once its answer is in.
var errRollback = errors.New("sqlcorpus: rolled back on purpose")

// isQuery tells a statement that only reads from one that may write.
func isQuery(sql string) bool {
	s := strings.ToUpper(strings.TrimSpace(sql))
	for _, w := range []string{"SELECT", "WITH", "VALUES", "EXPLAIN"} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// changes tells the statements whose count of changed rows means something.
func changes(sql string) bool {
	s := strings.ToUpper(strings.TrimSpace(sql))
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

func args(vs []difftest.Value) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = v.V
	}
	return out
}

func failed(e c.Engine, err error) *Answer {
	return &Answer{Error: difftest.Kind(e, err), Message: err.Error()}
}

func read(e c.Engine, h c.Handle, sql string, a []any) *Answer {
	rows, err := h.Query(sql, a...)
	if err != nil {
		return failed(e, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return failed(e, err)
	}
	ans := &Answer{Columns: cols}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return failed(e, err)
		}
		row := make([]difftest.Value, len(vals))
		for i, v := range vals {
			row[i] = difftest.Value{V: v}
		}
		ans.Rows = append(ans.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return failed(e, err)
	}
	return ans
}

// Run runs one case on db and returns its answer. A statement that may
// write runs inside an Update that's then rolled back, so the data stays as
// Fixture made it. beta says whether the engine takes the forms only the
// Beta has; when it doesn't, a case's Via runs instead.
func Run(e c.Engine, db c.DB, cs Case, beta bool) *Answer {
	sql := cs.SQL
	if cs.Via != "" && !beta {
		sql = cs.Via
	}
	a := args(cs.Args)
	if isQuery(sql) {
		return read(e, db, sql, a)
	}
	var ans *Answer
	err := db.Update(func(tx c.Handle) error {
		res, err := tx.Exec(sql, a...)
		if err != nil {
			ans = failed(e, err)
			return errRollback
		}
		ans = &Answer{}
		// Only these say how many rows they changed; after anything else,
		// SQLite's count is left over from an earlier statement.
		if changes(sql) {
			n, err := res.RowsAffected()
			if err != nil {
				ans = failed(e, err)
				return errRollback
			}
			ans.Changed = &n
		}
		if cs.After != "" {
			ans.After = read(e, tx, cs.After, nil)
		}
		return errRollback
	})
	switch {
	case ans == nil:
		return failed(e, err)
	case err != nil && !errors.Is(err, errRollback):
		return failed(e, fmt.Errorf("rolling back: %w", err))
	}
	return ans
}

// bare is an answer without its message, which isn't compared.
func bare(a *Answer) *Answer {
	if a == nil {
		return nil
	}
	b := *a
	b.Message = ""
	b.After = bare(a.After)
	return &b
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unwritable: " + err.Error()
	}
	return string(b)
}

// Same reports whether two answers agree, apart from their messages:
// exactly, or with reals within conformance.DistanceBound when close is set.
func Same(want, got *Answer, close bool) bool {
	if !close {
		return jsonOf(bare(want)) == jsonOf(bare(got))
	}
	if want == nil || got == nil {
		return want == got
	}
	if want.Error != got.Error || !slices.Equal(want.Columns, got.Columns) || len(want.Rows) != len(got.Rows) ||
		jsonOf(want.Changed) != jsonOf(got.Changed) {
		return false
	}
	for i := range want.Rows {
		if len(want.Rows[i]) != len(got.Rows[i]) {
			return false
		}
		for j := range want.Rows[i] {
			x, y := want.Rows[i][j].V, got.Rows[i][j].V
			fx, okx := x.(float64)
			fy, oky := y.(float64)
			if okx && oky {
				if !(math.Abs(fx-fy) <= c.DistanceBound) {
					return false
				}
				continue
			}
			if jsonOf(want.Rows[i][j]) != jsonOf(got.Rows[i][j]) {
				return false
			}
		}
	}
	return Same(want.After, got.After, close)
}

// Fixture fills a new database with the data every case runs on: three
// tables with fields of every type, vectors, and links between them. It
// goes through the conformance API, so any engine builds the same database.
func Fixture(db c.DB) error {
	return db.Update(func(tx c.Handle) error {
		for _, r := range fixture {
			if err := tx.Put(r.key, r.fields); err != nil {
				return fmt.Errorf("fixture %s: %w", r.key, err)
			}
		}
		for _, l := range fixtureLinks {
			if err := tx.Link(l.From, l.Type, l.To); err != nil {
				return fmt.Errorf("fixture link %v: %w", l, err)
			}
		}
		return nil
	})
}

// The fixture. The order of the Puts sets each table's field order, which
// SELECT * shows: Put adds a call's new fields in name order.
var fixture = []struct {
	key    string
	fields c.Fields
}{
	{"docs:1", c.Fields{"title": "Q3 plan", "status": "open", "n": int64(3), "score": 1.5, "vec": c.Vector{0.9, 0.1, 0}}},
	{"docs:2", c.Fields{"title": "Hiring notes", "status": "done", "n": int64(1), "score": math.Copysign(0, -1), "vec": c.Vector{0.1, 0.9, 0.1}}},
	{"docs:3", c.Fields{"title": "Q3 budget", "status": "open", "n": int64(7), "score": 2.25, "vec": c.Vector{0.8, 0.2, 0.1}, "data": []byte{0, 1, 2}}},
	{"docs:4", c.Fields{"title": "Retro", "status": "archived", "score": 0.1, "tags": []any{"a", "b"}}},
	{"docs:5", c.Fields{"title": "É accents", "status": "open", "n": int64(10), "score": 1e21, "vec": c.Vector{0.5, 0.5, 0.5}}},
	{"docs:6", c.Fields{"title": "", "status": "done", "n": int64(-2), "data": []byte{}, "vec": c.Vector{0, 0, 1}}},
	{"docs:7", c.Fields{"title": "a%b_c", "status": "OPEN", "n": int64(42), "score": int64(3), "vec": c.Vector{0.9, 0.1, 0}}},
	{"docs:8", c.Fields{"title": "12", "n": "5"}},
	{"people:1", c.Fields{"name": "Dana", "age": int64(34), "email": "dana@example.com", "joined": "2026-01-15"}},
	{"people:2", c.Fields{"name": "Eli", "age": int64(27), "joined": "2025-12-31"}},
	{"people:3", c.Fields{"name": "Noa", "email": "noa@example.com", "joined": "2024-02-29"}},
	{"people:4", c.Fields{"name": "ʼAmir", "age": int64(61), "joined": "2026-10-07"}},
	{"t_1:a", c.Fields{"x": int64(1), "y": 0.5, "vec": c.Vector{1, 0}}},
	{"t_1:b", c.Fields{"x": int64(2), "y": -1.25, "vec": c.Vector{0, 1}}},
	{"t_1:c", c.Fields{"x": int64(3), "y": 1e-7, "vec": c.Vector{0.6, 0.8}}},
	{"t_1:d", c.Fields{"x": int64(4), "vec": c.Vector{-1, 0}}},
	{"t_1:é", c.Fields{"x": int64(5), "y": 2.5}},
}

var fixtureLinks = []c.Link{
	{From: "people:1", Type: "owns", To: "docs:1"},
	{From: "people:1", Type: "owns", To: "docs:2"},
	{From: "people:2", Type: "owns", To: "docs:3"},
	{From: "people:3", Type: "owns", To: "docs:5"},
	{From: "docs:1", Type: "cites", To: "docs:3"},
	{From: "docs:3", Type: "cites", To: "docs:5"},
	{From: "docs:2", Type: "cites", To: "docs:1"},
	{From: "docs:5", Type: "cites", To: "docs:7"},
	{From: "people:3", Type: "knows", To: "people:1"},
	{From: "people:1", Type: "knows", To: "people:2"},
	{From: "t_1:a", Type: "next", To: "t_1:b"},
	{From: "t_1:b", Type: "next", To: "t_1:c"},
	{From: "t_1:c", Type: "next", To: "t_1:a"},
}

// OpenFixture opens a new database for e in a new folder under dir and
// fills it with Fixture.
func OpenFixture(e c.Engine, dir string) (c.DB, error) {
	d, err := os.MkdirTemp(dir, "corpus")
	if err != nil {
		return nil, err
	}
	db, err := e.Open(filepath.Join(d, "db"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.Name(), err)
	}
	if err := Fixture(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", e.Name(), err)
	}
	return db, nil
}

// Difference is a case whose answer isn't the one recorded.
type Difference struct {
	Case      Case
	Want, Got *Answer
}

func (d Difference) String() string {
	return fmt.Sprintf("%s: %s\n  want %s\n  got  %s", d.Case.ID, d.Case.SQL, jsonOf(d.Want), jsonOf(d.Got))
}

// Replay runs every case on a new database for e, built with Fixture, and
// returns the cases whose answers differ from the recorded ones. A "now"
// case is compared with ref's answer at the same moment, on ref's own
// database, also built with Fixture. beta is passed on to Run.
func Replay(e, ref c.Engine, cases []Case, dir string, beta bool) ([]Difference, error) {
	db, err := OpenFixture(e, dir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var refDB c.DB
	defer func() {
		if refDB != nil {
			refDB.Close()
		}
	}()
	var diffs []Difference
	for _, cs := range cases {
		want, got := cs.Answer, Run(e, db, cs, beta)
		if cs.Kind == "now" {
			if refDB == nil {
				if refDB, err = OpenFixture(ref, dir); err != nil {
					return nil, err
				}
			}
			// Two answers a moment apart can fall either side of a second.
			for try := 0; try < 3; try++ {
				if want = Run(ref, refDB, cs, false); Same(want, got, cs.Close) {
					break
				}
				got = Run(e, db, cs, beta)
			}
		}
		if !Same(want, got, cs.Close) {
			diffs = append(diffs, Difference{Case: cs, Want: want, Got: got})
		}
	}
	return diffs, nil
}

// Save writes cases as JSON lines.
func Save(path string, cases []Case) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, cs := range cases {
		if err := enc.Encode(cs); err != nil {
			return fmt.Errorf("%s: %w", cs.ID, err)
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// Load reads cases Save wrote.
func Load(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		var cs Case
		if err := json.Unmarshal(sc.Bytes(), &cs); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		cases = append(cases, cs)
	}
	return cases, sc.Err()
}
