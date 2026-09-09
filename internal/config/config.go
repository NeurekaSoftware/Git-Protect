// Package config loads, validates, and hot-reloads the daemon's settings file.
//
// The YAML schema is deliberately strict: unrecognized keys are rejected rather
// than dropped, because a typo like `includeIsues: true` would otherwise be
// accepted in silence and simply never back that data up. Loading normalizes
// defaults, resolves secrets, and validates before the daemon starts; the
// normalized Settings value is what the rest of the daemon reads.
package config

// Canonical values for enum-like settings, centralized so the validator, the
// normalizer, and the storage client cannot drift apart.
const (
	// Log levels.
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"

	// Payload signature modes.
	SignatureFull      = "full"
	SignatureStreaming = "streaming"
	SignatureUnsigned  = "unsigned"

	// Repository job modes.
	ModeProvider = "provider"
	ModeURL      = "url"

	// Repository providers.
	ProviderGitHub  = "github"
	ProviderGitLab  = "gitlab"
	ProviderForgejo = "forgejo"
)

// DefaultGitUsername is used for HTTP basic auth when a credential does not
// name one; forges ignore the username and match the token.
const DefaultGitUsername = "git"

// Settings is the normalized, default-resolved configuration the daemon runs
// on. It is produced only by a successful Load.
type Settings struct {
	Logging      Logging
	Storage      Storage
	Credentials  map[string]*Credential
	Repositories []*Repository
	Schedule     Schedule
	Concurrency  Concurrency
	Health       Health
}

// Logging controls console verbosity.
type Logging struct {
	LogLevel string
}

// Storage describes the S3-compatible destination and snapshot retention.
type Storage struct {
	Endpoint             string
	Region               string
	AccessKeyID          string
	AccessKeyIDFile      string
	SecretAccessKey      string
	SecretAccessKeyFile  string
	ForcePathStyle       bool
	PayloadSignatureMode string
	Bucket               string
	// Retention is the configured snapshot age limit in days; zero or less
	// disables retention entirely.
	Retention int
	// RetentionMinimum is how many newest snapshots are always kept.
	RetentionMinimum int
}

// Credential is one named forge credential. Name preserves the key's original
// casing from the settings file for error messages; lookups are
// case-insensitive via the map the loader builds.
type Credential struct {
	Name       string `yaml:"-"`
	Username   string `yaml:"username"`
	APIKey     string `yaml:"apiKey"`
	APIKeyFile string `yaml:"apiKeyFile"`
}

// Repository is one normalized backup job.
type Repository struct {
	Mode       string
	Provider   string
	Urls       []string
	Credential string
	BaseURL    string
	LFS        bool
	Cache      bool
	Enabled    bool

	IncludeStarred                bool
	IncludeSnippets               bool
	IncludeIssues                 bool
	IncludeIssueArtifacts         bool
	IncludeMergeRequests          bool
	IncludeMergeRequestsArtifacts bool
	IncludeReleases               bool
	IncludeReleaseArtifacts       bool
}

// Schedule holds per-job cron schedules.
type Schedule struct {
	Repositories JobSchedule
}

// JobSchedule is the cron schedule of one job.
type JobSchedule struct {
	Cron string
}

// Concurrency bounds parallelism within a sync run. Both default to 1 (fully
// sequential, the historical behavior).
type Concurrency struct {
	Repositories int
	Metadata     int
}

// Health configures the optional HTTP status listener.
type Health struct {
	// Port is the listening port; zero disables the listener.
	Port int
	// Bind is the interface address to listen on, e.g. "localhost" or
	// "0.0.0.0".
	Bind string
}
