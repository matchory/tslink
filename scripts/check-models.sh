#!/bin/bash
# check-models.sh - Model-check the TLA+ models with TLC, in a pinned container
#
# Usage:
#   scripts/check-models.sh [dir]    # dir defaults to test/models
#
# For each <module>.tla in dir (PlusCal translated in the file), the script
# checks that the committed translation is current, then runs TLC on:
#   <module>.cfg              must pass
#   <module>-mut-<name>.cfg   must fail on the property of its "\* Expect: <P>" line
#   <module>-known-<id>.cfg   like a mutation, plus "\* Known: <id>": a violation
#                             the code still has, which needs a Go test that
#                             calls knownViolation(t, "<id>")
# Every INVARIANT/PROPERTY of <module>.cfg must be the Expect of some -mut- cfg.
#
# Exits 0 only if every rule holds.
#
# CHECK_MODELS_SKIP_GO=1 skips the Go test check of known violations. It is for
# model work only, while the test does not exist yet.

set -euo pipefail

# Pinned tools. The image is eclipse-temurin 21 JRE, the jar the release of
# tlaplus/tlaplus named below; both are checked by digest before use.
JAVA_IMAGE="eclipse-temurin@sha256:227118764cccd3c3294617061aaa93a08309c63a395eefa83e6b35032992ea99"
TLA_VERSION="1.7.4"
TLA_URL="https://github.com/tlaplus/tlaplus/releases/download/v${TLA_VERSION}/tla2tools.jar"
TLA_SHA256="936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="${1:-test/models}"
JAR="${XDG_CACHE_HOME:-$HOME/.cache}/tslink/tla2tools-${TLA_VERSION}.jar"

failed=0
work=""

fail() {
	echo "FAIL $*"
	failed=1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

fetch_jar() {
	if [ ! -f "$JAR" ]; then
		mkdir -p "$(dirname "$JAR")"
		curl -fsSL -o "$JAR.tmp" "$TLA_URL"
		mv "$JAR.tmp" "$JAR"
	fi
	if [ "$(sha256 "$JAR")" != "$TLA_SHA256" ]; then
		echo "error: $JAR does not match the pinned checksum" >&2
		exit 2
	fi
}

# java runs the pinned JVM on the jar, in the work directory.
java_tla() {
	docker run --rm --user "$(id -u):$(id -g)" \
		-v "$JAR:/tla2tools.jar:ro" -v "$work:/work" -w /work \
		-e HOME=/work -e JAVA_TOOL_OPTIONS=-Duser.home=/work \
		"$JAVA_IMAGE" java -XX:+UseParallelGC -cp /tla2tools.jar "$@" 2>&1
}

# header prints the value of a "\* <key>: <value>" line of a cfg.
header() {
	sed -n 's/^\\\* '"$1"': *\([A-Za-z0-9_]*\) *$/\1/p' "$2" | head -n 1
}

# cfg_names prints the names the cfg lists after INVARIANT or PROPERTY keywords.
cfg_names() {
	awk -v kind="$1" '
		{ sub(/\\\*.*/, "") }
		{
			for (i = 1; i <= NF; i++) {
				if ($i ~ /^(SPECIFICATION|INIT|NEXT|CONSTANT|CONSTANTS|INVARIANT|INVARIANTS|PROPERTY|PROPERTIES|CONSTRAINT|CONSTRAINTS|SYMMETRY|VIEW|ALIAS)$/) {
					on = ($i == kind || $i == kind "S" || (kind == "PROPERTY" && $i == "PROPERTIES"))
				} else if (on) {
					print $i
				}
			}
		}' "$2"
}

# run_tlc runs TLC on module and cfg, and leaves its output in $work/tlc.out.
run_tlc() {
	rm -rf "${work:?}/states"
	cp "$DIR/$1.tla" "$2" "$work/"
	java_tla tlc2.TLC -workers auto -cleanup -metadir /work/states \
		-config "$(basename "$2")" "$1.tla" >"$work/tlc.out" || true
}

check_translation() {
	local module=$1
	cp "$DIR/$module.tla" "$work/$module.tla"
	if ! java_tla pcal.trans -nocfg "$module.tla" >"$work/pcal.out"; then
		fail "$module translation: $(tail -n 3 "$work/pcal.out")"
		return
	fi
	if ! diff -q "$DIR/$module.tla" "$work/$module.tla" >/dev/null; then
		fail "$module: the committed PlusCal translation is out of date"
		return
	fi
}

# check_failing checks a -mut- or -known- cfg: TLC must violate its Expect.
check_failing() {
	local module=$1 cfgfile=$2 kind=$3 name=$4
	local expect
	expect="$(header Expect "$cfgfile")"
	if [ -z "$expect" ]; then
		fail "$(basename "$cfgfile"): no \"\\* Expect: <Property>\" line"
		return
	fi
	run_tlc "$module" "$cfgfile"
	if grep -q "^Error: Invariant $expect is violated" "$work/tlc.out"; then
		:
	elif grep -q '^Error: Temporal properties were violated' "$work/tlc.out" &&
		[ "$(cfg_names PROPERTY "$cfgfile" | tr '\n' ' ')" = "$expect " ]; then
		:
	elif grep -q '^Model checking completed. No error has been found' "$work/tlc.out"; then
		echo "BROKEN $name (TLC accepts it)"
		failed=1
		return
	else
		fail "$name: TLC did not violate $expect: $(grep -m1 '^Error:' "$work/tlc.out" || tail -n 1 "$work/tlc.out")"
		return
	fi
	if [ "$kind" = known ]; then
		echo "EXPECTED FAILURE $(header Known "$cfgfile")"
	else
		echo "MUTATION CAUGHT $name"
	fi
}

# check_known_test checks that a Go test guards the known violation of a cfg.
check_known_test() {
	local cfgfile=$1 id=$2 header_id
	header_id="$(header Known "$cfgfile")"
	if [ "$(echo "$header_id" | tr '[:upper:]' '[:lower:]')" != "$id" ]; then
		fail "$(basename "$cfgfile"): \"\\* Known: $header_id\" does not match its file name"
	fi
	if [ "$(basename "$DIR")" = fixture ]; then
		echo "SKIPPED the Go test check of $id (fixture directory)"
		return
	fi
	if [ "${CHECK_MODELS_SKIP_GO:-}" = 1 ]; then
		return
	fi
	# Only files tracked in this tree count, and a commented-out call does not.
	if ! git -C "$ROOT" grep -hF -e "knownViolation(t, \"$id\")" -- '*_test.go' |
		grep -qv '^[[:space:]]*//'; then
		fail "$id: no Go test calls knownViolation(t, \"$id\")"
	fi
}

check_module() {
	local module=$1 start states cfgfile name p
	check_translation "$module"

	start=$SECONDS
	run_tlc "$module" "$DIR/$module.cfg"
	if grep -q '^Model checking completed. No error has been found' "$work/tlc.out"; then
		states="$(sed -n 's/^\([0-9]*\) states generated, \([0-9]*\) distinct states found.*/\2/p' "$work/tlc.out" | tail -n 1)"
		echo "PASS $module ($states states found, $((SECONDS - start)) s)"
	else
		fail "$module: $(grep -m1 '^Error:' "$work/tlc.out" || tail -n 1 "$work/tlc.out")"
	fi

	local expects=""
	for cfgfile in "$DIR/$module"-mut-*.cfg; do
		[ -e "$cfgfile" ] || continue
		name="$(basename "$cfgfile" .cfg)"
		name="${name#"$module"-mut-}"
		expects="$expects $(header Expect "$cfgfile")"
		check_failing "$module" "$cfgfile" mutation "$name"
	done
	for cfgfile in "$DIR/$module"-known-*.cfg; do
		[ -e "$cfgfile" ] || continue
		name="$(basename "$cfgfile" .cfg)"
		name="${name#"$module"-known-}"
		check_failing "$module" "$cfgfile" known "$name"
		check_known_test "$cfgfile" "$name"
	done

	for p in $(cfg_names INVARIANT "$DIR/$module.cfg") $(cfg_names PROPERTY "$DIR/$module.cfg"); do
		case " $expects " in
		*" $p "*) ;;
		*) fail "$module: $p has no -mut- cfg that expects it" ;;
		esac
	done
}

main() {
	local tla modules=0
	if [ "${CHECK_MODELS_SKIP_GO:-}" = 1 ]; then
		echo "SKIPPED the Go test check of known violations (CHECK_MODELS_SKIP_GO=1)"
	fi
	fetch_jar
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT
	for tla in "$DIR"/*.tla; do
		[ -e "$tla" ] || continue
		modules=$((modules + 1))
		check_module "$(basename "$tla" .tla)"
	done
	if [ "$modules" -eq 0 ]; then
		echo "error: no .tla files in $DIR" >&2
		exit 2
	fi
	exit "$failed"
}

main
