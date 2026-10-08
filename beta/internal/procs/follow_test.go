// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// The real log followed (F6). Writers commit through the real log as in
// realLog, on a slow disk, and each reader opens the database once and
// follows it, calling
// Follow again and again until it's killed, so it sees each commit once,
// as it's made. A writer's slot stays empty for a while after its writer
// is killed, so there are times with no writer at all. Then a follower that
// stops at what a killed writer left finds it can take the lock, and checks
// the end of the log itself: it marks the batch the writer left synced and
// unmarked, or cuts off what it left half written.
var realFollowed = Workload{Name: "real log followed", Write: func(w *Writer) error { return realWriteOn(slowSync{fsys.OS{}}, w) }, Read: realFollow, Final: realFinal}

// slowSync is the real calls, with each sync taking a millisecond more, as
// on a slow disk, so that a kill often lands between a batch's write and
// its marker's, whatever the disk under the test. The writer killed there
// leaves the batch, which the page cache keeps, for the next holder of the
// lock to mark.
type slowSync struct{ fsys.FS }

func (s slowSync) Open(path string) (fsys.File, error) {
	f, err := s.FS.Open(path)
	if err != nil {
		return nil, err
	}
	return slowFile{f}, nil
}

func (s slowSync) Create(path string, perm fs.FileMode) (fsys.File, error) {
	f, err := s.FS.Create(path, perm)
	if err != nil {
		return nil, err
	}
	return slowFile{f}, nil
}

type slowFile struct{ fsys.File }

func (f slowFile) Sync() error {
	err := f.File.Sync()
	time.Sleep(time.Millisecond)
	return err
}

// What a follower counts, each time a Follow of its makes the check.
const (
	followerChecked = "follower checked the end of the log"
	followerMarked  = "follower marked a batch a writer left"
	followerCut     = "follower cut off what a writer left"
)

// realFollow is a follower's work on the real log: the database opened
// once, then followed until the process is killed. After each Follow, it
// counts what that Follow's check of the end of the log did, by the calls
// this process made: flock got, a batch written again, and a cut.
func realFollow(r *Reader) error {
	r.Reset()
	files := &tallyFS{FS: fsys.OS{}}
	l, err := logfile.Open(files, r.Path(), readerTarget{r}, logfile.Options{})
	if err != nil {
		return err
	}
	defer l.Close()
	for {
		files.calls = tally{}
		if err := l.Follow(); err != nil {
			return err
		}
		if files.calls.locked > 0 {
			r.Count(followerChecked)
		}
		if files.calls.written > 0 {
			r.Count(followerMarked)
		}
		if files.calls.cut > 0 {
			r.Count(followerCut)
		}
		time.Sleep(time.Duration(r.Rand().IntN(2000)) * time.Microsecond)
	}
}

// tally is what a follower's files did: the times they got flock, the
// writes longer than a marker, which only write a batch again, and the
// cuts.
type tally struct{ locked, written, cut int }

// tallyFS is the real calls, keeping a tally of what the files it opens do.
// A follower uses it from one goroutine.
type tallyFS struct {
	fsys.FS
	calls tally
}

func (t *tallyFS) Open(path string) (fsys.File, error) {
	f, err := t.FS.Open(path)
	if err != nil {
		return nil, err
	}
	return &tallyFile{File: f, t: &t.calls}, nil
}

func (t *tallyFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	f, err := t.FS.Create(path, perm)
	if err != nil {
		return nil, err
	}
	return &tallyFile{File: f, t: &t.calls}, nil
}

type tallyFile struct {
	fsys.File
	t *tally
}

func (f *tallyFile) TryLock() (bool, error) {
	ok, err := f.File.TryLock()
	if ok {
		f.t.locked++
	}
	return ok, err
}

func (f *tallyFile) WriteAt(p []byte, off int64) (int, error) {
	if len(p) > format.MarkerSize {
		f.t.written++
	}
	return f.File.WriteAt(p, off)
}

func (f *tallyFile) Truncate(size int64) error {
	f.t.cut++
	return f.File.Truncate(size)
}

// TestFollowersSeeEveryCommitInOrder is F6's closing test: two writers
// committing through the real log on a slow disk, and two followers that
// open it once and follow it, each killed with SIGKILL at a random moment
// and a new one started in its place, a writer only after a gap of 20 to
// 150 milliseconds, for 2 seconds, or half a second with -short. Followers
// live 50 milliseconds to a second. Every follower sees every commit once
// and in order, the followers agree with each other and with the file at
// the end, every commit a writer saw succeed is in the file, and every
// commit under way when its writer was killed is there whole or not at
// all. The run goes on until followers have marked 3 batches that killed
// writers left, in the gaps.
func TestFollowersSeeEveryCommitInOrder(t *testing.T) {
	rep, err := Run(realFollowed, Options{
		Path:       filepath.Join(t.TempDir(), "db.hcx"),
		ReaderLife: Span{Min: 50 * time.Millisecond, Max: time.Second},
		WriterGap:  Span{Min: 20 * time.Millisecond, Max: 150 * time.Millisecond},
		Least:      map[string]int{followerMarked: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}
