package main

import (
	"strconv"

	"github.com/hivecommons/hive/pkg/config"
)

// Boot-time decisions lifted out of main() so they can be asserted without
// starting the process (#7232, step 2). Each helper is pure: it takes the
// inputs main() would read from the environment or config and returns what
// main() should do. main() keeps the effects.

// hiveConfigEnv is the environment variable entrypoint.sh uses to redirect
// the config path; -config on the command line outranks it.
const hiveConfigEnv = "HIVE_CONFIG"

// defaultConfigPath is where a hive looks for hive.yaml when neither
// HIVE_CONFIG nor -config says otherwise.
const defaultConfigPath = "/etc/hive/hive.yaml"

// resolveDefaultConfigPath picks the -config flag default: HIVE_CONFIG when
// set, else the packaged path.
func resolveDefaultConfigPath(envConfig string) string {
	if envConfig != "" {
		return envConfig
	}
	return defaultConfigPath
}

// configPathDisagrees reports the #5989 failure mode: HIVE_CONFIG names one
// file while an explicit -config made the process load another. Persisted
// state written to the loaded file is then invisible to everything reading
// HIVE_CONFIG directly. An unset HIVE_CONFIG can never disagree.
func configPathDisagrees(envConfig, loadedConfig string) bool {
	return envConfig != "" && envConfig != loadedConfig
}

// gitShortSHALen is the short-SHA width the hub stores and compares against.
const gitShortSHALen = 7

// canonicalGitShort trims a build-stamped short SHA to the hub's width. git
// may emit more than 7 characters when 7 is not unique in the repo; the hub
// always stores 7, so anything longer would never match.
func canonicalGitShort(sha string) string {
	if len(sha) > gitShortSHALen {
		return sha[:gitShortSHALen]
	}
	return sha
}

// hubTarget is what main() should use to reach the hub after the environment
// has been applied over cfg.Hub.
type hubTarget struct {
	url       string
	enabled   bool
	clusterID string
}

// resolveHubTarget applies HIVE_HUB_URL / HIVE_CLUSTER_ID over the hub config.
// A non-empty HIVE_HUB_URL both enables the hub AND replaces the URL — a
// hosted spoke is pointed at its hub by environment, never by hive.yaml.
// HIVE_CLUSTER_ID only overrides the cluster id when set.
func resolveHubTarget(hub config.HubConfig, envHubURL, envClusterID string) hubTarget {
	t := hubTarget{url: hub.URL, enabled: hub.Enabled, clusterID: hub.ClusterID}
	if envHubURL != "" {
		t.url = envHubURL
		t.enabled = true
	}
	if envClusterID != "" {
		t.clusterID = envClusterID
	}
	return t
}

// heartbeatsToHub reports whether main() should start the hub heartbeat: the
// hub must be enabled AND reachable by URL.
func (t hubTarget) heartbeatsToHub() bool {
	return t.enabled && t.url != ""
}

// maxACMMLevel is the highest ACMM maturity pack a hive can apply.
const maxACMMLevel = 6

// hiveLevelEnv seeds the ACMM level on FIRST start only; a persisted level
// always wins over it afterwards.
const hiveLevelEnv = "HIVE_LEVEL"

// acmmBootPlan is what main() should do about the ACMM pack at boot.
type acmmBootPlan struct {
	// level is the pack to apply; 0 means apply nothing.
	level int
	// action is the audit verb for the log line: "auto-applying" on a first
	// start, "merging pack updates" when the level is unchanged, or
	// "re-applying pack (level changed)" when it moved.
	action string
	// invalidEnv is the rejected HIVE_LEVEL value, if the environment named
	// a level that is not 1..maxACMMLevel; empty otherwise.
	invalidEnv string
}

const (
	acmmActionAutoApply = "auto-applying ACMM pack"
	acmmActionMerge     = "merging pack updates"
	acmmActionReapply   = "re-applying pack (level changed)"
)

// parseACMMLevel parses HIVE_LEVEL, reporting false for anything outside
// 1..maxACMMLevel or non-numeric.
func parseACMMLevel(s string) (int, bool) {
	level, err := strconv.Atoi(s)
	if err != nil || level < 1 || level > maxACMMLevel {
		return 0, false
	}
	return level, true
}

func validACMMLevel(p *int) (int, bool) {
	if p == nil || *p < 1 || *p > maxACMMLevel {
		return 0, false
	}
	return *p, true
}

// planACMMBoot decides which ACMM pack to apply at startup.
//
// firstStart (no persisted state): HIVE_LEVEL is the only source, and it is
// applied as a first-provisioning step.
//
// Restart: the config file is authoritative, then the persisted level, and
// HIVE_LEVEL is only a fallback when neither names a valid level. An invalid
// HIVE_LEVEL is reported but never applied. The action distinguishes a merge
// (same level as persisted) from a re-apply (level changed) so the audit log
// says which happened.
func planACMMBoot(firstStart bool, cfgLevel, savedLevel *int, envLevel string) acmmBootPlan {
	if firstStart {
		if envLevel == "" {
			return acmmBootPlan{}
		}
		level, ok := parseACMMLevel(envLevel)
		if !ok {
			return acmmBootPlan{invalidEnv: envLevel}
		}
		return acmmBootPlan{level: level, action: acmmActionAutoApply}
	}

	var plan acmmBootPlan
	if level, ok := validACMMLevel(cfgLevel); ok {
		plan.level = level
	} else if level, ok := validACMMLevel(savedLevel); ok {
		plan.level = level
	} else if envLevel != "" {
		if level, ok := parseACMMLevel(envLevel); ok {
			plan.level = level
		} else {
			plan.invalidEnv = envLevel
		}
	}
	if plan.level == 0 {
		return plan
	}
	plan.action = acmmActionMerge
	if savedLevel == nil || *savedLevel != plan.level {
		plan.action = acmmActionReapply
	}
	return plan
}

// upgradeMarkerVerdict is what a marker left by a previous self-upgrade
// attempt says about THIS boot.
type upgradeMarkerVerdict int

const (
	// upgradeLanded: we booted on a different SHA than the one that requested
	// the upgrade — it worked; drop the marker so the attempt budget resets.
	upgradeLanded upgradeMarkerVerdict = iota
	// upgradeDidNotLand: same SHA as the attempt that ran before this boot,
	// so the image never changed and that attempt failed.
	upgradeDidNotLand
)

// judgeUpgradeMarker compares the marker's recorded SHA with the running one.
func judgeUpgradeMarker(m upgradeMarker, runningSHA string) upgradeMarkerVerdict {
	if m.CurrentSHA != runningSHA {
		return upgradeLanded
	}
	return upgradeDidNotLand
}

// coverageBadgeURLEnv overrides the coverage badge the dashboard renders.
const coverageBadgeURLEnv = "HIVE_COVERAGE_BADGE_URL"

// defaultCoverageBadgeURL is the fleet-wide badge used when the env is unset.
const defaultCoverageBadgeURL = "https://gist.githubusercontent.com/clubanderson/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json"

func resolveCoverageBadgeURL(env string) string {
	if env != "" {
		return env
	}
	return defaultCoverageBadgeURL
}

// metricsPrimaryRepo is the repo the metrics collector reports on:
// project.primary_repo, else the first configured repo, else "".
func metricsPrimaryRepo(project config.ProjectConfig) string {
	if project.PrimaryRepo != "" {
		return project.PrimaryRepo
	}
	if len(project.Repos) > 0 {
		return project.Repos[0]
	}
	return ""
}

// fleetStatsIdentity is who the fleet-stats collector counts PRs for.
type fleetStatsIdentity struct {
	author string
	// fromToken is set when author was derived from the bot token's login
	// because project.ai_author was empty.
	fromToken bool
	// lookupErr is the identity-lookup failure when a token was available but
	// could not be resolved; the collector then runs with an empty author.
	lookupErr error
}

// enabled reports whether the collector will count anything: it needs both an
// author and an org, otherwise Start() returns early and the public
// fleet-stats strip shows nothing for this hive.
func (f fleetStatsIdentity) enabled(org string) bool {
	return f.author != "" && org != ""
}

// resolveFleetStatsIdentity picks the AI author for fleet stats.
//
// configuredAuthor is cfg.EffectiveAIAuthor() — App-authored hives derive
// "<slug>[bot]" from the installed App and leave ai_author empty, so the raw
// field must never be read here. When that is empty and a bot token exists
// (config, else HIVE_GITHUB_TOKEN), the token's own login is the author: it IS
// the account the agents open PRs as. It never falls back to an author-less
// org-wide search, which would count human PRs as agent work.
func resolveFleetStatsIdentity(configuredAuthor, cfgToken, envToken string, lookupLogin func(token string) (string, error)) fleetStatsIdentity {
	id := fleetStatsIdentity{author: configuredAuthor}
	if id.author != "" {
		return id
	}
	token := cfgToken
	if token == "" {
		token = envToken
	}
	if token == "" || lookupLogin == nil {
		return id
	}
	login, err := lookupLogin(token)
	if err != nil {
		id.lookupErr = err
		return id
	}
	if login != "" {
		id.author = login
		id.fromToken = true
	}
	return id
}

// defaultPoliciesLocalDir is where policy files are checked out when
// policies.local_dir is unset.
const defaultPoliciesLocalDir = "/data/policies"

// policiesLocalDir is the checkout root the policies watcher syncs into.
func policiesLocalDir(p config.PoliciesConfig) string {
	if p.LocalDir != "" {
		return p.LocalDir
	}
	return defaultPoliciesLocalDir
}

// policyDir is where agents look for policy files: the checkout root plus
// policies.path when one is configured. It is never empty.
func policyDir(p config.PoliciesConfig) string {
	dir := policiesLocalDir(p)
	if p.Path != "" {
		dir += "/" + p.Path
	}
	return dir
}
