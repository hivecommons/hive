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
	if got.Enabled || got.Logins == nil || len(got.Logins) != 0 || got.MaxAttemptsPerThread != 1 || !got.ResolveAfterFix || got.LoadError != "" {
		t.Fatalf("disabled defaults: %+v", got)
	}

	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n    max_attempts_per_thread: 3\n    resolve_after_fix: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = read()
	if !got.Enabled || !reflect.DeepEqual(got.Logins, []string{"Copilot"}) || got.MaxAttemptsPerThread != 3 || got.ResolveAfterFix || got.LoadError != "" {
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
	start := strings.Index(html, "External review bots (read-only)")
	if start < 0 {
		t.Fatal("Review card must display the external review-bot settings")
	}
	end := strings.Index(html[start:], ">Effectiveness")
	if end < 0 {
		t.Fatal("missing end of review-bot settings")
	}
	block := html[start : start+end]
	for _, want := range []string{"rb.enabled", "esc(login)", "rb.max_attempts_per_thread", "rb.resolve_after_fix", "esc(rb.load_error)", "settings unavailable", "trust boundary"} {
		if !strings.Contains(block, want) {
			t.Errorf("review-bot settings missing %q", want)
		}
	}
	for _, forbidden := range []string{"<input", "<select", "data-action=", "data-change-action="} {
		if strings.Contains(block, forbidden) {
			t.Errorf("read-only settings contain an editable control: %s", forbidden)
		}
	}
}
