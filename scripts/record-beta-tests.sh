#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Records the Beta's tests, as scripts/record-tests.sh records 0.1's (task
# R4), each to a file of its own in test/results/, headed as record-tests.sh
# heads its files, with the commit:
#
#   beta-tests.txt    every test in beta/, verbose, at full length
#   beta-race.txt     every test in beta/ with the race detector, in short mode
#   beta-fuzz.txt     each fuzz target for a minute, through scripts/fuzz.sh
#   beta-planted.txt  every planted bug, through scripts/planted.sh
#
# 0.1's files, tests.txt, race.txt, fuzz.txt and bench.txt, stay as they
# are, and scripts/record-beta-bench.sh records both engines' benchmarks.
# The planted bugs take most of the time. Timings are in the files, so the
# run needs the machine to itself:
#
#   sh scripts/record-beta-tests.sh
#
# To check the script, name another folder and add quick. The tests and the
# race detector then run on one small package in short mode, each fuzz
# target for a second, and planted.sh tries the first bug in beta/plants.txt
# alone, so it takes a minute or two:
#
#   sh scripts/record-beta-tests.sh /tmp/beta-results quick
#
# Every step runs, whatever the one before found, and the script fails at
# the end if any step failed, naming its file.
set -eu
cd "$(dirname "$0")/.."
out=${1:-test/results}
quick=${2:-}
case "$out" in
-* | quick)
	echo "the first argument is the folder to write to, before quick" >&2
	exit 2
	;;
esac
case "$quick" in
'' | quick) ;;
*)
	echo "the second argument is quick, or nothing" >&2
	exit 2
	;;
esac
if [ $# -gt 2 ]; then
	echo "usage: sh scripts/record-beta-tests.sh [FOLDER [quick]]" >&2
	exit 2
fi
plants=$(grep -c -v -e '^#' -e '^[[:space:]]*$' beta/plants.txt)
if [ -n "$quick" ]; then
	tests="go test -count=1 -v -short ./beta/internal/vecmath"
	race="go test -race -count=1 -short ./beta/internal/vecmath"
	fuzz="sh scripts/fuzz.sh 1s"
	planted="sh scripts/planted.sh 1/$plants"
	check="A check of scripts/record-beta-tests.sh, cut short as the command below shows; not results to quote. "
else
	tests="go test -count=1 -v ./beta/..."
	race="go test -race -count=1 -short ./beta/..."
	fuzz="sh scripts/fuzz.sh 60s"
	planted="sh scripts/planted.sh"
	check=""
fi
mkdir -p "$out"
# Read before anything is written, since writing the results makes the
# tree dirty once they're committed.
commit=$(git describe --always --dirty 2>/dev/null || echo '?')
sqlite=$(go run ./cmd/hypercrux version | sed 's/.*SQLite //')

header() {
	echo "# $check$1"
	echo "# recorded $(date -u '+%Y-%m-%d %H:%M UTC') with $(go version | cut -d' ' -f3-), SQLite $sqlite, at commit $commit"
	echo "# $(uname -sr), $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //'), $(nproc 2>/dev/null || echo '?') CPUs"
	echo "# command: $2"
	echo
}

failed=''
step() { # file title command, the command's words split as written
	echo "== $out/$1: $3"
	began=$(date +%s)
	status=0
	{
		header "$2" "$3"
		$3 2>&1 || status=$?
	} >"$out/$1"
	echo "   exit status $status, in $(($(date +%s) - began))s"
	if [ "$status" != 0 ]; then
		failed="$failed $1"
	fi
}

go vet ./beta/...
step beta-tests.txt "Every test in beta/, verbose, at full length (200 killed writers, 2,000 random sequences)" "$tests"
step beta-race.txt "Every test in beta/ with the race detector (short mode: 20 killed writers instead of 200, 300 random sequences instead of 2,000)" "$race"
step beta-fuzz.txt "Each fuzz target for a minute: the Beta's decoders of batches, headers and markers and its SQL parser, then the export format's line parser and reader, which both engines use" "$fuzz"
step beta-planted.txt "Each of the $plants bugs planted in the Beta's code, switched on alone with the tests that must catch it (beta/plants.txt)" "$planted"

grep -E '^(ok|FAIL|---)' "$out/beta-tests.txt" "$out/beta-race.txt" "$out/beta-fuzz.txt" | grep -v -- '--- PASS' || true
grep -E 'planted bugs caught|^(MISSED|BROKEN)' "$out/beta-planted.txt" || true
if [ -n "$failed" ]; then
	echo "these steps failed:$failed; their files in $out/ say how"
	exit 1
fi
echo "results written to $out/"
