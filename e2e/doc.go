//go:build !e2e

// Package e2e is the end-to-end test layer, and this file is what makes its
// absence quiet.
//
// Every test file in this directory carries `//go:build e2e`, so with the tag
// absent the package has no Go files left and the `./...` pattern skips the
// directory silently. That is what keeps `go test ./...` and CI unchanged —
// which is verified, not assumed: `go test ./...` and `go vet ./...` both exit 0
// with this file present, and the e2e tests do not run.
//
// It is here for the OTHER command. `go test ./e2e/` and `go vet ./e2e/` name
// the directory directly, and the go tool then reports "build constraints
// exclude all Go files" for a package that has no non-test source at all — the
// error a developer meets the moment they try to run the suite the way the
// README in this directory tells them to. With this file the package always
// exists and always compiles, and the direct command is a clean "no test files"
// instead.
//
// Do not add anything else here. A second buildable file without the tag would
// make `go test ./...` compile the package on every default run, which is the
// one outcome this arrangement exists to prevent. The tests themselves are in
// the files beside it.
//
// See harness_test.go for the suite's design, and docs/architecture.md#testing
// for what it covers and how to run it.
package e2e
