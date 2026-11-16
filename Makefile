# Release builds share one BUILD id across architectures so that A can
# install a matching binary on a B of a different architecture.
BUILD   ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M)
VERSION ?= 0.1.0
LDFLAGS := -s -w -X waldo/internal/proto.Version=$(VERSION) -X waldo/internal/proto.Build=$(BUILD)
export CGO_ENABLED = 0

.PHONY: build build-pair dev release test test-unit test-native test-attack test-e2e vet clean

dev:            ## build ./waldo with a content-hash version (auto-reinstalls on B)
	go build -o waldo ./cmd/waldo

build:          ## build ./waldo with a release version id
	go build -trimpath -ldflags '$(LDFLAGS)' -o waldo ./cmd/waldo

build-pair: build  ## native A plus both Linux B companions, with the same BUILD
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o waldo-linux-amd64 ./cmd/waldo
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o waldo-linux-arm64 ./cmd/waldo

release:        ## binaries for Linux and macOS, amd64 and arm64 in dist/
	mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/waldo-linux-amd64 ./cmd/waldo
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/waldo-linux-arm64 ./cmd/waldo
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/waldo-darwin-amd64 ./cmd/waldo
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/waldo-darwin-arm64 ./cmd/waldo
	cp install.sh dist/install.sh
	cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum waldo-linux-amd64 waldo-linux-arm64 waldo-darwin-amd64 waldo-darwin-arm64 install.sh > SHA256SUMS; else shasum -a 256 waldo-linux-amd64 waldo-linux-arm64 waldo-darwin-amd64 waldo-darwin-arm64 install.sh > SHA256SUMS; fi

vet:
	go vet ./...

test-unit:
	go test -count=1 ./internal/... ./test/install

test-native:    ## native macOS sandbox integration (macOS only)
	go test -p 1 -count=1 -v ./test/native

test-attack:    ## sandbox regression tests (needs bwrap, curl, python3)
	go test -p 1 -count=1 -v ./test/attack/

test-e2e:       ## full chain over a private loopback sshd (needs sshd, sshfs, bwrap)
	go test -p 1 -count=1 -v ./test/e2e/

test: vet test-unit test-attack test-e2e

clean:
	rm -rf waldo waldo-linux-amd64 waldo-linux-arm64 dist
