// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys

import "io/fs"

// File is an open file: the calls on its data, and its lock. The database
// file, NAME.compact and a .new- file are all used through it. T1's fault
// layer wraps it, losing or tearing what was written since the last Sync
// at a simulated power cut, and failing any call at its nth use.
//
// The real one is opened with O_CLOEXEC, so no child process can end up
// holding the lock.
type File interface {
	// ReadAt reads len(p) bytes from offset off, as io.ReaderAt does: it
	// returns fewer only with an error, which is io.EOF at the end of the
	// file.
	ReadAt(p []byte, off int64) (n int, err error)

	// WriteAt writes p at offset off, as io.WriterAt does. An append is a
	// WriteAt at the end of the log, which the writer keeps track of, so
	// the file is never opened with O_APPEND, and a fault layer sees every
	// write the same way.
	WriteAt(p []byte, off int64) (n int, err error)

	// Sync returns once everything written to the file, and its size, is
	// on the drive (fdatasync).
	Sync() error

	// Truncate cuts the file to size bytes, or grows it with zeros to
	// that. A cut lasts once it's synced.
	Truncate(size int64) error

	// Stat describes the open file (fstat).
	Stat() (Info, error)

	// Chown gives the file an owner and a group (fchown). Compaction gives
	// NAME.compact the database's, where it's allowed to, and ignores a
	// refusal.
	Chown(uid, gid int) error

	// TryLock takes an exclusive flock on the file without waiting, and
	// returns false, with no error, when another open file holds it.
	//
	// A flock belongs to an open file, so it can't keep apart goroutines
	// that share one: the log keeps a mutex in front of it. The log also
	// does the waiting, up to 10 seconds or for as long as a compaction
	// runs, by calling TryLock again after a pause (F2).
	TryLock() (bool, error)

	// Unlock lets go of the flock.
	Unlock() error

	// Close closes the file, which lets go of its flock too.
	Close() error
}

// FS is the calls on names in the file system. T2's fault layer wraps it,
// keeping or losing each creation, rename and removal at a simulated power
// cut, by whether a sync of its folder followed it.
//
// Errors wrap the system's error with the path, so errors.Is matches them
// against fs.ErrNotExist, fs.ErrExist or a syscall.Errno.
type FS interface {
	// Open opens an existing file for reading and writing.
	Open(path string) (File, error)

	// Create makes a file at path and opens it for reading and writing. It
	// fails when a file is there already (O_EXCL). The file gets exactly
	// the permissions perm, whatever the umask. A new database starts as
	// a .new- file made this way, and a compaction's file as NAME.compact.
	Create(path string, perm fs.FileMode) (File, error)

	// Rename moves the file at from to the path to, replacing any file
	// there (rename). The switch to a compacted file is one, and so is the
	// rename that makes a database of an empty file.
	Rename(from, to string) error

	// RenameNoReplace moves the file at from to the path to, and fails
	// with an error matching fs.ErrExist when a file is at to already
	// (renameat2 with RENAME_NOREPLACE). Creating a database takes this
	// rename. Where the file system can't rename that way, it fails with
	// an error matching errors.ErrUnsupported, and so creating a database
	// fails there too, as FORMAT.md says it must.
	RenameNoReplace(from, to string) error

	// Remove removes the name path (unlink).
	Remove(path string) error

	// SyncDir returns once the names in the folder dir are on the drive as
	// the creations, renames and removals in it left them (fsync on the
	// folder).
	SyncDir(dir string) error

	// Stat describes the file at path, following symbolic links (stat).
	// Before each read, a process stats the database's path to see whether
	// another file has taken its place, and how long it is.
	Stat(path string) (Info, error)

	// RealPath returns the absolute path of the existing file or folder at
	// path, with every symbolic link resolved. A database is opened by its
	// real path; a new one by its folder's real path and its name.
	RealPath(path string) (string, error)

	// List returns the names in the folder dir, in byte order. The writer
	// looks through them for leftover .new- files.
	List(dir string) ([]string, error)
}

// Info is what Stat says about a file.
type Info struct {
	Dev   uint64      // the device it's on
	Ino   uint64      // its inode number there
	Nlink uint64      // how many names it has
	Size  int64       // its length in bytes
	Mode  fs.FileMode // its type and permissions, as io/fs gives them
	Uid   int         // its owner
	Gid   int         // its group
}

// Same reports whether i and j describe the same file, by device and inode
// number. That's how a process finds that a compaction, or a backup moved
// into place, has put another file at the database's path, and how a
// writer checks that the file it locked is still the one there.
func (i Info) Same(j Info) bool { return i.Dev == j.Dev && i.Ino == j.Ino }
