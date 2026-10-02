// Package integration holds Patch's end-to-end tests. They are guarded by the
// `integration` build tag so a plain `go test ./...` never needs a database or a
// second service binary, and they run the real Config and Patch binaries as child
// processes. This file exists so the package always has a buildable Go file even
// when the tag is off.
package integration
