#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every test, the race detector, the fuzzers and the benchmarks, and
# writes the output to test/results/ with a note of the machine they ran on.
# The numbers in the README and on hypercrux.com come from these files.
#
#   sh scripts/record-tests.sh
set -eu
cd "$(dirname "$0")/.."
out=test/results
mkdir -p "$out"

header() {
	echo "# $1"
	echo "# recorded $(date -u '+%Y-%m-%d %H:%M UTC') with $(go version | cut -d' ' -f3-), SQLite $(go run ./cmd/hypercrux version | sed 's/.*SQLite //')"
	echo "# $(uname -sr), $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //'), $(nproc 2>/dev/null || echo '?') CPUs"
	echo "# command: $2"
	echo
}

go vet ./...

cmd="go test -count=1 -v ./..."
{ header "Every test, verbose" "$cmd"; $cmd 2>&1; } > "$out/tests.txt"

cmd="go test -race -count=1 -short ./..."
{ header "Every test with the race detector (short mode: 20 kills instead of 200)" "$cmd"; $cmd 2>&1; } > "$out/race.txt"

cmd="go test -run ^$ -fuzz FuzzReader -fuzztime 60s ./internal/export"
cmd2="go test -run ^$ -fuzz FuzzLine -fuzztime 60s ./internal/export"
{ header "The export format's reader under Go's fuzzer for a minute, then its line parser against encoding/json for a minute" "$cmd; $cmd2"; $cmd 2>&1; $cmd2 2>&1; } > "$out/fuzz.txt"

cmd="go test -run ^$ -bench . -benchtime 3s -count 3 ."
{ header "Benchmarks, three runs each. Put and Link are one committed transaction each, with synchronous = FULL. Nearest compares every vector (exact search) and returns the closest 10" "$cmd"; $cmd 2>&1; } > "$out/bench.txt"

grep -E '^(ok|FAIL|---)' "$out/tests.txt" "$out/race.txt" "$out/fuzz.txt" | grep -v -- '--- PASS' || true
echo "results written to $out/"
