// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The toy log is the driver's test subject, a log of its own that's small
// enough to read at a sitting: a header-free sequence of batches, in
// beta/internal/format's encoding. A commit writes its batch at the end of
// the log, syncs the file, then writes the batch's marker after it, as the
// real log does (FORMAT.md, "Writing").
//
// Opening it recovers just enough for the driver to check it:
//
//   - It reads the marked batches from the start, and the log ends at the
//     first batch that isn't marked.
//   - Past the end of the log, a whole marker naming the next batch or a
//     later one is damage, and opening fails. A writer marks a batch only
//     once its sync has returned, so no crash leaves a marker without its
//     batch.
//   - A batch that counts straight after the end of the log, with no marker
//     after it, was left by a writer that died once it had written it. It's
//     written again in place, synced and marked, and read with the rest, as
//     FORMAT.md's check of the end of the log does. A marker needs no sync
//     of its own, so a cut can take the marker of a commit that succeeded,
//     and this is how the commit comes back. The batch is written again
//     because after a failed sync Linux can mark its pages clean.
//
// Opening it for more commits does the same, then cuts off whatever lies
// after the last marker and syncs the cut, before anything is appended.

// toyPath is where the toy log lives.
const toyPath = "/db/toy"

// The toy log's database ID and generation, which every marker's check
// value covers.
var toyID = [16]byte{'t', 'o', 'y', ' ', 'l', 'o', 'g'}

const toyGen = 1

// The ways the toy log can go wrong, for the tests that show the driver
// catching them. The first is also the package's planted bug, which the
// toy log in TestTheToyLogPassesEveryPoint has when HYPERCRUX_PLANT names
// it in a build with the hypercrux_planted tag.
const (
	markerBeforeSync = "crash/marker-before-sync" // the marker written before the sync
	noSync           = "no sync"                  // the sync left out
	markerFirst      = "marker first"             // the marker written before its batch, then the sync
)

// toyLog is a toy log open for its commits.
type toyLog struct {
	f   fsys.File
	seq uint64 // the last batch committed, or 0
	end int64  // where the next batch goes
	bug string // one of the ways it can go wrong, or ""
}

// createToy makes a new toy log at name through sys, and syncs its folder,
// so its name lasts through a cut.
func createToy(sys fsys.FS, name, bug string) (*toyLog, error) {
	f, err := sys.Create(name, 0o644)
	if err != nil {
		return nil, err
	}
	if err := sys.SyncDir(path.Dir(name)); err != nil {
		f.Close()
		return nil, err
	}
	return &toyLog{f: f, bug: bug}, nil
}

// appendToy opens the toy log at toyPath through sys for more commits. It
// recovers the log as openToy does, then cuts off everything after the
// last marker and syncs the cut, as FORMAT.md's check of the end of the
// log does before anything is appended.
func appendToy(sys fsys.FS, bug string) (*toyLog, error) {
	f, err := sys.Open(toyPath)
	if err != nil {
		return nil, err
	}
	_, seq, end, err := recoverToy(f)
	if err == nil {
		err = f.Truncate(end)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return &toyLog{f: f, seq: seq, end: end, bug: bug}, nil
}

// commit appends changes as the next batch: the batch, a sync, then the
// batch's marker, unless the log has a bug.
func (l *toyLog) commit(changes []format.Change) error {
	seq := l.seq + 1
	b, sum, err := format.AppendBatch(nil, toyGen, seq, changes)
	if err != nil {
		return err
	}
	n := len(b)
	b = format.AppendMarker(b, toyID, toyGen, format.Marker{Seq: seq, Sum: sum})
	batch := func() error { _, err := l.f.WriteAt(b[:n], l.end); return err }
	marker := func() error { _, err := l.f.WriteAt(b[n:], l.end+int64(n)); return err }
	steps := []func() error{batch, l.f.Sync, marker}
	switch l.bug {
	case markerBeforeSync:
		steps = []func() error{batch, marker, l.f.Sync}
	case noSync:
		steps = []func() error{batch, marker}
	case markerFirst:
		steps = []func() error{marker, batch, l.f.Sync}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	l.seq, l.end = seq, l.end+int64(len(b))
	return nil
}

// openToy opens the toy log at toyPath through sys and recovers it. Its
// state is the keys its batches put, in order, with a space between each
// two. With no file at the path, the log is empty.
func openToy(sys fsys.FS) (string, error) {
	f, err := sys.Open(toyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	keys, _, _, err := recoverToy(f)
	return strings.Join(keys, " "), err
}

// recoverToy reads the toy log in f and recovers it, as the toy log's
// comment says. It returns the keys its batches put, in order, the last
// batch's sequence number, and where the log ends: just after the last
// marker.
func recoverToy(f fsys.File) (keys []string, seq uint64, end int64, err error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	data := make([]byte, info.Size)
	if n, err := f.ReadAt(data, 0); n < len(data) {
		return nil, 0, 0, err
	}
	off := 0
	for {
		bt, err := format.DecodeBatch(data[off:], toyGen, seq+1)
		if errors.Is(err, format.ErrDoesNotCount) {
			break
		}
		if err != nil {
			return nil, 0, 0, err // a batch that counts, holding changes that don't
		}
		m, whole := format.DecodeMarker(data[off+bt.Length:], toyID, toyGen)
		if !whole || m != (format.Marker{Seq: bt.Seq, Sum: bt.Sum}) {
			break
		}
		keys = append(keys, bt.Changes[0].Key)
		off += bt.Length + format.MarkerSize
		seq = bt.Seq
	}
	for p := off; p+format.MarkerSize <= len(data); p++ {
		if m, whole := format.DecodeMarker(data[p:], toyID, toyGen); whole && m.Seq > seq {
			return nil, 0, 0, fmt.Errorf("the toy log is damaged: a whole marker at offset %d names batch %d, and the log ends after batch %d", p, m.Seq, seq)
		}
	}
	if bt, err := format.DecodeBatch(data[off:], toyGen, seq+1); err == nil {
		marker := format.AppendMarker(nil, toyID, toyGen, format.Marker{Seq: bt.Seq, Sum: bt.Sum})
		if _, err := f.WriteAt(data[off:off+bt.Length], int64(off)); err != nil {
			return nil, 0, 0, err
		}
		if err := f.Sync(); err != nil {
			return nil, 0, 0, err
		}
		if _, err := f.WriteAt(marker, int64(off+bt.Length)); err != nil {
			return nil, 0, 0, err
		}
		keys = append(keys, bt.Changes[0].Key)
		off += bt.Length + format.MarkerSize
		seq = bt.Seq
	}
	return keys, seq, int64(off), nil
}

// toyWorkload makes commits commits on a new toy log with the bug bug, the
// ith a batch that puts the record t:i.
func toyWorkload(commits int, bug string) Workload {
	return Workload{
		Path: toyPath,
		Run: func(sys fsys.FS, m *Model) error {
			l, err := createToy(sys, toyPath, bug)
			if err != nil {
				return err
			}
			defer l.f.Close()
			return toyCommits(l, m, 1, commits)
		},
		Reopen: openToy,
	}
}

// toyCompactWorkload makes commits commits on a new toy log, then compacts
// it in the way BETA.md's "Compaction" describes for the real log: the
// same batches go into a new file beside it, which is synced and renamed
// over the log, and then the folder is synced. Then it makes commits more
// commits on the new file. A copy that opened the log before the rename
// goes on reading the old file.
func toyCompactWorkload(commits int, bug string) Workload {
	return Workload{
		Path: toyPath,
		Run: func(sys fsys.FS, m *Model) error {
			l, err := createToy(sys, toyPath, bug)
			if err != nil {
				return err
			}
			defer func() { l.f.Close() }()
			if err := toyCommits(l, m, 1, commits); err != nil {
				return err
			}
			next, err := createToy(sys, toyPath+".compact", bug)
			if err != nil {
				return err
			}
			l.f.Close()
			l = next
			if err := toyCommits(l, nil, 1, commits); err != nil {
				return err
			}
			if err := sys.Rename(toyPath+".compact", toyPath); err != nil {
				return err
			}
			if err := sys.SyncDir(path.Dir(toyPath)); err != nil {
				return err
			}
			return toyCommits(l, m, commits+1, 2*commits)
		},
		Reopen: openToy,
	}
}

// toySetupWorkload is setup commits made by Setup on a new toy log with
// the bug bug, then commits more on the same log, opened again for them.
func toySetupWorkload(setup, commits int, bug string) Workload {
	return Workload{
		Path: toyPath,
		Setup: func(sys fsys.FS) (string, error) {
			l, err := createToy(sys, toyPath, bug)
			if err != nil {
				return "", err
			}
			defer l.f.Close()
			return toyState(setup), toyCommits(l, nil, 1, setup)
		},
		Run: func(sys fsys.FS, m *Model) error {
			l, err := appendToy(sys, bug)
			if err != nil {
				return err
			}
			defer l.f.Close()
			return toyCommits(l, m, setup+1, setup+commits)
		},
		Reopen: openToy,
	}
}

// toyCommits makes the commits from first to last on l, telling m about
// each one when m isn't nil.
func toyCommits(l *toyLog, m *Model, first, last int) error {
	for i := first; i <= last; i++ {
		if m != nil {
			m.Begin(toyState(i))
		}
		if err := l.commit(toyBatch(i)); err != nil {
			return err
		}
		if m != nil {
			m.Done()
		}
	}
	return nil
}

// toyKey is the key commit i puts.
func toyKey(i int) string { return "t:" + strconv.Itoa(i) }

// toyState is the state after commit i: the keys of commits 1 to i.
func toyState(i int) string {
	keys := make([]string, i)
	for j := range keys {
		keys[j] = toyKey(j + 1)
	}
	return strings.Join(keys, " ")
}

// toyBatch is commit i's batch: a put of its key with a text field of 1 to
// 1,300 letters, as many as i picks, so that batches and their markers
// fall across sectors in many ways.
func toyBatch(i int) []format.Change {
	text := make([]byte, i*389%1300+1)
	for j := range text {
		text[j] = 'a' + byte((i+j)%26)
	}
	return []format.Change{{Op: format.Put, Key: toyKey(i), Fields: []format.Field{{Name: "x", Value: value.Text(string(text))}}}}
}
