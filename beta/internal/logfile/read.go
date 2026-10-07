// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// load reads l.f from its start: the header, then the log, handing every
// marked batch to the Target. An empty file holds no database yet, and
// load notes that and reads nothing more.
func (l *Log) load() error {
	l.empty, l.hdr, l.seq, l.end, l.tail = false, format.Header{}, 0, 0, false
	st, err := l.f.Stat()
	if err != nil {
		return err
	}
	if st.Size == 0 {
		l.empty = true
		return nil
	}
	n, err := l.f.ReadAt(l.head[:min(st.Size, format.HeaderSize)], 0)
	if err != nil && err != io.EOF {
		return err
	}
	h, err := format.DecodeHeader(l.head[:n], st.Size)
	if err != nil {
		var d *errs.Damage
		if errors.As(err, &d) {
			d.Path = l.path
			return d
		}
		return fmt.Errorf("%s: %w", l.path, err)
	}
	l.hdr = h
	l.end = format.HeaderSize
	return l.readOn(st.Size)
}

// readOn reads the log on from l.end, in a file of size bytes, handing
// every marked batch to the Target in order, and stops at the end of the
// log: the first batch that isn't marked, or the end of the file.
//
// Without the write lock, what lies past the end of the log can change
// while it's read (FORMAT.md, "Reading the log"). A batch read while its
// bytes change fails its checksum, or its marker isn't whole or names
// another batch, so a commit is applied only once it's whole and marked.
func (l *Log) readOn(size int64) error {
	defer l.r.release()
	for {
		off := l.end
		room := size - off
		if room < format.BatchHeadSize {
			break
		}
		head, err := l.r.read(l.f, off, format.BatchHeadSize, size)
		if errors.Is(err, io.EOF) {
			break // the file has been cut since its size was read: the log ends there for now
		}
		if err != nil {
			return err
		}
		n, err := format.BatchLength(head, l.hdr.Gen, l.seq+1, room)
		if err != nil {
			break // not a batch that counts
		}
		b, err := l.r.read(l.f, off, min(n+format.MarkerSize, room), size)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		bt, err := format.DecodeBatch(b[:n], l.hdr.Gen, l.seq+1)
		if errors.Is(err, format.ErrDoesNotCount) {
			break
		}
		if err != nil {
			// A batch that counts, whose changes are malformed or break
			// the rules for a change on its own: damage, marked or not.
			var d *errs.Damage
			if errors.As(err, &d) {
				d.Path = l.path
				d.Offset += off
			}
			return err
		}
		if int64(len(b)) < n+format.MarkerSize {
			break // no room for its marker
		}
		m, whole := format.DecodeMarker(b[n:], l.hdr.ID, l.hdr.Gen)
		if !whole || (m != format.Marker{Seq: bt.Seq, Sum: bt.Sum} && plant != "logfile/any-marker") {
			break // a commit under way, or the remains of one
		}
		if err := l.t.Apply(bt.Seq, bt.Changes); err != nil {
			return &errs.Damage{Path: l.path, Offset: off, Batch: bt.Seq, Reason: "changes that break the rules for the state they apply to: " + err.Error()}
		}
		l.seq = bt.Seq
		l.end = off + n + format.MarkerSize
		copy(l.last[:], b[n:])
	}
	// F4 goes here, where reading stops: a whole marker past the end of
	// the log naming the next batch or a later one is damage once a read
	// under the write lock agrees, and so is a marked batch that fails its
	// checks. F2 stops at the end of the log, and takes what it finds there
	// for a commit under way or the remains of one.
	return nil
}

// checkRead checks, holding the write lock, that what l has read is still
// in the file: the file is as long as the log l has read, at least, and the
// marker that ends it, or the header when l has read no batch, holds the
// same bytes as when l read it. Nothing before the end of a marker ever
// changes in a HyperCrux file, so a difference means the file was copied
// over or damaged (FORMAT.md, "Writing"). That's damage, and nothing is
// written.
func (l *Log) checkRead(info fsys.Info) error {
	if plant == "logfile/no-read-check" {
		return nil
	}
	if info.Size < l.end {
		return &errs.Damage{Path: l.path, Offset: info.Size, Reason: fmt.Sprintf("the file is %d bytes long, shorter than the %d this process has read from it: it was copied over or cut", info.Size, l.end)}
	}
	at, want, what := int64(0), l.head[:], "the header"
	if l.seq > 0 {
		at, want, what = l.end-format.MarkerSize, l.last[:], fmt.Sprintf("batch %d's marker", l.seq)
	}
	got := make([]byte, len(want))
	if _, err := l.f.ReadAt(got, at); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return &errs.Damage{Path: l.path, Offset: at, Batch: l.seq, Reason: what + " has changed since this process read it: the file was copied over or damaged"}
	}
	return nil
}

// window is how much the reader reads at a time, when the file has that
// much left. A batch longer than that is read whole.
const window = 1 << 20

// reader reads the log through a window onto the file, up to a megabyte
// at a time, so a log of many small batches takes few read calls.
type reader struct {
	buf []byte // the file's bytes from off on
	off int64
}

// read returns the file's n bytes from off, which lie within its first
// size bytes. They're good until the next call. When the file has been cut
// meanwhile, the error is io.EOF.
func (r *reader) read(f fsys.File, off, n, size int64) ([]byte, error) {
	if off >= r.off && off+n <= r.off+int64(len(r.buf)) {
		return r.buf[off-r.off : off-r.off+n], nil
	}
	want := max(n, min(window, size-off))
	if int64(cap(r.buf)) < want {
		r.buf = make([]byte, want)
	}
	m, err := f.ReadAt(r.buf[:want], off)
	r.buf, r.off = r.buf[:m], off
	if int64(m) < n {
		if err == nil {
			err = io.ErrUnexpectedEOF // ReadAt broke its promise
		}
		return nil, err
	}
	return r.buf[:n], nil
}

// release forgets what the window holds, since what lies past the end of
// the log can change before the next read, and lets a window that a long
// batch made larger than usual go.
func (r *reader) release() {
	if cap(r.buf) > window {
		r.buf = nil
	}
	r.buf = r.buf[:0]
}
