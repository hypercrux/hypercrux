// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Targets are the Beta's targets from BETA.md's Targets table, by
// benchmark. Put's is 0, since its target is to be no slower than 0.x in
// the same run.
var Targets = map[string]time.Duration{
	"Nearest_100k_384dims":               65 * time.Millisecond,
	"Nearest_100k_384dims_tenthFiltered": 12 * time.Millisecond,
	"Nearest_10k_1536dims":               26 * time.Millisecond,
	"Walk_100k_depth1":                   3 * time.Microsecond,
	"Walk_100k_depth3":                   40 * time.Microsecond,
	"Put":                                0,
	"PutBatch_384dims":                   5 * time.Microsecond,
	"Get":                                3 * time.Microsecond,
	"Open_100k_384dims":                  250 * time.Millisecond,
}

// A Run is a run of the benchmarks, read back from what go test -bench
// printed, with -v or without it.
type Run struct {
	// Benchmarks are the benchmarks' names, as 0.1 names them, without
	// Benchmark in front or the engine after, in the order they ran.
	Benchmarks []string
	// Engines are the engines' names, as the sub-benchmarks give them, in
	// the order they ran.
	Engines []string
	// Took is how long go test said the run took, or 0 if it didn't say.
	Took time.Duration
	// Failed says go test reported a failure.
	Failed bool

	results map[key]*result
}

type key struct{ name, engine string }

// result is what one benchmark on one engine gave.
type result struct {
	times   []float64 // nanoseconds an operation took, in each run
	skipped bool
	failed  bool
	output  []string // the lines it logged, each once, without file:line
}

// Times gives the time an operation took in each of a benchmark's runs on
// an engine, in the order of the runs.
func (r *Run) Times(name, engine string) []time.Duration {
	res := r.results[key{name, engine}]
	if res == nil {
		return nil
	}
	ts := make([]time.Duration, len(res.times))
	for i, t := range res.times {
		ts[i] = time.Duration(t)
	}
	return ts
}

// Middle gives the middle of a benchmark's runs on an engine, sorted by
// time, or the faster of the two middle ones when there's an even number of
// them. It reports false when the benchmark has no runs there.
func (r *Run) Middle(name, engine string) (time.Duration, bool) {
	res := r.results[key{name, engine}]
	if res == nil || len(res.times) == 0 {
		return 0, false
	}
	return time.Duration(middle(res.times)), true
}

func middle(times []float64) float64 {
	s := slices.Sorted(slices.Values(times))
	return s[(len(s)-1)/2]
}

// Skipped gives what a benchmark that was skipped on an engine said, or ""
// if it wasn't skipped there.
func (r *Run) Skipped(name, engine string) string {
	res := r.results[key{name, engine}]
	if res == nil || !res.skipped {
		return ""
	}
	return strings.Join(res.output, " ")
}

// fileLine is the place a line logged by a test or benchmark starts with.
var fileLine = regexp.MustCompile(`^\S+\.go:\d+: `)

// Parse reads what go test -bench printed, with -v or without it, into a
// Run.
func Parse(in io.Reader) (*Run, error) {
	r := &Run{results: map[key]*result{}}
	var cur *result // the benchmark the lines that follow belong to
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case strings.HasPrefix(line, " "):
			// What a benchmark logged, indented under its name. A line of a
			// message after its first has no file:line, so it's taken as it
			// is, whatever it starts with.
			if cur != nil {
				text := fileLine.ReplaceAllString(strings.TrimSpace(line), "")
				if !slices.Contains(cur.output, text) {
					cur.output = append(cur.output, text)
				}
			}
		case strings.HasPrefix(f[0], "Benchmark") && len(f) == 1:
			// With -v, a benchmark's name comes alone before it runs.
			cur = r.at(f[0])
		case strings.HasPrefix(f[0], "Benchmark"):
			// A run: the name with GOMAXPROCS after a dash, the number of
			// iterations, then each value with its unit.
			cur = r.at(withoutProcs(f[0]))
			for i := 2; i+1 < len(f); i += 2 {
				if f[i+1] != "ns/op" {
					continue
				}
				t, err := strconv.ParseFloat(f[i], 64)
				if err != nil {
					return nil, fmt.Errorf("bench: %q: %v", line, err)
				}
				cur.times = append(cur.times, t)
			}
		case strings.HasPrefix(line, "--- SKIP: ") && len(f) >= 3:
			cur = r.at(f[2])
			cur.skipped = true
		case strings.HasPrefix(line, "--- FAIL: ") && len(f) >= 3:
			cur = r.at(f[2])
			cur.failed = true
		case strings.HasPrefix(line, "--- BENCH: ") && len(f) >= 3:
			// Without -v, what a benchmark logged follows its run.
			cur = r.at(withoutProcs(f[2]))
		case f[0] == "ok" && len(f) >= 3:
			if d, err := time.ParseDuration(f[2]); err == nil {
				r.Took = d
			}
		case f[0] == "FAIL" || f[0] == "panic:":
			r.Failed = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// at gives the result for a benchmark's full name, such as
// BenchmarkGet/Beta, and keeps the order names come in.
func (r *Run) at(full string) *result {
	name, engine, _ := strings.Cut(strings.TrimPrefix(full, "Benchmark"), "/")
	if !slices.Contains(r.Benchmarks, name) {
		r.Benchmarks = append(r.Benchmarks, name)
	}
	if engine != "" && !slices.Contains(r.Engines, engine) {
		r.Engines = append(r.Engines, engine)
	}
	k := key{name, engine}
	if r.results[k] == nil {
		r.results[k] = &result{}
	}
	return r.results[k]
}

// withoutProcs takes the dash and GOMAXPROCS off the end of a benchmark's
// name, as go test prints it beside a run when GOMAXPROCS isn't 1.
func withoutProcs(name string) string {
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return name
	}
	if _, err := strconv.Atoi(name[i+1:]); err != nil {
		return name
	}
	return name[:i]
}

// waits reads the task out of a skip's reason, as the benchmarks give it.
var waits = regexp.MustCompile(`^waits for task (\S+?),`)

// WriteTable writes a table of the run: a row for each benchmark, with the
// middle of its runs on each engine and the Beta's target from BETA.md.
// What a benchmark logged, and the reason for a skip that names no task,
// go in notes under the table, each marked beside its time.
func (r *Run) WriteTable(w io.Writer) error {
	runs := 0
	for _, res := range r.results {
		runs = max(runs, len(res.times))
	}
	var buf bytes.Buffer
	switch runs {
	case 0:
		fmt.Fprintln(&buf, "No benchmark ran.")
	case 1:
		fmt.Fprintln(&buf, "Time per operation, from one run of each benchmark on each engine, with the")
		fmt.Fprintln(&buf, "Beta's targets from BETA.md.")
	default:
		fmt.Fprintf(&buf, "Time per operation, the middle of %s of each benchmark on each engine,\n", count(runs, "run"))
		fmt.Fprintln(&buf, "with the Beta's targets from BETA.md.")
	}
	if r.Took > 0 {
		fmt.Fprintf(&buf, "The run took %s.\n", took(r.Took))
	}
	if r.Failed {
		fmt.Fprintln(&buf, "The run failed: the output below says where.")
	}
	fmt.Fprintln(&buf)

	var notes []string
	mark := func(note string) string {
		i := slices.Index(notes, note)
		if i < 0 {
			notes = append(notes, note)
			i = len(notes) - 1
		}
		return fmt.Sprintf("[%d]", i+1)
	}
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Benchmark\t%s\tThe Beta's target\n", strings.Join(r.Engines, "\t"))
	for _, name := range r.Benchmarks {
		row := []string{name}
		for _, e := range r.Engines {
			row = append(row, r.cell(name, e, mark))
		}
		row = append(row, target(name))
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
	if len(notes) > 0 {
		fmt.Fprintln(&buf)
		for i, n := range notes {
			fmt.Fprintf(&buf, "[%d] %s\n", i+1, n)
		}
	}
	// tabwriter pads every cell but the last, so a row whose last cells
	// are empty ends in spaces.
	var out strings.Builder
	for line := range strings.Lines(buf.String()) {
		out.WriteString(strings.TrimRight(line, " \n"))
		out.WriteString("\n")
	}
	_, err := io.WriteString(w, out.String())
	return err
}

// cell is a benchmark's entry in the table for one engine.
func (r *Run) cell(name, engine string, mark func(string) string) string {
	res := r.results[key{name, engine}]
	switch {
	case res == nil:
		return "not run"
	case res.skipped:
		why := strings.Join(res.output, " ")
		if m := waits.FindStringSubmatch(why); m != nil {
			return "waits for " + m[1]
		}
		if why == "" {
			return "skipped"
		}
		return "skipped " + mark(engine+": "+why)
	case res.failed:
		return "failed"
	case len(res.times) == 0:
		return "no time"
	}
	c := duration(middle(res.times))
	for _, o := range res.output {
		c += " " + mark(engine+": "+o)
	}
	return c
}

// target is a benchmark's target in the table.
func target(name string) string {
	t, ok := Targets[name]
	switch {
	case !ok:
		return ""
	case t == 0:
		return "no slower than 0.x"
	}
	return duration(float64(t))
}

// duration writes nanoseconds in the largest unit that keeps a number of
// at least 1, to three significant figures: 4.25 ms, 391 ms, 42.5 µs, 3 µs.
func duration(ns float64) string {
	units := []struct {
		size float64
		name string
	}{{1e9, "s"}, {1e6, "ms"}, {1e3, "µs"}, {1, "ns"}}
	for _, u := range units {
		v := ns / u.size
		if v < 1 && u.size > 1 {
			continue
		}
		decimals := 0
		switch {
		case v < 10:
			decimals = 2
		case v < 100:
			decimals = 1
		}
		s := strconv.FormatFloat(v, 'f', decimals, 64)
		if strings.Contains(s, ".") {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		return s + " " + u.name
	}
	return "0 ns"
}

// took writes how long the run took, to the second.
func took(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s == 0:
		return "under a second"
	case s < 60:
		return plural(strconv.Itoa(s), s, "second")
	case s%60 == 0:
		return plural(strconv.Itoa(s/60), s/60, "minute")
	}
	return plural(strconv.Itoa(s/60), s/60, "minute") + " " + plural(strconv.Itoa(s%60), s%60, "second")
}

// count writes n things, in words up to nine.
func count(n int, thing string) string {
	words := []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	if n >= 0 && n < len(words) {
		return plural(words[n], n, thing)
	}
	return plural(strconv.Itoa(n), n, thing)
}

// plural writes num, which is n, and thing, with an s unless n is 1.
func plural(num string, n int, thing string) string {
	if n != 1 {
		thing += "s"
	}
	return num + " " + thing
}
