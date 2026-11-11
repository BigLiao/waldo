# Release builds share one BUILD id across architectures so that A can
# install a matching binary on a B of a different architecture.
BUILD   ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M)
LDFLAGS := -s -w -X waldo/internal/proto.Build=$(BUILD)
export CGO_ENABLED = 0

.PHONY: build dev release test test-unit test-attack test-e2e vet clean

dev:            ## build ./waldo with a content-hash version (auto-reinstalls on B)
	go build -o waldo ./cmd/waldo

build:          ## build ./waldo with a release version id
	go build -ldflags '$(LDFLAGS)' -o waldo ./cmd/waldo

release:        ## static binaries for linux/amd64 and linux/arm64 in dist/
	mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/waldo-linux-amd64 ./cmd/waldo
	GOOS=linux GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/waldo-linux-arm64 ./cmd/waldo

vet:
	go vet ./...

test-unit:
	go test -count=1 ./internal/...

test-attack:    ## sandbox regression tests (needs bwrap, curl, python3)
	go test -p 1 -count=1 -v ./test/attack/

test-e2e:       ## full chain over a private loopback sshd (needs sshd, sshfs, bwrap)
	go test -p 1 -count=1 -v ./test/e2e/

test: vet test-unit test-attack test-e2e

clean:
	rm -rf waldo dist
