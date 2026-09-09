package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/neurekadev/git-backup/internal/logging"
	"github.com/neurekadev/git-backup/internal/paths"
	"github.com/neurekadev/git-backup/internal/schedule"
	"gopkg.in/yaml.v3"
)

// rawSettings mirrors the YAML schema with optional fields so absent keys can
// be told apart from zero values.
type rawSettings struct {
	Logging      *rawLogging            `yaml:"logging"`
	Storage      *rawStorage            `yaml:"storage"`
	Credentials  map[string]*Credential `yaml:"credentials"`
	Repositories []*rawRepository       `yaml:"repositories"`
	Schedule     *rawSchedule           `yaml:"schedule"`
	Concurrency  *rawConcurrency        `yaml:"concurrency"`
	Health       *rawHealth             `yaml:"health"`
}

type rawLogging struct {
	LogLevel string `yaml:"logLevel"`
}

type rawStorage struct {
	Endpoint             string `yaml:"endpoint"`
	Region               string `yaml:"region"`
	AccessKeyID          string `yaml:"accessKeyId"`
	AccessKeyIDFile      string `yaml:"accessKeyIdFile"`
	SecretAccessKey      string `yaml:"secretAccessKey"`
	SecretAccessKeyFile  string `yaml:"secretAccessKeyFile"`
	ForcePathStyle       *bool  `yaml:"forcePathStyle"`
	PayloadSignatureMode string `yaml:"payloadSignatureMode"`
	Bucket               string `yaml:"bucket"`
	Retention            *int   `yaml:"retention"`
	RetentionMinimum     *int   `yaml:"retentionMinimum"`
}

type rawRepository struct {
	Mode       string  `yaml:"mode"`
	Provider   string  `yaml:"provider"`
	Urls       URLList `yaml:"url"`
	Credential string  `yaml:"credential"`
	BaseURL    string  `yaml:"baseUrl"`
	LFS        *bool   `yaml:"lfs"`
	Cache      *bool   `yaml:"cache"`
	Enabled    *bool   `yaml:"enabled"`

	IncludeStarred                *bool `yaml:"includeStarred"`
	IncludeSnippets               *bool `yaml:"includeSnippets"`
	IncludeIssues                 *bool `yaml:"includeIssues"`
	IncludeIssueArtifacts         *bool `yaml:"includeIssueArtifacts"`
	IncludeMergeRequests          *bool `yaml:"includeMergeRequests"`
	IncludeMergeRequestsArtifacts *bool `yaml:"includeMergeRequestsArtifacts"`
	IncludeReleases               *bool `yaml:"includeReleases"`
	IncludeReleaseArtifacts       *bool `yaml:"includeReleaseArtifacts"`
}

type rawSchedule struct {
	Repositories *rawJobSchedule `yaml:"repositories"`
}

type rawJobSchedule struct {
	Cron string `yaml:"cron"`
}

type rawConcurrency struct {
	Repositories *int `yaml:"repositories"`
	Metadata     *int `yaml:"metadata"`
}

type rawHealth struct {
	Port *int    `yaml:"port"`
	Bind *string `yaml:"bind"`
}

// URLList binds a YAML value that may be either a single scalar or a sequence
// of scalars, letting the url key accept one value (url: https://…) or many
// (url: followed by a list) without a second key.
type URLList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (u *URLList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" {
			*u = URLList{}
			return nil
		}
		*u = URLList{value.Value}
		return nil
	case yaml.SequenceNode:
		urls := make(URLList, 0, len(value.Content))
		for _, item := range value.Content {
			if item.Tag == "!!null" {
				continue
			}
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("url entries must be strings")
			}
			urls = append(urls, item.Value)
		}
		*u = urls
		return nil
	default:
		return fmt.Errorf("url must be a string or a list of strings")
	}
}

// Loader reads settings files into normalized Settings values.
type Loader struct{}

// NewLoader returns a settings loader.
func NewLoader() *Loader { return &Loader{} }

// Load reads, normalizes, secret-resolves, and validates the settings file at
// path. On failure it returns nil with every validation error; on success the
// error slice is nil.
func (l *Loader) Load(path string) (*Settings, []string) {
	if strings.TrimSpace(path) == "" {
		return nil, []string{"settings path is required."}
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, []string{fmt.Sprintf("settings file not found: '%s'", path)}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{fmt.Sprintf("failed to load settings '%s': %v", path, err)}
	}

	// Checked against the raw document before deserialization: a settings file
	// still using a deprecated key gets the migration message rather than an
	// unrecognized-key error.
	if errors := validateDeprecatedKeys(content); len(errors) > 0 {
		return nil, errors
	}

	raw, err := decode(content)
	if err != nil {
		return nil, []string{fmt.Sprintf("YAML parse error in '%s': %v", path, err)}
	}

	// Reject a present-but-unrecognized enum value before normalization coerces
	// it to a default — otherwise a typo like `logLevel: verbse` would be
	// silently accepted as info.
	if errors := validateRawEnums(raw); len(errors) > 0 {
		return nil, errors
	}

	settings := normalize(raw)

	// Resolve secrets before validation, so the required-value checks below see
	// what will actually be used rather than the placeholder standing in for it.
	var secretErrors []string
	resolveSecrets(settings, &secretErrors)
	if len(secretErrors) > 0 {
		return nil, secretErrors
	}

	if errors := validate(settings); len(errors) > 0 {
		return nil, errors
	}
	return settings, nil
}

func decode(content []byte) (*rawSettings, error) {
	raw := &rawSettings{}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	// Unrecognized keys are rejected rather than dropped: a typo like
	// `includeIsues: true` would otherwise be accepted in silence and simply
	// never back that data up.
	decoder.KnownFields(true)
	if err := decoder.Decode(raw); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is an empty configuration; validation reports what
			// is missing.
			return &rawSettings{}, nil
		}
		return nil, err
	}
	return raw, nil
}

func normalize(raw *rawSettings) *Settings {
	settings := &Settings{
		Credentials:  make(map[string]*Credential, len(raw.Credentials)),
		Repositories: make([]*Repository, 0, len(raw.Repositories)),
	}

	settings.Logging.LogLevel = logging.DefaultLevelName
	if raw.Logging != nil {
		settings.Logging.LogLevel = normalizeLogLevel(raw.Logging.LogLevel)
	}

	if raw.Storage != nil {
		storage := raw.Storage
		settings.Storage = Storage{
			Endpoint:             storage.Endpoint,
			Region:               storage.Region,
			AccessKeyID:          storage.AccessKeyID,
			AccessKeyIDFile:      storage.AccessKeyIDFile,
			SecretAccessKey:      storage.SecretAccessKey,
			SecretAccessKeyFile:  storage.SecretAccessKeyFile,
			ForcePathStyle:       storage.ForcePathStyle != nil && *storage.ForcePathStyle,
			PayloadSignatureMode: normalizeSignatureMode(storage.PayloadSignatureMode),
			Bucket:               storage.Bucket,
			Retention:            dereferenceInt(storage.Retention),
			RetentionMinimum:     dereferenceIntOr(storage.RetentionMinimum, 1),
		}
	}

	for name, credential := range raw.Credentials {
		if credential == nil {
			credential = &Credential{}
		}
		credential.Name = name
		settings.Credentials[strings.ToLower(name)] = credential
	}

	for _, repository := range raw.Repositories {
		if repository == nil {
			settings.Repositories = append(settings.Repositories, nil)
			continue
		}
		settings.Repositories = append(settings.Repositories, normalizeRepository(repository))
	}

	if raw.Schedule != nil && raw.Schedule.Repositories != nil {
		settings.Schedule.Repositories.Cron = raw.Schedule.Repositories.Cron
	}

	settings.Concurrency.Repositories = 1
	settings.Concurrency.Metadata = 1
	if raw.Concurrency != nil {
		settings.Concurrency.Repositories = dereferenceIntOr(raw.Concurrency.Repositories, 1)
		settings.Concurrency.Metadata = dereferenceIntOr(raw.Concurrency.Metadata, 1)
	}

	settings.Health.Port = 8080
	settings.Health.Bind = "localhost"
	if raw.Health != nil {
		if raw.Health.Port != nil {
			settings.Health.Port = *raw.Health.Port
		}
		if raw.Health.Bind != nil {
			settings.Health.Bind = *raw.Health.Bind
		}
	}

	return settings
}

func normalizeRepository(raw *rawRepository) *Repository {
	repository := &Repository{
		Mode:       strings.ToLower(strings.TrimSpace(raw.Mode)),
		Provider:   strings.ToLower(strings.TrimSpace(raw.Provider)),
		Credential: raw.Credential,
		BaseURL:    raw.BaseURL,
		LFS:        dereferenceBoolOr(raw.LFS, true),
		Cache:      dereferenceBoolOr(raw.Cache, true),
		Enabled:    dereferenceBoolOr(raw.Enabled, true),
	}

	for _, value := range raw.Urls {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			repository.Urls = append(repository.Urls, trimmed)
		}
	}

	repository.IncludeStarred = dereferenceBool(raw.IncludeStarred)
	repository.IncludeSnippets = dereferenceBool(raw.IncludeSnippets)
	repository.IncludeIssues = dereferenceBool(raw.IncludeIssues)
	repository.IncludeIssueArtifacts = dereferenceBool(raw.IncludeIssueArtifacts)
	repository.IncludeMergeRequests = dereferenceBool(raw.IncludeMergeRequests)
	repository.IncludeMergeRequestsArtifacts = dereferenceBool(raw.IncludeMergeRequestsArtifacts)
	repository.IncludeReleases = dereferenceBool(raw.IncludeReleases)
	repository.IncludeReleaseArtifacts = dereferenceBool(raw.IncludeReleaseArtifacts)

	return repository
}

func normalizeLogLevel(configured string) string {
	if _, ok := logging.ParseLevel(configured); ok {
		return strings.ToLower(strings.TrimSpace(configured))
	}
	return logging.DefaultLevelName
}

func normalizeSignatureMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SignatureStreaming:
		return SignatureStreaming
	case SignatureUnsigned:
		return SignatureUnsigned
	default:
		return SignatureFull
	}
}

func dereferenceBool(value *bool) bool {
	return value != nil && *value
}

func dereferenceBoolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func dereferenceInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func dereferenceIntOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func validate(settings *Settings) []string {
	var errors []string
	validateStorage(settings, &errors)
	validateRepositories(settings, &errors)
	validateSchedule(settings, &errors)
	validateConcurrency(settings, &errors)
	validateHealth(settings, &errors)
	return errors
}

func validateStorage(settings *Settings, errors *[]string) {
	storage := settings.Storage
	if strings.TrimSpace(storage.Endpoint) == "" {
		*errors = append(*errors, "storage.endpoint is required.")
	} else if _, ok := paths.ParseHTTPURL(storage.Endpoint); !ok {
		*errors = append(*errors, "storage.endpoint must be an absolute http or https URL.")
	}

	if strings.TrimSpace(storage.Region) == "" {
		*errors = append(*errors, "storage.region is required.")
	}
	if strings.TrimSpace(storage.AccessKeyID) == "" {
		*errors = append(*errors, "storage.accessKeyId is required.")
	}
	if strings.TrimSpace(storage.SecretAccessKey) == "" {
		*errors = append(*errors, "storage.secretAccessKey is required.")
	}
	if strings.TrimSpace(storage.Bucket) == "" {
		*errors = append(*errors, "storage.bucket is required.")
	}
	if storage.RetentionMinimum < 0 {
		*errors = append(*errors, "storage.retentionMinimum must be 0 or greater.")
	}
}

func validateRepositories(settings *Settings, errors *[]string) {
	for i, repository := range settings.Repositories {
		if repository == nil {
			*errors = append(*errors, fmt.Sprintf("repositories[%d] is required.", i))
			continue
		}

		if strings.TrimSpace(repository.Mode) == "" {
			*errors = append(*errors, fmt.Sprintf("repositories[%d].mode is required.", i))
			continue
		}

		switch repository.Mode {
		case ModeProvider:
			validateProviderRepository(settings, repository, i, errors)
		case ModeURL:
			validateURLRepository(settings, repository, i, errors)
		default:
			*errors = append(*errors, fmt.Sprintf(
				"repositories[%d].mode '%s' is not supported. Supported values: %s, %s.",
				i, repository.Mode, ModeProvider, ModeURL))
		}
	}
}

func validateProviderRepository(settings *Settings, repository *Repository, index int, errors *[]string) {
	label := fmt.Sprintf("repositories[%d]", index)

	if strings.TrimSpace(repository.Provider) == "" {
		*errors = append(*errors, label+".provider is required when mode is provider.")
	} else if repository.Provider != ProviderGitHub && repository.Provider != ProviderGitLab && repository.Provider != ProviderForgejo {
		*errors = append(*errors, fmt.Sprintf(
			"%s.provider '%s' is not supported. Supported values: %s, %s, %s.",
			label, repository.Provider, ProviderGitHub, ProviderGitLab, ProviderForgejo))
	}

	if strings.TrimSpace(repository.Credential) == "" {
		*errors = append(*errors, label+".credential is required when mode is provider.")
	} else if _, known := settings.Credentials[strings.ToLower(repository.Credential)]; !known {
		*errors = append(*errors, fmt.Sprintf("%s.credential references unknown credential '%s'.", label, repository.Credential))
	}

	if len(repository.Urls) > 0 {
		*errors = append(*errors, label+".url is not allowed when mode is provider.")
	}

	if strings.TrimSpace(repository.BaseURL) != "" {
		if _, ok := paths.ParseHTTPURL(repository.BaseURL); !ok {
			*errors = append(*errors, label+".baseUrl must be an absolute http or https URL.")
		}
	}

	if repository.IncludeIssueArtifacts && !repository.IncludeIssues {
		*errors = append(*errors, label+".includeIssueArtifacts requires includeIssues.")
	}
	if repository.IncludeMergeRequestsArtifacts && !repository.IncludeMergeRequests {
		*errors = append(*errors, label+".includeMergeRequestsArtifacts requires includeMergeRequests.")
	}
	if repository.IncludeReleaseArtifacts && !repository.IncludeReleases {
		*errors = append(*errors, label+".includeReleaseArtifacts requires includeReleases.")
	}
}

func validateURLRepository(settings *Settings, repository *Repository, index int, errors *[]string) {
	label := fmt.Sprintf("repositories[%d]", index)

	if len(repository.Urls) == 0 {
		*errors = append(*errors, label+".url is required when mode is url.")
	} else {
		for j, value := range repository.Urls {
			if _, ok := paths.ParseHTTPURL(value); !ok {
				*errors = append(*errors, fmt.Sprintf("%s.url[%d] must be an absolute http or https URL.", label, j))
			}
		}
	}

	if strings.TrimSpace(repository.Provider) != "" {
		*errors = append(*errors, label+".provider is not allowed when mode is url.")
	}
	if strings.TrimSpace(repository.BaseURL) != "" {
		*errors = append(*errors, label+".baseUrl is not allowed when mode is url.")
	}

	// Every include* flag is provider-only, so none are allowed in url mode.
	disallowed := []struct {
		name  string
		value bool
	}{
		{"includeStarred", repository.IncludeStarred},
		{"includeSnippets", repository.IncludeSnippets},
		{"includeIssues", repository.IncludeIssues},
		{"includeIssueArtifacts", repository.IncludeIssueArtifacts},
		{"includeMergeRequests", repository.IncludeMergeRequests},
		{"includeMergeRequestsArtifacts", repository.IncludeMergeRequestsArtifacts},
		{"includeReleases", repository.IncludeReleases},
		{"includeReleaseArtifacts", repository.IncludeReleaseArtifacts},
	}
	for _, flag := range disallowed {
		if flag.value {
			*errors = append(*errors, fmt.Sprintf("%s.%s is not allowed when mode is url.", label, flag.name))
		}
	}

	if strings.TrimSpace(repository.Credential) == "" {
		return
	}
	if _, known := settings.Credentials[strings.ToLower(repository.Credential)]; !known {
		*errors = append(*errors, fmt.Sprintf("%s.credential references unknown credential '%s'.", label, repository.Credential))
	}
}

func validateSchedule(settings *Settings, errors *[]string) {
	if _, err := schedule.Parse(settings.Schedule.Repositories.Cron); err != nil {
		*errors = append(*errors, fmt.Sprintf("schedule.repositories.cron is invalid: %v", err))
	}
}

func validateConcurrency(settings *Settings, errors *[]string) {
	if settings.Concurrency.Repositories < 1 {
		*errors = append(*errors, "concurrency.repositories must be 1 or greater.")
	}
	if settings.Concurrency.Metadata < 1 {
		*errors = append(*errors, "concurrency.metadata must be 1 or greater.")
	}
}

func validateHealth(settings *Settings, errors *[]string) {
	if settings.Health.Port < 0 || settings.Health.Port > 65535 {
		*errors = append(*errors, "health.port must be between 0 and 65535.")
	}
	if strings.TrimSpace(settings.Health.Bind) == "" {
		*errors = append(*errors, "health.bind must not be empty.")
	}
}

// validateRawEnums checks enum-like fields against their raw, pre-normalize
// values: both are optional (blank falls back to a default), so only a value
// that is present and unrecognized is reported — normalization would otherwise
// coerce it to a default and hide the mistake.
func validateRawEnums(raw *rawSettings) []string {
	var errors []string

	var logLevel string
	if raw.Logging != nil {
		logLevel = raw.Logging.LogLevel
	}
	if strings.TrimSpace(logLevel) != "" {
		if _, ok := logging.ParseLevel(logLevel); !ok {
			errors = append(errors, fmt.Sprintf("logging.logLevel '%s' is invalid. Supported values: %s.",
				logLevel, strings.Join(logging.SupportedLevelNames(), ", ")))
		}
	}

	var signatureMode string
	if raw.Storage != nil {
		signatureMode = raw.Storage.PayloadSignatureMode
	}
	if strings.TrimSpace(signatureMode) != "" {
		// Matched case-insensitively but without trimming, mirroring the
		// original validator.
		switch strings.ToLower(signatureMode) {
		case SignatureFull, SignatureStreaming, SignatureUnsigned:
		default:
			errors = append(errors, fmt.Sprintf("storage.payloadSignatureMode '%s' is invalid. Supported values: %s, %s, %s.",
				signatureMode, SignatureFull, SignatureStreaming, SignatureUnsigned))
		}
	}

	return errors
}

// validateDeprecatedKeys detects settings keys the schema retired, so a file
// still using them gets the migration message rather than an unrecognized-key
// error. Parse errors are swallowed: the main deserialization path reports
// them.
func validateDeprecatedKeys(content []byte) []string {
	var errors []string
	if len(bytes.TrimSpace(content)) == 0 {
		return errors
	}

	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return errors
	}
	if len(document.Content) == 0 {
		return errors
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors
	}

	findKey := func(mapping *yaml.Node, key string) *yaml.Node {
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			if strings.EqualFold(mapping.Content[i].Value, key) {
				return mapping.Content[i+1]
			}
		}
		return nil
	}

	if findKey(root, "backups") != nil {
		errors = append(errors, "backups is no longer supported. Use repositories entries with mode: provider.")
	}
	if findKey(root, "mirrors") != nil {
		errors = append(errors, "mirrors is no longer supported. Use repositories entries with mode: url.")
	}

	scheduleNode := findKey(root, "schedule")
	if scheduleNode == nil || scheduleNode.Kind != yaml.MappingNode {
		return errors
	}
	if findKey(scheduleNode, "backups") != nil {
		errors = append(errors, "schedule.backups is no longer supported. Use schedule.repositories.cron.")
	}
	if findKey(scheduleNode, "mirrors") != nil {
		errors = append(errors, "schedule.mirrors is no longer supported. Use schedule.repositories.cron.")
	}
	return errors
}
