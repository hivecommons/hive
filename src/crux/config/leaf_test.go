package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func TestPromptAndDefinitionSource(t *testing.T) {
	var nilP *PromptSourceConfig
	if nilP.Slug() != "" || nilP.IsSet() {
		t.Fatal("nil prompt source must be unset")
	}
	p := &PromptSourceConfig{Owner: "o", Repo: "r"}
	if p.Slug() != "o/r" || p.IsSet() {
		t.Fatalf("partial prompt source: slug %q set %v", p.Slug(), p.IsSet())
	}
	p.Path = "x.md"
	if !p.IsSet() {
		t.Fatal("full prompt source must be set")
	}
	if (&PromptSourceConfig{Owner: "o"}).Slug() != "" {
		t.Fatal("slug without repo must be empty")
	}

	var nilD *DefinitionSourceConfig
	if nilD.Slug() != "" || nilD.IsSet() {
		t.Fatal("nil definition source must be unset")
	}
	d := &DefinitionSourceConfig{Owner: "o", Repo: "r"}
	if d.Slug() != "o/r" || d.IsSet() {
		t.Fatalf("partial definition source: slug %q set %v", d.Slug(), d.IsSet())
	}
	d.Path = "agent.yaml"
	if !d.IsSet() {
		t.Fatal("full definition source must be set")
	}
	if (&DefinitionSourceConfig{Repo: "r"}).Slug() != "" {
		t.Fatal("slug without owner must be empty")
	}
}

func TestChannelConfigIsEnabled(t *testing.T) {
	if !(&ChannelConfig{Type: ChannelTypeKick}).IsEnabled() {
		t.Fatal("nil Enabled defaults to true")
	}
	if (&ChannelConfig{Enabled: boolPtr(false)}).IsEnabled() {
		t.Fatal("explicit false must disable")
	}
}

func TestAgentConfigChannels(t *testing.T) {
	a := &AgentConfig{}
	if !a.UsesGovernorKick() {
		t.Fatal("no channels means implicit kick")
	}
	a.Channels = []ChannelConfig{{Type: "other"}}
	if a.UsesGovernorKick() || a.HasChannel(ChannelTypeKick) {
		t.Fatal("non-kick channel must suppress governor kick")
	}
	a.Channels = append(a.Channels, ChannelConfig{Type: ChannelTypeKick}, ChannelConfig{Type: ChannelTypeKick})
	if !a.UsesGovernorKick() || len(a.ChannelsOfType(ChannelTypeKick)) != 2 || len(a.ChannelsOfType("none")) != 0 {
		t.Fatal("explicit kick channel must be found")
	}
}

func TestAgentConfigJevAndOwners(t *testing.T) {
	a := AgentConfig{}
	if a.JevEnabled() {
		t.Fatal("jev defaults off")
	}
	a.JevMode = JevModeAssist
	if !a.JevEnabled() {
		t.Fatal("assist enables jev")
	}
	if a.ModelIsOperatorOwned() || a.BackendIsOperatorOwned() || a.PauseIsOperatorOwned() || a.OnDemandIsOperatorOwned() || a.ReposIsOperatorOwned() {
		t.Fatal("unset owners are not operator-owned")
	}
	a.ModelOwner, a.BackendOwner, a.PauseOwner, a.OnDemandOwner, a.ReposOwner = FieldOwnerOperator, FieldOwnerOperator, FieldOwnerOperator, FieldOwnerOperator, FieldOwnerOperator
	if !a.ModelIsOperatorOwned() || !a.BackendIsOperatorOwned() || !a.PauseIsOperatorOwned() || !a.OnDemandIsOperatorOwned() || !a.ReposIsOperatorOwned() {
		t.Fatal("operator owner must be recognised")
	}
	a.ModelOwner = FieldOwnerPack
	if a.ModelIsOperatorOwned() {
		t.Fatal("pack owner is not operator")
	}
	if a.BackendIsPackOwned() {
		t.Fatal("operator-owned backend is not pack-owned")
	}
	a.BackendOwner = FieldOwnerPack
	if !a.BackendIsPackOwned() || (AgentConfig{}).BackendIsPackOwned() {
		t.Fatal("only a pack-stamped backend is pack-owned")
	}
}

func TestAgentConfigRepos(t *testing.T) {
	if (AgentConfig{}).IsRepoScoped() || (AgentConfig{Repos: []string{" ", ""}}).IsRepoScoped() {
		t.Fatal("blank repos are not a scope")
	}
	if !(AgentConfig{Repos: []string{"", "org/repo"}}).IsRepoScoped() {
		t.Fatal("non-blank repo is a scope")
	}
	cases := map[[2]string]string{
		{"org", "repo"}:     "org/repo",
		{"org", "o2/repo"}:  "o2/repo",
		{"", "repo"}:        "repo",
		{"org", "  "}:       "",
		{" org ", " repo "}: "org/repo",
	}
	for in, want := range cases {
		if got := QualifyRepo(in[0], in[1]); got != want {
			t.Errorf("QualifyRepo(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
}

func TestReviewModelsConfig(t *testing.T) {
	var r ReviewModelsConfig
	if r.Configured() {
		t.Fatal("zero value is not configured")
	}
	if !r.ExcludeAuthorModelEnabled() {
		t.Fatal("exclude author model defaults on")
	}
	if r.EffectiveFallback() != ReviewModelsFallbackPinned {
		t.Fatal("fallback defaults to pinned")
	}
	r.Fallback = " skip "
	if r.EffectiveFallback() != ReviewModelsFallbackSkip || !r.Configured() {
		t.Fatal("skip fallback")
	}
	r.Fallback = "custom"
	if r.EffectiveFallback() != "custom" {
		t.Fatal("unknown fallback passes through")
	}
	r.Fallback = ""
	r.ExcludeAuthorModel = boolPtr(false)
	if r.ExcludeAuthorModelEnabled() || !r.Configured() {
		t.Fatal("explicit false disables exclusion")
	}
	r = ReviewModelsConfig{ExcludeAuthorFamily: true}
	if !r.Configured() {
		t.Fatal("family exclusion counts as configured")
	}
	pool := []ReviewModelPoolEntry{{Backend: "claude", Model: "opus"}}
	w := r.WithPool(pool)
	pool[0].Model = "changed"
	if w.Pool[0].Model != "opus" || !w.ExcludeAuthorFamily || !w.Configured() {
		t.Fatalf("WithPool must copy the pool and keep settings: %+v", w)
	}
}

func TestAgentConfigLoadState(t *testing.T) {
	var nilA *AgentConfig
	if nilA.SourceFile() != "" {
		t.Fatal("nil SourceFile")
	}
	a := &AgentConfig{ID: "id1"}
	if a.BaseName() != "id1" || a.Name() != "" {
		t.Fatal("base name falls back to ID")
	}
	a.SetLoadState("agent-a", "/data/agent-configs/a.yaml")
	if a.Name() != "agent-a" || a.SourceFile() != "/data/agent-configs/a.yaml" || a.BaseName() != "agent-a" {
		t.Fatalf("load state: %q %q %q", a.Name(), a.SourceFile(), a.BaseName())
	}
	a.ReplicaOf = "base"
	if !a.IsReplica() || a.BaseName() != "base" {
		t.Fatal("replica base name")
	}
	a.MarkExplicit(true, true)
	if !a.EnabledExplicitlySet() || !a.ClearOnKickExplicitlySet() {
		t.Fatal("MarkExplicit")
	}
	a.ClearLoadState()
	if a.Name() != "" || a.SourceFile() != "" || a.EnabledExplicitlySet() || a.ClearOnKickExplicitlySet() {
		t.Fatal("ClearLoadState must reset all bookkeeping")
	}
	a.MarkExplicit(false, false)
	if a.EnabledExplicitlySet() || a.ClearOnKickExplicitlySet() {
		t.Fatal("MarkExplicit(false,false) is a no-op")
	}
}

func TestAgentConfigUnmarshalYAMLTracksExplicitKeys(t *testing.T) {
	var a AgentConfig
	if err := yaml.Unmarshal([]byte("backend: claude\nenabled: false\nclear_on_kick: false\n"), &a); err != nil {
		t.Fatal(err)
	}
	if a.Backend != "claude" || !a.EnabledExplicitlySet() || !a.ClearOnKickExplicitlySet() {
		t.Fatalf("explicit keys not tracked: %+v", a)
	}
	var b AgentConfig
	if err := yaml.Unmarshal([]byte("backend: claude\n"), &b); err != nil {
		t.Fatal(err)
	}
	if b.EnabledExplicitlySet() || b.ClearOnKickExplicitlySet() {
		t.Fatal("absent keys must not be marked explicit")
	}
	var c AgentConfig
	if err := yaml.Unmarshal([]byte("backend: [not, a, string]\n"), &c); err == nil {
		t.Fatal("type mismatch must surface")
	}
	var m map[string]AgentConfig
	if err := yaml.Unmarshal([]byte("a: 1\n"), &m); err == nil {
		t.Fatal("scalar agent block must fail the key-presence decode")
	}
}

func TestAgentConfigCadenceAndContinuous(t *testing.T) {
	a := AgentConfig{}
	if a.CadenceScopeMode() != CadenceScopeAggregate || a.UsesRepoScopedCadence() {
		t.Fatal("default cadence scope is aggregate")
	}
	a.CadenceScope = CadenceScopePerRepo
	if !a.UsesRepoScopedCadence() {
		t.Fatal("per_repo opts in")
	}
	if a.EffectiveContinuousTurnCeiling() != DefaultContinuousTurnCeiling {
		t.Fatal("ceiling default")
	}
	a.ContinuousTurnCeiling = -1
	if a.EffectiveContinuousTurnCeiling() != 0 {
		t.Fatal("negative ceiling disables")
	}
	a.ContinuousTurnCeiling = time.Minute
	if a.EffectiveContinuousTurnCeiling() != time.Minute {
		t.Fatal("explicit ceiling")
	}
	if a.EffectiveContinuousCooldown() != DefaultContinuousCooldown || a.EffectiveContinuousBudgetPct() != DefaultContinuousBudgetPct {
		t.Fatal("cooldown/budget defaults")
	}
	a.ContinuousCooldown, a.ContinuousBudgetPct = 5*time.Second, 50
	if a.EffectiveContinuousCooldown() != 5*time.Second || a.EffectiveContinuousBudgetPct() != 50 {
		t.Fatal("cooldown/budget explicit")
	}
	if a.EffectiveBusyNoActivityThreshold() != DefaultBusyNoActivityThreshold {
		t.Fatal("busy threshold default")
	}
	a.BusyNoActivityThreshold = time.Hour
	if a.EffectiveBusyNoActivityThreshold() != time.Hour {
		t.Fatal("busy threshold explicit")
	}

	if a.ContinuousInMode("ACTIVE", fakeCadence(false)) {
		t.Fatal("not continuous without opt-in")
	}
	if !a.ContinuousInMode("ACTIVE", fakeCadence(true)) {
		t.Fatal("continuous cadence opts the mode in")
	}
	a.Continuous = true
	if !a.ContinuousInMode("active", fakeCadence(false)) || a.ContinuousInMode(" Quiet ", fakeCadence(false)) {
		t.Fatal("legacy continuous bool covers every non-quiet mode")
	}
}

type fakeCadence bool

func (f fakeCadence) IsContinuous() bool { return bool(f) }

func TestAgentConfigMiscGetters(t *testing.T) {
	a := &AgentConfig{}
	if !a.ShouldIncludeRepos() {
		t.Fatal("include repos defaults true")
	}
	a.IncludeRepos = boolPtr(false)
	if a.ShouldIncludeRepos() {
		t.Fatal("explicit false")
	}
	if a.GetBeadRole() != "worker" || a.GetSortOrder() != 100 {
		t.Fatal("bead role/sort defaults")
	}
	a.BeadRole = "supervisor"
	if a.GetBeadRole() != "supervisor" || a.GetSortOrder() != 0 {
		t.Fatal("supervisor sorts first")
	}
	a.SortOrder = 7
	if a.GetSortOrder() != 7 {
		t.Fatal("explicit sort order")
	}
}

func TestAgentConfigSandboxResolution(t *testing.T) {
	global := AgentSandboxConfig{
		Enabled: true, Image: "g-img", EnvAllowlist: []string{"PATH"}, NetworkMode: "none", TimeoutS: 30,
		Job: &SandboxJobConfig{WorkspaceClaim: "ws", WorkspaceClaimMount: "/ws", TTLSeconds: 60, ServiceAccount: "global-sa"},
	}
	var nilA *AgentConfig
	if nilA.SandboxEnabled(global) || nilA.SandboxImage(global) != "g-img" || nilA.SandboxNetworkMode(global) != "none" || nilA.SandboxTimeoutS(global) != 30 {
		t.Fatal("nil agent falls back to global")
	}
	if nilA.SandboxRuntime(global) != SandboxRuntimePodman {
		t.Fatal("runtime defaults to podman")
	}
	if got := nilA.SandboxJob(global); got.ServiceAccount != "global-sa" {
		t.Fatalf("nil agent job = %+v", got)
	}
	if got := nilA.SandboxJob(AgentSandboxConfig{}); got.WorkspaceClaim != "" {
		t.Fatal("no global job yields zero value")
	}
	global.Runtime = " k8s "
	if nilA.SandboxRuntime(global) != "k8s" {
		t.Fatal("global runtime trimmed")
	}
	a := &AgentConfig{Sandbox: &AgentSandboxOverride{
		Enabled: boolPtr(true), Image: "a-img", EnvAllowlist: []string{"HOME"}, NetworkMode: "bridge", TimeoutS: 5, Runtime: "podman",
		Job: &SandboxJobConfig{ServiceAccount: "agent-sa"},
	}}
	if !a.SandboxEnabled(global) || a.SandboxImage(global) != "a-img" || a.SandboxNetworkMode(global) != "bridge" || a.SandboxTimeoutS(global) != 5 || a.SandboxRuntime(global) != "podman" {
		t.Fatal("per-agent overrides win")
	}
	if got := a.SandboxEnvAllowlist(global); !reflect.DeepEqual(got, []string{"HOME"}) {
		t.Fatalf("env allowlist = %v", got)
	}
	job := a.SandboxJob(global)
	if job.ServiceAccount != "agent-sa" || job.WorkspaceClaim != "ws" || job.WorkspaceClaimMount != "/ws" || job.TTLSeconds != 60 {
		t.Fatalf("job merge = %+v", job)
	}
	global.Enabled = false
	if a.SandboxEnabled(global) {
		t.Fatal("global gate off disables")
	}
	if (&AgentConfig{Sandbox: &AgentSandboxOverride{}}).SandboxEnabled(AgentSandboxConfig{Enabled: true}) {
		t.Fatal("nil per-agent Enabled is off")
	}
}

func TestReviewHeads(t *testing.T) {
	cases := []struct {
		h    ReviewHead
		want string
	}{
		{ReviewHead{Alias: "My Alias!"}, "my-alias"},
		{ReviewHead{Backend: "claude", Model: "opus"}, "claude-opus"},
		{ReviewHead{Backend: "claude", Model: "claude-opus"}, "claude-opus"},
		{ReviewHead{Backend: "codex", Model: "codex"}, "codex"},
		{ReviewHead{Model: "opus"}, "opus"},
		{ReviewHead{Backend: "codex"}, "codex"},
		{ReviewHead{Backend: "--", Model: "__"}, ""},
	}
	for _, c := range cases {
		if got := c.h.EffectiveAlias(); got != c.want {
			t.Errorf("%+v alias = %q want %q", c.h, got, c.want)
		}
	}
	a := AgentConfig{ReviewHeads: []ReviewHead{{Backend: "claude", Model: "opus"}, {Alias: "x"}}}
	if a.IsReviewHead() {
		t.Fatal("base is not a head")
	}
	if got := a.ReviewHeadKeys("rev"); !reflect.DeepEqual(got, []string{"rev-claude-opus", "rev-x"}) {
		t.Fatalf("keys = %v", got)
	}
	if !(AgentConfig{ReviewHead: "rev"}).IsReviewHead() {
		t.Fatal("derived head")
	}
}

func TestBackendsConfig(t *testing.T) {
	var b BackendsConfig
	if !b.Allowed("claude") || !b.Allowed("") || b.Fallback() != "" {
		t.Fatal("empty lists allow everything")
	}
	b = BackendsConfig{Allow: []string{" ", "Claude", "codex"}, Deny: []string{"codex"}}
	if !b.Allowed("claude") || b.Allowed("codex") || b.Allowed("goose") {
		t.Fatal("allow/deny evaluation")
	}
	if b.Fallback() != "Claude" {
		t.Fatalf("fallback = %q", b.Fallback())
	}
	if (BackendsConfig{Allow: []string{"codex"}, Deny: []string{"codex"}}).Fallback() != "" {
		t.Fatal("denied allow entry is no fallback")
	}
}

func TestKickLimits(t *testing.T) {
	var k KickLimitsConfig
	if k.IssuesPerKick() != DefaultMaxIssuesPerKick || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Fatal("defaults")
	}
	k = KickLimitsConfig{MaxIssues: intPtr(0), MaxPRs: intPtr(-3)}
	if k.IssuesPerKick() != KickListUnlimited || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Fatal("zero is unlimited, negative is default")
	}
	k = KickLimitsConfig{MaxIssues: intPtr(MaxKickListCap + 1), MaxPRs: intPtr(7)}
	if k.IssuesPerKick() != MaxKickListCap || k.PRsPerKick() != 7 {
		t.Fatal("clamp and passthrough")
	}
}

func TestKickLogSource(t *testing.T) {
	if NormalizeKickLogSource(" Transcript ") != KickLogSourceTranscript || NormalizeKickLogSource("bogus") != KickLogSourceScrollback {
		t.Fatal("normalize")
	}
	if !ValidKickLogSource(KickLogSourceAuto) || ValidKickLogSource("bogus") {
		t.Fatal("valid")
	}
	if got := KickLogSources(); !reflect.DeepEqual(got, []string{KickLogSourceScrollback, KickLogSourceTranscript, KickLogSourceAuto}) {
		t.Fatalf("sources = %v", got)
	}
	t.Setenv(KickLogSourceEnvVar, "nonsense")
	if KickLogSourceEnvOverride() != "" {
		t.Fatal("invalid env is no override")
	}
	t.Setenv(KickLogSourceEnvVar, "auto")
	if KickLogSourceEnvOverride() != KickLogSourceAuto {
		t.Fatal("valid env overrides")
	}
}

func TestLiteLLMURLHelpers(t *testing.T) {
	if LocalLiteLLMProxyURL() != "http://127.0.0.1:18445" {
		t.Fatal(LocalLiteLLMProxyURL())
	}
	if LiteLLMChatCompletionsURL(" https://x.example/v1/ ") != "https://x.example/v1/chat/completions" {
		t.Fatal("v1 suffix")
	}
	if LiteLLMChatCompletionsURL("https://x.example") != "https://x.example/v1/chat/completions" {
		t.Fatal("bare base")
	}
}

func TestProxyInjectGHAuth(t *testing.T) {
	t.Setenv(ProxyInjectGHAuthEnv, "")
	if ProxyInjectGHAuth() {
		t.Fatal("unset is off")
	}
	t.Setenv(ProxyInjectGHAuthEnv, " true ")
	if !ProxyInjectGHAuth() {
		t.Fatal("true is on")
	}
}

func TestTriStateToggles(t *testing.T) {
	if !(ProvidersConfig{}).StatusEnabled() || (ProvidersConfig{Status: ProviderStatusConfig{Enabled: boolPtr(false)}}).StatusEnabled() {
		t.Fatal("provider status")
	}
	if !(WatchdogConfig{}).WatchdogEnabled() || (WatchdogConfig{Enabled: boolPtr(false)}).WatchdogEnabled() {
		t.Fatal("watchdog enabled")
	}
	if !(WatchdogConfig{}).AuthProbeEnabled() || (WatchdogConfig{AuthProbe: boolPtr(false)}).AuthProbeEnabled() {
		t.Fatal("auth probe")
	}
	if !(TrajectoryConfig{}).IsEnabled() || (TrajectoryConfig{Enabled: boolPtr(false)}).IsEnabled() {
		t.Fatal("trajectory")
	}
	if (ProviderBudgetConfig{}).EffectiveProbeInterval() != DefaultProviderBudgetProbeIntervalS*time.Second {
		t.Fatal("probe default")
	}
	if (ProviderBudgetConfig{ProbeIntervalS: 5}).EffectiveProbeInterval() != 5*time.Second {
		t.Fatal("probe explicit")
	}
}

func TestBackendLists(t *testing.T) {
	if !IsCLIBackend("claude") || IsCLIBackend("vllm") {
		t.Fatal("cli list")
	}
	if !IsInferenceBackend("watsonx") || IsInferenceBackend("claude") {
		t.Fatal("inference list")
	}
	all := SupportedBackends()
	if len(all) != len(CLIBackends)+len(InferenceBackends) || all[0] != CLIBackends[0] || all[len(all)-1] != InferenceBackends[len(InferenceBackends)-1] {
		t.Fatalf("supported = %v", all)
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		text, pattern string
		want          bool
	}{
		{"Bash(git push)", "/^bash\\(git/", true},
		{"Bash(git push)", "/[/", false},
		{"Bash(git push)", "bash*push)", true},
		{"Bash(git push)", "*push*", true},
		{"Bash(git push)", "bash*pull", false},
		{"Bash(git push)", "git*", false},
		{"Bash(git push)", "*git", false},
		{"Bash(git push)", "git push", true},
		{"Bash(git push)", "rm -rf", false},
	}
	for _, c := range cases {
		if got := WildcardMatch(c.text, c.pattern); got != c.want {
			t.Errorf("WildcardMatch(%q,%q)=%v want %v", c.text, c.pattern, got, c.want)
		}
	}
}

func TestAPIKeySHA256(t *testing.T) {
	if APIKeySHA256("") != "" {
		t.Fatal("empty key has no digest")
	}
	if got := APIKeySHA256("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
}

func TestAllowSecretFileRoot(t *testing.T) {
	tmp := t.TempDir()
	defer SetSecretFileRootsForTest(filepath.Join(tmp, "a"))()
	p := filepath.Join(tmp, "extra", "key")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if SecretFilePathAllowed(p) {
		t.Fatal("extra dir not yet allowed")
	}
	AllowSecretFileRoot("  ")()
	restore := AllowSecretFileRoot(filepath.Join(tmp, "extra"))
	if !SecretFilePathAllowed(p) {
		t.Fatal("registered root must allow")
	}
	if SecretFilePathAllowed(filepath.Join(tmp, "extra")) {
		t.Fatal("the root itself is not a key file")
	}
	if SecretFilePathAllowed("relative/key") || SecretFilePathAllowed("") {
		t.Fatal("relative/empty paths refused")
	}
	restore()
	if SecretFilePathAllowed(p) {
		t.Fatal("restore must remove the root")
	}
}

func TestGatewayAndInferenceAuthResolveAPIKey(t *testing.T) {
	tmp := t.TempDir()
	defer SetSecretFileRootsForTest(tmp)()
	keyFile := filepath.Join(tmp, "key")
	if err := os.WriteFile(keyFile, []byte(" from-file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_GW_KEY", "from-env")
	t.Setenv("HIVE_TEST_GW_EMPTY", "")
	t.Setenv("HIVE_TEST_DEFAULT_KEY", "from-default")

	gw := GatewayConfig{APIKeyEnv: "HIVE_TEST_GW_KEY", APIKeyFile: keyFile}
	if gw.ResolveAPIKey() != "from-env" {
		t.Fatal("env wins")
	}
	gw.APIKeyEnv = "HIVE_TEST_GW_EMPTY"
	if gw.ResolveAPIKey() != "from-file" {
		t.Fatal("file after empty env")
	}
	gw.APIKeyFile = filepath.Join(tmp, "missing")
	if gw.ResolveAPIKey() != "" {
		t.Fatal("missing file is empty")
	}

	ia := &InferenceAuthConfig{APIKeyFile: keyFile, APIKeyEnv: "HIVE_TEST_GW_KEY"}
	if ia.ResolveAPIKey("HIVE_TEST_DEFAULT_KEY") != "from-file" {
		t.Fatal("inference file first")
	}
	ia.APIKeyFile = filepath.Join(tmp, "missing")
	if ia.ResolveAPIKey("") != "from-env" {
		t.Fatal("inference env second")
	}
	ia.APIKeyEnv = "HIVE_TEST_GW_EMPTY"
	if ia.ResolveAPIKey("HIVE_TEST_DEFAULT_KEY") != "from-default" {
		t.Fatal("inference default env third")
	}
	if ia.ResolveAPIKey("") != "" {
		t.Fatal("nothing configured")
	}
}

func TestLiteLLMConfigResolution(t *testing.T) {
	tmp := t.TempDir()
	defer SetSecretFileRootsForTest(tmp)()
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")
	t.Setenv(LiteLLMEndpointEnv, "")
	t.Setenv("HIVE_TEST_LL_KEY", "")

	c := &LiteLLMConfig{Endpoint: "https://ll.example", APIKeyEnv: "HIVE_TEST_LL_KEY", APIKeyFile: filepath.Join(tmp, "missing")}
	if c.ResolveAPIKey() != "" || c.ResolveAPIKeySource() != "" {
		t.Fatal("no key anywhere")
	}
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "default-env")
	if c.ResolveAPIKey() != "default-env" || c.ResolveAPIKeySource() != "env:"+DefaultLiteLLMAPIKeyEnv {
		t.Fatal("default env")
	}
	t.Setenv("HIVE_TEST_LL_KEY", "named-env")
	if c.ResolveAPIKeySource() != "env:HIVE_TEST_LL_KEY" {
		t.Fatal("named env beats default")
	}
	keyFile := filepath.Join(tmp, "key")
	if err := os.WriteFile(keyFile, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.APIKeyFile = keyFile
	if c.ResolveAPIKey() != "file-key" || c.ResolveAPIKeySource() != "file:"+keyFile {
		t.Fatal("file beats env")
	}
	c.APIKeyFile = "/etc/passwd"
	if strings.HasPrefix(c.ResolveAPIKeySource(), "file:") {
		t.Fatal("out-of-root file must be refused")
	}

	if c.ResolveEndpoint() != "https://ll.example" {
		t.Fatal("yaml endpoint")
	}
	t.Setenv(LiteLLMEndpointEnv, "https://env.example")
	if c.ResolveEndpoint() != "https://env.example" {
		t.Fatal("env endpoint wins")
	}

	if err := (&LiteLLMConfig{}).Validate(); err != nil {
		t.Fatal("empty endpoint is valid")
	}
	if err := (&LiteLLMConfig{Endpoint: "https://ok.example/v1"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"://bad", "ftp://x.example", "https://"} {
		if err := (&LiteLLMConfig{Endpoint: bad}).Validate(); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
