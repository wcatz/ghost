export GOTOOLCHAIN = auto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
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
