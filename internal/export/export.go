// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package export reads and writes HyperCrux's export format: JSON lines that
// hold every record table, record and link of a database. EXPORT.md in the
// repository describes the format.
//
// The package is plain Go with nothing in it that belongs to one engine. The
// Writer checks the order of what it's given and writes every value in one
// form only, so two engines holding the same data write the same bytes.
package export

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Version is the version of the format this package writes, and the newest
// one it reads.
const Version = 1

// Table is a record table: its name, its vector size (0 when the table has
// none recorded), and its fields, which are its columns other than the key,
// in order. The vector's column, vec, is among the fields where it falls.
type Table struct {
	Name   string
	Dims   int
	Fields []string
}

// Field is one of a record's values: an int64, a float64, a string, a
// []byte, or a []float32 for the vector, in the field named vec.
type Field struct {
	Name  string
	Value any
}

// Record is a record's key and its fields that aren't NULL, in the order of
// its table's fields.
type Record struct {
	Key    string
	Fields []Field
}

// Link is a link from one record to another.
type Link struct {
	From, Type, To string
}

// Counts are how many tables, records and links an export holds.
type Counts struct {
	Tables, Records, Links int
}

// ErrFormat marks input that isn't a valid export.
var ErrFormat = errors.New("not a valid HyperCrux export")

const header = `{"hypercrux":"export","version":1}`

// tableOf returns the part of a key before its first colon.
func tableOf(key string) (string, bool) {
	i := strings.IndexByte(key, ':')
	if i <= 0 {
		return "", false
	}
	return key[:i], true
}

// isVec reports whether a field is the vector's.
func isVec(name string) bool { return strings.EqualFold(name, "vec") }

func linkLess(a, b Link) bool {
	if a.From != b.From {
		return a.From < b.From
	}
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	return a.To < b.To
}

// Writer writes an export. Give it the tables first, in order of name, then
// the records table by table in that order, each table's in order of key,
// then the links in order of From, Type and To, and then call Close. Names
// and keys are compared byte by byte. Anything out of order is an error,
// and so is a value the format can't hold, such as an infinite number.
type Writer struct {
	w      *bufio.Writer
	stage  int // 0 tables, 1 records, 2 links, 3 closed
	tables []Table
	index  map[string]int // a table's place in tables
	cur    int            // the table whose records are being written
	last   string         // the last table name or key written
	link   Link           // the last link written
	n      Counts
	buf    []byte
	err    error
}

// NewWriter starts an export on w with its first line.
func NewWriter(w io.Writer) *Writer {
	ew := &Writer{w: bufio.NewWriterSize(w, 1<<16), index: map[string]int{}, cur: -1}
	ew.buf = append(ew.buf, header...)
	ew.line()
	return ew
}

// Counts returns how much has been written so far.
func (w *Writer) Counts() Counts { return w.n }

func (w *Writer) fail(format string, args ...any) error {
	if w.err == nil {
		w.err = fmt.Errorf("export: "+format, args...)
	}
	return w.err
}

func (w *Writer) line() error {
	w.buf = append(w.buf, '\n')
	if _, err := w.w.Write(w.buf); err != nil && w.err == nil {
		w.err = err
	}
	w.buf = w.buf[:0]
	return w.err
}

func (w *Writer) usable() error {
	if w.err == nil && w.stage == 3 {
		w.err = errors.New("export: the export is closed")
	}
	return w.err
}

// Table writes a table.
func (w *Writer) Table(t Table) error {
	if err := w.usable(); err != nil {
		return err
	}
	switch {
	case w.stage != 0:
		return w.fail("table %s comes after records or links; tables come first", t.Name)
	case len(w.tables) > 0 && t.Name <= w.last:
		return w.fail("table %s comes after table %s; tables go in order of name", t.Name, w.last)
	case t.Name == "" || strings.IndexByte(t.Name, ':') >= 0 || !utf8.ValidString(t.Name):
		return w.fail("table name %q", t.Name)
	case t.Dims < 0:
		return w.fail("table %s has a vector size of %d", t.Name, t.Dims)
	}
	seen := map[string]bool{}
	for _, f := range t.Fields {
		l := strings.ToLower(f)
		if f == "" || !utf8.ValidString(f) || seen[l] {
			return w.fail("table %s: field name %q is empty, repeated or not UTF-8", t.Name, f)
		}
		seen[l] = true
	}
	w.index[t.Name] = len(w.tables)
	w.tables = append(w.tables, Table{Name: t.Name, Dims: t.Dims, Fields: append([]string(nil), t.Fields...)})
	w.last = t.Name
	w.n.Tables++

	b := append(w.buf, `{"table":`...)
	b = appendString(b, t.Name)
	b = append(b, `,"dims":`...)
	if t.Dims == 0 {
		b = append(b, "null"...)
	} else {
		b = strconv.AppendInt(b, int64(t.Dims), 10)
	}
	b = append(b, `,"fields":[`...)
	for i, f := range t.Fields {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, f)
	}
	w.buf = append(b, "]}"...)
	return w.line()
}

// Record writes a record. Its fields must come in its table's order.
func (w *Writer) Record(r Record) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.stage > 1 {
		return w.fail("record %s comes after the links", r.Key)
	}
	if !utf8.ValidString(r.Key) {
		return w.fail("key %q isn't UTF-8", r.Key)
	}
	tbl, ok := tableOf(r.Key)
	if !ok {
		return w.fail("key %q has no table", r.Key)
	}
	i, ok := w.index[tbl]
	if !ok {
		return w.fail("record %s is in table %s, which wasn't written", r.Key, tbl)
	}
	w.stage = 1
	switch {
	case i < w.cur:
		return w.fail("record %s comes after the records of table %s; records go table by table, in the tables' order", r.Key, w.tables[w.cur].Name)
	case i == w.cur && r.Key <= w.last:
		return w.fail("record %s comes after %s; records go in order of key", r.Key, w.last)
	}
	w.cur, w.last = i, r.Key
	t := &w.tables[i]

	b := append(w.buf, `{"key":`...)
	b = appendString(b, r.Key)
	b = append(b, `,"fields":{`...)
	at := 0
	for j, f := range r.Fields {
		k := at
		for k < len(t.Fields) && t.Fields[k] != f.Name {
			k++
		}
		if k == len(t.Fields) {
			w.buf = w.buf[:0]
			return w.fail("record %s has the field %q, which comes before another of its fields in table %s or isn't one of its fields", r.Key, f.Name, t.Name)
		}
		at = k + 1
		if j > 0 {
			b = append(b, ',')
		}
		b = appendString(b, f.Name)
		b = append(b, ':')
		var err error
		if b, err = appendValue(b, f.Name, f.Value); err != nil {
			w.buf = w.buf[:0]
			return w.fail("record %s, field %s: %v", r.Key, f.Name, err)
		}
	}
	w.buf = append(b, "}}"...)
	w.n.Records++
	return w.line()
}

// Link writes a link.
func (w *Writer) Link(l Link) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.stage == 2 && !linkLess(w.link, l) {
		return w.fail("link %s -%s-> %s comes after %s -%s-> %s; links go in order of from, type and to",
			l.From, l.Type, l.To, w.link.From, w.link.Type, w.link.To)
	}
	if !utf8.ValidString(l.From) || !utf8.ValidString(l.Type) || !utf8.ValidString(l.To) {
		return w.fail("link %q -%q-> %q isn't UTF-8", l.From, l.Type, l.To)
	}
	w.stage, w.link = 2, l
	b := append(w.buf, `{"from":`...)
	b = appendString(b, l.From)
	b = append(b, `,"type":`...)
	b = appendString(b, l.Type)
	b = append(b, `,"to":`...)
	b = appendString(b, l.To)
	w.buf = append(b, '}')
	w.n.Links++
	return w.line()
}

// Close writes the last line, which holds the counts, and flushes. Without
// that line an export is incomplete, and a Reader refuses it.
func (w *Writer) Close() error {
	if err := w.usable(); err != nil {
		return err
	}
	w.stage = 3
	b := append(w.buf, `{"end":{"tables":`...)
	b = strconv.AppendInt(b, int64(w.n.Tables), 10)
	b = append(b, `,"records":`...)
	b = strconv.AppendInt(b, int64(w.n.Records), 10)
	b = append(b, `,"links":`...)
	b = strconv.AppendInt(b, int64(w.n.Links), 10)
	w.buf = append(b, "}}"...)
	if err := w.line(); err != nil {
		return err
	}
	if err := w.w.Flush(); err != nil {
		w.err = err
	}
	return w.err
}

func appendValue(b []byte, name string, v any) ([]byte, error) {
	vec := isVec(name)
	if _, ok := v.([]float32); vec != ok {
		if vec {
			return b, fmt.Errorf("the vector is a []float32, not a %T", v)
		}
		return b, errors.New("only the field vec holds a vector")
	}
	switch x := v.(type) {
	case int64:
		return strconv.AppendInt(b, x, 10), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return b, fmt.Errorf("%v has no form in the export", x)
		}
		return appendReal(b, x), nil
	case string:
		if !utf8.ValidString(x) {
			return b, errors.New("the text isn't valid UTF-8")
		}
		return appendString(b, x), nil
	case []byte:
		b = append(b, `{"base64":"`...)
		b = base64.StdEncoding.AppendEncode(b, x)
		return append(b, `"}`...), nil
	case []float32:
		if len(x) == 0 {
			return b, errors.New("the vector is empty")
		}
		b = append(b, '[')
		for i, f := range x {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return b, fmt.Errorf("vector value %d is %v, which has no form in the export", i, f)
			}
			if i > 0 {
				b = append(b, ',')
			}
			b = appendFloat(b, float64(f), 32)
		}
		return append(b, ']'), nil
	case nil:
		return b, errors.New("NULL fields are left out")
	}
	return b, fmt.Errorf("a %T has no form in the export", v)
}

// appendReal writes a float64 as appendFloat does, adding ".0" when that
// gives a whole number in plain digits, so a real never reads as an integer.
func appendReal(b []byte, f float64) []byte {
	n := len(b)
	b = appendFloat(b, f, 64)
	for _, c := range b[n:] {
		if c == '.' || c == 'e' {
			return b
		}
	}
	return append(b, ".0"...)
}

// appendFloat writes the shortest decimal that reads back to the same
// float32 or float64. Like JavaScript, it uses plain digits from 1e-6 up to
// 1e21 and an exponent outside that, written as e-7 or e+21. Unlike
// JavaScript, negative zero keeps its sign.
func appendFloat(b []byte, f float64, bits int) []byte {
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) ||
			bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	n := len(b)
	b = strconv.AppendFloat(b, f, format, -1, bits)
	if format == 'e' {
		if m := len(b); m-n >= 4 && b[m-4] == 'e' && b[m-3] == '-' && b[m-2] == '0' {
			b[m-2] = b[m-1]
			b = b[:m-1]
		}
	}
	return b
}

const hexDigits = "0123456789abcdef"

// appendString writes s as a JSON string. Only the quote, the backslash and
// control characters are escaped; everything else is written as it is.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// Reader reads an export.
type Reader struct {
	r      *bufio.Reader
	line   int
	stage  int // 0 tables, 1 records, 2 links, 3 ended
	tables map[string]*readTable
	n      Counts
}

type readTable struct {
	Table
	at map[string]int // a field's place in Fields
}

// NewReader reads the first line of an export and checks it.
func NewReader(r io.Reader) (*Reader, error) {
	rd := &Reader{r: bufio.NewReaderSize(r, 1<<16), tables: map[string]*readTable{}}
	v, err := rd.next()
	if err == io.EOF {
		return nil, fmt.Errorf("%w: the input is empty", ErrFormat)
	}
	if err != nil {
		return nil, err
	}
	m, err := rd.members(v, "hypercrux", "version")
	if err != nil || m["hypercrux"].kind != kString || m["hypercrux"].s != "export" || m["version"].kind != kNumber {
		return nil, rd.errorf("this isn't a HyperCrux export, which starts with the line %s", header)
	}
	ver, err := strconv.Atoi(m["version"].s)
	switch {
	case err != nil || ver < 1:
		return nil, rd.errorf("the export's version is %s", m["version"].s)
	case ver > Version:
		return nil, rd.errorf("the export is format version %d, and this HyperCrux reads up to version %d; upgrade HyperCrux", ver, Version)
	}
	return rd, nil
}

// Line returns the number of the line read last, counting from 1.
func (rd *Reader) Line() int { return rd.line }

// Counts returns how much has been read so far.
func (rd *Reader) Counts() Counts { return rd.n }

func (rd *Reader) errorf(format string, args ...any) error {
	return fmt.Errorf("%w: line %d: %s", ErrFormat, rd.line, fmt.Sprintf(format, args...))
}

// next reads and parses a line.
func (rd *Reader) next() (value, error) {
	b, err := rd.r.ReadBytes('\n')
	if len(b) == 0 && err == io.EOF {
		return value{}, io.EOF
	}
	if err != nil && err != io.EOF {
		return value{}, err
	}
	rd.line++
	if !utf8.Valid(b) {
		return value{}, rd.errorf("the line isn't valid UTF-8")
	}
	v, err := parseLine(string(b))
	if err != nil {
		return value{}, rd.errorf("%v", err)
	}
	return v, nil
}

// members checks that an object has exactly the members named, in any order,
// and returns them by name.
func (rd *Reader) members(v value, names ...string) (map[string]value, error) {
	m := make(map[string]value, len(v.obj))
	for _, mem := range v.obj {
		m[mem.name] = mem.value
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			return nil, rd.errorf("the line has no %q", n)
		}
	}
	if len(m) != len(names) {
		for _, mem := range v.obj {
			known := false
			for _, n := range names {
				known = known || mem.name == n
			}
			if !known {
				return nil, rd.errorf("the line has %q, which doesn't belong in it", mem.name)
			}
		}
	}
	return m, nil
}

func (v value) has(name string) bool {
	for _, m := range v.obj {
		if m.name == name {
			return true
		}
	}
	return false
}

// Next returns the next table, record or link, as a Table, a Record or a
// Link. After the last of them it reads the end line, checks its counts and
// returns io.EOF.
func (rd *Reader) Next() (any, error) {
	if rd.stage == 3 {
		return nil, io.EOF
	}
	v, err := rd.next()
	if err == io.EOF {
		return nil, fmt.Errorf("%w: the export stops after line %d, before its end line, so it isn't complete", ErrFormat, rd.line)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case v.has("table"):
		return rd.table(v)
	case v.has("key"):
		return rd.record(v)
	case v.has("from"):
		return rd.readLink(v)
	case v.has("end"):
		return nil, rd.end(v)
	case v.has("hypercrux"):
		return nil, rd.errorf("a second first line; is this two exports run together?")
	}
	return nil, rd.errorf("a line that isn't a table, a record, a link or the end")
}

func (rd *Reader) table(v value) (any, error) {
	m, err := rd.members(v, "table", "dims", "fields")
	if err != nil {
		return nil, err
	}
	if rd.stage != 0 {
		return nil, rd.errorf("a table comes after records or links; tables come first")
	}
	name := m["table"]
	if name.kind != kString || name.s == "" || strings.IndexByte(name.s, ':') >= 0 {
		return nil, rd.errorf("a table's name is text without a colon")
	}
	if rd.tables[name.s] != nil {
		return nil, rd.errorf("table %s is there twice", name.s)
	}
	t := &readTable{Table: Table{Name: name.s}, at: map[string]int{}}
	switch d := m["dims"]; {
	case d.kind == kNull:
	case d.kind == kNumber:
		n, err := strconv.Atoi(d.s)
		if err != nil || n < 1 || n > 1<<30 {
			return nil, rd.errorf("table %s has a vector size of %s; it's a whole number from 1, or null", name.s, d.s)
		}
		t.Dims = n
	default:
		return nil, rd.errorf("table %s: dims is a whole number or null", name.s)
	}
	fields := m["fields"]
	if fields.kind != kArray {
		return nil, rd.errorf("table %s: fields is a list of names", name.s)
	}
	lower := map[string]bool{}
	for _, f := range fields.arr {
		if f.kind != kString || f.s == "" {
			return nil, rd.errorf("table %s: a field's name is text", name.s)
		}
		if l := strings.ToLower(f.s); lower[l] {
			return nil, rd.errorf("table %s has the field %s twice, counting upper and lower case as the same", name.s, f.s)
		} else {
			lower[l] = true
		}
		t.at[f.s] = len(t.Fields)
		t.Fields = append(t.Fields, f.s)
	}
	rd.tables[name.s] = t
	rd.n.Tables++
	return Table{Name: t.Name, Dims: t.Dims, Fields: append([]string(nil), t.Fields...)}, nil
}

func (rd *Reader) record(v value) (any, error) {
	m, err := rd.members(v, "key", "fields")
	if err != nil {
		return nil, err
	}
	if rd.stage > 1 {
		return nil, rd.errorf("a record comes after the links; records come before them")
	}
	k := m["key"]
	if k.kind != kString {
		return nil, rd.errorf("a key is text")
	}
	tbl, ok := tableOf(k.s)
	if !ok {
		return nil, rd.errorf("key %q has no table: keys are table:id", k.s)
	}
	t := rd.tables[tbl]
	if t == nil {
		return nil, rd.errorf("record %s is in table %s, which the export doesn't list", k.s, tbl)
	}
	fields := m["fields"]
	if fields.kind != kObject {
		return nil, rd.errorf("record %s: fields is an object", k.s)
	}
	rd.stage = 1
	r := Record{Key: k.s, Fields: make([]Field, 0, len(fields.obj))}
	places := make([]int, 0, len(fields.obj))
	for _, f := range fields.obj {
		at, ok := t.at[f.name]
		if !ok {
			return nil, rd.errorf("record %s has the field %q, which table %s doesn't list", k.s, f.name, tbl)
		}
		val, err := fieldValue(f.name, f.value)
		if err != nil {
			return nil, rd.errorf("record %s, field %s: %v", k.s, f.name, err)
		}
		if val == nil {
			continue
		}
		// Keep the table's order, which is how the fields are written.
		i := len(places)
		for i > 0 && places[i-1] > at {
			i--
		}
		places = append(places, 0)
		copy(places[i+1:], places[i:])
		places[i] = at
		r.Fields = append(r.Fields, Field{})
		copy(r.Fields[i+1:], r.Fields[i:])
		r.Fields[i] = Field{Name: f.name, Value: val}
	}
	rd.n.Records++
	return r, nil
}

func (rd *Reader) readLink(v value) (any, error) {
	m, err := rd.members(v, "from", "type", "to")
	if err != nil {
		return nil, err
	}
	from, typ, to := m["from"], m["type"], m["to"]
	if from.kind != kString || typ.kind != kString || to.kind != kString {
		return nil, rd.errorf("a link's from, type and to are text")
	}
	rd.stage = 2
	rd.n.Links++
	return Link{From: from.s, Type: typ.s, To: to.s}, nil
}

func (rd *Reader) end(v value) error {
	m, err := rd.members(v, "end")
	if err != nil {
		return err
	}
	if m["end"].kind != kObject {
		return rd.errorf("the end line holds the counts as an object")
	}
	c, err := rd.members(m["end"], "tables", "records", "links")
	if err != nil {
		return err
	}
	var got [3]int
	for i, name := range []string{"tables", "records", "links"} {
		n, err := strconv.Atoi(c[name].s)
		if c[name].kind != kNumber || err != nil || n < 0 {
			return rd.errorf("the end line's %s is a whole number", name)
		}
		got[i] = n
	}
	if got != [3]int{rd.n.Tables, rd.n.Records, rd.n.Links} {
		return rd.errorf("the end line counts %d tables, %d records and %d links, and the export holds %d, %d and %d",
			got[0], got[1], got[2], rd.n.Tables, rd.n.Records, rd.n.Links)
	}
	rd.stage = 3
	if _, err := rd.next(); err != io.EOF {
		if err != nil {
			return err
		}
		return rd.errorf("something follows the end line")
	}
	return io.EOF
}

// fieldValue turns a parsed value into what a Field holds, or nil for null.
func fieldValue(name string, v value) (any, error) {
	vec := isVec(name)
	if vec != (v.kind == kArray) && v.kind != kNull {
		if vec {
			return nil, errors.New("the vector is a list of numbers")
		}
		return nil, errors.New("only the field vec holds a list, the record's vector")
	}
	switch v.kind {
	case kNull:
		return nil, nil
	case kNumber:
		return number(v.s)
	case kString:
		return v.s, nil
	case kObject:
		if len(v.obj) != 1 || v.obj[0].name != "base64" || v.obj[0].value.kind != kString {
			return nil, errors.New(`an object in a field holds bytes, as {"base64":"..."}`)
		}
		s := v.obj[0].value.s
		b, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil || base64.StdEncoding.EncodeToString(b) != s {
			return nil, errors.New("the bytes aren't in standard base64, with padding")
		}
		return b, nil
	case kArray:
		out := make([]float32, len(v.arr))
		for i, e := range v.arr {
			if e.kind != kNumber {
				return nil, fmt.Errorf("vector value %d isn't a number", i)
			}
			f, err := strconv.ParseFloat(e.s, 32)
			if err != nil {
				return nil, fmt.Errorf("vector value %d, %s, is out of float32's range", i, e.s)
			}
			out[i] = float32(f)
		}
		return out, nil
	case kBool:
		return nil, errors.New("true and false aren't stored; use 1 and 0")
	}
	return nil, errors.New("a value of no known kind")
}

// number reads an integer, or a real when it has a decimal point or an
// exponent.
func number(s string) (any, error) {
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("the number %s is out of range", s)
		}
		return f, nil
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("the integer %s is out of range; integers are 64-bit", s)
	}
	return i, nil
}
