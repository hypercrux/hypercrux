// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// These tests check the golden fixtures in testdata against beta/FORMAT.md,
// and against nothing else: they use no codec, so the fixtures stay a check
// on the one that's written for the format. When a checksum in a fixture is
// wrong, the error gives the right one.

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// sum returns the CRC32C of its arguments, one after another.
func sum(parts ...[]byte) uint32 {
	var c uint32
	for _, p := range parts {
		c = crc32.Update(c, castagnoli, p)
	}
	return c
}

const (
	headerSize = 68
	batchHead  = 28 // magic number, generation, length and sequence number
	markerSize = 20
	minBatch   = 36 // a batch holding the smallest change
	maxDims    = 65536
)

var (
	fileMagic   = []byte("HCRX\r\n\x1a\n")
	batchMagic  = []byte("HCRB")
	markerMagic = []byte("HCRM")
	sqliteMagic = []byte("SQLite format 3\x00")
)

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// readFixture reads a fixture's bytes: pairs of hex digits, with everything
// from a # to the end of its line a comment.
func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b []byte
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		for _, word := range strings.Fields(line) {
			x, err := hex.DecodeString(word)
			if err != nil || len(x) != 1 {
				t.Fatalf("%s, line %d: %q isn't one byte in hex", name, n, word)
			}
			b = append(b, x[0])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return b
}

// The header.

type header struct {
	id               []byte
	gen              uint64
	contGen, contSeq uint64
	contSum          uint32
	end              uint64
}

func readHeader(b []byte) (header, error) {
	var h header
	switch {
	case len(b) < headerSize:
		return h, fmt.Errorf("%d bytes, too short for a header", len(b))
	case bytes.HasPrefix(b, sqliteMagic):
		return h, fmt.Errorf("a 0.x database")
	case !bytes.Equal(b[:8], fileMagic):
		return h, fmt.Errorf("the magic number is % x", b[:8])
	}
	if v := le32(b[8:]); v != 1 {
		return h, fmt.Errorf("format version %d", v)
	}
	if got, want := le32(b[64:]), sum(b[:64]); got != want {
		return h, fmt.Errorf("the header checksum is %#08x, where CRC32C gives %#08x", got, want)
	}
	h = header{id: b[12:28], gen: le64(b[28:]), contGen: le64(b[36:]), contSeq: le64(b[44:]), contSum: le32(b[52:]), end: le64(b[56:])}
	switch {
	case h.gen == 0:
		return h, fmt.Errorf("generation 0")
	case h.gen == 1 && (h.contGen != 0 || h.contSeq != 0 || h.contSum != 0 || h.end != 0):
		return h, fmt.Errorf("generation 1 with the compaction fields set")
	case h.gen > 1 && h.contGen != h.gen-1:
		return h, fmt.Errorf("generation %d continuing from generation %d", h.gen, h.contGen)
	case h.gen > 1 && h.end < headerSize:
		return h, fmt.Errorf("the compacted part ends at %d, inside the header", h.end)
	}
	return h, nil
}

// Batches and their changes.

type value struct {
	kind byte
	i    int64
	bits uint64   // a real number's
	data string   // text's or bytes'
	vec  []uint32 // a vector's values, as their bits
}

type field struct {
	name string
	v    value
}

type change struct {
	kind   byte
	name   string   // the table for T and X, the key for P and D, the key a link is from for L and U
	typ    string   // a link's type
	to     string   // the key a link is to
	size   uint32   // T's vector size
	fields []string // T's fields
	puts   []field  // P's fields
}

type batch struct {
	seq     uint64
	sum     uint32
	length  int
	changes []change
}

// readBatch reads the batch at the start of b, in a file of generation gen,
// and checks everything that makes it count but its sequence number, which
// depends on the batch before it.
func readBatch(b []byte, gen uint64) (batch, error) {
	var bt batch
	switch {
	case len(b) < batchHead:
		return bt, fmt.Errorf("%d bytes, too short for a batch", len(b))
	case !bytes.Equal(b[:4], batchMagic):
		return bt, fmt.Errorf("the magic number is % x", b[:4])
	}
	if g := le64(b[4:]); g != gen {
		return bt, fmt.Errorf("generation %d in a file of generation %d", g, gen)
	}
	length := le64(b[12:])
	if length < minBatch || length > uint64(len(b)) {
		return bt, fmt.Errorf("a length of %d, outside %d to the %d bytes there are", length, minBatch, len(b))
	}
	bt.length = int(length)
	bt.seq = le64(b[20:])
	bt.sum = le32(b[bt.length-4:])
	if want := sum(b[:bt.length-4]); bt.sum != want {
		return bt, fmt.Errorf("the checksum is %#08x, where CRC32C gives %#08x", bt.sum, want)
	}
	r := &reader{b: b[batchHead : bt.length-4]}
	for r.off < len(r.b) && r.err == nil {
		if c := r.change(); r.err == nil {
			bt.changes = append(bt.changes, c)
		}
	}
	switch {
	case r.err != nil:
		return bt, fmt.Errorf("change %d: %v", len(bt.changes)+1, r.err)
	case len(bt.changes) == 0:
		return bt, fmt.Errorf("no changes")
	}
	return bt, nil
}

// reader walks through a batch's changes. Its first error sticks.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("at byte %d: %s", batchHead+r.off, fmt.Sprintf(format, args...))
	}
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n > len(r.b)-r.off {
		r.fail("%d bytes needed, %d left", n, len(r.b)-r.off)
		return nil
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *reader) u8() byte {
	if p := r.take(1); p != nil {
		return p[0]
	}
	return 0
}

func (r *reader) u16() int {
	if p := r.take(2); p != nil {
		return int(binary.LittleEndian.Uint16(p))
	}
	return 0
}

func (r *reader) u32() uint32 {
	if p := r.take(4); p != nil {
		return le32(p)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if p := r.take(8); p != nil {
		return le64(p)
	}
	return 0
}

// str reads a string, a u16 length and the bytes, from min to max of them,
// and checks it with the rule given.
func (r *reader) str(what string, min, max int, rule func(string) bool) string {
	n := r.u16()
	if r.err == nil && (n < min || n > max) {
		r.fail("%s of %d bytes, outside %d to %d", what, n, min, max)
	}
	s := string(r.take(n))
	if r.err == nil && !rule(s) {
		r.fail("%s %q, which breaks the rules", what, s)
	}
	return s
}

func (r *reader) table() string     { return r.str("a table name", 1, 63, validTable) }
func (r *reader) key() string       { return r.str("a key", 3, 1024, validKey) }
func (r *reader) fieldName() string { return r.str("a field name", 1, 64, validField) }
func (r *reader) linkType() string  { return r.str("a link type", 1, 800, validType) }

func (r *reader) change() change {
	c := change{kind: r.u8()}
	switch c.kind {
	case 'T':
		c.name = r.table()
		c.size = r.u32()
		if r.err == nil && c.size > maxDims {
			r.fail("a vector size of %d", c.size)
		}
		n := r.u16()
		seen := map[string]bool{}
		for i := 0; i < n && r.err == nil; i++ {
			f := r.fieldName()
			if seen[strings.ToLower(f)] {
				r.fail("two fields called %q, regardless of case", f)
			}
			seen[strings.ToLower(f)] = true
			c.fields = append(c.fields, f)
		}
		if r.err == nil && c.size != 0 && !seen["vec"] {
			r.fail("a vector size of %d and no vector field", c.size)
		}
	case 'P':
		c.name = r.key()
		n := r.u16()
		seen := map[string]bool{}
		for i := 0; i < n && r.err == nil; i++ {
			f := field{name: r.fieldName()}
			l := strings.ToLower(f.name)
			switch {
			case r.err != nil:
			case i > 0 && f.name <= c.puts[i-1].name:
				r.fail("the field %q after %q, out of byte order", f.name, c.puts[i-1].name)
			case seen[l]:
				r.fail("two fields called %q, regardless of case", f.name)
			}
			seen[l] = true
			f.v = r.value()
			switch {
			case r.err != nil:
			case l == "vec" && f.v.kind != 'v' && f.v.kind != 'n':
				r.fail("the vector field %q holding a value of kind %c", f.name, f.v.kind)
			case l != "vec" && f.v.kind == 'v':
				r.fail("the field %q holding a vector", f.name)
			}
			c.puts = append(c.puts, f)
		}
	case 'D':
		c.name = r.key()
	case 'L', 'U':
		c.name = r.key()
		c.typ = r.linkType()
		c.to = r.key()
	case 'X':
		c.name = r.table()
	default:
		r.fail("a change of unknown kind %#02x", c.kind)
	}
	return c
}

func (r *reader) value() value {
	v := value{kind: r.u8()}
	switch v.kind {
	case 'n':
	case 'i':
		v.i = int64(r.u64())
	case 'r':
		v.bits = r.u64()
		if f := math.Float64frombits(v.bits); r.err == nil && (math.IsNaN(f) || math.IsInf(f, 0)) {
			r.fail("the real number %v", f)
		}
	case 't', 'b':
		v.data = string(r.take(int(r.u32())))
		if r.err == nil && v.kind == 't' && !utf8.ValidString(v.data) {
			r.fail("text that isn't valid UTF-8")
		}
	case 'v':
		n := r.u32()
		if r.err == nil && (n < 1 || n > maxDims) {
			r.fail("a vector of %d values", n)
		}
		zeros := true
		for i := uint32(0); i < n && r.err == nil; i++ {
			bits := r.u32()
			if f := float64(math.Float32frombits(bits)); math.IsNaN(f) || math.IsInf(f, 0) {
				r.fail("the vector value %v", f)
			}
			zeros = zeros && bits&0x7fffffff == 0
			v.vec = append(v.vec, bits)
		}
		if r.err == nil && zeros {
			r.fail("a vector of zeros")
		}
	default:
		r.fail("a value of unknown kind %#02x", v.kind)
	}
	return v
}

// The rules for names, 0.x's, from records.go and links.go.

var (
	tableRule = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	fieldRule = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

func validTable(s string) bool {
	return tableRule.MatchString(s) && !strings.HasPrefix(s, "hc_") && !strings.HasPrefix(s, "sqlite_")
}

func validKey(s string) bool {
	i := strings.IndexByte(s, ':')
	return i > 0 && i < len(s)-1 && len(s) <= 1024 && validTable(s[:i]) &&
		utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

func validField(s string) bool {
	switch strings.ToLower(s) {
	case "key", "rowid", "oid", "_rowid_":
		return false
	}
	return fieldRule.MatchString(s)
}

func validType(s string) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= 1 && n <= 200 && s[0] != 0
}

// Markers.

func readMarker(b, id []byte, gen uint64) (seq uint64, batchSum uint32, err error) {
	switch {
	case len(b) < markerSize:
		return 0, 0, fmt.Errorf("%d bytes, too short for a marker", len(b))
	case !bytes.Equal(b[:4], markerMagic):
		return 0, 0, fmt.Errorf("the magic number is % x", b[:4])
	}
	if got, want := le32(b[16:]), sum(id, binary.LittleEndian.AppendUint64(nil, gen), b[:16]); got != want {
		return 0, 0, fmt.Errorf("the check value is %#08x, where CRC32C gives %#08x", got, want)
	}
	return le64(b[4:]), le32(b[12:]), nil
}

// Whole files, and the state their changes build.

type file struct {
	header
	batches []batch
	ends    []int // the offset just past each batch's marker
}

// readFile reads a file whose every batch is marked, as a fixture's is.
func readFile(b []byte) (file, error) {
	var f file
	h, err := readHeader(b)
	if err != nil {
		return f, fmt.Errorf("the header: %v", err)
	}
	f.header = h
	if h.end > uint64(len(b)) {
		return f, fmt.Errorf("the compacted part ends at %d, past the end of the file at %d", h.end, len(b))
	}
	for off := headerSize; off < len(b); {
		bt, err := readBatch(b[off:], h.gen)
		if err != nil {
			return f, fmt.Errorf("the batch at offset %d: %v", off, err)
		}
		if want := uint64(len(f.batches) + 1); bt.seq != want {
			return f, fmt.Errorf("the batch at offset %d has sequence number %d, where %d comes next", off, bt.seq, want)
		}
		off += bt.length
		seq, s, err := readMarker(b[off:], h.id, h.gen)
		if err != nil {
			return f, fmt.Errorf("the marker at offset %d: %v", off, err)
		}
		if seq != bt.seq || s != bt.sum {
			return f, fmt.Errorf("the marker at offset %d names batch %d, %#08x, after batch %d, %#08x", off, seq, s, bt.seq, bt.sum)
		}
		off += markerSize
		f.batches = append(f.batches, bt)
		f.ends = append(f.ends, off)
	}
	return f, nil
}

type table struct {
	fields []string
	size   uint32
}

type link struct{ from, typ, to string }

type state struct {
	tables  map[string]*table
	records map[string]map[string]value
	links   map[link]bool
}

// replay applies the file's batches in order, up to the offset upTo.
func (f file) replay(upTo int) (*state, error) {
	s := &state{tables: map[string]*table{}, records: map[string]map[string]value{}, links: map[link]bool{}}
	for i, bt := range f.batches {
		if f.ends[i] > upTo {
			break
		}
		for j, c := range bt.changes {
			if err := s.apply(c); err != nil {
				return nil, fmt.Errorf("batch %d, change %d: %v", bt.seq, j+1, err)
			}
		}
	}
	return s, nil
}

func tableOf(key string) string { return key[:strings.IndexByte(key, ':')] }

// apply makes one change, after checking it's valid in the state, as
// FORMAT.md's rules under Changes say.
func (s *state) apply(c change) error {
	switch c.kind {
	case 'T':
		if s.tables[c.name] != nil {
			return fmt.Errorf("creates the table %s, which exists", c.name)
		}
		s.tables[c.name] = &table{fields: slices.Clone(c.fields), size: c.size}
	case 'P':
		t := s.tables[tableOf(c.name)]
		if t == nil {
			return fmt.Errorf("puts %s, whose table doesn't exist", c.name)
		}
		rec := s.records[c.name]
		if rec == nil {
			rec = map[string]value{}
			s.records[c.name] = rec
		}
		for _, f := range c.puts {
			known := false
			for _, have := range t.fields {
				if have == f.name {
					known = true
				} else if strings.EqualFold(have, f.name) {
					return fmt.Errorf("puts the field %q, which the table spells %q", f.name, have)
				}
			}
			if !known {
				t.fields = append(t.fields, f.name)
			}
			switch f.v.kind {
			case 'n':
				delete(rec, f.name)
				continue
			case 'v':
				switch n := uint32(len(f.v.vec)); {
				case t.size == 0:
					t.size = n
				case n != t.size:
					return fmt.Errorf("puts a vector of %d values into a table of %d", n, t.size)
				}
			}
			rec[f.name] = f.v
		}
	case 'D':
		if s.records[c.name] == nil {
			return fmt.Errorf("deletes %s, which doesn't exist", c.name)
		}
		s.remove(c.name)
	case 'L':
		l := link{c.name, c.typ, c.to}
		switch {
		case s.records[c.name] == nil || s.records[c.to] == nil:
			return fmt.Errorf("links %s to %s, and one of them doesn't exist", c.name, c.to)
		case s.links[l]:
			return fmt.Errorf("adds the link %v, which is there", l)
		}
		s.links[l] = true
	case 'U':
		l := link{c.name, c.typ, c.to}
		if !s.links[l] {
			return fmt.Errorf("removes the link %v, which isn't there", l)
		}
		delete(s.links, l)
	case 'X':
		if s.tables[c.name] == nil {
			return fmt.Errorf("drops the table %s, which doesn't exist", c.name)
		}
		for key := range s.records {
			if tableOf(key) == c.name {
				s.remove(key)
			}
		}
		delete(s.tables, c.name)
	}
	return nil
}

// remove deletes a record and every link to or from it.
func (s *state) remove(key string) {
	delete(s.records, key)
	for l := range s.links {
		if l.from == key || l.to == key {
			delete(s.links, l)
		}
	}
}

// String lists the state in order, for error messages.
func (s *state) String() string {
	var lines []string
	for name, t := range s.tables {
		lines = append(lines, fmt.Sprintf("table %s: vector size %d, fields %q", name, t.size, t.fields))
	}
	for key, rec := range s.records {
		var fs []string
		for name, v := range rec {
			fs = append(fs, fmt.Sprintf("%s=%c%v", name, v.kind, v))
		}
		sort.Strings(fs)
		lines = append(lines, fmt.Sprintf("record %s: %s", key, strings.Join(fs, " ")))
	}
	for l := range s.links {
		lines = append(lines, fmt.Sprintf("link %s -%s-> %s", l.from, l.typ, l.to))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// The tests.

// TestCRC32C checks the table on CRC32C's standard check value.
func TestCRC32C(t *testing.T) {
	if got := sum([]byte("123456789")); got != 0xe3069283 {
		t.Fatalf("the CRC32C of 123456789 is %#08x, where the standard check value is 0xe3069283", got)
	}
}

var fixtures = []string{
	"batch-create-table.hex",
	"batch-delete.hex",
	"batch-drop.hex",
	"batch-link.hex",
	"batch-put.hex",
	"batch-several.hex",
	"batch-unlink.hex",
	"file-compacted.hex",
	"file-new.hex",
	"header-compacted.hex",
	"header-new.hex",
	"marker.hex",
}

func TestEveryFixtureIsChecked(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.hex")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	if !slices.Equal(names, fixtures) {
		t.Fatalf("testdata holds %q, and the tests check %q", names, fixtures)
	}
}

func TestHeaders(t *testing.T) {
	for _, name := range []string{"header-new.hex", "header-compacted.hex"} {
		b := readFixture(t, name)
		if len(b) != headerSize {
			t.Errorf("%s holds %d bytes, where a header has %d", name, len(b), headerSize)
		}
		if _, err := readHeader(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBatches(t *testing.T) {
	for name, kind := range map[string]byte{
		"batch-create-table.hex": 'T',
		"batch-put.hex":          'P',
		"batch-delete.hex":       'D',
		"batch-link.hex":         'L',
		"batch-unlink.hex":       'U',
		"batch-drop.hex":         'X',
	} {
		b := readFixture(t, name)
		bt, err := readBatch(b, 1)
		switch {
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case bt.length != len(b):
			t.Errorf("%s holds %d bytes after its batch", name, len(b)-bt.length)
		case len(bt.changes) != 1 || bt.changes[0].kind != kind:
			t.Errorf("%s doesn't hold one change of kind %c", name, kind)
		}
	}

	bt, err := readBatch(readFixture(t, "batch-put.hex"), 1)
	if err != nil {
		t.Fatalf("batch-put.hex: %v", err)
	}
	kinds := map[byte]bool{}
	for _, f := range bt.changes[0].puts {
		kinds[f.v.kind] = true
	}
	for _, k := range []byte("nirtbv") {
		if !kinds[k] {
			t.Errorf("batch-put.hex has no value of kind %c", k)
		}
	}

	b := readFixture(t, "batch-several.hex")
	bt, err = readBatch(b, 1)
	switch {
	case err != nil:
		t.Errorf("batch-several.hex: %v", err)
	case bt.length != len(b):
		t.Errorf("batch-several.hex holds %d bytes after its batch", len(b)-bt.length)
	case len(bt.changes) < 2:
		t.Errorf("batch-several.hex holds %d changes", len(bt.changes))
	}
}

func TestMarker(t *testing.T) {
	h, err := readHeader(readFixture(t, "header-new.hex"))
	if err != nil {
		t.Fatal(err)
	}
	m := readFixture(t, "marker.hex")
	if len(m) != markerSize {
		t.Errorf("marker.hex holds %d bytes, where a marker has %d", len(m), markerSize)
	}
	seq, s, err := readMarker(m, h.id, h.gen)
	if err != nil {
		t.Fatalf("marker.hex: %v", err)
	}
	bt, err := readBatch(readFixture(t, "batch-several.hex"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if seq != bt.seq || s != bt.sum {
		t.Errorf("marker.hex names batch %d, %#08x, and batch-several.hex is %d, %#08x", seq, s, bt.seq, bt.sum)
	}
	if _, _, err := readMarker(m, make([]byte, 16), h.gen); err == nil {
		t.Error("the marker passes for one of a database with another ID")
	}
	if _, _, err := readMarker(m, h.id, h.gen+1); err == nil {
		t.Error("the marker passes for one of a later generation")
	}
}

// TestFiles reads the two small databases, checks that the pieces match
// the fixtures of their own, and that compacting the new one gave the same
// state, in the order FORMAT.md gives for a compacted part.
func TestFiles(t *testing.T) {
	newFile := readFixture(t, "file-new.hex")
	nf, err := readFile(newFile)
	if err != nil {
		t.Fatalf("file-new.hex: %v", err)
	}
	want, err := nf.replay(len(newFile))
	if err != nil {
		t.Fatalf("file-new.hex: %v", err)
	}
	several := readFixture(t, "batch-several.hex")
	for _, piece := range []struct {
		name string
		at   int
		b    []byte
	}{
		{"header-new.hex", 0, readFixture(t, "header-new.hex")},
		{"batch-several.hex", headerSize, several},
		{"marker.hex", headerSize + len(several), readFixture(t, "marker.hex")},
	} {
		if end := piece.at + len(piece.b); end > len(newFile) || !bytes.Equal(newFile[piece.at:end], piece.b) {
			t.Errorf("file-new.hex doesn't hold %s at offset %d", piece.name, piece.at)
		}
	}

	comp := readFixture(t, "file-compacted.hex")
	cf, err := readFile(comp)
	if err != nil {
		t.Fatalf("file-compacted.hex: %v", err)
	}
	if !bytes.Equal(comp[:headerSize], readFixture(t, "header-compacted.hex")) {
		t.Error("file-compacted.hex doesn't start with header-compacted.hex")
	}
	if !bytes.Equal(cf.id, nf.id) {
		t.Error("compaction changed the database ID")
	}
	last := nf.batches[len(nf.batches)-1]
	if cf.contGen != nf.gen || cf.contSeq != last.seq || cf.contSum != last.sum {
		t.Errorf("file-compacted.hex continues from generation %d, batch %d, %#08x, where file-new.hex ends at generation %d, batch %d, %#08x",
			cf.contGen, cf.contSeq, cf.contSum, nf.gen, last.seq, last.sum)
	}
	if int(cf.end) != headerSize && !slices.Contains(cf.ends, int(cf.end)) {
		t.Fatalf("the compacted part ends at offset %d, which isn't just past a marker", cf.end)
	}
	got, err := cf.replay(int(cf.end))
	if err != nil {
		t.Fatalf("file-compacted.hex: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the compacted part holds\n%v\nand file-new.hex ends with\n%v", got, want)
	}
	if _, err := cf.replay(len(comp)); err != nil {
		t.Errorf("file-compacted.hex, after the compacted part: %v", err)
	}

	// The compacted part's order: each table with its records, then the links.
	var lastTable, lastKey string
	var lastLink *link
	for i, bt := range cf.batches {
		if cf.ends[i] > int(cf.end) {
			break
		}
		for _, c := range bt.changes {
			switch c.kind {
			case 'T':
				w := want.tables[c.name]
				switch {
				case lastLink != nil || c.name <= lastTable:
					t.Errorf("the compacted part creates %s out of order", c.name)
				case w == nil || c.size != w.size || !slices.Equal(c.fields, w.fields):
					t.Errorf("the compacted part creates %s without its vector size and whole field list", c.name)
				}
				lastTable, lastKey = c.name, ""
			case 'P':
				if lastLink != nil || tableOf(c.name) != lastTable || c.name <= lastKey {
					t.Errorf("the compacted part puts %s out of order", c.name)
				}
				for _, f := range c.puts {
					if f.v.kind == 'n' {
						t.Errorf("the compacted part puts a null into %s's field %s", c.name, f.name)
					}
				}
				lastKey = c.name
			case 'L':
				l := link{c.name, c.typ, c.to}
				if lastLink != nil && !linkBefore(*lastLink, l) {
					t.Errorf("the compacted part adds the link %v out of order", l)
				}
				lastLink = &l
			default:
				t.Errorf("the compacted part holds a change of kind %c", c.kind)
			}
		}
	}
}

func linkBefore(a, b link) bool {
	if a.from != b.from {
		return a.from < b.from
	}
	if a.typ != b.typ {
		return a.typ < b.typ
	}
	return a.to < b.to
}

// TestTheChecksCatchDamage makes sure the checks above aren't empty: a
// change to any byte of a small database, or zeros where a batch should
// be, has to fail them.
func TestTheChecksCatchDamage(t *testing.T) {
	for _, name := range []string{"file-new.hex", "file-compacted.hex"} {
		b := readFixture(t, name)
		for i := range b {
			for _, flip := range []byte{0x01, 0x80, 0xff} {
				c := slices.Clone(b)
				c[i] ^= flip
				if _, err := readFile(c); err == nil {
					t.Fatalf("%s with byte %d changed from %#02x to %#02x still passes", name, i, b[i], c[i])
				}
			}
		}
	}
	if _, err := readBatch(make([]byte, 64), 1); err == nil {
		t.Error("64 zero bytes pass as a batch")
	}
}
