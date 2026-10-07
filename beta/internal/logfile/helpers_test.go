// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// recorder is a Target that keeps what the log hands it.
type recorder struct {
	batches []applied
	resets  int
	refuse  uint64 // a batch Apply refuses, by sequence number, or 0
}

type applied struct {
	seq     uint64
	changes []format.Change
}

func (r *recorder) Apply(seq uint64, changes []format.Change) error {
	if seq == r.refuse {
		return errors.New("a put into a table that doesn't exist")
	}
	r.batches = append(r.batches, applied{seq, changes})
	return nil
}

func (r *recorder) Reset() {
	r.batches = nil
	r.resets++
}

// holds checks that r holds want, batch by batch, numbered from 1.
func (r *recorder) holds(t *testing.T, want ...[]format.Change) {
	t.Helper()
	r.holdsFrom(t, 1, want...)
}

// holdsFrom checks that r holds want, batch by batch, numbered from first.
func (r *recorder) holdsFrom(t *testing.T, first uint64, want ...[]format.Change) {
	t.Helper()
	if len(r.batches) != len(want) {
		t.Fatalf("the log handed over %d batches, where %d are wanted", len(r.batches), len(want))
	}
	for i, b := range r.batches {
		if seq := first + uint64(i); b.seq != seq {
			t.Errorf("batch %d came as sequence number %d", seq, b.seq)
		}
		if !sameChanges(b.changes, want[i]) {
			t.Errorf("batch %d holds\n  %v\nwhere\n  %v\nis wanted", b.seq, b.changes, want[i])
		}
	}
}

func sameChanges(a, b []format.Change) bool {
	return slices.EqualFunc(a, b, func(x, y format.Change) bool {
		return x.Op == y.Op && x.Table == y.Table && x.Size == y.Size && slices.Equal(x.Names, y.Names) &&
			x.Key == y.Key && x.Type == y.Type && x.To == y.To &&
			slices.EqualFunc(x.Fields, y.Fields, func(f, g format.Field) bool { return f.Name == g.Name && f.Value == g.Value })
	})
}

// openLog opens the database at path through the real calls, with a
// recorder, and closes it when the test ends.
func openLog(t *testing.T, path string, o Options) (*Log, *recorder) {
	t.Helper()
	rec := &recorder{}
	l, err := Open(fsys.OS{}, path, rec, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if l.locked { // a test that failed holding the lock, which Close would wait for
			l.Unlock()
		}
		l.Close()
	})
	return l, rec
}

// lockErr calls l.Lock where a test wants it to fail. When it works after
// all, lockErr lets go of the lock, so the test can go on and close l, and
// returns an error that matches nothing the test looks for.
func lockErr(l *Log) error {
	if err := l.Lock(); err != nil {
		return err
	}
	l.Unlock()
	return errors.New("Lock worked")
}

// commit takes the write lock, appends each change list as a batch of its
// own, and lets go.
func commit(t *testing.T, l *Log, lists ...[]format.Change) {
	t.Helper()
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	for _, c := range lists {
		if err := l.Append(c); err != nil {
			l.Unlock()
			t.Fatal(err)
		}
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// reread opens the database at path afresh and returns what it hands over.
func reread(t *testing.T, path string) *recorder {
	t.Helper()
	rec := &recorder{}
	l, err := Open(fsys.OS{}, path, rec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	return rec
}

// table is a change list that creates the table name: a small batch for
// tests that only count batches.
func table(name string) []format.Change {
	return []format.Change{{Op: format.CreateTable, Table: name}}
}

// randomChanges returns one to four changes that keep FORMAT.md's rules
// for a change on its own. The log takes any such list: the rules that
// depend on the state are the Target's.
func randomChanges(r *rand.Rand) []format.Change {
	changes := make([]format.Change, 1+r.IntN(4))
	for i := range changes {
		name := []string{"users", "docs", "t2"}[r.IntN(3)]
		key := func() string { return name + ":" + strconv.Itoa(r.IntN(50)) }
		switch r.IntN(6) {
		case 0:
			changes[i] = format.Change{Op: format.CreateTable, Table: name}
		case 1:
			changes[i] = format.Change{Op: format.Put, Key: key(), Fields: randomFields(r)}
		case 2:
			changes[i] = format.Change{Op: format.Delete, Key: key()}
		case 3:
			changes[i] = format.Change{Op: format.Link, Key: key(), Type: "owns", To: key()}
		case 4:
			changes[i] = format.Change{Op: format.Unlink, Key: key(), Type: "likes ☺", To: key()}
		default:
			changes[i] = format.Change{Op: format.Drop, Table: name}
		}
	}
	return changes
}

// randomFields returns some of the fields a, b, c and vec, which are in
// byte order, with values of every kind.
func randomFields(r *rand.Rand) []format.Field {
	var fields []format.Field
	for _, name := range []string{"a", "b", "c", "vec"} {
		if r.IntN(2) == 0 {
			continue
		}
		var v value.Value
		switch k := r.IntN(5); {
		case name == "vec":
			vec := make([]float32, 1+r.IntN(8))
			for j := range vec {
				vec[j] = float32(r.NormFloat64())
			}
			vec[0] = 0.5 // so it isn't all zeros
			v = value.Vector(vec)
		case k == 0:
			v = value.Null()
		case k == 1:
			v = value.Int(r.Int64() - r.Int64())
		case k == 2:
			v = value.Real(r.NormFloat64())
		case k == 3:
			v = value.Text(strings.Repeat("é", r.IntN(30)))
		default:
			b := make([]byte, r.IntN(40))
			for j := range b {
				b[j] = byte(r.Uint32())
			}
			v = value.Bytes(string(b))
		}
		fields = append(fields, format.Field{Name: name, Value: v})
	}
	return fields
}

// builder writes a database file by hand, with the codec, so a test can
// end the log in any of the ways a crash or damage can.
type builder struct {
	t   *testing.T
	hdr format.Header
	b   []byte
	seq uint64 // the last batch written
}

func newBuilder(t *testing.T) *builder {
	hdr := format.Header{ID: [16]byte{0x48, 0x43, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, Gen: 1}
	b, err := format.AppendHeader(nil, hdr)
	if err != nil {
		t.Fatal(err)
	}
	return &builder{t: t, hdr: hdr, b: b}
}

// batch writes changes as the next batch, without its marker, and returns
// its checksum.
func (w *builder) batch(changes []format.Change) uint32 {
	w.seq++
	b, sum, err := format.AppendBatch(w.b, w.hdr.Gen, w.seq, changes)
	if err != nil {
		w.t.Fatal(err)
	}
	w.b = b
	return sum
}

// marker writes a marker naming the batch seq with the checksum sum.
func (w *builder) marker(seq uint64, sum uint32) {
	w.b = format.AppendMarker(w.b, w.hdr.ID, w.hdr.Gen, format.Marker{Seq: seq, Sum: sum})
}

// commit writes changes as the next batch, with its marker.
func (w *builder) commit(changes []format.Change) {
	sum := w.batch(changes)
	w.marker(w.seq, sum)
}

// raw writes a batch by hand, whatever its changes: a head with the
// generation gen and the sequence number seq, the changes' bytes, and a
// checksum that fits. It returns the checksum, and leaves w.seq as it was.
func (w *builder) raw(gen, seq uint64, changes []byte) uint32 {
	start := len(w.b)
	w.b = append(w.b, "HCRB"...)
	w.b = binary.LittleEndian.AppendUint64(w.b, gen)
	w.b = binary.LittleEndian.AppendUint64(w.b, uint64(format.BatchHeadSize+len(changes)+4))
	w.b = binary.LittleEndian.AppendUint64(w.b, seq)
	w.b = append(w.b, changes...)
	sum := crc32.Checksum(w.b[start:], crc32.MakeTable(crc32.Castagnoli))
	w.b = binary.LittleEndian.AppendUint32(w.b, sum)
	return sum
}

func (w *builder) write(path string) {
	if err := os.WriteFile(path, w.b, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// hookFS is the real calls, with some of them replaced for a test.
type hookFS struct {
	fsys.FS
	renameNoReplace func(from, to string) error
	syncDir         func(dir string) error
	file            func(f fsys.File) fsys.File // wraps every file opened or created, when set
}

func (h *hookFS) Open(path string) (fsys.File, error) {
	f, err := h.FS.Open(path)
	if err == nil && h.file != nil {
		f = h.file(f)
	}
	return f, err
}

func (h *hookFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	f, err := h.FS.Create(path, perm)
	if err == nil && h.file != nil {
		f = h.file(f)
	}
	return f, err
}

func (h *hookFS) RenameNoReplace(from, to string) error {
	if h.renameNoReplace != nil {
		return h.renameNoReplace(from, to)
	}
	return h.FS.RenameNoReplace(from, to)
}

func (h *hookFS) SyncDir(dir string) error {
	if h.syncDir != nil {
		return h.syncDir(dir)
	}
	return h.FS.SyncDir(dir)
}

// spyFile records the calls on a file that change it or its lock, and can
// fail a sync.
type spyFile struct {
	fsys.File
	calls    *[]string
	failSync *int // the syncs left before one fails, counting down when above 0
}

func (f *spyFile) note(format string, args ...any) {
	if f.calls != nil {
		*f.calls = append(*f.calls, fmt.Sprintf(format, args...))
	}
}

func (f *spyFile) WriteAt(p []byte, off int64) (int, error) {
	f.note("write %d at %d", len(p), off)
	return f.File.WriteAt(p, off)
}

func (f *spyFile) Sync() error {
	f.note("sync")
	if f.failSync != nil && *f.failSync > 0 {
		if *f.failSync--; *f.failSync == 0 {
			return errors.New("the drive failed the sync")
		}
	}
	return f.File.Sync()
}

func (f *spyFile) Truncate(size int64) error {
	f.note("truncate to %d", size)
	return f.File.Truncate(size)
}

func (f *spyFile) Unlock() error {
	f.note("unlock")
	return f.File.Unlock()
}

// fileBytes reads the file at path.
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// inode returns the device and inode of the file at path.
func inode(t *testing.T, path string) fsys.Info {
	t.Helper()
	info, err := fsys.OS{}.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
