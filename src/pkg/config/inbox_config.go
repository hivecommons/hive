package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Inbox destinations. `knowledge` is implicit: every item is already a
// Knowledge fact when it reaches the Inbox.
const (
	InboxDestinationSpekRFC = "spek_rfc"
	InboxDestinationDibs    = "dibs"
	InboxDestinationDismiss = "dismiss"
)

// DefaultInboxSources are the connector types whose facts surface as Inbox
// items when inbox.sources is empty.
var DefaultInboxSources = []string{"plaud", "wisprflow"}

// InboxConfig is the `inbox:` block: which connector types feed the Inbox,
// how items are auto-routed, and how each destination is reached.
type InboxConfig struct {
	// Sources lists connector types (plaud, wisprflow, ...) whose facts are
	// Inbox items. Empty uses DefaultInboxSources.
	Sources []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	// Rules are evaluated in order against every new item; the first match
	// routes it automatically.
	Rules []InboxRule `yaml:"rules,omitempty" json:"rules,omitempty"`
	Spek  InboxSpek   `yaml:"spek,omitempty" json:"spek,omitempty"`
	Dibs  InboxDibs   `yaml:"dibs,omitempty" json:"dibs,omitempty"`
}

// InboxRule routes items whose source and title/tag match.
type InboxRule struct {
	// Source restricts the rule to one connector type; empty matches all.
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// TitlePrefix matches case-insensitively against the item title.
	TitlePrefix string `yaml:"title_prefix,omitempty" json:"title_prefix,omitempty"`
	// Tag matches any item tag exactly (case-insensitive).
	Tag string `yaml:"tag,omitempty" json:"tag,omitempty"`
	// Destination is spek_rfc, dibs or dismiss.
	Destination string `yaml:"destination" json:"destination"`
	// Repo (owner/repo) overrides inbox.spek.repo for spek_rfc.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
}

// InboxSpek configures the spek_rfc destination.
type InboxSpek struct {
	// Repo is the default owner/repo RFCs are filed to.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Binary is the spektacular executable (default runs.spektacular.binary).
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
	// Agent names a hive agent to kick with a spek-new/publish prompt when
	// the binary is unavailable in the hive image. Empty picks the first
	// enabled, unpaused agent by name (see InboxSpekAgent).
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
}

// InboxSpekAgent returns the agent the spek_rfc destination falls back to
// when the spektacular binary is absent: inbox.spek.agent if set, else the
// alphabetically first enabled, unpaused agent, else "".
func (c *Config) InboxSpekAgent() string {
	if a := strings.TrimSpace(c.Inbox.Spek.Agent); a != "" {
		return a
	}
	names := make([]string, 0, len(c.Agents))
	for name, a := range c.Agents {
		if a.Enabled && !a.Paused {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// InboxDibs configures the dibs destination.
type InboxDibs struct {
	// URL is the dibs instance base URL.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// TokenEnv names the environment variable holding the hub bearer token.
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
}

var inboxRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// EffectiveSources returns Sources or DefaultInboxSources.
func (c InboxConfig) EffectiveSources() []string {
	if len(c.Sources) == 0 {
		return append([]string(nil), DefaultInboxSources...)
	}
	return c.Sources
}

// ValidateInbox rejects unknown destinations, malformed repos and rules
// without a filter (a filter-less rule would route every item).
func ValidateInbox(c InboxConfig) error {
	if c.Spek.Repo != "" && !inboxRepoRe.MatchString(c.Spek.Repo) {
		return fmt.Errorf("inbox.spek.repo %q: must be owner/repo", c.Spek.Repo)
	}
	if c.Dibs.URL != "" && !strings.HasPrefix(c.Dibs.URL, "http://") && !strings.HasPrefix(c.Dibs.URL, "https://") {
		return fmt.Errorf("inbox.dibs.url %q: must start with http:// or https://", c.Dibs.URL)
	}
	for i, r := range c.Rules {
		switch r.Destination {
		case InboxDestinationSpekRFC, InboxDestinationDibs, InboxDestinationDismiss:
		default:
			return fmt.Errorf("inbox.rules[%d].destination %q: must be %s, %s or %s", i, r.Destination, InboxDestinationSpekRFC, InboxDestinationDibs, InboxDestinationDismiss)
		}
		if strings.TrimSpace(r.TitlePrefix) == "" && strings.TrimSpace(r.Tag) == "" {
			return fmt.Errorf("inbox.rules[%d]: title_prefix or tag is required", i)
		}
		if r.Repo != "" && !inboxRepoRe.MatchString(r.Repo) {
			return fmt.Errorf("inbox.rules[%d].repo %q: must be owner/repo", i, r.Repo)
		}
	}
	return nil
}
