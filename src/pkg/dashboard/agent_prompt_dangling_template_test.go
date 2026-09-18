package dashboard

// hivecommons/hive#7390: the prompt editor rendered a dangling kick_template
// as an empty box with a 404 repo link, and saving text into that box created
// a live override. These tests pin the surface: GET .../prompt reports the
// resolution (unresolved, fallback, paths tried) and omits the repo link for a
// path the repo does not ship; PUT .../general refuses to SET a new dangling
// name but leaves an unchanged one alone.

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// promptServer is apiServer plus a scheduler (the resolution authority) and
// an agent whose kick_template resolves nowhere.
//
// The scheduler's policy roots are pinned to empty temp dirs (#7477). They
// default to /data/policies, which template resolution consults BEFORE the
// embedded defaults; on a host running a live hive that directory is
// populated, so without this seam these tests assert against whatever the
// host happens to ship and "shipped template resolves to the embedded
// default" fails for a reason that has nothing to do with the code.
func promptServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	t.Cleanup(scheduler.SetPolicyDirsForTest(t.TempDir(), t.TempDir()))
	s, deps := apiServer(t)
	deps.Config.Agents["review"] = config.AgentConfig{Backend: "claude", Enabled: true, KickTemplate: "review.md"}
	deps.Config.Agents["scanner"] = config.AgentConfig{Backend: "claude", Model: "sonnet", Enabled: true, KickTemplate: "scanner-holdgated.md"}
	deps.Scheduler = scheduler.New(deps.Config, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	return s, deps
}

func TestHandleAgentPrompt_ReportsDanglingKickTemplate(t *testing.T) {
	s, _ := promptServer(t)
	rec := doGet(s, "/api/config/agent/review/prompt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	tpl, _ := resp["template"].(map[string]any)
	if tpl == nil {
		t.Fatalf("response carries no template resolution: %v", resp)
	}
	if tpl["kickTemplate"] != "review.md" || tpl["resolved"] != false || tpl["embeddedDefaultExists"] != false {
		t.Errorf("template = %v, want unresolved review.md with no embedded default", tpl)
	}
	if fb, _ := tpl["fallback"].(string); !strings.Contains(fb, "review") {
		t.Errorf("fallback must name what the kick will use, got %q", fb)
	}
	if paths, _ := tpl["pathsTried"].([]any); len(paths) == 0 {
		t.Error("pathsTried must list where the template was expected")
	}
	// The repo link must NOT be rendered for a path the repo does not ship.
	files, _ := resp["sourceFiles"].([]any)
	if len(files) != 1 {
		t.Fatalf("sourceFiles = %v", files)
	}
	sf := files[0].(map[string]any)
	if _, hasURL := sf["url"]; hasURL {
		t.Errorf("a dangling template must not get a repo link (it 404s): %v", sf)
	}
	if note, _ := sf["note"].(string); !strings.Contains(note, "not shipped") {
		t.Errorf("note must say the file is not shipped, got %q", note)
	}
}

func TestHandleAgentPrompt_ShippedTemplateKeepsLink(t *testing.T) {
	s, _ := promptServer(t)
	resp := decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt"))
	tpl, _ := resp["template"].(map[string]any)
	if tpl == nil || tpl["resolved"] != true || tpl["embeddedDefaultExists"] != true || tpl["source"] != scheduler.TemplateSourceEmbedded {
		t.Errorf("shipped template resolution = %v", tpl)
	}
	files, _ := resp["sourceFiles"].([]any)
	sf := files[0].(map[string]any)
	if url, _ := sf["url"].(string); !strings.HasSuffix(url, "src/pkg/policies/defaults/scanner-holdgated.md") {
		t.Errorf("shipped template must keep its repo link, got %v", sf)
	}
	if p, _ := resp["prompt"].(string); p == "" {
		t.Error("shipped template must render content")
	}
}

// The precedence order itself, pinned hermetically (#7477). The same request
// must report the embedded default when nothing is on disk and the on-disk
// copy when one exists — which is only a statement about the code if the
// roots are ours rather than the host's /data/policies.
func TestHandleAgentPrompt_UserSavedTemplateShadowsEmbeddedDefault(t *testing.T) {
	s, _ := promptServer(t)

	resp := decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt"))
	tpl, _ := resp["template"].(map[string]any)
	if tpl == nil || tpl["source"] != scheduler.TemplateSourceEmbedded {
		t.Fatalf("with an empty user-saved dir the embedded default must serve: %v", tpl)
	}

	// Now put a user-saved copy where the dashboard prompt editor writes one.
	userSaved := t.TempDir()
	t.Cleanup(scheduler.SetPolicyDirsForTest(userSaved, t.TempDir()))
	override := filepath.Join(userSaved, "scanner-holdgated.md")
	if err := os.WriteFile(override, []byte("# user-saved override"), 0o644); err != nil {
		t.Fatalf("seed user-saved template: %v", err)
	}

	resp = decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt"))
	tpl, _ = resp["template"].(map[string]any)
	if tpl == nil || tpl["resolved"] != true || tpl["source"] != override {
		t.Errorf("a user-saved template must shadow the embedded default, got %v (want source %q)", tpl, override)
	}
	// The embedded default still exists — the editor's repo link stays valid
	// even though the on-disk copy is what a kick would render.
	if tpl["embeddedDefaultExists"] != true {
		t.Errorf("embeddedDefaultExists must still report the shipped file: %v", tpl)
	}
}

// Setting a NEW dangling name is refused at write time; a path is refused
// outright; an unchanged dangling value does not block an unrelated edit.
func TestHandleAgentConfigGeneral_KickTemplateValidation(t *testing.T) {
	s, deps := promptServer(t)

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"kickTemplate": "does-not-exist.md"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("new dangling kick_template: status=%d body=%s, want 400 naming the missing file", rec.Code, rec.Body.String())
	}
	if deps.Config.Agents["scanner"].KickTemplate != "scanner-holdgated.md" {
		t.Error("a refused kick_template must not be persisted")
	}

	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"kickTemplate": "../../etc/passwd"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "kickTemplate must match pattern") {
		t.Errorf("path kick_template: status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// review already carries a dangling name; re-sending it unchanged next
	// to a display-name edit must succeed — the stale field cannot hold an
	// unrelated edit hostage.
	rec = doPut(s, "/api/config/agent/review/general", map[string]any{"kickTemplate": "review.md", "displayName": "Reviewer"})
	if rec.Code != http.StatusOK {
		t.Errorf("unchanged dangling kick_template alongside another edit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if deps.Config.Agents["review"].DisplayName != "Reviewer" {
		t.Error("the unrelated edit must land")
	}

	// A shipped name is accepted, and clearing the field is accepted.
	rec = doPut(s, "/api/config/agent/review/general", map[string]any{"kickTemplate": "scanner-advisory.md"})
	if rec.Code != http.StatusOK || deps.Config.Agents["review"].KickTemplate != "scanner-advisory.md" {
		t.Errorf("shipped kick_template: status=%d kick_template=%q", rec.Code, deps.Config.Agents["review"].KickTemplate)
	}
	rec = doPut(s, "/api/config/agent/review/general", map[string]any{"kickTemplate": ""})
	if rec.Code != http.StatusOK || deps.Config.Agents["review"].KickTemplate != "" {
		t.Errorf("clearing kick_template: status=%d kick_template=%q", rec.Code, deps.Config.Agents["review"].KickTemplate)
	}
}

// The editor strings and the no-link rendering are wired in the dashboard.
func TestDanglingTemplateWiredInDashboard(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"⚠ template not found: ",
		"kicks fall back to",
		"const dangling = !!(tpl && tpl.kickTemplate && !tpl.resolved);",
		"const pathHtml = sf.url",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q (#7390)", want)
		}
	}
}
