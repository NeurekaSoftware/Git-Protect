package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envPlaceholder matches a value that is *entirely* a ${ENV_VAR} placeholder.
// The pattern is deliberately anchored: a literal secret that merely happens to
// contain "${" is passed through untouched rather than mangled.
var envPlaceholder = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// resolveSecret resolves one configured secret so it never has to be written
// into the settings file in cleartext. A secret may be given literally, as a
// ${ENV_VAR} placeholder, or through a companion *File key naming a file to
// read — a Docker or Kubernetes secret mount.
//
// filePath takes precedence over value; naming both is an error. An unset
// variable or an unreadable file records an error against configPath and
// returns "" so validation reports the now-missing required value.
func resolveSecret(value, filePath, configPath string, errors *[]string) string {
	if strings.TrimSpace(filePath) != "" {
		if strings.TrimSpace(value) != "" {
			*errors = append(*errors, fmt.Sprintf("%s and %sFile are both set. Use one or the other.", configPath, configPath))
			return ""
		}

		content, err := os.ReadFile(filePath)
		if err != nil {
			*errors = append(*errors, fmt.Sprintf("%sFile '%s' could not be read: %v", configPath, filePath, err))
			return ""
		}
		return strings.TrimSpace(string(content))
	}

	if strings.TrimSpace(value) == "" {
		return value
	}

	match := envPlaceholder.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return value
	}

	variable := match[1]
	resolved := os.Getenv(variable)
	if strings.TrimSpace(resolved) == "" {
		*errors = append(*errors, fmt.Sprintf("%s references environment variable '%s', which is not set.", configPath, variable))
		return ""
	}
	return resolved
}

// resolveSecrets replaces each configured secret with its resolved value, so a
// token can be supplied through an environment variable or a secret file
// instead of being written into the settings file.
func resolveSecrets(settings *Settings, errors *[]string) {
	settings.Storage.AccessKeyID = resolveSecret(
		settings.Storage.AccessKeyID, settings.Storage.AccessKeyIDFile, "storage.accessKeyId", errors)
	settings.Storage.SecretAccessKey = resolveSecret(
		settings.Storage.SecretAccessKey, settings.Storage.SecretAccessKeyFile, "storage.secretAccessKey", errors)

	for _, credential := range settings.Credentials {
		credential.APIKey = resolveSecret(
			credential.APIKey, credential.APIKeyFile, fmt.Sprintf("credentials.%s.apiKey", credential.Name), errors)
	}
}
