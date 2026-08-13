# Gates for simdhttp. House style: bare gates, no pipe carrying a verdict,
# minima over repeated runs, shuffled order.
#
# A gate piped through tee or tail reports the pipe's exit status, so a red run
# leaves as green. Every target here either avoids the pipe or sets pipefail.
.PHONY: test race vet fuzz-smoke cross-arch tiers hot-loops-check bench bench-baseline bench-check check verify

FUZZTIME ?= 15s

test:
	go test ./...

race:
	go test -race ./...

vet:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

# Every fuzz target gets a smoke run. A crash writes a seed into testdata,
# which is a regression asset and is committed.
fuzz-smoke:
	go test -run '^$$' -fuzz FuzzParseAgainstNetHTTP -fuzztime $(FUZZTIME) ./http1
	go test -run '^$$' -fuzz FuzzBodyReader -fuzztime $(FUZZTIME) ./http1
	go test -run '^$$' -fuzz FuzzBodyDrainLeavesPipelined -fuzztime $(FUZZTIME) ./http1

# The parser reaches architecture kernels through simd, so every architecture
# it claims must at least compile and vet. Running them needs emulation and
# lives in the simd repository's lanes.
cross-arch:
	@for arch in arm64 s390x ppc64le riscv64 loong64; do \
		echo "== GOARCH=$$arch =="; \
		GOARCH=$$arch GOOS=linux go build ./... || exit 1; \
		GOARCH=$$arch GOOS=linux go vet ./... || exit 1; \
	done

# The portable path must give the same answers as the kernels. GOSIMD forces a
# tier at run time; scalar is the floor every other tier is checked against.
tiers:
	GOSIMD=scalar go test ./...
	go test -tags purego ./...

hot-loops-check:
	./scripts/hot-loops-check.sh

bench:
	go test -run '^$$' -bench . -benchmem -count=6 -shuffle=on .

# Regenerate the baseline on a quiet machine (load average under 1): the floor
# compares wall-clock, and a baseline captured under load is a slow baseline
# that hides real regressions. The header records the load it was taken at.
bench-baseline:
	@echo "# captured $$(date -u +%Y-%m-%dT%H:%M:%SZ) load:$$(cut -d' ' -f1 /proc/loadavg) $$(go version)" > testdata/bench.txt
	go test -run '^$$' -bench . -count=6 -shuffle=on . >> testdata/bench.txt

bench-check:
	./scripts/bench-check.sh

# check is the fast gate: everything that is machine-independent.
check: vet test race tiers hot-loops-check cross-arch
	@echo "check: green"

# verify is the full gate. bench-check compares wall-clock, so run this on a
# quiet machine (load average under 1) or it reports the machine rather than
# the code.
verify: check fuzz-smoke bench-check
	@echo "verify: green"
