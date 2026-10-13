#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Switches on each bug planted in the Beta's code, one at a time, and runs
# the tests that should catch it. beta/plants.txt lists them. Every bug has
# to make its tests fail; one that doesn't is a gap in the tests. Each run
# stops at the first test that fails, since one failure is all it needs.
#
#   sh scripts/planted.sh        # every planted bug
#   sh scripts/planted.sh 2/4    # the second shard of four
#
# Shard i of n takes the bugs on lines i, i+n, i+2n and so on of the list,
# so the n shards between them try each bug once, and each shard gets about
# a share of every package's bugs. CI runs the shards side by side (task R1).
# A run that fails without go test's FAIL line never got to its tests, so
# it's broken, and fails the shard as a missed bug does. Under GitHub
# Actions each is also an error annotation, and the count a notice, which
# the REST API shows.
set -u
cd "$(dirname "$0")/.."
shard=${1:-1/1}
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
title=planted
if [ "$n" -gt 1 ]; then
	title="planted $i of $n"
fi
log=$(mktemp)
trap 'rm -f "$log"' EXIT
plants=$(grep -v -e '^#' -e '^[[:space:]]*$' beta/plants.txt | awk -v i="$i" -v n="$n" '(NR - i) % n == 0')
if [ -z "$plants" ]; then
	echo "shard $shard has no planted bugs"
	exit 1
fi

annotate() { # level message, on one line
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		printf '::%s title=%s::%s\n' "$1" "$title" "$2"
	fi
}

# With the tag and no bug switched on, everything still passes, so a
# failure below comes from the bug and not from the build.
pkgs=$(echo "$plants" | cut -d' ' -f2- | tr ' ' '\n' | sort -u | tr '\n' ' ')
if ! go test -tags hypercrux_planted -count=1 $pkgs >"$log" 2>&1; then
	cat "$log"
	echo "the tests fail with the hypercrux_planted tag and no bug switched on"
	# The lines that say which, made into one line for the annotation.
	which=$(grep -E -e '--- FAIL|^FAIL|panic:' "$log" | head -n 20 |
		awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%%0A%s", $0 }')
	annotate error "the tests fail with the hypercrux_planted tag and no bug switched on:$which"
	exit 1
fi

missed=0
total=0
began=$(date +%s)
echo "$plants" | {
	while read -r name pkgs; do
		total=$((total + 1))
		start=$(date +%s)
		if HYPERCRUX_PLANT="$name" go test -tags hypercrux_planted -count=1 -failfast $pkgs >"$log" 2>&1; then
			echo "MISSED $name: the tests in $pkgs pass with it planted"
			annotate error "MISSED $name: the tests in $pkgs pass with it planted"
			missed=$((missed + 1))
		elif ! grep -q '^FAIL' "$log" 2>/dev/null; then
			# go test prints FAIL whenever a test fails, so this run never
			# got as far as its tests: its log couldn't be written, say.
			# That's no catch (R1.md).
			echo "BROKEN $name: its tests never ran: $(tail -n 1 "$log" 2>/dev/null)"
			annotate error "BROKEN $name: its tests never ran"
			missed=$((missed + 1))
		else
			echo "caught $name in $(($(date +%s) - start))s"
		fi
	done
	summary="$((total - missed)) of $total planted bugs caught"
	if [ "$n" -gt 1 ]; then
		summary="$summary, in shard $shard"
	fi
	echo "$summary, in $(($(date +%s) - began))s"
	annotate notice "$summary, in $(($(date +%s) - began))s"
	[ "$missed" -eq 0 ]
}
