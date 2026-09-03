package defsrc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

type stubFetcher struct {
	mu      sync.Mutex
	body    string
	err     error
	calls   int
	lastRef string
}

func (f *stubFetcher) GetFileContentRef(_ context.Context, _, _, _, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastRef = ref
	return f.body, f.err
}

func (f *stubFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func allowAll(string) bool  { return true }
func allowNone(string) bool { return false }

func fullSource() Source {
	return Source{Owner: "hivecommons", Repo: "hive", Path: "agents/scanner.yaml", Ref: "v2"}
}

// validDef is a minimal well-formed AgentDefinition YAML that changes several
// operator-safe fields.
const validDef = `
apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: scanner
  displayName: Live Scanner
  emoji: "🔎"
  description: sourced live
spec:
  backend: claude
  model: opus
  role: scanner
  mode: ISSUES_ONLY
  laneKeywords: [bug, triage]
  channels:
    - type: kick
`

// bakedAgent returns a config with sensitive/seed-only fields set that a live
// definition must NEVER be able to change.
func bakedAgent() config.AgentConfig {
	return config.AgentConfig{
		DisplayName:      "Baked Scanner",
		Enabled:          true,
		Paused:           true,
		CLIPinned:        true,
		ID:               "scanner",
		BeadsDir:         "/data/beads/scanner",
		MetricsCollector: "trusted-collector",
		ACMMLevels:       []int{5},
		PromptSource:     &config.PromptSourceConfig{Owner: "hivecommons", Repo: "hive", Path: "p.md"},
		DefinitionSource: &config.DefinitionSourceConfig{Owner: "hivecommons", Repo: "hive", Path: "agents/scanner.yaml"},
	}
}

// TestResolve_MergesAllowedFieldsOnly is the trust-boundary test: a live
// definition sets presentation/behavior fields but CANNOT touch security /
// seed-only / privileged fields.
func TestResolve_MergesAllowedFieldsOnly(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)

	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !res.Ok {
		t.Fatalf("expected ok merge, got %+v", res)
	}
	m := res.Merged

	// ALLOWED fields were applied.
	if m.DisplayName != "Live Scanner" {
		t.Errorf("displayName = %q, want Live Scanner", m.DisplayName)
	}
	if m.Emoji != "🔎" {
		t.Errorf("emoji not applied: %q", m.Emoji)
	}
	if m.Backend != "claude" || m.Model != "opus" {
		t.Errorf("backend/model not applied: %q/%q", m.Backend, m.Model)
	}
	if m.Mode != "ISSUES_ONLY" {
		t.Errorf("mode not applied: %q", m.Mode)
	}
	if len(m.LaneKeywords) != 2 || len(m.Channels) != 1 {
		t.Errorf("laneKeywords/channels not applied: %+v / %+v", m.LaneKeywords, m.Channels)
	}

	// FORBIDDEN fields were preserved from the baked config (a live definition
	// must not be able to un-pause, un-pin, re-point sources, or change identity).
	if !m.Paused {
		t.Error("live definition must NOT be able to un-pause the agent")
	}
	if !m.CLIPinned {
		t.Error("live definition must NOT be able to change cli_pinned")
	}
	if m.ID != "scanner" || m.BeadsDir != "/data/beads/scanner" {
		t.Error("live definition must NOT be able to change identity/beads dir")
	}
	if m.MetricsCollector != "trusted-collector" {
		t.Error("live definition must NOT be able to change metrics_collector")
	}
	if len(m.ACMMLevels) != 1 || m.ACMMLevels[0] != 5 {
		t.Error("live definition must NOT be able to change acmm_levels")
	}
	if m.PromptSource == nil || m.PromptSource.Owner != "hivecommons" {
		t.Error("live definition must NOT be able to re-point prompt_source")
	}
	if m.DefinitionSource == nil || m.DefinitionSource.Owner != "hivecommons" {
		t.Error("live definition must NOT be able to re-point definition_source")
	}
}

// TestResolve_FallbackToCacheOnError: after one success, a later fetch failure
// serves the last-known-good document instead of blanking the agent.
func TestResolve_FallbackToCacheOnError(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)

	first := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !first.Ok || first.Merged.DisplayName != "Live Scanner" {
		t.Fatalf("first resolve should succeed, got %+v", first)
	}

	f.mu.Lock()
	f.body = ""
	f.err = errors.New("repo unreachable")
	f.mu.Unlock()

	second := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !second.Ok || second.Merged.DisplayName != "Live Scanner" {
		t.Fatalf("failure should fall back to cached definition, got %+v", second)
	}
	if !strings.HasPrefix(second.Source, "github-cache:") {
		t.Errorf("provenance = %q, want github-cache:*", second.Source)
	}
}

// TestResolve_ErrorNoCache_KeepsBaked: a cold failure keeps the baked config.
func TestResolve_ErrorNoCache_KeepsBaked(t *testing.T) {
	f := &stubFetcher{err: errors.New("boom")}
	r := NewResolver(f, allowAll, nil)
	baked := bakedAgent()
	res := r.Resolve(context.Background(), fullSource(), baked)
	if res.Ok {
		t.Errorf("cold failure should not be ok, got %+v", res)
	}
	if res.Merged.DisplayName != baked.DisplayName {
		t.Error("cold failure must keep baked definition unchanged")
	}
}

// TestResolve_MalformedKeepsBaked: a document that isn't a valid AgentDefinition
// keeps the baked config rather than corrupting the agent.
func TestResolve_MalformedKeepsBaked(t *testing.T) {
	f := &stubFetcher{body: "kind: NotAnAgent\nmetadata: {}"}
	r := NewResolver(f, allowAll, nil)
	baked := bakedAgent()
	res := r.Resolve(context.Background(), fullSource(), baked)
	if res.Ok {
		t.Errorf("malformed doc should not be ok, got %+v", res)
	}
	if res.Merged.DisplayName != baked.DisplayName {
		t.Error("malformed doc must keep baked definition")
	}
}

// TestResolve_DeniedByAllowlist: a non-allowlisted repo is never fetched.
func TestResolve_DeniedByAllowlist(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowNone, nil)
	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if res.Ok {
		t.Errorf("denied source should not be ok, got %+v", res)
	}
	if f.callCount() != 0 {
		t.Errorf("denied source must not fetch, calls=%d", f.callCount())
	}
	if res.Source != "denied" {
		t.Errorf("source = %q, want denied", res.Source)
	}
}

// TestResolve_NilAllowDeniesAll: a nil AllowFunc fails closed.
func TestResolve_NilAllowDeniesAll(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, nil, nil)
	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if res.Ok || f.callCount() != 0 {
		t.Errorf("nil allow must deny all, got ok=%v calls=%d", res.Ok, f.callCount())
	}
}

// TestResolve_UnsetSourceKeepsBaked.
func TestResolve_UnsetSourceKeepsBaked(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), Source{Owner: "o", Repo: "r"}, bakedAgent())
	if res.Ok || f.callCount() != 0 {
		t.Errorf("incomplete source should be a no-fetch miss, got ok=%v calls=%d", res.Ok, f.callCount())
	}
}

// TestFetchOnce_Gating enforces the same allowlist gate and surfaces errors.
func TestFetchOnce_Gating(t *testing.T) {
	f := &stubFetcher{body: validDef}

	if _, err := FetchOnce(context.Background(), f, allowNone, fullSource()); err == nil {
		t.Error("denied repo should error")
	}
	if f.callCount() != 0 {
		t.Errorf("denied FetchOnce must not fetch, calls=%d", f.callCount())
	}
	if _, err := FetchOnce(context.Background(), f, allowAll, Source{Owner: "o"}); err == nil {
		t.Error("incomplete source should error")
	}
	if _, err := FetchOnce(context.Background(), nil, allowAll, fullSource()); err == nil {
		t.Error("nil fetcher should error")
	}
	def, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err != nil || def == nil || def.Metadata.Name != "scanner" {
		t.Errorf("valid FetchOnce failed: def=%+v err=%v", def, err)
	}
}

// TestApplyToConfig_OnlyLinkedAgents: agents without a definition_source are
// untouched; linked agents get merged and keep their source pointers.
func TestApplyToConfig_OnlyLinkedAgents(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)

	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {
			DisplayName:      "Baked Scanner",
			Paused:           true,
			DefinitionSource: &config.DefinitionSourceConfig{Owner: "hivecommons", Repo: "hive", Path: "agents/scanner.yaml", Ref: "v2"},
		},
		"reviewer": {DisplayName: "Reviewer", Paused: true}, // no definition_source
	}}

	ApplyToConfig(context.Background(), cfg, r, nil)

	sc := cfg.Agents["scanner"]
	if sc.DisplayName != "Live Scanner" {
		t.Errorf("linked agent not merged: %q", sc.DisplayName)
	}
	if !sc.Paused {
		t.Error("merge must preserve paused")
	}
	if sc.DefinitionSource == nil {
		t.Error("merge must preserve definition_source pointer")
	}
	if f.lastRef != "v2" {
		t.Errorf("ref not threaded: %q", f.lastRef)
	}

	rv := cfg.Agents["reviewer"]
	if rv.DisplayName != "Reviewer" {
		t.Error("unlinked agent must be left untouched")
	}
	if f.callCount() != 1 {
		t.Errorf("only the linked agent should be fetched, calls=%d", f.callCount())
	}
}

// TestApplyToConfig_NilResolverNoop.
func TestApplyToConfig_NilResolverNoop(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {DisplayName: "Baked", DefinitionSource: &config.DefinitionSourceConfig{Owner: "o", Repo: "r", Path: "a.yaml"}},
	}}
	ApplyToConfig(context.Background(), cfg, nil, nil) // must not panic
	if cfg.Agents["scanner"].DisplayName != "Baked" {
		t.Error("nil resolver must be a no-op")
	}
}

// TestParseDefinition_Validation.
func TestParseDefinition_Validation(t *testing.T) {
	if _, err := ParseDefinition("kind: Wrong\nmetadata:\n  name: x"); err == nil {
		t.Error("wrong kind should error")
	}
	if _, err := ParseDefinition("kind: AgentDefinition\nmetadata: {}"); err == nil {
		t.Error("missing name should error")
	}
	if _, err := ParseDefinition("::not yaml::"); err == nil {
		t.Error("malformed yaml should error")
	}
}
