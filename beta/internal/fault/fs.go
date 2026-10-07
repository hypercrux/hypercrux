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

// start begins a call on names, with d.mu held. It counts the call, and
// returns the error to give instead of making it: when the process is
// dead, when a rule fails the call, and when a rule cuts the power, which
// it does first.
func (p *proc) start(op Op, paths ...string) error {
	d := p.d
	if p.boot != d.boot {
		return wrap(op, ErrCut, paths...)
	}
	switch v, err := d.enter(op, paths...); v {
	case failIt:
		return wrap(op, err, paths...)
	case cutIt:
		d.cut()
		return wrap(op, ErrCut, paths...)
	}
	return nil
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

func (p *proc) Create(name string, perm fs.FileMode) (fsys.File, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	if err := p.start(Create, name); err != nil {
		return nil, err
	}
	if d.names[name] != nil {
		return nil, wrap(Create, syscall.EEXIST, name)
	}
	if err := d.room(name); err != nil {
		return nil, wrap(Create, err, name)
	}
	n := d.newInode(false, perm)
	d.names[name] = n
	return &file{d: d, boot: p.boot, path: name, n: n}, nil
}

func (p *proc) Rename(from, to string) error { return p.rename(Rename, from, to) }

func (p *proc) RenameNoReplace(from, to string) error { return p.rename(RenameNoReplace, from, to) }

// rename moves a file's name. It moves files only: a folder gives EISDIR,
// since the engine never renames one.
func (p *proc) rename(op Op, from, to string) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	from, to = norm(from), norm(to)
	if err := p.start(op, from, to); err != nil {
		return err
	}
	src, err := d.find(from)
	if err != nil {
		return wrap(op, err, from, to)
	}
	dst := d.names[to]
	switch {
	case src.dir:
		return wrap(op, syscall.EISDIR, from, to)
	case op == RenameNoReplace && dst != nil:
		return wrap(op, syscall.EEXIST, from, to)
	case dst == src:
		return nil
	case dst != nil && dst.dir:
		return wrap(op, syscall.EISDIR, from, to)
	}
	if err := d.room(to); err != nil {
		return wrap(op, err, from, to)
	}
	if dst != nil {
		dst.nlink = 0
	}
	d.names[to] = src
	delete(d.names, from)
	return nil
}

func (p *proc) Remove(name string) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	name = norm(name)
	if err := p.start(Remove, name); err != nil {
		return err
	}
	n, err := d.find(name)
	if err == nil && n.dir {
		err = syscall.EISDIR
	}
	if err != nil {
		return wrap(Remove, err, name)
	}
	n.nlink = 0
	delete(d.names, name)
	return nil
}

// SyncDir checks the folder is there. Names always last here, so there's
// nothing more for it to do until T2.
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
// fchown does.
func (f *file) Chown(uid, gid int) error {
	d := f.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v, err := f.start(Chown); v {
	case refuse, failIt:
		return err
	case cutIt:
		d.cut()
		return err
	}
	if uid != -1 {
		f.n.uid = uid
	}
	if gid != -1 {
		f.n.gid = gid
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
