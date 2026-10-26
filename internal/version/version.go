// Package version reports the build version. Release builds set Version with
// -ldflags "-X github.com/noahkiss/heraldo/internal/version.Version=v1.2.3".
package version

import "runtime/debug"

// Version is the release version. It stays "dev" in a plain go build.
var Version = "dev"

// String returns Version, or the module version when Version was not set and
// the binary came from go install.
func String() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}
