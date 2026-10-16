package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestBeadSynthPublishPipelineRoundTrip(t *testing.T) {
	s := covApiServer(t)
	read := func() config.BeadSynthPublishConfig {
		t.Helper()
		rec := doOwnerGet(s, knowledgePipelinePath)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			BeadSynthesizer struct {
				Publish config.BeadSynthPublishConfig `json:"publish"`
			} `json:"bead_synthesizer"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.BeadSynthesizer.Publish
	}
	put := func(p map[string]any) {
		t.Helper()
		rec := doPut(s, knowledgePipelinePath, map[string]any{"bead_synthesizer": map[string]any{"publish": p}})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
	}
	defaults := (config.BeadSynthPublishConfig{}).WithDefaults()
	if got := read(); got != defaults {
		t.Fatalf("GET defaults = %+v, want %+v", got, defaults)
	}
	put(map[string]any{"enabled": true, "repo": "org/repo", "path": ".hive/knowledge/", "base_branch": "v5", "mode": "commit", "cadence": "daily"})
	want := config.BeadSynthPublishConfig{Enabled: true, Repo: "org/repo", Path: ".hive/knowledge/", BaseBranch: "v5", Mode: "commit", Cadence: "daily"}
	if got := read(); got != want || s.deps.Config.Knowledge.BeadSynthesizer.Publish != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	put(map[string]any{"mode": "pr"})
	want.Mode = "pr"
	if got := read(); got != want {
		t.Fatalf("partial PUT changed unrelated fields: %+v", got)
	}
	put(map[string]any{"enabled": false, "repo": "", "path": "", "base_branch": "", "mode": "", "cadence": ""})
	if got := read(); got != defaults || s.deps.Config.Knowledge.BeadSynthesizer.Publish != defaults {
		t.Fatalf("cleared settings = %+v, want %+v", got, defaults)
	}
}

func TestBeadSynthPublishPipelineRejectsInvalid(t *testing.T) {
	s := covApiServer(t)
	before := s.deps.Config.Knowledge.BeadSynthesizer.Publish
	for _, bad := range []map[string]any{
		{"mode": "push"}, {"cadence": "weekly"}, {"path": "/etc"},
		{"path": "../knowledge"}, {"path": "docs/../knowledge"}, {"path": `C:\knowledge`},
	} {
		// A rejected request must not even apply a valid sibling field.
		bad["enabled"] = true
		rec := doPut(s, knowledgePipelinePath, map[string]any{"bead_synthesizer": map[string]any{"publish": bad}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: got %d %s, want 400", bad, rec.Code, rec.Body.String())
		}
		if got := s.deps.Config.Knowledge.BeadSynthesizer.Publish; got != before {
			t.Fatalf("rejected PUT changed config: %+v", got)
		}
	}
}

func TestBeadSynthPublishPipelineOwnerOnly(t *testing.T) {
	s := covApiServer(t)
	before := s.deps.Config.Knowledge.BeadSynthesizer.Publish
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, knowledgePipelinePath, strings.NewReader(`{"bead_synthesizer":{"publish":{"enabled":true,"mode":"commit"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-Role", "read-write")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner %s = %d, want 403", method, rec.Code)
		}
	}
	if got := s.deps.Config.Knowledge.BeadSynthesizer.Publish; got != before {
		t.Fatalf("non-owner changed publish settings: %+v", got)
	}
}

func TestBeadSynthPublishPipelineValidatesOnlyCarriedKeys(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Knowledge.BeadSynthesizer.Publish.Mode = "legacy"
	rec := doPut(s, knowledgePipelinePath, map[string]any{"bead_synthesizer": map[string]any{"publish": map[string]any{"enabled": false}}})
	if rec.Code != http.StatusOK || s.deps.Config.Knowledge.BeadSynthesizer.Publish.Mode != "legacy" {
		t.Fatalf("unrelated legacy mode blocked or overwritten: %d %s", rec.Code, rec.Body.String())
	}
}
