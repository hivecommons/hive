package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const testWritingGuide = "Short sentences.\nEvidence under details."

func TestWritingGuideConfigAPIRoundTrip(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.WritingGuide = "initial guide"

	cfgRec := doGet(s, "/api/config")
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("/api/config status = %d, want %d", cfgRec.Code, http.StatusOK)
	}
	var cfgPayload map[string]any
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfgPayload); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	if got := cfgPayload["writing_guide"]; got != "initial guide" {
		t.Fatalf("/api/config writing_guide = %v, want initial guide", got)
	}

	govRec := doOwnerGet(s, "/api/config/governor")
	if govRec.Code != http.StatusOK {
		t.Fatalf("/api/config/governor status = %d, want %d", govRec.Code, http.StatusOK)
	}
	var govPayload map[string]any
	if err := json.Unmarshal(govRec.Body.Bytes(), &govPayload); err != nil {
		t.Fatalf("decode /api/config/governor: %v", err)
	}
	if got := govPayload["writingGuide"]; got != "initial guide" {
		t.Fatalf("/api/config/governor writingGuide = %v, want initial guide", got)
	}

	rec := doPut(s, "/api/config/governor/labels", map[string]any{"writing_guide": testWritingGuide})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT writing_guide status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := deps.Config.Project.WritingGuide; got != testWritingGuide {
		t.Fatalf("Project.WritingGuide = %q, want %q", got, testWritingGuide)
	}
}

func TestWritingGuideSavePersistsToConfigFile(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.SourcePath = filepath.Join(dir, "hive.yaml")

	rec := doPut(s, "/api/config/governor/labels", map[string]any{"writing_guide": testWritingGuide})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT writing_guide status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	data, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(data), "writing_guide:") || !strings.Contains(string(data), "Short sentences.") {
		t.Fatalf("saved config does not contain writing_guide:\n%s", data)
	}
}

func TestWritingGuideRejectsOversize(t *testing.T) {
	s, _ := apiServer(t)
	tooLarge := strings.Repeat("x", config.MaxWritingGuideBytes+1)

	rec := doPut(s, "/api/config/governor/labels", map[string]any{"writing_guide": tooLarge})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize writing_guide status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWritingGuideEditRequiresOwner(t *testing.T) {
	s, deps := apiServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/labels", strings.NewReader(`{"writing_guide":"blocked"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-write writing_guide edit status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := deps.Config.Project.WritingGuide; got != "" {
		t.Fatalf("Project.WritingGuide changed despite failed role gate: %q", got)
	}
}

func TestWritingGuideSettingsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="cfg-writing-guide"`,
		`placeholder="No writing guide is set."`,
		`markDirty`,
		`data-arg1="writing_guide"`,
		`data-arg0="labels"`,
		`Lands as <code>${'${WRITING_GUIDE}'}</code> before the body template`,
		`review-swarm comments`,
		`https://github.com/hivecommons/hive/blob/v5/src/docs/agent-configuration.md#writing-guide-how-issues-and-prs-should-read-projectwriting_guide`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing writing guide wiring %q", snippet)
		}
	}
}
