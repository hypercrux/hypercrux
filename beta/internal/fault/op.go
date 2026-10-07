// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Op is a kind of call: one of the methods of fsys.File and fsys.FS, or
// Any, which stands for every one of them. Fstat is File's Stat, and Stat
// is FS's.
type Op int

// The kinds of call. ReadAt to Close are fsys.File's, and Open to List are
// fsys.FS's.
const (
	Any Op = iota
	ReadAt
	WriteAt
	Sync
	Truncate
	Fstat
	Chown
	TryLock
	Unlock
	Close
	Open
	Create
	Rename
	RenameNoReplace
	Remove
	SyncDir
	Stat
	RealPath
	List
	numOps
)

var opNames = [numOps]string{
	"Any", "ReadAt", "WriteAt", "Sync", "Truncate", "Fstat", "Chown", "TryLock", "Unlock", "Close",
	"Open", "Create", "Rename", "RenameNoReplace", "Remove", "SyncDir", "Stat", "RealPath", "List",
}

// sysNames are the kinds' names in the errors they give, after the system
// calls the real ones make.
var sysNames = [numOps]string{
	"", "pread", "pwrite", "fdatasync", "ftruncate", "fstat", "fchown", "flock", "flock", "close",
	"open", "open", "rename", "renameat2", "unlink", "fsync", "stat", "realpath", "readdir",
}

func (op Op) String() string {
	if op < 0 || op >= numOps {
		return fmt.Sprintf("Op(%d)", int(op))
	}
	return opNames[op]
}

// A Rule picks a call and says what goes wrong in it: it fails with an
// error, or the power is cut while it runs. Disk.Add sets a rule, and the
// rule counts the calls that match it from then on. When two rules pick
// one call, a cut wins over a failure, and of two failures the rule set
// first gives the error.
type Rule struct {
	// Op is the kind of call. Any matches every kind.
	Op Op

	// Path, when it isn't "", matches only calls on that path, as
	// Disk.Calls counts them. A relative path is taken from "/".
	Path string

	// N picks the matching call that goes wrong, counted from 1.
	N int

	// Times is how many matching calls in a row fail, from the nth on: 0
	// means 1, and a negative number means every one from the nth on, as a
	// disk that has died or filled up keeps failing. A cut comes once.
	Times int

	// Cut cuts the power during the call, as Disk's comment describes, in
	// place of failing it. Err is ignored then.
	Cut bool

	// Err is the error the call fails with. The call wraps it with its
	// own name and its path, as an *fs.PathError, or an *os.LinkError for
	// a rename, so errors.Is finds it. nil means syscall.EIO.
	Err error
}

// rule is a Rule that Disk.Add has set, with its count.
type rule struct {
	Rule
	seen int // the matching calls so far
}

func (r *rule) matches(op Op, paths []string) bool {
	if r.Op != Any && r.Op != op {
		return false
	}
	if r.Path == "" {
		return true
	}
	for _, p := range paths {
		if p == r.Path {
			return true
		}
	}
	return false
}

// fires reports whether the matching call just counted goes wrong.
func (r *rule) fires() bool {
	n := r.N
	if plant == "fault/nth-plus-one" {
		n++
	}
	if r.seen < n {
		return false
	}
	if r.Cut {
		return r.seen == n
	}
	if r.Times < 0 {
		return true
	}
	return r.seen < n+max(r.Times, 1)
}

// verdict is what happens to a call.
type verdict int

const (
	goAhead verdict = iota
	failIt          // a rule fails it
	cutIt           // a rule cuts the power in it
	refuse          // it never reaches the disk: its file is closed, or a cut killed its process
)

// callKey counts calls of a kind on a path. Any counts every kind, and ""
// every path.
type callKey struct {
	op   Op
	path string
}

// enter counts a call from a live process, with d.mu held, against every
// path it names and every rule that matches it, and says what happens to
// it. When a rule fails the call, err is that rule's error. A cut beats a
// failure, and among failures the first rule set wins.
func (d *Disk) enter(op Op, paths ...string) (v verdict, err error) {
	d.calls[callKey{op, ""}]++
	d.calls[callKey{Any, ""}]++
	for i, p := range paths {
		if p == "" || (i == 1 && p == paths[0]) {
			continue
		}
		d.calls[callKey{op, p}]++
		d.calls[callKey{Any, p}]++
	}
	for _, r := range d.rules {
		if !r.matches(op, paths) {
			continue
		}
		r.seen++
		switch {
		case !r.fires():
		case r.Cut:
			v = cutIt
		case v == goAhead:
			v, err = failIt, r.Err
			if err == nil {
				err = syscall.EIO
			}
		}
	}
	if v == failIt {
		d.failed++
	}
	return v, err
}

// wrap wraps err with the call's name and its paths, as the os package
// does: an *os.LinkError for a call with two paths, and an *fs.PathError
// otherwise.
func wrap(op Op, err error, paths ...string) error {
	if len(paths) == 2 {
		return &os.LinkError{Op: sysNames[op], Old: paths[0], New: paths[1], Err: err}
	}
	return &fs.PathError{Op: sysNames[op], Path: paths[0], Err: err}
}
