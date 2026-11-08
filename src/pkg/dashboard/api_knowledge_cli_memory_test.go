package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const drainedDraftEntry = "---\ntitle: Webhook retries need the idempotency key\ntype: reference\nlayer: project\nstate: draft\nsource: cli-memory\ncli: claude\nagent: scanner\nmemhash: aaaa\ntags: [cli-memory, cli:claude, memhash:aaaa, agent:scanner]\n---\n\nWebhook retries need the idempotency key.\n"

// withAutoMemoryVault connects a bead-synth-wiki vault holding one drained
// CLI-memory draft (cli-memory-aaaa) to s and returns its directory.
func withAutoMemoryVault(t *testing.T, s *Server) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bead-synth-wiki")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-memory-aaaa.md"), []byte(drainedDraftEntry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.deps.Knowledge.ConnectVault(dir, "bead-synth-wiki"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestKnowledgeAgentReadsHideAutoMemoryDrafts covers the #17058 exclusion on
// the pull-mode surfaces: an agent-identified TOC, entry or effective read
// never shows a drained draft, even when the request and the agent's scope
// admit drafts, while operator reads list it for review; approving it through
// the state API makes it visible to agents.
func TestKnowledgeAgentReadsHideAutoMemoryDrafts(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	withAutoMemoryVault(t, s)

	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"/api/knowledge/toc?include_states=draft", true},
		{"/api/knowledge/toc?agent=scanner&include_states=all", false},
		{"/api/knowledge/toc?agent=reviewer&include_states=all", false},
	} {
		code, body := tocGet(t, s, tc.target)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.target, code)
		}
		ids := tocIDs(t, body)
		if got := strings.Contains(ids, "cli-memory-aaaa"); got != tc.want {
			t.Errorf("%s ids = %q, auto-memory draft listed = %v, want %v", tc.target, ids, got, tc.want)
		}
		if strings.Contains(tc.target, "scanner") && !strings.Contains(ids, "draft-idea") {
			t.Errorf("%s ids = %q: the agent's ordinary drafts must still be admitted", tc.target, ids)
		}
	}

	if code, _ := tocGet(t, s, "/api/knowledge/entry/cli-memory-aaaa?include_states=draft"); code != http.StatusOK {
		t.Errorf("operator entry read status = %d, want 200", code)
	}
	if code, _ := tocGet(t, s, "/api/knowledge/entry/cli-memory-aaaa?agent=reviewer&include_states=draft"); code != http.StatusNotFound {
		t.Errorf("agent entry read status = %d, want 404", code)
	}
	if _, body := tocGet(t, s, "/api/knowledge/search?tag=cli-memory&include_states=draft"); body["count"] != float64(1) {
		t.Errorf("operator search for auto-memory drafts = %v, want one result", body)
	}
	_, eff := tocGet(t, s, "/api/knowledge/effective?agent=scanner&include_states=all")
	excluded, _ := json.Marshal(eff["excluded"])
	if !strings.Contains(string(excluded), `"id":"cli-memory-aaaa"`) || !strings.Contains(string(excluded), `"reason":"lifecycle"`) {
		t.Errorf("effective view excluded = %s, want cli-memory-aaaa with reason lifecycle", excluded)
	}

	if rec := doKnowledgeStatePut(s, "cli-memory-aaaa", "alice", true, `{"state":"approved","reason":"reviewed"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, body := tocGet(t, s, "/api/knowledge/toc?agent=scanner"); !strings.Contains(tocIDs(t, body), "cli-memory-aaaa") {
		t.Errorf("approved auto-memory entry missing from the agent TOC: %s", tocIDs(t, body))
	}
}

func postLegacyDrain(s *Server, body string) (int, map[string]interface{}) {
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/cli-memory/drain-legacy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "alice")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestKnowledgeDrainLegacyMemory(t *testing.T) {
	// Mirror ~/.claude/projects/<slug>/memory: repoFromClaudeSlug keys off
	// the "projects" segment, so the fixture must carry it too.
	projects := filepath.Join(t.TempDir(), ".claude", "projects")
	prev := legacyClaudeProjectsDir
	legacyClaudeProjectsDir = projects
	t.Cleanup(func() { legacyClaudeProjectsDir = prev })
	memFile := filepath.Join(projects, "-data-agents-scanner-hivecommons-hive", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(memFile), 0o755); err != nil {
		t.Fatal(err)
	}
	const memory = "- Never force-push a shared branch.\n"
	if err := os.WriteFile(memFile, []byte(memory), 0o644); err != nil {
		t.Fatal(err)
	}

	s := knowledgeTOCServer(t)
	if code, _ := postLegacyDrain(s, ""); code != http.StatusConflict {
		t.Fatalf("drain without the bead-synth vault status = %d, want 409", code)
	}
	vault := withAutoMemoryVault(t, s)
	s.deps.Config.Project.Repos = []string{"hivecommons/hive"}

	code, resp := postLegacyDrain(s, "")
	if code != http.StatusOK || resp["drained"] != float64(1) || resp["agent"] != "shared" || resp["truncate"] != false {
		t.Fatalf("first drain = %d %v", code, resp)
	}
	if b, _ := os.ReadFile(memFile); string(b) != memory {
		t.Fatalf("memory file changed without truncate: %q", b)
	}
	entries, _ := filepath.Glob(filepath.Join(vault, "cli-memory-*.md"))
	var drained string
	for _, e := range entries {
		if b, _ := os.ReadFile(e); strings.Contains(string(b), "force-push") {
			drained = string(b)
		}
	}
	for _, want := range []string{"state: draft", "agent:shared", "repo:hivecommons/hive"} {
		if !strings.Contains(drained, want) {
			t.Fatalf("drained entry missing %q:\n%s", want, drained)
		}
	}
	if _, body := tocGet(t, s, "/api/knowledge/toc?agent=reviewer&include_states=all"); strings.Contains(compactJSON(body), "force-push") {
		t.Fatal("legacy draft visible to an agent")
	}

	code, resp = postLegacyDrain(s, `{"truncate":true}`)
	if code != http.StatusOK || resp["drained"] != float64(0) || resp["dupes"] != float64(1) {
		t.Fatalf("second drain = %d %v", code, resp)
	}
	if got := compactJSON(resp["truncated"]); got != `["`+memFile+`"]` {
		t.Fatalf("truncated = %s, want [%s]", got, memFile)
	}
	if b, _ := os.ReadFile(memFile); len(b) != 0 {
		t.Fatalf("memory file not emptied: %q", b)
	}
	audits := knowledgeStateAudit(s, "knowledge_drain_legacy_memory")
	// Recent lists newest first: [0] is the truncating run.
	if len(audits) != 2 || !strings.Contains(audits[0].Detail, "truncate=true") || !strings.Contains(audits[1].Detail, "truncate=false") || audits[0].User != "alice" {
		t.Fatalf("audit = %+v", audits)
	}

	if code, _ := postLegacyDrain(s, "{not json"); code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", code)
	}
}

func compactJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
