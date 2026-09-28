#!/usr/bin/make -f

# Each target runs twice: with -race, and without it, because non-race amd64 builds replace the single sequencer's
# sync/atomic commit store with a plain release store (store_release_amd64.go) that race builds never compile.
test:
	go test -timeout=1s -short -race -covermode=atomic ./...
	go test -timeout=1s -short -count=1 ./...

test.long:
	go test -run TestEndToEnd -timeout=120s -race -covermode=atomic -v ./...
	go test -run TestEndToEnd -timeout=120s -count=1 -v ./...

benchmark:
	go test -bench=. -benchmem

compile:
	go build ./...

build: test compile

.PHONY: test test.long compile build benchmark
