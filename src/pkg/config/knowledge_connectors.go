package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KnowledgeConnector is one `knowledge.connectors` entry: an external
// knowledge system synced into the vault at Layer (see
// pkg/knowledge/connector). Credentials are referenced through Auth only;
// inline secrets are rejected by ValidateKnowledgeConnectors.
type KnowledgeConnector struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// Enabled is a pointer so an absent key defaults to true.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Interval is a Go duration ("15m", "1h"); empty uses the connector default.
	Interval string                 `yaml:"interval,omitempty"`
	Layer    string                 `yaml:"layer"`
	Scope    map[string]string      `yaml:"scope,omitempty"`
	Auth     KnowledgeConnectorAuth `yaml:"auth,omitempty"`
}

// KnowledgeConnectorAuth names where a connector credential lives. Unknown
// keys (token, password, ...) are captured in Inline so validation can
// reject them instead of the YAML decoder silently dropping a pasted secret.
type KnowledgeConnectorAuth struct {
	Env    string         `yaml:"env,omitempty"`
	File   string         `yaml:"file,omitempty"`
	Inline map[string]any `yaml:",inline"`
}

// IsEnabled reports whether the connector should be scheduled (default true).
func (k KnowledgeConnector) IsEnabled() bool {
	return k.Enabled == nil || *k.Enabled
}

// IntervalDuration parses Interval; zero means "use the connector default".
func (k KnowledgeConnector) IntervalDuration() (time.Duration, error) {
	if strings.TrimSpace(k.Interval) == "" {
		return 0, nil
	}
	return time.ParseDuration(strings.TrimSpace(k.Interval))
}

// MinKnowledgeConnectorInterval is the shortest accepted sync interval.
const MinKnowledgeConnectorInterval = time.Minute

var (
	knowledgeConnectorNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	knowledgeConnectorEnvRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	knowledgeLayerNames      = map[string]bool{"personal": true, "project": true, "org": true, "community": true}
	// Scope keys that would carry a credential inline. Credentials belong in
	// auth.env or auth.file so they never land in hive.yaml or its backups.
	knowledgeConnectorSecretWords = []string{"token", "password", "passwd", "secret", "apikey", "api_key", "api-key", "private_key", "credential"}
)

// ValidateKnowledgeConnectors checks every `knowledge.connectors` entry and
// names the offending field on error. Type-specific scope keys are checked by
// the connector itself when it is built.
func ValidateKnowledgeConnectors(conns []KnowledgeConnector) error {
	seen := map[string]bool{}
	for i, c := range conns {
		field := fmt.Sprintf("knowledge.connectors[%d]", i)
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return fmt.Errorf("%s.name is required", field)
		}
		if !knowledgeConnectorNameRe.MatchString(name) {
			return fmt.Errorf("%s.name %q must be lowercase letters, digits and dashes (max 63)", field, c.Name)
		}
		if seen[name] {
			return fmt.Errorf("%s.name %q is a duplicate", field, c.Name)
		}
		seen[name] = true
		field = fmt.Sprintf("knowledge.connectors[%d] (%s)", i, name)

		typ := strings.TrimSpace(c.Type)
		if typ == "" {
			return fmt.Errorf("%s.type is required", field)
		}
		if !knowledgeConnectorNameRe.MatchString(typ) {
			return fmt.Errorf("%s.type %q must be lowercase letters, digits and dashes", field, c.Type)
		}
		if !knowledgeLayerNames[strings.TrimSpace(c.Layer)] {
			return fmt.Errorf("%s.layer %q must be personal, project, org or community", field, c.Layer)
		}
		d, err := c.IntervalDuration()
		if err != nil {
			return fmt.Errorf("%s.interval %q: %w", field, c.Interval, err)
		}
		if d != 0 && d < MinKnowledgeConnectorInterval {
			return fmt.Errorf("%s.interval %q must be at least %s", field, c.Interval, MinKnowledgeConnectorInterval)
		}
		if err := validateKnowledgeConnectorAuth(field, c.Auth); err != nil {
			return err
		}
		keys := make([]string, 0, len(c.Scope))
		for k := range c.Scope {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%s.scope has an empty key", field)
			}
			if knowledgeConnectorSecretKey(k) {
				return fmt.Errorf("%s.scope.%s looks like an inline secret; reference it with auth.env or auth.file instead", field, k)
			}
		}
	}
	return nil
}

func validateKnowledgeConnectorAuth(field string, a KnowledgeConnectorAuth) error {
	if len(a.Inline) > 0 {
		keys := make([]string, 0, len(a.Inline))
		for k := range a.Inline {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("%s.auth.%s is not allowed: inline secrets are rejected; use auth.env or auth.file", field, keys[0])
	}
	env, file := strings.TrimSpace(a.Env), strings.TrimSpace(a.File)
	if env != "" && file != "" {
		return fmt.Errorf("%s.auth: set only one of env or file", field)
	}
	if env != "" && !knowledgeConnectorEnvRe.MatchString(env) {
		return fmt.Errorf("%s.auth.env %q must be an environment variable name", field, a.Env)
	}
	if file != "" && !filepath.IsAbs(file) {
		return fmt.Errorf("%s.auth.file %q must be an absolute path", field, a.File)
	}
	return nil
}

func knowledgeConnectorSecretKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, w := range knowledgeConnectorSecretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
