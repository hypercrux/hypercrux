// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// SectorSize is the unit a power cut keeps, loses or tears on its own:
// 512 bytes, the smallest sector drives write.
const SectorSize = 512

// MaxSize is the longest a file can grow on a Disk, which holds every file
// in memory twice. A write or a Truncate past it fails with EFBIG.
const MaxSize = 1 << 30

// ErrCut is the error a call gets when the power is cut in it, and the
// error every later call gets through an FS from before the cut, or on a
// file opened through one.
var ErrCut = errors.New("fault: the power was cut")

// Disk is a drive held in memory, with a page cache in front of it, for
// the crash tests. A process reaches it through FS, as the engine reaches
// the real file system through fsys.FS and fsys.File. The seed given to New
// decides every choice it makes, so the same seed and the same calls in
// the same order give the same disk, and a failure replays.
//
// Reads see the page cache, which holds everything written. The drive
// holds what each file's last Sync put there. A WriteAt, and the change of
// size a Truncate makes, stay pending until the file's next Sync puts them
// on the drive, with the size.
//
// Cut, or a Rule with Cut set, cuts the power, and each file's pending
// changes are settled:
//
//   - Each sector a pending change touched is kept, lost or torn on its
//     own, as a drive can write the sectors in its cache in any order.
//     Kept, it holds what reads saw. Lost, it holds what the drive held.
//     Torn, it holds the one up to a point and the other after it, the
//     point falling between the first and the last bytes where the two
//     differ, so a torn sector matches neither.
//   - A third of the time every pending sector of a file is kept, a third
//     of the time every one is lost, and otherwise each is kept, lost or
//     torn on its own, a third of the time each. Whole outcomes come up
//     often that way, and mixed ones too.
//   - Bytes no pending change touched keep what the drive held, even in a
//     torn sector, so synced data always survives a cut. SQLite calls this
//     powersafe overwrite, and assumes it by default.
//   - A file's size becomes the one its last Sync left, the one reads saw,
//     or any size between the smallest and the largest it has had since
//     that Sync, a third of the time each. Past what the drive held, a file
//     reads as zeros.
//   - A cut in a WriteAt or a Truncate comes once the call has reached the
//     cache, so its change is pending at the cut. In any other call, the
//     cut comes first.
//
// After the cut, every call through an FS from before it, or on a file
// opened through one, fails with ErrCut, and every flock is gone. FS then
// gives the machine started again, where reads see what the drive holds.
// Names stay as they were: a file created, renamed or removed stays so
// through a cut whether or not its folder was synced, and so do its
// permissions and its owner. T2 makes names depend on SyncDir.
//
// A Rule makes a call fail at its nth use. A failed call changes nothing,
// apart from these:
//
//   - A failed WriteAt writes the first n bytes it was given, where n, the
//     count it returns, is anything from 0 to one less than all of them.
//   - A failed ReadAt reads its first n bytes the same way.
//   - A failed Close closes the file and lets go of its flock all the same,
//     as Linux's close does.
//   - A failed Sync settles the file's pending sectors as Linux can after
//     an error writing them back: each is written to the drive, torn
//     there, marked clean without reaching it, or left pending for the
//     next Sync. A sector torn or marked clean stays as the drive holds it
//     even when a later Sync succeeds, and reads go on seeing what was
//     written, until something writes to the sector again. That's the
//     case beta/FORMAT.md writes a batch again for. A quarter of the time
//     every sector is marked clean, a quarter of the time every one is
//     left pending, a quarter of the time every one is written, and
//     otherwise each gets one of the four on its own. The size stays
//     pending.
//
// A flock belongs to an open file, as on Linux. Two opens of one file
// contend for it, in one process or in two, an open that holds it can
// take it again, and it stays with the file through a rename. Close lets
// go of it, and so does a cut.
//
// Paths are absolute, and a relative one is taken from "/". A Disk starts
// with the folder "/" alone, and Mkdir makes more.
type Disk struct {
	mu     sync.Mutex
	rng    *rand.Rand
	boot   int               // how many times the power has been cut
	names  map[string]*inode // every file and folder, by its path
	ino    uint64            // the last inode number given out
	rules  []*rule
	calls  map[callKey]int
	failed int
}

// New returns a disk holding the empty folder "/", whose choices the seed
// decides.
func New(seed uint64) *Disk {
	d := &Disk{
		rng:   rand.New(rand.NewPCG(seed, 0x6661756c74)),
		names: map[string]*inode{},
		calls: map[callKey]int{},
	}
	d.names["/"] = d.newInode(true, 0o755)
	return d
}

func (d *Disk) newInode(dir bool, perm fs.FileMode) *inode {
	d.ino++
	n := &inode{ino: d.ino, dir: dir, mode: perm.Perm(), uid: os.Getuid(), gid: os.Getgid(), nlink: 1}
	if dir {
		n.mode |= fs.ModeDir
		n.nlink = 2
	}
	return n
}

// FS returns the file system as a process sees it, until the power is next
// cut. Two processes on one machine are two FS from between the same cuts:
// they share the page cache, and their open files contend for flocks as
// any two open files do.
func (d *Disk) FS() fsys.FS {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &proc{d: d, boot: d.boot}
}

// Mkdir makes the folder dir, with any folders above it that are missing,
// as os.MkdirAll does. It's for setting a test up, so it isn't a call any
// rule sees or Calls counts.
func (d *Disk) Mkdir(dir string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dir == "" {
		return &fs.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOENT}
	}
	dir = clean(dir)
	at := ""
	for _, part := range strings.Split(dir, "/")[1:] {
		if part == "" {
			continue
		}
		at += "/" + part
		if n := d.names[at]; n == nil {
			d.names[at] = d.newInode(true, 0o755)
		} else if !n.dir {
			return &fs.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
		}
	}
	return nil
}

// Add sets a rule. It counts the calls that match it from now on. Add
// panics on a rule whose Op isn't a kind of call, or whose N is below 1.
func (d *Disk) Add(r Rule) {
	if r.Op < Any || r.Op >= numOps {
		panic(fmt.Sprintf("fault: a rule for %v, which isn't a kind of call", r.Op))
	}
	if r.N < 1 {
		panic(fmt.Sprintf("fault: a rule for call %d; calls are counted from 1", r.N))
	}
	if r.Path != "" {
		r.Path = clean(r.Path)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rules = append(d.rules, &rule{Rule: r})
}

// Clear removes every rule, so that, say, a test can look at what a disk
// that kept failing left behind.
func (d *Disk) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rules = nil
}

// Cut cuts the power now, between calls.
func (d *Disk) Cut() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cut()
}

// Cuts returns how many times the power has been cut.
func (d *Disk) Cuts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boot
}

// Failed returns how many calls rules have failed.
func (d *Disk) Failed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failed
}

// Calls returns how many calls of the kind op, or of every kind for Any,
// have been made on path, or on every path for "", since the disk was
// made. A call on an open file is on the path it was opened or created
// with, even once the file has been renamed or removed. A call on names is
// on each path it's given, so a rename counts for both of its paths. A
// call counts once it reaches the disk, whether it then fails or not. A
// call on a closed file doesn't reach it, and nor does one from a process
// a cut killed.
//
// A test can run its work once and count the calls, then run it again
// with a rule for each of them.
func (d *Disk) Calls(op Op, path string) int {
	if path != "" {
		path = clean(path)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[callKey{op, path}]
}

// cut cuts the power, with d.mu held: each file's pending changes are
// settled, in the order of inode numbers so the seed decides the same way
// each time, and every process so far is dead. A file without a name is
// gone, since nothing can open it once its process has died.
func (d *Disk) cut() {
	d.boot++
	files := make([]*inode, 0, len(d.names))
	for _, n := range d.names {
		if !n.dir {
			files = append(files, n)
		}
	}
	slices.SortFunc(files, func(a, b *inode) int { return cmp.Compare(a.ino, b.ino) })
	for _, n := range files {
		n.settle(d)
	}
}

// clean makes a path absolute, taking a relative one from "/", and cleans
// it, as Linux would resolve it with no symbolic links on the way.
func clean(p string) string {
	if !path.IsAbs(p) {
		p = "/" + p
	}
	return path.Clean(p)
}

// find returns the file or folder at name, or the error Linux gives when
// nothing is there.
func (d *Disk) find(name string) (*inode, error) {
	if n := d.names[name]; n != nil {
		return n, nil
	}
	return nil, d.missing(name)
}

// missing says why nothing is at name: ENOTDIR when a file stands where a
// folder above it should be, as Linux says, and ENOENT otherwise.
func (d *Disk) missing(name string) error {
	if name == "" {
		return syscall.ENOENT
	}
	for dir := path.Dir(name); dir != "/"; dir = path.Dir(dir) {
		if n := d.names[dir]; n != nil {
			if !n.dir {
				return syscall.ENOTDIR
			}
			break
		}
	}
	return syscall.ENOENT
}

// room returns nil when a new name can go at name, since the folder above
// it exists, or the error Linux gives otherwise.
func (d *Disk) room(name string) error {
	if name == "" {
		return syscall.ENOENT
	}
	if n := d.names[path.Dir(name)]; n == nil || !n.dir {
		return d.missing(name)
	}
	return nil
}
