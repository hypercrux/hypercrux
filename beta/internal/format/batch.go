// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

const (
	// BatchHeadSize is the size of a batch's head: its magic number,
	// generation, length and sequence number. The changes start straight
	// after it.
	BatchHeadSize = 28

	// MinBatchSize is the shortest a batch that counts can be: a head, the
	// smallest change (a drop of a table with a one-letter name) and a
	// checksum.
	MinBatchSize = 36
)

// ErrDoesNotCount means the bytes given don't start with a batch that
// counts: one of FORMAT.md's five checks fails. The batch doesn't start
// with the magic number, carries another generation, has a length under
// MinBatchSize or ends past the bytes there are, has the wrong sequence
// number, or fails its checksum.
//
// Where the log expects a batch, that's where the log ends. What lies there
// is a commit under way, the remains of one a crash cut short, or damage,
// and only a whole marker further on can show damage (FORMAT.md, "Reading
// the log"). So this error doesn't wrap errs.ErrDamaged. It's for the log,
// which works out what the end of the log holds, and the public package's
// callers never need it.
var ErrDoesNotCount = errors.New("hypercrux: not a batch that counts")

// The magic numbers, as strings.
const (
	magicFile   = "HCRX\r\n\x1a\n"
	magicBatch  = "HCRB"
	magicMarker = "HCRM"
	magicSQLite = "SQLite format 3\x00"
)

// crcTable is CRC32C's, on the Castagnoli polynomial. Go's hash/crc32
// computes it with the processor's own instruction on x86 and ARM.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// The kind letters of the six changes and the six kinds of value.
var (
	opLetter   = [...]byte{CreateTable: 'T', Put: 'P', Delete: 'D', Link: 'L', Unlink: 'U', Drop: 'X'}
	kindLetter = [...]byte{val.KindNull: 'n', val.KindInt: 'i', val.KindReal: 'r', val.KindText: 't', val.KindBytes: 'b', val.KindVector: 'v'}
)

// Batch is a batch DecodeBatch has read.
type Batch struct {
	Seq     uint64   // its sequence number
	Length  int      // its length in bytes, which is where its marker starts
	Sum     uint32   // its checksum, which its marker names
	Changes []Change // its changes, one or more
}

// AppendBatch appends a batch to dst: generation gen, sequence number seq,
// and the changes given, in order. It returns the longer slice and the
// batch's checksum, which the batch's marker names.
//
// It checks every change by FORMAT.md's rules for a change on its own, the
// rules DecodeBatch checks, so it never writes a batch that a reader would
// take for damage: the syntax, and the rules for names, kinds, lengths and
// values. The rules that depend on the state are the store's. A change
// that sets a field of Change its Op doesn't use is refused too. A batch
// needs one change at least, and a generation and a sequence number of 1
// or more. When anything is refused, the error wraps errs.ErrInvalid, and
// dst comes back as it was.
func AppendBatch(dst []byte, gen, seq uint64, changes []Change) ([]byte, uint32, error) {
	switch {
	case gen == 0:
		return dst, 0, fmt.Errorf("%w: a batch of generation 0", errs.ErrInvalid)
	case seq == 0:
		return dst, 0, fmt.Errorf("%w: a batch with sequence number 0", errs.ErrInvalid)
	case len(changes) == 0:
		return dst, 0, fmt.Errorf("%w: a batch with no changes", errs.ErrInvalid)
	}
	for i := range changes {
		if why := changes[i].problem(); why != "" {
			return dst, 0, fmt.Errorf("%w: change %d of the batch: %s", errs.ErrInvalid, i+1, why)
		}
	}
	start := len(dst)
	b := append(dst, magicBatch...)
	b = binary.LittleEndian.AppendUint64(b, gen)
	b = binary.LittleEndian.AppendUint64(b, 0) // the length, once it's known
	b = binary.LittleEndian.AppendUint64(b, seq)
	for i := range changes {
		b = appendChange(b, &changes[i])
	}
	binary.LittleEndian.PutUint64(b[start+12:], uint64(len(b)-start+4))
	sum := crc32.Checksum(b[start:], crcTable)
	return binary.LittleEndian.AppendUint32(b, sum), sum, nil
}

// appendChange appends c without checking it, so the tests can write
// changes that break the rules. Its Op has to be one of the six.
func appendChange(b []byte, c *Change) []byte {
	b = append(b, opLetter[c.Op])
	switch c.Op {
	case CreateTable:
		b = appendString(b, c.Table)
		b = binary.LittleEndian.AppendUint32(b, uint32(c.Size))
		b = binary.LittleEndian.AppendUint16(b, uint16(len(c.Names)))
		for _, name := range c.Names {
			b = appendString(b, name)
		}
	case Put:
		b = appendString(b, c.Key)
		b = binary.LittleEndian.AppendUint16(b, uint16(len(c.Fields)))
		for _, f := range c.Fields {
			b = appendString(b, f.Name)
			b = appendValue(b, f.Value)
		}
	case Delete:
		b = appendString(b, c.Key)
	case Link, Unlink:
		b = appendString(b, c.Key)
		b = appendString(b, c.Type)
		b = appendString(b, c.To)
	case Drop:
		b = appendString(b, c.Table)
	}
	return b
}

// appendString appends a string: a u16 holding its length, then its bytes.
func appendString(b []byte, s string) []byte {
	return append(binary.LittleEndian.AppendUint16(b, uint16(len(s))), s...)
}

func appendValue(b []byte, v val.Value) []byte {
	b = append(b, kindLetter[v.Kind()])
	switch v.Kind() {
	case val.KindInt:
		b = binary.LittleEndian.AppendUint64(b, uint64(v.Int()))
	case val.KindReal:
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v.Real()))
	case val.KindText, val.KindBytes:
		b = binary.LittleEndian.AppendUint32(b, uint32(len(v.Raw())))
		b = append(b, v.Raw()...)
	case val.KindVector:
		b = binary.LittleEndian.AppendUint32(b, uint32(v.Dims()))
		b = append(b, v.Raw()...)
	}
	return b
}

// BatchLength reads the head of the batch at the start of b and makes the
// four of FORMAT.md's checks for a batch that counts that a head can show:
// the magic number, the generation gen, a length of MinBatchSize or more
// that ends within room bytes, and the sequence number seq. room is how far
// the file runs past the batch's start, and b holds the batch's first
// BatchHeadSize bytes when the file has them. It returns the batch's
// length. The fifth check, the checksum, needs the whole batch, which
// DecodeBatch reads.
//
// It's for a reader that keeps the database open, which reads a batch's
// head first, then looks for the batch's marker before it reads the rest.
// An error wraps ErrDoesNotCount.
func BatchLength(b []byte, gen, seq uint64, room int64) (int64, error) {
	if len(b) < BatchHeadSize || room < BatchHeadSize {
		return 0, fmt.Errorf("%w: %d bytes, too few for a batch's head", ErrDoesNotCount, min(int64(len(b)), room))
	}
	if string(b[:4]) != magicBatch {
		return 0, fmt.Errorf("%w: the magic number is % x", ErrDoesNotCount, b[:4])
	}
	if g := binary.LittleEndian.Uint64(b[4:]); g != gen {
		return 0, fmt.Errorf("%w: generation %d, in a file of generation %d", ErrDoesNotCount, g, gen)
	}
	if n := binary.LittleEndian.Uint64(b[12:]); n < MinBatchSize || n > uint64(room) {
		return 0, fmt.Errorf("%w: a length of %d, outside %d to the %d bytes to the end of the file", ErrDoesNotCount, n, MinBatchSize, room)
	}
	if s := binary.LittleEndian.Uint64(b[20:]); s != seq {
		return 0, fmt.Errorf("%w: sequence number %d, where %d comes next", ErrDoesNotCount, s, seq)
	}
	return int64(binary.LittleEndian.Uint64(b[12:])), nil
}

// DecodeBatch reads the batch at the start of b, which runs from the
// batch's start to the end of the file, or to as far as the caller has
// read it. gen is the file's generation and seq the sequence number that
// comes next. The bytes after the batch, its marker first, are left alone.
//
// It keeps FORMAT.md's line between a batch that counts and damage:
//
//   - When any of the five checks fails, the batch doesn't count, and the
//     error wraps ErrDoesNotCount.
//   - When the batch counts but its changes aren't well formed, or break
//     the rules for a change on its own, which AppendBatch checks too, the
//     file is damaged. A batch that counts holds what its writer wrote, so
//     a crash can't account for bad changes in it. The error is a
//     *errs.Damage, which wraps errs.ErrDamaged, naming the batch by its
//     sequence number, with an Offset from the batch's start. The log adds
//     the batch's place in the file and the file's path.
//
// The rules that depend on the state, such as a put going into a table that
// exists, are the store's, which applies the changes.
//
// DecodeBatch copies the batch's changes out of b into one string, so the
// caller can use b again, and every string in the changes is a slice of
// that one: keys, table and field names, link types, text and bytes, and
// the bits of each vector, made with value.VectorBits. Holding any of them
// holds the whole string in memory. A caller that keeps one past the
// batch, as the store keeps keys, names and values, copies it with
// strings.Clone, so the batch's string can go. The store copies vectors
// into its table's array anyway.
func DecodeBatch(b []byte, gen, seq uint64) (Batch, error) {
	n, err := BatchLength(b, gen, seq, int64(len(b)))
	if err != nil {
		return Batch{}, err
	}
	sum := binary.LittleEndian.Uint32(b[n-4:])
	if want := crc32.Checksum(b[:n-4], crcTable); sum != want {
		return Batch{}, fmt.Errorf("%w: the checksum is %#08x, where CRC32C gives %#08x", ErrDoesNotCount, sum, want)
	}
	changes, err := decodeChanges(string(b[BatchHeadSize:n-4]), seq)
	if err != nil {
		return Batch{}, err
	}
	return Batch{Seq: seq, Length: int(n), Sum: sum, Changes: changes}, nil
}

// decoder reads the changes of a batch that counts. Its first error sticks:
// once there is one, every read returns nothing.
type decoder struct {
	s      string // the changes
	off    int    // where the next read starts in s
	seq    uint64 // the batch's sequence number, for errors
	n      int    // the change being read, from 1, for errors
	damage *errs.Damage

	// Every put's fields and every create change's field names, one change
	// after another, so a batch costs a few allocations however many
	// changes it holds.
	fields []Field
	names  []string
}

// decodeChanges reads s, a batch's changes, one after another.
func decodeChanges(s string, seq uint64) ([]Change, error) {
	d := &decoder{s: s, seq: seq}
	var changes []Change
	for d.off < len(d.s) {
		d.n++
		at := d.off
		c := d.change()
		if d.damage == nil {
			if why := c.problem(); why != "" {
				d.fail(at, "%s", why)
			}
		}
		if d.damage != nil {
			return nil, d.damage
		}
		changes = append(changes, c)
	}
	if len(changes) == 0 {
		return nil, &errs.Damage{Offset: BatchHeadSize, Batch: seq, Reason: "a batch with no changes"}
	}
	// Each change's slices point into the arrays as they were when it was
	// read, and appending may have moved them since. Point every change
	// into the final arrays, so the old ones can go, with each slice's
	// capacity ending where it does, so appending to one change's slice
	// never reaches the next change's.
	f, nm := 0, 0
	for i := range changes {
		c := &changes[i]
		if k := len(c.Fields); k > 0 {
			c.Fields = d.fields[f : f+k : f+k]
			f += k
		} else {
			c.Fields = nil
		}
		if k := len(c.Names); k > 0 {
			c.Names = d.names[nm : nm+k : nm+k]
			nm += k
		} else {
			c.Names = nil
		}
	}
	return changes, nil
}

// fail records the first error, at the offset at in the changes.
func (d *decoder) fail(at int, format string, args ...any) {
	if d.damage == nil {
		d.damage = &errs.Damage{
			Offset: int64(BatchHeadSize + at),
			Batch:  d.seq,
			Reason: fmt.Sprintf("change %d: ", d.n) + fmt.Sprintf(format, args...),
		}
	}
}

// take returns the next n bytes.
func (d *decoder) take(n uint64) string {
	if d.damage != nil {
		return ""
	}
	if left := len(d.s) - d.off; n > uint64(left) {
		d.fail(d.off, "%d bytes needed, and %d left before the checksum", n, left)
		return ""
	}
	p := d.s[d.off : d.off+int(n)]
	d.off += int(n)
	return p
}

func (d *decoder) u8() byte {
	if p := d.take(1); len(p) == 1 {
		return p[0]
	}
	return 0
}

func (d *decoder) u16() uint16 {
	if p := d.take(2); len(p) == 2 {
		return uint16(p[0]) | uint16(p[1])<<8
	}
	return 0
}

func (d *decoder) u32() uint32 {
	if p := d.take(4); len(p) == 4 {
		return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
	}
	return 0
}

func (d *decoder) u64() uint64 {
	if p := d.take(8); len(p) == 8 {
		return uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 |
			uint64(p[4])<<32 | uint64(p[5])<<40 | uint64(p[6])<<48 | uint64(p[7])<<56
	}
	return 0
}

// str reads a string: a u16 holding its length, then its bytes.
func (d *decoder) str() string { return d.take(uint64(d.u16())) }

// change reads one change's syntax. decodeChanges checks it by the rules.
func (d *decoder) change() Change {
	at := d.off
	switch kind := d.u8(); kind {
	case 'T':
		c := Change{Op: CreateTable}
		c.Table = d.str()
		c.Size = int(d.u32())
		n := int(d.u16())
		start := len(d.names)
		for i := 0; i < n && d.damage == nil; i++ {
			d.names = append(d.names, d.str())
		}
		c.Names = d.names[start:] // until decodeChanges points it into the final array
		return c
	case 'P':
		c := Change{Op: Put}
		c.Key = d.str()
		n := int(d.u16())
		start := len(d.fields)
		for i := 0; i < n && d.damage == nil; i++ {
			name := d.str()
			d.fields = append(d.fields, Field{Name: name, Value: d.value()})
		}
		c.Fields = d.fields[start:] // until decodeChanges points it into the final array
		return c
	case 'D':
		return Change{Op: Delete, Key: d.str()}
	case 'L', 'U':
		c := Change{Op: Link}
		if kind == 'U' {
			c.Op = Unlink
		}
		c.Key = d.str()
		c.Type = d.str()
		c.To = d.str()
		return c
	case 'X':
		return Change{Op: Drop, Table: d.str()}
	default:
		d.fail(at, "a change of unknown kind %#02x", kind)
	}
	return Change{}
}

// value reads one value of a put.
func (d *decoder) value() val.Value {
	at := d.off
	switch kind := d.u8(); kind {
	case 'n':
		return val.Null()
	case 'i':
		return val.Int(int64(d.u64()))
	case 'r':
		return val.Real(math.Float64frombits(d.u64()))
	case 't':
		return val.Text(d.take(uint64(d.u32())))
	case 'b':
		return val.Bytes(d.take(uint64(d.u32())))
	case 'v':
		return val.VectorBits(d.take(4 * uint64(d.u32())))
	default:
		d.fail(at, "a value of unknown kind %#02x", kind)
	}
	return val.Null()
}
