// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"errors"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A test is one of 0.x's tests, ported to the Engine interface. The names
// match 0.x's test names where a test came over whole, and say which part
// came over where only part of it did.
type test struct {
	name string
	fn   func(t *testing.T, e Engine)
}

// Tests lists every test in the suite, in the order Run runs them, so a plan
// can name the ones an engine must pass.
func Tests() []string {
	var names []string
	for _, tc := range tests() {
		names = append(names, tc.name)
	}
	return names
}

func tests() []test {
	return []test{
		// Records.
		{"OpenAndReopen", testOpenAndReopen},
		{"OpenNeedsAFile", testOpenNeedsAFile},
		{"PutAndGetRoundTrip", testPutAndGetRoundTrip},
		{"PutKeepsFieldsItIsNotGiven", testPutKeepsFieldsItIsNotGiven},
		{"KeyAndFieldRules", testKeyAndFieldRules},
		{"MoreFieldTypes", testMoreFieldTypes},
		{"GetAndDeleteMissing", testGetAndDeleteMissing},
		{"Scan", testScan},
		{"NewFieldsShowAtOnce", testNewFieldsShowAtOnce},
		// Links.
		{"LinksFollowTheirRecords", testLinksFollowTheirRecords},
		{"Walk", testWalk},
		{"RulesForLongKeysAndLinkTypes", testRulesForLongKeysAndLinkTypes},
		{"TableNamesLikeHyperCruxsOwn", testTableNamesLikeHyperCruxsOwn},
		{"DropTable", testDropTable},
		// Vectors.
		{"Vectors", testVectors},
		{"NearestIsExact", testNearestIsExact},
		// SQL.
		{"WalkInSQL", testWalkInSQL},
		{"OneStatementCrossesAllFour", testOneStatementCrossesAllFour},
		{"VectorAsQueryArgument", testVectorAsQueryArgument},
		{"PlainSQLFollowsTheRules", testPlainSQLFollowsTheRules},
		// Transactions and concurrency.
		{"UpdateIsAllOrNothing", testUpdateIsAllOrNothing},
		{"GoroutinesShareADB", testGoroutinesShareADB},
		{"OneConnection", testOneConnection},
		// Processes.
		{"KilledWritersNeverLeaveAMess", testKilledWritersNeverLeaveAMess},
		{"ProcessesShareAFile", testProcessesShareAFile},
	}
}

// Run runs every test against e, each as a subtest, skipping the ones e
// names in Skip. The process tests start copies of the test binary, so the
// adapter's TestMain must call Main.
func Run(t *testing.T, e Engine) {
	t.Logf("engine: %s", e.Name())
	skip := e.Skip()
	known := map[string]bool{}
	for _, tc := range tests() {
		known[tc.name] = true
	}
	for name := range skip {
		if !known[name] {
			t.Errorf("Skip names %q, which isn't a test", name)
		}
	}
	for _, tc := range tests() {
		t.Run(tc.name, func(t *testing.T) {
			if why, ok := skip[tc.name]; ok {
				t.Skip(why)
			}
			tc.fn(t, e)
		})
	}
}

// From TestOpenSetsUpTheFile: the part that doesn't look inside SQLite.
func testOpenAndReopen(t *testing.T, e Engine) {
	db, path := openTemp(t, e)
	checkOK(t, db)
	db.Close()
	db2 := must[DB](t)(e.Open(path))
	defer db2.Close()
	if rep := checkOK(t, db2); rep.Tables != 0 || rep.Records != 0 {
		t.Fatalf("empty file reported %+v", rep)
	}
}

// From TestOpenNeedsAFile.
func testOpenNeedsAFile(t *testing.T, e Engine) {
	for _, p := range []string{":memory:", "file::memory:", "", "a?b"} {
		if db, err := e.Open(p); !errors.Is(err, e.ErrInvalid()) {
			if err == nil {
				db.Close()
			}
			t.Errorf("Open(%q) = %v", p, err)
		}
	}
	if db, err := e.Open(filepath.Join(t.TempDir(), "no", "such", "dir.db")); err == nil {
		db.Close()
		t.Error("opened a file in a missing directory")
	}
}

// From TestPutAndGetRoundTrip.
func testPutAndGetRoundTrip(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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

// From TestPutKeepsFieldsItIsNotGiven.
func testPutKeepsFieldsItIsNotGiven(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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
	ok(t, db.Put("docs:2", nil))
	if f := must[Fields](t)(db.Get("docs:2")); len(f) != 0 {
		t.Fatalf("empty record has fields %v", f)
	}
	// Field names match without regard to case.
	ok(t, db.Put("docs:1", Fields{"TITLE": "final"}))
	if f := must[Fields](t)(db.Get("docs:1")); f["title"] != "final" {
		t.Fatalf("TITLE didn't update title: %v", f)
	}
}

// From TestKeyAndFieldRules.
func testKeyAndFieldRules(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	for _, key := range []string{"", "docs", "docs:", ":7", "Docs:7", "7docs:1", "hc_keys:1", "sqlite_master:1",
		"my-docs:1", "docs:\x00", "docs:" + strings.Repeat("x", MaxKeyLen), "docs:\xff"} {
		if err := db.Put(key, Fields{"a": 1}); !errors.Is(err, e.ErrInvalid()) {
			t.Errorf("Put(%q) = %v, want ErrInvalid", key, err)
		}
	}
	for _, f := range []Fields{{"key": 1}, {"rowid": 1}, {"has space": 1}, {"1st": 1}, {"a": 1, "A": 2},
		{"a": math.NaN()}, {"a": math.Inf(1)}, {"a": uint64(math.MaxUint64)}, {"a": make(chan int)}, {"a": func() {}},
		{"a": "\xff"}, {"emb": Vector{1}}, {"vec": "not a vector"}, {"vec": Vector{}}, {"vec": Vector{0, 0}},
		{"vec": Vector{1, float32(math.NaN())}}} {
		if err := db.Put("docs:1", f); !errors.Is(err, e.ErrInvalid()) {
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

type status string

// From TestMoreFieldTypes.
func testMoreFieldTypes(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	var nilInt *int
	n := 7
	ok(t, db.Put("docs:1", Fields{
		"status": status("open"), "tags": []string{"a", "b"}, "meta": map[string]string{"k": "v"},
		"point": struct{ X, Y int }{1, 2}, "missing": nilInt, "count": &n,
	}))
	f := must[Fields](t)(db.Get("docs:1"))
	want := Fields{"status": "open", "tags": `["a","b"]`, "meta": `{"k":"v"}`, "point": `{"X":1,"Y":2}`, "count": int64(7)}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got  %#v\nwant %#v", f, want)
	}
}

// From TestGetAndDeleteMissing.
func testGetAndDeleteMissing(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	_, err := db.Get("docs:1")
	wantErr(t, err, e.ErrNotFound())
	wantErr(t, db.Delete("docs:1"), e.ErrNotFound())
	ok(t, db.Put("docs:1", Fields{"a": 1}))
	_, err = db.Get("docs:2")
	wantErr(t, err, e.ErrNotFound())
	ok(t, db.Delete("docs:1"))
	_, err = db.Get("docs:1")
	wantErr(t, err, e.ErrNotFound())
	wantErr(t, db.Delete("docs:1"), e.ErrNotFound())
}

// From TestScan.
func testScan(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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
		if _, err := db.Scan(bad, "", 0); !errors.Is(err, e.ErrInvalid()) {
			t.Errorf("Scan(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

// From TestSchemaChangesShowAtOnce: a new field shows at once, inside a
// transaction too. The rest of that test changes columns with plain SQL.
func testNewFieldsShowAtOnce(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", Fields{"a": 1}))
	must[Fields](t)(db.Get("docs:1"))
	ok(t, db.Put("docs:1", Fields{"b": 2}))
	if f := must[Fields](t)(db.Get("docs:1")); !reflect.DeepEqual(f, Fields{"a": int64(1), "b": int64(2)}) {
		t.Fatalf("after a new field: %v", f)
	}
	ok(t, db.Update(func(tx Handle) error {
		ok(t, tx.Put("docs:2", Fields{"c": 3}))
		if f := must[Fields](t)(tx.Get("docs:2")); f["c"] != int64(3) {
			t.Errorf("inside the transaction, Get missed the new field: %v", f)
		}
		return nil
	}))
	checkOK(t, db)
}

// From TestLinksFollowTheirRecords.
func testLinksFollowTheirRecords(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	for _, k := range []string{"customer:42", "docs:1", "docs:2", "docs:3"} {
		ok(t, db.Put(k, Fields{"name": k}))
	}
	ok(t, db.Link("customer:42", "owns", "docs:1"))
	ok(t, db.Link("customer:42", "owns", "docs:1")) // again: no change
	ok(t, db.Link("customer:42", "owns", "docs:2"))
	ok(t, db.Link("customer:42", "watches", "docs:1"))
	ok(t, db.Link("docs:1", "cites", "docs:3"))
	ok(t, db.Link("docs:3", "cites", "docs:3")) // a record may link to itself

	wantErr(t, db.Link("customer:42", "owns", "docs:9"), e.ErrNotFound())
	wantErr(t, db.Link("customer:9", "owns", "docs:1"), e.ErrNotFound())
	wantErr(t, db.Link("customer:42", "", "docs:1"), e.ErrInvalid())
	wantErr(t, db.Link("customer:42", strings.Repeat("x", 201), "docs:1"), e.ErrInvalid())

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
	wantErr(t, err, e.ErrNotFound())

	// Deleting a record takes every link to and from it.
	ok(t, db.Delete("docs:1"))
	got = must[[]Link](t)(db.Neighbours("customer:42", Both, ""))
	if !reflect.DeepEqual(got, []Link{{"customer:42", "owns", "docs:2"}}) {
		t.Fatalf("after deleting docs:1: %v", got)
	}
	if got := must[[]Link](t)(db.Neighbours("docs:3", In, "")); !reflect.DeepEqual(got, []Link{{"docs:3", "cites", "docs:3"}}) {
		t.Fatalf("docs:3 still linked from the deleted docs:1: %v", got)
	}

	wantErr(t, db.Unlink("customer:42", "watches", "docs:2"), e.ErrNotFound())
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
func chain(t *testing.T, db DB) {
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
		out = append(out, x.Key+"@"+strconv.Itoa(x.Depth))
	}
	return strings.Join(out, " ")
}

// From TestWalk.
func testWalk(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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
		wantErr(t, err, e.ErrInvalid())
	}
	_, err := db.Walk("n:zz", Out, "", 1)
	wantErr(t, err, e.ErrNotFound())
}

// From TestRulesForLongKeysAndLinkTypes, without the inserts into 0.x's own
// link table.
func testRulesForLongKeysAndLinkTypes(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", nil))
	long := "docs:" + strings.Repeat("x", MaxKeyLen)
	if _, err := db.Exec(`INSERT INTO docs (key) VALUES (?)`, long); !errors.Is(err, e.ErrInvalid()) {
		t.Fatalf("plain SQL stored a %d-byte key: %v", len(long), err)
	}
	ok(t, db.Put("docs:2", nil))
	typ := strings.Repeat("ü", 150) // 150 characters, 300 bytes
	ok(t, db.Link("docs:1", typ, "docs:2"))
	tooLong := strings.Repeat("ü", 201)
	wantErr(t, db.Link("docs:1", tooLong, "docs:2"), e.ErrInvalid())
	checkOK(t, db)
}

// From TestTableNamesLikeHyperCruxsOwn: tables may be called anything,
// including the words 0.x uses in its own names. The vector of zeros goes in
// as an argument instead of SQLite's zeroblob().
func testTableNamesLikeHyperCruxsOwn(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs", "hc"} {
		if tbl == "hc" {
			ok(t, db.Put("hc:1", nil)) // hc alone is a fine name
			continue
		}
		ok(t, db.Put(tbl+":1", Fields{"vec": Vector{1, 0}}))
		ok(t, db.Put(tbl+":2", Fields{"vec": Vector{0, 1}}))
		ok(t, db.Link(tbl+":1", "next", tbl+":2"))
	}
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		ok(t, db.Delete(tbl+":2"))
		if links := must[[]Link](t)(db.Neighbours(tbl+":1", Both, "")); len(links) != 0 {
			t.Errorf("%s: deleting a record left its link: %v", tbl, links)
		}
		if _, err := db.Exec(`INSERT INTO "` + tbl + `" (key) VALUES ('other:9')`); !errors.Is(err, e.ErrInvalid()) {
			t.Errorf("%s took a key from another table: %v", tbl, err)
		}
		if _, err := db.Exec(`UPDATE "`+tbl+`" SET vec = ? WHERE key = '`+tbl+`:1'`, make([]byte, 8)); !errors.Is(err, e.ErrInvalid()) {
			t.Errorf("%s took a vector of zeros: %v", tbl, err)
		}
		if hits := must[[]Hit](t)(db.Nearest(tbl, Vector{1, 0}, 5, "")); len(hits) != 1 {
			t.Errorf("%s: Nearest found %v", tbl, hits)
		}
	}
	checkOK(t, db)
}

// From TestDropAndAdoptAfterSchemaChanges: the part about Drop. The rest
// rebuilds and adopts tables with plain SQL.
func testDropTable(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("customer:1", nil))
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		ok(t, db.Put(k, Fields{"title": k, "vec": Vector{1, 2, 3}}))
		ok(t, db.Link("customer:1", "owns", k))
	}
	ok(t, db.Drop("docs"))
	if links := must[[]Link](t)(db.Neighbours("customer:1", Out, "")); len(links) != 0 {
		t.Fatalf("after Drop: %v", links)
	}
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		_, err := db.Get(k)
		wantErr(t, err, e.ErrNotFound())
	}
	checkOK(t, db)
	// A new docs table can take vectors of another size.
	ok(t, db.Put("docs:9", Fields{"vec": Vector{1, 2}}))
	wantErr(t, db.Drop("nosuch"), e.ErrNotFound())
	checkOK(t, db)
}

// From TestVectors.
func testVectors(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", Fields{"title": "no vector yet"}))
	// The first vector sets the size for the table.
	ok(t, db.Put("docs:2", Fields{"vec": []float64{1, 0, 0}}))
	ok(t, db.Put("docs:1", Fields{"vec": []float32{0, 1, 0}}))
	wantErr(t, db.Put("docs:3", Fields{"vec": Vector{1, 0}}), e.ErrInvalid())
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
	if v, err := e.ParseVector("[0.5, 1e-3, -2]"); err != nil || !reflect.DeepEqual(v, Vector{0.5, 0.001, -2}) {
		t.Fatalf("ParseVector: %v %v", v, err)
	}
}

// From TestNearestIsExact, against a plain float64 reference.
func testNearestIsExact(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	r := rand.New(rand.NewPCG(1, 2))
	vecs := map[string]Vector{}
	ok(t, db.Update(func(tx Handle) error {
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
		sameHits(t, got, bruteNearest(vecs, q, 10, nil))
		// With a filter.
		got = must[[]Hit](t)(db.Nearest("docs", q, 7, "\"group\" = ?", 1))
		want := bruteNearest(vecs, q, 7, func(key string) bool {
			n, _ := strconv.Atoi(key[strings.IndexByte(key, '-')+1:])
			return n%3 == 1
		})
		sameHits(t, got, want)
	}
	_, err := db.Nearest("docs", Vector{1, 2}, 5, "")
	wantErr(t, err, e.ErrInvalid())
	_, err = db.Nearest("nosuch", randomVector(r, 32), 5, "")
	wantErr(t, err, e.ErrNotFound())
	_, err = db.Nearest("docs", randomVector(r, 32), 0, "")
	wantErr(t, err, e.ErrInvalid())
	ok(t, db.Put("plain:1", Fields{"a": 1}))
	if hits := must[[]Hit](t)(db.Nearest("plain", Vector{1}, 3, "")); len(hits) != 0 {
		t.Fatalf("a table without vectors gave %v", hits)
	}
}

// From TestWalkInSQL.
func testWalkInSQL(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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

// From TestOneStatementCrossesAllFour: the query from the README, records
// reached by links, filtered by a field, ordered by vector distance.
func testOneStatementCrossesAllFour(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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
	rows, err := db.Query(`
		SELECT d.key, d.title
		FROM json_each(walk('customer:42', 2)) w
		JOIN docs d ON d.key = w.value
		WHERE d.status = 'open' AND d.vec IS NOT NULL
		ORDER BY distance(d.vec, ?)
		LIMIT 10`, Vector{1, 0, 0})
	if got := firstColumn(t, rows, err); got != "docs:1 docs:3" {
		t.Fatalf("got %q", got)
	}
	// The query vector can be JSON text too, and vector() turns JSON into a blob.
	rows, err = db.Query(`SELECT key FROM docs ORDER BY distance(vec, '[0, 1, 0]') LIMIT 1`)
	if got := firstColumn(t, rows, err); got != "docs:2" {
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

// From TestVectorAsQueryArgument.
func testVectorAsQueryArgument(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", Fields{"vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"vec": Vector{0, 1}}))
	var key string
	ok(t, db.QueryRow(`SELECT key FROM docs ORDER BY distance(vec, ?) LIMIT 1`, Vector{0.1, 0.9}).Scan(&key))
	if key != "docs:2" {
		t.Fatalf("got %s", key)
	}
}

// From TestPlainSQLFollowsTheRules: inserts, updates and deletes on record
// tables go through the same rules as Put and Delete. The parts that write
// 0.x's own key and link tables, or use an upsert, stay in 0.x; blobs go in
// as arguments instead of SQLite's blob literals and zeroblob().
func testPlainSQLFollowsTheRules(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", Fields{"title": "a", "vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"title": "b"}))
	ok(t, db.Link("docs:1", "next", "docs:2"))

	fails := []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO docs (key, title) VALUES ('notes:1', 'wrong table')`, nil},
		{`INSERT INTO docs (key, title) VALUES ('docs:', 'no id')`, nil},
		{`INSERT INTO docs (key, title) VALUES (7, 'number')`, nil},
		{`UPDATE docs SET key = 'docs:9' WHERE key = 'docs:1'`, nil},
		{`UPDATE docs SET vec = ? WHERE key = 'docs:2'`, []any{[]byte{0, 0, 0x80, 0x3f}}},        // one value, the table holds two
		{`UPDATE docs SET vec = 'text' WHERE key = 'docs:2'`, nil},                               // not a blob
		{`UPDATE docs SET vec = ? WHERE key = 'docs:2'`, []any{make([]byte, 8)}},                 // all zeros
		{`INSERT INTO docs (key, vec) VALUES ('docs:3', ?)`, []any{[]byte{0, 0, 0x80, 0x3f, 0}}}, // not whole float32s
	}
	for _, f := range fails {
		if _, err := db.Exec(f.stmt, f.args...); !errors.Is(err, e.ErrInvalid()) {
			t.Errorf("%s: got %v, want one of HyperCrux's rules to refuse it", f.stmt, err)
		}
	}
	// What is allowed works, and keeps everything in step.
	must[any](t)(db.Exec(`INSERT INTO docs (key, title, vec) VALUES ('docs:3', 'c', vector('[0.5, 0.5]'))`))
	ok(t, db.Link("docs:3", "next", "docs:1"))
	must[any](t)(db.Exec(`UPDATE docs SET title = 'z' WHERE key = 'docs:2'`))
	if f := must[Fields](t)(db.Get("docs:3")); f["title"] != "c" {
		t.Fatalf("Get after an SQL insert: %v", f)
	}
	if f := must[Fields](t)(db.Get("docs:2")); f["title"] != "z" {
		t.Fatalf("Get after an SQL update: %v", f)
	}
	must[any](t)(db.Exec(`DELETE FROM docs WHERE key = 'docs:1'`))
	if links := must[[]Link](t)(db.Neighbours("docs:2", Both, "")); len(links) != 0 {
		t.Fatalf("an SQL delete left links: %v", links)
	}
	if links := must[[]Link](t)(db.Neighbours("docs:3", Out, "")); len(links) != 0 {
		t.Fatalf("an SQL delete left links: %v", links)
	}
	_, err := db.Get("docs:1")
	wantErr(t, err, e.ErrNotFound())
	ok(t, db.Link("docs:2", "next", "docs:3"))
	must[any](t)(db.Exec(`DELETE FROM docs`))
	if rep := checkOK(t, db); rep.Records != 0 || rep.Links != 0 {
		t.Fatalf("after deleting every doc: %+v", rep)
	}
}

// From TestUpdateIsAllOrNothing.
func testUpdateIsAllOrNothing(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	ok(t, db.Put("docs:1", Fields{"n": 1}))
	boom := errors.New("boom")
	err := db.Update(func(tx Handle) error {
		ok(t, tx.Put("docs:2", Fields{"n": 2, "new_column": "x", "vec": Vector{1, 2, 3}}))
		ok(t, tx.Link("docs:1", "next", "docs:2"))
		ok(t, tx.Delete("docs:1"))
		ok(t, tx.Put("fresh:1", Fields{"a": 1}))
		// Inside the transaction its own changes are visible to every handle.
		if _, err := tx.Get("docs:1"); !errors.Is(err, e.ErrNotFound()) {
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
		wantErr(t, err, e.ErrNotFound())
	}
	// The rolled-back new field and table don't confuse the next Put.
	ok(t, db.Put("docs:3", Fields{"new_column": "y"}))
	ok(t, db.Put("fresh:2", Fields{"a": 2}))
	func() {
		defer func() { recover() }()
		db.Update(func(tx Handle) error {
			tx.Put("docs:4", Fields{"n": 4})
			panic("handler panicked")
		})
	}()
	_, err = db.Get("docs:4")
	wantErr(t, err, e.ErrNotFound())
	checkOK(t, db)
}

// From TestGoroutinesShareADB.
func testGoroutinesShareADB(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
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
				err := db.Update(func(tx Handle) error {
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

// From TestOneConnection: everything works with a single connection, as
// some programs configure.
func testOneConnection(t *testing.T, e Engine) {
	db, _ := openTemp(t, e)
	db.SQL().SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		done <- db.Update(func(tx Handle) error {
			for _, k := range []string{"docs:1", "docs:2"} {
				if err := tx.Put(k, Fields{"title": k, "vec": Vector{1, float32(len(k))}}); err != nil {
					return err
				}
			}
			if err := tx.Link("docs:1", "next", "docs:2"); err != nil {
				return err
			}
			if _, err := tx.Get("docs:1"); err != nil {
				return err
			}
			if _, err := tx.Scan("docs:", "", 0); err != nil {
				return err
			}
			if _, err := tx.Nearest("docs", Vector{1, 1}, 1, ""); err != nil {
				return err
			}
			if _, err := tx.Walk("docs:1", Out, "", 2); err != nil {
				return err
			}
			if _, err := tx.Neighbours("docs:1", Both, ""); err != nil {
				return err
			}
			return tx.Delete("docs:2")
		})
	}()
	select {
	case err := <-done:
		ok(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("a transaction on a single connection hung")
	}
	checkOK(t, db)
}
