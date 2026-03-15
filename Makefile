export GOTOOLCHAIN = auto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"
BINARY   = ghost

.PHONY: build test test-race vet lint clean install

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY) ./cmd/ghost/

test:
	CGO_ENABLED=0 go test ./...

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
