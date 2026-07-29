package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BeadSynthesizerConfig.IsEnabled / ChannelConfig.IsEnabled
// ---------------------------------------------------------------------------

func TestBeadSynthesizerConfig_IsEnabled(t *testing.T) {
	trueVal := true
	falseVal := false
	tests := []struct {
		name string
		cfg  BeadSynthesizerConfig
		want bool
	}{
		{"nil defaults to enabled", BeadSynthesizerConfig{}, true},
		{"explicit true", BeadSynthesizerConfig{Enabled: &trueVal}, true},
		{"explicit false", BeadSynthesizerConfig{Enabled: &falseVal}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChannelConfig_IsEnabled(t *testing.T) {
	trueVal := true
	falseVal := false
	tests := []struct {
		name string
		cfg  ChannelConfig
		want bool
	}{
		{"nil defaults to enabled", ChannelConfig{Type: "kick"}, true},
		{"explicit true", ChannelConfig{Type: "kick", Enabled: &trueVal}, true},
		{"explicit false", ChannelConfig{Type: "kick", Enabled: &falseVal}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AgentConfig.HasChannel / ChannelsOfType / UsesGovernorKick
// ---------------------------------------------------------------------------

func TestAgentConfig_HasChannel(t *testing.T) {
	a := AgentConfig{Channels: []ChannelConfig{
		{Type: "kick"},
		{Type: "webhook"},
	}}
	if !a.HasChannel("kick") {
		t.Error("expected HasChannel(kick) to be true")
	}
	if !a.HasChannel("webhook") {
		t.Error("expected HasChannel(webhook) to be true")
	}
	if a.HasChannel("discord") {
		t.Error("expected HasChannel(discord) to be false")
	}
}

func TestAgentConfig_ChannelsOfType(t *testing.T) {
	a := AgentConfig{Channels: []ChannelConfig{
		{Type: "webhook", Events: []string{"push"}},
		{Type: "webhook", Events: []string{"pr"}},
		{Type: "kick"},
	}}
	webhooks := a.ChannelsOfType("webhook")
	if len(webhooks) != 2 {
		t.Fatalf("expected 2 webhook channels, got %d", len(webhooks))
	}
	if len(a.ChannelsOfType("discord")) != 0 {
		t.Error("expected 0 discord channels")
	}
}

func TestAgentConfig_UsesGovernorKick(t *testing.T) {
	tests := []struct {
		name string
		a    AgentConfig
		want bool
	}{
		{"no channels implies kick", AgentConfig{}, true},
		{"explicit kick channel", AgentConfig{Channels: []ChannelConfig{{Type: "kick"}}}, true},
		{"only webhook channel, no kick", AgentConfig{Channels: []ChannelConfig{{Type: "webhook", Events: []string{"push"}}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.UsesGovernorKick(); got != tt.want {
				t.Errorf("UsesGovernorKick() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GatewayConfig.ResolveAPIKey
// ---------------------------------------------------------------------------

func TestGatewayConfig_ResolveAPIKey(t *testing.T) {
	t.Run("from env var", func(t *testing.T) {
		t.Setenv("GW_TEST_KEY", "env-key-value")
		gw := GatewayConfig{APIKeyEnv: "GW_TEST_KEY"}
		if got := gw.ResolveAPIKey(); got != "env-key-value" {
			t.Errorf("got %q, want env-key-value", got)
		}
	})

	t.Run("from file when env unset", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key.txt")
		if err := os.WriteFile(keyPath, []byte("  file-key-value  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gw := GatewayConfig{APIKeyFile: keyPath}
		if got := gw.ResolveAPIKey(); got != "file-key-value" {
			t.Errorf("got %q, want file-key-value", got)
		}
	})

	t.Run("env takes priority over file", func(t *testing.T) {
		t.Setenv("GW_TEST_KEY2", "env-wins")
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key.txt")
		os.WriteFile(keyPath, []byte("file-loses"), 0o600)
		gw := GatewayConfig{APIKeyEnv: "GW_TEST_KEY2", APIKeyFile: keyPath}
		if got := gw.ResolveAPIKey(); got != "env-wins" {
			t.Errorf("got %q, want env-wins", got)
		}
	})

	t.Run("unset returns empty", func(t *testing.T) {
		gw := GatewayConfig{}
		if got := gw.ResolveAPIKey(); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("file path missing falls through to empty", func(t *testing.T) {
		gw := GatewayConfig{APIKeyFile: "/nonexistent/path/to/key"}
		if got := gw.ResolveAPIKey(); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// ---------------------------------------------------------------------------
// InferenceAuthConfig.ResolveAPIKey
// ---------------------------------------------------------------------------

func TestInferenceAuthConfig_ResolveAPIKey(t *testing.T) {
	t.Run("from file", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key.txt")
		os.WriteFile(keyPath, []byte("file-key"), 0o600)
		c := &InferenceAuthConfig{APIKeyFile: keyPath}
		if got := c.ResolveAPIKey("DEFAULT_ENV_UNUSED"); got != "file-key" {
			t.Errorf("got %q, want file-key", got)
		}
	})

	t.Run("from named env when file unset", func(t *testing.T) {
		t.Setenv("INF_TEST_KEY", "named-env-key")
		c := &InferenceAuthConfig{APIKeyEnv: "INF_TEST_KEY"}
		if got := c.ResolveAPIKey("DEFAULT_ENV_UNUSED"); got != "named-env-key" {
			t.Errorf("got %q, want named-env-key", got)
		}
	})

	t.Run("from default env when nothing else set", func(t *testing.T) {
		t.Setenv("INF_DEFAULT_KEY", "default-env-key")
		c := &InferenceAuthConfig{}
		if got := c.ResolveAPIKey("INF_DEFAULT_KEY"); got != "default-env-key" {
			t.Errorf("got %q, want default-env-key", got)
		}
	})

	t.Run("nothing configured returns empty", func(t *testing.T) {
		c := &InferenceAuthConfig{}
		if got := c.ResolveAPIKey(""); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty file falls through to env", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key.txt")
		os.WriteFile(keyPath, []byte(""), 0o600)
		t.Setenv("INF_TEST_KEY2", "fallback-env")
		c := &InferenceAuthConfig{APIKeyFile: keyPath, APIKeyEnv: "INF_TEST_KEY2"}
		if got := c.ResolveAPIKey(""); got != "fallback-env" {
			t.Errorf("got %q, want fallback-env", got)
		}
	})
}

// ---------------------------------------------------------------------------
// LiteLLMConfig.ResolveAPIKeySource
// ---------------------------------------------------------------------------

func TestLiteLLMConfig_ResolveAPIKeySource(t *testing.T) {
	t.Run("from configured file", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "key.txt")
		os.WriteFile(keyPath, []byte("secretvalue"), 0o600)
		c := &LiteLLMConfig{APIKeyFile: keyPath}
		src := c.ResolveAPIKeySource()
		if src != "file:"+keyPath {
			t.Errorf("got %q, want file:%s", src, keyPath)
		}
	})

	t.Run("from named env", func(t *testing.T) {
		t.Setenv("LITELLM_TEST_KEY", "envval")
		c := &LiteLLMConfig{APIKeyEnv: "LITELLM_TEST_KEY"}
		src := c.ResolveAPIKeySource()
		if src != "env:LITELLM_TEST_KEY" {
			t.Errorf("got %q, want env:LITELLM_TEST_KEY", src)
		}
	})

	t.Run("nothing configured returns empty source", func(t *testing.T) {
		c := &LiteLLMConfig{}
		if src := c.ResolveAPIKeySource(); src != "" {
			t.Errorf("got %q, want empty", src)
		}
	})
}

// ---------------------------------------------------------------------------
// GitHubConfig.IsGHE / ResolvedAppSlug / AppInstallURL
// ---------------------------------------------------------------------------

func TestGitHubConfig_IsGHE(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"empty base url is not GHE", "", false},
		{"default github.com is not GHE", DefaultGitHubBaseURL, false},
		{"custom base url is GHE", "https://github.mycorp.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GitHubConfig{BaseURL: tt.baseURL}
			if got := g.IsGHE(); got != tt.want {
				t.Errorf("IsGHE() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGitHubConfig_ResolvedAppSlug(t *testing.T) {
	g := GitHubConfig{}
	if got := g.ResolvedAppSlug(); got != DefaultGitHubAppSlug {
		t.Errorf("got %q, want default %q", got, DefaultGitHubAppSlug)
	}
	g.AppSlug = "custom-slug"
	if got := g.ResolvedAppSlug(); got != "custom-slug" {
		t.Errorf("got %q, want custom-slug", got)
	}
}

func TestGitHubConfig_AppInstallURL(t *testing.T) {
	t.Run("github.com uses /apps/ path", func(t *testing.T) {
		g := GitHubConfig{}
		want := "https://github.com/apps/" + DefaultGitHubAppSlug + "/installations/new"
		if got := g.AppInstallURL(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("GHE uses /github-apps/ path", func(t *testing.T) {
		g := GitHubConfig{BaseURL: "https://github.mycorp.com/", AppSlug: "corp-hive"}
		want := "https://github.mycorp.com/github-apps/corp-hive/installations/new"
		if got := g.AppInstallURL(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestGitHubConfig_AppInstallURL_Table locks the exact install URL for every
// host/slug shape the hub and spoke can hand this function. The regression it
// guards: a GitHub Enterprise user sent to public github.com never reaches
// their own org's install flow, so the request silently disappears and their
// GHE admin sees nothing pending.
func TestGitHubConfig_AppInstallURL_Table(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		appSlug string
		want    string
	}{
		{
			name: "public github.com with default slug",
			want: "https://github.com/apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
		{
			name:    "explicit public base url is not treated as GHE",
			baseURL: DefaultGitHubBaseURL,
			want:    "https://github.com/apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
		{
			name:    "public github.com with a custom slug",
			appSlug: "acme-hive",
			want:    "https://github.com/apps/acme-hive/installations/new",
		},
		{
			name:    "GHE host uses the enterprise /github-apps/ path",
			baseURL: "https://github.ibm.com",
			want:    "https://github.ibm.com/github-apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
		{
			name:    "GHE host with a trailing slash does not double up",
			baseURL: "https://github.cisco.com/",
			want:    "https://github.cisco.com/github-apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
		{
			name:    "GHE host with multiple trailing slashes",
			baseURL: "https://github.cisco.com///",
			want:    "https://github.cisco.com/github-apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
		{
			name:    "GHE host honours a per-instance app slug",
			baseURL: "https://github.ibm.com",
			appSlug: "katamari-hive",
			want:    "https://github.ibm.com/github-apps/katamari-hive/installations/new",
		},
		{
			name:    "empty base url falls back to public github.com",
			baseURL: "",
			appSlug: "acme-hive",
			want:    "https://github.com/apps/acme-hive/installations/new",
		},
		{
			// A malformed base URL is echoed back rather than silently
			// rewritten to github.com: sending a GHE user to public GitHub is
			// the exact failure this function exists to prevent, so a visibly
			// broken link is the safer outcome than a plausible wrong one.
			name:    "malformed base url is not rewritten to github.com",
			baseURL: "not a url",
			want:    "not a url/github-apps/" + DefaultGitHubAppSlug + "/installations/new",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GitHubConfig{BaseURL: tt.baseURL, AppSlug: tt.appSlug}
			if got := g.AppInstallURL(); got != tt.want {
				t.Errorf("AppInstallURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DashboardConfig.AuthorizedRole / splitAuthorizedEntry
// ---------------------------------------------------------------------------

func TestDashboardConfig_AuthorizedRole(t *testing.T) {
	d := DashboardConfig{AuthorizedUsers: []string{
		"owner-user",
		"reader-user",
		"explicit-owner:owner",
		"explicit-reader:read",
		"weird:banana",
	}}

	tests := []struct {
		name     string
		username string
		wantRole string
		wantOK   bool
	}{
		{"empty username", "", "", false},
		{"first entry defaults to owner", "owner-user", RoleOwner, true},
		{"later bare entry defaults to read", "reader-user", RoleRead, true},
		{"case-insensitive match", "OWNER-USER", RoleOwner, true},
		{"explicit owner suffix", "explicit-owner", RoleOwner, true},
		{"explicit read suffix", "explicit-reader", RoleRead, true},
		{"unknown role suffix downgrades to bare-name match", "weird:banana", RoleRead, true},
		{"not found", "someone-else", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := d.AuthorizedRole(tt.username)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Errorf("AuthorizedRole(%q) = (%q, %v), want (%q, %v)", tt.username, role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestDashboardConfig_AuthorizedRole_WeirdSuffixMatchesWholeEntry(t *testing.T) {
	// An unknown role suffix causes splitAuthorizedEntry to treat the WHOLE
	// string (including the colon) as the username, per the safety comment
	// in splitAuthorizedEntry. Confirm that a lookup for the literal whole
	// string succeeds since it's now the only-entry position (position 0),
	// hence defaults to owner.
	d := DashboardConfig{AuthorizedUsers: []string{"weird:banana"}}
	role, ok := d.AuthorizedRole("weird:banana")
	if !ok || role != RoleOwner {
		t.Errorf("AuthorizedRole(weird:banana) = (%q, %v), want (%q, true)", role, ok, RoleOwner)
	}
}

func TestSplitAuthorizedEntry(t *testing.T) {
	tests := []struct {
		entry    string
		wantName string
		wantRole string
	}{
		{"alice", "alice", ""},
		{"alice:owner", "alice", RoleOwner},
		{"alice:read", "alice", RoleRead},
		{"alice:READ", "alice", RoleRead},
		{"alice:bogus", "alice:bogus", ""},
		{"  alice  :  owner  ", "alice", RoleOwner},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			name, role := splitAuthorizedEntry(tt.entry)
			if name != tt.wantName || role != tt.wantRole {
				t.Errorf("splitAuthorizedEntry(%q) = (%q, %q), want (%q, %q)", tt.entry, name, role, tt.wantName, tt.wantRole)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateChannels / validateTools / validateConnections
// ---------------------------------------------------------------------------

func TestValidateChannels(t *testing.T) {
	tests := []struct {
		name    string
		ch      []ChannelConfig
		wantErr bool
	}{
		{"empty is valid", nil, false},
		{"valid kick", []ChannelConfig{{Type: "kick"}}, false},
		{"invalid type", []ChannelConfig{{Type: "carrier-pigeon"}}, true},
		{"webhook missing events", []ChannelConfig{{Type: "webhook"}}, true},
		{"webhook with events", []ChannelConfig{{Type: "webhook", Events: []string{"push"}}}, false},
		{"discord missing patterns", []ChannelConfig{{Type: "discord"}}, true},
		{"discord with patterns", []ChannelConfig{{Type: "discord", Patterns: []string{"*.go"}}}, false},
		{"schedule missing cron", []ChannelConfig{{Type: "schedule"}}, true},
		{"schedule with cron", []ChannelConfig{{Type: "schedule", Schedule: "0 * * * *"}}, false},
		{"bead missing match", []ChannelConfig{{Type: "bead"}}, true},
		{"bead with match", []ChannelConfig{{Type: "bead", Match: map[string]string{"status": "open"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateChannels("agent1", tt.ch)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateChannels() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTools(t *testing.T) {
	tests := []struct {
		name    string
		tools   *ToolsConfig
		wantErr bool
	}{
		{"nil tools valid", nil, false},
		{"empty preset valid", &ToolsConfig{}, false},
		{"valid preset advisory", &ToolsConfig{Preset: "advisory"}, false},
		{"valid preset full", &ToolsConfig{Preset: "full"}, false},
		{"invalid preset", &ToolsConfig{Preset: "bogus"}, true},
		{"rule missing pattern", &ToolsConfig{Rules: []ToolRule{{Action: "allow"}}}, true},
		{"rule invalid action", &ToolsConfig{Rules: []ToolRule{{Pattern: "foo", Action: "maybe"}}}, true},
		{"rule valid allow", &ToolsConfig{Rules: []ToolRule{{Pattern: "foo", Action: "allow"}}}, false},
		{"rule valid deny", &ToolsConfig{Rules: []ToolRule{{Pattern: "foo", Action: "deny"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTools("agent1", tt.tools)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTools() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateConnections(t *testing.T) {
	tests := []struct {
		name    string
		conns   []ConnectionConfig
		wantErr bool
	}{
		{"empty valid", nil, false},
		{"missing name", []ConnectionConfig{{Type: "mcp", URI: "http://x"}}, true},
		{"duplicate name", []ConnectionConfig{
			{Name: "a", Type: "mcp", URI: "http://x"},
			{Name: "a", Type: "api", URI: "http://y"},
		}, true},
		{"invalid type", []ConnectionConfig{{Name: "a", Type: "carrier-pigeon"}}, true},
		{"mcp missing uri", []ConnectionConfig{{Name: "a", Type: "mcp"}}, true},
		{"api missing uri", []ConnectionConfig{{Name: "a", Type: "api"}}, true},
		{"knowledge without uri is fine", []ConnectionConfig{{Name: "a", Type: "knowledge"}}, false},
		{"mcp with uri valid", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x"}}, false},
		{"auth invalid type", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x", Auth: &ConnectionAuth{Type: "bogus"}}}, true},
		{"auth env missing env_var", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x", Auth: &ConnectionAuth{Type: "env"}}}, true},
		{"auth env valid", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x", Auth: &ConnectionAuth{Type: "env", EnvVar: "TOK"}}}, false},
		{"auth file missing file", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x", Auth: &ConnectionAuth{Type: "file"}}}, true},
		{"auth file valid", []ConnectionConfig{{Name: "a", Type: "mcp", URI: "http://x", Auth: &ConnectionAuth{Type: "file", File: "/x"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConnections("agent1", tt.conns)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConnections() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_ChannelsToolsConnectionsPropagate ensures Config.validate()
// actually calls through to validateChannels/validateTools/validateConnections
// for each agent (rather than these only being reachable in isolation).
func TestValidate_ChannelsToolsConnectionsPropagate(t *testing.T) {
	base := func() *Config {
		return &Config{
			Project: ProjectConfig{Org: "acme"},
			GitHub:  GitHubConfig{Token: "x"},
			Agents: map[string]AgentConfig{
				"scanner": {Backend: "claude"},
			},
		}
	}

	c := base()
	c.Agents["scanner"] = AgentConfig{Backend: "claude", Channels: []ChannelConfig{{Type: "bogus"}}}
	if err := c.validate(); err == nil {
		t.Error("expected channel validation error to propagate")
	}

	c = base()
	c.Agents["scanner"] = AgentConfig{Backend: "claude", Tools: &ToolsConfig{Preset: "bogus"}}
	if err := c.validate(); err == nil {
		t.Error("expected tools validation error to propagate")
	}

	c = base()
	c.Agents["scanner"] = AgentConfig{Backend: "claude", Connections: []ConnectionConfig{{Type: "bogus", Name: "a"}}}
	if err := c.validate(); err == nil {
		t.Error("expected connections validation error to propagate")
	}
}

// ---------------------------------------------------------------------------
// saveLocked: create-fallback path when OpenFile fails (e.g. path is a dir)
// ---------------------------------------------------------------------------

func TestSaveLocked_CreateFallbackWhenOpenFails(t *testing.T) {
	dir := t.TempDir()
	// SourcePath points at a path that is itself a directory, so
	// os.OpenFile(O_WRONLY|O_TRUNC) fails and saveLocked falls back to
	// os.WriteFile — which will ALSO fail because you cannot write a file
	// over an existing directory. This exercises the fallback branch's
	// error path.
	asDir := filepath.Join(dir, "hive.yaml")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		SourcePath: asDir,
		Project:    ProjectConfig{Org: "acme"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
	if err := cfg.Save(); err == nil {
		t.Fatal("expected error writing config over a directory path")
	}
}

// TestSaveLocked_FileDoesNotExistYet exercises the "file may not exist yet"
// fallback-to-create branch on the happy path (no file present at all).
func TestSaveLocked_FileDoesNotExistYet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "hive.yaml")
	// Note: parent dir "nested" does not exist either, but os.WriteFile
	// (the fallback) does not create parent dirs, so this should still fail
	// cleanly rather than panic; assert we get a graceful error rather than
	// a crash. This exercises the create-fallback branch's own error path
	// (distinct from the directory-collision case above).
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "acme"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
	if err := cfg.Save(); err == nil {
		t.Fatal("expected error writing config to a path whose parent dir does not exist")
	}
}

// ---------------------------------------------------------------------------
// dashboardOverlayBytes: DASHBOARD_AUTH_TOKEN / HIVE_DASHBOARD_TOKEN scrubbing
// ---------------------------------------------------------------------------

func TestDashboardOverlayBytes_ScrubsAuthToken(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
	}{
		{"DASHBOARD_AUTH_TOKEN", "DASHBOARD_AUTH_TOKEN"},
		{"HIVE_DASHBOARD_TOKEN", "HIVE_DASHBOARD_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envVar, "super-secret-token")
			c := &Config{
				Project:   ProjectConfig{Org: "acme"},
				Agents:    map[string]AgentConfig{"scanner": {Role: "scanner"}},
				Dashboard: DashboardConfig{AuthToken: "super-secret-token"},
			}
			data, err := c.dashboardOverlayBytes()
			if err != nil {
				t.Fatalf("dashboardOverlayBytes: %v", err)
			}
			if containsString(string(data), "super-secret-token") {
				t.Error("overlay bytes should not contain the raw auth token")
			}
			// Live config unaffected.
			if c.Dashboard.AuthToken != "super-secret-token" {
				t.Errorf("live config mutated: %q", c.Dashboard.AuthToken)
			}
		})
	}
}

func TestDashboardOverlayBytes_NoScrubWhenTokenDiffers(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_TOKEN", "env-token")
	c := &Config{
		Project:   ProjectConfig{Org: "acme"},
		Agents:    map[string]AgentConfig{"scanner": {Role: "scanner"}},
		Dashboard: DashboardConfig{AuthToken: "different-token-not-from-env"},
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		t.Fatalf("dashboardOverlayBytes: %v", err)
	}
	if !containsString(string(data), "different-token-not-from-env") {
		t.Error("overlay should keep a token that does not match the env var value")
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// tool_presets.go: EffectiveRules / DenyPatterns / EffectiveMode
// ---------------------------------------------------------------------------

func TestToolsConfig_EffectiveRules(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var tc *ToolsConfig
		if got := tc.EffectiveRules(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("no preset, no rules", func(t *testing.T) {
		tc := &ToolsConfig{}
		if got := tc.EffectiveRules(); len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("preset only", func(t *testing.T) {
		tc := &ToolsConfig{Preset: "advisory"}
		got := tc.EffectiveRules()
		if len(got) != len(ToolPresets["advisory"]) {
			t.Errorf("expected %d preset rules, got %d", len(ToolPresets["advisory"]), len(got))
		}
	})

	t.Run("unknown preset yields no preset rules", func(t *testing.T) {
		tc := &ToolsConfig{Preset: "totally-bogus"}
		if got := tc.EffectiveRules(); len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("override replaces matching preset rule via wildcard", func(t *testing.T) {
		tc := &ToolsConfig{
			Preset: "advisory",
			Rules: []ToolRule{
				{Pattern: "mcp__github__create_pull_request", Action: "allow"},
			},
		}
		got := tc.EffectiveRules()
		found := false
		for _, r := range got {
			if r.Pattern == "mcp__github__create_pull_request" {
				found = true
				if r.Action != "allow" {
					t.Errorf("expected override to allow, got %q", r.Action)
				}
			}
		}
		if !found {
			t.Error("expected overridden rule to be present")
		}
		// Should not have duplicated the pattern.
		count := 0
		for _, r := range got {
			if r.Pattern == "mcp__github__create_pull_request" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly 1 rule for the pattern, got %d", count)
		}
	})

	t.Run("override with new pattern appends", func(t *testing.T) {
		tc := &ToolsConfig{
			Preset: "issues-only",
			Rules: []ToolRule{
				{Pattern: "mcp__custom__thing", Action: "deny"},
			},
		}
		got := tc.EffectiveRules()
		found := false
		for _, r := range got {
			if r.Pattern == "mcp__custom__thing" {
				found = true
			}
		}
		if !found {
			t.Error("expected new rule to be appended")
		}
	})
}

func TestToolsConfig_DenyPatterns(t *testing.T) {
	tc := &ToolsConfig{Preset: "advisory"}
	patterns := tc.DenyPatterns()
	if len(patterns) == 0 {
		t.Error("expected advisory preset to have deny patterns")
	}
	for _, p := range patterns {
		if p == "" {
			t.Error("unexpected empty pattern")
		}
	}

	tc2 := &ToolsConfig{Preset: "issues-prs"}
	if got := tc2.DenyPatterns(); len(got) != 0 {
		t.Errorf("expected no deny patterns for issues-prs, got %v", got)
	}
}

func TestToolsConfig_EffectiveMode(t *testing.T) {
	tests := []struct {
		name string
		tc   *ToolsConfig
		want string
	}{
		{"nil receiver", nil, ""},
		{"preset advisory maps directly", &ToolsConfig{Preset: "advisory"}, "ADVISORY"},
		{"preset full maps directly", &ToolsConfig{Preset: "full"}, "ISSUES_PRS_MERGE"},
		{"preset issues-only maps directly", &ToolsConfig{Preset: "issues-only"}, "ISSUES_ONLY"},
		{"preset issues-prs maps directly", &ToolsConfig{Preset: "issues-prs"}, "ISSUES_AND_PRS"},
		{"no preset, no denies", &ToolsConfig{}, "ISSUES_PRS_MERGE"},
		{
			"no preset, deny create_issue implies advisory",
			&ToolsConfig{Rules: []ToolRule{{Pattern: "mcp__github__create_issue", Action: "deny"}}},
			"ADVISORY",
		},
		{
			"no preset, deny create_pull_request implies issues-only",
			&ToolsConfig{Rules: []ToolRule{{Pattern: "mcp__github__create_pull_request", Action: "deny"}}},
			"ISSUES_ONLY",
		},
		{
			"no preset, deny unrelated pattern implies issues-and-prs",
			&ToolsConfig{Rules: []ToolRule{{Pattern: "mcp__something__else", Action: "deny"}}},
			"ISSUES_AND_PRS",
		},
		{
			"unknown preset falls through to deny-based derivation",
			&ToolsConfig{Preset: "totally-bogus"},
			"ISSUES_PRS_MERGE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tc.EffectiveMode(); got != tt.want {
				t.Errorf("EffectiveMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// watcher.go: error paths not covered by watcher_test.go
// ---------------------------------------------------------------------------

// TestWatcher_AddNonExistentPathLogsAndReturns exercises the fsw.Add error
// branch in Start: watching a path in a directory that does not exist fails
// at Add() time, so Start should log and return promptly rather than block.
func TestWatcher_AddNonExistentPathLogsAndReturns(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWatcher("/definitely/does/not/exist/hive.yaml", func(cfg *Config) {}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Start returned promptly because fsw.Add failed — expected.
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Add failure on nonexistent path")
	}
}

// TestWatcher_ScheduleReloadNoopAfterStopped verifies scheduleReload becomes
// a no-op once cancelTimer has set stopped=true (covers the early-return
// branch in scheduleReload).
func TestWatcher_ScheduleReloadNoopAfterStopped(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWatcher("/tmp/unused.yaml", func(cfg *Config) {}, logger)

	w.cancelTimer() // sets stopped = true directly, no fsnotify involved
	w.scheduleReload()

	w.mu.Lock()
	timerIsNil := w.timer == nil
	w.mu.Unlock()
	if !timerIsNil {
		t.Error("expected scheduleReload to no-op (no timer set) once stopped")
	}
}

// TestWatcher_ReloadSkipsWhenSkipNextSet verifies SkipNext causes exactly one
// reload to be swallowed without invoking onChange.
func TestWatcher_ReloadSkipsWhenSkipNextSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var called bool
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWatcher(path, func(cfg *Config) { called = true }, logger)

	w.SkipNext()
	w.reload() // direct call, bypasses fsnotify plumbing entirely

	if called {
		t.Error("onChange should not be invoked when SkipNext was set")
	}

	// skipNext should now be cleared, so the fields are back to the initial
	// zero-ish state ready for a real reload.
	w.mu.Lock()
	skip := w.skipNext
	w.mu.Unlock()
	if skip {
		t.Error("expected skipNext to be cleared after one skipped reload")
	}
}

// ---------------------------------------------------------------------------
// resolveKeyFromFileThenEnv (exercised indirectly via ResolveReviewer, since
// it is unexported and only reachable that way).
// ---------------------------------------------------------------------------

func TestResolveReviewer_KeyFromTrajectoryFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "reviewer-key.txt")
	if err := os.WriteFile(keyPath, []byte("  reviewer-file-key  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &GovernorConfig{
		Trajectory: TrajectoryConfig{
			Endpoint:   "https://reviewer.example.com",
			Model:      "reviewer-model",
			APIKeyFile: keyPath,
		},
	}
	_, apiKey, _ := g.ResolveReviewer()
	if apiKey != "reviewer-file-key" {
		t.Errorf("got %q, want reviewer-file-key", apiKey)
	}
}

func TestResolveReviewer_KeyFromTrajectoryEnvWhenFileEmpty(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "empty-key.txt")
	if err := os.WriteFile(keyPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWER_TEST_ENV_KEY", "reviewer-env-key")
	g := &GovernorConfig{
		Trajectory: TrajectoryConfig{
			Endpoint:   "https://reviewer.example.com",
			Model:      "reviewer-model",
			APIKeyFile: keyPath,
			APIKeyEnv:  "REVIEWER_TEST_ENV_KEY",
		},
	}
	_, apiKey, _ := g.ResolveReviewer()
	if apiKey != "reviewer-env-key" {
		t.Errorf("got %q, want reviewer-env-key", apiKey)
	}
}

func TestResolveReviewer_KeyFallsBackToLiteLLMWhenTrajectoryUnset(t *testing.T) {
	g := &GovernorConfig{
		LiteLLM: LiteLLMConfig{
			Endpoint:  "https://litellm.example.com",
			APIKeyEnv: "LITELLM_FALLBACK_KEY_TEST",
		},
	}
	t.Setenv("LITELLM_FALLBACK_KEY_TEST", "litellm-fallback-key")
	_, apiKey, _ := g.ResolveReviewer()
	if apiKey != "litellm-fallback-key" {
		t.Errorf("got %q, want litellm-fallback-key", apiKey)
	}
}

// ---------------------------------------------------------------------------
// saveDashboardOverlay: failure branches when the overlay path is unwritable.
// ---------------------------------------------------------------------------

func TestSaveDashboardOverlay_OpenTempFileFails(t *testing.T) {
	origOverlay := DashboardOverlayFile
	// Point the overlay at a path inside a directory that does not exist,
	// so OpenFile(tmpPath) fails. saveDashboardOverlay must log and return
	// without panicking (Save() itself must still succeed).
	DashboardOverlayFile = "/nonexistent/deeply/nested/dir/hive.yaml.dashboard"
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	dir := t.TempDir()
	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "acme"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save should succeed even when the dashboard overlay write fails: %v", err)
	}
}

// TestWatcher_ReloadHandlesLoadFailure verifies reload logs and returns
// (without calling onChange) when LoadWithDashboardOverlay fails, e.g.
// because the watched file was deleted out from under it.
func TestWatcher_ReloadHandlesLoadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	// Never create the file at all, so Load() inside reload() fails.
	var called bool
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWatcher(path, func(cfg *Config) { called = true }, logger)

	w.reload()

	if called {
		t.Error("onChange should not be invoked when the config file cannot be loaded")
	}
}
