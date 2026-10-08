// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// scanned is one record as 0.x's Scan gives it: its key and its fields,
// with the vector left out.
type scanned struct {
	Key    string
	Fields fields
}

// scanGo does what the public package's Scan does: the store's Scan, which
// checks the prefix, then 0.x's check that the limit isn't negative, then
// at most limit records, or all of them when it's 0, each without its
// vector, and nil when there are none, as 0.x gives.
func scanGo(r store.Reader, prefix, after string, limit int) ([]scanned, error) {
	c, err := r.Scan(prefix, after)
	if err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: limit %d", errs.ErrInvalid, limit)
	}
	var out []scanned
	for limit == 0 || len(out) < limit {
		rec, more := c.Next()
		if !more {
			break
		}
		tb, _ := r.Table(rec.Key[:strings.IndexByte(rec.Key, ':')])
		f := fields{}
		for _, fv := range rec.Fields {
			f[tb.Fields[fv.Index]] = fv.Value.Go()
		}
		out = append(out, scanned{rec.Key, f})
	}
	return out, nil
}

// fromZerox turns 0.x's records into scanned ones.
func fromZerox(recs []hc.Record) []scanned {
	var out []scanned
	for _, r := range recs {
		out = append(out, scanned{r.Key, fields(r.Fields)})
	}
	return out
}

func openZerox(t *testing.T) *hc.DB {
	t.Helper()
	db, err := hc.Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestScan is 0.x's TestScan and the suite's Scan, with more cases, run on
// 0.x and on the store side by side: both must give each case's records,
// in byte order of key, with their fields and without their vectors, and
// the same errors with the same messages. The cases past 0.x's own start
// after keys from outside the prefix and from other tables, after strings
// that aren't keys, and from prefixes that end inside a character.
func TestScan(t *testing.T) {
	db := openZerox(t)
	s := store.New()
	for _, k := range []string{"docs:b", "docs:a", "docs:c1", "docs:c2", "docs:d", "notes:a", "docs:é", "docs:C", "docs:c"} {
		ok(t, db.Put(k, hc.Fields{"k": k, "vec": hc.Vector{1, 2}}))
		ok(t, putGo(s, k, fields{"k": k, "vec": []float32{1, 2}}))
	}
	for _, c := range []struct {
		prefix, after string
		limit         int
		want          string
	}{
		// 0.x's, with docs:C, docs:c and docs:é added to the table.
		{"docs:", "", 0, "docs:C docs:a docs:b docs:c docs:c1 docs:c2 docs:d docs:é"},
		{"docs:c", "", 0, "docs:c docs:c1 docs:c2"},
		{"docs:", "docs:b", 2, "docs:c docs:c1"},
		{"docs:", "", 1, "docs:C"},
		{"notes:", "", 0, "notes:a"},
		{"other:", "", 0, ""},
		{"docs:zz", "", 0, ""},
		// after, inside and outside the prefix.
		{"docs:c", "docs:c", 0, "docs:c1 docs:c2"},
		{"docs:c", "docs:c1", 0, "docs:c2"},
		{"docs:c", "docs:b", 0, "docs:c docs:c1 docs:c2"},
		{"docs:c", "docs:d", 0, ""},
		{"docs:", "docs:d", 0, "docs:é"},
		{"docs:", "docs:é", 0, ""},
		{"docs:", "docs:\xc3", 0, "docs:é"},
		{"docs:\xc3", "", 0, "docs:é"},
		{"docs:", "a", 0, "docs:C docs:a docs:b docs:c docs:c1 docs:c2 docs:d docs:é"},
		{"docs:", "docs", 3, "docs:C docs:a docs:b"},
		{"docs:", "notes:a", 0, ""},
		{"notes:", "docs:z", 0, "notes:a"},
		{"docs:", "docs:c\xff", 0, "docs:d docs:é"},
		{"docs:", "", 100, "docs:C docs:a docs:b docs:c docs:c1 docs:c2 docs:d docs:é"},
		{"docs:a:b", "", 0, ""},
	} {
		want := fromZerox(must[[]hc.Record](t)(db.Scan(c.prefix, c.after, c.limit)))
		got := must[[]scanned](t)(scanGo(s, c.prefix, c.after, c.limit))
		if keys := keysOf(got); keys != c.want || !reflect.DeepEqual(got, want) {
			t.Errorf("Scan(%q, %q, %d) = %q, where 0.x gives %q and the case %q", c.prefix, c.after, c.limit, keys, keysOf(want), c.want)
		}
		for _, r := range got {
			if r.Fields["k"] != r.Key || r.Fields["vec"] != nil {
				t.Fatalf("Scan(%q) gave %s with the fields %v", c.prefix, r.Key, r.Fields)
			}
		}
	}
	for _, c := range []struct {
		prefix string
		limit  int
	}{
		{"docs", 0}, {"", 0}, {"Docs:", 0}, {":", 0}, {":docs", 0}, {"hc_x:", 0}, {"sqlite_x:", 0}, {"a-b:", 0},
		{strings.Repeat("a", 64) + ":", 0}, {"docs", -1}, {"Docs:", -1}, {"docs:", -1}, {"other:", -2},
	} {
		_, zeroxErr := db.Scan(c.prefix, "", c.limit)
		_, err := scanGo(s, c.prefix, "", c.limit)
		if !errors.Is(err, errs.ErrInvalid) || !errors.Is(zeroxErr, hc.ErrInvalid) || err.Error() != zeroxErr.Error() {
			t.Errorf("Scan(%q, %d) gave %v, and 0.x %v", c.prefix, c.limit, err, zeroxErr)
		}
	}
	// A table with no records left gives none, as one that doesn't exist does.
	ok(t, db.Delete("notes:a"))
	must[[]format.Change](t)(s.Delete(nil, "notes:a"))
	if got := must[[]scanned](t)(scanGo(s, "notes:", "", 0)); got != nil || must[[]hc.Record](t)(db.Scan("notes:", "", 0)) != nil {
		t.Errorf("a scan of an empty table gave %v", got)
	}
}

func keysOf(recs []scanned) string {
	var keys []string
	for _, r := range recs {
		keys = append(keys, r.Key)
	}
	return strings.Join(keys, " ")
}

// TestScanInsideATransaction: a scan through a transaction sees the
// transaction's changes, in 0.x's Update and through the store's Tx alike,
// and once it rolls back a scan sees none of them. A cursor opened before
// the changes carries on through them, and gives no more once the
// transaction has ended.
func TestScanInsideATransaction(t *testing.T) {
	db := openZerox(t)
	s := store.New()
	for _, k := range []string{"docs:1", "docs:2", "docs:3", "docs:4"} {
		ok(t, db.Put(k, hc.Fields{"k": k}))
		ok(t, putGo(s, k, fields{"k": k}))
	}
	tx := must[*store.Tx](t)(s.Begin())
	defer tx.Rollback()
	early := must[store.Cursor](t)(tx.Scan("docs:", ""))
	if rec, more := early.Next(); !more || rec.Key != "docs:1" {
		t.Fatalf("the cursor gave %v, %v", rec.Key, more)
	}
	var zeroxInside []scanned
	rollBack := errors.New("roll back")
	err := db.Update(func(ztx *hc.Tx) error {
		ok(t, ztx.Put("docs:0", hc.Fields{"k": "docs:0"}))
		ok(t, ztx.Put("docs:25", hc.Fields{"k": "docs:25"}))
		ok(t, ztx.Delete("docs:3"))
		ok(t, ztx.Put("notes:1", hc.Fields{"k": "notes:1"}))
		zeroxInside = fromZerox(must[[]hc.Record](t)(ztx.Scan("docs:", "docs:0", 0)))
		return rollBack
	})
	if !errors.Is(err, rollBack) {
		t.Fatal(err)
	}
	for _, w := range []struct {
		key string
		f   []format.Field
	}{{"docs:0", text("k", "docs:0")}, {"docs:25", text("k", "docs:25")}, {"docs:3", nil}, {"notes:1", text("k", "notes:1")}} {
		if w.f == nil {
			ok(t, tx.Delete(w.key))
		} else {
			ok(t, tx.Put(w.key, w.f))
		}
	}
	inside := must[[]scanned](t)(scanGo(tx, "docs:", "docs:0", 0))
	if keysOf(inside) != "docs:1 docs:2 docs:25 docs:4" || !reflect.DeepEqual(inside, zeroxInside) {
		t.Fatalf("inside the transaction the store gives %q, and 0.x %q", keysOf(inside), keysOf(zeroxInside))
	}
	// The early cursor carries on after docs:1, through the changes.
	var rest []string
	for rec, more := early.Next(); more && len(rest) < 2; rec, more = early.Next() {
		rest = append(rest, rec.Key)
	}
	if strings.Join(rest, " ") != "docs:2 docs:25" {
		t.Fatalf("the cursor from before the changes gave %q", rest)
	}
	tx.Rollback()
	if rec, more := early.Next(); more {
		t.Fatalf("the cursor gave %s after the rollback", rec.Key)
	}
	var after []scanned
	ok(t, s.Read(func(r store.Reader) error {
		var err error
		after, err = scanGo(r, "docs:", "", 0)
		return err
	}))
	if want := fromZerox(must[[]hc.Record](t)(db.Scan("docs:", "", 0))); keysOf(after) != "docs:1 docs:2 docs:3 docs:4" || !reflect.DeepEqual(after, want) {
		t.Fatalf("after the rollback the store gives %q, and 0.x %q", keysOf(after), keysOf(want))
	}
}

func text(name, v string) []format.Field { return []format.Field{{Name: name, Value: value.Text(v)}} }

// TestACursorThroughRollbacks: a cursor through a transaction finds its
// place again after a statement is taken back, however the statement
// changed the table's keys. Each case has the cursor give a record after
// the statement's change, so it found its place then, and the rollback
// moves the keys before its place in its block: a key the statement put in
// goes, and a key it took out comes back. A cursor opened on a table the
// statement dropped gives the table's records once the drop is taken back.
func TestACursorThroughRollbacks(t *testing.T) {
	s := store.New()
	for _, k := range []string{"docs:1", "docs:3", "docs:5", "docs:7", "docs:9", "notes:1", "notes:2"} {
		ok(t, putGo(s, k, fields{"k": k}))
	}
	tx := must[*store.Tx](t)(s.Begin())
	defer tx.Rollback()
	next := func(c store.Cursor, want string) {
		t.Helper()
		rec, more := c.Next()
		if more != (want != "") || rec.Key != want {
			t.Fatalf("the cursor gave %q, %v, where it should give %q", rec.Key, more, want)
		}
	}
	c := must[store.Cursor](t)(tx.Scan("docs:", ""))
	next(c, "docs:1")
	m := tx.Mark()
	ok(t, tx.Put("docs:2", nil))
	next(c, "docs:2")
	tx.RollbackTo(m) // docs:2 goes
	next(c, "docs:3")
	m = tx.Mark()
	ok(t, tx.Delete("docs:5"))
	next(c, "docs:7")
	tx.RollbackTo(m) // docs:5 comes back, before docs:7
	next(c, "docs:9")
	next(c, "")

	m = tx.Mark()
	ok(t, tx.Drop("notes"))
	notes := must[store.Cursor](t)(tx.Scan("notes:", ""))
	tx.RollbackTo(m) // notes comes back
	next(notes, "notes:1")
	next(notes, "notes:2")
	next(notes, "")
}

// TestDropFindsItsOwnRecords: a drop takes its table's records, which it
// finds through the table's keys, and leaves every other table's, the ones
// whose keys sort among them included, in 0.x and in the store alike.
func TestDropFindsItsOwnRecords(t *testing.T) {
	db := openZerox(t)
	s := store.New()
	for _, k := range []string{"doc:1", "docs:1", "docs:2", "docs_x:1", "docsa:1", "do:1", "docs:3"} {
		ok(t, db.Put(k, hc.Fields{"k": k}))
		ok(t, putGo(s, k, fields{"k": k}))
	}
	ok(t, db.Drop("docs"))
	must[[]format.Change](t)(s.Drop(nil, "docs"))
	for _, tbl := range []string{"doc", "docs", "docs_x", "docsa", "do"} {
		want := fromZerox(must[[]hc.Record](t)(db.Scan(tbl+":", "", 0)))
		got := must[[]scanned](t)(scanGo(s, tbl+":", "", 0))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after the drop, table %s gives %q, and 0.x %q", tbl, keysOf(got), keysOf(want))
		}
	}
	for _, k := range []string{"docs:1", "docs:2", "docs:3"} {
		_, err := s.Get(k)
		wantErr(t, err, errs.ErrNotFound)
	}
	for _, k := range []string{"doc:1", "docs_x:1", "docsa:1", "do:1"} {
		must[store.Record](t)(s.Get(k))
	}
}
