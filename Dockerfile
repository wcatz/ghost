FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=$VERSION" -o ghost ./cmd/ghost

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /src/ghost /usr/local/bin/ghost
ENTRYPOINT ["ghost"]
CMD ["mcp"]
