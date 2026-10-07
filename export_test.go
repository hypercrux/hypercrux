// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Values that are easy to get wrong, alongside random ones.
var (
	odd64 = []float64{math.Copysign(0, -1), 0, 1, -1, 0.1, 1e21, 1e-7, 123456789, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308, 1 << 53, 1<<53 + 2}
	oddInts = []int64{0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1}
	oddF32  = []float32{float32(math.Copysign(0, -1)), math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32,
		1.17549435e-38, 1e-7, 0.1, 1, 16777217}
	oddText = []string{"", " ", "\x00", "a\x00b", "\"", "\\", "\n\r\t\x01\x1f\x7f", "\u2028\u2029", "é", "e\u0301", "😀",
		"\ufffd", "שלום", "مرحبا", "中文", `{"base64":"AA=="}`, "[1,2]", "null", "1.0", "-0", strings.Repeat("x", 3000)}
)

func randomField(r *rand.Rand) any {
	switch r.IntN(9) {
	case 0:
		return oddInts[r.IntN(len(oddInts))]
	case 1:
		return int64(r.Uint64())
	case 2:
		return odd64[r.IntN(len(odd64))]
	case 3:
		for {
			if f := math.Float64frombits(r.Uint64()); finite(f) {
				return f
			}
		}
	case 4, 5:
		return oddText[r.IntN(len(oddText))]
	case 6:
		b := make([]byte, r.IntN(40))
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		return b
	case 7:
		return []byte{}
	}
	return nil
}

func randomVec(r *rand.Rand, dims int) Vector {
	for {
		v := make(Vector, dims)
		for i := range v {
			if r.IntN(3) == 0 {
				v[i] = oddF32[r.IntN(len(oddF32))]
				continue
			}
			for {
				if v[i] = math.Float32frombits(r.Uint32()); finite(float64(v[i])) {
					break
				}
			}
		}
		if checkVector(v) == nil {
			return v
		}
	}
}

// fillRandom puts random records and links into db, through Put, Delete,
// Link and plain SQL, so the tables' fields come in different orders.
func fillRandom(t *testing.T, db *DB, r *rand.Rand) {
	t.Helper()
	tables := []string{"docs", "customer", "a_1", "z9"}[:1+r.IntN(4)]
	fields := []string{"title", "Score", "body", "n", "when_", "x1", "flag"}
	dims := map[string]int{}
	var keys []string
	for i := 0; i < 20+r.IntN(40); i++ {
		tbl := tables[r.IntN(len(tables))]
		// Keys can't hold NUL or run past 1,024 bytes, so leave those out.
		key := tbl + ":" + strings.ReplaceAll(oddText[r.IntN(len(oddText)-1)], "\x00", "0") + strconv.Itoa(r.IntN(30))
		f := Fields{}
		for n := r.IntN(4); n > 0; n-- {
			f[fields[r.IntN(len(fields))]] = randomField(r)
		}
		if tbl != "customer" && r.IntN(2) == 0 {
			if dims[tbl] == 0 {
				dims[tbl] = 1 + r.IntN(5)
			}
			f["vec"] = randomVec(r, dims[tbl])
		}
		ok(t, db.Put(key, f))
		keys = append(keys, key)
	}
	for i := 0; i < r.IntN(30); i++ {
		from, to := keys[r.IntN(len(keys))], keys[r.IntN(len(keys))]
		typ := []string{"owns", "cites", "x", "😀", strings.Repeat("é", 200), "a b"}[r.IntN(6)]
		ok(t, db.Link(from, typ, to))
	}
	for i := 0; i < r.IntN(8); i++ {
		if err := db.Delete(keys[r.IntN(len(keys))]); err != nil {
			wantErr(t, err, ErrNotFound)
		}
	}
}

// dump describes everything in a file, with each value's SQLite type and
// its exact bits, for comparing two files.
func dump(t *testing.T, db *DB) string {
	t.Helper()
	var b strings.Builder
	rows := must[*sqlRows](t)(wrapRows(db.Query(`SELECT name, coalesce(dims, 0) FROM hc_tables ORDER BY name`)))
	type tbl struct {
		name string
		dims int
	}
	var tables []tbl
	for rows.Next() {
		var x tbl
		ok(t, rows.Scan(&x.name, &x.dims))
		tables = append(tables, x)
	}
	rows.Close()
	for _, x := range tables {
		var fields []string
		cols := must[*sqlRows](t)(wrapRows(db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, x.name)))
		for cols.Next() {
			var c string
			ok(t, cols.Scan(&c))
			if c != "key" {
				fields = append(fields, c)
			}
		}
		cols.Close()
		fmt.Fprintf(&b, "table %s dims %d fields %s\n", x.name, x.dims, strings.Join(fields, ","))
		q := "SELECT key"
		for _, f := range fields {
			q += ", typeof(" + quote(f) + "), +" + quote(f)
		}
		recs := must[*sqlRows](t)(wrapRows(db.Query(q + " FROM " + quote(x.name) + " ORDER BY key")))
		vals := make([]any, 1+2*len(fields))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for recs.Next() {
			ok(t, recs.Scan(ptrs...))
			fmt.Fprintf(&b, "  %q", vals[0])
			for i, f := range fields {
				v := vals[2+2*i]
				switch x := v.(type) {
				case float64:
					v = fmt.Sprintf("%016x", math.Float64bits(x))
				case []byte:
					v = hex.EncodeToString(x)
				case string:
					v = strconv.Quote(x)
				}
				fmt.Fprintf(&b, " %s=%s:%v", f, vals[1+2*i], v)
			}
			b.WriteString("\n")
		}
		recs.Close()
	}
	links := must[*sqlRows](t)(wrapRows(db.Query(`SELECT src, type, dst FROM hc_links ORDER BY src, type, dst`)))
	for links.Next() {
		var s, ty, d string
		ok(t, links.Scan(&s, &ty, &d))
		fmt.Fprintf(&b, "link %q %q %q\n", s, ty, d)
	}
	links.Close()
	return b.String()
}

func exportOf(t *testing.T, db *DB) []byte {
	t.Helper()
	var buf bytes.Buffer
	ok(t, db.Export(&buf))
	return buf.Bytes()
}

func importInto(t *testing.T, export []byte) *DB {
	t.Helper()
	db, _ := openTemp(t)
	ok(t, db.Import(bytes.NewReader(export)))
	return db
}

func TestExportImportRoundTrip(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	for i := 0; i < rounds; i++ {
		r := rand.New(rand.NewPCG(uint64(i), 99))
		a, _ := openTemp(t)
		fillRandom(t, a, r)
		first := exportOf(t, a)
		b := importInto(t, first)
		second := exportOf(t, b)
		if !bytes.Equal(first, second) {
			t.Fatalf("round %d: the second export differs:\n%s\n---\n%s", i, first, second)
		}
		if da, db := dump(t, a), dump(t, b); da != db {
			t.Fatalf("round %d: the imported file differs:\n%s\n---\n%s", i, da, db)
		}
		ra, rb := checkOK(t, a), checkOK(t, b)
		if ra.Tables != rb.Tables || ra.Records != rb.Records || ra.Links != rb.Links || ra.Vectors != rb.Vectors {
			t.Fatalf("round %d: Check counts %+v, then %+v", i, ra, rb)
		}
		a.Close()
		b.Close()
	}
}

func TestExportKeepsWhatTablesRemember(t *testing.T) {
	a, _ := openTemp(t)
	// Fields in the order they first came, not alphabetical.
	ok(t, a.Put("docs:1", Fields{"zeta": 1}))
	ok(t, a.Put("docs:1", Fields{"alpha": 2, "vec": Vector{1, 2}}))
	ok(t, a.Put("docs:2", Fields{"Mid": "x"}))
	// A vector size with no vectors left, and a table with no records.
	ok(t, a.Put("sized:1", Fields{"vec": Vector{1, 2, 3}}))
	ok(t, a.Delete("sized:1"))
	ok(t, a.Put("empty:1", Fields{"a": 1}))
	ok(t, a.Delete("empty:1"))
	// A table made with plain SQL and adopted, whose declared types would
	// otherwise change what go-sqlite3 hands back.
	must[any](t)(a.Exec(`CREATE TABLE typed (key TEXT PRIMARY KEY, d DATETIME, b BOOLEAN, i INTEGER, r REAL, t TEXT, n NUMERIC)`))
	must[any](t)(a.Exec(`INSERT INTO typed VALUES ('typed:1', '2026-10-07 12:00:00', 1, '42', 2, 3, '1.5'), ('typed:2', 1700000000, 0, 'x', -0.0, x'00', NULL)`))
	ok(t, a.Adopt("typed"))

	out := exportOf(t, a)
	for _, want := range []string{
		`{"table":"docs","dims":2,"fields":["zeta","alpha","vec","Mid"]}`,
		`{"table":"empty","dims":null,"fields":["a"]}`,
		`{"table":"sized","dims":3,"fields":["vec"]}`,
		`{"key":"docs:1","fields":{"zeta":1,"alpha":2,"vec":[1,2]}}`,
		`{"key":"typed:1","fields":{"d":"2026-10-07 12:00:00","b":1,"i":42,"r":2.0,"t":"3","n":1.5}}`,
		`{"key":"typed:2","fields":{"d":1700000000,"b":0,"i":"x","r":0.0,"t":{"base64":"AA=="}}}`,
		`{"end":{"tables":4,"records":4,"links":0}}`,
	} {
		if !strings.Contains(string(out), want+"\n") {
			t.Errorf("the export lacks %s:\n%s", want, out)
		}
	}
	b := importInto(t, out)
	if !bytes.Equal(out, exportOf(t, b)) {
		t.Fatal("the second export differs")
	}
	// The remembered size still holds after the import.
	wantErr(t, b.Put("sized:2", Fields{"vec": Vector{1, 2}}), ErrInvalid)
	ok(t, b.Put("sized:2", Fields{"vec": Vector{1, 2, 3}}))
	if got := string(exportOf(t, mustOpenEmpty(t))); got != `{"hypercrux":"export","version":1}`+"\n"+`{"end":{"tables":0,"records":0,"links":0}}`+"\n" {
		t.Fatalf("an empty file exports as:\n%s", got)
	}
}

func mustOpenEmpty(t *testing.T) *DB {
	db, _ := openTemp(t)
	return db
}

func TestExportRefusesWhatPutWouldNot(t *testing.T) {
	for _, c := range []struct {
		name, sql, want string
	}{
		{"infinity", `UPDATE docs SET score = 1e999 WHERE key = 'docs:1'`, "field score is +Inf"},
		{"bad text", `UPDATE docs SET title = CAST(x'ff' AS TEXT) WHERE key = 'docs:1'`, "field title isn't valid UTF-8"},
		{"bad key", `INSERT INTO docs (key) VALUES ('docs:' || CAST(x'ff' AS TEXT))`, "isn't clean UTF-8 text"},
		{"bad link type", `INSERT INTO hc_links VALUES ('docs:1', CAST(x'ff' AS TEXT), 'docs:2')`, "link type"},
		{"bad field name", `CREATE TABLE odd (key TEXT PRIMARY KEY, "first name" TEXT)`, `field name "first name"`},
		{"NaN in a vector", `UPDATE docs SET vec = x'0000c07f0000803f' WHERE key = 'docs:1'`, "check finds problems"},
		{"missing trigger", `DROP TRIGGER "hc.docs.delete"`, "check finds problems"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, _ := openTemp(t)
			ok(t, db.Put("docs:1", Fields{"title": "a", "score": 1.5, "vec": Vector{1, 2}}))
			ok(t, db.Put("docs:2", Fields{"title": "b"}))
			must[any](t)(db.Exec(c.sql))
			if c.name == "bad field name" {
				ok(t, db.Adopt("odd"))
			}
			var buf bytes.Buffer
			err := db.Export(&buf)
			wantErr(t, err, ErrInvalid)
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
			if strings.Contains(buf.String(), `{"end":`) {
				t.Fatalf("a refused export has its end line:\n%s", buf.String())
			}
			fresh, _ := openTemp(t)
			wantErr(t, fresh.Import(&buf), ErrInvalid)
		})
	}
}

func TestImportRefuses(t *testing.T) {
	const hdr = `{"hypercrux":"export","version":1}` + "\n"
	docs := `{"table":"docs","dims":2,"fields":["title","vec"]}` + "\n"
	ends := func(tables, records, links int) string {
		return fmt.Sprintf(`{"end":{"tables":%d,"records":%d,"links":%d}}`+"\n", tables, records, links)
	}
	rec := func(key, fields string) string { return `{"key":"` + key + `","fields":` + fields + "}\n" }
	link := func(from, typ, to string) string {
		return `{"from":"` + from + `","type":"` + typ + `","to":"` + to + `"}` + "\n"
	}
	good := hdr + docs + rec("docs:1", `{"title":"a"}`) + rec("docs:2", `{"vec":[1,2]}`) + link("docs:1", "x", "docs:2") + ends(1, 2, 1)
	for _, c := range []struct{ in, want string }{
		{"garbage\n", "not a valid HyperCrux export"},
		{hdr + `{"table":"Docs","dims":null,"fields":[]}` + "\n" + ends(1, 0, 0), `table name "Docs"`},
		{hdr + `{"table":"hc_x","dims":null,"fields":[]}` + "\n" + ends(1, 0, 0), `table name "hc_x"`},
		{hdr + `{"table":"docs","dims":null,"fields":["first name"]}` + "\n" + ends(1, 0, 0), `field name "first name"`},
		{hdr + `{"table":"docs","dims":null,"fields":["rowid"]}` + "\n" + ends(1, 0, 0), `field name "rowid"`},
		{hdr + `{"table":"docs","dims":70000,"fields":[]}` + "\n" + ends(1, 0, 0), "more than 65536"},
		{hdr + docs + rec("docs:", `{}`) + ends(1, 1, 0), "nothing after the colon"},
		{hdr + docs + rec(`docs:\u0000`, `{}`) + ends(1, 1, 0), "isn't clean UTF-8 text"},
		{hdr + docs + rec("docs:1", `{}`) + rec("docs:1", `{}`) + ends(1, 2, 0), "the record docs:1 is there twice"},
		{hdr + docs + rec("docs:1", `{"vec":[1,2,3]}`) + ends(1, 1, 0), "holds vectors of 2"},
		{hdr + docs + rec("docs:1", `{"vec":[0,0]}`) + ends(1, 1, 0), "only zeros"},
		{hdr + docs + rec("docs:1", `{"vec":[]}`) + ends(1, 1, 0), "a vector has 1 to 65536 values"},
		{hdr + docs + rec("docs:1", `{}`) + link("docs:1", "x", "docs:1") + link("docs:1", "x", "docs:1") + ends(1, 1, 2), "is there twice"},
		{hdr + docs + rec("docs:1", `{}`) + link("docs:1", "x", "docs:9") + ends(1, 1, 1), "a link to a key that does not exist"},
		{hdr + docs + rec("docs:1", `{}`) + link("docs:1", "", "docs:1") + ends(1, 1, 1), "use 1 to 200 characters"},
		{hdr + docs + rec("docs:1", `{}`) + link("docs:1", strings.Repeat("x", 201), "docs:1") + ends(1, 1, 1), "use 1 to 200 characters"},
		{hdr + docs + rec("docs:1", `{}`) + link("nocolon", "x", "docs:1") + ends(1, 1, 1), "has no table"},
	} {
		db, _ := openTemp(t)
		err := db.Import(strings.NewReader(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("input:\n%s\ngot %v, want an error with %q", c.in, err, c.want)
			continue
		}
		wantErr(t, err, ErrInvalid)
		if rep := checkOK(t, db); rep.Tables+rep.Records+rep.Links != 0 {
			t.Errorf("a failed import kept %+v", rep)
		}
	}

	// Cut short anywhere, the export is refused and nothing is kept.
	for cut := 0; cut < len(good); cut++ {
		db, _ := openTemp(t)
		err := db.Import(strings.NewReader(good[:cut]))
		if err == nil && cut < len(good)-1 { // the last line break can go
			t.Fatalf("an export cut at byte %d was taken:\n%s", cut, good[:cut])
		}
		if err != nil {
			if rep := checkOK(t, db); rep.Tables != 0 {
				t.Fatalf("an export cut at byte %d left %+v", cut, rep)
			}
		}
		db.Close()
	}

	// Only into a file without records, and without a table of the same name.
	db, path := openTemp(t)
	ok(t, db.Import(strings.NewReader(good)))
	err := db.Import(strings.NewReader(good))
	if err == nil || !strings.Contains(err.Error(), "already holds record tables") {
		t.Fatalf("a second import: %v", err)
	}
	db2, _ := openTemp(t)
	must[any](t)(db2.Exec(`CREATE TABLE DOCS (a)`))
	if err := db2.Import(strings.NewReader(good)); err == nil || !strings.Contains(err.Error(), "already has a table, view or index called docs") {
		t.Fatalf("an import over a plain table: %v", err)
	}
	_ = path
}

// TestExportReadsOneMoment exports while another goroutine keeps adding
// records and links. Every export must still import, which needs its links
// to point at records in the same export and its counts to add up.
func TestExportReadsOneMoment(t *testing.T) {
	db, path := openTemp(t)
	ok(t, db.Update(func(tx *Tx) error {
		for i := 0; i < 500; i++ {
			if err := tx.Put(fmt.Sprintf("docs:%04d", i), Fields{"n": i, "vec": Vector{1, float32(i)}}); err != nil {
				return err
			}
		}
		return nil
	}))
	writer := must[*DB](t)(Open(path))
	defer writer.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 500; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("docs:%04d", i)
			writer.Update(func(tx *Tx) error {
				if err := tx.Put(key, Fields{"n": i, "vec": Vector{1, float32(i)}}); err != nil {
					return err
				}
				return tx.Link(key, "after", fmt.Sprintf("docs:%04d", i-1))
			})
		}
	}()
	for i := 0; i < 5; i++ {
		out := exportOf(t, db)
		fresh := must[*DB](t)(Open(filepath.Join(t.TempDir(), "copy.db")))
		ok(t, fresh.Import(bytes.NewReader(out)))
		checkOK(t, fresh)
		fresh.Close()
	}
	close(stop)
	wg.Wait()
}
