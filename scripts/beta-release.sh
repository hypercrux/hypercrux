#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# The Beta's release files (task R4): the checks on each binary, its
# archive, the checksums and the release's notes, as the release workflow,
# .github/workflows/release.yml, makes them. The workflow builds the binary
# on each architecture's own runner, amd64 on ubuntu-latest and arm64 on
# ubuntu-24.04-arm, with
#
#   CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o hypercrux ./beta/cmd/hypercrux
#
# and then, in this order:
#
#   sh scripts/beta-release.sh stamp 0.3.0-beta.1            # a dry run only: Version set to the version it rehearses
#   sh scripts/beta-release.sh check ./hypercrux amd64 0.3.0-beta.1
#   sh scripts/beta-release.sh package ./hypercrux amd64     # writes hypercrux-beta_linux_amd64.tar.gz
#   sh scripts/beta-release.sh sums dist                     # writes dist/SHA256SUMS, over both archives in dist
#   sh scripts/beta-release.sh notes v0.3.0-beta.1 notes.md [MODE COMMIT]
#
# check runs three checks on the machine that built the binary: `file` has
# to say it's statically linked and built for that architecture, its
# `hypercrux version` line has to name the version being released, and
# scripts/smoke.sh has to pass. `file` alone checks a binary built for
# another machine, such as one cross-compiled for arm64:
#
#   sh scripts/beta-release.sh file ./hypercrux arm64
#
# package refuses a binary that holds any module besides HyperCrux's own,
# since its archive carries the licences of the Go runtime alone. Each
# archive holds the binary, LICENSE, NOTICE, THIRD_PARTY_LICENSES.md and
# beta/README.md as README.md. NOTICE and THIRD_PARTY_LICENSES.md are the
# root's, with what they say of go-sqlite3 and SQLite left out, since the
# Beta's binary has neither.
#
# notes writes the release's notes to the file named; with the mode
# (release, attach or dry-run) and the commit, it also says the title and
# the tag the release has, or would have. stamp, for a dry run, sets the
# Version in beta/hypercrux/hypercrux.go in the checkout before the build.
#
# Under GitHub Actions each result is a notice, and a check that fails an
# error annotation, since a cloud session reads a run's annotations through
# the REST API, and neither its log nor its artifacts.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
archives="hypercrux-beta_linux_amd64.tar.gz hypercrux-beta_linux_arm64.tar.gz"
form='^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-beta\.[1-9][0-9]*$'

# annotate writes an annotation under GitHub Actions, the message on
# standard input made into one line, as a workflow command takes it.
annotate() { # level title
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		printf '::%s title=%s::%s\n' "$1" "$2" "$(awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%s%s", (NR > 1 ? "%0A" : ""), $0 }')"
	else
		cat >/dev/null
	fi
}
note() { # title message
	printf '%s\n' "$2"
	printf '%s\n' "$2" | annotate notice "$1"
}
fail() { # title message
	printf '%s\n' "$2" >&2
	printf '%s\n' "$2" | annotate error "$1"
	exit 1
}
usage() {
	echo "usage: sh scripts/beta-release.sh check BINARY ARCH VERSION | file BINARY ARCH | package BINARY ARCH" >&2
	echo "       | sums DIR | notes TAG FILE [MODE COMMIT] | stamp VERSION" >&2
	exit 2
}
# given checks a binary and an architecture, and sets bin, the binary's
# path made absolute, and machine, what file calls the architecture.
given() { # binary arch
	[ -f "$1" ] || fail "beta $2" "$1 isn't a file"
	bin="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
	case "$2" in
	amd64) machine=x86-64 ;;
	arm64) machine=aarch64 ;;
	*) fail "beta" "the architecture is amd64 or arm64, and $2 is neither" ;;
	esac
}
version_ok() { # one line, of the form
	case "$1" in
	*"
"*) return 1 ;;
	esac
	printf '%s\n' "$1" | grep -Eq "$form"
}

check_file() { # binary arch, after given
	desc=$(file -b "$1")
	case "$desc" in
	*"statically linked"* | *"static-pie linked"*) ;;
	*) fail "beta $2" "file says hypercrux isn't statically linked: $desc" ;;
	esac
	case "$desc" in
	*"$machine"*) ;;
	*) fail "beta $2" "file says hypercrux isn't built for $2 ($machine): $desc" ;;
	esac
	note "beta $2" "file: hypercrux: $desc"
}

check_run() { # binary arch version
	line=$("$1" version 2>&1) || fail "beta $2" "hypercrux version failed: $line"
	case "$line" in
	"hypercrux $3, "*) note "beta $2" "hypercrux version: $line" ;;
	*) fail "beta $2" "hypercrux version says \"$line\", where the release is $3" ;;
	esac
	log=$(mktemp)
	if sh "$root/scripts/smoke.sh" "$1" >"$log" 2>&1; then
		cat "$log"
		last=$(tail -n 1 "$log")
		rm -f "$log"
		note "beta $2" "scripts/smoke.sh: $last"
	else
		cat "$log"
		tail -n 20 "$log" | annotate error "beta $2"
		rm -f "$log"
		fail "beta $2" "scripts/smoke.sh failed on the binary for $2"
	fi
}

licences() { # the Beta's THIRD_PARTY_LICENSES.md
	head -n 1 "$root/THIRD_PARTY_LICENSES.md"
	echo
	echo "The HyperCrux Beta's hypercrux command and its ready-built binaries include"
	echo "the code below. Unlike 0.x's, they hold no SQLite and no go-sqlite3."
	echo
	awk '/^## / { p = ($0 == "## Go") } p' "$root/THIRD_PARTY_LICENSES.md"
}

notice() { # the Beta's NOTICE
	sed '/^The HyperCrux package/,$d' "$root/NOTICE"
	cat <<'EOF'
The HyperCrux Beta's hypercrux command and its ready-built binaries also
contain:
  The Go runtime and standard library, Copyright 2009 The Go Authors,
  BSD-style license
Its notice is in THIRD_PARTY_LICENSES.md.
EOF
}

package() { # binary arch, after given
	deps=$(go version -m "$1" | awk '$1 == "dep" { printf "%s %s; ", $2, $3 }')
	if [ -n "$deps" ]; then
		fail "beta $2" "the binary holds modules besides HyperCrux's, whose licences its archive would need: $deps"
	fi
	# The root's files have to have the parts the Beta's are made from.
	grep -q '^The HyperCrux package' "$root/NOTICE" ||
		fail "beta $2" "NOTICE has changed, so scripts/beta-release.sh needs the Beta's notice written again"
	grep -qx '## Go' "$root/THIRD_PARTY_LICENSES.md" ||
		fail "beta $2" "THIRD_PARTY_LICENSES.md has no section ## Go, which the Beta's archive carries"
	dir=$(mktemp -d)
	chmod 755 "$dir" # the archive's ./, which tar can give the folder it unpacks in
	cp "$1" "$dir/hypercrux"
	chmod 755 "$dir/hypercrux"
	cp "$root/LICENSE" "$dir/LICENSE"
	notice >"$dir/NOTICE"
	licences >"$dir/THIRD_PARTY_LICENSES.md"
	cp "$root/beta/README.md" "$dir/README.md"
	out="hypercrux-beta_linux_$2.tar.gz"
	tar -czf "$out" -C "$dir" .
	rm -rf "$dir"
	note "beta $2" "$out holds $(tar -tzf "$out" | sed -n 's|^\./\(..*\)|\1|p' | sort | tr '\n' ' ' | sed 's/ $//')"
}

sums() { # dir
	[ -d "$1" ] || fail "SHA256SUMS" "$1 isn't a folder"
	for a in $archives; do
		[ -f "$1/$a" ] || fail "SHA256SUMS" "$1 has no $a"
	done
	# shellcheck disable=SC2086 # the archives' names have no spaces
	(cd "$1" && sha256sum $archives >SHA256SUMS)
	note "SHA256SUMS" "$(cat "$1/SHA256SUMS")"
}

notes() { # tag file [mode commit]
	version_ok "$1" && [ "${1#v}" != "$1" ] || fail "release notes" "$1 isn't a tag of the form vX.Y.Z-beta.N"
	sed "s/@TAG@/$1/g" >"$2" <<'EOF'
The HyperCrux Beta is a new engine for HyperCrux, written in Go with no SQLite and no C underneath. It keeps 0.x's Go API and its command, with SQL, key-value, graph and vector on one record, and holds the data in one append-only file, with a copy in memory in each process that opens it.

This is a pre-release. The Beta runs on Linux alone, on x86-64 and ARM64, and takes a subset of SQLite's SQL. 0.x stays the stable line, with its builds for Linux, macOS and Windows, and the repository's latest release stays 0.x's.

| System | File |
|---|---|
| Linux, x86-64 (static) | hypercrux-beta_linux_amd64.tar.gz |
| Linux, ARM64 (static) | hypercrux-beta_linux_arm64.tar.gz |

Each archive holds the Beta's `hypercrux` command, with its licence files and its README. SHA256SUMS lists the checksums.

From Go, import `github.com/hypercrux/hypercrux/beta/hypercrux` and fetch this version:

```sh
go get github.com/hypercrux/hypercrux/beta/hypercrux@@TAG@
```

[beta/README.md](https://github.com/hypercrux/hypercrux/blob/@TAG@/beta/README.md) says how to use the Beta, and [beta/MOVING.md](https://github.com/hypercrux/hypercrux/blob/@TAG@/beta/MOVING.md) how to move a program and its data across from 0.x.
EOF
	if [ $# -ge 4 ]; then
		case "$3" in
		dry-run) is="would be" ;;
		release | attach) is="is" ;;
		*) fail "release" "the mode is release, attach or dry-run, and $3 is none of them" ;;
		esac
		note "release" "The release $is \"HyperCrux $1\", tag $1 at $4, a pre-release, with $(echo "$archives" | sed 's/ /, /') and SHA256SUMS."
	fi
	cat "$2" | annotate notice "release notes"
	cat "$2"
}

stamp() { # version
	version_ok "$1" && [ "${1#v}" = "$1" ] || fail "dry run" "$1 isn't a version of the form X.Y.Z-beta.N"
	f="$root/beta/hypercrux/hypercrux.go"
	[ "$(grep -c '^const Version = ".*"$' "$f")" = 1 ] ||
		fail "dry run" "beta/hypercrux/hypercrux.go hasn't one line const Version = \"...\" to set"
	old=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$f")
	if [ "$old" = "$1" ]; then
		note "dry run" "Version is $1 in the code already, so the build is the commit's as it is"
		return
	fi
	sed "s/^const Version = \".*\"\$/const Version = \"$1\"/" "$f" >"$f.new"
	mv "$f.new" "$f"
	grep -qx "const Version = \"$1\"" "$f" || fail "dry run" "Version couldn't be set to $1"
	note "dry run" "Version set to $1 in place of $old, in this job's checkout alone, for the dry run"
}

[ $# -ge 1 ] || usage
cmd=$1
shift
case "$cmd" in
check)
	[ $# -eq 3 ] || usage
	given "$1" "$2"
	check_file "$bin" "$2"
	check_run "$bin" "$2" "$3"
	;;
file)
	[ $# -eq 2 ] || usage
	given "$1" "$2"
	check_file "$bin" "$2"
	;;
package)
	[ $# -eq 2 ] || usage
	given "$1" "$2"
	package "$bin" "$2"
	;;
sums)
	[ $# -eq 1 ] || usage
	sums "$1"
	;;
notes)
	[ $# -eq 2 ] || [ $# -eq 4 ] || usage
	notes "$@"
	;;
stamp)
	[ $# -eq 1 ] || usage
	stamp "$1"
	;;
*) usage ;;
esac
