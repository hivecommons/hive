package compliance

import (
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// BuiltinAuditRetentionDays mirrors the audit log's fixed lumberjack MaxAge
// (pkg/dashboard audit.go auditMaxAgeDays; docs/audit-log.md "Rotation and
// retention"). Retention is not yet an operator setting, so the
// audit.retention_days mapping reports this constant; a dashboard test pins
// the two together.
const BuiltinAuditRetentionDays = 90

// Setting kinds tell the evaluators how to read a value.
const (
	kindBool   = "bool"
	kindInt    = "int"
	kindString = "string"
	kindList   = "list"
)

// setting is one Hive setting a mapping may point at. Paths are the YAML
// key paths of config.Config (a test walks the struct tags to prove each one
// exists), except two documented pseudo-namespaces:
//
//   - env.<NAME>: a process environment variable, read through the Env
//     passed to EvaluateWith.
//   - builtin settings (Builtin: true): behaviour fixed in code that the
//     Compliance epic will make configurable; reported read-only.
type setting struct {
	Kind        string
	Description string
	Builtin     bool
	// read returns the current value: bool, int, string or []string, or nil
	// when the setting is unset.
	read func(cfg *config.Config, getenv func(string) string) any
}

var settings = map[string]setting{
	"dashboard.authorized_users": {
		Kind:        kindList,
		Description: "Dashboard allowlist with owner / merger / read-write / read roles",
		read: func(c *config.Config, _ func(string) string) any {
			return nonNilList(c.Dashboard.AuthorizedUsers)
		},
	},
	"review.require_approval": {
		Kind:        kindBool,
		Description: "Merge requires an aggregate approve from the review swarm",
		read:        func(c *config.Config, _ func(string) string) any { return c.Review.RequireApproval },
	},
	"review.post_comments": {
		Kind:        kindBool,
		Description: "Reviewers publish their verdict as a PR comment (review evidence on the PR)",
		read:        func(c *config.Config, _ func(string) string) any { return c.Review.PostComments },
	},
	"auto_merge.self_authored": {
		Kind:        kindBool,
		Description: "The App merges its own CI-green PRs without a human queue approval (default on)",
		read:        func(c *config.Config, _ func(string) string) any { return c.AutoMerge.SelfAuthoredEnabled() },
	},
	"auto_merge.required_checks": {
		Kind:        kindList,
		Description: "Status checks every automated merge must see green",
		read: func(c *config.Config, _ func(string) string) any {
			return nonNilList(c.AutoMerge.RequiredChecks)
		},
	},
	"auto_merge.human_merge_paths": {
		Kind:        kindList,
		Description: "Per-repo path globs a person must merge",
		read: func(c *config.Config, _ func(string) string) any {
			out := []string{}
			for repo, patterns := range c.AutoMerge.HumanMergePaths {
				repo = strings.TrimSpace(repo)
				var kept []string
				for _, p := range patterns {
					if p = strings.TrimSpace(p); p != "" {
						kept = append(kept, p)
					}
				}
				if repo == "" || len(kept) == 0 {
					continue
				}
				out = append(out, repo+": "+strings.Join(kept, ", "))
			}
			sort.Strings(out)
			return out
		},
	},
	"auto_merge.trusted_authors.enabled": {
		Kind:        kindBool,
		Description: "Merge-authority holders' own PRs auto-merge without a second person",
		read: func(c *config.Config, _ func(string) string) any {
			return c.AutoMerge.TrustedAuthors.Enabled
		},
	},
	"auto_merge.trusted_authors.require_github_permission": {
		Kind:        kindBool,
		Description: "Trusted-author lane also requires GitHub push/maintain/admin permission (default on)",
		read: func(c *config.Config, _ func(string) string) any {
			return c.AutoMerge.TrustedAuthors.EffectiveRequireGitHubPermission()
		},
	},
	"sentinel.enabled": {
		Kind:        kindBool,
		Description: "Sentinel flags PRs that look like security overrides or privilege escalation (default on)",
		read:        func(c *config.Config, _ func(string) string) any { return c.Sentinel.IsEnabled() },
	},
	"sentinel.disabled_behaviors": {
		Kind:        kindList,
		Description: "Sentinel detection rules switched off",
		read: func(c *config.Config, _ func(string) string) any {
			return nonNilList(c.Sentinel.DisabledBehaviors)
		},
	},
	"escalation.disabled": {
		Kind:        kindBool,
		Description: "Repeated-red-CI escalation breaker switched off",
		read:        func(c *config.Config, _ func(string) string) any { return c.Escalation.Disabled },
	},
	"tool_approval.enabled": {
		Kind:        kindBool,
		Description: "Approval desk: approval-shaped agent requests resolve through operator rules",
		read:        func(c *config.Config, _ func(string) string) any { return c.ToolApproval.Enabled },
	},
	"agent_sandbox.enabled": {
		Kind:        kindBool,
		Description: "Credential-free sandbox runner for agent kicks",
		read:        func(c *config.Config, _ func(string) string) any { return c.AgentSandbox.Enabled },
	},
	"acmm_level": {
		Kind:        kindInt,
		Description: "ACMM autonomy level (L5 = holdgated, L6 = agents merge on green)",
		read: func(c *config.Config, _ func(string) string) any {
			if c.ACMMLevel == nil {
				return nil
			}
			return *c.ACMMLevel
		},
	},
	"audit.retention_days": {
		Kind:        kindInt,
		Description: "Audit log retention in days (fixed in code today; configurable floor tracked by #11077)",
		Builtin:     true,
		read:        func(*config.Config, func(string) string) any { return BuiltinAuditRetentionDays },
	},
	"env." + config.ProxyInjectGHAuthEnv: {
		Kind:        kindBool,
		Description: "Proxy injects the GitHub credential so agents never hold the real token",
		read: func(_ *config.Config, getenv func(string) string) any {
			return config.ResolveProxyInjectGHAuth(getenv).Enabled
		},
	},
}

func nonNilList(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SettingInfo describes one known setting path for the UI and docs.
type SettingInfo struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Builtin     bool   `json:"builtin,omitempty"`
}

// KnownSettings lists every setting path a profile mapping may reference,
// sorted by path.
func KnownSettings() []SettingInfo {
	out := make([]SettingInfo, 0, len(settings))
	for p, s := range settings {
		out = append(out, SettingInfo{Path: p, Kind: s.Kind, Description: s.Description, Builtin: s.Builtin})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
