// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests cover problems found in review before the first release, so
// they stay fixed.

// Tables may be called anything, including the words HyperCrux uses in its
// own trigger names, without one table's triggers standing in for another's.
func TestTableNamesLikeHyperCruxsOwn(t *testing.T) {
	db, _ := openTemp(t)
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
		if _, err := db.Exec(`INSERT INTO "` + tbl + `" (key) VALUES ('other:9')`); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s took a key from another table: %v", tbl, err)
		}
		if _, err := db.Exec(`UPDATE "` + tbl + `" SET vec = zeroblob(8) WHERE key = '` + tbl + `:1'`); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s took a vector of zeros: %v", tbl, err)
		}
		if hits := must[[]Hit](t)(db.Nearest(tbl, Vector{1, 0}, 5, "")); len(hits) != 1 {
			t.Errorf("%s: Nearest found %v", tbl, hits)
		}
	}
	checkOK(t, db)
}

// REPLACE on another unique column removes a row, and that row's key and
// links must go with it.
func TestReplaceOnAnotherUniqueColumn(t *testing.T) {
	db, _ := openTemp(t)
	must[any](t)(db.Exec(`CREATE TABLE customer (key TEXT PRIMARY KEY, email TEXT UNIQUE)`))
	ok(t, db.Adopt("customer"))
	ok(t, db.Put("customer:1", Fields{"email": "a@example.com"}))
	ok(t, db.Put("docs:1", nil))
	ok(t, db.Link("customer:1", "owns", "docs:1"))
	must[any](t)(db.Exec(`INSERT OR REPLACE INTO customer (key, email) VALUES ('customer:3', 'a@example.com')`))
	if _, err := db.Get("customer:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("customer:1 should be gone: %v", err)
	}
	wantErr(t, db.Link("customer:1", "owns", "docs:1"), ErrNotFound)
	if links := must[[]Link](t)(db.Neighbours("docs:1", In, "")); len(links) != 0 {
		t.Fatalf("the replaced record's link stayed: %v", links)
	}
	checkOK(t, db)

	// The same through Put, with the conflict rule in the table itself.
	must[any](t)(db.Exec(`CREATE TABLE member (key TEXT PRIMARY KEY, email TEXT UNIQUE ON CONFLICT REPLACE)`))
	ok(t, db.Adopt("member"))
	ok(t, db.Put("member:1", Fields{"email": "b@example.com"}))
	ok(t, db.Link("member:1", "owns", "docs:1"))
	ok(t, db.Put("member:2", Fields{"email": "b@example.com"}))
	if links := must[[]Link](t)(db.Neighbours("docs:1", In, "")); len(links) != 0 {
		t.Fatalf("the replaced member's link stayed: %v", links)
	}
	checkOK(t, db)
}

// Schema changes show at once, from this program and from others.
func TestSchemaChangesShowAtOnce(t *testing.T) {
	db, path := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"a": 1}))
	must[Fields](t)(db.Get("docs:1"))
	ok(t, db.Put("docs:1", Fields{"b": 2}))
	if f := must[Fields](t)(db.Get("docs:1")); !reflect.DeepEqual(f, Fields{"a": int64(1), "b": int64(2)}) {
		t.Fatalf("after a new column: %v", f)
	}
	ok(t, db.Update(func(tx *Tx) error {
		ok(t, tx.Put("docs:2", Fields{"c": 3}))
		if f := must[Fields](t)(tx.Get("docs:2")); f["c"] != int64(3) {
			t.Errorf("inside the transaction, Get missed the new column: %v", f)
		}
		return nil
	}))
	// Another program drops and renames columns.
	other := must[*DB](t)(Open(path))
	defer other.Close()
	must[any](t)(other.Exec(`ALTER TABLE docs DROP COLUMN a`))
	if f := must[Fields](t)(db.Get("docs:1")); !reflect.DeepEqual(f, Fields{"b": int64(2)}) {
		t.Fatalf("after another program dropped a column: %v", f)
	}
	must[any](t)(other.Exec(`ALTER TABLE docs RENAME COLUMN b TO title`))
	if f := must[Fields](t)(db.Get("docs:1")); !reflect.DeepEqual(f, Fields{"title": int64(2)}) {
		t.Fatalf("after another program renamed a column: %v", f)
	}
	ok(t, db.Put("docs:1", Fields{"title": "x", "a": "back"}))
	checkOK(t, db)
}

// Everything works with a single connection, as some programs configure.
func TestOneConnection(t *testing.T) {
	db, _ := openTemp(t)
	db.SQL().SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		done <- db.Update(func(tx *Tx) error {
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

// A schema change in a transaction that rolls back leaves nothing behind,
// not even in HyperCrux's memory of the schema.
func TestRolledBackSchemaChange(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"a": 1}))
	boom := errors.New("boom")
	err := db.Update(func(tx *Tx) error {
		must[any](t)(tx.Exec(`ALTER TABLE docs ADD COLUMN foo`))
		ok(t, tx.Put("docs:1", Fields{"foo": 1}))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	must[any](t)(db.Exec(`CREATE INDEX docs_a ON docs (a)`))
	ok(t, db.Put("docs:2", Fields{"foo": 2}))
	if f := must[Fields](t)(db.Get("docs:2")); f["foo"] != int64(2) {
		t.Fatalf("got %v", f)
	}
}

// NaN or infinity can only get into the file through plain SQL. Searches
// leave such vectors out, distance() refuses them, and Check reports them.
func TestNonFiniteVectorsStayOutOfSearch(t *testing.T) {
	db, _ := openTemp(t)
	for i, v := range []Vector{{1, 0}, {0.9, 0.1}, {0.5, 0.5}, {0, 1}} {
		ok(t, db.Put("docs:"+string(rune('a'+i)), Fields{"vec": v}))
	}
	nan := Vector{1, 1}.Bytes()
	copy(nan[0:4], []byte{0, 0, 0xc0, 0x7f}) // a quiet NaN
	inf := Vector{1, 1}.Bytes()
	copy(inf[4:8], []byte{0, 0, 0x80, 0x7f}) // +Inf
	must[any](t)(db.Exec(`INSERT INTO docs (key, vec) VALUES ('docs:nan', ?), ('docs:inf', ?)`, nan, inf))
	hits := must[[]Hit](t)(db.Nearest("docs", Vector{1, 0}, 10, ""))
	var keys []string
	for _, h := range hits {
		if math.IsNaN(h.Distance) {
			t.Fatalf("a NaN distance: %v", hits)
		}
		keys = append(keys, h.Key)
	}
	if strings.Join(keys, " ") != "docs:a docs:b docs:c docs:d" {
		t.Fatalf("Nearest gave %v", hits)
	}
	var d float64
	if err := db.QueryRow(`SELECT distance(vec, '[1, 0]') FROM docs WHERE key = 'docs:nan'`).Scan(&d); err == nil {
		t.Fatal("distance() of a NaN vector worked")
	}
	rep := must[Report](t)(db.Check())
	if all := strings.Join(rep.Problems, "\n"); !strings.Contains(all, "docs:nan") || !strings.Contains(all, "docs:inf") {
		t.Fatalf("Check didn't report the non-finite vectors: %s", all)
	}
}

func TestDropAndAdoptAfterSchemaChanges(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("customer:1", nil))
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		ok(t, db.Put(k, Fields{"title": k, "vec": Vector{1, 2, 3}}))
		ok(t, db.Link("customer:1", "owns", k))
	}

	// Rebuilding a table the way SQLite's documentation describes loses
	// the triggers; Adopt puts them back and forgets the rows that went.
	for _, s := range []string{
		`CREATE TABLE docs_new (key TEXT PRIMARY KEY, title TEXT, vec BLOB)`,
		`INSERT INTO docs_new SELECT key, title, vec FROM docs WHERE key <> 'docs:2'`,
		`DROP TABLE docs`,
		`ALTER TABLE docs_new RENAME TO docs`,
	} {
		must[any](t)(db.Exec(s))
	}
	if rep := must[Report](t)(db.Check()); rep.OK() {
		t.Fatal("Check missed the rebuilt table")
	}
	ok(t, db.Adopt("docs"))
	checkOK(t, db)
	if links := must[[]Link](t)(db.Neighbours("customer:1", Out, "")); len(links) != 2 {
		t.Fatalf("after Adopt: %v", links)
	}
	if _, err := db.Exec(`INSERT INTO docs (key) VALUES ('notes:1')`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the rebuilt table's rules aren't back: %v", err)
	}

	// Drop removes a record table and every link into it.
	ok(t, db.Drop("docs"))
	if links := must[[]Link](t)(db.Neighbours("customer:1", Out, "")); len(links) != 0 {
		t.Fatalf("after Drop: %v", links)
	}
	checkOK(t, db)
	// A new docs table can take vectors of another size.
	ok(t, db.Put("docs:9", Fields{"vec": Vector{1, 2}}))

	// A table dropped with plain SQL leaves keys and links, and Drop clears them.
	ok(t, db.Link("customer:1", "owns", "docs:9"))
	must[any](t)(db.Exec(`DROP TABLE docs`))
	if rep := must[Report](t)(db.Check()); rep.OK() {
		t.Fatal("Check missed the dropped table")
	}
	ok(t, db.Drop("docs"))
	checkOK(t, db)
	if links := must[[]Link](t)(db.Neighbours("customer:1", Out, "")); len(links) != 0 {
		t.Fatalf("links to the dropped table stayed: %v", links)
	}

	must[any](t)(db.Exec(`CREATE TABLE plain (a)`))
	wantErr(t, db.Drop("plain"), ErrInvalid)
	wantErr(t, db.Drop("nosuch"), ErrNotFound)
	wantErr(t, db.Adopt("docs"), ErrNotFound)
}

func TestVectorAsQueryArgument(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", Fields{"vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"vec": Vector{0, 1}}))
	var key string
	ok(t, db.QueryRow(`SELECT key FROM docs ORDER BY distance(vec, ?) LIMIT 1`, Vector{0.1, 0.9}).Scan(&key))
	if key != "docs:2" {
		t.Fatalf("got %s", key)
	}
}

func TestStrictTables(t *testing.T) {
	db, _ := openTemp(t)
	must[any](t)(db.Exec(`CREATE TABLE notes (key TEXT PRIMARY KEY, body TEXT) STRICT`))
	ok(t, db.Adopt("notes"))
	ok(t, db.Put("notes:1", Fields{"body": "hello", "pages": 3, "vec": Vector{1, 2}}))
	if f := must[Fields](t)(db.Get("notes:1")); f["pages"] != int64(3) || f["body"] != "hello" {
		t.Fatalf("got %v", f)
	}
	checkOK(t, db)
}

type status string

func TestMoreFieldTypes(t *testing.T) {
	db, _ := openTemp(t)
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

func TestRulesForLongKeysAndLinkTypes(t *testing.T) {
	db, _ := openTemp(t)
	ok(t, db.Put("docs:1", nil))
	long := "docs:" + strings.Repeat("x", MaxKeyLen)
	if _, err := db.Exec(`INSERT INTO docs (key) VALUES (?)`, long); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plain SQL stored a %d-byte key: %v", len(long), err)
	}
	ok(t, db.Put("docs:2", nil))
	typ := strings.Repeat("ü", 150) // 150 characters, 300 bytes
	ok(t, db.Link("docs:1", typ, "docs:2"))
	must[any](t)(db.Exec(`INSERT INTO hc_links VALUES ('docs:2', ?, 'docs:1')`, typ))
	tooLong := strings.Repeat("ü", 201)
	wantErr(t, db.Link("docs:1", tooLong, "docs:2"), ErrInvalid)
	if _, err := db.Exec(`INSERT INTO hc_links VALUES ('docs:2', ?, 'docs:1')`, tooLong); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plain SQL stored a 201-character link type: %v", err)
	}
	checkOK(t, db)
}

func TestKeysCompareExactly(t *testing.T) {
	db, _ := openTemp(t)
	must[any](t)(db.Exec(`CREATE TABLE notes (key TEXT PRIMARY KEY COLLATE NOCASE, body TEXT)`))
	ok(t, db.Adopt("notes"))
	ok(t, db.Put("notes:a", Fields{"body": "x"}))
	if _, err := db.Exec(`UPDATE notes SET key = 'NOTES:A' WHERE key = 'notes:a'`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a key changed case: %v", err)
	}
	checkOK(t, db)
}

func TestOpenNeedsAFile(t *testing.T) {
	for _, p := range []string{":memory:", "file::memory:", "", "a?b"} {
		if _, err := Open(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q) = %v", p, err)
		}
	}
	// A directory that doesn't exist fails cleanly.
	if _, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir.db")); err == nil {
		t.Error("opened a file in a missing directory")
	}
}

func TestMissingKeyColumnIsNotFound(t *testing.T) {
	db, _ := openTemp(t)
	must[any](t)(db.Exec(`CREATE TABLE plain (a)`))
	_, err := db.Get("plain:1")
	wantErr(t, err, ErrNotFound)
	wantErr(t, db.Delete("plain:1"), ErrNotFound)
	recs, err := db.Scan("plain:", "", 0)
	if err != nil || len(recs) != 0 {
		t.Fatalf("Scan of a plain table: %v %v", recs, err)
	}
}
