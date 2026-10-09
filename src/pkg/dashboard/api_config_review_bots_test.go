package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorReviewBotsEffectiveSettings(t *testing.T) {
	project := filepath.Join(t.TempDir(), "hive-project.yaml")
	t.Setenv("HIVE_PROJECT_YAML", project)
	s := covApiServer(t)

	read := func() reviewBotsSection {
		t.Helper()
		rec := doOwnerGet(s, "/api/config/governor")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET governor: %d: %s", rec.Code, rec.Body.String())
		}
		var bundle map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
			t.Fatal(err)
		}
		raw, ok := bundle["review_bots"]
		if !ok {
			t.Fatal("governor bundle must expose review_bots")
		}
		var section reviewBotsSection
		if err := json.Unmarshal(raw, &section); err != nil {
			t.Fatal(err)
		}
		return section
	}

	// No project file: show the effective defaults and an empty list, not
	// omitted keys or JSON null that the UI would have to guess about.
	got := read()
	if got.Enabled || got.Logins == nil || len(got.Logins) != 0 || got.MinPriority != "" || got.MaxAttemptsPerThread != 1 || !got.ResolveAfterFix || got.LoadError != "" {
		t.Fatalf("disabled defaults: %+v", got)
	}

	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n    min_priority: p2\n    max_attempts_per_thread: 3\n    resolve_after_fix: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = read()
	if !got.Enabled || !reflect.DeepEqual(got.Logins, []string{"Copilot"}) || got.MinPriority != "P2" || got.MaxAttemptsPerThread != 3 || got.ResolveAfterFix || got.LoadError != "" {
		t.Fatalf("project fallback: %+v", got)
	}

	if s.deps.Config.Classification.ReviewBots.Enabled() {
		t.Fatal("GET must not copy project-file logins into hive.yaml")
	}

	// hive.yaml overrides the entire project block when it names a login.
	s.deps.Config.Classification.ReviewBots = config.ReviewBotsConfig{Logins: []string{"CodeRabbit"}}
	got = read()
	if !reflect.DeepEqual(got.Logins, []string{"CodeRabbit"}) || got.MaxAttemptsPerThread != 1 || !got.ResolveAfterFix {
		t.Fatalf("hive.yaml precedence/defaults: %+v", got)
	}

	// Reading must never materialize the fallback as a writable trust grant.
	s.deps.Config.Classification.ReviewBots = config.ReviewBotsConfig{}
	if err := os.WriteFile(project, []byte("classification: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	got = read()
	if got.Enabled || got.LoadError == "" {
		t.Fatalf("invalid project must be visibly unavailable, not enabled: %+v", got)
	}
	if s.deps.Config.Classification.ReviewBots.Enabled() {
		t.Fatal("GET mutated review-bot configuration")
	}
}

func TestReviewBotsSettingsReadOnlyUI(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	start := strings.Index(html, ">External review bots ")
	if start < 0 {
		t.Fatal("Review card must display the external review-bot settings")
	}
	mid := strings.Index(html[start:], `id="review-bots-min-priority-field"`)
	if mid < 0 {
		t.Fatal("Review card must offer the review-bot min_priority control")
	}
	end := strings.Index(html[start:], ">Effectiveness")
	if end < mid {
		t.Fatal("missing end of review-bot settings")
	}
	block := html[start : start+mid]
	for _, want := range []string{"rb.enabled", "esc(login)", "rb.max_attempts_per_thread", "rb.resolve_after_fix", "esc(rb.load_error)", "settings unavailable", "trust boundary"} {
		if !strings.Contains(block, want) {
			t.Errorf("review-bot settings missing %q", want)
		}
	}
	// Logins and the other fields stay read-only: adding a login grants
	// thread-resolution rights.
	for _, forbidden := range []string{"<input", "<select", "data-action=", "data-change-action="} {
		if strings.Contains(block, forbidden) {
			t.Errorf("read-only settings contain an editable control: %s", forbidden)
		}
	}

	control := html[start+mid : start+end]
	for _, want := range []string{"<select", `data-change-action="markDirtyReviewBotsMinPriority"`, "'all', 'P0', 'P1', 'P2', 'P3'", "rb.min_priority", "left open for a human", "unknown or badge-less format", "always routed"} {
		if !strings.Contains(control, want) {
			t.Errorf("min_priority control missing %q", want)
		}
	}
	if strings.Contains(control, "<input") || strings.Contains(control, "rbLogins") {
		t.Error("min_priority control must not edit logins")
	}
	if !strings.Contains(html, "markDirty('review', 'review_bots', { min_priority: value })") {
		t.Error("min_priority must save through the owner-gated PUT /api/config/review")
	}
	for _, native := range []string{"window.prompt(", "window.alert(", "window.confirm("} {
		if strings.Contains(control, native) {
			t.Errorf("min_priority control must not use native dialogs: %s", native)
		}
	}
}

// TestReviewConfigPut_ReviewBotsMinPriority covers the one editable
// review_bots field (#10481): owner-gated like fix_human_prs, absent key
// leaves it alone, values are validated and normalised before any mutation,
// and the override lands in hive.yaml without copying project-file logins.
func TestReviewConfigPut_ReviewBotsMinPriority(t *testing.T) {
	project := filepath.Join(t.TempDir(), "hive-project.yaml")
	t.Setenv("HIVE_PROJECT_YAML", project)
	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n    min_priority: P1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := covApiServer(t)
	stored := func() string { return s.deps.Config.Classification.ReviewBots.MinPriority }

	if rec := doPutNoRole(s, "/api/config/review", `{"review_bots":{"min_priority":"P0"}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT min_priority: expected 403, got %d", rec.Code)
	}
	if stored() != "" {
		t.Fatal("refused write still set min_priority")
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"require_approval": true, "review_bots": map[string]any{"min_priority": "P9"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid min_priority: expected 400, got %d", rec.Code)
	}
	if stored() != "" || s.deps.Config.Review.RequireApproval {
		t.Fatal("rejected request must not mutate anything")
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"review_bots": map[string]any{"min_priority": " p3 "}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT min_priority: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if stored() != "P3" {
		t.Fatalf("min_priority not normalised/stored: %q", stored())
	}
	if s.deps.Config.Classification.ReviewBots.Enabled() {
		t.Fatal("writing min_priority must not copy project-file logins into hive.yaml")
	}
	rb := reviewBotsSectionResponse(s.deps.Config)
	if rb.MinPriority != "P3" || !rb.Enabled || !reflect.DeepEqual(rb.Logins, []string{"Copilot"}) {
		t.Fatalf("hive.yaml override must win over the project threshold only: %+v", rb)
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"all_authors": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT without the key: expected 200, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"review_bots": map[string]any{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty review_bots: expected 200, got %d", rec.Code)
	}
	if stored() != "P3" {
		t.Fatalf("absent min_priority key changed the stored value: %q", stored())
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"review_bots": map[string]any{"min_priority": "all"}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT all: expected 200, got %d", rec.Code)
	}
	if stored() != config.ReviewBotsMinPriorityAll || reviewBotsSectionResponse(s.deps.Config).MinPriority != "" {
		t.Fatalf("explicit all must be stored and override the project threshold: %q", stored())
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"review_bots": map[string]any{"min_priority": ""}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty: expected 200, got %d", rec.Code)
	}
	if stored() != "" || reviewBotsSectionResponse(s.deps.Config).MinPriority != "P1" {
		t.Fatalf("empty must clear the override back to the project threshold: %q", stored())
	}
}
