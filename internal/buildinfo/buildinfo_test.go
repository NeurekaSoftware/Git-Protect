package buildinfo

import (
	"testing"
)

func TestLoadFromEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		tag         string
		hash        string
		wantVersion string
		wantCommit  string
	}{
		{name: "unset falls back", wantVersion: FallbackVersion, wantCommit: FallbackCommit},
		{name: "blank falls back", tag: "   ", hash: "\t", wantVersion: FallbackVersion, wantCommit: FallbackCommit},
		{name: "values trimmed", tag: " edge ", hash: "abc123", wantVersion: "edge", wantCommit: "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GIT_TAG", tt.tag)
			t.Setenv("GIT_HASH", tt.hash)
			got := LoadFromEnvironment()
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.Commit != tt.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tt.wantCommit)
			}
		})
	}
}
