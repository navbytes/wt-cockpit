// Package buildinfo resolves the version string printed by `wt -version` /
// `wtd -version`.
package buildinfo

import "runtime/debug"

// Version returns the release version to display. ldflagsVersion is each
// binary's main.version, stamped at build time via
// -ldflags "-X main.version=..." by `make build`. When it's the plain-build
// fallback "dev" (e.g. a `go install github.com/.../cmd/wt@v0.3.0` build, which
// never runs the Makefile), fall back to the module version the Go toolchain
// records in the binary — so installed binaries report a real version instead
// of "dev".
func Version(ldflagsVersion string) string {
	if ldflagsVersion != "" && ldflagsVersion != "dev" {
		return ldflagsVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		// "(devel)" is what the toolchain records for a build from a working
		// tree with no version (e.g. `go build` in the repo) — no better than
		// the "dev" fallback, so keep the latter.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return ldflagsVersion
}
