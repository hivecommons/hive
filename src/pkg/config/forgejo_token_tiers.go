package config

import "fmt"

// UsesForgejo reports whether any repo of the hive resolves to Forgejo:
// project.forge is forgejo, or a repo_policies entry moves a repo there.
func (c *Config) UsesForgejo() bool {
	if c == nil {
		return false
	}
	if c.Project.ForgeKind() == ForgeForgejo {
		return true
	}
	for _, repo := range c.Project.Repos {
		if c.ForgeKindForRepo(repo) == ForgeForgejo {
			return true
		}
	}
	for _, rp := range c.Project.RepoPolicies {
		if c.ForgeKindForRepo(rp.Repo) == ForgeForgejo {
			return true
		}
	}
	return false
}

// ForgejoSharedWriteTokenWarning is the operator warning for a hive whose
// Forgejo repos have no forgejo.integration (hivecommons/hive#19917), or ""
// when there is nothing to warn about.
//
// Without forgejo.integration, git on a Forgejo repo uses the forgejo.token_env
// access token for every token tier (cmd/hive forgejoMinter returns a
// pushbroker.StaticTokenMinter, and WithTier leaves it unchanged). That token
// needs write access for the hive to work, so the read-only tiers (advisor,
// shared-uid-read) can push with it. Boot logs this text and the dashboard's
// forge settings show it, so the operator learns it from hive and not from a
// push that should have failed.
func (c *Config) ForgejoSharedWriteTokenWarning() string {
	if c == nil || c.Forgejo.Integration != nil || !c.UsesForgejo() {
		return ""
	}
	return fmt.Sprintf("forgejo.integration is not set, so git on every Forgejo repo uses the %s token for every token tier, the read-only tiers (advisor, shared-uid-read) included: those agents' clones can push with a write token. Set forgejo.integration to give each tier its own short-lived token (docs/forge-app-setup.md, \"Forgejo git credentials\").",
		c.Forgejo.Credentials().Governor().TokenEnv)
}
