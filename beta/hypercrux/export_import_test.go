// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/internal/export"
)

// Export and import (G6): databases moved from 0.x to the Beta and back,
// and from the Beta to 0.x and back, byte for byte, on A2's random files;
// what tables remember beyond their records; Import's refusals against
// 0.x's; imports that fail, which leave the database and its file as they
// were; and exports that read one point in the log while another handle
// writes.

// emptyExport is what a database with nothing in it exports as.
const emptyExport = `{"hypercrux":"export","version":1}` + "\n" + `{"end":{"tables":0,"records":0,"links":0}}` + "\n"

// A2's random files: the values, the generator and the seeds of
// TestExportImportRoundTrip, in export_test.go at the root, which 0.x's
// export and import pass. The generator runs on either engine through
// handle, drawing the same numbers in the same order, so a seed makes the
// same calls, and the same file, on both.
var (
	odd64 = []float64{math.Copysign(0, -1), 0, 1, -1, 0.1, 1e21, 1e-7, 123456789, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308, 1 << 53, 1<<53 + 2}
	oddInts = []int64{0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1}
	oddF32  = []float32{float32(math.Copysign(0, -1)), math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32,
		1.17549435e-38, 1e-7, 0.1, 1, 16777217}
	oddText = []string{"", " ", "\x00", "a\x00b", "\"", "\\", "\n\r\t\x01\x1f\x7f", "\u2028\u2029", "é", "e\u0301", "😀",
		"\ufffd", "שלום", "مرحبا", "中文", `{"base64":"AA=="}`, "[1,2]", "null", "1.0", "-0", strings.Repeat("x", 3000)}
)

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// vectorOK is 0.x's check of a vector: 1 to 65,536 values, each finite,
// and not all of them zero.
func vectorOK(v []float32) bool {
	if len(v) == 0 || len(v) > hc.MaxDims {
		return false
	}
	zero := true
	for _, x := range v {
		if !finite(float64(x)) {
			return false
		}
		zero = zero && x == 0
	}
	return !zero
}

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

func randomVec(r *rand.Rand, dims int) vec {
	for {
		v := make(vec, dims)
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
		if vectorOK(v) {
			return v
		}
	}
}

// fillRandom is A2's: random records in one to four tables through Put,
// with fields that join their tables in different orders and vectors in
// all but one table, then random links, and then a few deletes, which take
// links with them.
func fillRandom(t *testing.T, h handle, r *rand.Rand) {
	t.Helper()
	tables := []string{"docs", "customer", "a_1", "z9"}[:1+r.IntN(4)]
	fields := []string{"title", "Score", "body", "n", "when_", "x1", "flag"}
	dims := map[string]int{}
	var keys []string
	for i := 0; i < 20+r.IntN(40); i++ {
		tbl := tables[r.IntN(len(tables))]
		// Keys can't hold NUL or run past 1,024 bytes, so leave those out.
		key := tbl + ":" + strings.ReplaceAll(oddText[r.IntN(len(oddText)-1)], "\x00", "0") + strconv.Itoa(r.IntN(30))
		f := goFields{}
		for n := r.IntN(4); n > 0; n-- {
			f[fields[r.IntN(len(fields))]] = randomField(r)
		}
		if tbl != "customer" && r.IntN(2) == 0 {
			if dims[tbl] == 0 {
				dims[tbl] = 1 + r.IntN(5)
			}
			f["vec"] = randomVec(r, dims[tbl])
		}
		ok(t, h.put(key, f))
		keys = append(keys, key)
	}
	for i := 0; i < r.IntN(30); i++ {
		from, to := keys[r.IntN(len(keys))], keys[r.IntN(len(keys))]
		typ := []string{"owns", "cites", "x", "😀", strings.Repeat("é", 200), "a b"}[r.IntN(6)]
		ok(t, h.link(from, typ, to))
	}
	for i := 0; i < r.IntN(8); i++ {
		if err := h.delete(keys[r.IntN(len(keys))]); kindOf(err) != "ok" && kindOf(err) != "not found" {
			t.Fatal(err)
		}
	}
}

func openZero(t *testing.T, path string) *zx.DB {
	t.Helper()
	db, err := zx.Open(path)
	ok(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// exporter is a database of either engine.
type exporter interface{ Export(w io.Writer) error }

func exportOf(t *testing.T, db exporter) []byte {
	t.Helper()
	var buf bytes.Buffer
	ok(t, db.Export(&buf))
	return buf.Bytes()
}

// keysOf reads an export back with internal/export's reader, and returns
// its records' keys.
func keysOf(t *testing.T, exp []byte) []string {
	t.Helper()
	rd, err := export.NewReader(bytes.NewReader(exp))
	ok(t, err)
	var keys []string
	for {
		item, err := rd.Next()
		if err == io.EOF {
			return keys
		}
		ok(t, err)
		if r, isRecord := item.(export.Record); isRecord {
			keys = append(keys, r.Key)
		}
	}
}

// sameAs0x checks that the Beta's database reads back what 0.x's holds: at
// each key, the fields with their types and bits, and the links both ways;
// each table's scan; and Check's counts.
func sameAs0x(t *testing.T, z *zx.DB, b *hc.DB, keys []string) {
	t.Helper()
	tables := map[string]bool{}
	for _, k := range keys {
		for _, cl := range []call{{op: "get", key: k}, {op: "neighbours", key: k, dir: int(hc.Both)}} {
			same(t, 0, cl.String(), do(betaOn{b}, cl), do(zeroOn{z}, cl))
		}
		tables[k[:strings.IndexByte(k, ':')]] = true
	}
	for tbl := range tables {
		cl := call{op: "scan", key: tbl + ":"}
		same(t, 0, cl.String(), do(betaOn{b}, cl), do(zeroOn{z}, cl))
	}
	zr, err := z.Check()
	ok(t, err)
	br, err := b.Check()
	ok(t, err)
	if !zr.OK() || br.Tables != zr.Tables || br.Records != zr.Records || br.Links != zr.Links || br.Vectors != zr.Vectors {
		t.Fatalf("Check counts %+v on the Beta, and %+v on 0.x", br, zr)
	}
}

func quoteName(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// dump describes everything in a 0.x file, with each value's SQLite type
// and its exact bits, as A2's test does, for comparing two files.
func dump(t *testing.T, db *zx.DB) string {
	t.Helper()
	var b strings.Builder
	type tbl struct {
		name string
		dims int
	}
	var list []tbl
	rows, err := db.Query(`SELECT name, coalesce(dims, 0) FROM hc_tables ORDER BY name`)
	ok(t, err)
	for rows.Next() {
		var x tbl
		ok(t, rows.Scan(&x.name, &x.dims))
		list = append(list, x)
	}
	ok(t, rows.Err())
	rows.Close()
	for _, x := range list {
		var fields []string
		cols, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, x.name)
		ok(t, err)
		for cols.Next() {
			var c string
			ok(t, cols.Scan(&c))
			if c != "key" {
				fields = append(fields, c)
			}
		}
		ok(t, cols.Err())
		cols.Close()
		fmt.Fprintf(&b, "table %s dims %d fields %s\n", x.name, x.dims, strings.Join(fields, ","))
		q := "SELECT key"
		for _, f := range fields {
			q += ", typeof(" + quoteName(f) + "), +" + quoteName(f)
		}
		recs, err := db.Query(q + " FROM " + quoteName(x.name) + " ORDER BY key")
		ok(t, err)
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
		ok(t, recs.Err())
		recs.Close()
	}
	links, err := db.Query(`SELECT src, type, dst FROM hc_links ORDER BY src, type, dst`)
	ok(t, err)
	for links.Next() {
		var s, ty, d string
		ok(t, links.Scan(&s, &ty, &d))
		fmt.Fprintf(&b, "link %q %q %q\n", s, ty, d)
	}
	ok(t, links.Err())
	links.Close()
	return b.String()
}

// TestTheExportKeepsWhatTablesRemember is A2's test of what a table keeps
// beyond its records, on both engines: its fields in the order they first
// came, its vector size once its vectors have gone, and the table itself
// once its records have gone. The same calls give 0.x's lines in the
// Beta's export. Tables 0.x made with plain SQL and adopted, with declared
// types and generated columns, come into the Beta as their export has
// them, and go back. After the import the Beta keeps the remembered size,
// and an empty database exports as two lines.
func TestTheExportKeepsWhatTablesRemember(t *testing.T) {
	dir := t.TempDir()
	z := openZero(t, filepath.Join(dir, "zero.db"))
	b := open(t, filepath.Join(dir, "beta.hcx"))
	for _, h := range []handle{zeroOn{z}, betaOn{b}} {
		ok(t, h.put("docs:1", goFields{"zeta": 1}))
		ok(t, h.put("docs:1", goFields{"alpha": 2, "vec": vec{1, 2}}))
		ok(t, h.put("docs:2", goFields{"Mid": "x"}))
		ok(t, h.put("sized:1", goFields{"vec": vec{1, 2, 3}}))
		ok(t, h.delete("sized:1"))
		ok(t, h.put("empty:1", goFields{"a": 1}))
		ok(t, h.delete("empty:1"))
	}
	want := strings.Join([]string{
		`{"hypercrux":"export","version":1}`,
		`{"table":"docs","dims":2,"fields":["zeta","alpha","vec","Mid"]}`,
		`{"table":"empty","dims":null,"fields":["a"]}`,
		`{"table":"sized","dims":3,"fields":["vec"]}`,
		`{"key":"docs:1","fields":{"zeta":1,"alpha":2,"vec":[1,2]}}`,
		`{"key":"docs:2","fields":{"Mid":"x"}}`,
		`{"end":{"tables":3,"records":2,"links":0}}`,
	}, "\n") + "\n"
	for name, db := range map[string]exporter{"0.x": z, "the Beta": b} {
		if got := string(exportOf(t, db)); got != want {
			t.Errorf("%s exports as\n%s\nwhere\n%s\nis wanted", name, got, want)
		}
	}

	// Tables of 0.x's plain SQL, whose declared types would change what
	// go-sqlite3 hands back, and whose generated columns come out as plain
	// fields holding their values.
	for _, q := range []string{
		`CREATE TABLE typed (key TEXT PRIMARY KEY, d DATETIME, b BOOLEAN, i INTEGER, r REAL, t TEXT, n NUMERIC)`,
		`INSERT INTO typed VALUES ('typed:1', '2026-10-07 12:00:00', 1, '42', 2, 3, '1.5'), ('typed:2', 1700000000, 0, 'x', -0.0, x'00', NULL)`,
		`CREATE TABLE gen (key TEXT PRIMARY KEY, a, b GENERATED ALWAYS AS (a * 2) STORED, c AS (a || 'x'), d)`,
		`INSERT INTO gen (key, a, d) VALUES ('gen:1', 5, 'y')`,
	} {
		_, err := z.Exec(q)
		ok(t, err)
	}
	ok(t, z.Adopt("typed"))
	ok(t, z.Adopt("gen"))
	out := exportOf(t, z)
	for _, line := range []string{
		`{"table":"gen","dims":null,"fields":["a","b","c","d"]}`,
		`{"key":"gen:1","fields":{"a":5,"b":10,"c":"5x","d":"y"}}`,
		`{"key":"typed:1","fields":{"d":"2026-10-07 12:00:00","b":1,"i":42,"r":2.0,"t":"3","n":1.5}}`,
		`{"key":"typed:2","fields":{"d":1700000000,"b":0,"i":"x","r":0.0,"t":{"base64":"AA=="}}}`,
	} {
		if !bytes.Contains(out, []byte(line+"\n")) {
			t.Fatalf("0.x's export lacks %s:\n%s", line, out)
		}
	}
	moved := open(t, filepath.Join(dir, "moved.hcx"))
	ok(t, moved.Import(bytes.NewReader(out)))
	if got := exportOf(t, moved); !bytes.Equal(out, got) {
		t.Fatalf("the Beta exports 0.x's export as\n%s\nwhere it was\n%s", got, out)
	}
	back := openZero(t, filepath.Join(dir, "back.db"))
	ok(t, back.Import(bytes.NewReader(exportOf(t, moved))))
	if got := exportOf(t, back); !bytes.Equal(out, got) {
		t.Fatalf("back in 0.x, the export is\n%s\nwhere it was\n%s", got, out)
	}
	if f, err := moved.Get("typed:2"); err != nil || show(f) != `{b: int64 0, d: int64 1700000000, i: string "x", r: float64 0000000000000000, t: []byte 00 (nil false)}` {
		t.Fatalf("typed:2 came into the Beta as %s, %v", show(f), err)
	}

	// The remembered size holds after the import.
	wantErr(t, moved.Put("sized:2", hc.Fields{"vec": hc.Vector{1, 2}}), hc.ErrInvalid)
	ok(t, moved.Put("sized:2", hc.Fields{"vec": hc.Vector{1, 2, 3}}))
	if got := string(exportOf(t, open(t, filepath.Join(dir, "empty.hcx")))); got != emptyExport {
		t.Fatalf("an empty database exports as\n%s", got)
	}
}

// TestImportRefusesWhat0xRefuses gives the same exports to 0.x's Import and
// the Beta's: broken ones, ones that break the rules of names, keys,
// values, vectors and links, and ones with a record or a link there twice.
// The Beta refuses each with 0.x's kind of error and 0.x's message, and
// keeps nothing: one database on each engine takes every case in turn,
// and after each the Beta's exports as an empty one, Check counts nothing,
// and its file keeps its size. Where the Beta's rules differ from 0.x's,
// the cases below the first list say how.
//
// Import goes into a database without record tables only. Into one with a
// record, or a table whose records have gone, both refuse with "already
// holds record tables", and once the table is dropped both take it.
func TestImportRefusesWhat0xRefuses(t *testing.T) {
	const hdr = `{"hypercrux":"export","version":1}` + "\n"
	docs := `{"table":"docs","dims":2,"fields":["title","vec"]}` + "\n"
	ends := func(tables, records, links int) string {
		return fmt.Sprintf(`{"end":{"tables":%d,"records":%d,"links":%d}}`+"\n", tables, records, links)
	}
	table := func(name, fields string) string {
		return `{"table":"` + name + `","dims":null,"fields":[` + fields + `]}` + "\n"
	}
	rec := func(key, fields string) string { return `{"key":"` + key + `","fields":` + fields + "}\n" }
	link := func(from, typ, to string) string {
		return `{"from":"` + from + `","type":"` + typ + `","to":"` + to + `"}` + "\n"
	}
	one := hdr + docs + rec("docs:1", `{}`)
	good := hdr + docs + rec("docs:1", `{"title":"a"}`) + rec("docs:2", `{"vec":[1,2]}`) + link("docs:1", "x", "docs:2") + ends(1, 2, 1)
	dir := t.TempDir()
	n := 0
	// fresh opens a new database on each engine, and gives the Beta's
	// file's size.
	fresh := func() (*zx.DB, *hc.DB, string, int64) {
		n++
		z := openZero(t, filepath.Join(dir, fmt.Sprintf("%d.db", n)))
		path := filepath.Join(dir, fmt.Sprintf("%d.hcx", n))
		b := open(t, path)
		info, err := os.Stat(path)
		ok(t, err)
		return z, b, path, info.Size()
	}
	// keptNothing checks that a refused import left the Beta's database as
	// a new one, in the copy and in the file.
	keptNothing := func(in string, b *hc.DB, path string, size int64) {
		t.Helper()
		if got := string(exportOf(t, b)); got != emptyExport {
			t.Errorf("input:\n%s\nafter the refusal, the Beta exports as\n%s", in, got)
		}
		if rep, err := b.Check(); err != nil || rep.Tables+rep.Records+rep.Links != 0 {
			t.Errorf("input:\n%s\nafter the refusal, Check gives %+v, %v", in, rep, err)
		}
		if info, err := os.Stat(path); err != nil {
			t.Error(err)
		} else if info.Size() != size {
			t.Errorf("input:\n%s\nafter the refusal, the file is %d bytes, where it was %d", in, info.Size(), size)
		}
	}
	z, b, path, size := fresh()
	for _, c := range []struct{ in, want string }{
		{"", "the input is empty"},
		{"garbage\n", "a line holds one JSON object"},
		{hdr, "before its end line"},
		{hdr + hdr, "a second first line"},
		{`{"hypercrux":"export","version":2}` + "\n", "upgrade HyperCrux"},
		{hdr + table("Docs", "") + ends(1, 0, 0), `table name "Docs"`},
		{hdr + table("hc_x", "") + ends(1, 0, 0), `table name "hc_x"`},
		{hdr + table("sqlite_x", "") + ends(1, 0, 0), `table name "sqlite_x"`},
		{hdr + table("my-docs", "") + ends(1, 0, 0), `table name "my-docs"`},
		{hdr + table("docs", `"first name"`) + ends(1, 0, 0), `field name "first name"`},
		{hdr + table("docs", `"a","rowid"`) + ends(1, 0, 0), `field name "rowid"`},
		{hdr + table("docs", `"KEY"`) + ends(1, 0, 0), `field name "KEY"`},
		{hdr + table("docs", `"_rowid_"`) + ends(1, 0, 0), `field name "_rowid_"`},
		{hdr + table("docs", `"1st"`) + ends(1, 0, 0), `field name "1st"`},
		{hdr + table("docs", `"a","A"`) + ends(1, 0, 0), "counting upper and lower case as the same"},
		{hdr + table("docs", `""`) + ends(1, 0, 0), "a field's name is text"},
		{hdr + `{"table":"docs","dims":70000,"fields":["vec"]}` + "\n" + ends(1, 0, 0), "more than 65536"},
		{hdr + `{"table":"docs","dims":0,"fields":["vec"]}` + "\n" + ends(1, 0, 0), "a whole number from 1, or null"},
		{hdr + docs + docs + ends(2, 0, 0), "table docs is there twice"},
		{hdr + docs + rec("docs:", `{}`) + ends(1, 1, 0), "nothing after the colon"},
		{hdr + docs + rec(`docs:\u0000`, `{}`) + ends(1, 1, 0), "isn't clean UTF-8 text"},
		{hdr + docs + rec("docs:"+strings.Repeat("x", 1020), `{}`) + ends(1, 1, 0), "more than 1024"},
		{hdr + docs + rec("other:1", `{}`) + ends(1, 1, 0), "which the export doesn't list"},
		{hdr + docs + rec("docs:1", `{"zz":1}`) + ends(1, 1, 0), "which table docs doesn't list"},
		{hdr + docs + rec("docs:1", `{"Title":1}`) + ends(1, 1, 0), "which table docs doesn't list"},
		{one + rec("docs:1", `{}`) + ends(1, 2, 0), "the record docs:1 is there twice"},
		{one + rec("docs:1", `{"title":"other"}`) + ends(1, 2, 0), "the record docs:1 is there twice"},
		{hdr + docs + rec("docs:1", `{"vec":[1,2,3]}`) + ends(1, 1, 0), "docs:1 has a vector of 3 values, and table docs holds vectors of 2"},
		{hdr + table("docs", `"vec"`) + rec("docs:1", `{"vec":[1,2]}`) + rec("docs:2", `{"vec":[1,2,3]}`) + ends(1, 2, 0), "holds vectors of 2"},
		{hdr + docs + rec("docs:1", `{"vec":[0,-0]}`) + ends(1, 1, 0), "only zeros"},
		{hdr + docs + rec("docs:1", `{"vec":[]}`) + ends(1, 1, 0), "a vector has 1 to 65536 values"},
		{hdr + docs + rec("docs:1", `{"vec":[1e39,1]}`) + ends(1, 1, 0), "out of float32's range"},
		{hdr + docs + rec("docs:1", `{"vec":[1,"2"]}`) + ends(1, 1, 0), "vector value 1 isn't a number"},
		{hdr + docs + rec("docs:1", `{"vec":"[1,2]"}`) + ends(1, 1, 0), "the vector is a list of numbers"},
		{hdr + docs + rec("docs:1", `{"title":[1,2]}`) + ends(1, 1, 0), "only the field vec holds a list"},
		{hdr + docs + rec("docs:1", `{"title":1e999}`) + ends(1, 1, 0), "out of range"},
		{hdr + docs + rec("docs:1", `{"title":99999999999999999999}`) + ends(1, 1, 0), "integers are 64-bit"},
		{hdr + docs + rec("docs:1", `{"title":true}`) + ends(1, 1, 0), "use 1 and 0"},
		{hdr + docs + rec("docs:1", `{"title":{"base64":"AA"}}`) + ends(1, 1, 0), "standard base64"},
		{hdr + docs + rec("docs:1", `{"title":{"x":"AA=="}}`) + ends(1, 1, 0), "holds bytes"},
		{hdr + docs + rec("docs:1", `{"title":"\ud800"}`) + ends(1, 1, 0), "half of a surrogate pair"},
		{hdr + docs + rec("docs:1", `{"title":"a","title":"b"}`) + ends(1, 1, 0), `the member "title" is there twice`},
		{one + link("docs:1", "x", "docs:1") + link("docs:1", "x", "docs:1") + ends(1, 1, 2), "the link docs:1 -x-> docs:1 is there twice"},
		{one + link("docs:1", "x", "docs:9") + ends(1, 1, 1), "the link docs:1 -x-> docs:9: a link to a key that does not exist"},
		{one + link("docs:9", "x", "docs:1") + ends(1, 1, 1), "a link from a key that does not exist"},
		{one + link("other:9", "x", "docs:9") + ends(1, 1, 1), "a link from a key that does not exist"},
		{one + link("docs:1", "", "docs:1") + ends(1, 1, 1), `link type "": use 1 to 200 characters`},
		{one + link("docs:1", strings.Repeat("é", 201), "docs:1") + ends(1, 1, 1), "use 1 to 200 characters"},
		{one + link("nocolon", "x", "docs:1") + ends(1, 1, 1), `key "nocolon" has no table`},
		{one + link("docs:1", "x", "Docs:1") + ends(1, 1, 1), `table name "Docs"`},
		{one + link("docs:1", "x", "docs:1") + rec("docs:2", `{}`) + ends(1, 2, 1), "a record comes after the links"},
		{one + table("other", "") + ends(2, 1, 0), "a table comes after records or links"},
		{one + `{"from":"docs:1","type":"x"}` + "\n" + ends(1, 1, 1), `the line has no "to"`},
		{hdr + docs + ends(2, 0, 0), "the end line counts 2 tables"},
		{hdr + docs + ends(1, 0, 0) + "x\n", "a line holds one JSON object"},
		{hdr + docs + ends(1, 0, 0) + hdr, "something follows the end line"},
		{good[:len(good)/2], "not a valid HyperCrux export"},
	} {
		zErr := z.Import(strings.NewReader(c.in))
		bErr := b.Import(strings.NewReader(c.in))
		switch {
		case zErr == nil || !strings.Contains(zErr.Error(), c.want):
			t.Errorf("input:\n%s\n0.x gives %v, where an error with %q is wanted; the case is wrong", c.in, zErr, c.want)
		case describeErr(bErr) != describeErr(zErr):
			t.Errorf("input:\n%s\nthe Beta gives %s, and 0.x %s", c.in, describeErr(bErr), describeErr(zErr))
		}
		keptNothing(c.in, b, path, size)
	}

	// Only into a database without record tables.
	z, b, _, _ = fresh()
	ok(t, z.Import(strings.NewReader(good)))
	ok(t, b.Import(strings.NewReader(good)))
	zErr, bErr := z.Import(strings.NewReader(good)), b.Import(strings.NewReader(good))
	if !strings.Contains(zErr.Error(), "already holds record tables") || describeErr(bErr) != describeErr(zErr) {
		t.Errorf("a second import: the Beta gives %v, and 0.x %v", bErr, zErr)
	}
	// Garbage is refused as garbage, before the tables are looked at.
	zErr, bErr = z.Import(strings.NewReader("garbage\n")), b.Import(strings.NewReader("garbage\n"))
	if !strings.Contains(bErr.Error(), "not a valid HyperCrux export") || describeErr(bErr) != describeErr(zErr) {
		t.Errorf("garbage into a database with records: the Beta gives %v, and 0.x %v", bErr, zErr)
	}
	z, b, path, size = fresh()
	for _, h := range []handle{zeroOn{z}, betaOn{b}} {
		ok(t, h.put("docs:1", nil))
		ok(t, h.delete("docs:1"))
	}
	info, err := os.Stat(path)
	ok(t, err)
	size = info.Size()
	zErr, bErr = z.Import(strings.NewReader(good)), b.Import(strings.NewReader(good))
	if !strings.Contains(zErr.Error(), "already holds record tables") || describeErr(bErr) != describeErr(zErr) {
		t.Errorf("an import over an empty table: the Beta gives %v, and 0.x %v", bErr, zErr)
	}
	if info, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if info.Size() != size {
		t.Errorf("a refused import changed the file from %d bytes to %d", size, info.Size())
	}
	for _, h := range []handle{zeroOn{z}, betaOn{b}} {
		ok(t, h.drop("docs"))
	}
	ok(t, z.Import(strings.NewReader(good)))
	ok(t, b.Import(strings.NewReader(good)))
	if string(exportOf(t, z)) != good || string(exportOf(t, b)) != good {
		t.Errorf("an import once the table was dropped exports as\n%s\nfrom 0.x and\n%s\nfrom the Beta", exportOf(t, z), exportOf(t, b))
	}

	// Where the rules differ. A link type that starts with a zero byte is
	// refused by both, by 0.x's link table's trigger in its own words.
	z, b, path, size = fresh()
	in := one + link("docs:1", `\u0000x`, "docs:1") + ends(1, 1, 1)
	zErr, bErr = z.Import(strings.NewReader(in)), b.Import(strings.NewReader(in))
	if !errors.Is(zErr, zx.ErrInvalid) || !errors.Is(bErr, hc.ErrInvalid) || !strings.Contains(bErr.Error(), `line 4 of the export: link type "\x00x" starts with a zero byte`) {
		t.Errorf("a link type that starts with a zero byte: the Beta gives %v, and 0.x %v", bErr, zErr)
	}
	keptNothing(in, b, path, size)
	// A vector size without a vector field: 0.x keeps it, and the Beta
	// can't hold it.
	z, b, path, size = fresh()
	in = hdr + `{"table":"docs","dims":3,"fields":["a"]}` + "\n" + rec("docs:1", `{"a":1}`) + ends(1, 1, 0)
	ok(t, z.Import(strings.NewReader(in)))
	bErr = b.Import(strings.NewReader(in))
	if describeErr(bErr) != "invalid: hypercrux: invalid: line 2 of the export: table docs has a vector size of 3 and no field vec, which a table can't have in the Beta; make its dims null" {
		t.Errorf("a vector size without a vector field: the Beta gives %v", bErr)
	}
	keptNothing(in, b, path, size)
}

// errReader is input that fails to read.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// bigExport makes a database of n records with vectors, and a tenth as
// many in another table, with links, and returns its export.
func bigExport(t *testing.T, path string, n int) string {
	t.Helper()
	src := open(t, path)
	ok(t, src.Update(func(tx *hc.Tx) error {
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("docs:%04d", i)
			if err := tx.Put(k, hc.Fields{"n": i, "title": strings.Repeat("é", i%7), "vec": hc.Vector{1, float32(i)}}); err != nil {
				return err
			}
			if i > 0 {
				if err := tx.Link(k, "after", fmt.Sprintf("docs:%04d", i-1)); err != nil {
					return err
				}
			}
			if i%10 == 0 {
				p := fmt.Sprintf("people:%d", i)
				if err := tx.Put(p, hc.Fields{"name": "p" + strconv.Itoa(i), "raw": []byte{byte(i), 0}}); err != nil {
					return err
				}
				if err := tx.Link(p, "owns", k); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	return string(exportOf(t, src))
}

// TestAFailedImportChangesNothing makes imports fail late, in a database
// whose file holds history: a table made, filled and dropped. Each export
// holds a few thousand lines, and fails after nearly all of them have gone
// into its transaction: at a record that's there twice, a vector of the
// wrong size, a link to a record the export lacks, a cut before the end
// line, and input that fails to read. Each time the database is as it was,
// nothing reached the file, which keeps the same bytes, and a reopen reads
// the database as it was. Then the whole export goes in, through the same
// handle, and a reopen reads it back.
func TestAFailedImportChangesNothing(t *testing.T) {
	dir := t.TempDir()
	good := bigExport(t, filepath.Join(dir, "src.hcx"), 1500)
	lines := strings.SplitAfter(good, "\n")
	end := len(lines) - 2 // the end line, since the last element is empty
	lastRecord := 0
	for i, l := range lines {
		if strings.HasPrefix(l, `{"key":`) {
			lastRecord = i
		}
	}
	// with puts extra after the line at i.
	with := func(i int, extra string) string {
		return strings.Join(lines[:i+1], "") + extra + strings.Join(lines[i+1:], "")
	}
	path := filepath.Join(dir, "test.hcx")
	db := open(t, path)
	ok(t, db.Put("old:1", hc.Fields{"vec": hc.Vector{1, 2, 3}}))
	ok(t, db.Put("old:2", nil))
	ok(t, db.Link("old:1", "x", "old:2"))
	ok(t, db.Drop("old"))
	file, err := os.ReadFile(path)
	ok(t, err)
	boom := errors.New("the input gave up")
	for _, c := range []struct {
		what string
		in   io.Reader
		kind error
		want string
	}{
		{"a record there twice", strings.NewReader(with(lastRecord, lines[lastRecord])), hc.ErrInvalid, "is there twice"},
		{"a vector of the wrong size", strings.NewReader(with(lastRecord, `{"key":"docs:9999","fields":{"vec":[1,2,3]}}`+"\n")), hc.ErrInvalid, "holds vectors of 2"},
		{"a link to a record the export lacks", strings.NewReader(with(end-1, `{"from":"docs:0001","type":"x","to":"docs:9999"}`+"\n")), hc.ErrInvalid, "a link to a key that does not exist"},
		{"no end line", strings.NewReader(strings.Join(lines[:end], "")), hc.ErrInvalid, "before its end line"},
		{"input that fails to read", io.MultiReader(strings.NewReader(strings.Join(lines[:end-1], "")), errReader{boom}), boom, boom.Error()},
	} {
		err := db.Import(c.in)
		if !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: Import gave %v, where an error with %q that wraps %v is wanted", c.what, err, c.want, c.kind)
		}
		if got := string(exportOf(t, db)); got != emptyExport {
			t.Fatalf("%s: after the failed import, the database exports as\n%.300s", c.what, got)
		}
		if rep, err := db.Check(); err != nil || rep.Tables+rep.Records+rep.Links+rep.Vectors != 0 {
			t.Fatalf("%s: after the failed import, Check gives %+v, %v", c.what, rep, err)
		}
		if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, file) {
			t.Fatalf("%s: the failed import changed the file, from %d bytes to %d: %v", c.what, len(file), len(now), err)
		}
		again, err := hc.Open(path)
		ok(t, err)
		if got := string(exportOf(t, again)); got != emptyExport {
			t.Fatalf("%s: a reopen after the failed import exports as\n%.300s", c.what, got)
		}
		ok(t, again.Close())
	}
	ok(t, db.Import(strings.NewReader(good)))
	if got := string(exportOf(t, db)); got != good {
		t.Fatal("the export of the import differs from the export imported")
	}
	ok(t, db.Close())
	if got := string(exportOf(t, open(t, path))); got != good {
		t.Fatal("after a reopen, the export of the import differs from the export imported")
	}
}

// TestAnImportWhoseCommitFails makes an import's commit fail on the fault
// layer's disk, at the batch's write, its sync and its marker's write. The
// import gives the disk's error, the database is as it was, and a reopen
// reads it as it was, since the log cuts the failed batch back. The same
// export goes in afterwards, through the same handle.
func TestAnImportWhoseCommitFails(t *testing.T) {
	const path = "/db/test.hcx"
	good := bigExport(t, filepath.Join(t.TempDir(), "src.hcx"), 200)
	for _, c := range []struct {
		what string
		rule fault.Rule
	}{
		{"the batch's write", fault.Rule{Op: fault.WriteAt, Path: path, N: 1}},
		{"the sync", fault.Rule{Op: fault.Sync, Path: path, N: 1}},
		{"the marker's write", fault.Rule{Op: fault.WriteAt, Path: path, N: 2}},
	} {
		for seed := uint64(1); seed <= 3; seed++ {
			d := fault.New(seed)
			db := openFault(t, d, path)
			d.Add(c.rule)
			err := db.Import(strings.NewReader(good))
			if !errors.Is(err, syscall.EIO) || d.Failed() != 1 {
				t.Fatalf("%s failing, seed %d: Import gave %v, and the disk failed %d calls", c.what, seed, err, d.Failed())
			}
			if got := string(exportOf(t, db)); got != emptyExport {
				t.Fatalf("%s failing, seed %d: the database exports as\n%.300s", c.what, seed, got)
			}
			d.Clear()
			again, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
			ok(t, err)
			if got := string(exportOf(t, again)); got != emptyExport {
				t.Fatalf("%s failing, seed %d: a reopen exports\n%.300s", c.what, seed, got)
			}
			ok(t, again.Close())
			ok(t, db.Import(strings.NewReader(good)))
			ok(t, db.Close())
			again, err = hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
			ok(t, err)
			if got := string(exportOf(t, again)); got != good {
				t.Fatalf("%s failing, seed %d: after a later import, a reopen exports something else", c.what, seed)
			}
			ok(t, again.Close())
		}
	}
}

// TestAnImportCutAtEveryCall cuts the power at each call the import makes
// on the database's file, on the fault layer's disk. However the cut
// settles, the database opened afterwards holds the whole import or none
// of it.
func TestAnImportCutAtEveryCall(t *testing.T) {
	const path = "/db/test.hcx"
	src := open(t, filepath.Join(t.TempDir(), "src.hcx"))
	ok(t, src.Update(func(tx *hc.Tx) error {
		for i := 0; i < 50; i++ {
			k := fmt.Sprintf("docs:%02d", i)
			if err := tx.Put(k, hc.Fields{"n": i, "vec": hc.Vector{1, float32(i)}}); err != nil {
				return err
			}
			if err := tx.Link(k, "self", k); err != nil {
				return err
			}
		}
		return nil
	}))
	good := string(exportOf(t, src))
	d := fault.New(0)
	db := openFault(t, d, path)
	before := d.Calls(fault.Any, path)
	ok(t, db.Import(strings.NewReader(good)))
	calls := d.Calls(fault.Any, path) - before
	if calls < 3 {
		t.Fatalf("the import made %d calls on the file", calls)
	}
	whole, none := 0, 0
	for n := 1; n <= calls; n++ {
		for seed := uint64(1); seed <= 4; seed++ {
			d := fault.New(seed)
			db := openFault(t, d, path)
			d.Add(fault.Rule{Op: fault.Any, Path: path, N: n, Cut: true})
			db.Import(strings.NewReader(good))
			if d.Cuts() != 1 {
				t.Fatalf("a cut at call %d of %d, seed %d: the power was cut %d times", n, calls, seed, d.Cuts())
			}
			again, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
			if err != nil {
				t.Fatalf("a cut at call %d of %d, seed %d: reopening: %v", n, calls, seed, err)
			}
			switch got := string(exportOf(t, again)); got {
			case good:
				whole++
			case emptyExport:
				none++
			default:
				t.Fatalf("a cut at call %d of %d, seed %d: the database holds part of the import:\n%.300s", n, calls, seed, got)
			}
			ok(t, again.Close())
		}
	}
	if whole == 0 || none == 0 {
		t.Fatalf("of the cuts, %d left the whole import and %d none of it", whole, none)
	}
	t.Logf("%d calls cut at, with 4 seeds each: %d left the whole import, and %d none of it", calls, whole, none)
}

// TestImportCutShortAnywhere cuts an export at every byte. Each cut is
// refused as an export that isn't valid, and nothing is kept, apart from
// the cut that takes off the last line break alone, which goes in, as in
// 0.x.
func TestImportCutShortAnywhere(t *testing.T) {
	src := open(t, filepath.Join(t.TempDir(), "src.hcx"))
	ok(t, src.Put("docs:1", hc.Fields{"title": "a", "n": 1.5}))
	ok(t, src.Put("docs:2", hc.Fields{"vec": hc.Vector{1, 2}, "raw": []byte{0, 1}}))
	ok(t, src.Link("docs:1", "x", "docs:2"))
	good := string(exportOf(t, src))
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	for cut := 0; cut < len(good); cut++ {
		err := db.Import(strings.NewReader(good[:cut]))
		if cut == len(good)-1 {
			ok(t, err)
			ok(t, db.Drop("docs"))
			continue
		}
		if !errors.Is(err, hc.ErrInvalid) || !strings.Contains(err.Error(), "not a valid HyperCrux export") {
			t.Fatalf("an export cut at byte %d of %d: Import gave %v", cut, len(good), err)
		}
		if got := string(exportOf(t, db)); got != emptyExport {
			t.Fatalf("an export cut at byte %d of %d left\n%s", cut, len(good), got)
		}
	}
	t.Logf("an export of %d bytes, cut at each", len(good))
}

// TestExportReadsOnePoint exports while another handle on the same file
// keeps committing, each commit a record and a link to the record before
// it. Each export reads one point in the log, so it imports, which needs
// its links to point at records in the same export and its counts to add
// up, and each holds more than the one before.
func TestExportReadsOnePoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcx")
	db := open(t, path)
	ok(t, db.Update(func(tx *hc.Tx) error {
		for i := 0; i < 300; i++ {
			k := fmt.Sprintf("docs:%05d", i)
			if err := tx.Put(k, hc.Fields{"n": i, "vec": hc.Vector{1, float32(i)}}); err != nil {
				return err
			}
			if i > 0 {
				if err := tx.Link(k, "after", fmt.Sprintf("docs:%05d", i-1)); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	writer := open(t, path)
	var commits atomic.Int64
	stop := make(chan struct{})
	stopped := make(chan struct{})
	var werr error // the writer's, read once it has stopped
	go func() {
		defer close(stopped)
		for i := 300; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := fmt.Sprintf("docs:%05d", i)
			if werr = writer.Update(func(tx *hc.Tx) error {
				if err := tx.Put(k, hc.Fields{"n": i, "vec": hc.Vector{1, float32(i)}}); err != nil {
					return err
				}
				return tx.Link(k, "after", fmt.Sprintf("docs:%05d", i-1))
			}); werr != nil {
				return
			}
			commits.Add(1)
		}
	}()
	last := 0
	for i := 0; i < 5; i++ {
		// Wait for another commit, so each export has more to hold.
		for at := commits.Load(); commits.Load() == at; {
			select {
			case <-stopped:
				t.Fatalf("the writer stopped: %v", werr)
			case <-time.After(time.Millisecond):
			}
		}
		out := exportOf(t, db)
		copied := open(t, filepath.Join(dir, fmt.Sprintf("copy%d.hcx", i)))
		ok(t, copied.Import(bytes.NewReader(out)))
		rep, err := copied.Check()
		ok(t, err)
		if rep.Records != rep.Links+1 || rep.Records <= last {
			t.Fatalf("export %d holds %+v, after one with %d records", i, rep, last)
		}
		last = rep.Records
		ok(t, copied.Close())
	}
	close(stop)
	<-stopped
	ok(t, werr)
	t.Logf("the last export held %d records, with %d commits made", last, commits.Load())
}

// failingWriter takes the first room bytes written to it, and then fails.
type failingWriter struct {
	got  bytes.Buffer
	room int
}

var errFull = errors.New("the writer is full")

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.room {
		w.got.Write(p[:w.room])
		n := w.room
		w.room = 0
		return n, errFull
	}
	w.room -= len(p)
	return w.got.Write(p)
}

// TestAnExportThatFailsStops exports to a writer that fails part of the
// way through. Export gives the writer's error, what reached the writer is
// the start of the export, without its last line, and Import refuses it
// and keeps nothing.
func TestAnExportThatFailsStops(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "src.hcx"))
	good := bigExport(t, filepath.Join(dir, "big.hcx"), 1500)
	big := open(t, filepath.Join(dir, "big.hcx"))
	end := strings.LastIndex(good, `{"end":`)
	for _, room := range []int{0, 100, len(good) / 2, end - 1, end} {
		w := &failingWriter{room: room}
		err := big.Export(w)
		if !errors.Is(err, errFull) {
			t.Fatalf("an export to a writer with room for %d bytes gave %v", room, err)
		}
		got := w.got.Bytes()
		if !strings.HasPrefix(good, string(got)) || bytes.Contains(got, []byte(`{"end":`)) {
			t.Fatalf("an export to a writer with room for %d bytes wrote %d bytes that aren't the start of the export without its end", room, len(got))
		}
		if err := db.Import(bytes.NewReader(got)); !errors.Is(err, hc.ErrInvalid) {
			t.Fatalf("an import of an export that failed at %d bytes gave %v", room, err)
		}
		if got := string(exportOf(t, db)); got != emptyExport {
			t.Fatalf("an import of an export that failed at %d bytes kept\n%.300s", room, got)
		}
	}
}

// TestReadsWaitForAnImport runs an import from a pipe, a line at a time.
// Until the import reads its first table, a read through the same handle
// goes ahead. From then until the commit, reads through that handle wait,
// as they do for any Update, and then see the whole import. A read through
// another handle, as another process would make, goes ahead throughout and
// sees nothing of the import until its commit.
func TestReadsWaitForAnImport(t *testing.T) {
	dir := t.TempDir()
	src := open(t, filepath.Join(dir, "src.hcx"))
	ok(t, src.Put("docs:1", hc.Fields{"n": 1}))
	ok(t, src.Put("docs:2", hc.Fields{"n": 2}))
	lines := strings.SplitAfter(string(exportOf(t, src)), "\n")
	path := filepath.Join(dir, "test.hcx")
	db := open(t, path)
	other := open(t, path)
	pr, pw := io.Pipe()
	imported := make(chan error, 1)
	go func() { imported <- db.Import(pr) }()
	write := func(line string) {
		t.Helper()
		_, err := io.WriteString(pw, line)
		ok(t, err)
	}
	read := func(h *hc.DB) chan string {
		c := make(chan string, 1)
		go func() {
			f, err := h.Get("docs:1")
			c <- fmt.Sprint(f["n"], " ", describeErr(err))
		}()
		return c
	}
	write(lines[0])
	time.Sleep(20 * time.Millisecond) // the import holds the write lock now, and waits for its next line
	if got := <-read(db); got != "<nil> not found: hypercrux: not found: docs:1" {
		t.Fatalf("before the import's first table, a read gave %s", got)
	}
	write(lines[1]) // the table
	write(lines[2]) // docs:1
	waiting := read(db)
	time.Sleep(50 * time.Millisecond)
	select {
	case got := <-waiting:
		t.Fatalf("after the import's first table, a read through its handle went ahead and gave %s", got)
	default:
	}
	if got := <-read(other); got != "<nil> not found: hypercrux: not found: docs:1" {
		t.Fatalf("during the import, a read through another handle gave %s", got)
	}
	for _, l := range lines[3:] {
		if l != "" {
			write(l)
		}
	}
	ok(t, pw.Close())
	ok(t, <-imported)
	if got := <-waiting; got != "1 ok" {
		t.Fatalf("the read that waited for the import gave %s", got)
	}
	if got := <-read(other); got != "1 ok" {
		t.Fatalf("after the import, a read through another handle gave %s", got)
	}
}

// TestZeroxToTheBetaAndBack is G6's closing test: going from 0.x to the
// Beta and back gives a byte-identical export. It takes A2's random files,
// with awkward values among them: subnormals, -0, the extremes, bytes and
// Unicode. There are 40, or 8 with -short.
//
// Each is filled through 0.x's API, and its export goes into a new Beta
// database. The Beta's export is the same bytes, before and after a
// reopen, which reads the import back from the file. That export goes
// into a new 0.x file, whose export is the same bytes again, and which
// holds every value with the same SQLite type and bits as the first file.
// Every key reads the same from 0.x and from the Beta, with its links both
// ways, and so do each table's scan and Check's counts.
func TestZeroxToTheBetaAndBack(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	var tables, records, vectors, links int
	for i := 0; i < rounds; i++ {
		r := rand.New(rand.NewPCG(uint64(i), 99))
		dir := t.TempDir()
		a := openZero(t, filepath.Join(dir, "a.db"))
		fillRandom(t, zeroOn{a}, r)
		first := exportOf(t, a)

		path := filepath.Join(dir, "b.hcx")
		b := open(t, path)
		ok(t, b.Import(bytes.NewReader(first)))
		if second := exportOf(t, b); !bytes.Equal(first, second) {
			t.Fatalf("round %d: the Beta's export differs from 0.x's:\n%s\n---\n%s", i, first, second)
		}
		ok(t, b.Close())
		b = open(t, path)
		second := exportOf(t, b)
		if !bytes.Equal(first, second) {
			t.Fatalf("round %d: after a reopen, the Beta's export differs from 0.x's:\n%s\n---\n%s", i, first, second)
		}
		sameAs0x(t, a, b, keysOf(t, first))

		c := openZero(t, filepath.Join(dir, "c.db"))
		ok(t, c.Import(bytes.NewReader(second)))
		if third := exportOf(t, c); !bytes.Equal(first, third) {
			t.Fatalf("round %d: back in 0.x, the export differs:\n%s\n---\n%s", i, first, third)
		}
		if da, dc := dump(t, a), dump(t, c); da != dc {
			t.Fatalf("round %d: back in 0.x, the file differs:\n%s\n---\n%s", i, da, dc)
		}
		rep, err := b.Check()
		ok(t, err)
		tables, records, vectors, links = tables+rep.Tables, records+rep.Records, vectors+rep.Vectors, links+rep.Links
	}
	t.Logf("%d files, with %d tables, %d records, %d vectors and %d links", rounds, tables, records, vectors, links)
}

// TestTheBetaToZeroxAndBack goes the other way, with A2's generator filling
// a Beta database from seeds of its own, 20 of them or 4 with -short. The
// same calls on 0.x give the same export, byte for byte. The Beta's export
// goes into a new 0.x file, whose export is the same bytes, and that goes
// into a new Beta database, whose export is the same bytes again, before
// and after a reopen. Every key reads the same from the two engines.
func TestTheBetaToZeroxAndBack(t *testing.T) {
	rounds := 20
	if testing.Short() {
		rounds = 4
	}
	for i := 0; i < rounds; i++ {
		seed := uint64(1000 + i)
		dir := t.TempDir()
		a := open(t, filepath.Join(dir, "a.hcx"))
		fillRandom(t, betaOn{a}, rand.New(rand.NewPCG(seed, 99)))
		first := exportOf(t, a)
		twin := openZero(t, filepath.Join(dir, "twin.db"))
		fillRandom(t, zeroOn{twin}, rand.New(rand.NewPCG(seed, 99)))
		if made := exportOf(t, twin); !bytes.Equal(first, made) {
			t.Fatalf("seed %d: the same calls export as\n%s\nfrom 0.x, and as\n%s\nfrom the Beta", seed, made, first)
		}

		z := openZero(t, filepath.Join(dir, "z.db"))
		ok(t, z.Import(bytes.NewReader(first)))
		second := exportOf(t, z)
		if !bytes.Equal(first, second) {
			t.Fatalf("seed %d: 0.x's export differs from the Beta's:\n%s\n---\n%s", seed, first, second)
		}
		path := filepath.Join(dir, "c.hcx")
		c := open(t, path)
		ok(t, c.Import(bytes.NewReader(second)))
		if third := exportOf(t, c); !bytes.Equal(first, third) {
			t.Fatalf("seed %d: back in the Beta, the export differs:\n%s\n---\n%s", seed, first, third)
		}
		ok(t, c.Close())
		c = open(t, path)
		if third := exportOf(t, c); !bytes.Equal(first, third) {
			t.Fatalf("seed %d: back in the Beta, after a reopen, the export differs:\n%s\n---\n%s", seed, first, third)
		}
		sameAs0x(t, z, c, keysOf(t, first))
	}
}

// TestTheWidestTablesMoveBothWays imports a table of 1,999 fields, the most
// Put lets a table have in the Beta and the most 0.x's SQLite tables can
// hold, besides the key: it goes into both engines, and exports the same
// from both. One of 2,000 fields is refused by both, by 0.x's SQLite with
// an error of its own and by the Beta with Put's limit, and the Beta keeps
// nothing. 0.x takes seconds over each, as it adds the columns one at a
// time, so with -short only the Beta has them.
func TestTheWidestTablesMoveBothWays(t *testing.T) {
	const hdr = `{"hypercrux":"export","version":1}` + "\n"
	names := make([]string, 2000)
	for i := range names {
		names[i] = strconv.Quote("f" + strconv.Itoa(i))
	}
	dir := t.TempDir()
	z := openZero(t, filepath.Join(dir, "zero.db"))
	path := filepath.Join(dir, "beta.hcx")
	b := open(t, path)
	in := hdr + `{"table":"docs","dims":null,"fields":[` + strings.Join(names, ",") + `]}` + "\n" + `{"end":{"tables":1,"records":0,"links":0}}` + "\n"
	if err := b.Import(strings.NewReader(in)); describeErr(err) != "invalid: hypercrux: invalid: line 2 of the export: table docs has 2000 fields, and a table holds at most 1999" {
		t.Fatalf("a table of 2,000 fields: the Beta gives %v", err)
	}
	if got := string(exportOf(t, b)); got != emptyExport {
		t.Fatalf("a refused table of 2,000 fields left\n%.300s", got)
	}
	if !testing.Short() {
		if err := z.Import(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), "too many columns") {
			t.Fatalf("a table of 2,000 fields: 0.x gives %v", err)
		}
	}
	in = hdr + `{"table":"docs","dims":null,"fields":[` + strings.Join(names[:1999], ",") + `]}` + "\n" +
		`{"key":"docs:1","fields":{"f1998":1}}` + "\n" + `{"end":{"tables":1,"records":1,"links":0}}` + "\n"
	ok(t, b.Import(strings.NewReader(in)))
	if got := string(exportOf(t, b)); got != in {
		t.Fatalf("a table of 1,999 fields exports from the Beta as\n%.300s", got)
	}
	if !testing.Short() {
		ok(t, z.Import(strings.NewReader(in)))
		if got := string(exportOf(t, z)); got != in {
			t.Fatalf("a table of 1,999 fields exports from 0.x as\n%.300s", got)
		}
	}
}
