export GOTOOLCHAIN = auto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"
TAGS     = -tags sqlite_fts5
BINARY   = ghost

.PHONY: build test vet clean install

build:
	CGO_ENABLED=1 go build $(TAGS) $(LDFLAGS) -o $(BINARY) ./cmd/ghost/

test:
	CGO_ENABLED=1 go test $(TAGS) ./...

vet:
	go vet $(TAGS) ./...

clean:
	rm -f $(BINARY)

install: build
	cp $(BINARY) $(GOPATH)/bin/ 2>/dev/null || cp $(BINARY) ~/go/bin/
