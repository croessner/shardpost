// Package version holds the release version, set at build time with
// -ldflags "-X shardpost/internal/version.Version=<v>".
package version

// Version is the release version ("dev" in untagged builds).
var Version = "dev"
