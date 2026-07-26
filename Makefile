.PHONY: fmt lint test bench

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

