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
