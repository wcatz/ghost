export GOTOOLCHAIN = auto
# --long on purpose: `git describe --tags` at an exact tag prints the bare tag,
# which selfupdate.IsRelease reads as a PUBLISHED release -- and `make build` /
# `make install` produce a binary nobody published. With --long the stamp is
# always "<tag>-<n>-g<sha>" (n=0 exactly at the tag), whose tail parses as a
# prerelease, so a local build is a development build and honours
# GHOST_DEV_FORBID_DATA_DIR. The release path does not use this: goreleaser
# stamps its own version.
VERSION ?= $(shell git describe --tags --always --dirty --long 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"
BINARY   = ghost

.PHONY: build test test-e2e test-race vet lint clean install

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY) ./cmd/ghost/

test:
	CGO_ENABLED=0 go test ./...

# End-to-end: builds the binary once and drives IT — every command, MCP tool,
# resource, prompt and lifecycle hook — in a sandbox with a fake harness and a
# fake embedding endpoint. Behind the `e2e` build tag, so `make test` and CI are
# unaffected: with the tag absent, every file in e2e/ is excluded and the ./...
# pattern skips the directory silently. `-count=1` because the suite's result
# depends on process boundaries, which the cache cannot speak for.
test-e2e:
	go test -tags e2e ./e2e/ -count=1 -timeout 15m

test-race:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run

clean:
	rm -f $(BINARY)

install: build
	cp $(BINARY) $(GOPATH)/bin/ 2>/dev/null || cp $(BINARY) ~/go/bin/
