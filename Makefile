.PHONY: fmt lint test bench fuzz

# Per-target budget for `make fuzz`; override for a longer soak
# (make fuzz FUZZTIME=10m).
FUZZTIME ?= 30s

fmt:
	gofmt -s -w .
	go tool -modfile=internal/tools/go.mod goimports -w .

lint:
	go tool -modfile=internal/tools/go.mod golangci-lint run

test:
	go test -race -v ./...
	go test -count=1 -run TestHotPathAllocations .

bench:
	go test -run '^$$' -bench . -benchmem -count=8 .

# go test fuzzes one target per run, so drive them one by one. The seed
# corpora (and any committed regression input) already run as part of
# `make test`; this is the generative pass.
fuzz:
	@for target in $$(go test -list '^Fuzz' . | grep '^Fuzz'); do \
		echo "==> $$target ($(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$$target$$" -fuzztime $(FUZZTIME) . || exit 1; \
	done
