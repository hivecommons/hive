package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for GET/PUT /api/config/decisions/delegate, the "Let hive decide"
// settings. Before this endpoint nothing wrote decisions.delegate, so every
// request here answered 404/405 and the owner had to edit hive.yaml.

const decisionsDelegateTestPath = "/api/config/decisions/delegate"

// delegatePutAs sends a PUT as a named, verified owner.
func delegatePutAs(s *Server, user string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, decisionsDelegateTestPath, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", user)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeDelegateSettings(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode decisions.delegate response: %v: %s", err, rec.Body.String())
	}
	return out
}

func delegateAuditRows(s *Server) []AuditEntry {
	return s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), auditConfigDecisionsDelegate)
}

// A fake key built at run time, so no scanner reads the test as a leak.
func fakeDelegateKey() string { return "sk-" + strings.Repeat("a1B2", 10) }

// GET shows the defaults; a PUT changes every field, is saved into the live
// config the Let hive decide endpoints read per request, is echoed back and
// audited with the actor and each change before and after.
func TestDecisionsDelegateSettingsRoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Project.Repos = []string{"repo1", "repo2"}

	rec := doOwnerGet(s, decisionsDelegateTestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeDelegateSettings(t, rec)
	llm, _ := got["llm"].(map[string]any)
	if got["enabled"] != true || got["enabled_set"] != false || llm["enabled"] != false || len(got["repos"].(map[string]any)) != 0 {
		t.Fatalf("defaults wrong: %+v", got)
	}

	rec = delegatePutAs(s, "alice", map[string]any{
		"enabled":        false,
		"repos":          map[string]any{"repo1": map[string]any{"enabled": true}},
		"llm":            map[string]any{"enabled": true, "model": "anthropic/claude-sonnet-4-5"},
		"effect_timeout": "45m",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got = decodeDelegateSettings(t, rec)
	llm, _ = got["llm"].(map[string]any)
	repos, _ := got["repos"].(map[string]any)
	if got["enabled"] != false || llm["enabled"] != true || llm["model"] != "anthropic/claude-sonnet-4-5" || got["effect_timeout"] != "45m0s" {
		t.Fatalf("PUT response did not echo the change: %+v", got)
	}
	if entry, _ := repos["myorg/repo1"].(map[string]any); entry["enabled"] != true || len(repos) != 1 {
		t.Fatalf("repos = %+v, want only myorg/repo1 on", repos)
	}

	d := s.deps.Config.Decisions.Delegate
	if d.IsEnabled() || !d.LLMEnabled() || d.LLMModel() != "anthropic/claude-sonnet-4-5" || d.EffectTimeoutDuration() != 45*time.Minute {
		t.Fatalf("config not updated: %+v", d)
	}
	// Live pickup: the gates the Let hive decide endpoints call read the
	// same config, so the change holds with no restart.
	if !s.delegateDecisionEnabled("myorg/repo1") || s.delegateDecisionEnabled("myorg/repo2") {
		t.Fatalf("delegate gate not live: repo1=%v repo2=%v", s.delegateDecisionEnabled("myorg/repo1"), s.delegateDecisionEnabled("myorg/repo2"))
	}

	rows := delegateAuditRows(s)
	if len(rows) != 1 || rows[0].User != "alice" {
		t.Fatalf("audit rows = %+v, want one by alice", rows)
	}
	for _, want := range []string{"enabled: default on → false", "llm.enabled: default off → true", "repos: none → myorg/repo1=true"} {
		if !strings.Contains(rows[0].Detail, want) {
			t.Fatalf("audit detail %q does not record %q", rows[0].Detail, want)
		}
	}

	// repos is the whole map: an entry sent with enabled null is dropped.
	rec = delegatePutAs(s, "alice", map[string]any{"repos": map[string]any{"myorg/repo1": map[string]any{"enabled": nil}, "repo2": map[string]any{"enabled": true}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT repos: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if r := s.deps.Config.Decisions.Delegate.Repos; len(r) != 1 || r["myorg/repo2"].Enabled == nil || !*r["myorg/repo2"].Enabled {
		t.Fatalf("repos after replace = %+v, want only myorg/repo2 on", r)
	}
	if !s.deps.Config.Decisions.Delegate.LLMEnabled() {
		t.Fatal("a repos-only PUT changed llm")
	}
}

// An override already held for a repository the hive no longer watches can be
// kept when the whole map is sent back, but a new one must name a watched
// repository.
func TestDecisionsDelegateKeepsHeldOverrideForUnwatchedRepo(t *testing.T) {
	s := covApiServer(t)
	off := false
	s.deps.Config.Decisions.Delegate.Repos = map[string]config.DelegateDecisionRepoConfig{"gone/old": {Enabled: &off}}
	rec := delegatePutAs(s, "alice", map[string]any{"repos": map[string]any{"gone/old": map[string]any{"enabled": false}, "repo1": map[string]any{"enabled": true}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("held override: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if r := s.deps.Config.Decisions.Delegate.Repos; len(r) != 2 {
		t.Fatalf("repos = %+v, want gone/old and myorg/repo1", r)
	}
}

// Every invalid body is 400 and leaves the config, and the audit log, as they
// were.
func TestDecisionsDelegatePutInvalidChangesNothing(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":                 `{nope`,
		"unknown top-level field":  `{"llm_enabled": true}`,
		"unknown llm field":        `{"llm": {"enabled": true, "api_key_env": "X"}}`,
		"unknown repo field":       `{"repos": {"repo1": {"enabled": true, "llm": true}}}`,
		"two objects":              `{"enabled": false} {"enabled": true}`,
		"negative effect timeout":  `{"effect_timeout": "-5m"}`,
		"zero effect timeout":      `{"effect_timeout": "0s"}`,
		"timeout not a duration":   `{"effect_timeout": "soon"}`,
		"repository not watched":   `{"repos": {"other/repo": {"enabled": true}}}`,
		"not a repository name":    `{"repos": {"a/b/c": {"enabled": true}}}`,
		"same repository twice":    `{"repos": {"repo1": {"enabled": true}, "MyOrg/Repo1": {"enabled": false}}}`,
		"model with a space":       `{"llm": {"model": "gpt 4"}}`,
		"model with a placeholder": `{"llm": {"model": "${HIVE_KEY}"}}`,
		"enabled not a boolean":    `{"enabled": "yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			// Two separately built values, so a handler that changed the
			// live block in place cannot also change the expectation.
			seed := func() config.DelegateDecisionConfig {
				on, off := true, false
				return config.DelegateDecisionConfig{
					Repos: map[string]config.DelegateDecisionRepoConfig{"myorg/repo1": {Enabled: &off}},
					LLM:   &config.DelegateDecisionLLMConfig{Enabled: &on, Model: "m1"},
				}
			}
			s := covApiServer(t)
			s.deps.Config.Decisions.Delegate = seed()
			rec := doPutRaw(s, decisionsDelegateTestPath, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(s.deps.Config.Decisions.Delegate, seed()) {
				t.Fatalf("a refused PUT changed the config: %+v", s.deps.Config.Decisions.Delegate)
			}
			if rows := delegateAuditRows(s); len(rows) != 0 {
				t.Fatalf("a refused PUT was audited: %+v", rows)
			}
		})
	}
}

// Only a verified owner may read or change the settings.
func TestDecisionsDelegateRefusesNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, decisionsDelegateTestPath); rec.Code != http.StatusForbidden {
		t.Fatalf("GET with no role: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, decisionsDelegateTestPath, `{"llm": {"enabled": true}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT with no role: expected 403, got %d", rec.Code)
	}
	for _, role := range []string{"owner", "merger", "read"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, decisionsDelegateTestPath, strings.NewReader(`{"llm": {"enabled": true}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-Role", role) // claimed, never verified
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("PUT with unverified role %s: expected 403, got %d", role, rec.Code)
		}
	}
	if s.deps.Config.Decisions.Delegate.LLMEnabled() {
		t.Fatal("a refused PUT turned the LLM step on")
	}
	if rows := delegateAuditRows(s); len(rows) != 0 {
		t.Fatalf("a refused PUT was audited: %+v", rows)
	}
}

// llm.model takes a model name. A pasted key is refused, and a key-like value
// already in hive.yaml is never returned by GET, the governor bundle or the
// audit row.
func TestDecisionsDelegateKeyLikeModelRefusedAndMasked(t *testing.T) {
	s := covApiServer(t)
	for _, model := range []string{fakeDelegateKey(), "ghp_" + strings.Repeat("Z9", 18), strings.Repeat("0f", 20)} {
		rec := doPut(s, decisionsDelegateTestPath, map[string]any{"llm": map[string]any{"model": model}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "looks like an API key") {
			t.Fatalf("key-like model: expected 400 naming the key, got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), model) {
			t.Fatalf("the refusal echoed the key: %s", rec.Body.String())
		}
	}
	if s.deps.Config.Decisions.Delegate.LLMModel() != "" {
		t.Fatal("a refused model was stored")
	}

	key := fakeDelegateKey()
	on := true
	s.deps.Config.Decisions.Delegate.LLM = &config.DelegateDecisionLLMConfig{Enabled: &on, Model: key}
	rec := doOwnerGet(s, decisionsDelegateTestPath)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), key) {
		t.Fatalf("GET returned the stored key-like model: %d %s", rec.Code, rec.Body.String())
	}
	llm, _ := decodeDelegateSettings(t, rec)["llm"].(map[string]any)
	if llm["model_masked"] != true || llm["model"] != "" {
		t.Fatalf("llm = %+v, want model masked", llm)
	}
	if rec := doOwnerGet(s, "/api/config/governor"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decisions_delegate"`) || strings.Contains(rec.Body.String(), key) {
		t.Fatalf("the governor settings bundle must carry decisions_delegate without the stored key-like model: %d", rec.Code)
	}
	if rec := delegatePutAs(s, "alice", map[string]any{"llm": map[string]any{"model": "openrouter/meta-llama/llama-3.1-405b-instruct"}}); rec.Code != http.StatusOK {
		t.Fatalf("replacing a masked model: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows := delegateAuditRows(s)
	if len(rows) != 1 || strings.Contains(rows[0].Detail, key) || !strings.Contains(rows[0].Detail, "(masked)") {
		t.Fatalf("audit rows = %+v, want the old model masked", rows)
	}
}
