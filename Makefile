# Benchmark and verification targets, matching the simd/simdjson house
# style: bare gates (no tail without pipefail), minima, shuffled.
.PHONY: test bench bench-check vet

test:
	go test ./...

vet:
	gofmt -l . ; go vet ./...

# bench runs the shape sweep and the head-to-head, one process, shuffled,
# minimum of the count -- the numbers the README quotes.
bench:
	go test -run '^$$' -bench . -benchmem -count=6 -shuffle=on .

# bench-check fails if a row regressed past 8% against the committed
# baseline; regenerate the baseline with `make bench > testdata/bench.txt`.
bench-check:
	@go test -run '^$$' -bench . -count=6 -shuffle=on . | tee /tmp/simdhttp-bench.txt
	@echo "compare /tmp/simdhttp-bench.txt against testdata/bench.txt (8% floor)"
