// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The cases of 0.x's tests that pin its rules for keys, fields and
// vectors, and the conformance suite's versions of them, run against the
// store. putGo does what the public package's Put does: it checks the key
// and the names first, converts the values with value.FromGo in byte order
// of name, and puts them, so the errors come in 0.x's order.

type fields = map[string]any

func putGo(s *store.Store, key string, f fields) error {
	_, err := putGoChanges(s, key, f)
	return err
}

func putGoChanges(s *store.Store, key string, f fields) ([]format.Change, error) {
	if _, err := rules.TableOf(key); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f))
	for name := range f {
		if err := rules.Field(name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for i := range names {
		for _, other := range names[:i] {
			if rules.SameName(names[i], other) {
				return nil, rules.CaseClash(names[i])
			}
		}
	}
	put := make([]format.Field, len(names))
	for i, name := range names {
		v, err := value.FromGo(name, f[name])
		if err != nil {
			return nil, err
		}
		put[i] = format.Field{Name: name, Value: v}
	}
	return s.Put(nil, key, put)
}

// getGo returns a record's fields as 0.x's Get gives them, with the vector
// as a []float32.
func getGo(s *store.Store, key string) (fields, error) {
	r, err := s.Get(key)
	if err != nil {
		return nil, err
	}
	t, ok := s.Table(key[:strings.IndexByte(key, ':')])
	if !ok {
		return nil, errors.New("no table for " + key)
	}
	out := fields{}
	for _, f := range r.Fields {
		out[t.Fields[f.Index]] = f.Value.Go()
	}
	if r.Vec != nil {
		out[t.Fields[t.Vec]] = slices.Clone(r.Vec)
	}
	return out, nil
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// From 0.x's TestKeyAndFieldRules and the suite's KeyAndFieldRules, with a
// []float32 standing in for 0.x's Vector.
func TestKeyAndFieldRules(t *testing.T) {
	s := store.New()
	for _, key := range []string{"", "docs", "docs:", ":7", "Docs:7", "7docs:1", "hc_keys:1", "sqlite_master:1",
		"my-docs:1", "docs:\x00", "docs:" + strings.Repeat("x", rules.MaxKeyLen), "docs:\xff"} {
		if err := putGo(s, key, fields{"a": 1}); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("Put(%q) = %v, want ErrInvalid", key, err)
		}
	}
	for _, f := range []fields{{"key": 1}, {"rowid": 1}, {"has space": 1}, {"1st": 1}, {"a": 1, "A": 2},
		{"a": math.NaN()}, {"a": math.Inf(1)}, {"a": uint64(math.MaxUint64)}, {"a": make(chan int)}, {"a": func() {}},
		{"a": "\xff"}, {"emb": []float32{1}}, {"vec": "not a vector"}, {"vec": []float32{}}, {"vec": []float32{0, 0}},
		{"vec": []float32{1, float32(math.NaN())}}} {
		if err := putGo(s, "docs:1", f); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("Put(docs:1, %v) = %v, want ErrInvalid", f, err)
		}
	}
	if _, ok := s.Table("docs"); ok {
		t.Fatal("puts that failed made a table")
	}
	// Keys can hold anything after the colon.
	for _, key := range []string{"docs:7", "docs:a:b:c", "docs:2026-10-06/report.pdf", "docs:Ünïcode ✓", "x:1"} {
		ok(t, putGo(s, key, fields{"a": 1}))
		must[fields](t)(getGo(s, key))
	}
}

// The same rules, with values that only SQL can make, which the store
// refuses as Put does.
func TestValuesFromSQL(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"title": "a", "vec": []float32{1, 0}}))
	for _, f := range [][]format.Field{
		{{Name: "title", Value: value.Text("\xff")}},
		{{Name: "n", Value: value.Real(math.Inf(-1))}},
		{{Name: "n", Value: value.Real(math.NaN())}},
		{{Name: "emb", Value: value.Vector([]float32{1})}},
		{{Name: "vec", Value: value.Bytes("\x00\x00\x80\x3f\x00\x00\x00\x00")}},
		{{Name: "vec", Value: value.Text("[1, 0]")}},
		{{Name: "vec", Value: value.Int(1)}},
		{{Name: "vec", Value: value.Vector([]float32{1})}},
		{{Name: "vec", Value: value.Vector([]float32{float32(math.Copysign(0, -1)), 0})}},
		{{Name: "vec", Value: value.VectorBits("")}},
		{{Name: "title", Value: value.Int(1)}, {Name: "title", Value: value.Int(2)}},
		{{Name: "Title", Value: value.Int(1)}, {Name: "title", Value: value.Int(2)}},
	} {
		changes, err := s.Put(nil, "docs:1", f)
		if !errors.Is(err, errs.ErrInvalid) || changes != nil {
			t.Errorf("Put(docs:1, %v) = %v, %v, want ErrInvalid", f, changes, err)
		}
	}
	if got := must[fields](t)(getGo(s, "docs:1")); !reflect.DeepEqual(got, fields{"title": "a", "vec": []float32{1, 0}}) {
		t.Fatalf("puts that failed changed the record to %v", got)
	}
}

// From TestPutAndGetRoundTrip.
func TestPutAndGetRoundTrip(t *testing.T) {
	s := store.New()
	when := time.Date(2026, 10, 6, 9, 30, 0, 0, time.FixedZone("IDT", 3*3600))
	ok(t, putGo(s, "docs:1", fields{
		"title": "Q3 plan", "pages": 12, "score": 0.75, "draft": true,
		"raw": []byte{0, 1, 255}, "due": when, "tags": []any{"plan", "q3"},
		"meta": map[string]any{"owner": "dana"}, "small": uint8(7),
		"vec": []float32{0.5, -0.25, 1},
	}))
	f := must[fields](t)(getGo(s, "docs:1"))
	want := fields{
		"title": "Q3 plan", "pages": int64(12), "score": 0.75, "draft": int64(1),
		"raw": []byte{0, 1, 255}, "due": "2026-10-06T06:30:00Z", "tags": `["plan","q3"]`,
		"meta": `{"owner":"dana"}`, "small": int64(7), "vec": []float32{0.5, -0.25, 1},
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got  %#v\nwant %#v", f, want)
	}
}

// From TestPutKeepsFieldsItIsNotGiven.
func TestPutKeepsFieldsItIsNotGiven(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"title": "draft", "status": "open"}))
	ok(t, putGo(s, "docs:1", fields{"status": "done", "owner": "sam"}))
	f := must[fields](t)(getGo(s, "docs:1"))
	if f["title"] != "draft" || f["status"] != "done" || f["owner"] != "sam" {
		t.Fatalf("after two puts: %v", f)
	}
	ok(t, putGo(s, "docs:1", fields{"owner": nil}))
	f = must[fields](t)(getGo(s, "docs:1"))
	if _, has := f["owner"]; has {
		t.Fatalf("nil didn't clear the field: %v", f)
	}
	ok(t, putGo(s, "docs:2", nil))
	if f := must[fields](t)(getGo(s, "docs:2")); len(f) != 0 {
		t.Fatalf("empty record has fields %v", f)
	}
	// Field names match without regard to case.
	ok(t, putGo(s, "docs:1", fields{"TITLE": "final"}))
	if f := must[fields](t)(getGo(s, "docs:1")); f["title"] != "final" {
		t.Fatalf("TITLE didn't update title: %v", f)
	}
}

type status string

// From TestMoreFieldTypes.
func TestMoreFieldTypes(t *testing.T) {
	s := store.New()
	var nilInt *int
	n := 7
	ok(t, putGo(s, "docs:1", fields{
		"status": status("open"), "tags": []string{"a", "b"}, "meta": map[string]string{"k": "v"},
		"point": struct{ X, Y int }{1, 2}, "missing": nilInt, "count": &n,
	}))
	f := must[fields](t)(getGo(s, "docs:1"))
	want := fields{"status": "open", "tags": `["a","b"]`, "meta": `{"k":"v"}`, "point": `{"X":1,"Y":2}`, "count": int64(7)}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got  %#v\nwant %#v", f, want)
	}
	// The nil pointer still added its field.
	if tb, _ := s.Table("docs"); !slices.Contains(tb.Fields, "missing") {
		t.Fatalf("the table's fields are %q", tb.Fields)
	}
}

// From TestGetAndDeleteMissing, with 0.x's messages, which the command's
// tests look for.
func TestGetAndDeleteMissing(t *testing.T) {
	s := store.New()
	_, err := s.Get("docs:1")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.Delete(nil, "docs:1")
	wantErr(t, err, errs.ErrNotFound)
	ok(t, putGo(s, "docs:1", fields{"a": 1}))
	_, err = s.Get("docs:2")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.Delete(nil, "docs:1")
	ok(t, err)
	_, err = s.Get("docs:1")
	wantErr(t, err, errs.ErrNotFound)
	if err.Error() != "hypercrux: not found: docs:1" {
		t.Errorf("Get gave %q", err)
	}
	_, err = s.Delete(nil, "docs:1")
	wantErr(t, err, errs.ErrNotFound)
	for _, bad := range []string{"nocolon", "Docs:1", "docs:"} {
		_, err := s.Get(bad)
		wantErr(t, err, errs.ErrInvalid)
		_, err = s.Delete(nil, bad)
		wantErr(t, err, errs.ErrInvalid)
	}
	if _, err := s.Get("nocolon"); !strings.Contains(err.Error(), "has no table") {
		t.Errorf("Get(nocolon) gave %q", err)
	}
}

// From TestSchemaChangesShowAtOnce, as the suite has it: a new field shows
// at once.
func TestNewFieldsShowAtOnce(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"a": 1}))
	ok(t, putGo(s, "docs:1", fields{"b": 2}))
	if f := must[fields](t)(getGo(s, "docs:1")); !reflect.DeepEqual(f, fields{"a": int64(1), "b": int64(2)}) {
		t.Fatalf("after a new field: %v", f)
	}
	if tb, _ := s.Table("docs"); !slices.Equal(tb.Fields, []string{"a", "b"}) {
		t.Fatalf("the table's fields are %q", tb.Fields)
	}
}

// From TestRulesForLongKeysAndLinkTypes, without the inserts into 0.x's
// link table.
func TestRulesForLongKeysAndLinkTypes(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", nil))
	long := "docs:" + strings.Repeat("x", rules.MaxKeyLen)
	if err := putGo(s, long, nil); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("stored a %d-byte key: %v", len(long), err)
	}
	if _, err := s.Put(nil, long, nil); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("the store took a %d-byte key: %v", len(long), err)
	}
	ok(t, putGo(s, "docs:2", nil))
	typ := strings.Repeat("ü", 150) // 150 characters, 300 bytes
	must[[]format.Change](t)(s.Link(nil, "docs:1", typ, "docs:2"))
	_, err := s.Link(nil, "docs:1", strings.Repeat("ü", 201), "docs:2")
	wantErr(t, err, errs.ErrInvalid)
	if l := must[[]store.Link](t)(s.Neighbours("docs:2", store.In, "")); len(l) != 1 || l[0].Type != typ {
		t.Fatalf("docs:2 has the links %v", l)
	}
}

// From TestTableNamesLikeHyperCruxsOwn: tables may be called anything,
// including the words 0.x uses in its own names, and a vector of zeros is
// refused in each.
func TestTableNamesLikeHyperCruxsOwn(t *testing.T) {
	s := store.New()
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs", "hc"} {
		if tbl == "hc" {
			ok(t, putGo(s, "hc:1", nil)) // hc alone is a fine name
			continue
		}
		ok(t, putGo(s, tbl+":1", fields{"vec": []float32{1, 0}}))
		ok(t, putGo(s, tbl+":2", fields{"vec": []float32{0, 1}}))
		must[[]format.Change](t)(s.Link(nil, tbl+":1", "next", tbl+":2"))
	}
	for _, tbl := range []string{"keys", "links", "docs_vec", "docs"} {
		_, err := s.Delete(nil, tbl+":2")
		ok(t, err)
		if l := must[[]store.Link](t)(s.Neighbours(tbl+":1", store.Both, "")); len(l) != 0 {
			t.Errorf("%s: deleting a record left its link: %v", tbl, l)
		}
		if _, err := s.Put(nil, tbl+":1", []format.Field{{Name: "vec", Value: value.VectorBits(string(make([]byte, 8)))}}); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s took a vector of zeros: %v", tbl, err)
		}
		if f := must[fields](t)(getGo(s, tbl+":1")); !reflect.DeepEqual(f["vec"], []float32{1, 0}) {
			t.Errorf("%s:1 has %v", tbl, f)
		}
	}
}

// From TestVectors: the first vector sets the table's size, which every
// later one must have, and each table has its own.
func TestVectors(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"title": "no vector yet"}))
	if tb, _ := s.Table("docs"); tb.Vec != -1 || tb.Size != 0 {
		t.Fatalf("docs has %+v", tb)
	}
	ok(t, putGo(s, "docs:2", fields{"vec": []float64{1, 0, 0}}))
	ok(t, putGo(s, "docs:1", fields{"vec": []float32{0, 1, 0}}))
	err := putGo(s, "docs:3", fields{"vec": []float32{1, 0}})
	wantErr(t, err, errs.ErrInvalid)
	if err.Error() != "hypercrux: invalid: table docs holds vectors of 3 values, and docs:3 has 2" {
		t.Errorf("the wrong size gave %q", err)
	}
	if _, err := s.Get("docs:3"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("the put that failed made docs:3: %v", err)
	}
	if f := must[fields](t)(getGo(s, "docs:1")); !reflect.DeepEqual(f["vec"], []float32{0, 1, 0}) {
		t.Fatalf("vec came back as %#v", f["vec"])
	}
	ok(t, putGo(s, "imgs:1", fields{"vec": []float32{1, 2, 3, 4, 5}}))
	ok(t, putGo(s, "docs:2", fields{"vec": nil}))
	if f := must[fields](t)(getGo(s, "docs:2")); f["vec"] != nil {
		t.Fatalf("vec not cleared: %v", f)
	}
	if tb, _ := s.Table("docs"); tb.Size != 3 || tb.Vec != 1 || !slices.Equal(tb.Fields, []string{"title", "vec"}) {
		t.Fatalf("docs has %+v", tb)
	}
	if tb, _ := s.Table("imgs"); tb.Size != 5 || tb.Vec != 0 {
		t.Fatalf("imgs has %+v", tb)
	}
	// A vector field spelt otherwise is the same field.
	ok(t, putGo(s, "docs:4", fields{"VEC": []float32{0, 0, 1}}))
	if f := must[fields](t)(getGo(s, "docs:4")); !reflect.DeepEqual(f, fields{"vec": []float32{0, 0, 1}}) {
		t.Fatalf("docs:4 has %v", f)
	}
	// The first vector field's spelling stays.
	ok(t, putGo(s, "pics:1", fields{"Vec": nil}))
	ok(t, putGo(s, "pics:1", fields{"vec": []float32{1}}))
	if tb, _ := s.Table("pics"); !slices.Equal(tb.Fields, []string{"Vec"}) || tb.Vec != 0 || tb.Size != 1 {
		t.Fatalf("pics has %+v", tb)
	}
}

// From TestDropAndAdoptAfterSchemaChanges, the part about Drop: a dropped
// table's records, vector size and links go with it.
func TestDropTable(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "customer:1", nil))
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		ok(t, putGo(s, k, fields{"title": k, "vec": []float32{1, 2, 3}}))
		must[[]format.Change](t)(s.Link(nil, "customer:1", "owns", k))
	}
	changes := must[[]format.Change](t)(s.Drop(nil, "docs"))
	if len(changes) != 1 || changes[0].Op != format.Drop || changes[0].Table != "docs" {
		t.Fatalf("Drop gave %v", changes)
	}
	if l := must[[]store.Link](t)(s.Neighbours("customer:1", store.Out, "")); len(l) != 0 {
		t.Fatalf("after Drop, customer:1 has the links %v", l)
	}
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		_, err := s.Get(k)
		wantErr(t, err, errs.ErrNotFound)
	}
	if _, ok := s.Table("docs"); ok {
		t.Fatal("docs is still there")
	}
	must[fields](t)(getGo(s, "customer:1"))
	// A new docs table can take vectors of another size, and starts with
	// fields of its own.
	ok(t, putGo(s, "docs:9", fields{"vec": []float32{1, 2}}))
	if tb, _ := s.Table("docs"); tb.Size != 2 || !slices.Equal(tb.Fields, []string{"vec"}) {
		t.Fatalf("the new docs has %+v", tb)
	}
	_, err := s.Drop(nil, "nosuch")
	wantErr(t, err, errs.ErrNotFound)
	if err.Error() != "hypercrux: not found: no record table nosuch" {
		t.Errorf("Drop(nosuch) gave %q", err)
	}
	_, err = s.Drop(nil, "Docs")
	wantErr(t, err, errs.ErrInvalid)
}

// TestFieldOrder pins each table's field order, the order SELECT * shows:
// a put's new fields join in byte order of name, as in 0.x, and keep the
// spelling the first put gave them.
func TestFieldOrder(t *testing.T) {
	s := store.New()
	// SQL.md's example: a first put of title, n and vec adds n, then
	// title, then vec.
	ok(t, putGo(s, "docs:1", fields{"title": "a", "n": 1, "vec": []float32{1}}))
	if tb, _ := s.Table("docs"); !slices.Equal(tb.Fields, []string{"n", "title", "vec"}) || tb.Vec != 2 {
		t.Fatalf("docs has %+v", tb)
	}
	// What 0.x gives, checked against it: upper case before _ before
	// lower case, and later puts' new fields after the first's.
	ok(t, putGo(s, "o:1", fields{"title": 1, "n": 2, "vec": []float32{1}, "Zed": 3, "_x": 4}))
	ok(t, putGo(s, "o:2", fields{"TITLE": 5, "b": 6, "A": 7}))
	if tb, _ := s.Table("o"); !slices.Equal(tb.Fields, []string{"Zed", "_x", "n", "title", "vec", "A", "b"}) {
		t.Fatalf("o has the fields %q", tb.Fields)
	}
	if f := must[fields](t)(getGo(s, "o:2")); !reflect.DeepEqual(f, fields{"A": int64(7), "b": int64(6), "title": int64(5)}) {
		t.Fatalf("o:2 has %v", f)
	}
}

// TestFind looks fields up regardless of case, as SQL does.
func TestFind(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"Title": "a", "n": 1, "VEC": []float32{1}}))
	tb, _ := s.Table("docs")
	if !slices.Equal(tb.Fields, []string{"Title", "VEC", "n"}) {
		t.Fatalf("docs has the fields %q", tb.Fields)
	}
	for name, want := range map[string]int{"title": 0, "TITLE": 0, "Title": 0, "N": 2, "vec": 1, "Vec": tb.Vec,
		"titles": -1, "": -1, "key": -1, "\u212aey": -1} {
		if got := tb.Find(name); got != want {
			t.Errorf("Find(%q) = %d, want %d, in %q", name, got, want, tb.Fields)
		}
	}
	fake := store.Table{Name: "notes", Fields: []string{"body"}, Vec: -1}
	if fake.Find("BODY") != 0 || fake.Find("vec") != -1 {
		t.Error("Find on a table built by hand")
	}
}

// TestANullAddsAField is P5's note: a put that names a new field with a
// null still adds the field, which the command's SQL section depends on.
func TestANullAddsAField(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:3", fields{"title": "Q3 budget"}))
	changes := must[[]format.Change](t)(putGoChanges(s, "docs:3", fields{"phone": nil}))
	if tb, _ := s.Table("docs"); !slices.Equal(tb.Fields, []string{"title", "phone"}) {
		t.Fatalf("docs has the fields %q", tb.Fields)
	}
	if f := must[fields](t)(getGo(s, "docs:3")); !reflect.DeepEqual(f, fields{"title": "Q3 budget"}) {
		t.Fatalf("docs:3 has %v", f)
	}
	want := []format.Change{{Op: format.Put, Key: "docs:3", Fields: []format.Field{{Name: "phone"}}}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("the put gave %v", changes)
	}
}

// TestTheFieldLimit: a table holds 1,999 fields besides its key, as in
// 0.x, whose tables hold 2,000 columns.
func TestTheFieldLimit(t *testing.T) {
	s := store.New()
	many := func(prefix string, n int) []format.Field {
		f := make([]format.Field, n)
		for i := range f {
			f[i] = format.Field{Name: prefix + strconv.Itoa(i), Value: value.Int(int64(i))}
		}
		return f
	}
	_, err := s.Put(nil, "wide:1", many("f", rules.MaxFields))
	ok(t, err)
	_, err = s.Put(nil, "wide:1", many("g", 1))
	wantErr(t, err, errs.ErrInvalid)
	if err.Error() != "hypercrux: invalid: table wide would hold 2000 fields, and a table holds at most 1999" {
		t.Errorf("the 2,000th field gave %q", err)
	}
	if tb, _ := s.Table("wide"); len(tb.Fields) != rules.MaxFields {
		t.Fatalf("the put that failed left %d fields", len(tb.Fields))
	}
	// Every field the table has, spelt otherwise, adds none.
	all := many("F", rules.MaxFields)
	_, err = s.Put(nil, "wide:2", all)
	ok(t, err)
	if r := must[store.Record](t)(s.Get("wide:2")); len(r.Fields) != rules.MaxFields {
		t.Fatalf("wide:2 has %d fields", len(r.Fields))
	}
	_, err = s.Put(nil, "wider:1", many("f", rules.MaxFields+1))
	wantErr(t, err, errs.ErrInvalid)
	if _, ok := s.Table("wider"); ok {
		t.Fatal("the put that failed made the table")
	}
}

// TestAFileMayHoldWiderTables: a change list read from a file keeps the
// format's limit of 65,535 fields, so a file from a build that allows more
// fields than this one opens here, while puts keep this build's limit.
func TestAFileMayHoldWiderTables(t *testing.T) {
	s := store.New()
	names := make([]string, rules.MaxFields+1)
	for i := range names {
		names[i] = "f" + strconv.Itoa(i)
	}
	ok(t, s.Apply(format.Change{Op: format.CreateTable, Table: "wide", Names: names}))
	ok(t, s.Apply(format.Change{Op: format.Put, Key: "wide:1", Fields: []format.Field{{Name: "zz", Value: value.Int(1)}}}))
	if tb, _ := s.Table("wide"); len(tb.Fields) != rules.MaxFields+2 {
		t.Fatalf("wide has %d fields", len(tb.Fields))
	}
	_, err := s.Put(nil, "wide:1", []format.Field{{Name: "more", Value: value.Int(1)}})
	wantErr(t, err, errs.ErrInvalid)
	_, err = s.Put(nil, "wide:1", []format.Field{{Name: "f7", Value: value.Int(7)}, {Name: "ZZ", Value: value.Int(2)}})
	ok(t, err)
	if r := must[store.Record](t)(s.Get("wide:1")); len(r.Fields) != 2 {
		t.Fatalf("wide:1 has %v", r.Fields)
	}
}

// TestTheChanges checks the change lists the writes give, appended to the
// list given, and left as they were when a write fails.
func TestTheChanges(t *testing.T) {
	s := store.New()
	start := []format.Change{{Op: format.Delete, Key: "elsewhere:1"}}
	got := must[[]format.Change](t)(s.Put(slices.Clone(start), "docs:1", []format.Field{
		{Name: "title", Value: value.Text("a")}, {Name: "n", Value: value.Int(1)}, {Name: "Vec", Value: value.Vector([]float32{1, 2})},
	}))
	want := append(slices.Clone(start),
		format.Change{Op: format.CreateTable, Table: "docs"},
		format.Change{Op: format.Put, Key: "docs:1", Fields: []format.Field{
			{Name: "Vec", Value: value.Vector([]float32{1, 2})}, {Name: "n", Value: value.Int(1)}, {Name: "title", Value: value.Text("a")},
		}},
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// The table's spellings, sorted again: Vec, n and title, from vec,
	// TITLE and N.
	got = must[[]format.Change](t)(s.Put(nil, "docs:2", []format.Field{
		{Name: "vec", Value: value.Vector([]float32{3, 4})}, {Name: "TITLE", Value: value.Text("b")}, {Name: "N", Value: value.Null()},
	}))
	want = []format.Change{{Op: format.Put, Key: "docs:2", Fields: []format.Field{
		{Name: "Vec", Value: value.Vector([]float32{3, 4})}, {Name: "n", Value: value.Null()}, {Name: "title", Value: value.Text("b")},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// A put of no fields is still a change.
	got = must[[]format.Change](t)(s.Put(nil, "docs:3", nil))
	if !reflect.DeepEqual(got, []format.Change{{Op: format.Put, Key: "docs:3"}}) {
		t.Fatalf("an empty put gave %v", got)
	}
	got = must[[]format.Change](t)(s.Delete(nil, "docs:3"))
	if !reflect.DeepEqual(got, []format.Change{{Op: format.Delete, Key: "docs:3"}}) {
		t.Fatalf("Delete gave %v", got)
	}
	// Failures leave the list as it was.
	for _, try := range []func(dst []format.Change) ([]format.Change, error){
		func(dst []format.Change) ([]format.Change, error) {
			return s.Put(dst, "docs:4", []format.Field{{Name: "key"}})
		},
		func(dst []format.Change) ([]format.Change, error) {
			return s.Put(dst, "docs:4", []format.Field{{Name: "vec", Value: value.Vector([]float32{1})}})
		},
		func(dst []format.Change) ([]format.Change, error) { return s.Delete(dst, "docs:404") },
		func(dst []format.Change) ([]format.Change, error) { return s.Drop(dst, "nosuch") },
	} {
		got, err := try(slices.Clone(start))
		if err == nil || !reflect.DeepEqual(got, start) {
			t.Errorf("a write that should fail gave %v, %v", got, err)
		}
	}
}

// TestPutLeavesItsSliceAlone: Put sorts and respells a copy of its fields.
func TestPutLeavesItsSliceAlone(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"Title": "a"}))
	f := []format.Field{{Name: "zeta", Value: value.Int(1)}, {Name: "title", Value: value.Text("b")}}
	given := slices.Clone(f)
	changes := must[[]format.Change](t)(s.Put(nil, "docs:1", f))
	if !reflect.DeepEqual(f, given) {
		t.Fatalf("Put changed its fields to %v", f)
	}
	changes[0].Fields[0].Name = "changed" // the change owns its slice
	if !reflect.DeepEqual(f, given) {
		t.Fatalf("the change shares Put's slice: %v", f)
	}
}

// TestReadsShareNothingThatChanges: what a read hands out stays as it was
// through later writes, and appending to it never reaches the store.
func TestReadsShareNothingThatChanges(t *testing.T) {
	s := store.New()
	ok(t, putGo(s, "docs:1", fields{"a": 1, "b": 2, "vec": []float32{1, 2}}))
	tb, _ := s.Table("docs")
	r := must[store.Record](t)(s.Get("docs:1"))
	_ = append(tb.Fields, "x")
	_ = append(r.Fields, store.FieldValue{Index: 9, Value: value.Int(9)})
	_ = append(r.Vec, 9)
	ok(t, putGo(s, "docs:1", fields{"a": 10, "c": 3, "vec": []float32{3, 4}}))
	if !slices.Equal(tb.Fields, []string{"a", "b", "vec"}) || r.Field(0) != value.Int(1) || r.Field(1) != value.Int(2) ||
		!slices.Equal(r.Vec, []float32{1, 2}) {
		t.Fatalf("a later put changed what the reads gave: %q %v", tb.Fields, r)
	}
	tb2, _ := s.Table("docs")
	if !slices.Equal(tb2.Fields, []string{"a", "b", "vec", "c"}) {
		t.Fatalf("docs has the fields %q", tb2.Fields)
	}
	r2 := must[store.Record](t)(s.Get("docs:1"))
	if r2.Field(0) != value.Int(10) || r2.Field(3) != value.Int(3) || !slices.Equal(r2.Vec, []float32{3, 4}) || len(r2.Fields) != 3 {
		t.Fatalf("docs:1 is %v", r2)
	}
}

// TestApply applies change lists as a batch the log has read holds them:
// tables created whole, with their fields and vector size, then puts.
func TestApply(t *testing.T) {
	s := store.New()
	for _, c := range []format.Change{
		{Op: format.CreateTable, Table: "docs", Size: 2, Names: []string{"title", "Vec", "n"}},
		{Op: format.CreateTable, Table: "empty"},
		{Op: format.Put, Key: "docs:1", Fields: []format.Field{
			{Name: "Vec", Value: value.Vector([]float32{1, 0})}, {Name: "title", Value: value.Text("a")}, {Name: "zeta", Value: value.Null()},
		}},
		{Op: format.Put, Key: "empty:1"},
		{Op: format.Put, Key: "docs:2", Fields: []format.Field{{Name: "n", Value: value.Int(2)}}},
		{Op: format.Delete, Key: "docs:2"},
		{Op: format.CreateTable, Table: "gone"},
		{Op: format.Drop, Table: "gone"},
	} {
		if err := s.Apply(c); err != nil {
			t.Fatalf("Apply(%v): %v", c, err)
		}
	}
	tb, _ := s.Table("docs")
	if !reflect.DeepEqual(tb, store.Table{Name: "docs", Fields: []string{"title", "Vec", "n", "zeta"}, Vec: 1, Size: 2}) {
		t.Fatalf("docs is %+v", tb)
	}
	if f := must[fields](t)(getGo(s, "docs:1")); !reflect.DeepEqual(f, fields{"title": "a", "Vec": []float32{1, 0}}) {
		t.Fatalf("docs:1 has %v", f)
	}
	if tb, ok := s.Table("empty"); !ok || len(tb.Fields) != 0 || tb.Size != 0 || tb.Vec != -1 {
		t.Fatalf("empty is %+v, %v", tb, ok)
	}
	must[store.Record](t)(s.Get("empty:1"))
	if _, err := s.Get("docs:2"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("docs:2 is still there: %v", err)
	}
	if _, ok := s.Table("gone"); ok {
		t.Fatal("gone is still there")
	}
}

// TestApplyRefuses holds changes that break FORMAT.md's rules, each of
// which leaves the store as it was.
func TestApplyRefuses(t *testing.T) {
	s := store.New()
	ok(t, s.Apply(format.Change{Op: format.CreateTable, Table: "docs", Size: 2, Names: []string{"title", "vec"}}))
	ok(t, s.Apply(format.Change{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "title", Value: value.Text("a")}}}))
	ok(t, s.Apply(format.Change{Op: format.Put, Key: "docs:3"}))
	cites := format.Change{Op: format.Link, Key: "docs:1", Type: "cites", To: "docs:3"}
	ok(t, s.Apply(cites))
	link := func(op format.Op, from, typ, to string) format.Change {
		return format.Change{Op: op, Key: from, Type: typ, To: to}
	}
	tooMany := make([]string, rules.FormatMaxFields+1)
	for i := range tooMany {
		tooMany[i] = "f" + strconv.Itoa(i)
	}
	put := func(f ...format.Field) format.Change { return format.Change{Op: format.Put, Key: "docs:1", Fields: f} }
	field := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	for _, c := range []struct {
		c      format.Change
		target error
	}{
		{format.Change{Op: format.CreateTable, Table: "docs"}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "Docs"}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Names: []string{"key"}}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Names: []string{"a", "A"}}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Size: 3, Names: []string{"a"}}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Size: rules.MaxDims + 1, Names: []string{"vec"}}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Size: -1, Names: []string{"vec"}}, errs.ErrInvalid},
		{format.Change{Op: format.CreateTable, Table: "t", Names: tooMany}, errs.ErrInvalid},
		{format.Change{Op: format.Put, Key: "notes:1"}, errs.ErrInvalid},
		{format.Change{Op: format.Put, Key: "Docs:1"}, errs.ErrInvalid},
		{put(field("title", value.Int(1)), field("n", value.Int(1))), errs.ErrInvalid},
		{put(field("n", value.Int(1)), field("n", value.Int(2))), errs.ErrInvalid},
		{put(field("A", value.Int(1)), field("a", value.Int(2))), errs.ErrInvalid},
		{put(field("Title", value.Int(1))), errs.ErrInvalid},
		{put(field("has space", value.Int(1))), errs.ErrInvalid},
		{put(field("title", value.Text("\xff"))), errs.ErrInvalid},
		{put(field("n", value.Real(math.Inf(1)))), errs.ErrInvalid},
		{put(field("n", value.Vector([]float32{1}))), errs.ErrInvalid},
		{put(field("vec", value.Vector([]float32{1}))), errs.ErrInvalid},
		{put(field("vec", value.Vector([]float32{0, 0}))), errs.ErrInvalid},
		{put(field("vec", value.Bytes("ab"))), errs.ErrInvalid},
		{format.Change{Op: format.Delete, Key: "docs:2"}, errs.ErrNotFound},
		{format.Change{Op: format.Delete, Key: "docs"}, errs.ErrInvalid},
		{format.Change{Op: format.Drop, Table: "notes"}, errs.ErrNotFound},
		{format.Change{Op: format.Drop, Table: "hc_x"}, errs.ErrInvalid},
		{link(format.Link, "docs:1", "x", "docs:9"), errs.ErrNotFound},
		{link(format.Link, "docs:9", "x", "docs:1"), errs.ErrNotFound},
		{link(format.Link, "docs:1", "", "docs:3"), errs.ErrInvalid},
		{link(format.Link, "docs:1", strings.Repeat("t", 201), "docs:3"), errs.ErrInvalid},
		{link(format.Link, "docs:1", "\x00x", "docs:3"), errs.ErrInvalid},
		{link(format.Link, "Docs:1", "x", "docs:3"), errs.ErrInvalid},
		{link(format.Link, "docs:1", "x", "docs"), errs.ErrInvalid},
		{cites, errs.ErrInvalid}, // there already
		{link(format.Unlink, "docs:1", "x", "docs:3"), errs.ErrNotFound},
		{link(format.Unlink, "docs:3", "cites", "docs:1"), errs.ErrNotFound},
		{link(format.Unlink, "nosuch:1", "cites", "docs:3"), errs.ErrNotFound},
		{link(format.Unlink, "docs:1", "", "docs:3"), errs.ErrInvalid},
		{link(format.Unlink, "docs:1", "cites", "Docs:3"), errs.ErrInvalid},
		{link(format.Unlink, "docs:1", "\xff", "docs:3"), errs.ErrInvalid},
		{format.Change{}, errs.ErrInvalid},
		{format.Change{Op: 99}, errs.ErrInvalid},
	} {
		if err := s.Apply(c.c); !errors.Is(err, c.target) {
			t.Errorf("Apply(%v) = %v, want %v", c.c, err, c.target)
		}
	}
	tb, _ := s.Table("docs")
	if !reflect.DeepEqual(tb, store.Table{Name: "docs", Fields: []string{"title", "vec"}, Vec: 1, Size: 2}) {
		t.Fatalf("docs is %+v", tb)
	}
	if f := must[fields](t)(getGo(s, "docs:1")); !reflect.DeepEqual(f, fields{"title": "a"}) {
		t.Fatalf("docs:1 has %v", f)
	}
	if _, ok := s.Table("t"); ok {
		t.Fatal("a create that failed made its table")
	}
	if l, err := s.Neighbours("docs:1", store.Both, ""); err != nil || !reflect.DeepEqual(l, []store.Link{{From: "docs:1", Type: "cites", To: "docs:3"}}) {
		t.Fatalf("docs:1 has the links %v, %v", l, err)
	}
}

// TestTheStoreKeepsItsOwnStrings: a store copies the text it keeps, so it
// never holds on to a decoded batch that its values were sliced from.
func TestTheStoreKeepsItsOwnStrings(t *testing.T) {
	batch := "docs" + "title" + "docs:1" + "Q3 plan" + "\x00\x01"
	sub := func(from, to int) string { return batch[from:to] }
	s := store.New()
	ok(t, s.Apply(format.Change{Op: format.CreateTable, Table: sub(0, 4), Names: []string{sub(4, 9)}}))
	ok(t, s.Apply(format.Change{Op: format.Put, Key: sub(9, 15), Fields: []format.Field{
		{Name: "raw", Value: value.Bytes(sub(22, 24))}, {Name: sub(4, 9), Value: value.Text(sub(15, 22))},
	}}))
	tb, _ := s.Table("docs")
	r := must[store.Record](t)(s.Get("docs:1"))
	start, end := uintptr(unsafe.Pointer(unsafe.StringData(batch))), uintptr(unsafe.Pointer(unsafe.StringData(batch)))+uintptr(len(batch))
	for _, kept := range []string{tb.Name, tb.Fields[0], tb.Fields[1], r.Key, r.Fields[0].Value.Raw(), r.Fields[1].Value.Raw()} {
		if p := uintptr(unsafe.Pointer(unsafe.StringData(kept))); start <= p && p < end {
			t.Errorf("the store keeps %q inside the batch", kept)
		}
	}
}

// TestWhatComesLater checks that the part of Reader a later task writes
// says so.
func TestWhatComesLater(t *testing.T) {
	s := store.New()
	_, err := s.Nearest("docs", []float32{1}, 1, nil)
	wantErr(t, err, errors.ErrUnsupported)
}
