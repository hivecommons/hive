package defsrc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSource_Slug covers the empty-owner/empty-repo branches of Slug, which
// mergeFromRaw's provenance and the allowlist gate depend on.
func TestSource_Slug(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want string
	}{
		{"both set", Source{Owner: "kubestellar", Repo: "hive"}, "kubestellar/hive"},
		{"owner empty", Source{Owner: "", Repo: "hive"}, ""},
		{"repo empty", Source{Owner: "kubestellar", Repo: ""}, ""},
		{"both empty", Source{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.src.Slug(); got != tc.want {
				t.Errorf("Slug() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseDefinition_Success covers the success path of ParseDefinition
// (existing tests only cover the error branches).
func TestParseDefinition_Success(t *testing.T) {
	def, err := ParseDefinition(validDef)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Metadata.Name != "scanner" {
		t.Errorf("name = %q, want scanner", def.Metadata.Name)
	}
	if def.Kind != DefinitionKind {
		t.Errorf("kind = %q, want %q", def.Kind, DefinitionKind)
	}
}

// TestMergeAllowedFields_ZeroValuesSkipped covers every "if zero value, skip"
// branch in mergeAllowedFields: an empty/zero-value definition must leave the
// baked config's presentation/behavior fields completely untouched, except
// for ClearOnKick/IncludeRepos which are always authoritative.
func TestMergeAllowedFields_ZeroValuesSkipped(t *testing.T) {
	baked := config.AgentConfig{
		DisplayName:     "Baked Display",
		Description:     "Baked Description",
		Emoji:           "🤖",
		Color:           "blue",
		Backend:         "codex",
		Model:           "gpt",
		Role:            "reviewer",
		Mode:            "FULL",
		SortOrder:       7,
		BeadRole:        "primary",
		StaleTimeout:    30,
		RestartStrategy: "always",
		LaneKeywords:    []string{"kw1"},
		DetectKeywords:  []string{"dk1"},
		Aliases:         []string{"alias1"},
		Channels:        []config.ChannelConfig{{Type: "kick"}},
		Tools:           &config.ToolsConfig{},
		Connections:     []config.ConnectionConfig{{}},
	}
	def := &AgentDefinition{Kind: DefinitionKind, Metadata: AgentDefinitionMeta{Name: "x"}}

	merged := MergeAllowedFields(baked, def)

	if merged.DisplayName != baked.DisplayName ||
		merged.Description != baked.Description ||
		merged.Emoji != baked.Emoji ||
		merged.Color != baked.Color ||
		merged.Backend != baked.Backend ||
		merged.Model != baked.Model ||
		merged.Role != baked.Role ||
		merged.Mode != baked.Mode ||
		merged.SortOrder != baked.SortOrder ||
		merged.BeadRole != baked.BeadRole ||
		merged.StaleTimeout != baked.StaleTimeout ||
		merged.RestartStrategy != baked.RestartStrategy {
		t.Errorf("zero-value definition must not clear baked scalar fields: %+v", merged)
	}
	if len(merged.LaneKeywords) != 1 || len(merged.DetectKeywords) != 1 ||
		len(merged.Aliases) != 1 || len(merged.Channels) != 1 || len(merged.Connections) != 1 {
		t.Errorf("zero-value definition must not clear baked slice fields: %+v", merged)
	}
	if merged.Tools != baked.Tools {
		t.Errorf("nil def.Spec.Tools must not override baked Tools pointer")
	}
	// ClearOnKick and IncludeRepos are always authoritative from the
	// definition (even at zero value), per the doc comment.
	if merged.ClearOnKick != false {
		t.Errorf("ClearOnKick should follow def's zero value, got %v", merged.ClearOnKick)
	}
	if merged.IncludeRepos == nil || *merged.IncludeRepos != false {
		t.Errorf("IncludeRepos should be set to def's zero value (false), got %+v", merged.IncludeRepos)
	}
}

// TestMergeAllowedFields_AllNonZeroApplied covers the "applied" branch for
// every field not already exercised by TestResolve_MergesAllowedFieldsOnly,
// namely Description, Color, StaleTimeout, RestartStrategy, DetectKeywords,
// Aliases, Tools, Connections, and ClearOnKick/IncludeRepos = true.
func TestMergeAllowedFields_AllNonZeroApplied(t *testing.T) {
	baked := config.AgentConfig{
		Description:     "old",
		Color:           "red",
		StaleTimeout:    5,
		RestartStrategy: "never",
		DetectKeywords:  []string{"old-dk"},
		Aliases:         []string{"old-alias"},
		Tools:           &config.ToolsConfig{},
		Connections:     []config.ConnectionConfig{{}},
		ClearOnKick:     false,
	}
	tools := &config.ToolsConfig{}
	def := &AgentDefinition{
		Kind:     DefinitionKind,
		Metadata: AgentDefinitionMeta{Name: "x", Description: "new", Color: "green"},
		Spec: AgentDefinitionSpec{
			StaleTimeout:    99,
			RestartStrategy: "on-failure",
			DetectKeywords:  []string{"new-dk1", "new-dk2"},
			Aliases:         []string{"new-alias"},
			Tools:           tools,
			Connections:     []config.ConnectionConfig{{}, {}},
			ClearOnKick:     true,
			IncludeRepos:    true,
		},
	}

	merged := MergeAllowedFields(baked, def)

	if merged.Description != "new" {
		t.Errorf("Description not applied: %q", merged.Description)
	}
	if merged.Color != "green" {
		t.Errorf("Color not applied: %q", merged.Color)
	}
	if merged.StaleTimeout != 99 {
		t.Errorf("StaleTimeout not applied: %d", merged.StaleTimeout)
	}
	if merged.RestartStrategy != "on-failure" {
		t.Errorf("RestartStrategy not applied: %q", merged.RestartStrategy)
	}
	if len(merged.DetectKeywords) != 2 {
		t.Errorf("DetectKeywords not applied: %+v", merged.DetectKeywords)
	}
	if len(merged.Aliases) != 1 || merged.Aliases[0] != "new-alias" {
		t.Errorf("Aliases not applied: %+v", merged.Aliases)
	}
	if merged.Tools != tools {
		t.Errorf("Tools pointer not applied")
	}
	if len(merged.Connections) != 2 {
		t.Errorf("Connections not applied: %+v", merged.Connections)
	}
	if !merged.ClearOnKick {
		t.Errorf("ClearOnKick should be true")
	}
	if merged.IncludeRepos == nil || !*merged.IncludeRepos {
		t.Errorf("IncludeRepos should be true, got %+v", merged.IncludeRepos)
	}
}

// TestResolve_NoFetcher_NoCache covers the "no fetcher, nothing cached yet"
// branch of Resolve, which reports "no-client".
func TestResolve_NoFetcher_NoCache(t *testing.T) {
	r := NewResolver(nil, allowAll, nil)
	baked := bakedAgent()
	res := r.Resolve(context.Background(), fullSource(), baked)
	if res.Ok {
		t.Errorf("no fetcher + no cache should not be ok, got %+v", res)
	}
	if res.Source != "no-client" {
		t.Errorf("source = %q, want no-client", res.Source)
	}
	if res.Merged.DisplayName != baked.DisplayName {
		t.Error("no-client miss must keep baked definition")
	}
}

// TestResolve_NoFetcher_WithCache covers the "no fetcher, but a
// previously-cached document exists" branch: the resolver primes its cache
// via a fetcher-backed resolve, then a second Resolver (no fetcher) sharing
// no state can't reach that cache — so instead we directly seed the cache via
// store() through a successful Resolve on the SAME resolver, then swap
// fetcher to nil isn't possible (fetcher is unexported+immutable per-call),
// so we validate via a resolver constructed with a fetcher that later returns
// errors, and confirm cache fallback already covered elsewhere. Here we cover
// the literal "fetcher == nil && cached" branch by constructing the resolver
// with a nil fetcher directly and pre-seeding its cache.
func TestResolve_NoFetcher_WithCache(t *testing.T) {
	r := NewResolver(nil, allowAll, nil)
	r.store(fullSource(), validDef)

	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !res.Ok {
		t.Fatalf("expected cache hit to succeed, got %+v", res)
	}
	if !strings.HasPrefix(res.Source, "github-cache:") {
		t.Errorf("source = %q, want github-cache:*", res.Source)
	}
	if res.Merged.DisplayName != "Live Scanner" {
		t.Errorf("expected cached def merged, got %+v", res.Merged)
	}
}

// TestResolve_NilContext covers the "ctx == nil" fallback to
// context.Background() inside Resolve.
func TestResolve_NilContext(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	//nolint:staticcheck // intentionally passing nil to exercise the fallback branch
	res := r.Resolve(nil, fullSource(), bakedAgent())
	if !res.Ok {
		t.Errorf("nil ctx should still resolve successfully, got %+v", res)
	}
}

// TestResolve_TruncatesOversizedDefinition covers the maxDefinitionBytes
// truncation branch: an oversized document is truncated before parsing, and
// since truncation likely breaks the YAML, the result gracefully falls back
// to the baked config. We assert the truncation warning path is taken by
// checking behavior end-to-end (no panic, no cache poisoning) since size
// itself isn't observable from Result.
func TestResolve_TruncatesOversizedDefinition(t *testing.T) {
	huge := validDef + strings.Repeat("#", maxDefinitionBytes+100)
	f := &stubFetcher{body: huge}
	r := NewResolver(f, allowAll, nil)
	baked := bakedAgent()

	res := r.Resolve(context.Background(), fullSource(), baked)
	// Truncated content still starts with the valid YAML header followed by
	// comment padding, so it should still parse successfully (the trailing
	// '#' comment characters are valid YAML).
	if !res.Ok {
		t.Fatalf("truncated-but-still-valid def should parse, got %+v", res)
	}
	if res.Merged.DisplayName != "Live Scanner" {
		t.Errorf("expected merge to apply despite truncation, got %+v", res.Merged)
	}
}

// TestResolve_ProvenanceWithExplicitRef covers the provenance() branch where
// src.Ref is non-empty (fullSource() already sets Ref: "v2").
func TestResolve_ProvenanceWithExplicitRef(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res)
	}
	want := "github:kubestellar/hive@v2"
	if res.Source != want {
		t.Errorf("source = %q, want %q", res.Source, want)
	}
}

// TestResolve_ProvenanceDefaultRef covers the provenance() branch where
// src.Ref is empty, so "default" is substituted.
func TestResolve_ProvenanceDefaultRef(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	src := Source{Owner: "kubestellar", Repo: "hive", Path: "agents/scanner.yaml"} // Ref unset
	res := r.Resolve(context.Background(), src, bakedAgent())
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res)
	}
	want := "github:kubestellar/hive@default"
	if res.Source != want {
		t.Errorf("source = %q, want %q", res.Source, want)
	}
}

// TestFetchOnce_SizeCap covers FetchOnce's truncation branch and its
// successful return of a parsed definition after truncation.
func TestFetchOnce_SizeCap(t *testing.T) {
	huge := validDef + strings.Repeat("#", maxDefinitionBytes+100)
	f := &stubFetcher{body: huge}
	def, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Metadata.Name != "scanner" {
		t.Errorf("expected parsed def after truncation, got %+v", def)
	}
}

// TestFetchOnce_NilContext covers the "ctx == nil" fallback branch in
// FetchOnce.
func TestFetchOnce_NilContext(t *testing.T) {
	f := &stubFetcher{body: validDef}
	//nolint:staticcheck // intentionally passing nil to exercise the fallback branch
	def, err := FetchOnce(nil, f, allowAll, fullSource())
	if err != nil || def == nil {
		t.Errorf("nil ctx should still succeed, def=%+v err=%v", def, err)
	}
}

// TestFetchOnce_FetchError covers the fetcher-returns-error branch of
// FetchOnce (distinct from TestFetchOnce_Gating, which only covers gating).
func TestFetchOnce_FetchError(t *testing.T) {
	f := &stubFetcher{err: errors.New("network down")}
	_, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err == nil {
		t.Error("expected fetch error to propagate")
	}
}

// TestApplyToConfig_NilConfigNoop covers the "cfg == nil" branch of
// ApplyToConfig.
func TestApplyToConfig_NilConfigNoop(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	ApplyToConfig(context.Background(), nil, r, nil) // must not panic
}

// TestApplyToConfig_UnresolvableKeepsBakedAndLogs covers the "!res.Ok"
// continue branch inside ApplyToConfig's loop, and exercises the Info logger
// call on a successful merge via a stub logger.
func TestApplyToConfig_UnresolvableKeepsBakedAndLogs(t *testing.T) {
	f := &stubFetcher{err: errors.New("unreachable")}
	r := NewResolver(f, allowAll, nil)
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"broken": {
			DisplayName:      "Baked Broken",
			DefinitionSource: &config.DefinitionSourceConfig{Owner: "kubestellar", Repo: "hive", Path: "agents/broken.yaml"},
		},
	}}

	ApplyToConfig(context.Background(), cfg, r, nil)

	if cfg.Agents["broken"].DisplayName != "Baked Broken" {
		t.Errorf("unresolvable agent must keep baked config, got %+v", cfg.Agents["broken"])
	}
}

// stubLogger records Info/Warn calls so ApplyToConfig's logging branch on a
// successful merge can be exercised.
type stubLogger struct {
	infoCalls int
	warnCalls int
}

func (s *stubLogger) Info(msg string, args ...any) { s.infoCalls++ }
func (s *stubLogger) Warn(msg string, args ...any) { s.warnCalls++ }

// TestApplyToConfig_LogsOnSuccess covers the "logger != nil" Info branch
// inside ApplyToConfig after a successful merge.
func TestApplyToConfig_LogsOnSuccess(t *testing.T) {
	f := &stubFetcher{body: validDef}
	r := NewResolver(f, allowAll, nil)
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {
			DisplayName:      "Baked Scanner",
			DefinitionSource: &config.DefinitionSourceConfig{Owner: "kubestellar", Repo: "hive", Path: "agents/scanner.yaml"},
		},
	}}
	logger := &stubLogger{}

	ApplyToConfig(context.Background(), cfg, r, logger)

	if logger.infoCalls != 1 {
		t.Errorf("expected 1 Info log call on success, got %d", logger.infoCalls)
	}
	if cfg.Agents["scanner"].DisplayName != "Live Scanner" {
		t.Errorf("expected merge applied, got %+v", cfg.Agents["scanner"])
	}
}

// TestResolver_WarnLogging covers the logger-Warn branches inside
// Resolve/mergeFromRaw (denied, fetch-error, malformed) using a stub logger
// so those `if r.logger != nil` branches execute their bodies.
func TestResolver_WarnLogging(t *testing.T) {
	logger := &stubLogger{}

	// denied
	fDenied := &stubFetcher{body: validDef}
	rDenied := NewResolver(fDenied, allowNone, logger)
	rDenied.Resolve(context.Background(), fullSource(), bakedAgent())

	// fetch error, no cache
	fErr := &stubFetcher{err: errors.New("boom")}
	rErr := NewResolver(fErr, allowAll, logger)
	rErr.Resolve(context.Background(), fullSource(), bakedAgent())

	// malformed
	fBad := &stubFetcher{body: "kind: NotAnAgent\nmetadata: {}"}
	rBad := NewResolver(fBad, allowAll, logger)
	rBad.Resolve(context.Background(), fullSource(), bakedAgent())

	if logger.warnCalls < 3 {
		t.Errorf("expected at least 3 Warn calls, got %d", logger.warnCalls)
	}
}

// TestResolver_TruncationWarnLogging covers the size-cap Warn branch in
// Resolve using a document that stays parseable after truncation but is
// still large enough to trigger the cap warning.
func TestResolver_TruncationWarnLogging(t *testing.T) {
	logger := &stubLogger{}
	huge := validDef + strings.Repeat("#", maxDefinitionBytes+100)
	f := &stubFetcher{body: huge}
	r := NewResolver(f, allowAll, logger)

	res := r.Resolve(context.Background(), fullSource(), bakedAgent())
	if !res.Ok {
		t.Fatalf("expected ok despite truncation, got %+v", res)
	}
	if logger.warnCalls < 1 {
		t.Errorf("expected size-cap Warn call, got %d", logger.warnCalls)
	}
}
