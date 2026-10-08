// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bench

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRecord writes the table for the results file, when
// scripts/record-beta-bench.sh asks for it: it reads the benchmarks' output
// from the file HYPERCRUX_BENCH_OUTPUT names, and writes the table to the
// file HYPERCRUX_BENCH_TABLE names. Otherwise it's skipped.
func TestRecord(t *testing.T) {
	in, out := os.Getenv("HYPERCRUX_BENCH_OUTPUT"), os.Getenv("HYPERCRUX_BENCH_TABLE")
	if in == "" || out == "" {
		t.Skip("scripts/record-beta-bench.sh sets HYPERCRUX_BENCH_OUTPUT and HYPERCRUX_BENCH_TABLE to write the results file's table")
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	run, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Benchmarks) == 0 {
		t.Fatalf("%s holds no benchmarks", in)
	}
	var b strings.Builder
	if err := run.WriteTable(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// verbose is a run's output as go test -v prints it, cut down: three runs
// of each benchmark that ran, a skip that names a task, one that doesn't, a
// benchmark that failed, one that logged a line in each of its runs, and
// one that ran on 0.x alone.
const verbose = `goos: linux
goarch: amd64
pkg: github.com/hypercrux/hypercrux/beta/bench
cpu: Intel(R) Xeon(R) Processor @ 2.10GHz
BenchmarkNearest_100k_384dims
BenchmarkNearest_100k_384dims/0.x
BenchmarkNearest_100k_384dims/0.x-2      	       8	 391085689 ns/op
BenchmarkNearest_100k_384dims/0.x-2      	       8	 408978998 ns/op
BenchmarkNearest_100k_384dims/0.x-2      	       8	 405921246 ns/op
BenchmarkNearest_100k_384dims/Beta
    bench_test.go:29: waits for task G2, which brings Nearest
--- SKIP: BenchmarkNearest_100k_384dims/Beta
BenchmarkWalk_100k_depth1
BenchmarkWalk_100k_depth1/0.x
BenchmarkWalk_100k_depth1/0.x-2          	   84974	     42542 ns/op	         5.000 records/walk
BenchmarkWalk_100k_depth1/0.x-2          	   78156	     43184 ns/op	         5.000 records/walk
BenchmarkWalk_100k_depth1/0.x-2          	   75949	     42121 ns/op	         5.000 records/walk
BenchmarkWalk_100k_depth1/Beta
    bench_test.go:29: not on this machine
--- SKIP: BenchmarkWalk_100k_depth1/Beta
BenchmarkPut
BenchmarkPut/0.x
BenchmarkPut/0.x-2                       	    9898	    388333 ns/op
BenchmarkPut/0.x-2                       	    9397	    348132 ns/op
BenchmarkPut/0.x-2                       	    9337	    326381 ns/op
BenchmarkPut/Beta
BenchmarkPut/Beta-2                      	   10021	    301000 ns/op
BenchmarkPut/Beta-2                      	    9888	    299100 ns/op
BenchmarkPut/Beta-2                      	   10102	    310500 ns/op
BenchmarkGet
BenchmarkGet/0.x
BenchmarkGet/0.x-2                       	  223614	     17912 ns/op
BenchmarkGet/0.x-2                       	  207741	     17694 ns/op
BenchmarkGet/0.x-2                       	  200644	     18287 ns/op
BenchmarkGet/Beta
    bench_test.go:200: hypercrux: something went wrong
--- FAIL: BenchmarkGet/Beta
BenchmarkOpen_100k_384dims
BenchmarkOpen_100k_384dims/0.x
BenchmarkOpen_100k_384dims/0.x-2         	    2905	   1203340 ns/op
BenchmarkOpen_100k_384dims/0.x-2         	    2931	   1187000 ns/op
BenchmarkOpen_100k_384dims/0.x-2         	    2899	   1210981 ns/op
BenchmarkOpen_100k_384dims/Beta
    bench_test.go:234: the file is a log of the 20 commits that built it, until compaction (tasks F8 and G5)
    bench_test.go:234: the file is a log of the 20 commits that built it, until compaction (tasks F8 and G5)
BenchmarkOpen_100k_384dims/Beta-2        	      10	 412000000 ns/op
    bench_test.go:234: the file is a log of the 20 commits that built it, until compaction (tasks F8 and G5)
BenchmarkOpen_100k_384dims/Beta-2        	      10	 431000000 ns/op
    bench_test.go:234: the file is a log of the 20 commits that built it, until compaction (tasks F8 and G5)
BenchmarkOpen_100k_384dims/Beta-2        	      10	 409500000 ns/op
BenchmarkLink
BenchmarkLink/0.x
BenchmarkLink/0.x-2                      	   10690	    337978 ns/op
BenchmarkLink/0.x-2                      	   10371	    332637 ns/op
BenchmarkLink/0.x-2                      	    9982	    334742 ns/op
--- FAIL: BenchmarkGet/Beta
FAIL
exit status 1
FAIL	github.com/hypercrux/hypercrux/beta/bench	312.871s
`

const verboseTable = `Time per operation, the middle of three runs of each benchmark on each engine,
with the Beta's targets from BETA.md.
The run failed: the output below says where.

Benchmark             0.x      Beta          The Beta's target
Nearest_100k_384dims  406 ms   waits for G2  65 ms
Walk_100k_depth1      42.5 µs  skipped [1]   3 µs
Put                   348 µs   301 µs        no slower than 0.x
Get                   17.9 µs  failed        3 µs
Open_100k_384dims     1.2 ms   412 ms [2]    250 ms
Link                  335 µs   not run

[1] Beta: not on this machine
[2] Beta: the file is a log of the 20 commits that built it, until compaction (tasks F8 and G5)
`

func TestParse(t *testing.T) {
	run, err := Parse(strings.NewReader(verbose))
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"Nearest_100k_384dims", "Walk_100k_depth1", "Put", "Get", "Open_100k_384dims", "Link"}
	if !slices.Equal(run.Benchmarks, wantNames) || !slices.Equal(run.Engines, []string{"0.x", "Beta"}) {
		t.Fatalf("benchmarks %q on %q", run.Benchmarks, run.Engines)
	}
	if !run.Failed || run.Took != 0 {
		t.Errorf("failed %v, took %v", run.Failed, run.Took)
	}
	middles := []struct {
		name, engine string
		want         time.Duration
		ok           bool
	}{
		{"Nearest_100k_384dims", "0.x", 405921246, true},
		{"Nearest_100k_384dims", "Beta", 0, false},
		{"Walk_100k_depth1", "0.x", 42542, true},
		{"Put", "Beta", 301000, true},
		{"Get", "Beta", 0, false},
		{"Open_100k_384dims", "Beta", 412000000, true},
		{"Link", "Beta", 0, false},
		{"NoSuchBenchmark", "0.x", 0, false},
	}
	for _, m := range middles {
		if got, ok := run.Middle(m.name, m.engine); got != m.want || ok != m.ok {
			t.Errorf("Middle(%s, %s) = %v, %v, want %v, %v", m.name, m.engine, got, ok, m.want, m.ok)
		}
	}
	if got := run.Times("Put", "0.x"); !slices.Equal(got, []time.Duration{388333, 348132, 326381}) {
		t.Errorf("Put's times on 0.x are %v", got)
	}
	if got := run.Skipped("Nearest_100k_384dims", "Beta"); got != "waits for task G2, which brings Nearest" {
		t.Errorf("the skip says %q", got)
	}
	if got := run.Skipped("Put", "Beta"); got != "" {
		t.Errorf("Put on the Beta was skipped: %q", got)
	}
	var b strings.Builder
	if err := run.WriteTable(&b); err != nil {
		t.Fatal(err)
	}
	if b.String() != verboseTable {
		t.Errorf("the table reads\n%s\nwant\n%s", b.String(), verboseTable)
	}
}

// TestParseWithoutV reads a run without -v, where what a benchmark logged
// follows its run and a skip leaves no trace, and one that took a while. A
// message of two lines has a second line that looks like a run that
// failed, and has to be read as part of the message.
func TestParseWithoutV(t *testing.T) {
	out := `BenchmarkOpen_100k_384dims/0.x-2         	    2905	   1203340 ns/op
BenchmarkOpen_100k_384dims/Beta-2        	      10	 412000000 ns/op
--- BENCH: BenchmarkOpen_100k_384dims/Beta-2
    bench_test.go:234: a note
    bench_test.go:234: a note
BenchmarkOpen_100k_384dims/Beta-2        	      10	 431000000 ns/op
--- BENCH: BenchmarkOpen_100k_384dims/Beta-2
    bench_test.go:234: a note
    bench_test.go:240: a note of two lines
        BenchmarkSecondLine-2 1 12 ns/op FAIL
PASS
ok  	github.com/hypercrux/hypercrux/beta/bench	312.871s
`
	run, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run.WriteTable(&b); err != nil {
		t.Fatal(err)
	}
	want := `Time per operation, the middle of two runs of each benchmark on each engine,
with the Beta's targets from BETA.md.
The run took 5 minutes 13 seconds.

Benchmark          0.x     Beta                The Beta's target
Open_100k_384dims  1.2 ms  412 ms [1] [2] [3]  250 ms

[1] Beta: a note
[2] Beta: a note of two lines
[3] Beta: BenchmarkSecondLine-2 1 12 ns/op FAIL
`
	if !slices.Equal(run.Benchmarks, []string{"Open_100k_384dims"}) || run.Failed {
		t.Errorf("benchmarks %q, failed %v", run.Benchmarks, run.Failed)
	}
	if b.String() != want {
		t.Errorf("the table reads\n%s\nwant\n%s", b.String(), want)
	}
}

func TestParseRefusesABadTime(t *testing.T) {
	if _, err := Parse(strings.NewReader("BenchmarkGet/0.x-2  100  1.2.3 ns/op\n")); err == nil {
		t.Error("Parse took a time of 1.2.3 ns")
	}
}

func TestTheMiddleRun(t *testing.T) {
	for _, c := range []struct {
		times []float64
		want  float64
	}{
		{[]float64{5}, 5},
		{[]float64{9, 4}, 4},
		{[]float64{9, 4, 7}, 7},
		{[]float64{1, 9, 4, 7}, 4},
		{[]float64{3, 3, 1, 8, 2}, 3},
	} {
		if got := middle(c.times); got != c.want {
			t.Errorf("middle(%v) = %v, want %v", c.times, got, c.want)
		}
	}
}

func TestDurations(t *testing.T) {
	for _, c := range []struct {
		ns   float64
		want string
	}{
		{0, "0 ns"},
		{0.5, "0.5 ns"},
		{7, "7 ns"},
		{850, "850 ns"},
		{999.4, "999 ns"},
		{1000, "1 µs"},
		{3000, "3 µs"},
		{17912, "17.9 µs"},
		{42542, "42.5 µs"},
		{50145, "50.1 µs"},
		{388333, "388 µs"},
		{1203340, "1.2 ms"},
		{4247695, "4.25 ms"},
		{65e6, "65 ms"},
		{391085689, "391 ms"},
		{1.2e9, "1.2 s"},
		{61.5e9, "61.5 s"},
	} {
		if got := duration(c.ns); got != c.want {
			t.Errorf("duration(%v) = %q, want %q", c.ns, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{400 * time.Millisecond, "under a second"},
		{900 * time.Millisecond, "1 second"},
		{42 * time.Second, "42 seconds"},
		{2 * time.Minute, "2 minutes"},
		{312871 * time.Millisecond, "5 minutes 13 seconds"},
	} {
		if got := took(c.d); got != c.want {
			t.Errorf("took(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
