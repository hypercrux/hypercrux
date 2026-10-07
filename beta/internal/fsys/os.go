// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// OS is the real file calls, straight to Linux through Go's syscall
// package.
//
//   - Every file and folder it opens has O_CLOEXEC, so no child process can
//     end up holding a lock.
//   - A call the kernel interrupts with EINTR is made again, apart from
//     close, which Linux finishes whatever it returns.
//   - Errors are the system's, wrapped with the path in a *fs.PathError,
//     or both paths in an *os.LinkError for a rename, so errors.Is matches
//     them against fs.ErrExist, fs.ErrNotExist or a syscall.Errno.
type OS struct{}

var _ FS = OS{}

// Open opens an existing file for reading and writing.
func (OS) Open(path string) (File, error) {
	fd, err := open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &file{fd: fd, path: path}, nil
}

// Create makes a file at path with O_EXCL, then sets its permissions with
// fchmod, since the umask may have taken some of perm away.
func (OS) Create(path string, perm fs.FileMode) (File, error) {
	mode := unixMode(perm)
	fd, err := open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: path, Err: err}
	}
	if err := retry(func() error { return syscall.Fchmod(fd, mode) }); err != nil {
		// The file is this call's own, made with O_EXCL a moment ago, so
		// removing it harms nobody, and the caller gets no file with the
		// wrong permissions.
		syscall.Close(fd)
		retry(func() error { return syscall.Unlink(path) })
		return nil, &fs.PathError{Op: "fchmod", Path: path, Err: err}
	}
	return &file{fd: fd, path: path}, nil
}

// Rename moves from to to with rename, replacing any file there.
func (OS) Rename(from, to string) error {
	if err := retry(func() error { return syscall.Rename(from, to) }); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// RenameNoReplace moves from to to with renameat2 and RENAME_NOREPLACE.
func (OS) RenameNoReplace(from, to string) error {
	if err := renameat2(from, to, renameNoReplace); err != nil {
		return &os.LinkError{Op: "renameat2", Old: from, New: to, Err: err}
	}
	return nil
}

// Remove removes the name path with unlink.
func (OS) Remove(path string) error {
	if err := retry(func() error { return syscall.Unlink(path) }); err != nil {
		return &fs.PathError{Op: "remove", Path: path, Err: err}
	}
	return nil
}

// SyncDir opens the folder dir and syncs it with fsync.
func (OS) SyncDir(dir string) error {
	fd, err := open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	err = retry(func() error { return syscall.Fsync(fd) })
	closed := syscall.Close(fd)
	if err != nil {
		return &fs.PathError{Op: "fsync", Path: dir, Err: err}
	}
	if closed != nil {
		return &fs.PathError{Op: "close", Path: dir, Err: closed}
	}
	return nil
}

// Stat describes the file at path with stat, which follows symbolic links.
func (OS) Stat(path string) (Info, error) {
	var st syscall.Stat_t
	if err := retry(func() error { return syscall.Stat(path, &st) }); err != nil {
		return Info{}, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return infoOf(&st), nil
}

// RealPath resolves every symbolic link in path, and makes the result
// absolute from the working folder's own real path, so a ".." in path, or
// a symbolic link in the working folder's name, can't lead it astray.
func (OS) RealPath(path string) (string, error) {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if wd, err = filepath.EvalSymlinks(wd); err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// List reads the folder dir's names, which os.ReadDir sorts in byte order.
func (OS) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// file is a File the real calls opened: a file descriptor, and the path it
// was opened by, for errors. Close mustn't run alongside the file's other
// calls, since Linux can give the descriptor's number to another file as
// soon as it's closed.
type file struct {
	fd   int
	path string
}

// ReadAt reads with pread until p is full, the file ends, which gives
// io.EOF, or a call fails.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: "read", Path: f.path, Err: errors.New("negative offset")}
	}
	n := 0
	for n < len(p) {
		m, err := pread(f.fd, p[n:], off+int64(n))
		if err != nil {
			return n, &fs.PathError{Op: "read", Path: f.path, Err: err}
		}
		if m == 0 {
			return n, io.EOF
		}
		n += m
	}
	return n, nil
}

// WriteAt writes with pwrite until all of p is written or a call fails.
func (f *file) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: "write", Path: f.path, Err: errors.New("negative offset")}
	}
	n := 0
	for n < len(p) {
		m, err := pwrite(f.fd, p[n:], off+int64(n))
		if err != nil {
			return n, &fs.PathError{Op: "write", Path: f.path, Err: err}
		}
		if m == 0 {
			return n, &fs.PathError{Op: "write", Path: f.path, Err: io.ErrShortWrite}
		}
		n += m
	}
	return n, nil
}

// Sync calls fdatasync, which also makes a change to the file's size last.
func (f *file) Sync() error {
	if err := retry(func() error { return syscall.Fdatasync(f.fd) }); err != nil {
		return &fs.PathError{Op: "fdatasync", Path: f.path, Err: err}
	}
	return nil
}

// Truncate calls ftruncate.
func (f *file) Truncate(size int64) error {
	if err := retry(func() error { return syscall.Ftruncate(f.fd, size) }); err != nil {
		return &fs.PathError{Op: "truncate", Path: f.path, Err: err}
	}
	return nil
}

// Stat calls fstat.
func (f *file) Stat() (Info, error) {
	var st syscall.Stat_t
	if err := retry(func() error { return syscall.Fstat(f.fd, &st) }); err != nil {
		return Info{}, &fs.PathError{Op: "stat", Path: f.path, Err: err}
	}
	return infoOf(&st), nil
}

// Chown calls fchown.
func (f *file) Chown(uid, gid int) error {
	if err := retry(func() error { return syscall.Fchown(f.fd, uid, gid) }); err != nil {
		return &fs.PathError{Op: "chown", Path: f.path, Err: err}
	}
	return nil
}

// TryLock calls flock with LOCK_EX and LOCK_NB. EWOULDBLOCK means another
// open file holds the lock.
func (f *file) TryLock() (bool, error) {
	switch err := retry(func() error { return syscall.Flock(f.fd, syscall.LOCK_EX|syscall.LOCK_NB) }); err {
	case nil:
		return true, nil
	case syscall.EWOULDBLOCK:
		return false, nil
	default:
		return false, &fs.PathError{Op: "flock", Path: f.path, Err: err}
	}
}

// Unlock calls flock with LOCK_UN.
func (f *file) Unlock() error {
	if err := retry(func() error { return syscall.Flock(f.fd, syscall.LOCK_UN) }); err != nil {
		return &fs.PathError{Op: "flock", Path: f.path, Err: err}
	}
	return nil
}

// Close calls close once. Linux frees the descriptor even when close
// fails with EINTR, so calling it again could close another file that has
// taken the number meanwhile.
func (f *file) Close() error {
	if f.fd < 0 {
		return &fs.PathError{Op: "close", Path: f.path, Err: fs.ErrClosed}
	}
	fd := f.fd
	f.fd = -1
	if err := syscall.Close(fd); err != nil {
		return &fs.PathError{Op: "close", Path: f.path, Err: err}
	}
	return nil
}

// retry makes call again for as long as it fails with EINTR.
func retry(call func() error) error {
	for {
		if err := call(); err != syscall.EINTR {
			return err
		}
	}
}

func open(path string, flags int, mode uint32) (int, error) {
	for {
		fd, err := syscall.Open(path, flags, mode)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func pread(fd int, p []byte, off int64) (int, error) {
	for {
		n, err := syscall.Pread(fd, p, off)
		if err != syscall.EINTR {
			return n, err
		}
	}
}

func pwrite(fd int, p []byte, off int64) (int, error) {
	for {
		n, err := syscall.Pwrite(fd, p, off)
		if err != syscall.EINTR {
			return n, err
		}
	}
}

// renameat2's arguments that Go's syscall package doesn't export. Its
// number on each architecture, sysRenameat2, is in renameat2_*.go.
const (
	atFdcwd         = -100 // AT_FDCWD: a path that isn't absolute starts from the working folder
	renameNoReplace = 1    // RENAME_NOREPLACE
)

// renameat2 renames from to to with the system call renameat2 and the
// flags given. Where the file system lacks a flag, or doesn't know it, the
// call fails with EINVAL, and the error returned then matches
// errors.ErrUnsupported, and EINVAL too. An old kernel without renameat2
// gives ENOSYS, which matches errors.ErrUnsupported already, and so does
// an architecture whose number for the call isn't known here.
func renameat2(from, to string, flags uintptr) error {
	if sysRenameat2 == 0 {
		return fmt.Errorf("%w: renameat2's number on %s isn't known", errors.ErrUnsupported, runtime.GOARCH)
	}
	p, err := syscall.BytePtrFromString(from)
	if err != nil {
		return err
	}
	q, err := syscall.BytePtrFromString(to)
	if err != nil {
		return err
	}
	cwd := atFdcwd // a variable, since the constant -100 can't become a uintptr
	for {
		_, _, e := syscall.Syscall6(sysRenameat2, uintptr(cwd), uintptr(unsafe.Pointer(p)), uintptr(cwd), uintptr(unsafe.Pointer(q)), flags, 0)
		switch e {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		case syscall.EINVAL:
			return unsupportedFlag(e)
		}
		return e
	}
}

// unsupportedFlag is the error for renameat2's EINVAL: the file system
// can't rename the way the flags ask.
func unsupportedFlag(e syscall.Errno) error {
	return fmt.Errorf("%w: the file system can't rename the way asked (%w)", errors.ErrUnsupported, e)
}

// unixMode turns perm into the mode bits open and fchmod take.
func unixMode(perm fs.FileMode) uint32 {
	m := uint32(perm.Perm())
	if perm&fs.ModeSetuid != 0 {
		m |= syscall.S_ISUID
	}
	if perm&fs.ModeSetgid != 0 {
		m |= syscall.S_ISGID
	}
	if perm&fs.ModeSticky != 0 {
		m |= syscall.S_ISVTX
	}
	return m
}

func infoOf(st *syscall.Stat_t) Info {
	return Info{
		Dev:   uint64(st.Dev),
		Ino:   uint64(st.Ino),
		Nlink: uint64(st.Nlink),
		Size:  st.Size,
		Mode:  fileMode(st.Mode),
		Uid:   int(st.Uid),
		Gid:   int(st.Gid),
	}
}

// fileMode turns stat's mode bits into an fs.FileMode, as os.Stat does.
func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & syscall.S_IFMT {
	case syscall.S_IFBLK:
		mode |= fs.ModeDevice
	case syscall.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFDIR:
		mode |= fs.ModeDir
	case syscall.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case syscall.S_IFLNK:
		mode |= fs.ModeSymlink
	case syscall.S_IFSOCK:
		mode |= fs.ModeSocket
	}
	if m&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// Umask returns the process's umask, the permissions a file goes without
// when it's made in the usual way, as Linux reports it in /proc/self/status.
// Reading it there leaves it as it is, where syscall.Umask would have to
// change it for a moment under every other goroutine. When it can't be
// read, Umask returns 0o077, which errs on the side of a file that only
// its owner can read.
//
// FS.Create sets exactly the permissions it's given, so a caller that
// wants the umask to count, as for a new database, takes it away itself.
func Umask() fs.FileMode {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0o077
	}
	for line := range bytes.Lines(b) {
		if v, ok := bytes.CutPrefix(line, []byte("Umask:")); ok {
			if m, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 8, 32); err == nil {
				return fs.FileMode(m) & fs.ModePerm
			}
		}
	}
	return 0o077
}
