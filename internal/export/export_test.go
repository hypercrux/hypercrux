// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRealForms(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1, "1.0"},
		{-1, "-1.0"},
		{1.5, "1.5"},
		{0.1, "0.1"},
		{100, "100.0"},
		{123456789, "123456789.0"},
		{9007199254740993, "9007199254740992.0"},
		{1e20, "100000000000000000000.0"},
		{1e21, "1e+21"},
		{1.5e300, "1.5e+300"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{1.2345e-7, "1.2345e-7"},
		{1e-10, "1e-10"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{-math.MaxFloat64, "-1.7976931348623157e+308"},
		{math.SmallestNonzeroFloat64, "5e-324"},
		{2.2250738585072014e-308, "2.2250738585072014e-308"},
	} {
		if got := string(appendReal(nil, c.f)); got != c.want {
			t.Errorf("%v: got %s, want %s", c.f, got, c.want)
		}
		back, err := number(c.want)
		if err != nil || math.Float64bits(back.(float64)) != math.Float64bits(c.f) {
			t.Errorf("%s reads back as %v, %v", c.want, back, err)
		}
	}
	for _, c := range []struct {
		f    float32
		want string
	}{
		{0, "0"},
		{float32(math.Copysign(0, -1)), "-0"},
		{1, "1"},
		{0.1, "0.1"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{math.MaxFloat32, "3.4028235e+38"},
		{math.SmallestNonzeroFloat32, "1e-45"},
		{1.17549435e-38, "1.1754944e-38"},
	} {
		if got := string(appendFloat(nil, float64(c.f), 32)); got != c.want {
			t.Errorf("float32 %v: got %s, want %s", c.f, got, c.want)
		}
	}
}

func TestStringForms(t *testing.T) {
	s := "a\"b\\c/\n\r\t\b\f\x00\x1f\x7f é ü 中文 😀   �"
	want := `"a\"b\\c/\n\r\t\b\f\u0000\u001f` + "\x7f é ü 中文 😀   �\""
	got := string(appendString(nil, s))
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	var back string
	if err := json.Unmarshal([]byte(got), &back); err != nil || back != s {
		t.Fatalf("encoding/json reads it as %q, %v", back, err)
	}
	p := parser{s: got}
	if mine, err := p.str(); err != nil || mine != s {
		t.Fatalf("the parser reads it as %q, %v", mine, err)
	}
}

// awkward floats and integers, alongside random ones.
var (
	awkwardReals = []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 1e21, 1e-7, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308, 4.9e-324 * 3, 1 << 53, 1<<53 + 2}
	awkwardInts = []int64{0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1}
	awkwardF32  = []float32{float32(math.Copysign(0, -1)), math.MaxFloat32, -math.MaxFloat32,
		math.SmallestNonzeroFloat32, 1.17549435e-38, 1e-7, 0.1, 1, 16777217}
	awkwardText = []string{"", " ", "\x00", "a\x00b", "\"", "\\", "\n\r\t", "  ", "é", "😀", "�",
		"é", "שלום", "مرحبا", "{\"base64\":\"AA==\"}", "[1,2]", "null", "1.0"}
)

func randomString(r *rand.Rand) string {
	if r.IntN(3) == 0 {
		return awkwardText[r.IntN(len(awkwardText))]
	}
	var b []byte
	for n := r.IntN(12); n > 0; n-- {
		var c rune
		switch r.IntN(5) {
		case 0:
			c = rune(r.IntN(0x20))
		case 1:
			c = rune(0x20 + r.IntN(0x60))
		case 2:
			c = rune(0x80 + r.IntN(0x800))
		case 3:
			c = rune(0xe000 + r.IntN(0x2000))
		default:
			c = rune(0x10000 + r.IntN(0x100000))
		}
		b = utf8.AppendRune(b, c)
	}
	return string(b)
}

func randomValue(r *rand.Rand, vec bool) any {
	if vec {
		v := make([]float32, 1+r.IntN(6))
		for i := range v {
			if r.IntN(3) == 0 {
				v[i] = awkwardF32[r.IntN(len(awkwardF32))]
				continue
			}
			for {
				v[i] = math.Float32frombits(r.Uint32())
				if !math.IsNaN(float64(v[i])) && !math.IsInf(float64(v[i]), 0) {
					break
				}
			}
		}
		return v
	}
	switch r.IntN(4) {
	case 0:
		if r.IntN(2) == 0 {
			return awkwardInts[r.IntN(len(awkwardInts))]
		}
		return int64(r.Uint64())
	case 1:
		if r.IntN(2) == 0 {
			return awkwardReals[r.IntN(len(awkwardReals))]
		}
		for {
			f := math.Float64frombits(r.Uint64())
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f
			}
		}
	case 2:
		return randomString(r)
	}
	b := make([]byte, r.IntN(9))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

type contents struct {
	tables  []Table
	records []Record
	links   []Link
}

// randomContents makes tables, records and links in the order a Writer
// takes them.
func randomContents(r *rand.Rand) contents {
	var c contents
	names := map[string]bool{}
	for n := r.IntN(4); n > 0; n-- {
		names[string(rune('a'+r.IntN(6)))+randomName(r)] = true
	}
	for name := range names {
		t := Table{Name: name}
		if r.IntN(2) == 0 {
			t.Dims = 1 + r.IntN(6)
		}
		seen := map[string]bool{}
		for n := r.IntN(5); n > 0; n-- {
			f := randomName(r)
			if !seen[strings.ToLower(f)] {
				seen[strings.ToLower(f)] = true
				t.Fields = append(t.Fields, f)
			}
		}
		if r.IntN(2) == 0 {
			at := r.IntN(len(t.Fields) + 1)
			t.Fields = append(t.Fields[:at], append([]string{"vec"}, t.Fields[at:]...)...)
		}
		c.tables = append(c.tables, t)
	}
	sort.Slice(c.tables, func(i, j int) bool { return c.tables[i].Name < c.tables[j].Name })
	var keys []string
	for _, t := range c.tables {
		ids := map[string]bool{}
		for n := r.IntN(6); n > 0; n-- {
			ids[t.Name+":"+randomString(r)+"x"] = true
		}
		var ks []string
		for k := range ids {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			rec := Record{Key: k}
			for _, f := range t.Fields {
				if r.IntN(3) > 0 {
					rec.Fields = append(rec.Fields, Field{Name: f, Value: randomValue(r, f == "vec")})
				}
			}
			c.records = append(c.records, rec)
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		seen := map[Link]bool{}
		for n := r.IntN(8); n > 0; n-- {
			l := Link{From: keys[r.IntN(len(keys))], Type: randomString(r) + "t", To: keys[r.IntN(len(keys))]}
			if !seen[l] {
				seen[l] = true
				c.links = append(c.links, l)
			}
		}
		sort.Slice(c.links, func(i, j int) bool { return linkLess(c.links[i], c.links[j]) })
	}
	return c
}

func randomName(r *rand.Rand) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABC_0123456789"
	b := []byte{"abcdefghijklmnop"[r.IntN(16)]}
	for n := r.IntN(6); n > 0; n-- {
		b = append(b, letters[r.IntN(len(letters))])
	}
	return string(b)
}

func write(t *testing.T, c contents) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, tb := range c.tables {
		if err := w.Table(tb); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range c.records {
		if err := w.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range c.links {
		if err := w.Link(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func read(r io.Reader) (contents, error) {
	var c contents
	rd, err := NewReader(r)
	if err != nil {
		return c, err
	}
	for {
		item, err := rd.Next()
		if err == io.EOF {
			return c, nil
		}
		if err != nil {
			return c, err
		}
		switch x := item.(type) {
		case Table:
			c.tables = append(c.tables, x)
		case Record:
			c.records = append(c.records, x)
		case Link:
			c.links = append(c.links, x)
		}
	}
}

// same compares two contents, floats by their bits so -0 stays -0.
func same(a, b contents) bool {
	if len(a.tables) != len(b.tables) || len(a.records) != len(b.records) || !reflect.DeepEqual(a.links, b.links) {
		return false
	}
	for i := range a.tables {
		ta, tb := a.tables[i], b.tables[i]
		if ta.Name != tb.Name || ta.Dims != tb.Dims || strings.Join(ta.Fields, "\x00") != strings.Join(tb.Fields, "\x00") {
			return false
		}
	}
	for i := range a.records {
		ra, rb := a.records[i], b.records[i]
		if ra.Key != rb.Key || len(ra.Fields) != len(rb.Fields) {
			return false
		}
		for j := range ra.Fields {
			fa, fb := ra.Fields[j], rb.Fields[j]
			if fa.Name != fb.Name || !sameValue(fa.Value, fb.Value) {
				return false
			}
		}
	}
	return true
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case []float32:
		y, ok := b.([]float32)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
		return true
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}

func TestRandomRoundTrips(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 2000; i++ {
		c := randomContents(r)
		out := write(t, c)
		back, err := read(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("round %d: %v\n%s", i, err, out)
		}
		if !same(c, back) {
			t.Fatalf("round %d: read back differently\n%s", i, out)
		}
		if again := write(t, back); !bytes.Equal(out, again) {
			t.Fatalf("round %d: written again differently\n%s\n%s", i, out, again)
		}
		// Every line is JSON that encoding/json takes too.
		for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
			if !json.Valid(line) {
				t.Fatalf("round %d: not JSON: %s", i, line)
			}
		}
	}
}

func TestTheFormatByExample(t *testing.T) {
	c := contents{
		tables: []Table{
			{Name: "customer", Fields: []string{"name"}},
			{Name: "docs", Dims: 3, Fields: []string{"title", "vec", "pages", "score", "scan"}},
		},
		records: []Record{
			{Key: "customer:42", Fields: []Field{{"name", "Dana"}}},
			{Key: "docs:1", Fields: []Field{{"title", "Q3 plan"}, {"vec", []float32{0.9, 0.1, 0}}, {"pages", int64(3)}, {"score", 1.0}}},
			{Key: "docs:2", Fields: []Field{{"scan", []byte{0, 1, 2, 0xff}}}},
		},
		links: []Link{{"customer:42", "owns", "docs:1"}, {"customer:42", "owns", "docs:2"}},
	}
	want := `{"hypercrux":"export","version":1}
{"table":"customer","dims":null,"fields":["name"]}
{"table":"docs","dims":3,"fields":["title","vec","pages","score","scan"]}
{"key":"customer:42","fields":{"name":"Dana"}}
{"key":"docs:1","fields":{"title":"Q3 plan","vec":[0.9,0.1,0],"pages":3,"score":1.0}}
{"key":"docs:2","fields":{"scan":{"base64":"AAEC/w=="}}}
{"from":"customer:42","type":"owns","to":"docs:1"}
{"from":"customer:42","type":"owns","to":"docs:2"}
{"end":{"tables":2,"records":3,"links":2}}
`
	if got := string(write(t, c)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Members can come in any order, with spaces, and a field can be null.
	loose := `{ "version": 1, "hypercrux": "export" }
{"fields": ["name"], "dims": null, "table": "customer"}
{"table":"docs","dims":3,"fields":["title","vec","pages","score","scan"]}
{"fields": {"name": "Dana"}, "key": "customer:42"}
{"key":"docs:1","fields":{"score":1.0,"pages":3,"vec":[0.9,1e-1,0],"title":"Q3 plan","scan":null}}
{"key":"docs:2","fields":{"scan":{"base64":"AAEC/w=="}}}
{"to":"docs:1","from":"customer:42","type":"owns"}
{"from":"customer:42","type":"owns","to":"docs:2"}
{"end":{"links":2,"tables":2,"records":3}}`
	back, err := read(strings.NewReader(loose))
	if err != nil {
		t.Fatal(err)
	}
	if !same(c, back) {
		t.Fatalf("the loose form reads differently: %+v", back)
	}
}

func TestReaderRefuses(t *testing.T) {
	const hdr = `{"hypercrux":"export","version":1}` + "\n"
	const docs = `{"table":"docs","dims":2,"fields":["title","vec","n"]}` + "\n"
	const end0 = `{"end":{"tables":0,"records":0,"links":0}}` + "\n"
	const end1 = `{"end":{"tables":1,"records":1,"links":0}}` + "\n"
	rec := func(fields string) string { return docs + `{"key":"docs:1","fields":` + fields + "}\n" + end1 }
	for _, c := range []struct{ in, want string }{
		{"", "the input is empty"},
		{"\n", "one JSON object"},
		{`{"hypercrux":"export"}` + "\n", "starts with the line"},
		{`{"hypercrux":"import","version":1}` + "\n", "starts with the line"},
		{`{"hypercrux":"export","version":2}` + "\n" + end0, "format version 2"},
		{`{"hypercrux":"export","version":1,"by":"me"}` + "\n", "starts with the line"},
		{hdr + `{"table":"docs","dims":null,"fields":[],"by":"me"}` + "\n", `"by", which doesn't belong`},
		{hdr, "before its end line"},
		{hdr + docs, "before its end line"},
		{hdr + end1, "counts 1 tables"},
		{hdr + end0 + docs, "something follows the end line"},
		{hdr + end0 + "\n", "one JSON object"},
		{hdr + hdr, "a second first line"},
		{hdr + `{"tabel":"docs"}` + "\n", "isn't a table, a record"},
		{hdr + docs + docs, "there twice"},
		{hdr + `{"table":"docs","dims":0,"fields":[]}` + "\n", "vector size of 0"},
		{hdr + `{"table":"docs","dims":2.0,"fields":[]}` + "\n", "vector size of 2.0"},
		{hdr + `{"table":"docs","dims":null,"fields":["a","A"]}` + "\n", "twice, counting upper and lower case"},
		{hdr + `{"table":"do:cs","dims":null,"fields":[]}` + "\n", "without a colon"},
		{hdr + `{"table":"docs","dims":null}` + "\n", `no "fields"`},
		{hdr + `{"key":"docs:1","fields":{}}` + "\n", "which the export doesn't list"},
		{hdr + docs + `{"key":"docs","fields":{}}` + "\n", "has no table"},
		{hdr + docs + `{"key":"docs:1","fields":{}}` + "\n" + docs, "tables come first"},
		{hdr + docs + `{"key":"docs:1","fields":{}}` + "\n" + `{"from":"docs:1","type":"x","to":"docs:1"}` + "\n" +
			`{"key":"docs:2","fields":{}}` + "\n", "after the links"},
		{hdr + rec(`{"size":1}`), `"size", which table docs doesn't list`},
		{hdr + rec(`{"title":1,"title":2}`), `"title" is there twice`},
		{hdr + rec(`{"n":true}`), "true and false"},
		{hdr + rec(`{"n":[1,2]}`), "only the field vec holds a list"},
		{hdr + rec(`{"vec":"[1,2]"}`), "the vector is a list"},
		{hdr + rec(`{"vec":[1,"2"]}`), "vector value 1 isn't a number"},
		{hdr + rec(`{"vec":[1,1e39]}`), "out of float32's range"},
		{hdr + rec(`{"n":9223372036854775808}`), "integer 9223372036854775808 is out of range"},
		{hdr + rec(`{"n":1e309}`), "number 1e309 is out of range"},
		{hdr + rec(`{"n":01}`), "comma or a closing brace"},
		{hdr + rec(`{"n":1.}`), "digit goes after a decimal point"},
		{hdr + rec(`{"n":+1}`), "can't start with"},
		{hdr + rec(`{"n":NaN}`), "can't start with"},
		{hdr + rec(`{"n":{"bytes":"AA=="}}`), "holds bytes"},
		{hdr + rec(`{"n":{"base64":"AA"}}`), "standard base64"},
		{hdr + rec(`{"n":{"base64":"AB=="}}`), "standard base64"},
		{hdr + rec(`{"n":{"base64":"A\nA=="}}`), "standard base64"},
		{hdr + rec(`{"title":"a`+"\x01"+`b"}`), "control character"},
		{hdr + rec(`{"title":"\ud800"}`), "half of a surrogate pair"},
		{hdr + rec(`{"title":"\udc00\ud800"}`), "half of a surrogate pair"},
		{hdr + rec(`{"title":"\x"}`), "unknown escape"},
		{hdr + rec(`{"title":"\u12"}`), "four hex digits"},
		{hdr + rec(`{"title":"abc}}`), "control character"},
		{hdr + docs + `{"key":"docs:1","fields":{"title":"abc`, "past the end of the line"},
		{hdr + rec(`{"title":{"a":{"b":[1]}}}`), "nested too deeply"},
		{hdr + docs + "{\"key\":\"docs:\xff\",\"fields\":{}}\n", "isn't valid UTF-8"},
		{hdr + `{"from":"a:1","type":7,"to":"a:2"}` + "\n", "are text"},
		{hdr + `{"end":{"tables":0,"records":0}}` + "\n", `no "links"`},
		{hdr + `{"end":{"tables":-1,"records":0,"links":0}}` + "\n", "whole number"},
		{hdr + `{"end":{"tables":0,"records":0,"links":0}} {}` + "\n", "something follows the JSON object"},
	} {
		_, err := read(strings.NewReader(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("input:\n%s\ngot error %v, want one with %q", c.in, err, c.want)
			continue
		}
		if !errors.Is(err, ErrFormat) {
			t.Errorf("%v isn't an ErrFormat", err)
		}
	}
	// Lines can end in CR LF, and the last can end without a line break.
	if _, err := read(strings.NewReader(strings.ReplaceAll(hdr+end0, "\n", "\r\n"))); err != nil {
		t.Errorf("CR LF: %v", err)
	}
	if _, err := read(strings.NewReader(hdr + strings.TrimSuffix(end0, "\n"))); err != nil {
		t.Errorf("no last line break: %v", err)
	}
	// Numbers too small for a float become zero or a subnormal, as JSON
	// readers usually do.
	if back, err := read(strings.NewReader(hdr + rec(`{"n":1e-400}`))); err != nil || back.records[0].Fields[0].Value != 0.0 {
		t.Errorf("1e-400: %v %v", back, err)
	}
}

func TestWriterRefuses(t *testing.T) {
	docs := Table{Name: "docs", Dims: 2, Fields: []string{"a", "vec", "b"}}
	for _, c := range []struct {
		do   func(w *Writer) error
		want string
	}{
		{func(w *Writer) error { w.Table(Table{Name: "b"}); return w.Table(Table{Name: "a"}) }, "go in order of name"},
		{func(w *Writer) error { w.Table(Table{Name: "a"}); return w.Table(Table{Name: "a"}) }, "go in order of name"},
		{func(w *Writer) error { return w.Table(Table{Name: "a:b"}) }, "table name"},
		{func(w *Writer) error { return w.Table(Table{Name: "a", Fields: []string{"x", "X"}}) }, "repeated"},
		{func(w *Writer) error { return w.Record(Record{Key: "docs:1"}) }, "which wasn't written"},
		{func(w *Writer) error {
			w.Table(docs)
			w.Record(Record{Key: "docs:2"})
			return w.Record(Record{Key: "docs:1"})
		}, "go in order of key"},
		{func(w *Writer) error {
			w.Table(Table{Name: "a"})
			w.Table(docs)
			w.Record(Record{Key: "docs:1"})
			return w.Record(Record{Key: "a:1"})
		}, "table by table"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"b", int64(1)}, {"a", int64(1)}}})
		}, "comes before another"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"c", int64(1)}}})
		}, "isn't one of its fields"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"a", math.Inf(1)}}})
		}, "+Inf has no form"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"a", "\xff"}}})
		}, "isn't valid UTF-8"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"a", []float32{1}}}})
		}, "only the field vec"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"vec", []float32{float32(math.NaN())}}}})
		}, "NaN, which has no form"},
		{func(w *Writer) error {
			w.Table(docs)
			return w.Record(Record{Key: "docs:1", Fields: []Field{{"a", true}}})
		}, "a bool has no form"},
		{func(w *Writer) error {
			w.Link(Link{"a:2", "x", "a:1"})
			return w.Link(Link{"a:1", "x", "a:2"})
		}, "go in order of from, type and to"},
		{func(w *Writer) error { w.Link(Link{"a:1", "x", "a:2"}); return w.Table(docs) }, "tables come first"},
		{func(w *Writer) error { w.Close(); return w.Link(Link{"a:1", "x", "a:2"}) }, "closed"},
	} {
		w := NewWriter(io.Discard)
		if err := c.do(w); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %v, want an error with %q", err, c.want)
		}
	}
}

// FuzzReader feeds the reader anything at all. It mustn't panic, and an
// export it reads must write and read back the same.
func FuzzReader(f *testing.F) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		c := randomContents(r)
		for _, t := range c.tables {
			w.Table(t)
		}
		for _, rec := range c.records {
			w.Record(rec)
		}
		for _, l := range c.links {
			w.Link(l)
		}
		w.Close()
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		c, err := read(bytes.NewReader(in))
		if err != nil {
			return
		}
		// Put what was read in the order the writer takes.
		sort.SliceStable(c.tables, func(i, j int) bool { return c.tables[i].Name < c.tables[j].Name })
		place := map[string]int{}
		for i, tb := range c.tables {
			place[tb.Name] = i
		}
		sort.SliceStable(c.records, func(i, j int) bool {
			ti, _ := tableOf(c.records[i].Key)
			tj, _ := tableOf(c.records[j].Key)
			if place[ti] != place[tj] {
				return place[ti] < place[tj]
			}
			return c.records[i].Key < c.records[j].Key
		})
		sort.SliceStable(c.links, func(i, j int) bool { return linkLess(c.links[i], c.links[j]) })
		var buf bytes.Buffer
		w := NewWriter(&buf)
		for _, tb := range c.tables {
			w.Table(tb)
		}
		for _, rec := range c.records {
			w.Record(rec)
		}
		for _, l := range c.links {
			w.Link(l)
		}
		if err := w.Close(); err != nil {
			return // repeated keys or links, which the engine refuses
		}
		back, err := read(bytes.NewReader(buf.Bytes()))
		if err != nil || !same(c, back) {
			t.Fatalf("written from what was read, it reads back as %v, %v\n%s", back, err, buf.Bytes())
		}
	})
}

// FuzzLine checks the parser against encoding/json: whatever it accepts is
// valid JSON, and the strings come out the same.
func FuzzLine(f *testing.F) {
	for _, s := range []string{`{}`, `{"a":"bé😀"}`, `{"a":[1,-0.5e3,null,true]}`, `{"a":{"b":"\n"}}`, `{"a":"\/"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		v, err := parseLine(s)
		if err != nil {
			return
		}
		if !json.Valid([]byte(s)) {
			t.Fatalf("accepted %q, which isn't JSON", s)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		for _, mem := range v.obj {
			if mem.value.kind == kString && m[mem.name] != mem.value.s {
				t.Fatalf("%q: member %q is %q here and %q to encoding/json", s, mem.name, mem.value.s, m[mem.name])
			}
		}
	})
}
