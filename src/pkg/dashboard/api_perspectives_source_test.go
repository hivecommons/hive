package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/perspsrc"
)

// hivecommons/hive#18797: review.perspectives_source is settable and clearable
// through PUT /api/config/review, gated like definition_source.

const perspectivesDoc = `kind: ReviewPerspectives
perspectives:
  - name: security
  - name: api-compat
    focus: "public API breakage"
`

type docFetcher struct{ calls int }

func (f *docFetcher) GetFileContentRef(_ context.Context, _, _, _, _ string) (string, error) {
	f.calls++
	return perspectivesDoc, nil
}

func approveSeedRepo(cfg *config.Config, slug string) {
	cfg.Variables.Security.AllowGitHubPrompt = true
	cfg.Variables.Security.GitHubPromptAllowlist = []string{slug}
}

func TestReviewConfigPutSetsAndClearsPerspectivesSource(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	approveSeedRepo(cfg, "acme/rules")
	fetcher := &docFetcher{}
	resolver := perspsrc.NewResolver(fetcher, cfg.GitHubPromptAllowed, nil)
	applied := 0
	// Mirrors the hive's ReviewConfigApplied hook: re-resolve on every save.
	s.deps.ReviewConfigApplied = func(config.ReviewConfig) {
		applied++
		perspsrc.ApplyToConfig(context.Background(), cfg, resolver, nil)
	}

	rec := doPut(s, "/api/config/review", map[string]any{"perspectives_source": map[string]any{
		"owner": "acme", "repo": "rules", "path": "/.hive/p.yaml", "ref": "main",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	src := cfg.Review.PerspectivesSource
	if src == nil || src.Owner != "acme" || src.Repo != "rules" || src.Path != ".hive/p.yaml" || src.Ref != "main" {
		t.Fatalf("source not stored (leading slash trimmed): %+v", src)
	}
	// No restart: the linked set resolved inside the PUT.
	if applied != 1 || fetcher.calls != 1 || cfg.Review.LinkedPerspectives == nil {
		t.Fatalf("not re-resolved after save: applied=%d fetches=%d linked=%v", applied, fetcher.calls, cfg.Review.LinkedPerspectives)
	}
	got := decodeJSON(t, doGetNoRole(s, "/api/config/review"))
	if got["perspectives_linked"] != true || got["perspectives_source_approved"] != true {
		t.Fatalf("GET after set: %v", got)
	}
	if names, _ := got["perspectives"].([]any); len(names) != 2 || names[1] != "api-compat" {
		t.Fatalf("GET perspectives = %v, want the linked set", got["perspectives"])
	}
	if !strings.HasPrefix(got["perspectives_source_status"].(string), "github:acme/rules") {
		t.Fatalf("status = %v", got["perspectives_source_status"])
	}

	// Inline edits are refused while linked, and the message names Unlink.
	rec = doPut(s, "/api/config/review", map[string]any{"perspectives": []string{"security"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Unlink") {
		t.Fatalf("expected 409 pointing at Unlink, got %d: %s", rec.Code, rec.Body.String())
	}

	// null clears the source and the runtime-only linked set with it.
	rec = doPut(s, "/api/config/review", map[string]any{"perspectives_source": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if cfg.Review.PerspectivesSource != nil || cfg.Review.LinkedPerspectives != nil {
		t.Fatalf("not cleared: %+v", cfg.Review)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"perspectives": []string{"security"}}); rec.Code != http.StatusOK {
		t.Fatalf("inline edit after unlink: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReviewConfigPutPerspectivesSourceUnapprovedRepoIsActionable(t *testing.T) {
	s := covApiServer(t)
	rec := doPut(s, "/api/config/review", map[string]any{"perspectives_source": map[string]any{
		"owner": "evil", "repo": "rules", "path": "p.yaml",
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"evil/rules", "not approved", "Settings, Security, Prompt sources"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("error %q does not mention %q", rec.Body.String(), want)
		}
	}
	if s.deps.Config.Review.PerspectivesSource != nil {
		t.Fatalf("refused PUT stored a source: %+v", s.deps.Config.Review.PerspectivesSource)
	}
}

func TestReviewConfigPutPerspectivesSourceValidation(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	approveSeedRepo(cfg, "acme/rules")
	cfg.Review.PerspectivesSource = &config.DefinitionSourceConfig{Owner: "acme", Repo: "rules", Path: "p.yaml"}

	// A source missing owner, repo or path counts as unset, as for definition_source.
	rec := doPut(s, "/api/config/review", map[string]any{"perspectives_source": map[string]any{"owner": "acme", "repo": "rules"}})
	if rec.Code != http.StatusOK || cfg.Review.PerspectivesSource != nil {
		t.Fatalf("partial source should clear: %d %s %+v", rec.Code, rec.Body.String(), cfg.Review.PerspectivesSource)
	}
	// A non-object is a 400 and changes nothing.
	cfg.Review.PerspectivesSource = &config.DefinitionSourceConfig{Owner: "acme", Repo: "rules", Path: "p.yaml"}
	rec = doPut(s, "/api/config/review", map[string]any{"perspectives_source": "acme/rules"})
	if rec.Code != http.StatusBadRequest || cfg.Review.PerspectivesSource == nil {
		t.Fatalf("string source: %d %s", rec.Code, rec.Body.String())
	}
	// Unlinking and editing inline in one request is allowed.
	rec = doPut(s, "/api/config/review", map[string]any{"perspectives_source": nil, "perspectives": []string{"security"}})
	if rec.Code != http.StatusOK || cfg.Review.PerspectivesSource != nil || len(cfg.Review.Perspectives) != 1 {
		t.Fatalf("unlink + edit: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReviewConfigGetFlagsUnapprovedLinkedRepo(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Review.PerspectivesSource = &config.DefinitionSourceConfig{Owner: "acme", Repo: "rules", Path: "p.yaml"}
	got := decodeJSON(t, doGetNoRole(s, "/api/config/review"))
	if got["perspectives_linked"] != true || got["perspectives_source_approved"] != false {
		t.Fatalf("GET = %v, want linked and not approved", got)
	}
}
