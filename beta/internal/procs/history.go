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
// A backup moved into place (Options.Restores) sets the log back to the
// backup's commits, and the commits after them differ from the ones the
// replaced file held after them. So the log has eras (F9): era 0, the
// database before any restore, and one for each restore after it. Each
// reader reads one era at a time, the era of the file it says it reads, and
// its places in the log, and the first sight of each place, are its era's.
// The run knows what each era's backup held, and what the file a restore
// replaced held then, so each era is checked against its own file, and a
// commit that succeeded is either in the file at the end or among those a
// restore lost.
type history struct {
	begun   map[string]*commit // every commit a writer began, by what it holds
	writers map[int]*writerLog
	readers map[int]*readerLog
	eras    []*era // the log's eras, from 0

	done, errors, underWay int // the commits that ended each way
	killedUnderWay         int // writers killed with a commit under way
	killedSeeing           int // readers killed once they had seen a commit
	seen                   int // the commits the readers saw, each once for each reader

	counts   map[string]int // what the workload's processes counted, by name
	least    map[string]int // Options.Least
	restores int            // the least restores the run makes: 2 with Options.Restores, or 0
}

// era is one era of the log: the file at the path from one restore to the
// next, with the compactions that moved its commits from file to file.
type era struct {
	held   map[uint64]sight // what the first reader to see each place in the era found there
	backup []string         // the commits of the backup that began it, or nil for era 0
	gone   []string         // the commits its file held when the next era's restore replaced it, or nil while it lasts
}

func newEra(backup []string) *era { return &era{held: map[uint64]sight{}, backup: backup} }

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
	era  int    // the era it reads
	seen uint64 // the last commit it saw in it
}

// sight is what a reader found at a place in the log.
type sight struct {
	text   string
	reader int
}

func newHistory() *history {
	return &history{begun: map[string]*commit{}, writers: map[int]*writerLog{}, readers: map[int]*readerLog{},
		eras: []*era{newEra(nil)}, counts: map[string]int{}}
}

// last is the era of the file at the path now.
func (h *history) last() int { return len(h.eras) - 1 }

// at names the place seq in era e, as "commit 5", or "commit 5 of era 2"
// once a backup has been moved into place.
func (h *history) at(seq uint64, e int) string {
	if h.last() == 0 {
		return fmt.Sprintf("commit %d", seq)
	}
	return fmt.Sprintf("commit %d of era %d", seq, e)
}

// tally notes one event that a process counted under the name what.
func (h *history) tally(what string) { h.counts[what]++ }

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
// text, in the era it reads. Its reports come in order, so seq is the one
// after the last it saw. Another reader that saw the same place in the era
// has to have found the same there.
func (h *history) saw(id int, seq uint64, text string) *Failure {
	r := h.reader(id)
	if seq != r.seen+1 {
		return &Failure{Problem: OutOfOrder, Reason: fmt.Sprintf("reader %d reported %s where commit %d comes next", id, h.at(seq, r.era), r.seen+1)}
	}
	r.seen = seq
	h.seen++
	e := h.eras[r.era]
	s, ok := e.held[seq]
	switch {
	case !ok:
		e.held[seq] = sight{text: text, reader: id}
	case s.text != text:
		return &Failure{Problem: Disagree, Reason: fmt.Sprintf("reader %d found %s holding %s, and reader %d found it holding %s",
			s.reader, h.at(seq, r.era), short(s.text), id, short(text))}
	}
	return nil
}

// moved notes that reader id is starting again from the first commit, in
// the file of era n, which is later than the era it read. A restore has to
// have begun that era already, since the run notes each restore before it
// moves the backup into place, so before any reader can read its file.
func (h *history) moved(id, n int) *Failure {
	r := h.reader(id)
	switch {
	case n <= r.era:
		return &Failure{Problem: OutOfOrder, Reason: fmt.Sprintf("reader %d reported the file of era %d after reading one of era %d", id, n, r.era)}
	case n > h.last():
		return &Failure{Problem: Failed, Reason: fmt.Sprintf("reader %d read the file of era %d, where %d backups have been moved into place, so the workload marks its backups wrongly", id, n, h.last())}
	}
	r.era, r.seen = n, 0
	return nil
}

// restoring notes that a restore is starting, which moves into place the
// backup that holds backup: a new era begins.
func (h *history) restoring(backup []string) { h.eras = append(h.eras, newEra(backup)) }

// restored notes that the restore has been made: the file it replaced held
// gone, which is the file of the era before the last.
func (h *history) restored(gone []string) { h.eras[h.last()-1].gone = gone }

// The least a run has to do to mean something (Run).
const (
	leastDone        = 10 // commits that writers saw succeed
	leastWriterKills = 3  // writers killed with a commit under way
	leastReaderKills = 2  // readers killed once they had seen a commit
	leastRestores    = 2  // backups moved into place, in a run with Options.Restores
)

// enough reports whether the run has done the least it has to, the
// workload's own counts in Options.Least among it.
func (h *history) enough() bool {
	for what, n := range h.least {
		if h.counts[what] < n {
			return false
		}
	}
	return h.done >= leastDone && h.killedUnderWay >= leastWriterKills && h.killedSeeing >= leastReaderKills && h.last() >= h.restores
}

// lacks says what the run has done of the least it has to do.
func (h *history) lacks() string {
	s := fmt.Sprintf("writers saw %s succeed, %s killed with a commit under way, and %s killed once they had seen a commit, where at least %d, %d and %d are wanted",
		counted(h.done, "commit"), counted(h.killedUnderWay, "writer"), counted(h.killedSeeing, "reader"), leastDone, leastWriterKills, leastReaderKills)
	if h.restores > 0 {
		s += fmt.Sprintf("; %s moved into place, where at least %d are wanted", counted(h.last(), "backup"), h.restores)
	}
	for _, what := range slices.Sorted(maps.Keys(h.least)) {
		if h.counts[what] < h.least[what] {
			s += fmt.Sprintf("; the workload counted %q %s, short of the %d wanted", what, counted(h.counts[what], "time"), h.least[what])
		}
	}
	return s
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
// end. Each era's file is the one a restore replaced, as it was then, or for
// the last era the file at the end. It returns the first problem it finds,
// in this order:
//
//  1. NotBegun: a reader found a commit no writer began, or a file holds
//     one, a backup among them.
//  2. Lost: a reader saw a commit at a place its era's file doesn't reach.
//     Disagree: its era's file holds another commit at a place than the
//     readers found there. The same for the commits of the backup that began
//     the era, which the era's file begins with, and the next era's backup,
//     which is the start of the era's file.
//  3. Twice: a file holds a commit in two places.
//  4. Missing: a commit a writer saw succeed isn't in the file at the end,
//     and isn't one a restore lost: one that the replaced file held past
//     the backup that took its place.
//  5. OutOfOrder: a file holds a writer's commits in another order than the
//     writer made them.
//
// Together with the checks as the reports came in, that's every reader
// seeing every commit of each era once and in order and nothing a writer
// didn't begin, the readers and the files agreeing on what each commit
// holds, a commit a writer saw succeed lasting until a restore takes it
// away, and a commit under way when its writer was killed being there whole
// or not at all, since a commit read torn is one no writer began.
func (h *history) check(final []string) *Failure {
	file := func(e int) []string {
		if e == h.last() {
			return final
		}
		return h.eras[e].gone
	}
	for e, er := range h.eras {
		for _, seq := range slices.Sorted(maps.Keys(er.held)) {
			if s := er.held[seq]; h.begun[s.text] == nil {
				return &Failure{Problem: NotBegun, Reason: fmt.Sprintf("reader %d found %s holding %s, which no writer began: a commit read torn, or made up", s.reader, h.at(seq, e), short(s.text))}
			}
		}
		for _, f := range []struct {
			what    string
			commits []string
		}{{"the backup that began era " + fmt.Sprint(e), er.backup}, {h.fileName(e), file(e)}} {
			for i, text := range f.commits {
				if h.begun[text] == nil {
					return &Failure{Problem: NotBegun, Reason: fmt.Sprintf("%s holds %s as commit %d, which no writer began: a commit left torn, or made up", f.what, short(text), i+1)}
				}
			}
		}
	}
	for e, er := range h.eras {
		if e < h.last() && plant == "procs/replaced-eras-unchecked" {
			continue
		}
		there := file(e)
		for _, seq := range slices.Sorted(maps.Keys(er.held)) {
			s := er.held[seq]
			if seq > uint64(len(there)) {
				return &Failure{Problem: Lost, Reason: fmt.Sprintf("reader %d saw %s, holding %s, and %s holds %s", s.reader, h.at(seq, e), short(s.text), h.fileName(e), counted(len(there), "commit"))}
			}
			if t := there[seq-1]; t != s.text {
				return &Failure{Problem: Disagree, Reason: fmt.Sprintf("reader %d found %s holding %s, and %s holds %s there", s.reader, h.at(seq, e), short(s.text), h.fileName(e), short(t))}
			}
		}
		if f := h.starts(er.backup, there, "the backup that began era "+fmt.Sprint(e), h.fileName(e)); f != nil {
			return f
		}
		if e < h.last() {
			if f := h.starts(h.eras[e+1].backup, there, "the backup that began era "+fmt.Sprint(e+1), h.fileName(e)); f != nil {
				return f
			}
		}
	}
	places := make([]map[string]int, len(h.eras)) // each commit's place in each era's file
	for e := range h.eras {
		places[e] = map[string]int{}
		for i, text := range file(e) {
			if j, ok := places[e][text]; ok {
				return &Failure{Problem: Twice, Reason: fmt.Sprintf("%s holds %s as commit %d and as commit %d", h.fileName(e), short(text), j, i+1)}
			}
			places[e][text] = i + 1
		}
	}
	at := places[h.last()]    // in the file at the end
	lost := map[string]bool{} // the commits the restores lost
	for e := range h.last() {
		for _, text := range h.eras[e].gone[len(h.eras[e+1].backup):] {
			lost[text] = true
		}
	}
	for _, c := range h.inOrder() {
		if _, ok := at[c.text]; c.how == done && !ok && !lost[c.text] {
			return &Failure{Problem: Missing, Reason: fmt.Sprintf("writer %d saw its commit holding %s succeed, and the file at the end doesn't hold it", c.writer, short(c.text))}
		}
	}
	for e := range h.eras {
		last := map[int]*commit{} // each writer's last commit, so far, in the file
		for i, text := range file(e) {
			c := h.begun[text]
			if p := last[c.writer]; p != nil && p.n > c.n {
				return &Failure{Problem: OutOfOrder, Reason: fmt.Sprintf("writer %d made its commit holding %s before the one holding %s, and %s holds them as commits %d and %d",
					c.writer, short(c.text), short(p.text), h.fileName(e), i+1, places[e][p.text])}
			}
			last[c.writer] = c
		}
	}
	return nil
}

// fileName names the file of era e, for a reason: the file at the end, or
// the file a restore replaced.
func (h *history) fileName(e int) string {
	if e == h.last() {
		return "the file at the end"
	}
	return fmt.Sprintf("the file of era %d, when a restore replaced it,", e)
}

// starts checks that a file, there, starts with the commits of a backup,
// which it was copied from or which was copied from it.
func (h *history) starts(backup, there []string, what, where string) *Failure {
	for i, text := range backup {
		if i >= len(there) {
			return &Failure{Problem: Lost, Reason: fmt.Sprintf("%s holds %s as commit %d, and %s holds %s", what, short(text), i+1, where, counted(len(there), "commit"))}
		}
		if there[i] != text {
			return &Failure{Problem: Disagree, Reason: fmt.Sprintf("%s holds %s as commit %d, and %s holds %s there", what, short(text), i+1, where, short(there[i]))}
		}
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
	rep.Counts = nil
	if len(h.counts) > 0 {
		rep.Counts = maps.Clone(h.counts)
	}
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
	rep.Restores, rep.Lost = h.last(), 0
	for e := range h.last() {
		if n := len(h.eras[e].gone) - len(h.eras[e+1].backup); n > 0 {
			rep.Lost += n
		}
	}
}
