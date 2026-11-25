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
	// file rotates the key.
	KeyFile string `yaml:"key_file"`
	// Audiences maps a token tier (agentmode.KnownTokenTiers) to the aud
	// Forgejo generated for that tier's integration.
	Audiences map[string]string `yaml:"audiences"`
}

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
	if len(c.Audiences) == 0 {
		return fmt.Errorf("%s.audiences: at least one token tier needs the audience of its Forgejo integration", field)
	}
	for _, tier := range c.tiers() {
		if !slices.Contains(agentmode.KnownTokenTiers, agentmode.TokenTier(tier)) {
			return fmt.Errorf("%s.audiences: unknown token tier %q (want one of %s)", field, tier, knownTokenTierList())
		}
		if strings.TrimSpace(c.Audiences[tier]) == "" {
			return fmt.Errorf("%s.audiences.%s: audience is empty", field, tier)
		}
	}
	return nil
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
