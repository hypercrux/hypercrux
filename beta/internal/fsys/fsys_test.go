// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys_test

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// The stubs implement both interfaces and do nothing, so the interfaces
// compile against something until F2 writes the real calls. If an
// interface changes, they stop compiling, which is the reminder that every
// implementation and caller changes with it.

var errStub = errors.New("fsys: a stub")

type stubFS struct{}

func (stubFS) Open(string) (fsys.File, error)                { return stubFile{}, errStub }
func (stubFS) Create(string, fs.FileMode) (fsys.File, error) { return stubFile{}, errStub }
func (stubFS) Rename(string, string) error                   { return errStub }
func (stubFS) RenameNoReplace(string, string) error          { return errStub }
func (stubFS) Remove(string) error                           { return errStub }
func (stubFS) SyncDir(string) error                          { return errStub }
func (stubFS) Stat(string) (fsys.Info, error)                { return fsys.Info{}, errStub }
func (stubFS) RealPath(string) (string, error)               { return "", errStub }
func (stubFS) List(string) ([]string, error)                 { return nil, errStub }

type stubFile struct{}

func (stubFile) ReadAt([]byte, int64) (int, error)  { return 0, errStub }
func (stubFile) WriteAt([]byte, int64) (int, error) { return 0, errStub }
func (stubFile) Sync() error                        { return errStub }
func (stubFile) Truncate(int64) error               { return errStub }
func (stubFile) Stat() (fsys.Info, error)           { return fsys.Info{}, errStub }
func (stubFile) Chown(int, int) error               { return errStub }
func (stubFile) TryLock() (bool, error)             { return false, errStub }
func (stubFile) Unlock() error                      { return errStub }
func (stubFile) Close() error                       { return errStub }

func TestTheCallsCompile(t *testing.T) {
	var s fsys.FS = stubFS{}
	f, err := s.Open("db")
	errs := []error{err}
	_, err = s.Create("db.new-abc", 0o644)
	errs = append(errs, err, s.Rename("a", "b"), s.RenameNoReplace("a", "b"), s.Remove("a"), s.SyncDir("."))
	_, err = s.Stat("db")
	errs = append(errs, err)
	_, err = s.RealPath("db")
	errs = append(errs, err)
	_, err = s.List(".")
	errs = append(errs, err)

	_, err = f.ReadAt(make([]byte, 28), 68)
	errs = append(errs, err)
	_, err = f.WriteAt([]byte("HCRM"), 68)
	errs = append(errs, err, f.Sync(), f.Truncate(68))
	_, err = f.Stat()
	errs = append(errs, err, f.Chown(0, 0))
	_, err = f.TryLock()
	errs = append(errs, err, f.Unlock(), f.Close())

	if len(errs) != 18 {
		t.Errorf("%d calls made, and the two interfaces have 18", len(errs))
	}
	for i, err := range errs {
		if !errors.Is(err, errStub) {
			t.Errorf("call %d returned %v", i+1, err)
		}
	}
}

func TestInfoSame(t *testing.T) {
	a := fsys.Info{Dev: 2049, Ino: 77, Nlink: 1, Size: 68}
	b := a
	b.Size, b.Nlink = 4096, 2
	if !a.Same(b) {
		t.Error("the same device and inode aren't the same file")
	}
	b.Ino = 78
	if a.Same(b) {
		t.Error("another inode is the same file")
	}
	b.Ino, b.Dev = 77, 2050
	if a.Same(b) {
		t.Error("another device is the same file")
	}
}

// TestSyscallHasTheCalls checks that Go's syscall package has, on this
// architecture, what the real file calls need, since golang.org/x/sys
// can't be fetched here. renameat2 is the gap. syscall has its number on
// arm64 (SYS_RENAMEAT2, 276) and not on amd64 (316), and exports neither
// AT_FDCWD (-100) nor RENAME_NOREPLACE (1), so F2 defines those and calls
// renameat2 through Syscall6. RealPath and List can use path/filepath and
// os.
func TestSyscallHasTheCalls(t *testing.T) {
	calls := []any{
		syscall.Open, syscall.Close, syscall.Pread, syscall.Pwrite, syscall.Fdatasync,
		syscall.Fsync, syscall.Ftruncate, syscall.Fstat, syscall.Stat, syscall.Fchown,
		syscall.Fchmod, syscall.Flock, syscall.Rename, syscall.Unlink,
		syscall.Syscall6, syscall.BytePtrFromString,
	}
	flags := []int{
		syscall.O_RDWR, syscall.O_RDONLY, syscall.O_CREAT, syscall.O_EXCL, syscall.O_CLOEXEC,
		syscall.O_DIRECTORY, syscall.LOCK_EX, syscall.LOCK_NB, syscall.LOCK_UN,
	}
	if len(calls) != 16 || len(flags) != 9 {
		t.Fatal("the lists changed without the counts")
	}
	// What FS's errors promise errors.Is will match. renameat2 refuses a
	// flag the file system lacks with EINVAL, which doesn't match
	// errors.ErrUnsupported, so RenameNoReplace turns that one itself.
	for _, c := range []struct {
		errno  syscall.Errno
		target error
	}{
		{syscall.EEXIST, fs.ErrExist},
		{syscall.ENOENT, fs.ErrNotExist},
		{syscall.ENOSYS, errors.ErrUnsupported},
	} {
		if !errors.Is(c.errno, c.target) {
			t.Errorf("%v doesn't match %v", c.errno, c.target)
		}
	}
}
