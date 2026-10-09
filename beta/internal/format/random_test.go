// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// Random round trips. A maker makes random change lists with awkward
// values and names at their limits; with bad set, it breaks a rule now and
// then too, always in a way the syntax can write.

type maker struct {
	r   *rand.Rand
	bad bool
}

func newMaker(seed uint64, bad bool) *maker {
	return &maker{r: rand.New(rand.NewPCG(seed, 0x48435258)), bad: bad}
}

// one is true one time in n.
func (m *maker) one(n int) bool { return m.r.IntN(n) == 0 }

// breaks is true now and then, for a maker that breaks rules.
func (m *maker) breaks() bool { return m.bad && m.one(12) }

func pick[T any](m *maker, from []T) T { return from[m.r.IntN(len(from))] }

// size returns a length from lo to hi: often an end, mostly short, and
// sometimes anywhere between.
func (m *maker) size(lo, hi int) int {
	switch m.r.IntN(10) {
	case 0:
		return lo
	case 1:
		return hi
	case 2:
		return lo + m.r.IntN(hi-lo+1)
	}
	return lo + m.r.IntN(min(hi-lo, 8)+1)
}

const (
	tableTail = "abcdefghijklmnopqrstuvwxyz0123456789_"
	fieldHead = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_"
	fieldTail = fieldHead + "0123456789"
)

// runes holds code points that take 1 to 4 bytes in UTF-8, at the edges of
// each length, and some that matter to the rules.
var runes = []rune{'a', 'Z', '0', ':', ' ', '_', 0x7f, 0x80, 'é', 0x7ff, 0x800, '☃', 0xd7ff, 0xe000, 0xfffd, 0xffff, 0x10000, '𝄞', 0x10ffff}

// text returns valid UTF-8 of exactly n bytes, holding zero bytes when
// zeros is set.
func (m *maker) text(n int, zeros bool) string {
	var b []byte
	for len(b) < n {
		r := pick(m, runes)
		if zeros && m.one(10) {
			r = 0
		}
		if len(b)+utf8.RuneLen(r) > n {
			r = 'x'
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

func (m *maker) tableName() string {
	if m.breaks() {
		return pick(m, []string{"", "Docs", "1x", "_x", "hc_x", "sqlite_x", "a-b", "é", strings.Repeat("a", 64), "a\x00"})
	}
	if m.one(10) {
		return pick(m, []string{"hc", "hcx", "h_c", "sqlite", "sqlitex", "sqlite1_", "a", "z9_"})
	}
	for {
		b := []byte{byte('a' + m.r.IntN(26))}
		for n := m.size(1, maxTableName); len(b) < n; {
			b = append(b, tableTail[m.r.IntN(len(tableTail))])
		}
		if s := string(b); isTableName(s) {
			return s
		}
	}
}

func (m *maker) key() string {
	if m.breaks() {
		return pick(m, []string{"", "docs", ":7", "docs:", "a:", "Docs:7", "hc_x:1", "docs:\xff", "docs:a\x00b", "docs:" + strings.Repeat("x", 1020)})
	}
	t := m.tableName()
	n := m.size(1, 40)
	if m.one(8) {
		n = maxKey - len(t) - 1
	}
	return t + ":" + m.text(max(n, 1), false)
}

func (m *maker) fieldName() string {
	if m.breaks() {
		return pick(m, []string{"", "1x", "a-b", "é", "key", "KEY", "Rowid", "oid", "_ROWID_", strings.Repeat("f", 65)})
	}
	if m.one(8) {
		return pick(m, []string{"keys", "key_", "_key", "rowid2", "oid_", "_rowid", "vec_", "vecs", "ve", "_", "A", strings.Repeat("F", maxFieldName)})
	}
	for {
		b := []byte{fieldHead[m.r.IntN(len(fieldHead))]}
		for n := m.size(1, maxFieldName); len(b) < n; {
			b = append(b, fieldTail[m.r.IntN(len(fieldTail))])
		}
		if s := string(b); isFieldName(s) && !isVec(s) {
			return s
		}
	}
}

// vecName returns the vector field's name, spelt in any case.
func (m *maker) vecName() string {
	return pick(m, []string{"vec", "vec", "Vec", "VEC", "vEc"})
}

func (m *maker) linkType() string {
	if m.breaks() {
		return pick(m, []string{"", "\x00x", "x\xff", strings.Repeat("é", 201)})
	}
	if m.one(10) {
		return pick(m, []string{"x", strings.Repeat("𝄞", maxTypeRunes), strings.Repeat("a", maxTypeRunes), "a\x00b"})
	}
	var b []byte
	for n := m.size(1, maxTypeRunes); utf8.RuneCount(b) < n; {
		r := pick(m, runes)
		if len(b) > 0 && m.one(10) {
			r = 0
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// Awkward bits for reals and for vector values.
var (
	realBits = []uint64{
		0, 1 << 63, // 0 and -0
		1, 0x800fffffffffffff, // the smallest subnormal, and the most negative
		0x0010000000000000,                     // the smallest normal
		0x7fefffffffffffff, 0xffefffffffffffff, // the largest, and the most negative
		0x3ff8000000000000, // 1.5
		0x3fb999999999999a, // 0.1
	}
	vecBits = []uint32{
		0, 1 << 31, // 0 and -0
		1, 0x807fffff, // the smallest subnormal, and the most negative
		0x00800000,             // the smallest normal
		0x7f7fffff, 0xff7fffff, // the largest, and the most negative
		0x3f800000, 0x3dcccccd, // 1 and 0.1
	}
)

// value returns a value for the field called name.
func (m *maker) value(name string) val.Value {
	if m.breaks() {
		return pick(m, []val.Value{
			val.Real(math.NaN()), val.Real(math.Inf(1)), val.Real(math.Inf(-1)),
			val.Text("\xff"), val.Text("na\xc3"),
			vector(1), val.Text("[1]"), val.Int(1),
			val.Vector(nil), vector(0, negZero32), vector(1, nan32), vector(inf32),
		})
	}
	if isVec(name) {
		if m.one(4) {
			return val.Null()
		}
		return m.vector()
	}
	switch m.r.IntN(5) {
	case 0:
		return val.Null()
	case 1:
		return val.Int(pick(m, []int64{0, 1, -1, math.MaxInt64, math.MinInt64, m.r.Int64() - m.r.Int64()}))
	case 2:
		bits := pick(m, realBits)
		if m.one(3) {
			for bits = m.r.Uint64(); !finite64(bits); bits = m.r.Uint64() {
			}
		}
		return val.Real(math.Float64frombits(bits))
	case 3:
		return val.Text(m.text(m.blobSize(), true))
	}
	b := make([]byte, m.blobSize())
	for i := range b {
		b[i] = byte(m.r.Uint32())
	}
	return val.Bytes(string(b))
}

// blobSize returns a length for text or bytes, beyond what a u16 holds now
// and then.
func (m *maker) blobSize() int {
	if m.one(200) {
		return math.MaxUint16 + 1 + m.r.IntN(100)
	}
	return m.size(0, 50)
}

func (m *maker) vector() val.Value {
	n := pick(m, []int{1, 2, 3, 384, 1 + m.r.IntN(64)})
	if m.one(500) {
		n = maxVecDims
	}
	bits := make([]byte, 0, 4*n)
	zeros := true
	for range n {
		x := pick(m, vecBits)
		if m.one(3) {
			for x = m.r.Uint32(); x&0x7f800000 == 0x7f800000; x = m.r.Uint32() {
			}
		}
		zeros = zeros && x&0x7fffffff == 0
		bits = binary.LittleEndian.AppendUint32(bits, x)
	}
	if zeros {
		binary.LittleEndian.PutUint32(bits[4*m.r.IntN(n):], 0x3f800000)
	}
	return val.VectorBits(string(bits))
}

// names returns n field names, no two matching regardless of case, among
// them the vector field when vec is set.
func (m *maker) names(n int, vec bool) []string {
	var names []string
	seen := map[string]bool{}
	if vec {
		v := m.vecName()
		names, seen["vec"] = append(names, v), true
	}
	for len(names) < n {
		name := m.fieldName()
		if l := strings.ToLower(name); !seen[l] {
			names, seen[l] = append(names, name), true
		}
	}
	m.r.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	if m.breaks() && len(names) > 0 {
		switch m.r.IntN(3) {
		case 0: // the same name twice
			names = append(names, names[m.r.IntN(len(names))])
		case 1: // a name that matches another regardless of case
			names = append(names, strings.ToUpper(names[m.r.IntN(len(names))]))
		default: // a vector field too many
			names = append(names, "VeC")
		}
	}
	return names
}

func (m *maker) count() int {
	switch {
	case m.one(100):
		return 100 + m.r.IntN(300)
	case m.one(20):
		return 20 + m.r.IntN(30)
	}
	return m.r.IntN(8)
}

func (m *maker) change() Change {
	switch m.r.IntN(8) {
	case 0:
		vec := m.one(2)
		c := Change{Op: CreateTable, Table: m.tableName(), Names: m.names(m.count(), vec)}
		if vec && m.one(2) || m.breaks() {
			c.Size = pick(m, []int{1, 2, 384, m.size(1, maxVecDims)})
		}
		if m.breaks() {
			c.Size = pick(m, []int{maxVecDims + 1, math.MaxUint32})
		}
		return c
	case 1, 2, 3:
		c := Change{Op: Put, Key: m.key()}
		names := m.names(m.count(), m.one(3))
		slices.Sort(names)
		if m.breaks() && len(names) > 1 {
			i := m.r.IntN(len(names) - 1)
			names[i], names[i+1] = names[i+1], names[i]
		}
		for _, name := range names {
			c.Fields = append(c.Fields, Field{Name: name, Value: m.value(name)})
		}
		return c
	case 4:
		return Change{Op: Delete, Key: m.key()}
	case 5:
		return Change{Op: Link, Key: m.key(), Type: m.linkType(), To: m.key()}
	case 6:
		return Change{Op: Unlink, Key: m.key(), Type: m.linkType(), To: m.key()}
	}
	return Change{Op: Drop, Table: m.tableName()}
}

func (m *maker) changes() []Change {
	n := 1 + m.r.IntN(6)
	if m.one(20) {
		n = 20 + m.r.IntN(30)
	}
	cs := make([]Change, n)
	for i := range cs {
		cs[i] = m.change()
	}
	return cs
}

// genSeq returns a generation and a sequence number, often at an edge.
func (m *maker) genSeq() (uint64, uint64) {
	edges := []uint64{1, 2, 258, math.MaxUint32 + 1, math.MaxUint64}
	return pick(m, append(edges, 1+m.r.Uint64N(1000))), pick(m, append(edges, 1+m.r.Uint64N(1000)))
}

func rounds(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}

// TestRandomRoundTrips checks decode(encode(c)) == c and encode(decode(b))
// == b on random change lists, and that fixtures_test.go's reader reads
// the same from every batch.
func TestRandomRoundTrips(t *testing.T) {
	m := newMaker(1, false)
	for round := range rounds(3000, 300) {
		cs := m.changes()
		gen, seq := m.genSeq()
		b, s, err := AppendBatch(nil, gen, seq, cs)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// The room AppendBatch makes for a batch, at once, is the batch's
		// length to the byte, so a buffer with that room is never grown.
		n := BatchHeadSize + 4
		for i := range cs {
			n += cs[i].size()
		}
		if in, _, _ := AppendBatch(make([]byte, 0, n), gen, seq, cs); n != len(b) || cap(in) != n {
			t.Fatalf("round %d: the changes' sizes come to a batch of %d bytes, which is %d, and a buffer of that room grew to %d", round, n, len(b), cap(in))
		}
		bt, err := DecodeBatch(b, gen, seq)
		if err != nil || !sameChanges(bt.Changes, cs) || bt.Length != len(b) || bt.Sum != s || bt.Seq != seq {
			t.Fatalf("round %d: %v\ncomes back as %+v, %v", round, cs, bt, err)
		}
		if again, s2, err := AppendBatch(nil, gen, seq, bt.Changes); err != nil || s2 != s || !bytes.Equal(again, b) {
			t.Fatalf("round %d: the batch decoded and encoded again differs, %v", round, err)
		}
		if ref, err := readBatch(b, gen); err != nil || !sameChanges(toChanges(ref.changes), cs) {
			t.Fatalf("round %d: fixtures_test.go's reader gives %v", round, err)
		}
		// With its marker and the start of another batch after it, the
		// batch reads the same, and its marker names it.
		id := [16]byte{byte(round)}
		more := AppendMarker(slices.Clone(b), id, gen, Marker{Seq: seq, Sum: s})
		more = append(more, b[:m.r.IntN(len(b))]...)
		if bt, err := DecodeBatch(more, gen, seq); err != nil || bt.Length != len(b) || !sameChanges(bt.Changes, cs) {
			t.Fatalf("round %d: with bytes after it, the batch reads as %+v, %v", round, bt, err)
		}
		if mk, ok := DecodeMarker(more[len(b):], id, gen); !ok || mk != (Marker{Seq: seq, Sum: s}) {
			t.Fatalf("round %d: the marker reads as %+v, %v", round, mk, ok)
		}
	}
}

// TestTheRulesMatchTheFixturesReader writes random changes that break a
// rule now and then, without checking them, in batches that count. The
// codec has to take or refuse each batch as fixtures_test.go's reader
// does, and AppendBatch has to refuse exactly the changes DecodeBatch takes
// for damage.
func TestTheRulesMatchTheFixturesReader(t *testing.T) {
	m := newMaker(2, true)
	took, refused := 0, 0
	for round := range rounds(6000, 600) {
		cs := m.changes()
		var raw []byte
		for i := range cs {
			raw = appendChange(raw, &cs[i])
		}
		b := seal(1, 1, raw)
		_, _, encErr := AppendBatch(nil, 1, 1, cs)
		if len(b) < MinBatchSize {
			// A drop with an empty table name, alone: too short to count.
			mustNotCount(t, "a batch of fewer than 36 bytes", b, 1, 1)
			if encErr == nil {
				t.Fatalf("round %d: AppendBatch writes %v", round, cs)
			}
			continue
		}
		bt, err := DecodeBatch(b, 1, 1)
		_, refErr := readBatch(b, 1)
		switch {
		case (err == nil) != (refErr == nil):
			t.Fatalf("round %d: DecodeBatch gives %v, and fixtures_test.go's reader %v, for %v", round, err, refErr, cs)
		case (err == nil) != (encErr == nil):
			t.Fatalf("round %d: DecodeBatch gives %v, and AppendBatch %v, for %v", round, err, encErr, cs)
		case err == nil && !sameChanges(bt.Changes, cs):
			t.Fatalf("round %d: %v comes back as %v", round, cs, bt.Changes)
		case err != nil && (!errors.As(err, new(*errs.Damage)) || errors.Is(err, ErrDoesNotCount)):
			t.Fatalf("round %d: a batch that counts gives %v, which isn't damage", round, err)
		case err != nil && !errors.Is(encErr, errs.ErrInvalid):
			t.Fatalf("round %d: AppendBatch gives %v", round, encErr)
		}
		if err == nil {
			took++
		} else {
			refused++
		}
	}
	t.Logf("%d batches taken and %d refused", took, refused)
	if took < refused/4 || refused < took/4 {
		t.Errorf("%d batches were taken and %d refused; the test needs plenty of each", took, refused)
	}
}

// TestChangedBytes changes random bytes of random batches. With the
// checksum made to fit, the batch counts, so it's damage or it reads, and
// what reads encodes back to the same bytes. Without, it doesn't count.
func TestChangedBytes(t *testing.T) {
	m := newMaker(3, false)
	for round := range rounds(20000, 2000) {
		b, _, err := AppendBatch(nil, 1, 1, m.changes())
		if err != nil {
			t.Fatal(err)
		}
		c := slices.Clone(b)
		for range 1 + m.r.IntN(3) {
			i := BatchHeadSize + m.r.IntN(len(c)-BatchHeadSize-4)
			c[i] = pick(m, []byte{c[i] ^ 1, c[i] ^ 0x80, 0, 0xff, byte(m.r.Uint32()), 'T', 'P', 'v', 'n'})
		}
		if bytes.Equal(b, c) {
			continue
		}
		mustNotCount(t, "a batch with a byte changed", c, 1, 1)
		binary.LittleEndian.PutUint32(c[len(c)-4:], sum(c[:len(c)-4]))
		bt, err := DecodeBatch(c, 1, 1)
		_, refErr := readBatch(c, 1)
		switch {
		case (err == nil) != (refErr == nil):
			t.Fatalf("round %d: DecodeBatch gives %v, and fixtures_test.go's reader %v", round, err, refErr)
		case err != nil && !errors.As(err, new(*errs.Damage)):
			t.Fatalf("round %d: a batch that counts gives %v", round, err)
		case err == nil:
			if again, _, err := AppendBatch(nil, 1, 1, bt.Changes); err != nil || !bytes.Equal(again, c) {
				t.Fatalf("round %d: the batch decoded and encoded again differs, %v", round, err)
			}
		}
	}
}
