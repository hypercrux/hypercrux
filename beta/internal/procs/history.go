// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// history is the parent's account of a run: each commit a writer began and
// how it ended, and the commits the readers saw, in their places in the
// log. It checks what can be checked as the reports come in, and the rest
// against the file at the end (check).
//
// Reports from different children come through pipes of their own, so the
// parent can hear a reader's report of a commit before it hears the
// writer's report that began it, though the writer sent its report first.
// So whether a commit was begun is checked only at the end, once every
// pipe has been read to its end. A disagreement between two readers, or a
// commit out of order in one reader's reports, is a problem however the
// reports come, so those are checked at once.
//
// F9 changes this: a backup moved into place sets the log back to the
// backup's state, and the commits after it differ from the ones before.
// Then the readers' places in the log need an era each, the era of the
// file a reader read them from.
type history struct {
	begun   map[string]*commit // every commit a writer began, by what it holds
	writers map[int]*writerLog
	readers map[int]*readerLog
	held    map[uint64]sight // what the first reader to see each place in the log found there

	done, errors, underWay int // the commits that ended each way
	killedUnderWay         int // writers killed with a commit under way
	killedSeeing           int // readers killed once they had seen a commit
	seen                   int // the commits the readers saw, each once for each reader
}

// commit is a commit a writer began.
type commit struct {
	text   string // what it holds
	writer int
	n      int // its place among its writer's commits, from 1
	how    outcome
}

// outcome is how a commit ended.
type outcome uint8

const (
	underWay outcome = iota // its writer ended with it under way
	done                    // its writer saw it succeed
	failed                  // it failed, so its outcome is unknown
)

type writerLog struct {
	under *commit // the commit under way, or nil
	made  int     // the commits it began
}

type readerLog struct {
	seen uint64 // the last commit it saw
}

// sight is what a reader found at a place in the log.
type sight struct {
	text   string
	reader int
}

func newHistory() *history {
	return &history{begun: map[string]*commit{}, writers: map[int]*writerLog{}, readers: map[int]*readerLog{}, held: map[uint64]sight{}}
}

// writer returns writer id's log, made when it's the first report from it.
func (h *history) writer(id int) *writerLog {
	w := h.writers[id]
	if w == nil {
		w = &writerLog{}
		h.writers[id] = w
	}
	return w
}

// reader returns reader id's log, made when it's the first report from it.
func (h *history) reader(id int) *readerLog {
	r := h.readers[id]
	if r == nil {
		r = &readerLog{}
		h.readers[id] = r
	}
	return r
}

// begin notes that writer id began a commit holding text. A commit that
// holds what another holds is a fault of the workload's, since the checks
// tell commits apart by what they hold.
func (h *history) begin(id int, text string) *Failure {
	if c := h.begun[text]; c != nil {
		return &Failure{Problem: Failed, Reason: fmt.Sprintf("writer %d began a commit holding %s, which writer %d had begun already, and a workload's commits have to hold something different each",
			id, short(text), c.writer)}
	}
	w := h.writer(id)
	if w.under != nil {
		return &Failure{Problem: Failed, Reason: fmt.Sprintf("writer %d began a commit with another under way, which its Writer refuses", id)}
	}
	w.made++
	c := &commit{text: text, writer: id, n: w.made}
	h.begun[text] = c
	w.under = c
	return nil
}

// ended notes how writer id's commit under way ended: done or failed.
func (h *history) ended(id int, how outcome) *Failure {
	w := h.writer(id)
	if w.under == nil {
		return &Failure{Problem: Failed, Reason: fmt.Sprintf("writer %d ended a commit with none under way, which its Writer refuses", id)}
	}
	w.under.how = how
	w.under = nil
	if how == done {
		h.done++
	} else {
		h.errors++
	}
	return nil
}

// gone notes that the process id has ended, and whether the harness killed
// it. A writer's commit under way stays under way: its outcome is unknown.
func (h *history) gone(id int, killed bool) {
	if w := h.writers[id]; w != nil && w.under != nil {
		w.under = nil
		h.underWay++
		if killed {
			h.killedUnderWay++
		}
	}
	if r := h.readers[id]; r != nil && r.seen > 0 && killed {
		h.killedSeeing++
	}
}

// saw notes that reader id saw commit seq for the first time, holding
// text. Its reports come in order, so seq is the one after the last it
// saw. Another reader that saw the same place in the log has to have found
// the same there.
func (h *history) saw(id int, seq uint64, text string) *Failure {
	r := h.reader(id)
	if seq != r.seen+1 {
		return &Failure{Problem: OutOfOrder, Reason: fmt.Sprintf("reader %d reported commit %d where commit %d comes next", id, seq, r.seen+1)}
	}
	r.seen = seq
	h.seen++
	s, ok := h.held[seq]
	switch {
	case !ok:
		h.held[seq] = sight{text: text, reader: id}
	case s.text != text:
		return &Failure{Problem: Disagree, Reason: fmt.Sprintf("reader %d found commit %d holding %s, and reader %d found it holding %s",
			s.reader, seq, short(s.text), id, short(text))}
	}
	return nil
}

// The least a run has to do to mean something (Run).
const (
	leastDone        = 10 // commits that writers saw succeed
	leastWriterKills = 3  // writers killed with a commit under way
	leastReaderKills = 2  // readers killed once they had seen a commit
)

// enough reports whether the run has done the least it has to.
func (h *history) enough() bool {
	return h.done >= leastDone && h.killedUnderWay >= leastWriterKills && h.killedSeeing >= leastReaderKills
}

// lacks says what the run has done of the least it has to do.
func (h *history) lacks() string {
	return fmt.Sprintf("writers saw %s succeed, %s killed with a commit under way, and %s killed once they had seen a commit, where at least %d, %d and %d are wanted",
		counted(h.done, "commit"), counted(h.killedUnderWay, "writer"), counted(h.killedSeeing, "reader"), leastDone, leastWriterKills, leastReaderKills)
}

// counted is n things, such as "1 commit" or "2 commits".
func counted(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// check checks the run once every process has ended and every pipe has
// been read to its end, against final, the commits the file held at the
// end. It returns the first problem it finds, in this order:
//
//  1. NotBegun: a reader found a commit no writer began, or the file holds
//     one.
//  2. Lost: a reader saw a commit at a place the file at the end doesn't
//     reach. Disagree: the file holds another commit at a place than the
//     readers found there.
//  3. Twice: the file holds a commit in two places.
//  4. Missing: a commit a writer saw succeed isn't in the file.
//  5. OutOfOrder: the file holds a writer's commits in another order than
//     the writer made them.
//
// Together with the checks as the reports came in, that's every reader
// seeing every commit once and in order and nothing a writer didn't
// begin, the readers and the file agreeing on what each commit holds, a
// commit a writer saw succeed being in the file, and a commit under way
// when its writer was killed being there whole or not at all, since a
// commit read torn is one no writer began.
func (h *history) check(final []string) *Failure {
	places := slices.Sorted(maps.Keys(h.held))
	for _, seq := range places {
		if s := h.held[seq]; h.begun[s.text] == nil {
			return &Failure{Problem: NotBegun, Reason: fmt.Sprintf("reader %d found commit %d holding %s, which no writer began: a commit read torn, or made up", s.reader, seq, short(s.text))}
		}
	}
	for i, text := range final {
		if h.begun[text] == nil {
			return &Failure{Problem: NotBegun, Reason: fmt.Sprintf("the file at the end holds %s as commit %d, which no writer began: a commit left torn, or made up", short(text), i+1)}
		}
	}
	for _, seq := range places {
		s := h.held[seq]
		if seq > uint64(len(final)) {
			return &Failure{Problem: Lost, Reason: fmt.Sprintf("reader %d saw commit %d, holding %s, and the file at the end holds %s", s.reader, seq, short(s.text), counted(len(final), "commit"))}
		}
		if there := final[seq-1]; there != s.text {
			return &Failure{Problem: Disagree, Reason: fmt.Sprintf("reader %d found commit %d holding %s, and the file at the end holds %s there", s.reader, seq, short(s.text), short(there))}
		}
	}
	at := map[string]int{} // each commit's place in the file at the end
	for i, text := range final {
		if j, ok := at[text]; ok {
			return &Failure{Problem: Twice, Reason: fmt.Sprintf("the file at the end holds %s as commit %d and as commit %d", short(text), j, i+1)}
		}
		at[text] = i + 1
	}
	for _, c := range h.inOrder() {
		if _, ok := at[c.text]; c.how == done && !ok {
			return &Failure{Problem: Missing, Reason: fmt.Sprintf("writer %d saw its commit holding %s succeed, and the file at the end doesn't hold it", c.writer, short(c.text))}
		}
	}
	last := map[int]*commit{} // each writer's last commit, so far, in the file at the end
	for i, text := range final {
		c := h.begun[text]
		if p := last[c.writer]; p != nil && p.n > c.n {
			return &Failure{Problem: OutOfOrder, Reason: fmt.Sprintf("writer %d made its commit holding %s before the one holding %s, and the file at the end holds them as commits %d and %d",
				c.writer, short(c.text), short(p.text), i+1, at[p.text])}
		}
		last[c.writer] = c
	}
	return nil
}

// inOrder returns every commit begun, by writer, and then in the order
// each writer began them.
func (h *history) inOrder() []*commit {
	return slices.SortedFunc(maps.Values(h.begun), func(a, b *commit) int {
		return cmp.Or(cmp.Compare(a.writer, b.writer), cmp.Compare(a.n, b.n))
	})
}

// count fills in the report's counts of commits, with final the commits
// the file held at the end.
func (h *history) count(rep *Report, final []string) {
	rep.Begun, rep.Done, rep.Errors, rep.UnderWay = len(h.begun), h.done, h.errors, h.underWay
	rep.Seen, rep.Commits = h.seen, len(final)
	in := map[string]bool{}
	for _, text := range final {
		in[text] = true
	}
	rep.UnderWayIn = 0
	for _, c := range h.begun {
		if c.how == underWay && in[c.text] {
			rep.UnderWayIn++
		}
	}
}
