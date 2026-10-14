// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Report is what Check found.
type Report struct {
	Tables   int      // tables
	Records  int      // records in them
	Links    int      // links between records
	Vectors  int      // records with a vector
	Problems []string // empty when the database is consistent

	// FileBytes is the size of the database's file, in bytes, as Check
	// found it.
	FileBytes int64
	// MemoryBytes is about how much memory the database's copy in this
	// process takes, in bytes: every read goes to the copy, and Check adds
	// it up from its parts. Go's heap can grow to twice what it holds
	// between garbage collections, and GOMEMLIMIT, set a little above this,
	// keeps it near.
	MemoryBytes int64
	// CompactionError is what went wrong in the last compaction made
	// through this DB, by Compact or after a commit, or "" when it worked or
	// none has run. A compaction that fails leaves the database whole, so
	// it isn't one of the problems, and OK doesn't count it. When it failed
	// before its new file took the path, the database carries on in its old
	// file, and when the folder's sync failed after that, every write
	// through this DB fails with ErrStuck until it's closed.
	CompactionError string
}

// OK reports whether Check found no problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Check reads the whole database and confirms that the four ways into it
// agree, at a single point in the log. Each process holds the database in
// memory, and Check reads that copy: every key starts with its table's
// name, and every record is in its table's keys, in byte order, and in the
// hash table that finds it by its key; every link joins two records that
// exist and is listed at both ends; and every vector has its table's size
// and holds finite values that aren't all zero. It also reads the file again
// from its start, as far as this process has read it, and checks each
// batch's checksum and marker there, so damage to a part of the file read
// long ago shows before the next Open meets it. That read costs about what
// reading the file costs, and takes no lock, so other processes write while
// it runs.
//
// Damage in the file is a problem in the report, whether Check's own read
// finds it or the read before it meets it, as it takes in what other
// processes have committed. The problem names the damaged batch and the
// offset where the damage was found, and the counts are the copy's, which
// holds what this process read before it met the damage. After a reload
// that failed on damage, the copy holds part of the file at the path, which
// no read sees, so the report holds that problem and the file's size alone.
//
// Check returns an error when it can't check: on a closed database, inside
// an Update after its first change, as every read through the database
// does, or when a read fails, a wait for the write lock to confirm damage
// times out, or a reload fails for a reason other than damage.
func (db *DB) Check() (Report, error) {
	if _, err := db.current(); err != nil {
		return Report{}, err
	}
	// Damage the read meets as it follows the file is the report's, and any
	// other error ends Check.
	var met *Damage
	if err := db.follow(); err != nil {
		if !errors.As(err, &met) {
			return Report{}, err
		}
		if plant == "hypercrux/check-follow-damage-dropped" {
			met = nil
		}
	}
	var (
		c       store.Checked
		partial bool // the copy holds part of a file, after a reload that failed on damage
		at      logfile.Applied
		read    bool // at says how far the copy's log has been read
	)
	err := func() error {
		db.gate.RLock()
		defer db.gate.RUnlock()
		s, err := db.current()
		if err != nil {
			return err
		}
		if db.unread != nil {
			// The follow just now met the reload's error itself, unless an
			// Update holds the write lock's mutex, whose Lock reads the file
			// again.
			var d *Damage
			if !errors.As(db.unread, &d) {
				return db.unread
			}
			met, partial = d, true
			if plant != "hypercrux/check-reload-copy-read" {
				return nil
			}
		}
		return s.Read(func(store.Reader) error {
			c = checkCopy(s)
			at, read = db.log.Applied()
			return nil
		})
	}()
	if err != nil {
		return Report{}, err
	}
	var rep Report
	if met != nil {
		rep.Problems = append(rep.Problems, damaged(met))
	}
	size, err := db.readFile(at, read && !partial)
	var d *Damage
	if errors.As(err, &d) {
		// A file shorter than this process has read is damage the follow
		// meets too, and named once.
		if line := damaged(d); met == nil || line != damaged(met) {
			rep.Problems = append(rep.Problems, line)
		}
	} else if err != nil {
		return Report{}, err
	}
	rep.FileBytes = size
	rep.CompactionError = db.compactionError()
	if partial && plant != "hypercrux/check-reload-copy-read" {
		return rep, nil
	}
	rep.Tables, rep.Records, rep.Links, rep.Vectors = c.Tables, c.Records, c.Links, c.Vectors
	rep.Problems = append(rep.Problems, c.Problems...)
	rep.MemoryBytes = c.Bytes
	return rep, nil
}

// checkCopy is the copy's own check, which Check, Export and Import run. A
// test can put another in its place, to plant problems in what it finds.
var checkCopy = (*store.Store).Check

// damaged is a problem in a report that names damage in the file: what
// Damage's Error says, without the package's name and the file's.
func damaged(d *Damage) string {
	var b strings.Builder
	b.WriteString("the file is damaged: ")
	if d.Batch != 0 {
		fmt.Fprintf(&b, "batch %d at ", d.Batch)
	}
	fmt.Fprintf(&b, "offset %d", d.Offset)
	if d.Reason != "" {
		b.WriteString(": " + d.Reason)
	}
	return b.String()
}

// refuse is the error Export and Import give when the copy's own check
// finds problems, in 0.x's words, with outcome saying what didn't happen,
// or nil when it finds none. s is the copy, read inside its Read or by the
// Update that holds it.
func refuse(s *store.Store, outcome string) error {
	c := checkCopy(s)
	if len(c.Problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: hypercrux check finds problems in the file, so %s; the first is: %s", ErrInvalid, outcome, c.Problems[0])
}

// readFile is Check's read of the file: it returns the size of the file at
// the database's path, and when check is true and that file is the one at,
// it reads the file's log from its start to at.End and checks it
// (readLog). A file shorter than that is damage, as it is when a follower
// finds it. When another file has taken the path since this process last
// read it, there's nothing of the copy's to read there, and the next read
// reloads.
func (db *DB) readFile(at logfile.Applied, check bool) (int64, error) {
	path := db.log.Path()
	info, err := db.files.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !check || info.Dev != at.Dev || info.Ino != at.Ino || at.End == 0 || plant == "hypercrux/check-file-skipped" {
		return info.Size, nil
	}
	if info.Size < at.End {
		return info.Size, &Damage{Path: path, Offset: info.Size, Reason: fmt.Sprintf(
			"the file is %d bytes long, shorter than the %d this process has read from it: it was copied over or cut", info.Size, at.End)}
	}
	f, err := db.files.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return info.Size, nil
	}
	if err != nil {
		return info.Size, err
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil || !got.Same(info) {
		return info.Size, err
	}
	return got.Size, readLog(f, path, at, got.Size)
}

// readSize is how much readLog reads from the file at a time.
const readSize = 1 << 20

// castagnoli is CRC32C's table, which FORMAT.md's checksums use.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// readLog reads the log in f, a file of size bytes at path, from its start
// to at.End, the end of the log as a process has read it. Nothing before the
// end of a marker ever changes in a HyperCrux file, so every batch up to the
// end has to count and be marked, one after another, and end there, and the
// header and the last marker have to be the ones the process read, at.Head
// and at.Last: another database's file, or another copy of this one, copied
// over the file in place can have its length and keep every rule (G7.md's
// note for R2). readLog checks the header, then each batch's head, its
// checksum and its marker, streaming the batch through the checksum a
// megabyte at a time, so a batch of any size costs that much memory. It
// decodes no changes: the process read each batch whole and took its
// changes, or wrote them, and the checksum shows the bytes are as they were.
//
// It returns the first damage it finds, a *Damage naming the batch and the
// offset where the damage was found: the batch's start for its head and its
// checksum, and its marker's offset for its marker, or the header's start.
// Or it returns a failed read's error.
func readLog(f fsys.File, path string, at logfile.Applied, size int64) error {
	end := at.End
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, end), readSize)
	var head [format.HeaderSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	h, err := format.DecodeHeader(head[:], size)
	if err != nil {
		var d *Damage
		if !errors.As(err, &d) {
			d = &Damage{Reason: strings.TrimPrefix(err.Error(), "hypercrux: ")}
		}
		d.Path, d.Reason = path, d.Reason+"; this process read the header whole, so the file has changed since"
		return d
	}
	if head != at.Head && plant != "hypercrux/check-copy-over-unseen" {
		return &Damage{Path: path, Reason: "the header isn't the one this process read: another database's file, or another copy of this one, has been copied over it"}
	}
	sum := crc32.New(castagnoli)
	var tail [4 + format.MarkerSize]byte
	off, seq := int64(format.HeaderSize), uint64(1)
	damage := func(at int64, why string) error {
		return &Damage{Path: path, Offset: at, Batch: seq, Reason: why + "; this process read the log past it, so the file has changed since"}
	}
	// Nothing cuts a HyperCrux file short of a marker, so a file that ends
	// before end while it's read was copied over or cut.
	cut := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return damage(off, "the file ends inside the batch")
		}
		return err
	}
	for off < end {
		b, err := r.Peek(format.BatchHeadSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		n, err := format.BatchLength(b, h.Gen, seq, end-off)
		if err != nil {
			return damage(off, strings.TrimPrefix(err.Error(), format.ErrDoesNotCount.Error()+": "))
		}
		if n+format.MarkerSize > end-off {
			return damage(off, fmt.Sprintf("the batch is %d bytes long, and with its marker would end past offset %d, where this process read the log to", n, end))
		}
		sum.Reset()
		for left := int(n) - 4; left > 0; {
			b, err := r.Peek(min(left, readSize))
			if len(b) == 0 {
				return cut(err)
			}
			sum.Write(b)
			r.Discard(len(b))
			left -= len(b)
		}
		if _, err := io.ReadFull(r, tail[:]); err != nil {
			return cut(err)
		}
		markerAt := off + n
		if plant == "hypercrux/check-marker-at-the-batch" {
			markerAt = off
		}
		got := binary.LittleEndian.Uint32(tail[:4])
		m, whole := format.DecodeMarker(tail[4:], h.ID, h.Gen)
		switch {
		case got != sum.Sum32():
			return damage(off, fmt.Sprintf("the checksum is %#08x, where CRC32C gives %#08x", got, sum.Sum32()))
		case !whole:
			return damage(markerAt, "its marker isn't whole")
		case m != format.Marker{Seq: seq, Sum: got}:
			return damage(markerAt, fmt.Sprintf("the whole marker after it names batch %d with the checksum %#08x, where the batch is %d with %#08x", m.Seq, m.Sum, seq, got))
		}
		off += n + format.MarkerSize
		seq++
	}
	// The log ends at the batch the process read last, with its marker.
	if (seq-1 != at.Seq || at.Seq > 0 && [format.MarkerSize]byte(tail[4:]) != at.Last) && plant != "hypercrux/check-copy-over-unseen" {
		return &Damage{Path: path, Offset: off - format.MarkerSize, Batch: seq - 1, Reason: fmt.Sprintf(
			"the marker that ends the log isn't the one this process read there, after batch %d: another database's file, or another copy of this one, has been copied over it", at.Seq)}
	}
	return nil
}
