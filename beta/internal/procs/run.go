// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The environment variables a run reads.
const (
	envSeed = "HYPERCRUX_PROCS_SEED" // Options.Seed, when that's 0
	envTime = "HYPERCRUX_PROCS_TIME" // Options.Time, when that's 0
)

// How long a run waits for what only a fault, or a machine slower than any
// should be, would make it wait for. The tests of the harness shorten them
// (waits).
const (
	startWait   = 30 * time.Second // for a child to start its work
	killWait    = 30 * time.Second // for a child to end once it has had SIGKILL, and its reports to be read to their end
	finalWait   = 30 * time.Second // for Final to read the file
	catchUpWait = 30 * time.Second // for the readers to come to the end of the file once Final has read it
	idleWait    = time.Minute      // from the start, for the least work, or ten times Time when that's longer
)

// Run runs w's writers and readers side by side on the database at
// o.Path, kills each with SIGKILL at the end of a life of random length,
// and starts a new one in its place, after a gap of random length for a
// writer when o.WriterGap gives one, until o.Time is up, or o.Kills
// processes have been killed. Then it ends the run:
//
//  1. It kills every writer, and waits for each to end and for its reports
//     to be read to their end.
//  2. It reads the file with w.Final, while the readers go on reading. A
//     reader that was killed meanwhile gets a new one in its place, which
//     isn't killed.
//  3. It waits for every reader still running to come to the end of the
//     file Final read, and kills them.
//
// Then it checks the run, as history.check says: every reader saw every
// commit exactly once and in order, and nothing a writer didn't begin;
// every reader agrees on what each commit holds, and so does the file at
// the end; a commit its writer saw succeed is in the file at the end; and
// a commit under way when its writer was killed, or one that failed, is
// in the file whole or not at all. The children check their own reports
// as they go (Reader), and the parent checks the readers against each
// other as their reports come in. The run stops at the first problem.
//
// With o.Restores, the run also takes a backup of the database at random
// moments, in its own process, and moves it into place a while later, one
// backup at a time, while the run's time lasts (F9). Each restore begins an
// era of the log, and the checks above are made era by era: a reader sees
// every commit of each era it reads in order, in the file of that era, which
// begins with the backup's commits; and a commit its writer saw succeed is in
// the file at the end, unless a restore lost it, as the replaced file held
// it past the backup's commits. The end of the run waits for a backup or a
// restore under way, and the readers still running then have to come to the
// end of the file of the last era.
//
// The run goes on past Time until it has done enough to mean something:
// 10 commits that writers saw succeed, 3 writers killed with a commit under
// way, 2 readers killed once they had seen a commit, 2 restores when
// o.Restores asks for them, and the counts in o.Least. A slow machine makes
// fewer commits in the time, so the run waits for them, and fails with the
// problem Idle only after a minute without them, or ten times Time when
// that's longer.
//
// Run returns a *Failure for the first problem it finds, and a plain
// error when the workload or the options can't be used, or a child can't
// be started. Run is for tests: the children are copies of the test
// binary, which the test's TestMain sends to their roles through Main.
func Run(w Workload, o Options) (Report, error) {
	r, err := newRun(w, o)
	if err != nil {
		return Report{}, err
	}
	return r.run()
}

// run is one call of Run. Its loop owns every field below, and the
// goroutines beside it only send it events.
type run struct {
	w    Workload
	o    Options
	exe  string // the test binary
	seed uint64
	rng  *rand.Rand
	h    *history

	began time.Time
	waits waits

	events chan event
	quit   chan struct{} // closed once the loop has ended, so nothing waits to send it an event

	phase  phase
	kids   map[int]*kid // the processes started, until each has ended and its reports have been read
	slots  []*kid       // the process in each slot, the writers' first, or nil
	nextID int
	kills  int // the kills made at the end of a life, toward Options.Kills

	final     []string  // the commits the file held at the end
	finalAt   time.Time // when Final started reading the file, and then when it had read it
	finalBusy bool      // Final is reading the file

	restoring bool      // the workload's Backup or Restore is running
	restoreAt time.Time // when it started
	backedUp  bool      // a backup waits to be moved into place
	backup    []string  // the commits it holds

	rep  Report
	fail *Failure
	err  error // a plain error that stopped the run
}

// waits is how long a run waits for each of the things it waits for.
type waits struct {
	start, kill, final, catchUp time.Duration // startWait, killWait, finalWait and catchUpWait
	idle                        time.Duration // from the start, for the least work
}

// The phases of a run.
type phase uint8

const (
	running    phase = iota // processes are killed at the end of their lives, and new ones started in their place
	ending                  // every writer has been killed, and the run waits for them to end
	finalRead               // Final is reading the file
	catchingUp              // the run waits for the readers to come to the end of the file
	stopping                // every process has been killed, and the run waits for them to end
)

// kid is a child process.
type kid struct {
	id    int
	role  string
	slot  int
	at    time.Time     // when it was started
	life  time.Duration // from its start report to its kill
	cmd   *exec.Cmd
	stdin *os.File // the parent's end of its standard input, which it watches (watchParent)
	out   *tail    // the end of what it wrote to its standard output and error
	timer *time.Timer

	started bool // its start report has come
	killed  bool // the harness has sent it SIGKILL
	exited  bool // Wait has returned
	drained bool // its reports have all been read
	ending  bool // its work has returned nil, and it's ending by itself
	gone    bool // it hadn't ended long after SIGKILL, and the run has stopped waiting for it
	state   *os.ProcessState
	killAt  time.Time // when it had SIGKILL
}

func (k *kid) String() string { return k.role + " " + strconv.Itoa(k.id) }

// event is what the goroutines beside the loop send it.
type event struct {
	kind  eventKind
	kid   *kid
	line  string           // evLine's
	state *os.ProcessState // evExited's
	final []string         // evFinal's
	err   error            // evFinal's
	slot  int              // evStart's
}

type eventKind uint8

const (
	evLine     eventKind = iota // a report from kid
	evCut                       // a report from kid that a kill cut short
	evDrained                   // the end of kid's reports
	evExited                    // kid has ended
	evKill                      // kid's life is over
	evFinal                     // Final has read the file
	evStart                     // a writer's slot has been empty for its gap
	evBackup                    // the time has come to take a backup
	evBackedUp                  // Backup has returned, with the commits in final and its error
	evRestore                   // the time has come to move the backup into place
	evRestored                  // Restore has returned, with the commits the replaced file held in final and its error
)

func newRun(w Workload, o Options) (*run, error) {
	switch {
	case w.Name == "" || w.Write == nil || w.Read == nil || w.Final == nil:
		return nil, errors.New("procs: a Workload needs a Name, a Write, a Read and a Final")
	case !filepath.IsAbs(o.Path):
		return nil, fmt.Errorf("procs: Options.Path is %q, where an absolute path is wanted", o.Path)
	case o.Writers < 0 || o.Readers < 0 || o.Time < 0 || o.Kills < 0:
		return nil, errors.New("procs: Options with a number below 0")
	}
	if o.Writers == 0 {
		o.Writers = 2
	}
	if o.Readers == 0 {
		o.Readers = 2
	}
	if s := os.Getenv(envTime); o.Time == 0 && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("procs: %s is %q, where a Go duration above 0 is wanted, such as 10m", envTime, s)
		}
		o.Time = d
	}
	if o.Time == 0 {
		o.Time = 2 * time.Second
		if shortTests() {
			o.Time = 500 * time.Millisecond
		}
	}
	for _, s := range []*Span{&o.WriterLife, &o.ReaderLife} {
		if *s == (Span{}) {
			*s = Span{Min: 10 * time.Millisecond, Max: 200 * time.Millisecond}
		}
		if s.Min < 0 || s.Max < s.Min || s.Max == 0 {
			return nil, fmt.Errorf("procs: a life of %v to %v, where a span from 0 or more to more than 0 is wanted", s.Min, s.Max)
		}
	}
	if g := o.WriterGap; g != (Span{}) && (g.Min < 0 || g.Max < g.Min || g.Max == 0) {
		return nil, fmt.Errorf("procs: a writer's gap of %v to %v, where none, or a span from 0 or more to more than 0, is wanted", g.Min, g.Max)
	}
	if g := o.Restores; g != (Span{}) {
		switch {
		case g.Min < 0 || g.Max < g.Min || g.Max == 0:
			return nil, fmt.Errorf("procs: restores %v to %v apart, where none, or a span from 0 or more to more than 0, is wanted", g.Min, g.Max)
		case w.Backup == nil || w.Restore == nil:
			return nil, errors.New("procs: a run with restores needs a Workload with a Backup and a Restore")
		}
	}
	for what, n := range o.Least {
		if what == "" || n < 0 {
			return nil, fmt.Errorf("procs: Options.Least wants %d counts of %q, where a name and a number from 0 are wanted", n, what)
		}
	}
	seed := o.Seed
	if s := os.Getenv(envSeed); seed == 0 && s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("procs: %s is %q, where a whole number from 1 is wanted", envSeed, s)
		}
		seed = n
	}
	for seed == 0 {
		seed = rand.Uint64()
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("procs: finding the test binary: %w", err)
	}
	h := newHistory()
	h.least = o.Least
	if o.Restores != (Span{}) {
		h.restores = leastRestores
	}
	return &run{
		w: w, o: o, exe: exe, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x70726f6373)), h: h,
		waits:  waits{start: startWait, kill: killWait, final: finalWait, catchUp: catchUpWait, idle: max(idleWait, 10*o.Time)},
		events: make(chan event, 256), quit: make(chan struct{}),
		kids: map[int]*kid{}, slots: make([]*kid, o.Writers+o.Readers),
	}, nil
}

// shortTests reports whether the tests run with -short.
func shortTests() bool { return testing.Testing() && flag.Parsed() && testing.Short() }

// run starts a process in every slot, then runs the loop until every
// process has ended and its reports have been read, and checks the run.
func (r *run) run() (Report, error) {
	r.began = time.Now()
	for slot := range r.slots {
		if r.phase == running {
			r.start(slot)
		}
	}
	if r.o.Restores != (Span{}) {
		r.after(r.o.Restores, evBackup)
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for r.phase != stopping || len(r.kids) > 0 || r.finalBusy || r.restoring {
		select {
		case ev := <-r.events:
			r.handle(ev)
		case <-tick.C:
			r.tick()
		}
	}
	close(r.quit)
	os.Remove(r.backupPath()) // a backup still waiting for its restore
	r.rep.Seed, r.rep.Took = r.seed, time.Since(r.began)
	if r.fail == nil && r.err == nil {
		r.fail = r.h.check(r.final)
	}
	r.h.count(&r.rep, r.final)
	switch {
	case r.err != nil:
		return r.rep, r.err
	case r.fail != nil:
		r.fail.Report = r.rep
		return r.rep, r.fail
	}
	return r.rep, nil
}

// send hands the loop an event, unless the loop has ended.
func (r *run) send(ev event) {
	select {
	case r.events <- ev:
	case <-r.quit:
	}
}

func (r *run) handle(ev event) {
	k := ev.kid
	if k != nil && k.gone {
		return
	}
	switch ev.kind {
	case evLine:
		r.report(k, ev.line)
	case evCut:
		r.rep.Cut++
	case evDrained:
		k.drained = true
		r.ended(k)
	case evExited:
		k.exited, k.state = true, ev.state
		r.ended(k)
	case evKill:
		if r.phase == running && !k.killed && !k.exited {
			r.kill(k)
			r.kills++
		}
	case evFinal:
		if r.finalBusy {
			r.finalBusy = false
			r.read(ev.final, ev.err)
		}
	case evStart:
		if r.phase == running && r.slots[ev.slot] == nil {
			r.start(ev.slot)
		}
	case evBackup:
		if r.phase == running && !r.restoring && !r.backedUp {
			r.restoring, r.restoreAt = true, time.Now()
			era := r.h.last() + 1
			go func() {
				commits, err := r.w.Backup(r.o.Path, r.backupPath(), era)
				r.send(event{kind: evBackedUp, final: commits, err: err})
			}()
		}
	case evBackedUp:
		if !r.restoring {
			return // the run stopped waiting for it
		}
		r.restoring = false
		switch {
		case ev.err != nil:
			r.found(Failed, "Backup failed: %v", ev.err)
		case r.phase == running:
			r.backup, r.backedUp = ev.final, true
			r.after(r.o.Restores, evRestore)
		}
	case evRestore:
		if r.phase == running && r.backedUp && !r.restoring {
			r.restoring, r.restoreAt = true, time.Now()
			// The new era begins before the backup goes into place, so a
			// reader's report of it, which can come before Restore returns,
			// always finds it begun.
			r.h.restoring(r.backup)
			r.backup, r.backedUp = nil, false
			go func() {
				gone, err := r.w.Restore(r.backupPath(), r.o.Path)
				r.send(event{kind: evRestored, final: gone, err: err})
			}()
		}
	case evRestored:
		if !r.restoring {
			return
		}
		r.restoring = false
		if ev.err != nil {
			r.found(Failed, "Restore failed: %v", ev.err)
			return
		}
		r.h.restored(ev.final)
		if r.phase == running {
			r.after(r.o.Restores, evBackup)
		}
	}
}

// after sends the loop an event of the kind kind once a wait drawn from
// span has gone by, unless the loop has ended by then.
func (r *run) after(span Span, kind eventKind) {
	d := span.Min + time.Duration(r.rng.Int64N(int64(span.Max-span.Min)+1))
	time.AfterFunc(d, func() { r.send(event{kind: kind}) })
}

// backupPath is where a backup waits for its restore, beside the database.
func (r *run) backupPath() string { return r.o.Path + ".backup" }

// report deals with one report from k.
func (r *run) report(k *kid, line string) {
	rp, err := parseReport(line)
	if err != nil {
		r.found(Failed, "%v sent a report the harness can't read, %q: %v", k, line, err)
		return
	}
	writer := k.role == roleWriter
	switch rp.kind {
	case kindStart:
		if k.started {
			r.found(Failed, "%v started its work twice", k)
			return
		}
		k.started = true
		if r.phase == running {
			k.timer = time.AfterFunc(k.life, func() { r.send(event{kind: evKill, kid: k}) })
		}
	case kindBegin, kindDone, kindFailed:
		switch {
		case !writer:
			r.found(Failed, "%v sent a writer's report, %q", k, line)
		case rp.kind == kindBegin:
			r.problem(r.h.begin(k.id, rp.text))
		case rp.kind == kindDone:
			r.problem(r.h.ended(k.id, done))
		default:
			r.problem(r.h.ended(k.id, failed))
		}
	case kindReset, kindSaw, kindEra:
		switch {
		case writer:
			r.found(Failed, "%v sent a reader's report, %q", k, line)
		case rp.kind == kindReset:
			r.rep.Reads++
		case rp.kind == kindEra:
			r.rep.Reads++
			r.problem(r.h.moved(k.id, rp.era))
			if r.phase == catchingUp && r.caughtUp() {
				r.stop()
			}
		default:
			r.problem(r.h.saw(k.id, rp.seq, rp.text))
			if r.phase == catchingUp && r.caughtUp() {
				r.stop()
			}
		}
	case kindProblem:
		r.found(rp.problem, "%s", rp.text)
	case kindFail:
		r.found(Failed, "%v failed: %s%s", k, rp.text, k.out)
	case kindEnd:
		k.ending = true
		if k.timer != nil {
			k.timer.Stop()
		}
	case kindCount:
		r.h.tally(rp.text)
	}
}

// tick looks at the clock: for the end of the run's time, and for waits
// that have gone on too long.
func (r *run) tick() {
	for _, k := range r.kids {
		if k.killed && time.Since(k.killAt) > r.waits.kill {
			// A process stuck in the kernel, or one that handed its
			// reports' pipe on to another process.
			r.found(Failed, "%v hadn't ended, or its reports' pipe was still open, %v after SIGKILL%s", k, r.waits.kill, k.out)
			k.gone = true
			delete(r.kids, k.id)
			k.stdin.Close()
		}
	}
	if r.finalBusy && time.Since(r.finalAt) > r.waits.final {
		r.found(Failed, "Final hadn't returned %v after it started reading the file", r.waits.final)
		r.finalBusy = false // and what it returns later is dropped
	}
	if r.restoring && time.Since(r.restoreAt) > r.waits.final {
		r.found(Failed, "the workload's Backup or Restore hadn't returned %v after it started", r.waits.final)
		r.restoring = false // and what it returns later is dropped
	}
	switch r.phase {
	case running:
		for _, k := range r.kids {
			if !k.started && time.Since(k.at) > r.waits.start {
				r.found(Failed, "%v didn't start its work within %v of being started%s", k, r.waits.start, k.out)
				return
			}
		}
		took := time.Since(r.began)
		up := took >= r.o.Time || (r.o.Kills > 0 && r.kills >= r.o.Kills)
		switch {
		case up && r.h.enough():
			r.end()
		case took >= r.waits.idle:
			r.found(Idle, "in %v, %s", took.Round(time.Millisecond), r.h.lacks())
		}
	case ending:
		for _, k := range r.kids {
			if k.role == roleWriter {
				return
			}
		}
		if r.restoring {
			return // Final reads the file once the backup or the restore under way is done
		}
		r.phase, r.finalBusy, r.finalAt = finalRead, true, time.Now()
		go func() {
			final, err := r.w.Final(r.o.Path)
			r.send(event{kind: evFinal, final: final, err: err})
		}()
	case catchingUp:
		if time.Since(r.finalAt) > r.waits.catchUp {
			r.found(Behind, "%s, %v after Final had read the file", r.behind(), r.waits.catchUp)
		}
	}
}

// end ends the time of kills: every writer is killed, and the readers live
// on.
func (r *run) end() {
	r.phase = ending
	for _, k := range r.kids {
		if k.timer != nil {
			k.timer.Stop()
		}
		if k.role == roleWriter {
			r.kill(k)
		}
	}
}

// read takes what Final read, once every writer had ended.
func (r *run) read(final []string, err error) {
	switch {
	case r.phase != finalRead:
		return // the run is stopping for a problem
	case err != nil:
		r.found(Failed, "Final couldn't read the database once every writer had ended: %v", err)
		return
	}
	r.final, r.finalAt, r.phase = final, time.Now(), catchingUp
	if r.caughtUp() {
		r.stop()
	}
}

// caughtUp reports whether every reader still running has seen every
// commit the file held at the end, in the last era.
func (r *run) caughtUp() bool {
	for _, k := range r.kids {
		if rd := r.h.readers[k.id]; k.role == roleReader && (rd == nil || rd.era != r.h.last() || rd.seen < uint64(len(r.final))) {
			return false
		}
	}
	return true
}

// behind names the readers that haven't seen every commit the file held
// at the end, in the last era.
func (r *run) behind() string {
	var b []string
	for _, k := range r.kids {
		var seen uint64
		era := 0
		if rd := r.h.readers[k.id]; rd != nil {
			seen, era = rd.seen, rd.era
		}
		switch {
		case k.role != roleReader || era == r.h.last() && seen >= uint64(len(r.final)):
		case r.h.last() == 0:
			b = append(b, fmt.Sprintf("%v had seen %d", k, seen))
		default:
			b = append(b, fmt.Sprintf("%v had seen %d of era %d", k, seen, era))
		}
	}
	slices.Sort(b)
	end := "the file held at the end"
	if r.h.last() > 0 {
		end = fmt.Sprintf("the file of era %d held at the end", r.h.last())
	}
	return fmt.Sprintf("of the %d commits %s, %s", len(r.final), end, strings.Join(b, ", "))
}

// stop kills every process, and the run ends once they've all ended.
func (r *run) stop() {
	r.phase = stopping
	for _, k := range r.kids {
		r.kill(k)
	}
}

// kill sends k SIGKILL, unless it has had it already or has ended.
func (r *run) kill(k *kid) {
	if k.killed || k.exited {
		return
	}
	k.killed, k.killAt = true, time.Now()
	if k.timer != nil {
		k.timer.Stop()
	}
	k.cmd.Process.Signal(syscall.SIGKILL)
}

// found stops the run for a problem, unless it has found one already.
func (r *run) found(p Problem, format string, args ...any) {
	r.problem(&Failure{Problem: p, Reason: fmt.Sprintf(format, args...)})
}

// problem stops the run for f, unless f is nil, or the run has found a
// problem already.
func (r *run) problem(f *Failure) {
	if f == nil {
		return
	}
	if r.fail == nil {
		r.fail = f
	}
	r.stop()
}

// start starts a new process in the slot slot.
func (r *run) start(slot int) {
	role, life := roleWriter, r.o.WriterLife
	if slot >= r.o.Writers {
		role, life = roleReader, r.o.ReaderLife
	}
	r.nextID++
	k := &kid{id: r.nextID, role: role, slot: slot, at: time.Now(), out: &tail{}}
	k.life = life.Min + time.Duration(r.rng.Int64N(int64(life.Max-life.Min)+1))
	reports, err := r.command(k, r.rng.Uint64())
	if err != nil {
		if r.err == nil {
			r.err = err
		}
		r.stop()
		return
	}
	r.kids[k.id], r.slots[slot] = k, k
	if role == roleWriter {
		r.rep.Writers++
	} else {
		r.rep.Readers++
	}
	go r.reports(k, reports)
	go func() {
		k.cmd.Wait()
		r.send(event{kind: evExited, kid: k, state: k.cmd.ProcessState})
	}()
}

// command starts the process k, a copy of the test binary sent to its
// role, with seed for its Rand. It sets k.cmd and k.stdin, and returns the
// read end of k's reports.
func (r *run) command(k *kid, seed uint64) (*os.File, error) {
	reports, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	in, stdin, err := os.Pipe()
	if err != nil {
		reports.Close()
		w.Close()
		return nil, err
	}
	cmd := exec.Command(r.exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), envRole+"="+k.role, envWorkload+"="+r.w.Name, envPath+"="+r.o.Path,
		envID+"="+strconv.Itoa(k.id), envChildSeed+"="+strconv.FormatUint(seed, 10), "GORACE="+childRace(os.Getenv("GORACE")))
	cmd.Stdin = in
	cmd.Stdout, cmd.Stderr = k.out, k.out
	cmd.ExtraFiles = []*os.File{w}
	err = cmd.Start()
	w.Close()
	in.Close()
	if err != nil {
		reports.Close()
		stdin.Close()
		return nil, fmt.Errorf("procs: starting %v: %w", k, err)
	}
	k.cmd, k.stdin = cmd, stdin
	return reports, nil
}

// childRace is GORACE for a child, from the test's own. In a race
// detector's build, it has the detector end the child at the first race it
// finds, so the run fails with the race's report, which a kill would
// otherwise hide in the child's output. And it has the child end at once
// when it ends, where the detector would wait a second first, by default,
// and a kill could land in that second and hide that the child ended by
// itself. An option the test's GORACE sets stays as it is. Other builds
// take no notice of GORACE.
func childRace(gorace string) string {
	for _, opt := range []string{"halt_on_error=1", "atexit_sleep_ms=0"} {
		name, _, _ := strings.Cut(opt, "=")
		if !strings.Contains(gorace, name+"=") {
			gorace = strings.TrimSpace(gorace + " " + opt)
		}
	}
	return gorace
}

// reports reads k's reports from f until k has ended and f has been read
// to its end.
func (r *run) reports(k *kid, f *os.File) {
	lines(f, func(line string) { r.send(event{kind: evLine, kid: k, line: line}) }, func() { r.send(event{kind: evCut, kid: k}) })
	f.Close()
	r.send(event{kind: evDrained, kid: k})
}

// ended deals with k once it has ended and its reports have all been
// read: it notes how k ended, and starts a new process in its place while
// the run goes on, or a new reader until the readers are to catch up.
func (r *run) ended(k *kid) {
	if !k.exited || !k.drained {
		return
	}
	delete(r.kids, k.id)
	if r.slots[k.slot] == k {
		r.slots[k.slot] = nil
	}
	k.stdin.Close()
	if k.timer != nil {
		k.timer.Stop()
	}
	ws, _ := k.state.Sys().(syscall.WaitStatus)
	switch {
	case k.killed && ws.Signaled() && ws.Signal() == syscall.SIGKILL:
		if k.role == roleWriter {
			r.rep.WritersKilled++
		} else {
			r.rep.ReadersKilled++
		}
	case r.fail != nil || r.err != nil:
		// It ended for the problem the run has found, or the run is
		// stopping for one.
	case !k.started:
		r.found(Failed, "%v ended before it started its work, with %v, so the test binary's TestMain doesn't call procs.Main%s", k, k.state, k.out)
	case k.ending && ws.Exited() && ws.ExitStatus() == 0:
		// Its work returned nil.
	default:
		r.found(Failed, "%v ended by itself, with %v%s", k, k.state, k.out)
	}
	r.h.gone(k.id, k.killed)
	switch {
	case k.role == roleWriter && r.phase == running && r.o.WriterGap != (Span{}):
		// The slot stays empty for a while, and a new writer starts there
		// then, unless the run has moved on.
		g := r.o.WriterGap
		slot := k.slot
		time.AfterFunc(g.Min+time.Duration(r.rng.Int64N(int64(g.Max-g.Min)+1)), func() { r.send(event{kind: evStart, slot: slot}) })
	case r.phase == running || (k.role == roleReader && r.phase < catchingUp):
		r.start(k.slot)
	}
}

// tail keeps the last 4 KiB written to it: the end of what a child wrote
// to its standard output and error, for a failure's reason.
type tail struct {
	mu sync.Mutex
	b  []byte
}

const tailSize = 4 << 10

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailSize {
		t.b = append(t.b[:0], t.b[len(t.b)-tailSize:]...)
	}
	return len(p), nil
}

// String is what the tail holds, made to end a failure's reason: nothing
// when the child wrote nothing.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.b) == 0 {
		return ""
	}
	return ". It wrote:\n" + string(t.b[max(0, len(t.b)-tailSize):])
}
