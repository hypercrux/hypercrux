// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Check (G7): damage in the file named with its batch and offset, whether
// Check's own read of the file finds it, the read before it meets it, or a
// reload fails on it; a check while a writer commits; the sizes; and Export
// and Import refusing a database Check finds problems in. The store's tests
// plant damage in the copy itself (beta/internal/store/check_test.go).

// spot is a batch's place in a file: its offset and its length, without
// its marker.
type spot struct{ at, n int64 }

// batchesIn returns the place of each batch in the database at path, from
// batch 1 on.
func batchesIn(t *testing.T, path string) []spot {
	t.Helper()
	b, err := os.ReadFile(path)
	ok(t, err)
	h, err := format.DecodeHeader(b, int64(len(b)))
	ok(t, err)
	var out []spot
	for off := int64(format.HeaderSize); off < int64(len(b)); {
		n, err := format.BatchLength(b[off:], h.Gen, uint64(len(out)+1), int64(len(b))-off)
		ok(t, err)
		out = append(out, spot{off, n})
		off += n + format.MarkerSize
	}
	return out
}

// fourCommits makes the database the damaged fixtures are made from, through
// the API: a table docs made with docs:1, a put of docs:2, a table people
// made with people:1, and the delete of docs:1. The copy then holds 2
// tables and 2 records.
func fourCommits(t *testing.T, path string) *hc.DB {
	t.Helper()
	db := open(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	ok(t, db.Put("docs:2", hc.Fields{"n": 2}))
	ok(t, db.Put("people:1", hc.Fields{"name": "Ann"}))
	ok(t, db.Delete("docs:1"))
	return db
}

// writeAt writes b into the file at path at off, in place.
func writeAt(t *testing.T, path string, b []byte, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	ok(t, err)
	defer f.Close()
	_, err = f.WriteAt(b, off)
	ok(t, err)
}

// flip changes the bits in mask of the byte at off in the file at path.
func flip(t *testing.T, path string, off int64, mask byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	ok(t, err)
	writeAt(t, path, []byte{b[off] ^ mask}, off)
}

// TestCheckNamesDamageInTheFile is the half of G7's closing test that
// plants damage in files. A database is open, its copy holding all four of
// its commits, when its file is damaged in place, one way at a time: each
// part of batch 2, of the four, and the header, as the damaged fixtures are
// damaged, and the file cut short. No read in this process sees damage in a
// part of the file it has read already, so reads go on, and Check's own
// read of the file finds it, naming the batch and the offset where the
// damage was found: the batch's start for its head and its checksum, and
// its marker's offset for its marker. The copy's counts come with it, and
// Open fails on the file. A file cut short is damage the read before Check
// meets, named once, and a fresh Open takes the cut for a torn end.
func TestCheckNamesDamageInTheFile(t *testing.T) {
	zeros := func(n int64) []byte { return make([]byte, n) }
	for _, c := range []struct {
		what   string
		plant  func(t *testing.T, path string, b []spot)
		batch  int
		at     func(b []spot) int64
		reason string // what the damage's reason starts with
	}{
		{"a bit changed in batch 2's changes", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at+30, 0x10) },
			2, func(b []spot) int64 { return b[1].at }, "the checksum is "},
		{"a bit changed in batch 2's magic number", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at, 0x01) },
			2, func(b []spot) int64 { return b[1].at }, "the magic number is "},
		{"a bit changed in batch 2's generation", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at+4, 0x02) },
			2, func(b []spot) int64 { return b[1].at }, "generation 3, in a file of generation 1"},
		{"a bit changed in batch 2's length", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at+19, 0x01) },
			2, func(b []spot) int64 { return b[1].at }, "a length of "},
		{"a bit changed in batch 2's sequence number", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at+20, 0x01) },
			2, func(b []spot) int64 { return b[1].at }, "sequence number 3, where 2 comes next"},
		{"batch 2's head turned to zeros", func(t *testing.T, path string, b []spot) { writeAt(t, path, zeros(format.BatchHeadSize), b[1].at) },
			2, func(b []spot) int64 { return b[1].at }, "the magic number is 00 00 00 00"},
		{"batch 2 turned to zeros", func(t *testing.T, path string, b []spot) { writeAt(t, path, zeros(b[1].n), b[1].at) },
			2, func(b []spot) int64 { return b[1].at }, "the magic number is 00 00 00 00"},
		{"a bit changed in batch 2's marker", func(t *testing.T, path string, b []spot) { flip(t, path, b[1].at+b[1].n+16, 0x80) },
			2, func(b []spot) int64 { return b[1].at + b[1].n }, "its marker isn't whole"},
		{"batch 2's marker naming batch 1", func(t *testing.T, path string, b []spot) {
			f, err := os.ReadFile(path)
			ok(t, err)
			writeAt(t, path, f[b[0].at+b[0].n:b[0].at+b[0].n+format.MarkerSize], b[1].at+b[1].n)
		}, 2, func(b []spot) int64 { return b[1].at + b[1].n }, "the whole marker after it names batch 1 "},
		{"a byte of the header's database ID changed", func(t *testing.T, path string, b []spot) { flip(t, path, 12, 0xff) },
			0, func([]spot) int64 { return 64 }, "the header's checksum is "},
		{"the file cut short in batch 3", func(t *testing.T, path string, b []spot) { ok(t, os.Truncate(path, b[2].at+10)) },
			0, func(b []spot) int64 { return b[2].at + 10 }, "the file is "},
	} {
		t.Run(c.what, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.hcx")
			db := fourCommits(t, path)
			b := batchesIn(t, path)
			if len(b) != 4 {
				t.Fatalf("the file holds %d batches, where 4 are wanted", len(b))
			}
			if rep, err := db.Check(); err != nil || !rep.OK() {
				t.Fatalf("before the damage, Check gives %+v, %v", rep, err)
			}
			c.plant(t, path, b)
			rep, err := db.Check()
			want := fmt.Sprintf("the file is damaged: offset %d: %s", c.at(b), c.reason)
			if c.batch != 0 {
				want = fmt.Sprintf("the file is damaged: batch %d at offset %d: %s", c.batch, c.at(b), c.reason)
			}
			if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], want) {
				t.Fatalf("Check gave %q, %v, where one problem starting %q is wanted", rep.Problems, err, want)
			}
			if rep.Tables != 2 || rep.Records != 2 || rep.Links != 0 || rep.Vectors != 0 {
				t.Errorf("Check counts %+v, where the copy holds 2 tables and 2 records", rep)
			}
			cut := strings.HasPrefix(c.what, "the file cut short")
			if _, err := db.Get("docs:2"); (err == nil) == cut {
				t.Errorf("after the damage, a read gives %v", err)
			}
			if again, err := hc.Open(path); cut {
				ok(t, err)
				again.Close()
			} else if err == nil {
				again.Close()
				t.Errorf("Open takes the damaged file")
			} else {
				wantErr(t, err, hc.ErrDamaged)
			}
		})
	}
}

// TestCheckNamesTheDamagedFixtures moves each of the damaged fixtures into
// place under an open database, as a backup put back, so the database's next
// read reloads the file and fails on its damage. Check names the batch and
// the offset that damage.txt gives, as opening the file does, with the
// file's size and nothing of the copy, which holds part of the file. Nothing
// in the file changes.
func TestCheckNamesTheDamagedFixtures(t *testing.T) {
	dir := filepath.Join("..", "internal", "logfile", "testdata", "damaged")
	list, err := os.Open(filepath.Join(dir, "damage.txt"))
	ok(t, err)
	defer list.Close()
	n := 0
	for in := bufio.NewScanner(list); in.Scan(); {
		line := in.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		name := fields[0]
		batch, err := strconv.Atoi(fields[1])
		ok(t, err)
		offset, err := strconv.ParseInt(fields[2], 10, 64)
		ok(t, err)
		n++
		t.Run(name, func(t *testing.T) {
			fixture, err := os.ReadFile(filepath.Join(dir, name))
			ok(t, err)
			path := filepath.Join(t.TempDir(), "test.hcx")
			db := open(t, path)
			ok(t, db.Put("docs:1", nil))
			ok(t, os.WriteFile(path+".backup", fixture, 0o644))
			ok(t, os.Rename(path+".backup", path))
			rep, err := db.Check()
			want := fmt.Sprintf("the file is damaged: offset %d: ", offset)
			if batch != 0 {
				want = fmt.Sprintf("the file is damaged: batch %d at offset %d: ", batch, offset)
			}
			if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], want) {
				t.Fatalf("Check gave %q, %v, where one problem starting %q is wanted", rep.Problems, err, want)
			}
			if rep.Tables+rep.Records+rep.Links+rep.Vectors != 0 || rep.MemoryBytes != 0 || rep.FileBytes != int64(len(fixture)) {
				t.Errorf("Check gives %+v, where the file's size, %d, and nothing more is wanted", rep, len(fixture))
			}
			if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, fixture) {
				t.Errorf("the file changed, from %d bytes to %d: %v", len(fixture), len(now), err)
			}
		})
	}
	ok(t, list.Close())
	if n < 25 {
		t.Fatalf("damage.txt lists %d fixtures", n)
	}
}

// TestCheckNamesABatchRefusedAtAFollow writes a batch whose changes break
// the rules for the state they apply to beside a database that has the
// file open, as no store would write it. The read before Check meets it as
// it follows the file, and Check names it among its problems, at the offset
// where it starts, with the change at fault, beside the copy's counts as the
// batch before left them.
func TestCheckNamesABatchRefusedAtAFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	info, err := os.Stat(path)
	ok(t, err)
	n := func(i int64) []format.Field { return []format.Field{{Name: "n", Value: value.Int(i)}} }
	writeBatches(t, path, []format.Change{{Op: format.Put, Key: "docs:2", Fields: n(2)}, {Op: format.Put, Key: "nosuch:1", Fields: n(3)}})
	rep, err := db.Check()
	want := fmt.Sprintf("the file is damaged: batch 2 at offset %d: change 2: ", info.Size())
	if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], want) {
		t.Fatalf("Check gave %q, %v, where one problem starting %q is wanted", rep.Problems, err, want)
	}
	if rep.Tables != 1 || rep.Records != 1 || rep.MemoryBytes == 0 {
		t.Errorf("Check gives %+v, where the copy holds a table and a record", rep)
	}
}

// TestCheckAfterAReloadThatFails: a reload that fails on a read gives its
// error to Check, which can't check, until a reload works. And while an
// Update holds the write lock after a reload that failed on damage, a
// Check's read doesn't reload, and Check names the damage the last reload
// met, with nothing of the copy, which holds part of the file.
func TestCheckAfterAReloadThatFails(t *testing.T) {
	defer watchdog()()
	t.Run("a read that fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		db, trap := openTrapped(t, path)
		ok(t, db.Put("docs:1", nil))
		moveIn(t, path, func(o *hc.DB) { ok(t, o.Put("good:1", nil)) })
		trap.setRead(func(p string) error {
			if p == path {
				return errDisk
			}
			return nil
		})
		rep, err := db.Check()
		trap.setRead(nil)
		if !errors.Is(err, errDisk) {
			t.Fatalf("Check gave %+v, %v, where the reload's error is wanted", rep, err)
		}
		if rep, err := db.Check(); err != nil || !rep.OK() || rep.Records != 1 {
			t.Fatalf("once the file reads, Check gives %+v, %v", rep, err)
		}
	})
	t.Run("damage, while an Update holds the lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		db, trap := openTrapped(t, path)
		ok(t, db.Put("docs:1", nil))
		other := path + ".other"
		o, err := hc.Open(other)
		ok(t, err)
		ok(t, o.Put("good:1", nil))
		ok(t, o.Close())
		writeBatches(t, other, []format.Change{{Op: format.Put, Key: "nosuch:1", Fields: []format.Field{{Name: "n", Value: value.Int(3)}}}})
		ok(t, os.Rename(other, path))
		rep, err := db.Check()
		if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], "the file is damaged: batch 2 at offset ") {
			t.Fatalf("Check gave %+v, %v, where the damage to batch 2 is wanted", rep, err)
		}
		// An Update whose Lock waits to read the file again, holding the
		// write lock's mutex, so the next Check's read doesn't reload.
		entered, gate := make(chan struct{}), make(chan struct{})
		var once, opening sync.Once
		letOpen := func() { opening.Do(func() { close(gate) }) }
		defer letOpen()
		trap.setOpen(func(p string) {
			if p == path {
				once.Do(func() { close(entered); <-gate })
			}
		})
		updated := make(chan error, 1)
		go func() { updated <- db.Update(func(tx *hc.Tx) error { return tx.Put("after:1", nil) }) }()
		await(t, entered, "the Update's Lock to read the file again")
		rep, err = db.Check()
		letOpen()
		if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], "the file is damaged: batch 2 at offset ") ||
			rep.Tables+rep.Records+rep.Links+rep.Vectors != 0 || rep.MemoryBytes != 0 {
			t.Errorf("while the Update held the lock, Check gave %+v, %v, where the damage to batch 2 alone is wanted", rep, err)
		}
		if err := <-updated; err == nil || !errors.Is(err, hc.ErrDamaged) {
			t.Errorf("the Update on the damaged file gave %v", err)
		}
	})
}

// TestCheckWhileAWriterCommits checks through two handles on one file,
// over and over, while one of them commits a chain of records, each with a
// vector and a link to the one before: through the other handle, which
// follows the file as another process does, and through the writer's own,
// from a goroutine beside its Updates. Every report finds no problems, holds
// a whole chain, so one link fewer than records and a vector for each, and
// holds at least what the one before it through the same handle held. The
// file grows under each check's read of it, which reads only as far as the
// copy has read.
func TestCheckWhileAWriterCommits(t *testing.T) {
	defer watchdog()()
	commits := 200
	if testing.Short() {
		commits = 60
	}
	path := filepath.Join(t.TempDir(), "test.hcx")
	w := open(t, path)
	r := open(t, path)
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= commits; i++ {
			err := w.Update(func(tx *hc.Tx) error {
				key := fmt.Sprintf("chain:%05d", i)
				if err := tx.Put(key, hc.Fields{"n": i, "vec": hc.Vector{float32(i), 1}}); err != nil {
					return err
				}
				if i == 1 {
					return nil
				}
				return tx.Link(key, "prev", fmt.Sprintf("chain:%05d", i-1))
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	last := map[*hc.DB]hc.Report{}
	checks := 0
	for running := true; running; {
		select {
		case err := <-done:
			ok(t, err)
			running = false
		default:
		}
		for _, db := range []*hc.DB{r, w} {
			rep, err := db.Check()
			before := last[db]
			if err != nil || !rep.OK() || rep.Records > 0 && rep.Links != rep.Records-1 || rep.Vectors != rep.Records ||
				rep.Records < before.Records || rep.FileBytes < before.FileBytes {
				t.Fatalf("check %d gave %+v, %v, after %+v", checks, rep, err, before)
			}
			last[db] = rep
			checks++
		}
	}
	for _, db := range []*hc.DB{r, w} {
		if rep, err := db.Check(); err != nil || !rep.OK() || rep.Records != commits {
			t.Fatalf("once the writer is done, Check gives %+v, %v", rep, err)
		}
	}
	t.Logf("%d checks while %d commits went in", checks, commits)
}

// TestCheckReportsSizes: the file's size is its size on disk, and the
// copy's comes to about 1.7 KB a record for records with 384-value vectors,
// as BETA.md has it, a little more than their values, and the same after a
// reopen.
func TestCheckReportsSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	size := func(rep hc.Report) {
		t.Helper()
		info, err := os.Stat(path)
		ok(t, err)
		if rep.FileBytes != info.Size() {
			t.Fatalf("Check gives the file's size as %d, and it's %d", rep.FileBytes, info.Size())
		}
	}
	rep, err := db.Check()
	ok(t, err)
	size(rep)
	if rep.FileBytes != format.HeaderSize || rep.MemoryBytes <= 0 || rep.MemoryBytes > 1024 {
		t.Fatalf("a new database gives %+v", rep)
	}
	const n = 1000
	ok(t, db.Update(func(tx *hc.Tx) error {
		vec := make(hc.Vector, 384)
		for i := range n {
			for j := range vec {
				vec[j] = float32(i+j) + 0.5
			}
			if err := tx.Put(fmt.Sprintf("docs:%04d", i), hc.Fields{"vec": vec}); err != nil {
				return err
			}
		}
		return nil
	}))
	rep, err = db.Check()
	ok(t, err)
	size(rep)
	per := float64(rep.MemoryBytes) / n
	if !rep.OK() || per < 1536 || per > 2048 {
		t.Fatalf("%d records with 384-value vectors give %+v, %.0f bytes a record", n, rep, per)
	}
	ok(t, db.Close())
	again := open(t, path)
	rep2, err := again.Check()
	ok(t, err)
	size(rep2)
	if rep2.Records != n || rep2.MemoryBytes < rep.MemoryBytes*9/10 || rep2.MemoryBytes > rep.MemoryBytes*11/10 {
		t.Fatalf("after a reopen, Check gives %+v, where it gave %+v", rep2, rep)
	}
	t.Logf("%d records with 384-value vectors: a file of %d bytes, and %d bytes in memory, %.0f a record, or %d after a reopen", n, rep.FileBytes, rep.MemoryBytes, per, rep2.MemoryBytes)
}

// TestExportAndImportRefuseWhatCheckFinds: with a problem planted in what
// the copy's check finds, Check reports it, Export refuses before it writes
// anything, and Import into a new database refuses and keeps nothing, each
// in 0.x's words. Once the problem has gone, both go ahead.
func TestExportAndImportRefuseWhatCheckFinds(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "a.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	var exp bytes.Buffer
	ok(t, db.Export(&exp))
	problem := "keys in docs missing from the hash table: docs:1"
	restore := hc.PlantProblems(problem)
	defer restore()
	rep, err := db.Check()
	if err != nil || rep.OK() || rep.Problems[0] != problem || rep.Records != 1 {
		t.Fatalf("Check gave %+v, %v, where the planted problem is wanted", rep, err)
	}
	var out bytes.Buffer
	err = db.Export(&out)
	wantErr(t, err, hc.ErrInvalid)
	if want := "hypercrux: invalid: hypercrux check finds problems in the file, so it isn't exported; the first is: " + problem; err.Error() != want || out.Len() != 0 {
		t.Fatalf("Export gave %v and wrote %d bytes, where it should refuse with %q and write nothing", err, out.Len(), want)
	}
	b := open(t, filepath.Join(dir, "b.hcx"))
	err = b.Import(bytes.NewReader(exp.Bytes()))
	wantErr(t, err, hc.ErrInvalid)
	if want := "hypercrux: invalid: hypercrux check finds problems in the file, so nothing was imported; the first is: " + problem; err.Error() != want {
		t.Fatalf("Import gave %v, where %q is wanted", err, want)
	}
	if rep, err := b.Check(); err != nil || rep.Tables != 0 {
		t.Fatalf("after the refused import, Check gives %+v, %v", rep, err)
	}
	restore()
	ok(t, b.Import(bytes.NewReader(exp.Bytes())))
	out.Reset()
	ok(t, b.Export(&out))
	if out.String() != exp.String() {
		t.Fatalf("once the problem has gone, the import exports as\n%s\nwhere\n%s\nwent in", out.String(), exp.String())
	}
}
