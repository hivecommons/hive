package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
)

// ForgejoIntegrationConfig is the forgejo.integration block: git clone and
// push on Forgejo repos authenticate with a JWT that hive signs per token
// tier, instead of the forgejo block's long-lived access token
// (hivecommons/hive#19576).
//
// The operator creates one Forgejo authorized integration per tier, scoped
// to the tier's permission and to the hive's repos, with the claim rule
// sub=hive:<tier>, and publishes the key list `hive forge key-list` renders
// at Issuer. Each integration's audience goes under Audiences, keyed by tier.
// A tier with no audience gets the closest less-privileged tier that has one
// (agentmode.LessPrivilegedTokenTier), never a more-privileged one; a tier
// with none at all cannot clone or push.
type ForgejoIntegrationConfig struct {
	// Issuer is the https origin (no path) that serves the discovery document
	// and key list; it is the iss claim.
	Issuer string `yaml:"issuer"`
	// KeyFile is the absolute path of the RSA signing key (PEM, mode 0600 or
	// 0400, or 0440 with group hive-launch as the /secrets mount presents
	// it; see forgejwt.LaunchGID). It is read at every mint, so replacing the
	// file rotates the key. A cached REST token (rest_audience) is dropped
	// when the file's size or modification time changes.
	KeyFile string `yaml:"key_file"`
	// Audiences maps a token tier (agentmode.KnownTokenTiers) to the aud
	// Forgejo generated for that tier's integration.
	Audiences map[string]string `yaml:"audiences"`
	// RESTAudience is the aud of the integration for the hive's own Forgejo
	// REST calls (work discovery, the relays, merges and the sweeps). When
	// set, those calls send a JWT with sub hive:rest instead of the forgejo
	// block's access token; when empty they keep using the access token
	// (hivecommons/hive#19920). It must differ from every tier audience.
	RESTAudience string `yaml:"rest_audience,omitempty"`
	// ServeKeyList, when true, makes the spoke dashboard serve the discovery
	// document and key list itself, anonymously, at
	// /.well-known/openid-configuration and /jwks.json, rendered from
	// KeyFile and PreviousKeyFiles. Off by default: the operator then hosts
	// the files `hive forge key-list` writes. Only turn it on when Issuer is
	// the dashboard's own https origin (hivecommons/hive#19996).
	ServeKeyList bool `yaml:"serve_key_list,omitempty"`
	// PreviousKeyFiles are keys published in the served key list but never
	// used to mint: the old key after a rotation, or the next key before
	// one, so tokens and Forgejo's cached copy of the list keep working
	// across the switch. Each is a key
	// file in the same form and mode as KeyFile; only its public half is
	// served. They are used only when ServeKeyList is on.
	PreviousKeyFiles []string `yaml:"previous_key_files,omitempty"`
}

// MaxPreviousKeyFiles caps previous_key_files. A rotation needs one; the cap
// keeps the served key list far below the 16 KiB Forgejo reads of it.
const MaxPreviousKeyFiles = 4

// Validate checks the block. A nil block (the default) is valid: the
// forgejo block's access token is used.
func (c *ForgejoIntegrationConfig) Validate() error {
	if c == nil {
		return nil
	}
	const field = "forgejo.integration"
	u, err := url.Parse(strings.TrimSpace(c.Issuer))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s.issuer: %q must be https://host[:port] with no path, query or fragment", field, c.Issuer)
	}
	if k := strings.TrimSpace(c.KeyFile); k == "" || !filepath.IsAbs(k) {
		return fmt.Errorf("%s.key_file: %q must be an absolute path to the RSA signing key", field, c.KeyFile)
	}
	if err := c.validatePreviousKeyFiles(field); err != nil {
		return err
	}
	if len(c.Audiences) == 0 && !c.ServeKeyList {
		return fmt.Errorf("%s.audiences: at least one token tier needs the audience of its Forgejo integration", field)
	}
	if c.RESTAudience != "" && strings.TrimSpace(c.RESTAudience) == "" {
		return fmt.Errorf("%s.rest_audience: audience is blank", field)
	}
	rest := strings.TrimSpace(c.RESTAudience)
	for _, tier := range c.tiers() {
		if !slices.Contains(agentmode.KnownTokenTiers, agentmode.TokenTier(tier)) {
			return fmt.Errorf("%s.audiences: unknown token tier %q (want one of %s)", field, tier, knownTokenTierList())
		}
		aud := strings.TrimSpace(c.Audiences[tier])
		if aud == "" {
			return fmt.Errorf("%s.audiences.%s: audience is empty", field, tier)
		}
		// The REST integration and a tier's integration are separate, each
		// pinned to its own sub; one audience for both is a copy-paste slip
		// that Forgejo would only refuse at the first REST call.
		if rest != "" && aud == rest {
			return fmt.Errorf("%s.rest_audience: same audience as audiences.%s; the REST integration needs its own", field, tier)
		}
	}
	return nil
}

// validatePreviousKeyFiles checks previous_key_files: each an absolute path,
// none repeated and none equal to key_file, and at most MaxPreviousKeyFiles.
func (c *ForgejoIntegrationConfig) validatePreviousKeyFiles(field string) error {
	if len(c.PreviousKeyFiles) == 0 {
		return nil
	}
	if !c.ServeKeyList {
		return fmt.Errorf("%s.previous_key_files: only the served key list uses it; set serve_key_list: true, or publish the old key with hive forge key-list --key", field)
	}
	if len(c.PreviousKeyFiles) > MaxPreviousKeyFiles {
		return fmt.Errorf("%s.previous_key_files: %d keys listed, at most %d", field, len(c.PreviousKeyFiles), MaxPreviousKeyFiles)
	}
	seen := map[string]bool{filepath.Clean(strings.TrimSpace(c.KeyFile)): true}
	for i, p := range c.PreviousKeyFiles {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			return fmt.Errorf("%s.previous_key_files[%d]: %q must be an absolute path to an RSA signing key", field, i, c.PreviousKeyFiles[i])
		}
		if seen[filepath.Clean(p)] {
			return fmt.Errorf("%s.previous_key_files[%d]: %q is key_file or is listed twice", field, i, c.PreviousKeyFiles[i])
		}
		seen[filepath.Clean(p)] = true
	}
	return nil
}

// PreviousKeyPaths is PreviousKeyFiles trimmed, for the served key list.
func (c *ForgejoIntegrationConfig) PreviousKeyPaths() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.PreviousKeyFiles))
	for _, p := range c.PreviousKeyFiles {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// IssuerOrigin is Issuer without a trailing slash, the exact iss value.
func (c *ForgejoIntegrationConfig) IssuerOrigin() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
}

func (c *ForgejoIntegrationConfig) tiers() []string {
	tiers := make([]string, 0, len(c.Audiences))
	for t := range c.Audiences {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	return tiers
}

// knownTokenTierList is agentmode.KnownTokenTiers joined for an error message.
func knownTokenTierList() string {
	names := make([]string, 0, len(agentmode.KnownTokenTiers))
	for _, t := range agentmode.KnownTokenTiers {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}
