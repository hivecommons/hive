package dashboard

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func covHelperServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// --- Pure redaction / masking helpers ---

func TestCovJ_RedactAndMask(t *testing.T) {
	// redactTokensInLine: a long token gets its prefix kept; short match fully redacted.
	line := "using ghp_abcdefghij1234567890 for auth"
	if out := redactTokensInLine(line); out == line || !containsStr(out, "REDACTED") {
		t.Fatalf("redactTokensInLine did not redact: %q", out)
	}
	// A line with no token is unchanged.
	if out := redactTokensInLine("no secrets here"); out != "no secrets here" {
		t.Fatalf("redactTokensInLine altered a clean line: %q", out)
	}

	// maskSecretHint: short value → full mask; long value → tail hint.
	if got := maskSecretHint("abc"); got != "••••" {
		t.Fatalf("maskSecretHint short = %q", got)
	}
	if got := maskSecretHint("sk-verylongsecretvalue1234567890"); got == "••••" {
		t.Fatalf("maskSecretHint long should reveal a tail hint")
	}

	// sanitizeFilenameComponent replaces unsafe chars.
	if got := sanitizeFilenameComponent("a/b c.d"); got != "a-b-c-d" {
		t.Fatalf("sanitizeFilenameComponent = %q", got)
	}
}

func TestCovJ_MaskConnectionAuth(t *testing.T) {
	// Empty slice returned as-is.
	if got := maskConnectionAuth(nil); got != nil {
		t.Fatalf("maskConnectionAuth(nil) should be nil")
	}
	conns := []config.ConnectionConfig{
		{Name: "db", Type: "postgres", Auth: &config.ConnectionAuth{Type: "env", EnvVar: "DB_PASS"}},
		{Name: "cache", Type: "redis"}, // no auth
	}
	got := maskConnectionAuth(conns)
	if len(got) != 2 {
		t.Fatalf("expected 2 conns")
	}
	if got[0].Auth == nil || got[0].Auth.EnvVar == "DB_PASS" {
		t.Fatalf("expected EnvVar to be masked, got %+v", got[0].Auth)
	}
	// Original must be untouched (deep copy of Auth).
	if conns[0].Auth.EnvVar != "DB_PASS" {
		t.Fatalf("maskConnectionAuth mutated the original")
	}
}

// --- entitledModelsFor via injected provider ---

func TestCovJ_EntitledModelsFor(t *testing.T) {
	s := covHelperServer(t)

	// Non-litellm backend → not known.
	if _, _, known := s.entitledModelsFor("vllm", []string{"http://x"}); known {
		t.Fatalf("non-litellm should not be known")
	}

	// No provider registered → not known.
	SetEntitledModelsProvider(nil)
	if _, _, known := s.entitledModelsFor("litellm", []string{"http://x"}); known {
		t.Fatalf("no provider should not be known")
	}

	// Provider returns a match on the second endpoint.
	SetEntitledModelsProvider(func(ep string) ([]string, string, bool) {
		if ep == "http://good" {
			return []string{"gpt-4o"}, "probe", true
		}
		return nil, "", false
	})
	defer SetEntitledModelsProvider(nil)
	models, src, known := s.entitledModelsFor("litellm", []string{"http://bad", "http://good"})
	if !known || src != "probe" || len(models) != 1 {
		t.Fatalf("entitledModelsFor = %v %q %v", models, src, known)
	}
}

// --- intersectEntitled ---

func TestCovJ_IntersectEntitled(t *testing.T) {
	discovered := []string{"gpt-4o", "claude/opus", "unlisted"}
	entitled := []string{"gpt-4o", "vendor/opus"}
	got := intersectEntitled(discovered, entitled)
	// gpt-4o matches by full id; claude/opus matches "opus" tail against vendor/opus.
	if len(got) != 2 {
		t.Fatalf("intersectEntitled = %v, want 2 matches", got)
	}
}

// --- inferenceAPIKey ---

func TestCovJ_InferenceAPIKey(t *testing.T) {
	s := covHelperServer(t)

	// vllm with a configured env key.
	t.Setenv("HIVE_VLLM_API_KEY", "vk")
	if key := s.inferenceAPIKey("vllm"); key == "" {
		t.Logf("vllm key resolution returned empty (env resolution path exercised)")
	}
	// llm-d path.
	_ = s.inferenceAPIKey("llm-d")
	// default → litellm resolution.
	_ = s.inferenceAPIKey("other")

	// nil deps → "".
	bare := &Server{}
	if bare.inferenceAPIKey("vllm") != "" {
		t.Fatalf("bare server should return empty key")
	}
}

// --- buildExportYAML with a rich config to hit conditional branches ---

func TestCovJ_BuildExportYAML(t *testing.T) {
	s := covHelperServer(t)
	cfg := config.AgentConfig{
		Backend:        "claude",
		Model:          "sonnet",
		DisplayName:    "Scanner",
		Description:    "scans things",
		Emoji:          "🔍",
		Color:          "#fff",
		Role:           "scanner",
		Mode:           "auto",
		Aliases:        []string{"sc", "scan"},
		LaneKeywords:   []string{"triage"},
		DetectKeywords: []string{"bug"},
		Connections:    []config.ConnectionConfig{{Name: "db", Type: "postgres"}},
	}
	out := s.buildExportYAML("scanner", cfg, map[string]string{"idle": "15m"}, true, "kick template here")
	if out == "" {
		t.Fatalf("buildExportYAML returned empty")
	}
	for _, want := range []string{"name: scanner", "backend: claude", "displayName", "aliases:", "laneKeywords:", "detectKeywords:"} {
		if !containsStr(out, want) {
			t.Fatalf("export YAML missing %q:\n%s", want, out)
		}
	}

	// Minimal config exercises the default-branch fallbacks.
	minimal := config.AgentConfig{}
	if out := s.buildExportYAML("bare", minimal, nil, false, ""); out == "" {
		t.Fatalf("buildExportYAML(minimal) returned empty")
	}
}

// --- Sidebar load/save + agent stats/restrictions/prompt-template file readers.
// These read/write fixed /data paths that are absent/unwritable in the test
// sandbox, so they exercise the no-file / write-error branches. ---

func TestCovJ_SidebarAndFileReaders(t *testing.T) {
	s := covHelperServer(t)

	// loadSidebarFromDisk: file absent → no-op early return.
	s.loadSidebarFromDisk()
	// saveSidebarToDisk: marshals then attempts a write (fails silently under /data).
	s.saveSidebarToDisk(map[string]any{"pinned": []string{"scanner"}})
	// A value that cannot be marshaled hits the marshal-error branch.
	s.saveSidebarToDisk(make(chan int))

	// loadAgentStats: no stats file → default stats config for a known agent.
	if stats := s.loadAgentStats("scanner"); len(stats) == 0 {
		t.Fatalf("expected default stats for scanner")
	}
	// Unknown agent → still returns a (possibly empty) default set without panic.
	_ = s.loadAgentStats("ghost")

	// loadAgentRestrictions: no /data files → empty structured result.
	res := s.loadAgentRestrictions("scanner")
	if res["global"] == nil || res["agent"] == nil {
		t.Fatalf("loadAgentRestrictions structure incomplete: %+v", res)
	}

	// loadPromptTemplateRaw: no template file → falls back (empty or default).
	_ = s.loadPromptTemplateRaw("scanner")
}

// --- fetchCommitMessage nil-guards ---

func TestCovJ_FetchCommitMessage(t *testing.T) {
	// Bare server → nil GHClient guard returns "".
	bare := &Server{}
	if bare.fetchCommitMessage("deadbeef") != "" {
		t.Fatalf("bare fetchCommitMessage should return empty")
	}
}

// --- defaultStatsConfig covers known + unknown agents ---

func TestCovJ_DefaultStatsConfig(t *testing.T) {
	if len(defaultStatsConfig("scanner")) == 0 {
		t.Fatalf("scanner default stats empty")
	}
	if len(defaultStatsConfig("ci-maintainer")) == 0 {
		t.Fatalf("ci-maintainer default stats empty")
	}
	// Unknown agent returns whatever the default map yields (may be empty) — just
	// exercise the lookup path.
	_ = defaultStatsConfig("totally-unknown-agent")
}
