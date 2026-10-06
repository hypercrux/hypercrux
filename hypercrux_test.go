// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t testing.TB) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func must[T any](t testing.TB) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func checkOK(t testing.TB, db *DB) Report {
	t.Helper()
	rep := must[Report](t)(db.Check())
	if !rep.OK() {
		t.Fatalf("Check found problems:\n%s", strings.Join(rep.Problems, "\n"))
	}
	return rep
}

func TestOpenSetsUpTheFile(t *testing.T) {
	db, path := openTemp(t)
	var app int64
	var mode, version string
	ok(t, db.QueryRow(`PRAGMA application_id`).Scan(&app))
	ok(t, db.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	ok(t, db.QueryRow(`SELECT value FROM hc_meta WHERE name = 'format_version'`).Scan(&version))
	if app != ApplicationID || mode != "wal" || version != "1" {
		t.Fatalf("application_id %#x, journal_mode %s, format_version %s", app, mode, version)
	}
	db.Close()
	// Opening again finds the tables in place.
	db2 := must[*DB](t)(Open(path))
	defer db2.Close()
	if rep := checkOK(t, db2); rep.Tables != 0 || rep.Records != 0 {
		t.Fatalf("empty file reported %+v", rep)
	}
}

func TestOpenRefusesANewerFormat(t *testing.T) {
	db, path := openTemp(t)
	must[any](t)(db.Exec(`UPDATE hc_meta SET value = '2' WHERE name = 'format_version'`))
	db.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "format version 2") {
		t.Fatalf("opened a format 2 file: %v", err)
	}
}

func TestOpenAddsTablesToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db := must[*DB](t)(Open(path))
	db.Close()
	// A second database that already has an application table of its own.
	path2 := filepath.Join(t.TempDir(), "app2.db")
	raw := must[*DB](t)(Open(path2)) // creates it; drop our tables to simulate a foreign file
	for _, s := range []string{`DROP TABLE hc_meta`, `DROP TABLE hc_tables`, `DROP TABLE hc_keys`, `DROP TABLE hc_links`, `PRAGMA application_id = 0`,
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`, `INSERT INTO settings VALUES ('theme', 'dark')`} {
		must[any](t)(raw.Exec(s))
	}
	raw.Close()
	db = must[*DB](t)(Open(path2))
	defer db.Close()
	var app int64
	ok(t, db.QueryRow(`PRAGMA application_id`).Scan(&app))
	if app != 0 {
		t.Fatalf("set application_id %#x on a database that had its own tables", app)
	}
	// The app's own table is not a record table, and its rows are untouched.
	if _, err := db.Get("settings:theme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unadopted table answered Get: %v", err)
	}
	must[any](t)(db.Exec(`INSERT INTO settings VALUES ('lang', 'en')`))
	checkOK(t, db)
}

func TestPutAndGetRoundTrip(t *testing.T) {
	db, _ := openTemp(t)
	when := time.Date(2026, 10, 6, 9, 30, 0, 0, time.FixedZone("IDT", 3*3600))
	ok(t, db.Put("docs:1", Fields{
		"title": "Q3 plan", "pages": 12, "score": 0.75, "draft": true,
		"raw": []byte{0, 1, 255}, "due": when, "tags": []any{"plan", "q3"},
		"meta": map[string]any{"owner": "dana"}, "small": uint8(7),
		"vec": Vector{0.5, -0.25, 1},
	}))
	f := must[Fields](t)(db.Get("docs:1"))
	want := Fields{
		"title": "Q3 plan", "pages": int64(12), "score": 0.75, "draft": int64(1),
		"raw": []byte{0, 1, 255}, "due": "2026-10-06T06:30:00Z", "tags": `["plan","q3"]`,
		"meta": `{"owner":"dana"}`, "small": int64(7), "vec": Vector{0.5, -0.25, 1},
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got  %#v\nwant %#v", f, want)
	}
	checkOK(t, db)
}

func TestPutKeepsFieldsItIsNotGiven(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"title": "draft", "status": "open"}))
	ok(t, db.Put("docs:1", Fields{"status": "done", "owner": "sam"}))
	f := must[Fields](t)(db.Get("docs:1"))
	if f["title"] != "draft" || f["status"] != "done" || f["owner"] != "sam" {
		t.Fatalf("after two puts: %v", f)
	}
	ok(t, db.Put("docs:1", Fields{"owner": nil}))
	f = must[Fields](t)(db.Get("docs:1"))
	if _, has := f["owner"]; has {
		t.Fatalf("nil didn't clear the field: %v", f)
	}
	ok(t, db.Put("docs:2", nil)) // a record with only a key
	if f := must[Fields](t)(db.Get("docs:2")); len(f) != 0 {
		t.Fatalf("empty record has fields %v", f)
	}
	// Field names match columns without regard to case.
	ok(t, db.Put("docs:1", Fields{"TITLE": "final"}))
	if f := must[Fields](t)(db.Get("docs:1")); f["title"] != "final" {
		t.Fatalf("TITLE didn't update title: %v", f)
	}
}

func TestKeyAndFieldRules(t *testing.T) {
	db, _ := openTemp(t)
	for _, key := range []string{"", "docs", "docs:", ":7", "Docs:7", "7docs:1", "hc_keys:1", "sqlite_master:1",
		"my-docs:1", "docs:\x00", "docs:" + strings.Repeat("x", MaxKeyLen), "docs:\xff"} {
		if err := db.Put(key, Fields{"a": 1}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Put(%q) = %v, want ErrInvalid", key, err)
		}
	}
	for _, f := range []Fields{{"key": 1}, {"rowid": 1}, {"has space": 1}, {"1st": 1}, {"a": 1, "A": 2},
		{"a": math.NaN()}, {"a": math.Inf(1)}, {"a": uint64(math.MaxUint64)}, {"a": make(chan int)}, {"a": func() {}},
		{"a": "\xff"}, {"emb": Vector{1}}, {"vec": "not a vector"}, {"vec": Vector{}}, {"vec": Vector{0, 0}},
		{"vec": Vector{1, float32(math.NaN())}}} {
		if err := db.Put("docs:1", f); !errors.Is(err, ErrInvalid) {
			t.Errorf("Put(docs:1, %v) = %v, want ErrInvalid", f, err)
		}
	}
	// Keys can hold anything after the colon.
	for _, key := range []string{"docs:7", "docs:a:b:c", "docs:2026-10-06/report.pdf", "docs:Ünïcode ✓", "x:1"} {
		ok(t, db.Put(key, Fields{"a": 1}))
		must[Fields](t)(db.Get(key))
	}
	checkOK(t, db)
}

func TestGetAndDeleteMissing(t *testing.T) {
	db, _ := openTemp(t)
	_, err := db.Get("docs:1")
	wantErr(t, err, ErrNotFound)
	wantErr(t, db.Delete("docs:1"), ErrNotFound)
	ok(t, db.Put("docs:1", Fields{"a": 1}))
	_, err = db.Get("docs:2")
	wantErr(t, err, ErrNotFound)
	ok(t, db.Delete("docs:1"))
	_, err = db.Get("docs:1")
	wantErr(t, err, ErrNotFound)
	wantErr(t, db.Delete("docs:1"), ErrNotFound)
}

func TestScan(t *testing.T) {
	db, _ := openTemp(t)
	for _, k := range []string{"docs:b", "docs:a", "docs:c1", "docs:c2", "docs:d", "notes:a"} {
		ok(t, db.Put(k, Fields{"k": k, "vec": Vector{1, 2}}))
	}
	keys := func(recs []Record) string {
		var s []string
		for _, r := range recs {
			s = append(s, r.Key)
			if _, has := r.Fields["vec"]; has {
				t.Fatalf("Scan returned a vector for %s", r.Key)
			}
			if r.Fields["k"] != r.Key {
				t.Fatalf("Scan mixed up fields: %s has %v", r.Key, r.Fields)
			}
		}
		return strings.Join(s, " ")
	}
	cases := []struct {
		prefix, after string
		limit         int
		want          string
	}{
		{"docs:", "", 0, "docs:a docs:b docs:c1 docs:c2 docs:d"},
		{"docs:c", "", 0, "docs:c1 docs:c2"},
		{"docs:", "docs:b", 2, "docs:c1 docs:c2"},
		{"docs:", "", 1, "docs:a"},
		{"notes:", "", 0, "notes:a"},
		{"other:", "", 0, ""},
		{"docs:zz", "", 0, ""},
	}
	for _, c := range cases {
		got := keys(must[[]Record](t)(db.Scan(c.prefix, c.after, c.limit)))
		if got != c.want {
			t.Errorf("Scan(%q, %q, %d) = %q, want %q", c.prefix, c.after, c.limit, got, c.want)
		}
	}
	for _, bad := range []string{"docs", "", "Docs:"} {
		if _, err := db.Scan(bad, "", 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("Scan(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	for _, c := range []struct{ in, out string }{{"docs:", "docs;"}, {"a\xff", "b"}, {"a\xff\xff", "b"}} {
		if got, _ := prefixEnd(c.in); got != c.out {
			t.Errorf("prefixEnd(%q) = %q, want %q", c.in, got, c.out)
		}
	}
	if _, ok := prefixEnd("\xff\xff"); ok {
		t.Error("prefixEnd of all 0xff should have no end")
	}
}

func TestLinksFollowTheirRecords(t *testing.T) {
	db, _ := openTemp(t)
	for _, k := range []string{"customer:42", "docs:1", "docs:2", "docs:3"} {
		ok(t, db.Put(k, Fields{"name": k}))
	}
	ok(t, db.Link("customer:42", "owns", "docs:1"))
	ok(t, db.Link("customer:42", "owns", "docs:1")) // again: no change
	ok(t, db.Link("customer:42", "owns", "docs:2"))
	ok(t, db.Link("customer:42", "watches", "docs:1"))
	ok(t, db.Link("docs:1", "cites", "docs:3"))
	ok(t, db.Link("docs:3", "cites", "docs:3")) // a record may link to itself

	wantErr(t, db.Link("customer:42", "owns", "docs:9"), ErrNotFound)
	wantErr(t, db.Link("customer:9", "owns", "docs:1"), ErrNotFound)
	wantErr(t, db.Link("customer:42", "", "docs:1"), ErrInvalid)
	wantErr(t, db.Link("customer:42", strings.Repeat("x", 201), "docs:1"), ErrInvalid)

	got := must[[]Link](t)(db.Neighbours("customer:42", Out, ""))
	want := []Link{{"customer:42", "owns", "docs:1"}, {"customer:42", "owns", "docs:2"}, {"customer:42", "watches", "docs:1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Out: %v", got)
	}
	got = must[[]Link](t)(db.Neighbours("docs:1", In, "owns"))
	if !reflect.DeepEqual(got, []Link{{"customer:42", "owns", "docs:1"}}) {
		t.Fatalf("In owns: %v", got)
	}
	got = must[[]Link](t)(db.Neighbours("docs:3", Both, ""))
	if !reflect.DeepEqual(got, []Link{{"docs:1", "cites", "docs:3"}, {"docs:3", "cites", "docs:3"}}) {
		t.Fatalf("Both: %v", got)
	}
	_, err := db.Neighbours("docs:9", Out, "")
	wantErr(t, err, ErrNotFound)

	// Deleting a record takes every link to and from it.
	ok(t, db.Delete("docs:1"))
	got = must[[]Link](t)(db.Neighbours("customer:42", Both, ""))
	if !reflect.DeepEqual(got, []Link{{"customer:42", "owns", "docs:2"}}) {
		t.Fatalf("after deleting docs:1: %v", got)
	}
	if got := must[[]Link](t)(db.Neighbours("docs:3", In, "")); !reflect.DeepEqual(got, []Link{{"docs:3", "cites", "docs:3"}}) {
		t.Fatalf("docs:3 still linked from the deleted docs:1: %v", got)
	}

	wantErr(t, db.Unlink("customer:42", "watches", "docs:2"), ErrNotFound)
	ok(t, db.Link("customer:42", "watches", "docs:2"))
	ok(t, db.Unlink("customer:42", "", "docs:2")) // every type
	if got := must[[]Link](t)(db.Neighbours("customer:42", Out, "")); len(got) != 0 {
		t.Fatalf("Unlink with no type left %v", got)
	}
	rep := checkOK(t, db)
	if rep.Links != 1 || rep.Records != 3 {
		t.Fatalf("report %+v", rep)
	}
}

// chain builds a -> b -> c -> d with "next" links, plus a side link a -> x
// of type "see", and a loop d -> a.
func chain(t *testing.T, db *DB) {
	for _, k := range []string{"n:a", "n:b", "n:c", "n:d", "n:x"} {
		ok(t, db.Put(k, nil))
	}
	ok(t, db.Link("n:a", "next", "n:b"))
	ok(t, db.Link("n:b", "next", "n:c"))
	ok(t, db.Link("n:c", "next", "n:d"))
	ok(t, db.Link("n:d", "next", "n:a"))
	ok(t, db.Link("n:a", "see", "n:x"))
}

func steps(s []Step) string {
	var out []string
	for _, x := range s {
		out = append(out, x.Key+"@"+string(rune('0'+x.Depth)))
	}
	return strings.Join(out, " ")
}

func TestWalk(t *testing.T) {
	db, _ := openTemp(t)
	chain(t, db)
	cases := []struct {
		key   string
		dir   Direction
		typ   string
		depth int
		want  string
	}{
		{"n:a", Out, "", 1, "n:b@1 n:x@1"},
		{"n:a", Out, "next", 2, "n:b@1 n:c@2"},
		{"n:a", Out, "", 10, "n:b@1 n:x@1 n:c@2 n:d@3"}, // the loop back to n:a ends
		{"n:a", In, "", 1, "n:d@1"},
		{"n:a", In, "", 3, "n:d@1 n:c@2 n:b@3"},
		{"n:c", Both, "next", 1, "n:b@1 n:d@1"},
		{"n:x", Both, "", 2, "n:a@1 n:b@2 n:d@2"},
		{"n:x", Out, "", 5, ""},
	}
	for _, c := range cases {
		got := steps(must[[]Step](t)(db.Walk(c.key, c.dir, c.typ, c.depth)))
		if got != c.want {
			t.Errorf("Walk(%s, %v, %q, %d) = %q, want %q", c.key, c.dir, c.typ, c.depth, got, c.want)
		}
	}
	for _, d := range []int{0, -1, MaxDepth + 1} {
		_, err := db.Walk("n:a", Out, "", d)
		wantErr(t, err, ErrInvalid)
	}
	_, err := db.Walk("n:zz", Out, "", 1)
	wantErr(t, err, ErrNotFound)
}

func TestWalkInSQL(t *testing.T) {
	db, _ := openTemp(t)
	chain(t, db)
	var got string
	ok(t, db.QueryRow(`SELECT walk('n:a', 2)`).Scan(&got))
	if got != `["n:b","n:x","n:c"]` {
		t.Fatalf("walk('n:a', 2) = %s", got)
	}
	ok(t, db.QueryRow(`SELECT walk('n:a', 3, 'next', 'in')`).Scan(&got))
	if got != `["n:d","n:c","n:b"]` {
		t.Fatalf("walk in = %s", got)
	}
	ok(t, db.QueryRow(`SELECT walk('n:x', 1, NULL, 'both')`).Scan(&got))
	if got != `["n:a"]` {
		t.Fatalf("walk both = %s", got)
	}
	ok(t, db.QueryRow(`SELECT walk('n:zz', 1)`).Scan(&got))
	if got != `[]` {
		t.Fatalf("walk from a missing key = %s", got)
	}
	var n int
	ok(t, db.QueryRow(`SELECT count(*) FROM json_each(walk('n:a', 10))`).Scan(&n))
	if n != 4 {
		t.Fatalf("json_each(walk) gave %d keys", n)
	}
	for _, bad := range []string{`SELECT walk('n:a', 0)`, `SELECT walk('n:a')`, `SELECT walk('n:a', 1, 'next', 'sideways')`, `SELECT walk(1, 1)`} {
		var s string
		if err := db.QueryRow(bad).Scan(&s); err == nil {
			t.Errorf("%s worked", bad)
		}
	}
}

func TestVectors(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"title": "no vector yet"}))
	// The first vector sets the size for the table.
	ok(t, db.Put("docs:2", Fields{"vec": []float64{1, 0, 0}}))
	ok(t, db.Put("docs:1", Fields{"vec": []float32{0, 1, 0}}))
	wantErr(t, db.Put("docs:3", Fields{"vec": Vector{1, 0}}), ErrInvalid)
	if f := must[Fields](t)(db.Get("docs:1")); !reflect.DeepEqual(f["vec"], Vector{0, 1, 0}) {
		t.Fatalf("vec came back as %#v", f["vec"])
	}
	// Another table has its own size.
	ok(t, db.Put("imgs:1", Fields{"vec": Vector{1, 2, 3, 4, 5}}))
	// Clearing a vector is fine.
	ok(t, db.Put("docs:2", Fields{"vec": nil}))
	if f := must[Fields](t)(db.Get("docs:2")); f["vec"] != nil {
		t.Fatalf("vec not cleared: %v", f)
	}
	rep := checkOK(t, db)
	if rep.Vectors != 2 {
		t.Fatalf("report %+v", rep)
	}
	if v, err := ParseVector("[0.5, 1e-3, -2]"); err != nil || !reflect.DeepEqual(v, Vector{0.5, 0.001, -2}) {
		t.Fatalf("ParseVector: %v %v", v, err)
	}
	if Vector([]float32{0.5, 1}).String() != "[0.5,1]" {
		t.Fatal("Vector.String")
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b Vector
		want float64
	}{
		{Vector{1, 0}, Vector{1, 0}, 0},
		{Vector{1, 0}, Vector{0, 1}, 1},
		{Vector{1, 0}, Vector{-1, 0}, 2},
		{Vector{1, 1}, Vector{2, 2}, 0},
		{Vector{3, 4}, Vector{4, 3}, 1 - 24.0/25},
		{Vector{0, 0}, Vector{1, 0}, 1},
	}
	for _, c := range cases {
		got, err := cosine(c.a.Bytes(), c.b.Bytes())
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("cosine(%v, %v) = %v, %v; want %v", c.a, c.b, got, err, c.want)
		}
	}
	if _, err := cosine(Vector{1}.Bytes(), Vector{1, 2}.Bytes()); err == nil {
		t.Error("cosine of different sizes worked")
	}
}

// bruteNearest is the reference: every vector compared, by plain Go.
func bruteNearest(vecs map[string]Vector, q Vector, k int, keep func(string) bool) []Hit {
	var hits []Hit
	for key, v := range vecs {
		if keep != nil && !keep(key) {
			continue
		}
		d, _ := cosine(v.Bytes(), q.Bytes())
		hits = append(hits, Hit{key, d})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Distance != hits[j].Distance {
			return hits[i].Distance < hits[j].Distance
		}
		return hits[i].Key < hits[j].Key
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

func randomVector(r *rand.Rand, dims int) Vector {
	v := make(Vector, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func TestNearestIsExact(t *testing.T) {
	db, _ := openTemp(t)
	r := rand.New(rand.NewPCG(1, 2))
	vecs := map[string]Vector{}
	ok(t, db.Update(func(tx *Tx) error {
		for i := 0; i < 2000; i++ {
			key := "docs:" + string(rune('a'+i%26)) + "-" + strconv.Itoa(i)
			v := randomVector(r, 32)
			vecs[key] = v
			if err := tx.Put(key, Fields{"group": i % 3, "vec": v}); err != nil {
				return err
			}
		}
		return nil
	}))
	for trial := 0; trial < 20; trial++ {
		q := randomVector(r, 32)
		got := must[[]Hit](t)(db.Nearest("docs", q, 10, ""))
		want := bruteNearest(vecs, q, 10, nil)
		sameHits(t, got, want)
		// With a filter.
		got = must[[]Hit](t)(db.Nearest("docs", q, 7, "\"group\" = ?", 1))
		want = bruteNearest(vecs, q, 7, func(key string) bool {
			n, _ := strconv.Atoi(key[strings.IndexByte(key, '-')+1:])
			return n%3 == 1
		})
		sameHits(t, got, want)
	}
	_, err := db.Nearest("docs", Vector{1, 2}, 5, "")
	wantErr(t, err, ErrInvalid)
	_, err = db.Nearest("nosuch", randomVector(r, 32), 5, "")
	wantErr(t, err, ErrNotFound)
	_, err = db.Nearest("docs", randomVector(r, 32), 0, "")
	wantErr(t, err, ErrInvalid)
	ok(t, db.Put("plain:1", Fields{"a": 1}))
	if hits := must[[]Hit](t)(db.Nearest("plain", Vector{1}, 3, "")); len(hits) != 0 {
		t.Fatalf("a table without vectors gave %v", hits)
	}
}

func sameHits(t *testing.T, got, want []Hit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Key != want[i].Key || math.Abs(got[i].Distance-want[i].Distance) > 1e-12 {
			t.Fatalf("hit %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

type sqlRows struct{ *sql.Rows }

func wrapRows(r *sql.Rows, err error) (*sqlRows, error) {
	if err != nil {
		return nil, err
	}
	return &sqlRows{r}, nil
}

// keys returns the first column of every row, joined by spaces.
func (r *sqlRows) keys(t *testing.T) string {
	t.Helper()
	defer r.Close()
	cols, err := r.Columns()
	if err != nil {
		t.Fatal(err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []string
	for r.Next() {
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals[0]))
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " ")
}

// TestOneStatementCrossesAllFour runs the query from the README: records
// reached by links, filtered by a field, ordered by vector distance.
func TestOneStatementCrossesAllFour(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("customer:42", Fields{"name": "Dana"}))
	docs := map[string]Fields{
		"docs:1": {"title": "Q3 plan", "status": "open", "vec": Vector{0.9, 0.1, 0}},
		"docs:2": {"title": "Hiring notes", "status": "done", "vec": Vector{0.1, 0.9, 0.1}},
		"docs:3": {"title": "Q3 budget", "status": "open", "vec": Vector{0.8, 0.2, 0.1}},
		"docs:4": {"title": "Q3 risks", "status": "open", "vec": Vector{0.95, 0.05, 0}}, // not linked
	}
	for k, f := range docs {
		ok(t, db.Put(k, f))
	}
	ok(t, db.Link("customer:42", "owns", "docs:1"))
	ok(t, db.Link("customer:42", "owns", "docs:2"))
	ok(t, db.Link("docs:1", "cites", "docs:3"))
	rows := must[*sqlRows](t)(wrapRows(db.Query(`
		SELECT d.key, d.title
		FROM json_each(walk('customer:42', 2)) w
		JOIN docs d ON d.key = w.value
		WHERE d.status = 'open' AND d.vec IS NOT NULL
		ORDER BY distance(d.vec, ?)
		LIMIT 10`, Vector{1, 0, 0})))
	if got := rows.keys(t); got != "docs:1 docs:3" {
		t.Fatalf("got %q", got)
	}
	// The query vector can be JSON text too, and vector() turns JSON into a blob.
	rows = must[*sqlRows](t)(wrapRows(db.Query(`
		SELECT key FROM docs ORDER BY distance(vec, '[0, 1, 0]') LIMIT 1`)))
	if got := rows.keys(t); got != "docs:2" {
		t.Fatalf("JSON query vector: %q", got)
	}
	var d float64
	ok(t, db.QueryRow(`SELECT distance(vector('[1, 0]'), vector('[0, 1]'))`).Scan(&d))
	if d != 1 {
		t.Fatalf("distance of vector() blobs = %v", d)
	}
	var null any
	ok(t, db.QueryRow(`SELECT distance(NULL, vec) FROM docs LIMIT 1`).Scan(&null))
	if null != nil {
		t.Fatalf("distance with NULL = %v", null)
	}
	for _, bad := range []string{`SELECT distance('[1,2]', '[1,2,3]')`, `SELECT vector('[0, 0]')`, `SELECT vector('nope')`, `SELECT distance(1, 2)`} {
		var x any
		if err := db.QueryRow(bad).Scan(&x); err == nil {
			t.Errorf("%s worked", bad)
		}
	}
}

func TestPlainSQLFollowsTheRules(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"title": "a", "vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"title": "b"}))
	ok(t, db.Link("docs:1", "next", "docs:2"))

	fails := []string{
		`INSERT INTO docs (key, title) VALUES ('notes:1', 'wrong table')`,
		`INSERT INTO docs (key, title) VALUES ('docs:', 'no id')`,
		`INSERT INTO docs (key, title) VALUES (7, 'number')`,
		`UPDATE docs SET key = 'docs:9' WHERE key = 'docs:1'`,
		`INSERT INTO hc_links VALUES ('docs:1', 'next', 'docs:99')`,
		`INSERT INTO hc_links VALUES ('docs:99', 'next', 'docs:1')`,
		`INSERT INTO hc_links VALUES ('docs:1', '', 'docs:2')`,
		`UPDATE hc_links SET type = 'other'`,
		`UPDATE hc_keys SET key = 'docs:7' WHERE key = 'docs:1'`,
		`UPDATE docs SET vec = x'0000803f' WHERE key = 'docs:2'`,       // one value, the table holds two
		`UPDATE docs SET vec = 'text' WHERE key = 'docs:2'`,            // not a blob
		`UPDATE docs SET vec = zeroblob(8) WHERE key = 'docs:2'`,       // all zeros
		`INSERT INTO docs (key, vec) VALUES ('docs:3', x'0000803f00')`, // not whole float32s
	}
	for _, s := range fails {
		if _, err := db.Exec(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want one of HyperCrux's rules to refuse it", s, err)
		}
	}
	// What is allowed works, and keeps everything in step.
	must[any](t)(db.Exec(`INSERT INTO docs (key, title, vec) VALUES ('docs:3', 'c', vector('[0.5, 0.5]'))`))
	must[any](t)(db.Exec(`INSERT INTO hc_links VALUES ('docs:3', 'next', 'docs:1')`))
	must[any](t)(db.Exec(`UPDATE docs SET title = 'z' WHERE key = 'docs:2'`))
	if f := must[Fields](t)(db.Get("docs:3")); f["title"] != "c" {
		t.Fatalf("Get after an SQL insert: %v", f)
	}
	must[any](t)(db.Exec(`DELETE FROM docs WHERE key = 'docs:1'`))
	if links := must[[]Link](t)(db.Neighbours("docs:2", Both, "")); len(links) != 0 {
		t.Fatalf("an SQL delete left links: %v", links)
	}
	if links := must[[]Link](t)(db.Neighbours("docs:3", Out, "")); len(links) != 0 {
		t.Fatalf("an SQL delete left links: %v", links)
	}
	_, err := db.Get("docs:1")
	wantErr(t, err, ErrNotFound)
	// An upsert in plain SQL keeps the record's links.
	ok(t, db.Link("docs:2", "next", "docs:3"))
	must[any](t)(db.Exec(`INSERT INTO docs (key, title) VALUES ('docs:2', 'y') ON CONFLICT (key) DO UPDATE SET title = excluded.title`))
	if links := must[[]Link](t)(db.Neighbours("docs:2", Out, "")); len(links) != 1 {
		t.Fatalf("an upsert lost links: %v", links)
	}
	must[any](t)(db.Exec(`DELETE FROM docs`))
	checkOK(t, db)
	var n int
	ok(t, db.QueryRow(`SELECT (SELECT count(*) FROM hc_keys) + (SELECT count(*) FROM hc_links)`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d keys and links left after deleting every doc", n)
	}
}

func TestUpdateIsAllOrNothing(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"n": 1}))
	boom := errors.New("boom")
	err := db.Update(func(tx *Tx) error {
		ok(t, tx.Put("docs:2", Fields{"n": 2, "new_column": "x", "vec": Vector{1, 2, 3}}))
		ok(t, tx.Link("docs:1", "next", "docs:2"))
		ok(t, tx.Delete("docs:1"))
		ok(t, tx.Put("fresh:1", Fields{"a": 1}))
		// Inside the transaction its own changes are visible to every handle.
		if _, err := tx.Get("docs:1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleted record still there inside the transaction: %v", err)
		}
		if hits := must[[]Hit](t)(tx.Nearest("docs", Vector{1, 2, 3}, 1, "")); len(hits) != 1 || hits[0].Key != "docs:2" {
			t.Errorf("Nearest inside the transaction: %v", hits)
		}
		var w string
		ok(t, tx.QueryRow(`SELECT walk('docs:2', 1, NULL, 'in')`).Scan(&w))
		if w != `[]` {
			t.Errorf("walk() inside the transaction: %s", w)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v", err)
	}
	if f := must[Fields](t)(db.Get("docs:1")); f["n"] != int64(1) {
		t.Fatalf("docs:1 after rollback: %v", f)
	}
	for _, k := range []string{"docs:2", "fresh:1"} {
		_, err := db.Get(k)
		wantErr(t, err, ErrNotFound)
	}
	// The rolled-back new column and table don't confuse the next Put.
	ok(t, db.Put("docs:3", Fields{"new_column": "y"}))
	ok(t, db.Put("fresh:2", Fields{"a": 2}))
	func() {
		defer func() { recover() }()
		db.Update(func(tx *Tx) error {
			tx.Put("docs:4", Fields{"n": 4})
			panic("handler panicked")
		})
	}()
	_, err = db.Get("docs:4")
	wantErr(t, err, ErrNotFound)
	checkOK(t, db)
}

func TestAdopt(t *testing.T) {
	db, _ := openTemp(t)
	must[any](t)(db.Exec(`CREATE TABLE notes (key TEXT PRIMARY KEY, body TEXT, vec BLOB)`))
	must[any](t)(db.Exec(`INSERT INTO notes VALUES ('notes:1', 'one', vector('[1, 0, 0]')), ('notes:2', 'two', NULL)`))
	ok(t, db.Adopt("notes"))
	ok(t, db.Adopt("notes")) // again: nothing to do
	if f := must[Fields](t)(db.Get("notes:1")); f["body"] != "one" {
		t.Fatalf("adopted record: %v", f)
	}
	ok(t, db.Link("notes:1", "see", "notes:2"))
	wantErr(t, db.Put("notes:3", Fields{"vec": Vector{1, 2}}), ErrInvalid) // size came from the existing vector
	ok(t, db.Put("notes:3", Fields{"vec": Vector{1, 2, 3}}))
	checkOK(t, db)

	must[any](t)(db.Exec(`CREATE TABLE things (key TEXT PRIMARY KEY, a)`))
	must[any](t)(db.Exec(`INSERT INTO things VALUES ('wrong:1', 1)`))
	wantErr(t, db.Adopt("things"), ErrInvalid)
	must[any](t)(db.Exec(`CREATE TABLE ids (id INTEGER PRIMARY KEY, key TEXT)`))
	wantErr(t, db.Adopt("ids"), ErrInvalid)
	must[any](t)(db.Exec(`CREATE TABLE badvec (key TEXT PRIMARY KEY, vec BLOB)`))
	must[any](t)(db.Exec(`INSERT INTO badvec VALUES ('badvec:1', x'00000000')`))
	wantErr(t, db.Adopt("badvec"), ErrInvalid)
	wantErr(t, db.Adopt("nosuch"), ErrNotFound)
	wantErr(t, db.Adopt("Bad-Name"), ErrInvalid)
	// A failed adoption leaves nothing behind.
	if _, err := db.Get("things:1"); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	checkOK(t, db)
}

func TestCheckFindsProblems(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"vec": Vector{0, 1}}))
	ok(t, db.Link("docs:1", "next", "docs:2"))
	checkOK(t, db)
	// Break things the way only someone working around the rules could.
	for _, s := range []string{
		`DROP TRIGGER "hc.docs.delete"`,
		`DELETE FROM docs WHERE key = 'docs:2'`, // no trigger now: key and link left behind
		`DROP TRIGGER "hc.docs.vec_update"`,
		`UPDATE docs SET vec = x'0000c07f0000803f' WHERE key = 'docs:1'`, // NaN
		`DROP TRIGGER hc_links_insert`,
		`INSERT INTO hc_links VALUES ('docs:1', 'ghost', 'docs:404')`,
	} {
		must[any](t)(db.Exec(s))
	}
	rep := must[Report](t)(db.Check())
	all := strings.Join(rep.Problems, "\n")
	for _, want := range []string{"missing its trigger hc.docs.delete", "missing its trigger hc.docs.vec_update",
		"keys in hc_keys with no row in docs: docs:2", "links to keys that don't exist: docs:404",
		"the vector of docs:1 has a value that isn't a finite number", "missing the trigger hc_links_insert"} {
		if !strings.Contains(all, want) {
			t.Errorf("Check didn't report %q; it said:\n%s", want, all)
		}
	}
}

func TestGoroutinesShareADB(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("hub:1", nil))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(g), 9))
			for i := 0; i < 50; i++ {
				key := "item:" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
				err := db.Update(func(tx *Tx) error {
					if err := tx.Put(key, Fields{"g": g, "vec": randomVector(r, 8)}); err != nil {
						return err
					}
					return tx.Link("hub:1", "has", key)
				})
				if err != nil {
					errs <- err
					return
				}
				if _, err := db.Nearest("item", randomVector(r, 8), 3, ""); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if links := must[[]Link](t)(db.Neighbours("hub:1", Out, "has")); len(links) != 400 {
		t.Fatalf("%d links, want 400", len(links))
	}
	checkOK(t, db)
}
