// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests start copies of the test binary as separate processes on one
// file. Main sends the copies to their role instead of the tests.

const (
	envRole = "HYPERCRUX_CONFORMANCE_ROLE"
	envFile = "HYPERCRUX_CONFORMANCE_FILE"
	envP    = "HYPERCRUX_CONFORMANCE_P"
	envN    = "HYPERCRUX_CONFORMANCE_N"
)

// Main is for the adapter's TestMain: it runs a child process's role when
// the test binary was started as one, and the tests otherwise.
func Main(m *testing.M, e Engine) {
	switch os.Getenv(envRole) {
	case "writer":
		os.Exit(childWriter(e, os.Getenv(envFile)))
	case "sharer":
		p, _ := strconv.Atoi(os.Getenv(envP))
		n, _ := strconv.Atoi(os.Getenv(envN))
		os.Exit(childSharer(e, os.Getenv(envFile), p, n))
	}
	os.Exit(m.Run())
}

func startChild(role, file string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{envRole + "=" + role, envFile + "=" + file}, env...)...)
	cmd.Stderr = os.Stderr
	return cmd
}

// vecFor is the vector the writer gives item i: fixed, so a check can tell
// a torn or stale vector from the right one.
func vecFor(i int) Vector {
	r := rand.New(rand.NewPCG(uint64(i), 77))
	v := make(Vector, 16)
	for j := range v {
		v[j] = float32(r.NormFloat64())
	}
	return v
}

// deletedBy reports whether item j is deleted once the writer has finished
// transaction c. Transaction i deletes item i-20 when i is a multiple of 7.
func deletedBy(j, c int) bool {
	i := j + 20
	return i <= c && i%7 == 0
}

// childWriter writes until it is killed. Each transaction touches every
// handle at once: it puts a record with fields and a vector, links it to the
// previous record and to an anchor, sometimes deletes an old record (which
// takes its links away), and moves the counter.
func childWriter(e Engine, file string) int {
	db, err := e.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	f, err := db.Get("meta:counter")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := int(f["n"].(int64))
	for i := c + 1; ; i++ {
		err := db.Update(func(tx Handle) error {
			key := "item:" + strconv.Itoa(i)
			if err := tx.Put(key, Fields{"n": i, "label": "item " + strconv.Itoa(i), "vec": vecFor(i)}); err != nil {
				return err
			}
			if i > 1 {
				if err := tx.Link(key, "next", "item:"+strconv.Itoa(i-1)); err != nil {
					return err
				}
			}
			if err := tx.Link(key, "group", "anchor:"+strconv.Itoa(i%10)); err != nil {
				return err
			}
			if i%7 == 0 && i > 20 {
				if err := tx.Delete("item:" + strconv.Itoa(i-20)); err != nil {
					return err
				}
			}
			return tx.Put("meta:counter", Fields{"n": i})
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
}

// verifyWriterFile checks the file against what the writer must have done
// by transaction c, the counter: every record, field and vector, the number
// of links, and nothing more. 0.x's own version reads its link table in one
// query; this one asks Neighbours, record by record, for the links of every
// record a transaction after since could have changed, which are the ones
// from since-20 on. With since 0 it checks every link.
func verifyWriterFile(t *testing.T, e Engine, path string, since int) (counter, items, links int) {
	t.Helper()
	db, err := e.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rep := must[Report](t)(db.Check())
	if !rep.OK() {
		t.Fatalf("Check after a kill:\n%s", strings.Join(rep.Problems, "\n"))
	}
	f := must[Fields](t)(db.Get("meta:counter"))
	c := int(f["n"].(int64))

	got := map[int]bool{}
	if c > 0 { // before the first commit there's no item table to query
		rows, err := db.Query(`SELECT key, n, label, vec FROM item ORDER BY n`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var key, label string
			var n int
			var vec []byte
			if err := rows.Scan(&key, &n, &label, &vec); err != nil {
				t.Fatal(err)
			}
			if key != "item:"+strconv.Itoa(n) || label != "item "+strconv.Itoa(n) || string(vec) != string(vecFor(n).Bytes()) {
				t.Fatalf("record %s is torn: n=%d label=%q", key, n, label)
			}
			got[n] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	var want, have []string
	all := 0
	lo := max(1, since-20)
	for j := 1; j <= c; j++ {
		exists := !deletedBy(j, c)
		if exists != got[j] {
			t.Fatalf("counter %d: item:%d exists=%v, want %v", c, j, got[j], exists)
		}
		if !exists {
			continue
		}
		items++
		mine := []string{fmt.Sprintf("item:%d group anchor:%d", j, j%10)}
		if j > 1 && !deletedBy(j-1, c) {
			mine = append(mine, fmt.Sprintf("item:%d next item:%d", j, j-1))
		}
		all += len(mine)
		if j < lo {
			continue
		}
		want = append(want, mine...)
		for _, l := range must[[]Link](t)(db.Neighbours("item:"+strconv.Itoa(j), Out, "")) {
			have = append(have, l.From+" "+l.Type+" "+l.To)
		}
	}
	if len(got) != items {
		t.Fatalf("counter %d: %d items in the file, want %d (a transaction past the counter left a record)", c, len(got), items)
	}
	sort.Strings(want)
	sort.Strings(have)
	if strings.Join(want, "\n") != strings.Join(have, "\n") {
		t.Fatalf("counter %d: links from item:%d on don't match: %d in the file, %d expected", c, lo, len(have), len(want))
	}
	if rep.Links != all {
		t.Fatalf("counter %d: Check counts %d links, want %d", c, rep.Links, all)
	}
	// The vector handle agrees too: each record is its own nearest neighbour.
	if items > 0 {
		for _, j := range []int{c, c / 2, 1 + c/3} {
			if !got[j] {
				continue
			}
			hits := must[[]Hit](t)(db.Nearest("item", vecFor(j), 1, ""))
			if len(hits) != 1 || hits[0].Key != "item:"+strconv.Itoa(j) || hits[0].Distance > 1e-6 {
				t.Fatalf("Nearest to item:%d's vector gave %v", j, hits)
			}
		}
		// And so does a walk along the chain.
		if got[c] && c > 1 && !deletedBy(c-1, c) {
			steps := must[[]Step](t)(db.Walk("item:"+strconv.Itoa(c), Out, "next", 1))
			if len(steps) != 1 || steps[0].Key != "item:"+strconv.Itoa(c-1) {
				t.Fatalf("Walk from item:%d: %v", c, steps)
			}
		}
	}
	return c, items, all
}

// From TestKilledWritersNeverLeaveAMess: a writing process is killed with
// SIGKILL at random moments, and after every kill keys, rows, links and
// vectors must agree exactly with the last committed transaction.
func testKilledWritersNeverLeaveAMess(t *testing.T, e Engine) {
	rounds := 200
	if testing.Short() {
		rounds = 20
	}
	db, path := openTemp(t, e)
	ok(t, db.Update(func(tx Handle) error {
		for i := 0; i < 10; i++ {
			if err := tx.Put("anchor:"+strconv.Itoa(i), Fields{"name": "anchor " + strconv.Itoa(i)}); err != nil {
				return err
			}
		}
		return tx.Put("meta:counter", Fields{"n": 0})
	}))
	db.Close()
	r := rand.New(rand.NewPCG(11, 12))
	start := time.Now()
	last, empty := 0, 0
	for round := 0; round < rounds; round++ {
		cmd := startChild("writer", path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(40+r.IntN(160)) * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
			t.Fatalf("round %d: the writer stopped on its own before the kill: %v", round, err)
		}
		c, _, _ := verifyWriterFile(t, e, path, last)
		if c == last {
			empty++
		}
		last = c
	}
	c, items, links := verifyWriterFile(t, e, path, 0) // every link
	if c == 0 || empty > rounds/2 {
		t.Fatalf("the writer committed %d transactions, and %d of %d rounds committed nothing: the test didn't exercise anything", c, empty, rounds)
	}
	t.Logf("%d SIGKILLs at random moments in %s: %d transactions committed; after every kill every record, the link count and every link the round could have changed matched the last committed transaction, and at the end every link did (%d records, %d links)",
		rounds, time.Since(start).Round(time.Millisecond), c, items, links)
}

// childSharer does n transactions on its own records, linking each to the
// shared hub and to its own previous record, and reads between writes.
func childSharer(e Engine, file string, p, n int) int {
	db, err := e.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(uint64(p), 5))
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("share:%d-%d", p, i)
		err := db.Update(func(tx Handle) error {
			if err := tx.Put(key, Fields{"p": p, "i": i, "vec": randomVector(r, 8)}); err != nil {
				return err
			}
			if err := tx.Link(key, "hub", "hub:0"); err != nil {
				return err
			}
			if i > 0 {
				return tx.Link(key, "prev", fmt.Sprintf("share:%d-%d", p, i-1))
			}
			return nil
		})
		if err == nil && i%10 == 0 {
			_, err = db.Nearest("share", randomVector(r, 8), 5, "")
			if err == nil {
				_, err = db.Walk("hub:0", In, "hub", 1)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

// From TestProcessesShareAFile: four writing processes on one file at once,
// while this one reads.
func testProcessesShareAFile(t *testing.T, e Engine) {
	const procs = 4
	n := 300
	if testing.Short() {
		n = 50
	}
	db, path := openTemp(t, e)
	ok(t, db.Put("hub:0", nil))
	start := time.Now()
	var cmds []*exec.Cmd
	for p := 0; p < procs; p++ {
		cmd := startChild("sharer", path, envP+"="+strconv.Itoa(p), envN+"="+strconv.Itoa(n))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	done := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := db.Neighbours("hub:0", In, "hub"); err != nil {
				readErr <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	var waitErr error
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil && waitErr == nil {
			waitErr = err
		}
	}
	close(done)
	if err := <-readErr; err != nil {
		t.Fatalf("reading while they wrote: %v", err)
	}
	if waitErr != nil {
		t.Fatalf("a writer failed: %v", waitErr)
	}
	elapsed := time.Since(start)
	rep := checkOK(t, db)
	if want := procs*n + 1; rep.Records != want {
		t.Fatalf("%d records, want %d", rep.Records, want)
	}
	if want := procs*n + procs*(n-1); rep.Links != want {
		t.Fatalf("%d links, want %d", rep.Links, want)
	}
	for p := 0; p < procs; p++ {
		steps := must[[]Step](t)(db.Walk(fmt.Sprintf("share:%d-%d", p, n-1), Out, "prev", MaxDepth))
		if len(steps) != min(n-1, MaxDepth) {
			t.Fatalf("process %d: the prev chain has %d steps", p, len(steps))
		}
	}
	t.Logf("%d processes wrote %d transactions each to one file in %s while reading it; every record and link arrived and Check passed",
		procs, n, elapsed.Round(time.Millisecond))
}
