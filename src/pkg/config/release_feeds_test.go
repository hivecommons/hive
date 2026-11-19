package config

import (
	"strings"
	"testing"
	"time"
)

func releaseFeedTestConfig() *Config {
	return &Config{Governor: GovernorConfig{ReleaseFeeds: &ReleaseFeedsConfig{
		Agents: map[string]ReleaseFeedAgentConfig{
			"deps": {Sources: []ReleaseFeedSource{{Kind: ReleaseFeedKindPyPI, Package: "requests"}}},
		},
	}}}
}

func TestReleaseFeedsEnabled(t *testing.T) {
	var nilCfg *Config
	if nilCfg.ReleaseFeedsEnabled() || (&Config{}).ReleaseFeedsEnabled() {
		t.Fatal("no release_feeds block must mean off")
	}
	cfg := releaseFeedTestConfig()
	if !cfg.ReleaseFeedsEnabled() {
		t.Fatal("an agent with a source must turn the watcher on")
	}
	off := false
	cfg.Governor.ReleaseFeeds.Enabled = &off
	if cfg.ReleaseFeedsEnabled() {
		t.Fatal("enabled: false must win")
	}
	empty := &Config{Governor: GovernorConfig{ReleaseFeeds: &ReleaseFeedsConfig{
		Agents: map[string]ReleaseFeedAgentConfig{"deps": {}},
	}}}
	if empty.ReleaseFeedsEnabled() {
		t.Fatal("agents without sources must leave the watcher off")
	}
}

func TestReleaseFeedInterval_Precedence(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := releaseFeedInterval(nil, getenv); got != DefaultReleaseFeedInterval {
		t.Fatalf("default = %s", got)
	}
	env[ReleaseFeedIntervalEnv] = "5m"
	if got := releaseFeedInterval(nil, getenv); got != 5*time.Minute {
		t.Fatalf("env = %s", got)
	}
	env[ReleaseFeedIntervalEnv] = "bogus"
	if got := releaseFeedInterval(nil, getenv); got != DefaultReleaseFeedInterval {
		t.Fatalf("bad env must fall back to default, got %s", got)
	}
	env[ReleaseFeedIntervalEnv] = "1s"
	if got := releaseFeedInterval(nil, getenv); got != MinReleaseFeedInterval {
		t.Fatalf("interval below the floor must clamp, got %s", got)
	}
	cfg := releaseFeedTestConfig()
	cfg.Governor.ReleaseFeeds.IntervalS = 600
	if got := releaseFeedInterval(cfg, getenv); got != 10*time.Minute {
		t.Fatalf("config must win over env, got %s", got)
	}
	if got := cfg.ReleaseFeedInterval(); got != 10*time.Minute {
		t.Fatalf("ReleaseFeedInterval = %s", got)
	}
}

func TestReleaseFeedCooldown_Precedence(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := releaseFeedCooldown(nil, "deps", getenv); got != DefaultReleaseFeedCooldown {
		t.Fatalf("default = %s", got)
	}
	env[ReleaseFeedCooldownEnv] = "20m"
	if got := releaseFeedCooldown(nil, "deps", getenv); got != 20*time.Minute {
		t.Fatalf("env = %s", got)
	}
	cfg := releaseFeedTestConfig()
	cfg.Governor.ReleaseFeeds.CooldownS = 120
	if got := releaseFeedCooldown(cfg, "deps", getenv); got != 2*time.Minute {
		t.Fatalf("block cooldown = %s", got)
	}
	a := cfg.Governor.ReleaseFeeds.Agents["deps"]
	a.CooldownS = 30
	cfg.Governor.ReleaseFeeds.Agents["deps"] = a
	if got := releaseFeedCooldown(cfg, "deps", getenv); got != 30*time.Second {
		t.Fatalf("agent cooldown = %s", got)
	}
	if got := releaseFeedCooldown(cfg, "other", getenv); got != 2*time.Minute {
		t.Fatalf("other agent takes the block cooldown, got %s", got)
	}
	if got := cfg.ReleaseFeedCooldown("deps"); got != 30*time.Second {
		t.Fatalf("ReleaseFeedCooldown = %s", got)
	}
	if _, ok := envDuration(nil, ReleaseFeedCooldownEnv); ok {
		t.Fatal("nil getenv must report unset")
	}
}

func TestValidateReleaseFeeds(t *testing.T) {
	if err := validateReleaseFeeds(nil); err != nil {
		t.Fatal(err)
	}
	ok := []ReleaseFeedSource{
		{Kind: ReleaseFeedKindGitHubRelease, Repo: "psf/requests"},
		{Kind: ReleaseFeedKindGitHubTag, Repo: "psf/requests"},
		{Kind: ReleaseFeedKindPyPI, Package: "requests", AuthVar: "PYPI_TOKEN"},
		{Kind: ReleaseFeedKindIndex, URL: "https://index.example.com/latest"},
	}
	if err := validateReleaseFeeds(&ReleaseFeedsConfig{Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: ok}}}); err != nil {
		t.Fatalf("valid sources rejected: %v", err)
	}
	cases := map[string]*ReleaseFeedsConfig{
		"interval_s":             {IntervalS: -1},
		"cooldown_s":             {CooldownS: -1},
		"agents.deps.cooldown_s": {Agents: map[string]ReleaseFeedAgentConfig{"deps": {CooldownS: -1}}},
		"owner/repo":             {Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: []ReleaseFeedSource{{Kind: ReleaseFeedKindGitHubRelease, Repo: "requests"}}}}},
		"needs package":          {Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: []ReleaseFeedSource{{Kind: ReleaseFeedKindPyPI}}}}},
		"http(s) url":            {Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: []ReleaseFeedSource{{Kind: ReleaseFeedKindIndex, URL: "ftp://x"}}}}},
		"kind must be":           {Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: []ReleaseFeedSource{{Kind: "npm"}}}}},
		"auth_var":               {Agents: map[string]ReleaseFeedAgentConfig{"deps": {Sources: []ReleaseFeedSource{{Kind: ReleaseFeedKindPyPI, Package: "x", AuthVar: "bad-name"}}}}},
	}
	for want, rf := range cases {
		err := validateReleaseFeeds(rf)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

// Config.Validate rejects a bad release_feeds block, so a typo fails at load
// instead of the watcher silently skipping the feed.
func TestValidate_RejectsBadReleaseFeeds(t *testing.T) {
	cfg := validatableConfig(nil)
	cfg.Governor.ReleaseFeeds = &ReleaseFeedsConfig{Agents: map[string]ReleaseFeedAgentConfig{
		"deps": {Sources: []ReleaseFeedSource{{Kind: "npm"}}},
	}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "release_feeds") {
		t.Fatalf("Validate = %v, want a release_feeds error", err)
	}
}
