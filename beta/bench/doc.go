// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package bench times the Beta beside 0.x. Its benchmarks are 0.1's, from
// bench_test.go at the root of the repository, each run on both engines
// through their public packages as sub-benchmarks named for the engine, such
// as BenchmarkGet/0.x and BenchmarkGet/Beta. Each builds the same data on
// both, from the same seeds, and times the same calls, so one run gives the
// two side by side. One more, BenchmarkOpen_100k_384dims, times opening a
// database of 100,000 records with 384-value vectors, which 0.1 never timed.
//
// What the Beta can't run yet is skipped there, with a message naming the
// task that makes it run, so the results fill in as the work goes on.
//
// scripts/record-beta-bench.sh runs them as scripts/record-tests.sh runs
// 0.1's, three runs of 3 seconds each, and writes
// test/results/beta-bench.txt: a table of the middle run of each benchmark
// on each engine, with the Beta's targets from BETA.md, and then the runs as
// go test printed them. Parse reads the runs back, and WriteTable writes the
// table.
//
// The benchmarks run only with -bench, so they add nothing to go test ./...
// With -short, each database is a hundredth of its size, for checking the
// benchmarks in seconds. The names keep 0.1's sizes, so figures from a run
// with -short are never to be quoted.
//
// Like the rest of the beta folder it's Linux only; on other systems this
// package is empty.
package bench
