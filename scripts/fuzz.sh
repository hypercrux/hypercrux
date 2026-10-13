#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every fuzz target in the module for a while: 30 seconds each unless
# a time is given. A target that finds a failure writes the input to its
# package's testdata/fuzz folder, where go test replays it from then on.
#
#   sh scripts/fuzz.sh            # 30 seconds a target
#   sh scripts/fuzz.sh 10m        # longer runs
#   sh scripts/fuzz.sh 60m 2/3    # the second shard of three, 60 minutes a target
#
# Shard i of n takes the targets i, i+n, i+2n and so on, in the order go
# list and the test files give them, so the n shards between them run each
# target once. The long runs use shards (task R1).
#
# Go's fuzzer minimises each new input it finds, for up to a minute by
# default, and explores nothing new meanwhile. With the vector block's syntax,
# FuzzDecodeBatch spent half of its 30 seconds that way (V2.md), so the
# minimising stops at 2 seconds an input. A failing input is kept either
# way.
#
# A target that fails doesn't stop the ones after it, and the script fails
# at the end, naming them. Under GitHub Actions each failure is also an
# error annotation, with the failing input when it's small: the REST API
# shows annotations, where a cloud session can't fetch the log or the
# artifact.
set -eu
cd "$(dirname "$0")/.."
time=${1:-30s}
shard=${2:-1/1}
case "$shard" in
[0-9]*/[0-9]*) i=${shard%/*} n=${shard#*/} ;;
*) i='' n='' ;;
esac
case "$i$n" in
'' | *[!0-9]*)
	echo "a shard reads like 2/4, and $shard doesn't" >&2
	exit 2
	;;
esac
if [ "$i" -lt 1 ] || [ "$i" -gt "$n" ]; then
	echo "shard $shard: its number runs from 1 to $n" >&2
	exit 2
fi
log=$(mktemp)
status=$(mktemp)
trap 'rm -f "$log" "$status"' EXIT

# annotate writes an annotation when the script runs under GitHub Actions,
# with the message on standard input made into one line, as a workflow
# command takes it.
annotate() { # level title
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		printf '::%s title=%s::%s\n' "$1" "$2" "$(awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%s%s", (NR > 1 ? "%0A" : ""), $0 }')"
	else
		cat >/dev/null
	fi
}

k=0
found=0
failed=''
for pkg in $(go list ./...); do
	dir=$(go list -f '{{.Dir}}' "$pkg")
	for target in $(grep -ho '^func Fuzz[A-Za-z0-9_]*' "$dir"/*_test.go 2>/dev/null | sed 's/^func //'); do
		k=$((k + 1))
		if [ $(((k - i) % n)) -ne 0 ]; then
			continue
		fi
		found=$((found + 1))
		echo "== $target in $pkg, for $time"
		echo 1 >"$status"
		{
			set +e # in this subshell only, so the status gets written
			go test -run '^$' -fuzz "^$target\$" -fuzztime "$time" -fuzzminimizetime 2s "$pkg"
			echo $? >"$status"
		} 2>&1 | tee "$log"
		if [ "$(cat "$status")" = 0 ]; then
			continue
		fi
		failed="$failed $target"
		if grep -q -e '--- FAIL' "$log"; then
			sed -n '/--- FAIL/,$p' "$log" | head -n 40
		else
			tail -n 20 "$log" # a build that failed, say
		fi | annotate error "$target failed in $pkg"
		input=$(sed -n 's/.*Failing input written to \(testdata\/fuzz\/[^ ]*\).*/\1/p' "$log" | head -n 1)
		if [ -n "$input" ] && [ -f "$dir/$input" ]; then
			rel=${dir#"$PWD"/}
			if [ "$(wc -c <"$dir/$input")" -le 4096 ]; then
				{
					echo "To replay it, write this to $rel/$input and run: go test -run '^$target\$/^${input##*/}\$' ./$rel"
					cat "$dir/$input"
				} | annotate error "$target's failing input"
			else
				echo "It's $rel/$input, over 4 KB, so it's in the job's artifact only." | annotate error "$target's failing input"
			fi
		fi
	done
done
if [ "$found" -eq 0 ]; then
	echo "found no fuzz targets for shard $shard, of $k in all"
	exit 1
fi
if [ -n "$failed" ]; then
	echo "ran $found fuzz targets, and these failed:$failed"
	exit 1
fi
echo "ran $found fuzz targets"
