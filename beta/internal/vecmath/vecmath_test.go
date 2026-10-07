// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package vecmath

import (
	"bufio"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pair is two vectors to compare, with a name for the golden file.
type pair struct {
	name string
	a, b []float32
}

// uniform makes n values between -1 and 1 from r. It uses only Uint32 and
// operations that every processor rounds the same way, so amd64 and arm64
// make the same vectors; NormFloat64 can reach math.Exp, which isn't
// guaranteed to give the same bits everywhere.
func uniform(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = 2*r.Float32() - 1
	}
	return v
}

func scaled(v []float32, k float32) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * k
	}
	return out
}

// pairs are the vectors the golden file holds: random ones of many lengths,
// and awkward ones.
func pairs() []pair {
	r := rand.New(rand.NewPCG(9, 0x7663))
	var ps []pair
	add := func(name string, a, b []float32) { ps = append(ps, pair{name, a, b}) }
	for n := 1; n <= 17; n++ {
		add(fmt.Sprintf("random-%d", n), uniform(r, n), uniform(r, n))
	}
	for _, n := range []int{31, 32, 33, 384, 1536, 65536} {
		add(fmt.Sprintf("random-%d", n), uniform(r, n), uniform(r, n))
	}

	tiny := make([]float32, 40) // subnormal float32s only
	for i := range tiny {
		tiny[i] = math.Float32frombits(uint32(1 + i*977))
	}
	add("subnormals", tiny, scaled(tiny, -1))
	add("subnormals-with-normals", tiny, uniform(r, 40))

	huge := uniform(r, 64)
	for i := range huge {
		huge[i] *= math.MaxFloat32
	}
	add("near-float32-max", huge, uniform(r, 64))
	add("near-float32-max-squared", huge, huge)

	mixed := make([]float32, 100) // big and small together, so order matters
	for i := range mixed {
		switch i % 4 {
		case 0:
			mixed[i] = 1e30
		case 1:
			mixed[i] = 1e-30
		case 2:
			mixed[i] = -1e30
		default:
			mixed[i] = 3e-7
		}
	}
	add("big-and-small", mixed, uniform(r, 100))
	add("big-and-small-squared", mixed, mixed)

	zeros := uniform(r, 24)
	for i := 0; i < len(zeros); i += 3 {
		zeros[i] = float32(math.Copysign(0, -1))
	}
	add("negative-zeros", zeros, uniform(r, 24))

	v := uniform(r, 384)
	nudged := append([]float32{}, v...)
	nudged[7] = math.Nextafter32(nudged[7], 2)
	add("one-ulp-apart", v, nudged)
	add("the-same", v, v)
	add("opposite", v, scaled(v, -1))
	add("orthogonal", []float32{1, 0, 0, 0, 0, 0, 0, 0, 0}, []float32{0, 1, 0, 0, 0, 0, 0, 0, 0})
	add("all-zeros", make([]float32, 8), uniform(r, 8))

	one := make([]float32, 1000) // one big value among many small ones
	for i := range one {
		one[i] = 1e-8
	}
	one[500] = 1e8
	add("one-big-many-small", one, one)
	return ps
}

// line is what the golden file holds for a pair: every result's bits.
func line(p pair) string {
	dot := Dot(p.a, p.b)
	na, nb := Norm(p.a), Norm(p.b)
	return fmt.Sprintf("%s dot=%016x norm_a=%016x norm_b=%016x distance=%016x",
		p.name, math.Float64bits(dot), math.Float64bits(na), math.Float64bits(nb), math.Float64bits(Distance(dot, na, nb)))
}

const golden = "testdata/golden.txt"

// TestSameBitsEverywhere checks every result against the bits recorded on
// amd64. CI runs it on arm64 too. Record the file again only when the loop
// is meant to change, with HYPERCRUX_VECMATH_RECORD=1, on amd64.
func TestSameBitsEverywhere(t *testing.T) {
	var got []string
	for _, p := range pairs() {
		got = append(got, line(p))
	}
	if os.Getenv("HYPERCRUX_VECMATH_RECORD") != "" {
		if runtime.GOARCH != "amd64" {
			t.Fatal("record the golden file on amd64")
		}
		head := "# Dot products, norms and distances from vecmath, as float64 bits, recorded on amd64.\n" +
			"# Every other processor has to give the same. vecmath_test.go makes the vectors.\n"
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(head+strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d pairs", len(got))
		return
	}
	f, err := os.Open(golden)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "#") {
			want = append(want, l)
		}
	}
	if len(want) != len(got) {
		t.Fatalf("the golden file has %d pairs and the test makes %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("on %s/%s:\n  got  %s\n  want %s", runtime.GOOS, runtime.GOARCH, got[i], want[i])
		}
	}
	t.Logf("%d pairs give the recorded bits on %s/%s", len(got), runtime.GOOS, runtime.GOARCH)
}

// reference is the loop written as plainly as possible: value i goes to sum
// i mod 8, and the sums are combined in the same order. The unrolled loops
// must match it bit for bit.
func reference(a, b []float32) float64 {
	var s [8]float64
	for i := range a {
		s[i%8] += float64(a[i]) * float64(b[i])
	}
	return ((s[0] + s[1]) + (s[2] + s[3])) + ((s[4] + s[5]) + (s[6] + s[7]))
}

func TestTheLoopsMatchTheReference(t *testing.T) {
	r := rand.New(rand.NewPCG(10, 1))
	check := func(name string, a, b []float32) {
		t.Helper()
		want := math.Float64bits(reference(a, b))
		if got := math.Float64bits(Dot(a, b)); got != want {
			t.Fatalf("%s: Dot gives %016x, the reference %016x", name, got, want)
		}
		if got := math.Float64bits(Widen(a).Dot(b)); got != want {
			t.Fatalf("%s: Wide.Dot gives %016x, the reference %016x", name, got, want)
		}
	}
	for _, p := range pairs() {
		check(p.name, p.a, p.b)
	}
	for i := 0; i < 2000; i++ {
		n := r.IntN(70)
		a, b := make([]float32, n), make([]float32, n)
		for j := range a {
			a[j] = math.Float32frombits(r.Uint32())
			b[j] = math.Float32frombits(r.Uint32())
			if f := float64(a[j]); math.IsNaN(f) || math.IsInf(f, 0) {
				a[j] = 1
			}
			if f := float64(b[j]); math.IsNaN(f) || math.IsInf(f, 0) {
				b[j] = -1
			}
		}
		check(fmt.Sprintf("random bits, %d values", n), a, b)
	}
}

// zeroxDistance is 0.x's arithmetic, from cosine in vectors.go: the sums
// added in plain order, then 1 - dot/(sqrt(na) * sqrt(nb)), kept between 0
// and 2, and 1 for a vector of zeros.
func zeroxDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return min(max(1-dot/(math.Sqrt(na)*math.Sqrt(nb)), 0), 2)
}

// TestCloseToZerox checks the distances against 0.x's, which add in another
// order, within conformance.DistanceBound (1e-9).
func TestCloseToZerox(t *testing.T) {
	const bound = 1e-9
	r := rand.New(rand.NewPCG(11, 1))
	worst := 0.0
	check := func(name string, a, b []float32) {
		t.Helper()
		got := Distance(Dot(a, b), Norm(a), Norm(b))
		diff := math.Abs(got - zeroxDistance(a, b))
		worst = max(worst, diff)
		if diff > bound {
			t.Fatalf("%s: %v here and %v in 0.x", name, got, zeroxDistance(a, b))
		}
	}
	for _, p := range pairs() {
		check(p.name, p.a, p.b)
	}
	for i := 0; i < 5000; i++ {
		n := 1 + r.IntN(1600)
		check(fmt.Sprintf("random, %d values", n), uniform(r, n), uniform(r, n))
	}
	t.Logf("the largest difference from 0.x was %.3g", worst)
}

func TestDistanceEdges(t *testing.T) {
	v := []float32{0.6, 0.8}
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"the same", Distance(Dot(v, v), Norm(v), Norm(v)), 0},
		{"opposite", Distance(Dot(v, []float32{-0.6, -0.8}), 1, 1), 2},
		{"orthogonal", Distance(Dot(v, []float32{0.8, -0.6}), 1, 1), 1},
		{"a zero vector", Distance(0, 0, 1), 1},
		{"kept above 0", Distance(1.0000001, 1, 1), 0},
		{"kept below 2", Distance(-1.0000001, 1, 1), 2},
	} {
		if math.Abs(c.got-c.want) > 1e-15 {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The benchmarks set the loop against BETA.md's targets: 65 ms to search
// 100,000 vectors of 384 values, and 26 ms for 10,000 of 1,536. A search
// does a little more than the loop, keeping the closest k in a heap.
func benchScan(b *testing.B, count, dims int) {
	r := rand.New(rand.NewPCG(12, 1))
	flat := uniform(r, count*dims)
	q := Widen(uniform(r, dims))
	b.SetBytes(int64(count * dims * 4))
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		for j := 0; j < count; j++ {
			sink += q.Dot(flat[j*dims : (j+1)*dims])
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/scan")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(count), "ns/vector")
	if sink == 42 {
		b.Log(sink)
	}
}

func BenchmarkScan100kOf384(b *testing.B) { benchScan(b, 100000, 384) }
func BenchmarkScan10kOf1536(b *testing.B) { benchScan(b, 10000, 1536) }

func BenchmarkDot384(b *testing.B) {
	r := rand.New(rand.NewPCG(13, 1))
	x, y := uniform(r, 384), uniform(r, 384)
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += Dot(x, y)
	}
	if sink == 42 {
		b.Log(sink)
	}
}
