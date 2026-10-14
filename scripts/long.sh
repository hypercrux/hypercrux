#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs one job of the long runs (task R1), as the long workflow,
# .github/workflows/long.yml, runs it on amd64 and on arm64, at the length
# given:
#
#   sh scripts/long.sh fuzz 70m 1/2   # the first half of the fuzz targets, 70 minutes each
#   sh scripts/long.sh crash 3840     # the crash suites, 3,840 seeds a point
#   sh scripts/long.sh procs 42m      # each many-process run for 42 minutes
#   sh scripts/long.sh diff 800000    # 800,000 random sequences on 0.x and the Beta
#   sh scripts/long.sh planted 2/2    # the second half of the planted bugs
#
# Short lengths check a job in a few minutes: fuzz 10s, crash 1, which
# leaves every crash test at its own seeds, procs 5s and diff 2000.
#
# The output goes to the terminal and to output.txt in the folder LONG_OUT
# names, or a new temporary folder, and the tests' temporary files go in its
# tmp folder. When the job fails, the folder keeps what replays it, and
# beta/tasks/R1.md says how:
#
#   - output.txt, which names each crash point, many-process seed and
#     planted bug that failed;
#   - found/, the files the tests wrote into the tree, with their paths: a
#     fuzz target's failing input, in its package's testdata/fuzz, and a
#     sequence the differential harness saved in beta/difftest/testdata;
#   - tmp/, whatever a test left there, such as the database of a test that
#     go test's timeout stopped before it could clean up.
#
# LONG_TIMEOUT is go test's -timeout for the crash, procs and diff suites,
# 0 (none) when it's unset. The workflow sets it below the job's own limit,
# so a test that hangs prints its goroutines before the job is stopped.
# Under GitHub Actions a failure's lines are error annotations, and what
# the job did is a notice, since the REST API shows annotations where a
# cloud session can't fetch the log or the artifact.
set -eu
cd "$(dirname "$0")/.."
job=${1:-}
length=${2:-}
shard=${3:-1/1}
timeout=${LONG_TIMEOUT:-0}
case "$job" in
crash | diff)
	case "$length" in
	'' | *[!0-9]* | 0*)
		echo "$job takes a whole number from 1, and '$length' isn't one" >&2
		exit 2
		;;
	esac
	if [ "$job" = crash ]; then
		what="crash, HYPERCRUX_CRASH_SEEDS=$length"
	else
		what="diff, HYPERCRUX_DIFF_SEQUENCES=$length"
	fi
	;;
fuzz)
	what="fuzz, shard $shard, $length a target"
	;;
procs)
	what="procs, HYPERCRUX_PROCS_TIME=$length"
	;;
planted)
	shard=${length:-1/1}
	what="planted, shard $shard"
	;;
*)
	echo "usage: sh scripts/long.sh fuzz|crash|procs|diff|planted [length] [shard]" >&2
	exit 2
	;;
esac
if [ -z "$length" ] && [ "$job" != planted ]; then
	echo "$job needs a length" >&2
	exit 2
fi

out=${LONG_OUT:-}
if [ -z "$out" ]; then
	out=$(mktemp -d)
fi
mkdir -p "$out/tmp"
out=$(cd "$out" && pwd -P)
# A real path, since the log's tests compare the paths of their files
# under t.TempDir with the real ones (F7.md).
TMPDIR=$out/tmp
export TMPDIR

# annotate writes an annotation when the script runs under GitHub Actions,
# with the message on standard input made into one line of at most about
# 4 KB, as a workflow command takes it.
annotate() { # level title
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		printf '::%s title=%s::%s\n' "$1" "$2" "$(head -c 4000 | awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%s%s", (NR > 1 ? "%0A" : ""), $0 }')"
	else
		cat >/dev/null
	fi
}

arch=$(go env GOARCH)
gotest() { # the package's tests at the length given, with -v so the reports show
	go test -count=1 -v -timeout "$timeout" "$@"
}
run() {
	case "$job" in
	fuzz) sh scripts/fuzz.sh "$length" "$shard" ;;
	crash)
		# F8's measure of a compaction's rate, on 160 MB, comes along.
		HYPERCRUX_CRASH_SEEDS=$length HYPERCRUX_COMPACT_RECORDS=100000 \
			gotest ./beta/internal/crash ./beta/internal/logfile ./beta/hypercrux
		;;
	procs) HYPERCRUX_PROCS_TIME=$length gotest ./beta/internal/procs ;;
	diff)
		# 0.x against itself is A3's check of the harness; the hours go to
		# 0.x against the Beta. A renamed test would leave nothing to run.
		if ! go test -list '^TestZeroxAgainstTheBeta$' ./beta/difftest | grep -qx TestZeroxAgainstTheBeta; then
			echo "beta/difftest has no TestZeroxAgainstTheBeta"
			return 1
		fi
		HYPERCRUX_DIFF_SEQUENCES=$length gotest -run '^TestZeroxAgainstTheBeta$' ./beta/difftest
		;;
	planted) sh scripts/planted.sh "$shard" ;;
	esac
}

echo "== long run: $what, on $arch, keeping its files in $out"
began=$(date +%s)
echo 1 >"$out/status"
{
	set +e # in this subshell only, so the status gets written
	run
	echo $? >"$out/status"
} 2>&1 | tee "$out/output.txt"
status=$(cat "$out/status")
took=$(($(date +%s) - began))

# What the job did, for the notice: each package's result, each fuzz
# target's last count of executions, and the compaction's rate.
awk -v what="$what on $arch, in ${took}s" '
	BEGIN { print what }
	/^== / { if (last != "") print "  " last; last = ""; print $0; next }
	/^fuzz: elapsed/ { last = $0; next }
	/^(ok|FAIL)[ \t]/ || /the compaction took/ { print }
	END { if (last != "") print "  " last }
' "$out/output.txt" | annotate notice "long $job"

if [ "$status" = 0 ]; then
	if [ -z "${LONG_OUT:-}" ]; then
		rm -rf "$out"
	fi
	echo "== long run passed: $what, on $arch, in ${took}s"
	exit 0
fi

# The files the tests wrote into the tree, copied with their paths.
git ls-files --others --exclude-standard -- '*/testdata/*' | while read -r f; do
	mkdir -p "$out/found/$(dirname "$f")"
	cp "$f" "$out/found/$f"
done
# The lines that say what failed, or the output's end when none does, as
# when a build fails. fuzz.sh and planted.sh annotate their own failures
# too, and these lines sum them up.
lines=$(grep -v '^::' "$out/output.txt" | grep -E \
	-e '--- FAIL|MISSED|HYPERCRUX_CRASH_POINT|HYPERCRUX_PROCS_SEED|saved as|panic:|^FAIL' \
	-e 'running tests:|^[[:space:]]+Test[^ ]* \([0-9]' | head -n 40) || true
if [ -z "$lines" ]; then
	lines=$(grep -v '^::' "$out/output.txt" | tail -n 20)
fi
printf '%s\n' "$lines" | annotate error "long $job failed"
for f in $(cd "$out/found" 2>/dev/null && find . -path '*/difftest/testdata/*.json' | sed 's|^\./||'); do
	{
		echo "To replay it, write this to $f and run: go test -count=1 -run '^TestSavedSequences\$' ./beta/difftest"
		cat "$out/found/$f"
	} | annotate error "the saved sequence $f"
done
echo "== long run failed: $what, on $arch, in ${took}s; what replays it is in $out"
exit 1
