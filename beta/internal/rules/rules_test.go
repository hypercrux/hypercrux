// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package rules_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

func invalid(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("%s: got %v, want an error that wraps ErrInvalid", what, err)
	}
}

func fine(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

// TestKeys holds the keys of 0.x's KeyAndFieldRules and
// RulesForLongKeysAndLinkTypes, and the differential harness's bad keys,
// with the limits on both sides.
func TestKeys(t *testing.T) {
	long := "docs:" + strings.Repeat("x", rules.MaxKeyLen-len("docs:"))
	for _, key := range []string{"", "docs", "docs:", ":7", "Docs:7", "7docs:1", "hc_keys:1", "sqlite_master:1",
		"my-docs:1", "docs:\x00", "docs:" + strings.Repeat("x", rules.MaxKeyLen), "docs:\xff",
		"nocolon", "docs:a\x00b", "hc_x:1", long + "x", "_docs:1", "dócs:1", " docs:1", "docs\n:1", "hc_:1"} {
		_, err := rules.TableOf(key)
		invalid(t, "TableOf("+strconv.Quote(key)+")", err)
	}
	for key, table := range map[string]string{
		"docs:7": "docs", "docs:a:b:c": "docs", "docs:2026-10-06/report.pdf": "docs", "docs:Ünïcode ✓": "docs",
		"x:1": "x", long: "docs", "hc:1": "hc", "keys:1": "keys", "links:1": "links", "docs_vec:1": "docs_vec",
		"t_1: ": "t_1", "docs::": "docs", "docs:\n": "docs", strings.Repeat("a", 63) + ":1": strings.Repeat("a", 63),
	} {
		got, err := rules.TableOf(key)
		if err != nil || got != table {
			t.Errorf("TableOf(%q) = %q, %v, want %q", key, got, err, table)
		}
	}
	if len(long) != rules.MaxKeyLen {
		t.Fatalf("the longest key has %d bytes", len(long))
	}
}

// TestTheMessagesTestsLookFor pins 0.x's wording where 0.x's tests, or its
// command's, look for it.
func TestTheMessagesTestsLookFor(t *testing.T) {
	_, noTable := rules.TableOf("nocolon")
	_, upper := rules.TableOf("Docs:1")
	_, empty := rules.TableOf("docs:")
	_, long := rules.TableOf("docs:" + strings.Repeat("x", 1024))
	_, dirty := rules.TableOf("docs:\xff")
	for _, c := range []struct {
		err  error
		want string
	}{
		{noTable, `hypercrux: invalid: key "nocolon" has no table: write it as table:id, such as docs:7`},
		{upper, `key "Docs:1": hypercrux: invalid: table name "Docs": use lower-case letters, digits and underscores, starting with a letter`},
		{empty, `hypercrux: invalid: key "docs:" has nothing after the colon`},
		{long, `hypercrux: invalid: key is 1029 bytes, more than 1024`},
		{dirty, `hypercrux: invalid: key "docs:\xff" isn't clean UTF-8 text`},
		{rules.Field("1st"), `hypercrux: invalid: field name "1st": use letters, digits and underscores, starting with a letter, and not key or rowid`},
		{rules.CaseClash("A"), `hypercrux: invalid: fields "A" and another differ only in case`},
		{rules.LinkType(""), `hypercrux: invalid: link type "": use 1 to 200 characters`},
		{rules.Vector(nil), `hypercrux: invalid: a vector has 1 to 65536 values, not 0`},
		{rules.Vector([]float32{1, float32(math.NaN())}), `hypercrux: invalid: vector value 1 is NaN`},
		{rules.Vector([]float32{float32(math.Inf(-1))}), `hypercrux: invalid: vector value 0 is -Inf`},
		{rules.Vector([]float32{0, 0}), `hypercrux: invalid: a vector of only zeros has no direction to compare`},
		{rules.VectorElsewhere("emb"), `hypercrux: invalid: field emb holds a vector; a record's vector goes in the field vec`},
		{rules.Text("a", "\xff"), `hypercrux: invalid: field a isn't valid UTF-8; store bytes as []byte`},
		{rules.Real("a", math.Inf(1)), `hypercrux: invalid: field a is +Inf`},
		{rules.Real("a", math.NaN()), `hypercrux: invalid: field a is NaN`},
	} {
		if c.err == nil || c.err.Error() != c.want {
			t.Errorf("got  %v\nwant %s", c.err, c.want)
		}
		invalid(t, c.want, c.err)
	}
}

func TestTableNames(t *testing.T) {
	for _, name := range []string{"docs", "x", "t_1", "keys", "links", "docs_vec", "hc", "hcx", "sqlite", "a9",
		strings.Repeat("a", rules.MaxTableLen)} {
		fine(t, "Table("+name+")", rules.Table(name))
	}
	for _, name := range []string{"", "Docs", "7docs", "my-docs", "hc_keys", "hc_", "sqlite_master", "sqlite_",
		"_docs", "docs:", "dócs", " docs", "docs\n", strings.Repeat("a", rules.MaxTableLen+1)} {
		invalid(t, "Table("+strconv.Quote(name)+")", rules.Table(name))
	}
}

func TestFieldNames(t *testing.T) {
	for _, name := range []string{"a", "A", "_x", "_", "title", "Title", "a1", "vec", "VEC", "keys", "rowids",
		"key_", "_key", "oid1", strings.Repeat("x", rules.MaxFieldLen)} {
		fine(t, "Field("+name+")", rules.Field(name))
	}
	for _, name := range []string{"", "key", "KEY", "Key", "rowid", "ROWID", "RowId", "oid", "OID", "_rowid_", "_ROWID_",
		"has space", "1st", "a-b", "é", "\u212aey", "a\x00", "title\n", strings.Repeat("x", rules.MaxFieldLen+1)} {
		invalid(t, "Field("+strconv.Quote(name)+")", rules.Field(name))
	}
}

func TestLinkTypes(t *testing.T) {
	for _, typ := range []string{"owns", "x", "é", "a b", strings.Repeat("ü", 150), strings.Repeat("ü", rules.MaxLinkType),
		strings.Repeat("😀", rules.MaxLinkType), "a\x00", "a\x00b", ":", "key"} {
		fine(t, "LinkType("+strconv.Quote(typ)+")", rules.LinkType(typ))
	}
	for _, typ := range []string{"", strings.Repeat("x", rules.MaxLinkType+1), strings.Repeat("ü", rules.MaxLinkType+1),
		"\xff", "a\xff", "\x00", "\x00a"} {
		invalid(t, "LinkType("+strconv.Quote(typ)+")", rules.LinkType(typ))
	}
}

func TestVectors(t *testing.T) {
	negZero := float32(math.Copysign(0, -1))
	big := make([]float32, rules.MaxDims)
	big[rules.MaxDims-1] = 1
	for _, v := range [][]float32{{1}, {0, 1}, {negZero, math.Float32frombits(1)}, {math.MaxFloat32, -math.MaxFloat32}, big} {
		fine(t, "Vector", rules.Vector(v))
	}
	for _, v := range [][]float32{nil, {}, make([]float32, rules.MaxDims+1), {0, 0}, {negZero, negZero}, {0, negZero},
		{1, float32(math.NaN())}, {float32(math.Inf(1))}, {float32(math.Inf(-1)), 1}, {0, math.Float32frombits(0x7fc00001)}} {
		invalid(t, "Vector", rules.Vector(v))
	}
}

func TestTextAndReals(t *testing.T) {
	for _, s := range []string{"", "é", "a\x00b", "😀"} {
		fine(t, "Text", rules.Text("a", s))
	}
	for _, s := range []string{"\xff", "a\xc3", "\xed\xa0\x80"} {
		invalid(t, "Text", rules.Text("a", s))
	}
	for _, f := range []float64{0, math.Copysign(0, -1), math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64} {
		fine(t, "Real", rules.Real("a", f))
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		invalid(t, "Real", rules.Real("a", f))
	}
}

func TestCase(t *testing.T) {
	for in, want := range map[string]string{"TiTle": "title", "title": "title", "VEC": "vec", "_A1": "_a1", "": "",
		"\u212a": "\u212a", "É": "É"} {
		if got := rules.Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
	if n := testing.AllocsPerRun(10, func() { rules.Fold("title") }); n != 0 {
		t.Errorf("Fold of a name in lower case allocates %v times", n)
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"vec", "VEC", true}, {"Title", "tItLe", true}, {"", "", true}, {"vec", "vect", false}, {"k", "\u212a", false},
		{"a_", "A_", true}, {"[", "{", false}, {"@", "`", false},
	} {
		if got := rules.SameName(c.a, c.b); got != c.same {
			t.Errorf("SameName(%q, %q) = %v", c.a, c.b, got)
		}
	}
	for name, want := range map[string]bool{"vec": true, "Vec": true, "VEC": true, "vEc": true, "vec_": false,
		"vecs": false, "ve": false, "": false, "v\u212ac": false} {
		if got := rules.IsVec(name); got != want {
			t.Errorf("IsVec(%q) = %v", name, got)
		}
	}
}

// awkward builds a random string from pieces that sit on the edges of the
// rules.
func awkward(r *rand.Rand, pieces []string, most int) string {
	var b strings.Builder
	for n := r.IntN(most); n > 0; n-- {
		b.WriteString(pieces[r.IntN(len(pieces))])
	}
	return b.String()
}

var pieces = []string{"a", "z", "A", "Z", "0", "9", "_", ":", "-", " ", "\x00", "\xff", "é", "ü", "\u212a", "\n",
	"hc_", "sqlite_", "key", "KEY", "rowid", "oid", "_rowid_", "docs", "x", "😀"}

// TestTableOfAgrees0x runs 0.x's TableOf and this one on the same random
// keys: both must give the same table, or the same message.
func TestTableOfAgrees0x(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 7))
	n := 20000
	if testing.Short() {
		n = 2000
	}
	var tally tally
	for i := 0; i < n; i++ {
		key := awkward(r, pieces, 8)
		switch r.IntN(10) {
		case 0: // around the longest key
			key = "docs:" + strings.Repeat("x", rules.MaxKeyLen-6+r.IntN(3)) + key[:min(len(key), 1)]
		case 1: // around the longest table name
			key = strings.Repeat("t", rules.MaxTableLen-1+r.IntN(3)) + ":" + key
		case 2, 3, 4, 5, 6: // a good table, and anything after it
			key = []string{"docs", "x", "hc", "t_1", "keys"}[r.IntN(5)] + ":" + key
		}
		want, wantErr := hc.TableOf(key)
		got, err := rules.TableOf(key)
		if got != want || (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
			t.Fatalf("TableOf(%q) = %q, %v; 0.x gives %q, %v", key, got, err, want, wantErr)
		}
		if err != nil {
			invalid(t, "TableOf", err)
		}
		tally.add(err)
	}
	tally.check(t, "keys")
}

// tally counts what a random test took and refused, so a test whose
// inputs all fall on one side of a rule fails.
type tally struct{ took, refused int }

func (c *tally) add(err error) {
	if err == nil {
		c.took++
	} else {
		c.refused++
	}
}

func (c tally) check(t *testing.T, what string) {
	t.Helper()
	if n := c.took + c.refused; c.took < n/10 || c.refused < n/10 {
		t.Errorf("of %d %s, %d were taken and %d refused; the inputs should fall on both sides", n, what, c.took, c.refused)
	}
}

// TestNamesTypesAndVectorsAgree0x puts random field names, link types and
// vectors into 0.x and checks each against the rule here: 0.x takes it
// exactly when the rule does. The one difference is a link type that
// starts with a zero byte, which both refuse, 0.x with a plain error.
func TestNamesTypesAndVectorsAgree0x(t *testing.T) {
	db, err := hc.Open(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(2, 7))
	n := 400
	if testing.Short() {
		n = 100
	}
	refused := func(err error) bool { return err != nil }

	var names, types, vectors tally
	columns, table := 0, 0
	for i := 0; i < n; i++ {
		name := awkward(r, pieces, 5)
		if r.IntN(4) == 0 {
			name = strings.Repeat("f", rules.MaxFieldLen-1+r.IntN(3))
		}
		if rules.IsVec(name) { // vec takes a vector, which 1 isn't
			continue
		}
		if columns > 1500 { // 0.x's tables hold 2,000 columns, and every name tried may add one
			table, columns = table+1, 0
		}
		key := "names" + strconv.Itoa(table) + ":1"
		zeroxErr := db.Put(key, hc.Fields{name: 1})
		if refused(zeroxErr) != refused(rules.Field(name)) || zeroxErr != nil && !errors.Is(zeroxErr, hc.ErrInvalid) {
			t.Fatalf("field name %q: 0.x gives %v, and the rule %v", name, zeroxErr, rules.Field(name))
		}
		names.add(zeroxErr)
		columns++
	}
	names.check(t, "field names")

	if err := db.Put("links:1", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("links:2", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		typ := awkward(r, pieces, 6)
		if r.IntN(4) == 0 {
			typ = strings.Repeat("ü", rules.MaxLinkType-1+r.IntN(3))
		}
		zeroxErr := db.Link("links:1", typ, "links:2")
		err := rules.LinkType(typ)
		if refused(zeroxErr) != refused(err) {
			t.Fatalf("link type %q: 0.x gives %v, and the rule %v", typ, zeroxErr, err)
		}
		if zeroxErr != nil && !errors.Is(zeroxErr, hc.ErrInvalid) && !strings.HasPrefix(typ, "\x00") {
			t.Fatalf("link type %q: 0.x gives %v, a plain error, which only a zero byte first should give", typ, zeroxErr)
		}
		types.add(err)
	}
	types.check(t, "link types")

	awkwardFloats := []float32{0, float32(math.Copysign(0, -1)), 1, -1, math.Float32frombits(1), math.MaxFloat32,
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))}
	for i := 0; i < n; i++ {
		v := make([]float32, r.IntN(5))
		for j := range v {
			if r.IntN(3) == 0 {
				v[j] = float32(r.NormFloat64())
			} else {
				v[j] = awkwardFloats[r.IntN(len(awkwardFloats))]
			}
		}
		// A table for each size, so the size never decides.
		zeroxErr := db.Put("vecs"+strconv.Itoa(len(v))+":"+strconv.Itoa(i), hc.Fields{"vec": hc.Vector(v)})
		err := rules.Vector(v)
		if refused(zeroxErr) != refused(err) || zeroxErr != nil && zeroxErr.Error() != err.Error() {
			t.Fatalf("vector %v: 0.x gives %v, and the rule %v", v, zeroxErr, err)
		}
		vectors.add(err)
	}
	vectors.check(t, "vectors")
}
