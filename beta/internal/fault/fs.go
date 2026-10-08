// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"io"
	"io/fs"
	"path"
	"slices"
	"syscall"

	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

var (
	_ fsys.FS   = (*proc)(nil)
	_ fsys.File = (*file)(nil)
)

// proc is a Disk as one process sees it, from FS: it lives until the next
// cut.
type proc struct {
	d    *Disk
	boot int // the cut it came after
}

// norm cleans a path given to a call. "" stays "", which every call
// refuses, as Linux does.
func norm(p string) string {
	if p == "" {
		return ""
	}
	return clean(p)
}

// begin begins a call on names, with d.mu held, and says what happens to
// it, as file.start does for a call on a file. It counts the call. For
// anything but goAhead it returns the error the call gives, wrapped:
// ErrCut when the process is dead or a rule cuts the power in the call,
// or a rule's error.
func (p *proc) begin(op Op, paths ...string) (verdict, error) {
	d := p.d
	if p.boot != d.boot {
		return refuse, wrap(op, ErrCut, paths...)
	}
	v, err := d.enter(op, paths...)
	switch v {
	case failIt:
		return v, wrap(op, err, paths...)
	case cutIt:
		return v, wrap(op, ErrCut, paths...)
	}
	return v, nil
}

// start begins a call on names that changes none, or a SyncDir, with d.mu
// held. It counts the call, and returns the error to give instead of
// making it: when the process is dead, when a rule fails the call, and
// when a rule cuts the power, which it does first.
func (p *proc) start(op Op, paths ...string) error {
	v, err := p.begin(op, paths...)
	if v == cutIt {
		p.d.cut()
	}
	return err
}

func (p *proc) Open(name string) (fsys.File, error) {
	p.d.mu.Lock()
	defer p.d.mu.Unlock()
	name = norm(name)
	if err := p.start(Open, name); err != nil {
		return nil, err
	}
	n, err := p.d.find(name)
	if err == nil && n.dir {
		err = syscall.EISDIR
	}
	if err != nil {
		return nil, wrap(Open, err, name)
	}
	return &file{d: p.d, boot: p.boot, path: name, n: n}, nil
}

// Create, Rename, RenameNoReplace and Remove each make their change and put
// it in the journal, where it stays until a SyncDir puts it on the drive or
// a cut settles it. When a rule cuts the power in one of them, the cut
// comes once the change is made, so the change is pending at the cut.

func (p *proc) Create(name string, perm fs.FileMode) (fsys.File, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	v, err := p.begin(Create, name)
	if v == refuse || v == failIt || d.early(v) {
		return nil, err
	}
	n, bad := d.create(name, perm)
	switch {
	case v == cutIt:
		d.cut()
		return nil, err
	case bad != nil:
		return nil, wrap(Create, bad, name)
	}
	return &file{d: d, boot: p.boot, path: name, n: n}, nil
}

// create makes a file at name, with d.mu held, or returns the error Linux
// gives.
func (d *Disk) create(name string, perm fs.FileMode) (*inode, error) {
	if d.names[name] != nil {
		return nil, syscall.EEXIST
	}
	if err := d.room(name); err != nil {
		return nil, err
	}
	n := d.newInode(false, perm)
	d.names[name] = n
	d.note(change{dirs: []string{path.Dir(name)}, names: []entry{{name, n}}})
	return n, nil
}

func (p *proc) Rename(from, to string) error { return p.rename(Rename, from, to) }

func (p *proc) RenameNoReplace(from, to string) error { return p.rename(RenameNoReplace, from, to) }

func (p *proc) rename(op Op, from, to string) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	from, to = norm(from), norm(to)
	v, err := p.begin(op, from, to)
	if v == refuse || v == failIt || d.early(v) {
		return err
	}
	bad := d.move(op, from, to)
	switch {
	case v == cutIt:
		d.cut()
		return err
	case bad != nil:
		return wrap(op, bad, from, to)
	}
	return nil
}

// move moves a file's name from from to to, with d.mu held, or returns the
// error Linux gives. It moves files only: a folder gives EISDIR, since the
// engine never renames one. A file already at to loses its name.
func (d *Disk) move(op Op, from, to string) error {
	src, err := d.find(from)
	if err != nil {
		return err
	}
	dst := d.names[to]
	switch {
	case src.dir:
		return syscall.EISDIR
	case op == RenameNoReplace && dst != nil:
		return syscall.EEXIST
	case dst == src:
		return nil
	case dst != nil && dst.dir:
		return syscall.EISDIR
	}
	if err := d.room(to); err != nil {
		return err
	}
	if dst != nil {
		dst.nlink = 0
	}
	d.names[to] = src
	delete(d.names, from)
	dirs := []string{path.Dir(from)}
	if dir := path.Dir(to); dir != dirs[0] {
		dirs = append(dirs, dir)
	}
	d.note(change{dirs: dirs, names: []entry{{from, nil}, {to, src}}})
	return nil
}

func (p *proc) Remove(name string) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	v, err := p.begin(Remove, name)
	if v == refuse || v == failIt || d.early(v) {
		return err
	}
	bad := d.unlink(name)
	switch {
	case v == cutIt:
		d.cut()
		return err
	case bad != nil:
		return wrap(Remove, bad, name)
	}
	return nil
}

// unlink removes the name name, with d.mu held, or returns the error Linux
// gives.
func (d *Disk) unlink(name string) error {
	n, err := d.find(name)
	if err == nil && n.dir {
		err = syscall.EISDIR
	}
	if err != nil {
		return err
	}
	n.nlink = 0
	delete(d.names, name)
	d.note(change{dirs: []string{path.Dir(name)}, names: []entry{{name, nil}}})
	return nil
}

// SyncDir puts every change in the folder on the drive, with the changes
// they lean on, as Disk's comment says. A cut in it comes first, so it
// puts nothing there.
func (p *proc) SyncDir(dir string) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	dir = norm(dir)
	if err := p.start(SyncDir, dir); err != nil {
		return err
	}
	n, err := d.find(dir)
	if err == nil && !n.dir {
		err = syscall.ENOTDIR
	}
	if err != nil {
		return wrap(SyncDir, err, dir)
	}
	d.syncDir(dir)
	return nil
}

func (p *proc) Stat(name string) (fsys.Info, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	if err := p.start(Stat, name); err != nil {
		return fsys.Info{}, err
	}
	n, err := d.find(name)
	if err != nil {
		return fsys.Info{}, wrap(Stat, err, name)
	}
	return n.info(), nil
}

// RealPath returns the path cleaned and made absolute, since a Disk has no
// symbolic links.
func (p *proc) RealPath(name string) (string, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	if err := p.start(RealPath, name); err != nil {
		return "", err
	}
	if _, err := d.find(name); err != nil {
		return "", wrap(RealPath, err, name)
	}
	return name, nil
}

func (p *proc) List(dir string) ([]string, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	dir = norm(dir)
	if err := p.start(List, dir); err != nil {
		return nil, err
	}
	n, err := d.find(dir)
	if err == nil && !n.dir {
		err = syscall.ENOTDIR
	}
	if err != nil {
		return nil, wrap(List, err, dir)
	}
	names := []string{}
	for name := range d.names {
		if name != dir && path.Dir(name) == dir {
			names = append(names, path.Base(name))
		}
	}
	slices.Sort(names)
	return names, nil
}

// file is a file a process opened or created.
type file struct {
	d      *Disk
	boot   int    // the cut its process came after
	path   string // the path it was opened or created with
	n      *inode
	closed bool
}

// start begins a call on the file, with d.mu held, and says what happens
// to it. For anything but goAhead it returns the error the call gives,
// wrapped: ErrCut when its process is dead or the power is cut in it,
// fs.ErrClosed when the file is closed, or a rule's error.
func (f *file) start(op Op) (verdict, error) {
	switch {
	case f.boot != f.d.boot:
		return refuse, wrap(op, ErrCut, f.path)
	case f.closed:
		return refuse, wrap(op, fs.ErrClosed, f.path)
	}
	v, err := f.d.enter(op, f.path)
	switch v {
	case failIt:
		return v, wrap(op, err, f.path)
	case cutIt:
		return v, wrap(op, ErrCut, f.path)
	}
	return v, nil
}

// prefix picks how many of l bytes a failed read or write gets through:
// from none to all but one.
func (f *file) prefix(l int) int {
	if l == 0 {
		return 0
	}
	return f.d.rng.IntN(l)
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	v, err := f.start(ReadAt)
	switch {
	case v == refuse:
		return 0, err
	case v == cutIt:
		d.cut()
		return 0, err
	case off < 0:
		if v == failIt {
			return 0, err
		}
		return 0, wrap(ReadAt, syscall.EINVAL, f.path)
	case v == failIt:
		return f.n.read(p[:f.prefix(len(p))], off), err
	}
	n := f.n.read(p, off)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	v, err := f.start(WriteAt)
	if v == refuse {
		return 0, err
	}
	var bad error
	switch {
	case off < 0:
		bad = syscall.EINVAL
	case off > MaxSize-int64(len(p)):
		bad = syscall.EFBIG
	}
	switch {
	case v == cutIt:
		if bad == nil {
			f.n.write(p, off)
		}
		d.cut()
		return 0, err
	case v == failIt:
		if bad != nil {
			return 0, err
		}
		k := f.prefix(len(p))
		f.n.write(p[:k], off)
		return k, err
	case bad != nil:
		return 0, wrap(WriteAt, bad, f.path)
	}
	f.n.write(p, off)
	return len(p), nil
}

func (f *file) Sync() error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v, err := f.start(Sync); v {
	case refuse:
		return err
	case cutIt:
		d.cut()
		return err
	case failIt:
		f.n.failSync(d)
		return err
	}
	f.n.sync()
	if plant == "fault/sync-keeps-name" {
		for _, dir := range d.dirsOf(f.n) {
			d.syncDir(dir)
		}
	}
	return nil
}

func (f *file) Truncate(size int64) error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	v, err := f.start(Truncate)
	var bad error
	switch {
	case size < 0:
		bad = syscall.EINVAL
	case size > MaxSize:
		bad = syscall.EFBIG
	}
	switch {
	case v == refuse || v == failIt:
		return err
	case v == cutIt:
		if bad == nil {
			f.n.truncate(size)
		}
		d.cut()
		return err
	case bad != nil:
		return wrap(Truncate, bad, f.path)
	}
	f.n.truncate(size)
	return nil
}

func (f *file) Stat() (fsys.Info, error) {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v, err := f.start(Fstat); v {
	case refuse, failIt:
		return fsys.Info{}, err
	case cutIt:
		d.cut()
		return fsys.Info{}, err
	}
	return f.n.info(), nil
}

// Chown sets the owner and the group, leaving either as it is for -1, as
// fchown does. The change goes in the journal as a change in the folder
// that holds the file's name, or in none when the file has no name. A cut
// in it comes once the change is made.
func (f *file) Chown(uid, gid int) error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	v, err := f.start(Chown)
	if v == refuse || v == failIt || d.early(v) {
		return err
	}
	n := f.n
	if uid != -1 {
		n.uid = uid
	}
	if gid != -1 {
		n.gid = gid
	}
	if plant == "fault/chown-lasts" {
		n.suid, n.sgid = n.uid, n.gid
	} else {
		d.note(change{dirs: d.dirsOf(n), owner: n, uid: n.uid, gid: n.gid})
	}
	if v == cutIt {
		d.cut()
		return err
	}
	return nil
}

func (f *file) TryLock() (bool, error) {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v, err := f.start(TryLock); v {
	case refuse, failIt:
		return false, err
	case cutIt:
		d.cut()
		return false, err
	}
	if h := f.n.holder; h == nil || h == f || plant == "fault/second-open-locks" {
		f.n.holder = f
		return true, nil
	}
	return false, nil
}

func (f *file) Unlock() error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v, err := f.start(Unlock); v {
	case refuse, failIt:
		return err
	case cutIt:
		d.cut()
		return err
	}
	if f.n.holder == f {
		f.n.holder = nil
	}
	return nil
}

func (f *file) Close() error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	v, err := f.start(Close)
	switch v {
	case refuse:
		return err
	case cutIt:
		d.cut()
		return err
	}
	f.closed = true
	if f.n.holder == f {
		f.n.holder = nil
	}
	return err
}
