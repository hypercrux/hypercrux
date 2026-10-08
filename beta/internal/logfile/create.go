// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// FORMAT.md's "Creating a database": a new database starts as a file
// holding only its header, written beside the database under a .new- name,
// locked before anything is written to it, synced, and renamed into place.
// Nobody else can lock it until the folder's sync makes the rename last, so
// nobody can commit to a file a power cut could still take away.

// newPerm is a new database's permissions, less the umask. 0.x's SQLite
// makes its databases the same way.
const newPerm = 0o644

// errAppeared means a creator's rename found a file at the database's
// path, which another creator put there meanwhile. Open opens that one.
var errAppeared = errors.New("hypercrux: a database appeared at the path during its creation")

// create makes a new database at the path, where nothing was a moment ago.
// The rename into place fails if a file has appeared there meanwhile, and
// then create removes its own file and returns errAppeared. On a file
// system that can't rename that way, creating a database fails with an
// error that matches errors.ErrUnsupported, since a plain rename could
// replace a database that appeared meanwhile. On success, l holds the new
// file, without its lock, and its header.
func (l *Log) create() error {
	n, err := l.newFile(newPerm&^fsys.Umask(), -1, -1)
	if err != nil {
		return err
	}
	if err := l.fsys.RenameNoReplace(n.name, l.path); err != nil {
		l.fsys.Remove(n.name)
		n.f.Close()
		if errors.Is(err, fs.ErrExist) {
			return errAppeared
		}
		return fmt.Errorf("hypercrux: creating %s: %w", l.path, err)
	}
	if err := l.fsys.SyncDir(l.dir); err != nil {
		// The rename may not last a power cut, so nobody may commit to the
		// new database. It holds only a header, and its lock has been held
		// throughout, so nobody has: its name goes again.
		if mine, e := n.f.Stat(); e == nil {
			if there, e := l.fsys.Stat(l.path); e == nil && there.Same(mine) {
				l.fsys.Remove(l.path)
			}
		}
		n.f.Close()
		return fmt.Errorf("hypercrux: creating %s: %w", l.path, err)
	}
	// The rename lasts now, so others may lock the new database.
	if err := n.f.Unlock(); err != nil {
		n.f.Close()
		return err
	}
	l.use(n)
	return nil
}

// fill deals with the empty file l.f, holding its lock, once it has
// checked that it's still the one at the path. When it's still empty, it
// becomes a database (adopt). When something has written into it in place
// since it was read, which HyperCrux never does, it's read again as it is.
// On an error, l holds no flock, unless stuck is set.
func (l *Log) fill(info fsys.Info) error {
	if info.Size == 0 {
		err := l.adopt(info)
		if err != nil && l.stuck == nil {
			l.f.Unlock()
		}
		return err
	}
	l.t.Reset()
	if _, err := l.load(); err != nil {
		l.f.Unlock()
		l.f.Close()
		l.f = nil
		return err
	}
	return nil
}

// adopt makes a database of the empty file l.f, whose lock is held, as
// FORMAT.md says for an empty file: a new file is made as for a new
// database, with the empty file's permissions and, where it's allowed, its
// owner, and renamed over the empty file, and then the folder is synced.
// The new file takes the empty one's place in l, locked, and the empty one
// is closed, which lets go of its lock. Anyone who had the empty file open
// finds the new one at the path, as after a compaction.
//
// If the folder's sync fails after the rename, nobody can tell which file
// a power cut would leave at the path. So l keeps the new file's lock until
// Close, and refuses writes: stuck is set. On any other error, l still
// holds the empty file, locked.
func (l *Log) adopt(info fsys.Info) error {
	n, err := l.newFile(info.Mode.Perm(), info.Uid, info.Gid)
	if err != nil {
		return err
	}
	if err := l.fsys.Rename(n.name, l.path); err != nil {
		l.fsys.Remove(n.name)
		n.f.Close()
		return err
	}
	empty := l.f
	l.use(n)
	defer empty.Close()
	if err := l.fsys.SyncDir(l.dir); err != nil {
		l.stuck = fmt.Errorf("%w: %s: the folder's sync failed once an empty file had become a database: %w", errs.ErrStuck, l.path, err)
		return l.stuck
	}
	return nil
}

// fresh is a new database file under a .new- name: locked, holding a new
// database's header, and synced.
type fresh struct {
	f    fsys.File
	info fsys.Info // what fstat said about it, which a rename keeps
	name string
	hdr  format.Header
	head []byte
}

// newFile makes a new database file beside the database, named after it
// with .new- and random letters added. The file gets the permissions perm,
// and the owner uid and group gid unless uid is -1, where that's allowed.
// It's locked before anything is written to it, given a new database's
// header with a new random ID, or Options.ID when that's set, and synced.
//
// A writer that removes leftover .new- files removes one it can lock. It
// may take this one in the moment between its creation and its lock: then
// the lock fails, or the name has gone once the lock is held, and newFile
// tries another name.
func (l *Log) newFile(perm fs.FileMode, uid, gid int) (*fresh, error) {
	hdr := format.Header{ID: l.id, Gen: 1}
	if hdr.ID == ([16]byte{}) {
		rand.Read(hdr.ID[:])
	}
	head, err := format.AppendHeader(nil, hdr)
	if err != nil {
		return nil, err
	}
	for range maxTries {
		name := l.path + ".new-" + letters(12)
		f, err := l.fsys.Create(name, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ok, err := f.TryLock()
		if err != nil {
			f.Close()
			l.fsys.Remove(name)
			return nil, err
		}
		if !ok {
			f.Close() // the writer that holds it removes it
			continue
		}
		mine, err := f.Stat()
		if err != nil {
			f.Close()
			l.fsys.Remove(name)
			return nil, err
		}
		if there, err := l.fsys.Stat(name); err != nil || !there.Same(mine) {
			f.Close() // removed between its creation and its lock
			continue
		}
		if uid >= 0 && (mine.Uid != uid || mine.Gid != gid) {
			f.Chown(uid, gid) // a refusal leaves the file this process's, as compaction does
		}
		_, err = f.WriteAt(head, 0)
		if err == nil {
			err = f.Sync()
		}
		if err != nil {
			l.fsys.Remove(name)
			f.Close()
			return nil, err
		}
		return &fresh{f: f, info: mine, name: name, hdr: hdr, head: head}, nil
	}
	return nil, fmt.Errorf("hypercrux: %s: no free name beside it for a new database file", l.path)
}

// use makes n the database file in l: a header, and a log with nothing in
// it yet.
func (l *Log) use(n *fresh) {
	l.f = n.f
	l.file = n.info
	l.empty = false
	l.hdr = n.hdr
	copy(l.head[:], n.head)
	l.seq = 0
	l.end = format.HeaderSize
	l.retry = 0
}

// removeLeftovers removes the .new- files that creators left beside the
// database when they crashed: the ones it can lock. One that's locked is
// a creation under way, and stays. It goes by the name's shape, the
// database's name, .new- and letters only, and removes a regular file
// only, and only while the name still leads to the file it locked. Errors
// are ignored, since a leftover costs a few bytes, and a later writer
// tries again.
func (l *Log) removeLeftovers() {
	names, err := l.fsys.List(l.dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(l.path) + ".new-"
	for _, name := range names {
		if rest, ok := strings.CutPrefix(name, prefix); !ok || !isLetters(rest) {
			continue
		}
		path := filepath.Join(l.dir, name)
		f, err := l.fsys.Open(path)
		if err != nil {
			continue
		}
		if ok, err := f.TryLock(); err == nil && ok {
			mine, err := f.Stat()
			if err == nil && mine.Mode.IsRegular() {
				if there, err := l.fsys.Stat(path); err == nil && there.Same(mine) {
					l.fsys.Remove(path)
				}
			}
		}
		f.Close()
	}
}

// letters returns n random lower-case letters.
func letters(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = 'a' + b[i]%26
	}
	return string(b)
}

// isLetters reports whether s is one or more ASCII letters.
func isLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return s != ""
}
