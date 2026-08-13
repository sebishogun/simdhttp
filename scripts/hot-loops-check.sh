#!/usr/bin/env bash
# Fails if any hot loop disassembles with an indirect call.
#
# An indirect call is a call through a register: the target is not known until
# it happens, so it cannot be inlined, cannot be predicted as cheaply, and
# hides whatever it reaches from every gate that reads the disassembly. The
# functions listed here are the ones a request pays on every hit, and they are
# meant to contain none.
#
# Router.ServeHTTP is deliberately NOT in the list. Its indirect calls are
# w.Header(), w.WriteHeader() and the handler handoff -- calls through
# http.ResponseWriter and http.Handler, which are interfaces by definition and
# are the API this package exists to fit. The walk it performs is checked
# instead, under matchSegments.
set -euo pipefail

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

go test -c -o "$out/root.test" . >/dev/null
go test -c -o "$out/http1.test" ./http1 >/dev/null

check() {
	local bin=$1 sym=$2
	local dis
	dis=$(go tool objdump -s "$sym" "$bin" 2>/dev/null || true)
	if [ -z "$dis" ]; then
		echo "hot-loops-check: symbol not found: $sym" >&2
		return 1
	fi
	# A direct call names a symbol and ends in (SB). Anything else -- CALL CX,
	# CALL (DX), CALL 0x18(AX) -- is indirect.
	local bad
	bad=$(echo "$dis" | grep -E '\bCALL\b' | grep -vE '\bCALL [^ ]*\(SB\)' || true)
	if [ -n "$bad" ]; then
		echo "hot-loops-check: indirect call in $sym:" >&2
		echo "$bad" >&2
		return 1
	fi
	echo "  ok  $sym"
}

fail=0
echo "hot loops, root package:"
for sym in 'simdhttp\.matchSegments' 'simdhttp\.collectMethods' \
	'simdhttp\.decodeSegmentFast' 'simdhttp\.queryLookup' 'simdhttp\.cleanPath'; do
	check "$out/root.test" "$sym" || fail=1
done
echo "hot loops, http1:"
for sym in 'http1\.Parse' 'http1\.\(\*BodyReader\)\.readFixed' \
	'http1\.\(\*BodyReader\)\.readChunked' 'http1\.\(\*BodyReader\)\.readChunkHeader' \
	'http1\.\(\*BodyReader\)\.readLine'; do
	check "$out/http1.test" "$sym" || fail=1
done
exit $fail
