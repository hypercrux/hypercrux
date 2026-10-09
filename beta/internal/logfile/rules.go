// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// The file rules (F7): BETA.md's "Rules for the file".
//
// The real path. Open works from the path made real: every symbolic link
// resolved, and a relative path made absolute from the working folder's own
// real path, by fsys.RealPath (realPath). A path that names nothing yet,
// for a new database, becomes its folder's real path and its name. From then
// on the Log works from that path alone, and from the folder it's in: the
// stat before each read (Follow), Lock's look at the path, a reload, the
// .new- files a creation makes and a writer removes, and NAME.compact. So a
// relative path names the file it named when Open ran, whatever the working
// folder is later, and a compaction renames its file over the database
// itself, leaving a symbolic link to it as it was.
//
// What's opened. Only a regular file with one name is a database (ruled):
//
//   - A file with two names or more, made by hard links, is refused, since
//     a compaction replaces the file at one path only, and the other names
//     would keep the old file, which a process that opened it by one of them
//     would go on reading and writing. 0.x opens such a file, so the refusal
//     is the Beta's own, and its error wraps errs.ErrInvalid (P3.md).
//   - Anything else at the path, a folder, a FIFO, a device or a socket, is
//     refused before anything opens it, from a stat of the path. Opening a
//     FIFO for reading waits for a writer to come, and opening a device can
//     do whatever the device does. Linux lets the log's open, for reading
//     and writing, through a FIFO at once, but that still joins it to a
//     process waiting at the FIFO's other end. 0.x fails on a folder and on
//     a FIFO with a plain error, so the error wraps errs.ErrNotDatabase,
//     whose kind is the same.
//
// The file opened is checked again by fstat, in case another took the path
// between the stat and the open (openPath). The same goes for the leftovers
// beside the database: a NAME.compact or a .new- file is opened only when a
// stat finds a regular file there (openLeftover).
//
// Later. Every file the log opens at the path goes through openPath: at Open,
// also when it starts again because another file took the path, at Lock when
// it moves to another file (reopen), and at Reload. So a FIFO, or a file with
// two names, that takes the path later is refused there, before it's opened
// and before the Target is reset. That leaves the Log as a reload whose open
// failed leaves it: holding the file it had, or after Lock let go of that,
// none. Every read and every commit gives the refusal then, until a file that
// keeps the rules is at the path again. openPath also refuses a path whose
// last element has become a symbolic link since Open (stillReal): a
// compaction's rename would replace the link, and leave the file it names
// holding the old data. The database is opened again then, by its new real
// path. A folder on the way that has become a link does no harm, since a
// rename through it renames within the folder it leads to.
//
// Writers. A hard link made while the database is open gives the file that's
// open a second name, which no reload sees. So every writer checks the number
// of names once it holds the lock and has found the file it locked still at
// the path (lockFile), and a compaction checks again just before its rename,
// with the stat of the path it makes there anyway: a commit or a compaction
// never goes ahead in a file with two names, and reads go on. The two places
// that rename a file over the path, a compaction's switch and making a
// database of an empty file, also check just before it that the path's last
// element isn't a symbolic link, which is the one way a link to the same file
// can take the path unseen, since every stat follows it to that file.

// realPath returns the path the database at path is opened by: the real path
// of the file there, or, when the path names nothing yet, its folder's real
// path and its name.
func realPath(files fsys.FS, path string) (string, error) {
	if plant == "logfile/path-as-given" {
		return path, nil
	}
	real, err := files.RealPath(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("hypercrux: %s: finding its real path: %w", path, err)
	}
	dir, err := files.RealPath(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("hypercrux: %s: finding its folder's real path: %w", path, err)
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

// Path returns the database's real path, the one the Log works from: the path
// Open was given, with every symbolic link resolved, made absolute.
func (l *Log) Path() string { return l.path }

// openPath opens the file at the database's path by the rules. It stats the
// path first, and opens nothing that isn't a regular file with one name, nor
// a path whose last element has become a symbolic link (stillReal). Then it
// checks the file it opened by fstat, since another file can take the path
// between the stat and the open. When nothing is at the path, the error
// matches fs.ErrNotExist, so Open can make a database there.
func (l *Log) openPath() (fsys.File, error) {
	if plant != "logfile/fifo-opened-first" {
		info, err := l.fsys.Stat(l.path)
		if err != nil {
			return nil, err
		}
		if err := l.ruled(info); err != nil {
			return nil, err
		}
	}
	if err := l.stillReal(); err != nil {
		return nil, err
	}
	f, err := l.fsys.Open(l.path)
	if err != nil {
		return nil, err
	}
	mine, err := f.Stat()
	if err == nil {
		err = l.ruled(mine)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ruled checks what a stat of the database's path, or an fstat of the file
// opened there, says about it, by the rules: a regular file with one name. A
// file with none, which has gone from its name since it was opened, is the
// inode check's to find.
func (l *Log) ruled(info fsys.Info) error {
	if !info.Mode.IsRegular() && plant != "logfile/any-kind-of-file" {
		return fmt.Errorf("%w: %s is %s, and a database is a regular file", errs.ErrNotDatabase, l.path, kindOf(info.Mode))
	}
	if info.Nlink > 1 && plant != "logfile/hard-link-let-through" {
		return l.names(info.Nlink)
	}
	return nil
}

// names is the refusal of the database's file when it has n names.
func (l *Log) names(n uint64) error {
	return fmt.Errorf("%w: %s has %d names, made by hard links, and a database has one: a compaction replaces the file at one path only, and the other names would keep the old file", errs.ErrInvalid, l.path, n)
}

// kindOf names the kind of a file that isn't a regular one, by its mode.
func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a folder"
	case m&fs.ModeNamedPipe != 0:
		return "a FIFO"
	case m&fs.ModeCharDevice != 0:
		return "a character device"
	case m&fs.ModeDevice != 0:
		return "a block device"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	}
	return "a file of a kind that isn't regular"
}

// stillReal checks that the last element of the database's path hasn't become
// a symbolic link since Open made the path real, since a rename over the path
// would replace the link. When a folder on the way has become a link, the
// path's real path is that folder's real path with the file's name, which
// does no harm. When nothing is at the path, the error matches
// fs.ErrNotExist.
func (l *Log) stillReal() error {
	if plant == "logfile/links-unchecked" {
		return nil
	}
	real, err := l.fsys.RealPath(l.path)
	if err != nil || real == l.path {
		return err
	}
	dir, err := l.fsys.RealPath(l.dir)
	if err != nil {
		return err
	}
	if real == filepath.Join(dir, filepath.Base(l.path)) {
		return nil
	}
	return fmt.Errorf("%w: %s has become a symbolic link to %s since the database was opened, and a rename over it would replace the link: the database has to be opened again, by its new real path", errs.ErrInvalid, l.path, real)
}

// appeared is what create returns when its rename into place finds a name at
// the path already. Most often another creator's database has taken it, and
// Open opens that one (errAppeared). When the path still leads to no file, the
// name is a symbolic link to nothing, through which no database can be made,
// since the rename into place would replace the link.
func (l *Log) appeared() error {
	if _, err := l.fsys.Stat(l.path); errors.Is(err, fs.ErrNotExist) && plant != "logfile/dangling-link-retried" {
		return fmt.Errorf("%w: %s is a symbolic link that leads to no file, and a new database can't be made through one, since its rename into place would replace the link", errs.ErrInvalid, l.path)
	}
	return errAppeared
}

// openLeftover opens the file at path, beside the database, when a stat finds
// a regular file there, for a writer removing a leftover NAME.compact or
// .new- file, or a waiter looking for a compaction under way. Otherwise it
// returns nil, and opens nothing.
func (l *Log) openLeftover(path string) fsys.File {
	if info, err := l.fsys.Stat(path); err != nil || !info.Mode.IsRegular() {
		return nil
	}
	f, err := l.fsys.Open(path)
	if err != nil {
		return nil
	}
	return f
}
