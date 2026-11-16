package config

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Key-source prefixes. Every resolver in this package reports where it found a
// key as "<prefix><where>" so callers (dashboard, proxy logs) can show the
// source without ever touching the value.
const (
	KeySourceFilePrefix = "file:"
	KeySourceEnvPrefix  = "env:"
)

// LiteLLMAPIKeyFileField and gatewayAPIKeyFileFieldFormat name the config
// fields an operator sets, so validation and runtime warnings point at the
// exact hive.yaml key to fix.
const (
	LiteLLMAPIKeyFileField       = "governor.litellm.api_key_file"
	gatewayAPIKeyFileFieldFormat = "governor.gateways[%s].api_key_file"
)

// SecretFileRoots returns a copy of the directories an api_key_file may live
// under (see SecretFilePathAllowed). Safe to show to operators: these are
// paths, never key material.
func SecretFileRoots() []string {
	return append([]string(nil), secretFileRoots...)
}

// keyFileSkipped reports whether p is a configured key file the resolvers will
// refuse to read (set, but outside the allowed dirs).
func keyFileSkipped(p string) bool {
	p = strings.TrimSpace(p)
	return p != "" && !SecretFilePathAllowed(p)
}

// SecretFilePathRejection returns the operator-facing message for an
// api_key_file outside the allowed secret dirs (hive#18533), or "" when path
// is empty or allowed. It names the field and path, lists the allowed dirs,
// and says how to fix it: mount the Secret under one of them. The audit N8
// gate itself (SecretFilePathAllowed) is unchanged; this only explains it.
func SecretFilePathRejection(field, path string) string {
	path = strings.TrimSpace(path)
	if !keyFileSkipped(path) {
		return ""
	}
	return fmt.Sprintf("%s %q is outside the allowed secret dirs (%s), so hive will never read it; "+
		"mount the Kubernetes Secret under one of those dirs (for example a volumeMount at /secrets/<name>) "+
		"and point %s at the file there",
		field, path, strings.Join(SecretFileRoots(), ", "), field)
}

// GatewayAPIKeyFileField names a gateway's api_key_file field for messages.
func GatewayAPIKeyFileField(name string) string {
	return fmt.Sprintf(gatewayAPIKeyFileFieldFormat, name)
}

// DescribeKeySource renders an effective key source for display, noting a
// configured api_key_file that was skipped, for example
// "key from /secrets/litellm_api_key (configured path /secrets-cb/k not allowed)".
// It takes the log-safe source string and never contains key material.
func DescribeKeySource(source, configuredFile string) string {
	var desc string
	switch {
	case source == "":
		desc = "no key found"
	case strings.HasPrefix(source, KeySourceFilePrefix):
		desc = "key from " + strings.TrimPrefix(source, KeySourceFilePrefix)
	case strings.HasPrefix(source, KeySourceEnvPrefix):
		desc = "key from env " + strings.TrimPrefix(source, KeySourceEnvPrefix)
	default:
		desc = "key from " + source
	}
	if keyFileSkipped(configuredFile) {
		desc += fmt.Sprintf(" (configured path %s not allowed)", strings.TrimSpace(configuredFile))
	}
	return desc
}

// skippedKeyFileWarned remembers which (field, path) pairs have already been
// warned about, so a skipped api_key_file logs ONE warning per process instead
// of one per resolution (the dashboard and proxy resolve keys constantly).
var skippedKeyFileWarned sync.Map

// warnSkippedKeyFileOnce logs, once per field and path, that a configured
// api_key_file was skipped and which source was used instead. Returns true
// when this call logged. Never logs key material.
func warnSkippedKeyFileOnce(field, path, effectiveSource string) bool {
	path = strings.TrimSpace(path)
	if _, loaded := skippedKeyFileWarned.LoadOrStore(field+"\x00"+path, struct{}{}); loaded {
		return false
	}
	effective := effectiveSource
	if effective == "" {
		effective = "none"
	}
	slog.Warn("configured api_key_file is outside the allowed secret dirs and was skipped",
		"field", field,
		"skipped_path", path,
		"effective_key_source", effective,
		"allowed_dirs", strings.Join(SecretFileRoots(), ","),
		"remedy", "mount the Secret under an allowed dir and point "+field+" at the file there")
	return true
}

// resetSkippedKeyFileWarnings clears the warn-once memory (tests only).
func resetSkippedKeyFileWarnings() {
	skippedKeyFileWarned.Range(func(k, _ any) bool {
		skippedKeyFileWarned.Delete(k)
		return true
	})
}
