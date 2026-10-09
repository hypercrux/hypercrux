// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The growth of the file and of a commit (F7).
//
// The sync cost of a growing file. BETA.md's "Commits" says that each
// sync of a file that grew also commits its new size, which can cost more than
// a sync after a write into space the file has already. BenchmarkCommitSync
// measures it, with the log's own calls and the bytes it writes: each commit
// writes its batch where the log ends, syncs the file, then writes the
// marker, as Append does, and the next commit starts after the marker.
//
// It runs three sizes of commit, each three ways:
//
//   - put: the batch of BenchmarkPut's committed put in beta/bench, a record
//     of three small fields, 96 bytes;
//   - vector: one of Put_384dims, with a 384-value vector, 1,613 bytes;
//   - thousand: one of PutBatch_384dims, a thousand of those, 1.6 MB.
//
// And the ways:
//
//   - append: each commit makes the file longer, as the log's commits do;
//   - zeros: each commit writes over zeros that were written and synced
//     before the timing starts, so the file's size never changes. That's
//     what writing zeros ahead of the log would give. The zeros are a
//     region of 64 MB at most, which the commits go round when they reach
//     its end, so they always write over space the file has;
//   - reserved: each commit makes the file longer, into room fallocate
//     reserved past its end beforehand (FALLOC_FL_KEEP_SIZE), so a reader
//     could still take the file's size for the end of the log. BETA.md
//     expects no gain from it, since the first write into reserved room
//     still needs a journal commit on ext4 and XFS.
//
// ns/op is a whole commit's three calls, and sync-ns/op the sync's share of
// it. It runs only with -bench, on the file system of the temporary folder
// go test uses, so it measures that folder's drive:
//
//	go test -run '^$' -bench CommitSync -benchtime 3s -count 3 ./beta/internal/logfile
func BenchmarkCommitSync(b *testing.B) {
	for _, size := range commitSizes() {
		for _, way := range []string{"append", "zeros", "reserved"} {
			b.Run(size.name+"/"+way, func(b *testing.B) { benchCommitSync(b, size.batch, way) })
		}
	}
}

// commitSize is a commit's batch, as Append writes it, named.
type commitSize struct {
	name  string
	batch []byte
}

// commitSizes returns the batches of the three commits BenchmarkCommitSync
// times, made by the codec as the store's change lists make them: the puts of
// beta/bench's benchmarks, of a record that's in its table already.
func commitSizes() []commitSize {
	small := func(i int) format.Change {
		return format.Change{Op: format.Put, Key: "docs:" + strconv.Itoa(100000+i), Fields: []format.Field{
			{Name: "n", Value: value.Int(int64(i))},
			{Name: "status", Value: value.Text("open")},
			{Name: "title", Value: value.Text("a title")},
		}}
	}
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i%7) - 3.5
	}
	vector := func(i int) format.Change {
		return format.Change{Op: format.Put, Key: "docs:" + strconv.Itoa(100000+i), Fields: []format.Field{
			{Name: "title", Value: value.Text("a title")},
			{Name: "vec", Value: value.Vector(vec)},
		}}
	}
	thousand := make([]format.Change, 1000)
	for i := range thousand {
		thousand[i] = vector(i)
	}
	var sizes []commitSize
	for _, c := range []struct {
		name    string
		changes []format.Change
	}{
		{"put", []format.Change{small(1)}},
		{"vector", []format.Change{vector(1)}},
		{"thousand", thousand},
	} {
		batch, _, err := format.AppendBatch(nil, 1, 1, c.changes)
		if err != nil {
			panic(err) // the changes keep the rules
		}
		sizes = append(sizes, commitSize{c.name, batch})
	}
	return sizes
}

// zerosRegion is the most room BenchmarkCommitSync's zeros take.
const zerosRegion = 64 << 20

// fallocKeepSize is FALLOC_FL_KEEP_SIZE, which the syscall package doesn't
// name: room reserved past the end of the file leaves its size as it is.
const fallocKeepSize = 1

// benchCommitSync times b.N commits of batch, the way way says, in a file of
// its own that starts with a database's header, synced, as a new database
// does.
func benchCommitSync(b *testing.B, batch []byte, way string) {
	path := filepath.Join(b.TempDir(), "db")
	f, err := fsys.OS{}.Create(path, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	head, err := format.AppendHeader(nil, format.Header{ID: [16]byte{'s', 'y', 'n', 'c'}, Gen: 1})
	if err != nil {
		b.Fatal(err)
	}
	marker := format.AppendMarker(nil, [16]byte{'s', 'y', 'n', 'c'}, 1, format.Marker{Seq: 1, Sum: 1})
	commit := int64(len(batch) + len(marker))
	start := int64(len(head))
	if _, err := f.WriteAt(head, 0); err != nil {
		b.Fatal(err)
	}

	// The room each way needs, made before the timing starts.
	end := int64(-1) // where the commits go round to the start, for zeros
	switch way {
	case "zeros":
		n := min(int64(b.N), max(zerosRegion/commit, 2))
		end = start + n*commit
		zeros := make([]byte, 1<<20)
		for at := start; at < end; at += int64(len(zeros)) {
			if _, err := f.WriteAt(zeros[:min(int64(len(zeros)), end-at)], at); err != nil {
				b.Fatal(err)
			}
		}
	case "reserved":
		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			b.Fatal(err)
		}
		err = syscall.Fallocate(int(raw.Fd()), fallocKeepSize, start, int64(b.N)*commit)
		raw.Close()
		if err != nil {
			b.Skipf("fallocate with FALLOC_FL_KEEP_SIZE: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		b.Fatal(err)
	}

	var synced time.Duration
	at := start
	b.SetBytes(int64(len(batch)))
	b.ResetTimer()
	for range b.N {
		if end > 0 && at+commit > end {
			at = start
		}
		if _, err := f.WriteAt(batch, at); err != nil {
			b.Fatal(err)
		}
		began := time.Now()
		if err := f.Sync(); err != nil {
			b.Fatal(err)
		}
		synced += time.Since(began)
		if _, err := f.WriteAt(marker, at+int64(len(batch))); err != nil {
			b.Fatal(err)
		}
		at += commit
	}
	b.StopTimer()
	b.ReportMetric(float64(synced.Nanoseconds())/float64(b.N), "sync-ns/op")
	if info, err := f.Stat(); err == nil && way != "zeros" && info.Size != start+int64(b.N)*commit {
		b.Fatal(fmt.Errorf("the file is %d bytes, where %d commits of %d bytes make %d", info.Size, b.N, commit, start+int64(b.N)*commit))
	}
}

// TestACommitsBufferIsKept: Append builds each batch in a buffer the Log keeps
// for the next commit, up to maxKept, so a run of commits of a thousand puts
// with 384-value vectors, 1.6 MB a batch, makes the buffer once. A batch
// larger than maxKept gets a buffer of its own, which goes with its commit,
// and the Log keeps the one it had. Every batch is in the file as the codec
// makes it, whatever buffer it was built in.
func TestACommitsBufferIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	l, _ := openLog(t, path, Options{})
	sizes := commitSizes()
	thousand, err := format.DecodeBatch(sizes[2].batch, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	huge := []format.Change{{Op: format.Put, Key: "docs:huge", Fields: []format.Field{{Name: "body", Value: value.Text(strings.Repeat("h", maxKept))}}}}
	commit(t, l, thousand.Changes)
	kept := unsafe.SliceData(l.batch)
	if cap(l.batch) < len(sizes[2].batch) {
		t.Fatalf("after a commit of %d bytes, the Log keeps a buffer of %d", len(sizes[2].batch), cap(l.batch))
	}
	commit(t, l, thousand.Changes, huge, thousand.Changes)
	if unsafe.SliceData(l.batch) != kept {
		t.Error("the Log made its buffer again, or kept the one a batch past maxKept had")
	}
	r := reread(t, path)
	r.holds(t, thousand.Changes, thousand.Changes, huge, thousand.Changes)
}
