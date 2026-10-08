// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The damaged fixtures: a database file for each kind of damage FORMAT.md
// names, at the end of the log and in the middle of it, in
// testdata/damaged, with damage.txt saying what each must report. Their
// README says how they're made and changed. damagedFixtures makes them, and
// TestTheDamagedFixtures checks the files against it, then opens and locks
// each one.

// damagedDir is where the fixtures are kept.
var damagedDir = filepath.Join("testdata", "damaged")

// damagedID is every fixture's database ID.
var damagedID = [16]byte{'d', 'a', 'm', 'a', 'g', 'e', 'd', ' ', 'f', 'i', 'x', 't', 'u', 'r', 'e', 's'}

// fixture is one damaged file, and what reading it must report.
type fixture struct {
	name  string // the file's name, without .hcx
	what  string // what's damaged, for damage.txt
	b     []byte
	batch uint64 // the batch the damage names, or 0 for the header
	at    int64  // the offset the damage gives

	// confirm says that a reader without the write lock can't tell the
	// damage from a writer at work, so it reads the file again holding the
	// lock before it reports it. A damaged header, and a batch that counts
	// with changes that break the rules, are damage whatever a writer does.
	confirm bool
}

// layout is a file the fixtures are made from: its bytes, and where each
// batch and each marker starts, from batch 1.
type layout struct {
	hdr     format.Header
	b       []byte
	batches []int
	markers []int
}

// lay writes a file of the header h and a batch with its marker for each
// change list, as the log writes them.
func lay(t *testing.T, h format.Header, lists ...[]format.Change) layout {
	t.Helper()
	b, err := format.AppendHeader(nil, h)
	if err != nil {
		t.Fatal(err)
	}
	f := layout{hdr: h, b: b}
	for i, c := range lists {
		f.batches = append(f.batches, len(f.b))
		var sum uint32
		f.b, sum, err = format.AppendBatch(f.b, h.Gen, uint64(i+1), c)
		if err != nil {
			t.Fatal(err)
		}
		f.markers = append(f.markers, len(f.b))
		f.b = format.AppendMarker(f.b, h.ID, h.Gen, format.Marker{Seq: uint64(i + 1), Sum: sum})
	}
	return f
}

// changed returns a copy of b with the byte at offset at changed by x.
func changed(b []byte, at int, x byte) []byte {
	c := slices.Clone(b)
	c[at] ^= x
	return c
}

// resealed returns a copy of f's bytes with the byte at offset at changed
// by x, inside batch seq's changes, and the batch's checksum and its
// marker made to fit again, so the batch counts and is marked, as its
// writer would have written it with that byte.
func (f layout) resealed(seq int, at int, x byte) []byte {
	c := changed(f.b, at, x)
	start, end := f.batches[seq-1], f.markers[seq-1]
	sum := crc32.Checksum(c[start:end-4], crc32.MakeTable(crc32.Castagnoli))
	binary.LittleEndian.PutUint32(c[end-4:], sum)
	copy(c[end:], format.AppendMarker(nil, f.hdr.ID, f.hdr.Gen, format.Marker{Seq: uint64(seq), Sum: sum}))
	return c
}

// sum returns batch seq's checksum, as its marker names it.
func (f layout) sum(seq int) uint32 {
	return binary.LittleEndian.Uint32(f.b[f.markers[seq-1]-4:])
}

// damagedFixtures makes every damaged fixture, from two small databases: a
// new one of four commits, and the same state compacted, with a commit after
// the compacted part. The fixtures named -end are damaged in the log's last
// batch, so only that batch's own marker shows the damage; those named
// -middle in batch 2, with batches 3 and 4 after it.
func damagedFixtures(t *testing.T) []fixture {
	put := func(key string, fields ...format.Field) format.Change {
		return format.Change{Op: format.Put, Key: key, Fields: fields}
	}
	text := func(name, s string) format.Field { return format.Field{Name: name, Value: value.Text(s)} }
	n := func(i int64) format.Field { return format.Field{Name: "n", Value: value.Int(i)} }
	commits := [][]format.Change{
		{{Op: format.CreateTable, Table: "docs"}, put("docs:1", text("title", "one"))},
		{put("docs:2", n(2), text("title", "two"))},
		{{Op: format.CreateTable, Table: "people"}, put("people:1", text("name", "Ann"))},
		{{Op: format.Delete, Key: "docs:1"}},
	}
	gen1 := format.Header{ID: damagedID, Gen: 1}
	base := lay(t, gen1, commits...)
	end, mid := 4, 2 // the batches damaged at the end of the log and in the middle
	head := func(seq int) int { return base.batches[seq-1] }
	off := func(seq int) int64 { return int64(head(seq)) }

	// The same state compacted, continuing from the new file's batch 4: a
	// compacted part of two batches, each table with its whole field list
	// and then its records, and then a commit after the compacted part.
	compactedLists := [][]format.Change{
		{{Op: format.CreateTable, Table: "docs", Names: []string{"title", "n"}}, put("docs:2", n(2), text("title", "two"))},
		{{Op: format.CreateTable, Table: "people", Names: []string{"name"}}, put("people:1", text("name", "Ann"))},
		{put("docs:3", n(3))},
	}
	compacted := func(lists ...[]format.Change) layout {
		draft := lay(t, format.Header{ID: damagedID, Gen: 2, FromGen: 1, CompactedEnd: format.HeaderSize}, lists...)
		return lay(t, format.Header{ID: damagedID, Gen: 2, FromGen: 1, FromSeq: 4, FromSum: base.sum(4), CompactedEnd: uint64(draft.markers[1] + format.MarkerSize)}, lists...)
	}
	whole := compacted(compactedLists...)
	parts := compacted(compactedLists[:2]...)

	// A file whose batch seq's changes are the ones given, in place of its
	// own, with its checksum and marker fitting.
	instead := func(seq int, c []format.Change) layout {
		lists := slices.Clone(commits)
		lists[seq-1] = c
		return lay(t, gen1, lists...)
	}
	missing := slices.Concat(base.b[:head(mid)], base.b[head(mid+1):])

	fixtures := []fixture{
		{name: "header-checksum", what: "the header, with a byte of the database ID changed, fails its checksum",
			b: changed(base.b, 20, 0x01), batch: 0, at: 64},
		{name: "header-compacted-past-end", what: "a compacted file cut short inside its compacted part, so the header's end of the compacted part lies past the end of the file",
			b: whole.b[:whole.batches[1]+10], batch: 0, at: 56},

		{name: "changes-malformed-end", what: "the last batch counts and is marked, and its change is of a kind there isn't",
			b: base.resealed(end, head(end)+format.BatchHeadSize, 'D'^'Z'), batch: 4, at: off(end) + format.BatchHeadSize},
		{name: "changes-malformed-middle", what: "batch 2 counts and is marked, and its change is of a kind there isn't",
			b: base.resealed(mid, head(mid)+format.BatchHeadSize, 'P'^'Z'), batch: 2, at: off(mid) + format.BatchHeadSize},
		{name: "changes-break-rules-end", what: "the last batch counts and is marked, and deletes the key Docs:1, which breaks the rules for keys",
			b: base.resealed(end, head(end)+format.BatchHeadSize+3, 'd'^'D'), batch: 4, at: off(end) + format.BatchHeadSize},
		{name: "changes-break-rules-middle", what: "batch 2 counts and is marked, and puts the key Docs:2, which breaks the rules for keys",
			b: base.resealed(mid, head(mid)+format.BatchHeadSize+3, 'd'^'D'), batch: 2, at: off(mid) + format.BatchHeadSize},
		{name: "changes-break-rules-unmarked", what: "the last batch counts, with no marker after it, and deletes the key Docs:1, which breaks the rules for keys",
			b: base.resealed(end, head(end)+format.BatchHeadSize+3, 'd'^'D')[:base.markers[end-1]], batch: 4, at: off(end) + format.BatchHeadSize},
		{name: "state-rules-end", what: "the last batch counts and is marked, and puts a record into a table that doesn't exist",
			b: instead(end, []format.Change{put("nosuch:1", n(1))}).b, batch: 4, at: off(end)},
		{name: "state-rules-middle", what: "batch 2 counts and is marked, and puts a record into a table that doesn't exist",
			b: instead(mid, []format.Change{put("nosuch:2", n(2))}).b, batch: 2, at: off(mid)},

		{name: "marker-names-another-end", what: "the last batch counts, and the whole marker after it names the batch before",
			b:     append(base.b[:base.markers[end-1]:base.markers[end-1]], format.AppendMarker(nil, damagedID, 1, format.Marker{Seq: 3, Sum: base.sum(3)})...),
			batch: 4, at: int64(base.markers[end-1]), confirm: true},
		{name: "marker-names-another-middle", what: "batch 2 counts, and the whole marker after it names the batch before",
			b:     slices.Concat(base.b[:base.markers[mid-1]], format.AppendMarker(nil, damagedID, 1, format.Marker{Seq: 1, Sum: base.sum(1)}), base.b[base.markers[mid-1]+format.MarkerSize:]),
			batch: 2, at: off(mid), confirm: true},
	}
	// A marked batch that fails one of the five checks for a batch that
	// counts, a byte changed in it, or its head of zeros, or all of it
	// zeros: its marker, and those after it, show the damage.
	for _, c := range []struct {
		name, what string
		at         func(seq int) int
		x          byte
	}{
		{"checksum", "fails its checksum, with a bit changed in its changes", func(seq int) int { return base.markers[seq-1] - 5 }, 0x10},
		{"magic", "has a bit changed in its magic number", func(seq int) int { return head(seq) + 3 }, 0x01},
		{"generation", "has a bit changed in its generation", func(seq int) int { return head(seq) + 4 }, 0x02},
		{"length", "has a bit changed in its length", func(seq int) int { return head(seq) + 13 }, 0x01},
		{"sequence-number", "has a bit changed in its sequence number", func(seq int) int { return head(seq) + 20 }, 0x04},
	} {
		fixtures = append(fixtures,
			fixture{name: c.name + "-end", what: "the last batch is marked and " + c.what, b: changed(base.b, c.at(end), c.x), batch: 4, at: off(end), confirm: true},
			fixture{name: c.name + "-middle", what: "batch 2 is marked and " + c.what, b: changed(base.b, c.at(mid), c.x), batch: 2, at: off(mid), confirm: true})
	}
	zeroed := func(from, to int) []byte {
		c := slices.Clone(base.b)
		clear(c[from:to])
		return c
	}
	fixtures = append(fixtures,
		fixture{name: "head-zeros-end", what: "the last batch's head is zeros, and its marker is whole",
			b: zeroed(head(end), head(end)+format.BatchHeadSize), batch: 4, at: off(end), confirm: true},
		fixture{name: "head-zeros-middle", what: "batch 2's head is zeros, and its marker is whole",
			b: zeroed(head(mid), head(mid)+format.BatchHeadSize), batch: 2, at: off(mid), confirm: true},
		fixture{name: "batch-zeros-end", what: "the last batch is zeros, and its marker is whole",
			b: zeroed(head(end), base.markers[end-1]), batch: 4, at: off(end), confirm: true},
		fixture{name: "batch-zeros-middle", what: "batch 2 is zeros, and its marker is whole",
			b: zeroed(head(mid), base.markers[mid-1]), batch: 2, at: off(mid), confirm: true},
		fixture{name: "batch-missing-middle", what: "batch 2 and its marker are missing, and batch 3 follows batch 1",
			b: missing, batch: 2, at: off(mid), confirm: true},
		fixture{name: "marker-damaged-middle", what: "batch 2 counts and its marker has a bit changed, with batches 3 and 4 marked after it",
			b: changed(base.b, base.markers[mid-1]+6, 0x01), batch: 2, at: off(mid), confirm: true},

		fixture{name: "compacted-part-end", what: "a compacted file whose compacted part's last marker has a bit changed, with nothing after it",
			b: changed(parts.b, parts.markers[1]+10, 0x01), batch: 2, at: int64(parts.batches[1]), confirm: true},
		fixture{name: "compacted-part-middle", what: "a compacted file whose compacted part's first batch fails its checksum, with a commit after the compacted part",
			b: changed(whole.b, whole.markers[0]-5, 0x10), batch: 1, at: format.HeaderSize, confirm: true},
	)
	return fixtures
}

// manifest is damage.txt, which lists the fixtures and what each must
// report.
func manifest(fixtures []fixture) string {
	var b strings.Builder
	b.WriteString("# The damaged fixtures, one a line: the file, the batch its damage names\n")
	b.WriteString("# (0 for the header), the offset the damage gives, and what's damaged.\n")
	b.WriteString("# Reading a file has to report that batch and offset, and change nothing.\n")
	b.WriteString("# README.md says how the files are made and changed.\n")
	for _, f := range fixtures {
		fmt.Fprintf(&b, "%s.hcx %d %d %s\n", f.name, f.batch, f.at, f.what)
	}
	return b.String()
}

// tables is a Target that keeps the one rule for the state that the damaged
// fixtures break: a change names a table that exists, which a create change
// makes and a drop takes away. It refuses a batch that breaks it as the
// store does, with a *errs.Damage naming the batch and the change, and
// takes the whole batch or none of it.
type tables struct {
	recorder
	have map[string]bool
}

func (r *tables) Apply(seq uint64, changes []format.Change) error {
	have := maps.Clone(r.have)
	if have == nil {
		have = map[string]bool{}
	}
	for i, c := range changes {
		name := c.Table
		if c.Op != format.CreateTable && c.Op != format.Drop {
			name, _, _ = strings.Cut(c.Key, ":")
		}
		switch {
		case c.Op == format.CreateTable && have[name]:
			return &errs.Damage{Batch: seq, Reason: fmt.Sprintf("change %d: the table %s exists already", i+1, name)}
		case c.Op == format.CreateTable:
			have[name] = true
		case !have[name]:
			return &errs.Damage{Batch: seq, Reason: fmt.Sprintf("change %d: no table %s", i+1, name)}
		case c.Op == format.Drop:
			delete(have, name)
		}
	}
	r.have = have
	return r.recorder.Apply(seq, changes)
}

func (r *tables) Reset() {
	r.have = nil
	r.recorder.Reset()
}

// TestTheDamagedFixtures is F4's closing test. The files in testdata/damaged
// are the ones damagedFixtures makes, byte for byte, and damage.txt lists
// them. Every one is reported, with the batch and offset damage.txt gives,
// and nothing is cut: each file's bytes are as they were after every read.
// Each is read four ways:
//
//   - Open, which gets the write lock at once and checks the end of the log
//     holding it;
//   - Open while another holds the lock and lets go of it a moment later.
//     What a writer at work could explain is reported only once Open has
//     waited for the lock and read the file again holding it;
//   - Lock, on a Log whose file another took the place of: the fixture,
//     moved into place as a backup would be, which Lock reads from its
//     start;
//   - Lock, on a Log that opened the file when it held only its header, so
//     the batches come to it as another process's commits would. A file
//     with a damaged header, or a compacted one, can't be opened that way
//     first.
//
// With HYPERCRUX_LOGFILE_RECORD set, it writes the files and damage.txt
// first.
func TestTheDamagedFixtures(t *testing.T) {
	fixtures := damagedFixtures(t)
	if os.Getenv("HYPERCRUX_LOGFILE_RECORD") != "" {
		recordFixtures(t, fixtures)
	}
	want := []string{"README.md", "damage.txt"}
	for _, f := range fixtures {
		want = append(want, f.name+".hcx")
	}
	slices.Sort(want)
	got, err := fsys.OS{}.List(damagedDir)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%s holds %v, %v, where %v is wanted: record the fixtures with HYPERCRUX_LOGFILE_RECORD=1", damagedDir, got, err, want)
	}
	if b, err := os.ReadFile(filepath.Join(damagedDir, "damage.txt")); err != nil || string(b) != manifest(fixtures) {
		t.Fatalf("damage.txt isn't what damagedFixtures makes, %v: record the fixtures with HYPERCRUX_LOGFILE_RECORD=1", err)
	}
	const hold = 30 * time.Millisecond
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			if b, err := os.ReadFile(filepath.Join(damagedDir, f.name+".hcx")); err != nil || !bytes.Equal(b, f.b) {
				t.Fatalf("the file isn't what damagedFixtures makes, %v: record the fixtures with HYPERCRUX_LOGFILE_RECORD=1", err)
			}
			reported := func(how, path string, err error) {
				t.Helper()
				var d *errs.Damage
				if !errors.As(err, &d) || d.Path != path || d.Batch != f.batch || d.Offset != f.at || d.Reason == "" {
					t.Errorf("%s gave %v, where damage naming batch %d at offset %d is wanted", how, err, f.batch, f.at)
				}
				if !bytes.Equal(fileBytes(t, path), f.b) {
					t.Errorf("%s changed the file", how)
				}
			}
			dir := t.TempDir()

			path := filepath.Join(dir, "open.hcx")
			write(t, path, f.b)
			_, err := Open(fsys.OS{}, path, &tables{}, Options{})
			reported("Open", path, err)

			path = filepath.Join(dir, "held.hcx")
			write(t, path, f.b)
			release := holdLock(t, path)
			began := time.Now()
			releaseAfter(t, release, hold)
			_, err = Open(fsys.OS{}, path, &tables{}, Options{})
			reported("Open while another held the lock", path, err)
			if took := time.Since(began); f.confirm && took < hold {
				t.Errorf("Open reported damage after %v, before the lock it had to read the file again under came free", took)
			}

			path = filepath.Join(dir, "moved.hcx")
			l, err := Open(fsys.OS{}, path, &tables{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			write(t, path+".backup", f.b)
			if err := os.Rename(path+".backup", path); err != nil {
				t.Fatal(err)
			}
			reported("Lock, once the file was moved into place", path, lockErr(l))

			if f.batch == 0 || f.b[28] != 1 { // a damaged header, or a later generation than 1
				return
			}
			path = filepath.Join(dir, "lock.hcx")
			write(t, path, f.b[:format.HeaderSize])
			l, err = Open(fsys.OS{}, path, &tables{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			write(t, path, f.b)
			reported("Lock, once the file was written in place", path, lockErr(l))
		})
	}
}

// write writes b as the file at path.
func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// recordFixtures writes the fixtures and damage.txt into testdata/damaged,
// and removes any other .hcx file there.
func recordFixtures(t *testing.T, fixtures []fixture) {
	if err := os.MkdirAll(damagedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names, err := fsys.OS{}.List(damagedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".hcx") {
			if err := os.Remove(filepath.Join(damagedDir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range fixtures {
		write(t, filepath.Join(damagedDir, f.name+".hcx"), f.b)
	}
	write(t, filepath.Join(damagedDir, "damage.txt"), []byte(manifest(fixtures)))
}
