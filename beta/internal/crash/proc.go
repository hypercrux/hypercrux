// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"errors"
	"io"
	"io/fs"
	"sync"

	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

var (
	_ fsys.FS   = (*proc)(nil)
	_ fsys.File = (*file)(nil)
)

// proc is one process's file calls: the disk's own, from one
// fault.Disk.FS, with the files the process has open kept track of, so the
// driver can end the process as Linux ends one, by closing them. It counts
// the calls made through it, and lets a copy read a chunk just before each
// one.
type proc struct {
	sys fsys.FS

	mu    sync.Mutex
	files []*file // every file opened or created through it, in order
	calls int
	copy  *copier // a copy taken alongside the process's calls, or nil
}

func newProc(sys fsys.FS) *proc { return &proc{sys: sys} }

// enter counts a call, and lets the copy read just before it.
func (p *proc) enter() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.copy != nil {
		p.copy.before(p.calls)
	}
}

// opened keeps track of a file the process has opened or created.
func (p *proc) opened(f fsys.File, err error) (fsys.File, error) {
	if err != nil {
		return nil, err
	}
	pf := &file{p: p, f: f}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files = append(p.files, pf)
	return pf, nil
}

// kill ends the process as Linux does: every file it has left open is
// closed, which lets go of its flocks, and the page cache keeps what it
// wrote. A call through the process afterwards would still reach the disk,
// so the driver ends a process only once its code has returned.
func (p *proc) kill() {
	p.mu.Lock()
	var open []*file
	for _, f := range p.files {
		if !f.closed {
			open = append(open, f)
			f.closed = true
		}
	}
	p.files = nil
	p.mu.Unlock()
	for _, f := range open {
		f.f.Close()
	}
}

func (p *proc) Open(name string) (fsys.File, error) {
	p.enter()
	return p.opened(p.sys.Open(name))
}

func (p *proc) Create(name string, perm fs.FileMode) (fsys.File, error) {
	p.enter()
	return p.opened(p.sys.Create(name, perm))
}

func (p *proc) Rename(from, to string) error {
	p.enter()
	return p.sys.Rename(from, to)
}

func (p *proc) RenameNoReplace(from, to string) error {
	p.enter()
	return p.sys.RenameNoReplace(from, to)
}

func (p *proc) Remove(name string) error {
	p.enter()
	return p.sys.Remove(name)
}

func (p *proc) SyncDir(dir string) error {
	p.enter()
	return p.sys.SyncDir(dir)
}

func (p *proc) Stat(name string) (fsys.Info, error) {
	p.enter()
	return p.sys.Stat(name)
}

func (p *proc) RealPath(name string) (string, error) {
	p.enter()
	return p.sys.RealPath(name)
}

func (p *proc) List(dir string) ([]string, error) {
	p.enter()
	return p.sys.List(dir)
}

// file is a file a proc opened or created.
type file struct {
	p      *proc
	f      fsys.File
	closed bool // Close has been called, so the process's end leaves it be
}

func (f *file) ReadAt(b []byte, off int64) (int, error) {
	f.p.enter()
	return f.f.ReadAt(b, off)
}

func (f *file) WriteAt(b []byte, off int64) (int, error) {
	f.p.enter()
	return f.f.WriteAt(b, off)
}

func (f *file) Sync() error {
	f.p.enter()
	return f.f.Sync()
}

func (f *file) Truncate(size int64) error {
	f.p.enter()
	return f.f.Truncate(size)
}

func (f *file) Stat() (fsys.Info, error) {
	f.p.enter()
	return f.f.Stat()
}

func (f *file) Chown(uid, gid int) error {
	f.p.enter()
	return f.f.Chown(uid, gid)
}

func (f *file) TryLock() (bool, error) {
	f.p.enter()
	return f.f.TryLock()
}

func (f *file) Unlock() error {
	f.p.enter()
	return f.f.Unlock()
}

// Close closes the file. Linux closes it whatever close returns, so it
// counts as closed either way.
func (f *file) Close() error {
	f.p.enter()
	f.p.mu.Lock()
	f.closed = true
	f.p.mu.Unlock()
	return f.f.Close()
}

// copier takes a copy of the file at a path as cp does: it opens the file
// once, through a process of its own, and reads it from the start a chunk
// at a time, until a read comes back short. It reads what the page cache
// holds, as every read does, and a file renamed or removed while it's open
// goes on being read, as on Linux.
type copier struct {
	sys  fsys.FS // a process of the copy's own
	path string
	at   int    // the workload's call it starts just before
	buf  []byte // a chunk
	m    *Model // the run's, for the commits the copy may hold

	stage    int
	f        fsys.File
	data     []byte // what it has read so far
	end      int    // the call its last read came just before, one past the workload's last for a read once it had ended
	from, to int    // it may hold the model's states from index from up to to, with to left out
	err      error
}

// A copy's stages.
const (
	waiting = iota // for the call it starts at
	reading
	copied
	noFile // there was no file at the path when it was to start
)

// before reads the next chunk just before the workload's call n, and
// starts the copy first when n is its call.
func (c *copier) before(n int) {
	if c.stage == waiting && n == c.at {
		c.start()
	}
	if c.stage == reading {
		c.read(n)
	}
}

// finish reads the rest of the file once the workload has ended, after n
// calls, and starts the copy first when it's to be taken at the end.
func (c *copier) finish(n int) {
	if c.stage == waiting && c.at == n+1 {
		c.start()
	}
	for c.stage == reading {
		c.read(n + 1)
	}
}

func (c *copier) start() {
	f, err := c.sys.Open(c.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.stage = noFile
	case err != nil:
		c.stage, c.err = copied, err
	default:
		c.stage, c.f, c.from = reading, f, c.m.last
	}
}

// read reads the next chunk, just before the workload's call call.
func (c *copier) read(call int) {
	n, err := c.f.ReadAt(c.buf, int64(len(c.data)))
	c.data = append(c.data, c.buf[:n]...)
	if n == len(c.buf) && err == nil {
		return
	}
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	c.f.Close()
	c.stage, c.end, c.to = copied, call, len(c.m.states)
}
