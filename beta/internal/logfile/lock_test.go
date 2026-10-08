// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// Some tests start copies of the test binary as helper processes on one
// database. TestMain sends a copy to its role instead of the tests.
func TestMain(m *testing.M) {
	if role := os.Getenv("HYPERCRUX_LOGFILE_ROLE"); role != "" {
		os.Exit(helper(role, os.Getenv("HYPERCRUX_LOGFILE_PATH")))
	}
	os.Exit(m.Run())
}

// helperChanges is the batch a holding helper commits, and helperDied the
// batch a dying helper leaves without its marker.
var (
	helperChanges = table("helper")
	helperDied    = table("died")
)

// helper runs a helper process's role on the database at path:
//
//   - hold: opens the database, takes the write lock, commits a batch,
//     prints "locked", and keeps the lock until its standard input closes,
//     or for HYPERCRUX_LOGFILE_HOLD when that's set.
//   - create: waits for a line on its standard input, then opens the
//     database, creating it when it isn't there, and prints its ID.
//   - die: opens the database, takes the write lock, writes the next batch
//     and syncs it, prints "written", and exits holding the lock, before it
//     writes the marker, as a writer killed then would. With
//     HYPERCRUX_LOGFILE_TAIL=half, it writes half the batch.
func helper(role, path string) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "helper:", role+":", err)
		return 1
	}
	switch role {
	case "hold":
		l, err := Open(fsys.OS{}, path, &recorder{}, Options{})
		if err != nil {
			return fail(err)
		}
		if err := l.Lock(); err != nil {
			return fail(err)
		}
		if err := l.Append(helperChanges); err != nil {
			return fail(err)
		}
		fmt.Println("locked")
		if d, err := time.ParseDuration(os.Getenv("HYPERCRUX_LOGFILE_HOLD")); err == nil {
			time.Sleep(d)
		} else {
			io.Copy(io.Discard, os.Stdin)
		}
		if err := l.Unlock(); err != nil {
			return fail(err)
		}
		if err := l.Close(); err != nil {
			return fail(err)
		}
	case "create":
		bufio.NewReader(os.Stdin).ReadString('\n')
		l, err := Open(fsys.OS{}, path, &recorder{}, Options{})
		if err != nil {
			return fail(err)
		}
		fmt.Printf("%x\n", l.hdr.ID)
		if err := l.Close(); err != nil {
			return fail(err)
		}
	case "die":
		l, err := Open(fsys.OS{}, path, &recorder{}, Options{})
		if err != nil {
			return fail(err)
		}
		if err := l.Lock(); err != nil {
			return fail(err)
		}
		b, _, err := format.AppendBatch(nil, l.hdr.Gen, l.seq+1, helperDied)
		if err != nil {
			return fail(err)
		}
		if os.Getenv("HYPERCRUX_LOGFILE_TAIL") == "half" {
			b = b[:len(b)/2]
		}
		if _, err := l.f.WriteAt(b, l.end); err != nil {
			return fail(err)
		}
		if err := l.f.Sync(); err != nil {
			return fail(err)
		}
		fmt.Println("written")
	default:
		return fail(errors.New("no such role"))
	}
	return 0
}

// proc is a helper process, with pipes to its standard input and output.
type proc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// start starts a helper process in role on the database at path, with the
// environment variables env added. It's killed when the test ends, if it
// hasn't ended by then.
func start(t *testing.T, role, path string, env ...string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HYPERCRUX_LOGFILE_ROLE="+role, "HYPERCRUX_LOGFILE_PATH="+path)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, in: in, out: bufio.NewReader(out)}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	return p
}

// line reads a line the helper prints.
func (p *proc) line(t *testing.T) string {
	t.Helper()
	s, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatalf("reading from a helper: %v", err)
	}
	return strings.TrimSuffix(s, "\n")
}

// end closes the helper's standard input, which ends a holding helper, and
// waits for it to exit.
func (p *proc) end(t *testing.T) {
	t.Helper()
	p.in.Close()
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("a helper failed: %v", err)
	}
}

// TestTheLockTimesOutAcrossProcesses is the second part of F2's closing
// test, with a short wait. While another process holds the write lock,
// Lock waits the whole wait and fails with errs.ErrLockTimeout. Once the
// other process lets go, Lock gets the lock and reads the batch the other
// process committed.
func TestTheLockTimesOutAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	const wait = 300 * time.Millisecond
	l, rec := openLog(t, path, Options{Wait: wait})
	h := start(t, "hold", path)
	if s := h.line(t); s != "locked" {
		t.Fatalf("the helper said %q", s)
	}
	began := time.Now()
	err := lockErr(l)
	took := time.Since(began)
	if !errors.Is(err, errs.ErrLockTimeout) {
		t.Fatalf("Lock while another process holds the lock: %v", err)
	}
	if took < wait || took > wait+5*time.Second {
		t.Errorf("Lock gave up after %v, where %v is the wait", took, wait)
	}
	rec.holds(t)

	h.end(t)
	commit(t, l, table("mine"))
	rec.holds(t, helperChanges)
	reread(t, path).holds(t, helperChanges, table("mine"))
}

// TestTheLockComesFreeDuringTheWait: Lock keeps trying flock until the
// other process lets go, and then reads what that process committed.
func TestTheLockComesFreeDuringTheWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	l, rec := openLog(t, path, Options{})
	h := start(t, "hold", path, "HYPERCRUX_LOGFILE_HOLD=400ms")
	if s := h.line(t); s != "locked" {
		t.Fatalf("the helper said %q", s)
	}
	began := time.Now()
	if err := l.Lock(); err != nil {
		t.Fatalf("Lock while another process holds the lock for 400 ms: %v", err)
	}
	took := time.Since(began)
	if err := l.Append(table("mine")); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if took < 100*time.Millisecond {
		t.Errorf("Lock took %v, so it didn't wait for the other process", took)
	}
	rec.holds(t, helperChanges)
	h.end(t)
	reread(t, path).holds(t, helperChanges, table("mine"))
}

// TestTheLockWaitsTenSeconds is the closing test's real wait: with the
// default wait, Lock gives up after 10 seconds, as 0.x does. It runs
// alongside the other parallel tests, and isn't run with -short, or with
// a planted bug switched on, since none is about the length of the wait.
func TestTheLockWaitsTenSeconds(t *testing.T) {
	if testing.Short() || os.Getenv("HYPERCRUX_PLANT") != "" {
		t.Skip("a 10-second wait")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "db")
	l, _ := openLog(t, path, Options{})
	h := start(t, "hold", path)
	if s := h.line(t); s != "locked" {
		t.Fatalf("the helper said %q", s)
	}
	began := time.Now()
	err := lockErr(l)
	took := time.Since(began)
	if !errors.Is(err, errs.ErrLockTimeout) || !strings.Contains(err.Error(), "10s") {
		t.Fatalf("Lock while another process holds the lock: %v", err)
	}
	if took < DefaultWait || took > DefaultWait+5*time.Second {
		t.Errorf("Lock gave up after %v, where the wait is %v", took, DefaultWait)
	}
	h.end(t)
}

// TestTheMutexKeepsGoroutinesApart: goroutines share one open file, whose
// flock can't keep them apart, so the mutex does.
func TestTheMutexKeepsGoroutinesApart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	l, rec := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error)
	go func() { errc <- lockErr(l) }()
	if err := <-errc; !errors.Is(err, errs.ErrLockTimeout) {
		t.Fatalf("Lock from another goroutine while one holds it: %v", err)
	}
	if err := l.Append(table("one")); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}

	// Many goroutines committing at once each get the lock in turn.
	l.wait = 30 * time.Second
	const n = 20
	for i := range n {
		go func() {
			if err := l.Lock(); err != nil {
				errc <- err
				return
			}
			err := l.Append(table(fmt.Sprintf("g%d", i)))
			if e := l.Unlock(); err == nil {
				err = e
			}
			errc <- err
		}()
	}
	for range n {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
	got := reread(t, path)
	if len(got.batches) != n+1 {
		t.Errorf("%d batches in the file, where %d are wanted", len(got.batches), n+1)
	}
	rec.holds(t)
}

// TestTheWaitIsForBothHalves: the wait covers the mutex and flock
// together. Here the mutex comes free halfway through the wait, and
// another open file holds flock throughout, so Lock gives up when the
// whole wait is over, and no later.
func TestTheWaitIsForBothHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	const wait = time.Second
	a, _ := openLog(t, path, Options{Wait: wait})
	b, _ := openLog(t, path, Options{})
	if err := b.Lock(); err != nil {
		t.Fatal(err)
	}
	defer b.Unlock()
	a.mu <- struct{}{} // another goroutine's commit, as far as a can tell
	go func() {
		time.Sleep(wait / 2)
		<-a.mu
	}()
	began := time.Now()
	err := lockErr(a)
	took := time.Since(began)
	if !errors.Is(err, errs.ErrLockTimeout) {
		t.Fatalf("Lock: %v", err)
	}
	if took < wait || took > wait+wait/3 {
		t.Errorf("Lock gave up after %v, where the wait for both halves is %v", took, wait)
	}
}

// TestTwoLogsInOneProcess: two Logs on one database each have an open
// file of their own, so flock keeps them apart as it does two processes,
// and each reads what the other commits.
func TestTwoLogsInOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	a, ra := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	b, rb := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := lockErr(b); !errors.Is(err, errs.ErrLockTimeout) {
		t.Errorf("Lock while another Log holds the lock: %v", err)
	}
	if err := a.Append(table("one")); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	commit(t, b, table("two"))
	rb.holds(t, table("one"))
	commit(t, a, table("three"))
	ra.holdsFrom(t, 2, table("two"))
	reread(t, path).holds(t, table("one"), table("two"), table("three"))
	if a.seq != 3 || b.seq != 2 {
		t.Errorf("a has read to batch %d and b to %d", a.seq, b.seq)
	}
}
