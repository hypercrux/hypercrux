#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs the benchmarks in beta/bench, which are 0.1's on 0.x and on the Beta
# side by side, and writes test/results/beta-bench.txt: a note of the machine
# they ran on, a table of the middle run of each benchmark on each engine
# with the Beta's targets from BETA.md, and the runs as go test printed them.
# They run as scripts/record-tests.sh runs 0.1's, three runs of 3 seconds
# each, and need the machine to themselves.
#
#   sh scripts/record-beta-bench.sh
#
# To check the script, name another file, and give go test flags to use in
# place of -benchtime 3s -count 3. With -short every database is a hundredth
# of its size, so this takes a few seconds:
#
#   sh scripts/record-beta-bench.sh /tmp/beta-bench.txt -short -benchtime 1x -count 1
set -eu
cd "$(dirname "$0")/.."
out=${1:-test/results/beta-bench.txt}
case "$out" in
-*)
	echo "the first argument is the file to write, before any flags" >&2
	exit 2
	;;
esac
if [ $# -gt 1 ]; then
	shift
	flags="$*"
	what="A check of the benchmarks in beta/bench, run with the flags in the command below; not figures to quote"
else
	flags="-benchtime 3s -count 3"
	what="0.1's benchmarks on 0.x and on the Beta, side by side on the same data, three runs each, and opening a database on both. Put and Link are one committed transaction each, synced to disk. Nearest compares every vector (exact search) and returns the closest 10"
fi
raw=$(mktemp)
status=$(mktemp)
table=$(mktemp)
log=$(mktemp)
trap 'rm -f "$raw" "$status" "$table" "$log"' EXIT

# -v, so that each benchmark the Beta can't run yet shows with its reason.
# The output shows as it comes, since a recorded run takes minutes.
cmd="go test -run ^$ -bench . $flags -timeout 60m -v ./beta/bench"
when=$(date -u '+%Y-%m-%d %H:%M UTC')
load=$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || echo '?')
# Read before anything is written, since writing the results file makes the
# tree dirty once that file is committed.
commit=$(git describe --always --dirty 2>/dev/null || echo '?')
{
	set +e # in this subshell only, so the status gets written
	$cmd 2>&1
	echo $? >"$status"
} | tee "$raw"
if [ "$(cat "$status")" != 0 ]; then
	echo "the benchmarks failed, so nothing was written to $out"
	exit 1
fi
if ! HYPERCRUX_BENCH_OUTPUT="$raw" HYPERCRUX_BENCH_TABLE="$table" go test -count=1 -run '^TestRecord$' ./beta/bench >"$log" 2>&1; then
	cat "$log"
	exit 1
fi
{
	echo "# $what"
	echo "# recorded $when with $(go version | cut -d' ' -f3-), SQLite $(go run ./cmd/hypercrux version | sed 's/.*SQLite //'), at commit $commit"
	echo "# $(uname -sr), $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //'), $(nproc 2>/dev/null || echo '?') CPUs, load average $load at the start"
	echo "# command: $cmd"
	echo
	cat "$table"
	echo
	cat "$raw"
} >"$out"
echo "results written to $out"
