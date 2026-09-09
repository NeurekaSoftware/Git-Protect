package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSettings = `logging:
  logLevel: debug
storage:
  endpoint: http://localhost:9000
  region: us-east-1
  accessKeyId: test-key
  secretAccessKey: test-secret
  bucket: backups
  retention: 30
  retentionMinimum: 2
credentials:
  token:
    username: octo
    apiKey: key-123
repositories:
  - mode: provider
    provider: github
    credential: token
    includeIssues: true
    includeIssueArtifacts: true
schedule:
  repositories:
    cron: "0 */6 * * *"
concurrency:
  repositories: 2
  metadata: 3
`

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadContent(t *testing.T, content string) (*Settings, []string) {
	t.Helper()
	return NewLoader().Load(writeSettings(t, content))
}

func TestLoadValidSettingsAppliesDefaults(t *testing.T) {
	settings, errors := loadContent(t, validSettings)
	if len(errors) != 0 {
		t.Fatalf("unexpected validation errors: %v", errors)
	}

	if settings.Logging.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", settings.Logging.LogLevel)
	}
	storage := settings.Storage
	if storage.Endpoint != "http://localhost:9000" || storage.Region != "us-east-1" ||
		storage.AccessKeyID != "test-key" || storage.SecretAccessKey != "test-secret" ||
		storage.Bucket != "backups" || storage.Retention != 30 || storage.RetentionMinimum != 2 {
		t.Errorf("storage not bound as configured: %+v", storage)
	}
	if storage.ForcePathStyle {
		t.Error("ForcePathStyle should default to false")
	}
	if storage.PayloadSignatureMode != SignatureFull {
		t.Errorf("PayloadSignatureMode = %q, want full", storage.PayloadSignatureMode)
	}

	credential, ok := settings.Credentials["token"]
	if !ok || credential.Name != "token" || credential.Username != "octo" || credential.APIKey != "key-123" {
		t.Errorf("credential not bound as configured: %+v (found=%v)", credential, ok)
	}

	if len(settings.Repositories) != 1 {
		t.Fatalf("Repositories length = %d, want 1", len(settings.Repositories))
	}
	repository := settings.Repositories[0]
	if !repository.Enabled || !repository.LFS || !repository.Cache {
		t.Error("enabled/lfs/cache should default to true")
	}
	if repository.IncludeIssues != true {
		t.Error("includeIssues should be true as configured")
	}
	if repository.IncludeIssueArtifacts != true {
		t.Error("includeIssueArtifacts should be true as configured")
	}
	if repository.IncludeStarred || repository.IncludeSnippets || repository.IncludeMergeRequests ||
		repository.IncludeMergeRequestsArtifacts || repository.IncludeReleases || repository.IncludeReleaseArtifacts {
		t.Error("unset include flags should default to false")
	}

	if settings.Schedule.Repositories.Cron != "0 */6 * * *" {
		t.Errorf("cron = %q", settings.Schedule.Repositories.Cron)
	}
	if settings.Concurrency.Repositories != 2 || settings.Concurrency.Metadata != 3 {
		t.Errorf("concurrency = %+v", settings.Concurrency)
	}
	if settings.Health.Port != 8080 || settings.Health.Bind != "localhost" {
		t.Errorf("health defaults wrong: %+v", settings.Health)
	}
}

func TestLoadMinimalSettings(t *testing.T) {
	minimal := `storage:
  endpoint: http://127.0.0.1:9000
  region: r1
  accessKeyId: k
  secretAccessKey: s
  bucket: b
schedule:
  repositories:
    cron: "0 0 * * *"
`
	settings, errors := loadContent(t, minimal)
	if len(errors) != 0 {
		t.Fatalf("unexpected validation errors: %v", errors)
	}
	if settings.Logging.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want the info default", settings.Logging.LogLevel)
	}
	if settings.Storage.Retention != 0 || settings.Storage.RetentionMinimum != 1 {
		t.Errorf("retention defaults wrong: %+v", settings.Storage)
	}
	if settings.Concurrency.Repositories != 1 || settings.Concurrency.Metadata != 1 {
		t.Errorf("concurrency should default to 1/1: %+v", settings.Concurrency)
	}
	if len(settings.Repositories) != 0 || len(settings.Credentials) != 0 {
		t.Error("repositories and credentials should default to empty")
	}
	if settings.Health.Port != 8080 || settings.Health.Bind != "localhost" {
		t.Errorf("health should default to 8080/localhost: %+v", settings.Health)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	base := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
credentials:
  token:
    apiKey: key
schedule:
  repositories:
    cron: "0 * * * *"
`

	tests := []struct {
		name       string
		content    string
		wantErrors []string
	}{
		{
			name:       "missing endpoint",
			content:    "storage:\n  region: r\n  accessKeyId: k\n  secretAccessKey: s\n  bucket: b\nschedule:\n  repositories:\n    cron: \"0 * * * *\"\n",
			wantErrors: []string{"storage.endpoint is required."},
		},
		{
			name: "endpoint not a URL",
			content: `storage:
  endpoint: not-a-url
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`,
			wantErrors: []string{"storage.endpoint must be an absolute http or https URL."},
		},
		{
			name: "several required fields missing",
			content: `storage:
  endpoint: http://127.0.0.1:9000
schedule:
  repositories:
    cron: "0 * * * *"
`,
			wantErrors: []string{
				"storage.region is required.",
				"storage.accessKeyId is required.",
				"storage.secretAccessKey is required.",
				"storage.bucket is required.",
			},
		},
		{
			name: "negative retention minimum",
			content: `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
  retentionMinimum: -1
schedule:
  repositories:
    cron: "0 * * * *"
`,
			wantErrors: []string{"storage.retentionMinimum must be 0 or greater."},
		},
		{
			name:       "empty file",
			content:    "",
			wantErrors: []string{"storage.endpoint is required.", "storage.region is required.", "storage.accessKeyId is required.", "storage.secretAccessKey is required.", "storage.bucket is required.", "schedule.repositories.cron is invalid: cron expression is required."},
		},
		{
			name: "repository mode missing",
			content: base + `repositories:
  - provider: github
    credential: token
`,
			wantErrors: []string{"repositories[0].mode is required."},
		},
		{
			name: "repository mode unsupported",
			content: base + `repositories:
  - mode: rsync
`,
			wantErrors: []string{"repositories[0].mode 'rsync' is not supported. Supported values: provider, url."},
		},
		{
			name: "null repository entry",
			content: base + `repositories:
  -
`,
			wantErrors: []string{"repositories[0] is required."},
		},
		{
			name: "provider missing",
			content: base + `repositories:
  - mode: provider
    credential: token
`,
			wantErrors: []string{"repositories[0].provider is required when mode is provider."},
		},
		{
			name: "provider unsupported",
			content: base + `repositories:
  - mode: provider
    provider: sourcehut
    credential: token
`,
			wantErrors: []string{"repositories[0].provider 'sourcehut' is not supported. Supported values: github, gitlab, forgejo."},
		},
		{
			name: "credential missing",
			content: base + `repositories:
  - mode: provider
    provider: github
`,
			wantErrors: []string{"repositories[0].credential is required when mode is provider."},
		},
		{
			name: "credential unknown",
			content: base + `repositories:
  - mode: provider
    provider: github
    credential: other
`,
			wantErrors: []string{"repositories[0].credential references unknown credential 'other'."},
		},
		{
			name: "credential name matching is case insensitive",
			content: base + `repositories:
  - mode: provider
    provider: github
    credential: TOKEN
`,
			wantErrors: nil,
		},
		{
			name: "url not allowed in provider mode",
			content: base + `repositories:
  - mode: provider
    provider: github
    credential: token
    url: https://example.com/repo.git
`,
			wantErrors: []string{"repositories[0].url is not allowed when mode is provider."},
		},
		{
			name: "bad base url",
			content: base + `repositories:
  - mode: provider
    provider: github
    credential: token
    baseUrl: also not a url
`,
			wantErrors: []string{"repositories[0].baseUrl must be an absolute http or https URL."},
		},
		{
			name: "artifacts require their collection",
			content: base + `repositories:
  - mode: provider
    provider: github
    credential: token
    includeIssueArtifacts: true
    includeMergeRequestsArtifacts: true
    includeReleaseArtifacts: true
`,
			wantErrors: []string{
				"repositories[0].includeIssueArtifacts requires includeIssues.",
				"repositories[0].includeMergeRequestsArtifacts requires includeMergeRequests.",
				"repositories[0].includeReleaseArtifacts requires includeReleases.",
			},
		},
		{
			name: "url mode requires url",
			content: base + `repositories:
  - mode: url
`,
			wantErrors: []string{"repositories[0].url is required when mode is url."},
		},
		{
			name: "url mode rejects provider-only keys",
			content: base + `repositories:
  - mode: url
    url: https://example.com/repo.git
    provider: github
    baseUrl: https://example.com
    includeStarred: true
    includeSnippets: true
    includeIssues: true
    includeIssueArtifacts: true
    includeMergeRequests: true
    includeMergeRequestsArtifacts: true
    includeReleases: true
    includeReleaseArtifacts: true
`,
			wantErrors: []string{
				"repositories[0].provider is not allowed when mode is url.",
				"repositories[0].baseUrl is not allowed when mode is url.",
				"repositories[0].includeStarred is not allowed when mode is url.",
				"repositories[0].includeSnippets is not allowed when mode is url.",
				"repositories[0].includeIssues is not allowed when mode is url.",
				"repositories[0].includeIssueArtifacts is not allowed when mode is url.",
				"repositories[0].includeMergeRequests is not allowed when mode is url.",
				"repositories[0].includeMergeRequestsArtifacts is not allowed when mode is url.",
				"repositories[0].includeReleases is not allowed when mode is url.",
				"repositories[0].includeReleaseArtifacts is not allowed when mode is url.",
			},
		},
		{
			name: "url mode rejects invalid url entry",
			content: base + `repositories:
  - mode: url
    url:
      - https://example.com/one.git
      - ftp://example.com/two.git
`,
			wantErrors: []string{"repositories[0].url[1] must be an absolute http or https URL."},
		},
		{
			name: "invalid cron",
			content: `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
schedule:
  repositories:
    cron: "99 99 99 99 99"
`,
			wantErrors: []string{"schedule.repositories.cron is invalid: must be a valid 5-field or 6-field cron expression."},
		},
		{
			name: "invalid log level",
			content: base + `logging:
  logLevel: verbse
`,
			wantErrors: []string{"logging.logLevel 'verbse' is invalid. Supported values: debug, info, warn, error."},
		},
		{
			name: "invalid payload signature mode",
			content: `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
  payloadSignatureMode: chunked
schedule:
  repositories:
    cron: "0 * * * *"
`,
			wantErrors: []string{"storage.payloadSignatureMode 'chunked' is invalid. Supported values: full, streaming, unsigned."},
		},
		{
			name: "concurrency below one",
			content: base + `concurrency:
  repositories: 0
  metadata: -3
`,
			wantErrors: []string{"concurrency.repositories must be 1 or greater.", "concurrency.metadata must be 1 or greater."},
		},
		{
			name: "health port out of range",
			content: base + `health:
  port: 70000
`,
			wantErrors: []string{"health.port must be between 0 and 65535."},
		},
		{
			name: "health bind blank",
			content: base + `health:
  bind: ""
`,
			wantErrors: []string{"health.bind must not be empty."},
		},
		{
			name: "unknown top-level key",
			content: base + `backupsX: true
`,
			wantErrors: nil, // reported as a parse error, asserted separately below
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errors := loadContent(t, tt.content)
			if tt.name == "unknown top-level key" {
				if len(errors) != 1 || !strings.Contains(errors[0], "YAML parse error in") {
					t.Fatalf("unknown key should surface as a YAML parse error, got %v", errors)
				}
				return
			}
			if len(errors) != len(tt.wantErrors) {
				t.Fatalf("errors = %v, want %v", errors, tt.wantErrors)
			}
			for i, want := range tt.wantErrors {
				if errors[i] != want {
					t.Errorf("errors[%d] = %q, want %q", i, errors[i], want)
				}
			}
		})
	}
}

func TestLoadPathErrors(t *testing.T) {
	loader := NewLoader()

	if _, errors := loader.Load(""); len(errors) != 1 || errors[0] != "settings path is required." {
		t.Errorf("empty path errors = %v", errors)
	}

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if _, errors := loader.Load(missing); len(errors) != 1 || errors[0] != "settings file not found: '"+missing+"'" {
		t.Errorf("missing file errors = %v", errors)
	}

	directory := t.TempDir()
	if _, errors := loader.Load(directory); len(errors) != 1 || errors[0] != "settings file not found: '"+directory+"'" {
		t.Errorf("directory path errors = %v", errors)
	}
}

func TestLoadDeprecatedKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "top-level backups",
			content: "backups: []\nstorage:\n  endpoint: x\n",
			want:    "backups is no longer supported. Use repositories entries with mode: provider.",
		},
		{
			name:    "top-level mirrors",
			content: "mirrors: []\nstorage:\n  endpoint: x\n",
			want:    "mirrors is no longer supported. Use repositories entries with mode: url.",
		},
		{
			name:    "schedule backups",
			content: "schedule:\n  backups: \"0 * * * *\"\nstorage:\n  endpoint: x\n",
			want:    "schedule.backups is no longer supported. Use schedule.repositories.cron.",
		},
		{
			name:    "schedule mirrors",
			content: "schedule:\n  mirrors: \"0 * * * *\"\nstorage:\n  endpoint: x\n",
			want:    "schedule.mirrors is no longer supported. Use schedule.repositories.cron.",
		},
		{
			name:    "deprecated key is case insensitive",
			content: "Backups: []\n",
			want:    "backups is no longer supported. Use repositories entries with mode: provider.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errors := loadContent(t, tt.content)
			if len(errors) != 1 || errors[0] != tt.want {
				t.Fatalf("errors = %v, want [%q]", errors, tt.want)
			}
		})
	}
}

func TestURLScalarOrSequence(t *testing.T) {
	storage := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`

	scalar := storage + `repositories:
  - mode: url
    url: https://example.com/one.git
`
	settings, errors := loadContent(t, scalar)
	if len(errors) != 0 {
		t.Fatalf("scalar url should load: %v", errors)
	}
	if len(settings.Repositories[0].Urls) != 1 || settings.Repositories[0].Urls[0] != "https://example.com/one.git" {
		t.Errorf("scalar url = %v", settings.Repositories[0].Urls)
	}

	sequence := storage + `repositories:
  - mode: url
    url:
      - https://example.com/one.git
      - https://example.com/two.git
`
	settings, errors = loadContent(t, sequence)
	if len(errors) != 0 {
		t.Fatalf("sequence url should load: %v", errors)
	}
	if len(settings.Repositories[0].Urls) != 2 {
		t.Errorf("sequence url = %v", settings.Repositories[0].Urls)
	}

	nullURL := storage + `repositories:
  - mode: url
    url:
`
	// A null url normalizes to an empty list, so the loader demands a value.
	_, errors = loadContent(t, nullURL)
	if len(errors) != 1 || errors[0] != "repositories[0].url is required when mode is url." {
		t.Fatalf("null url should require a value, got %v", errors)
	}
}

func TestSecretResolution(t *testing.T) {
	storage := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: %s
  secretAccessKey: %s
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`

	t.Run("env placeholder resolves", func(t *testing.T) {
		t.Setenv("GITBACKUP_TEST_KEY", "resolved-key")
		content := fmt.Sprintf(storage, "${GITBACKUP_TEST_KEY}", "plain-secret")
		settings, errors := loadContent(t, content)
		if len(errors) != 0 {
			t.Fatalf("errors = %v", errors)
		}
		if settings.Storage.AccessKeyID != "resolved-key" {
			t.Errorf("AccessKeyID = %q, want the env value", settings.Storage.AccessKeyID)
		}
		if settings.Storage.SecretAccessKey != "plain-secret" {
			t.Errorf("SecretAccessKey = %q, want the literal", settings.Storage.SecretAccessKey)
		}
	})

	t.Run("unset env fails", func(t *testing.T) {
		content := fmt.Sprintf(storage, "${GITBACKUP_TEST_MISSING}", "s")
		_, errors := loadContent(t, content)
		if len(errors) != 1 || errors[0] != "storage.accessKeyId references environment variable 'GITBACKUP_TEST_MISSING', which is not set." {
			t.Fatalf("errors = %v", errors)
		}
	})

	t.Run("partial placeholder passes through", func(t *testing.T) {
		content := fmt.Sprintf(storage, "prefix-${HOME}-suffix", "s")
		settings, errors := loadContent(t, content)
		if len(errors) != 0 {
			t.Fatalf("errors = %v", errors)
		}
		if settings.Storage.AccessKeyID != "prefix-${HOME}-suffix" {
			t.Errorf("partial placeholder should pass through untouched, got %q", settings.Storage.AccessKeyID)
		}
	})

	t.Run("file source", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(secretPath, []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		content := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKeyFile: ` + secretPath + `
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`
		settings, errors := loadContent(t, content)
		if len(errors) != 0 {
			t.Fatalf("errors = %v", errors)
		}
		if settings.Storage.SecretAccessKey != "file-secret" {
			t.Errorf("SecretAccessKey = %q, want the trimmed file value", settings.Storage.SecretAccessKey)
		}
	})

	t.Run("both value and file fail", func(t *testing.T) {
		content := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKey: s
  secretAccessKeyFile: somewhere.txt
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`
		_, errors := loadContent(t, content)
		if len(errors) != 1 || errors[0] != "storage.secretAccessKey and storage.secretAccessKeyFile are both set. Use one or the other." {
			t.Fatalf("errors = %v", errors)
		}
	})

	t.Run("unreadable file fails", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
		content := `storage:
  endpoint: http://127.0.0.1:9000
  region: r
  accessKeyId: k
  secretAccessKeyFile: ` + missing + `
  bucket: b
schedule:
  repositories:
    cron: "0 * * * *"
`
		_, errors := loadContent(t, content)
		if len(errors) != 1 || !strings.Contains(errors[0], "could not be read") {
			t.Fatalf("errors = %v", errors)
		}
	})
}

func TestLiveSettingsReload(t *testing.T) {
	previousInterval := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = previousInterval })

	path := writeSettings(t, validSettings)
	loader := NewLoader()
	initial, errors := loader.Load(path)
	if len(errors) != 0 {
		t.Fatalf("initial load failed: %v", errors)
	}

	reloaded := make(chan *Settings, 4)
	live, err := NewLiveSettings(path, initial, func(s *Settings) { reloaded <- s })
	if err != nil {
		t.Fatal(err)
	}
	live.Start()
	t.Cleanup(live.Close)

	if live.Current() != initial {
		t.Fatal("current should start as the initial settings")
	}

	updated := `storage:
  endpoint: http://127.0.0.1:9000
  region: us-east-2
  accessKeyId: k2
  secretAccessKey: s2
  bucket: b2
schedule:
  repositories:
    cron: "0 1 * * *"
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case settings := <-reloaded:
		if settings.Schedule.Repositories.Cron != "0 1 * * *" {
			t.Errorf("reloaded cron = %q", settings.Schedule.Repositories.Cron)
		}
		if live.Current() != settings {
			t.Error("Current() should return the reloaded settings")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settings change was not picked up in time")
	}

	// An invalid rewrite keeps the previous settings.
	if err := os.WriteFile(path, []byte("storage: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if live.Current().Schedule.Repositories.Cron != "0 1 * * *" {
		t.Error("failed reload must keep the previous settings")
	}

	// A transiently missing file keeps the baseline and does not crash.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if live.Current().Schedule.Repositories.Cron != "0 1 * * *" {
		t.Error("transient read miss must keep the current settings")
	}
}
