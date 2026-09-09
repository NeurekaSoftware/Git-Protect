// Package buildinfo exposes the build identity of the running binary.
package buildinfo

import (
	"os"
	"strings"
)

// Fallback values reported when the build metadata environment is absent, so
// local development runs identify themselves without a release tag.
const (
	FallbackVersion = "dev"
	FallbackCommit  = "unknown"
)

// Info is the build identity of a running binary, reported at startup and used
// in HTTP user agents.
type Info struct {
	Version string
	Commit  string
}

// LoadFromEnvironment reads the version and commit from GIT_TAG and GIT_HASH,
// falling back to "dev" and "unknown" when they are unset or blank.
func LoadFromEnvironment() Info {
	return Info{
		Version: readEnv("GIT_TAG", FallbackVersion),
		Commit:  readEnv("GIT_HASH", FallbackCommit),
	}
}

func readEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
