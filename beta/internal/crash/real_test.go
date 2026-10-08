// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// TestCopiesOfTheRealLog drives the real log, F2's, with copies alone: a
// copy taken mid-commit, as cp would take it, opens at a state the model
// allows, from the database's creation on. Cuts and failures wait for F3
// and F5, since F2's log can't recover yet: a cut can take the marker of
// the last commit, which nothing writes again.
func TestCopiesOfTheRealLog(t *testing.T) {
	rep, err := Run(realWorkload(5), Options{Kinds: Copies, Seeds: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
	if rep.Copies == 0 || rep.NoFile == 0 {
		t.Errorf("%d copies, and %d points with no file to copy, where the database is created by the workload", rep.Copies, rep.NoFile)
	}
}

// realPath is where the real log's database lives.
const realPath = "/db/real.hcx"

// realWorkload creates a database with the real log and makes commits
// commits on it, the ith with the toy log's batch i. A state is each batch
// the log hands over, a line each.
func realWorkload(commits int) Workload {
	return Workload{
		Path: realPath,
		Run: func(sys fsys.FS, m *Model) error {
			l, _, err := openReal(sys)
			if err != nil {
				return err
			}
			defer l.Close()
			var state strings.Builder
			for i := 1; i <= commits; i++ {
				changes := toyBatch(i)
				fmt.Fprintln(&state, i, changes)
				m.Begin(state.String())
				if err := l.Lock(); err != nil {
					return err
				}
				if err := l.Append(changes); err != nil {
					l.Unlock()
					return err
				}
				m.Done() // once Append returns, the commit is on the drive
				if err := l.Unlock(); err != nil {
					return err
				}
			}
			return nil
		},
		Reopen: func(sys fsys.FS) (string, error) {
			l, rec, err := openReal(sys)
			if err != nil {
				return "", err
			}
			return rec.state.String(), l.Close()
		},
	}
}

// openReal opens the real log at realPath through sys, with a recorder.
func openReal(sys fsys.FS) (*logfile.Log, *recorder, error) {
	rec := &recorder{}
	l, err := logfile.Open(sys, realPath, rec, logfile.Options{})
	return l, rec, err
}

// recorder is the real log's Target: it writes down each batch it's
// handed, on a line of its own.
type recorder struct{ state strings.Builder }

func (r *recorder) Apply(seq uint64, changes []format.Change) error {
	fmt.Fprintln(&r.state, seq, changes)
	return nil
}

func (r *recorder) Reset() { r.state.Reset() }
