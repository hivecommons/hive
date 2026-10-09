package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func scopedKnowledgeTOCServer(t *testing.T) *Server {
	t.Helper()
	s := knowledgeTOCServer(t)
	s.deps.Config.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{
		"scanner": {Layers: []string{"project"}, Repos: []string{"hivecommons/hive"}, IncludeStates: []string{"draft"}},
	}
	return s
}

func TestKnowledgeTOCAppliesAgentScope(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	for _, tc := range [][2]string{
		{"/api/knowledge/toc?agent=scanner", "current-gotcha"},
		// The request asks for more layers, repos and states than the agent
		// scope allows; it must not widen it.
		{"/api/knowledge/toc?agent=scanner&layers=org,project&repos=someone/else,hivecommons/hive&include_states=all", "current-gotcha,draft-idea"},
		{"/api/knowledge/toc?agent=scanner&types=pattern", ""},
		{"/api/knowledge/toc?agent=reviewer", "current-gotcha,org-wide-pattern,other-repo"},
		{"/api/knowledge/toc", "current-gotcha,org-wide-pattern,other-repo"},
	} {
		target, want := tc[0], tc[1]
		code, body := tocGet(t, s, target)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d", target, code)
		}
		if got := tocIDs(t, body); got != want {
			t.Errorf("%s ids = %q, want %q", target, got, want)
		}
	}
}

func TestKnowledgeEntryAppliesAgentScope(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/api/knowledge/entry/current-gotcha?agent=scanner", http.StatusOK},
		{"/api/knowledge/entry/org-wide-pattern?agent=scanner", http.StatusNotFound},
		{"/api/knowledge/entry/org-wide-pattern?agent=scanner&layers=org", http.StatusNotFound},
		{"/api/knowledge/entry/other-repo?agent=scanner&repos=someone/else", http.StatusNotFound},
		{"/api/knowledge/entry/draft-idea?agent=scanner", http.StatusNotFound},
		{"/api/knowledge/entry/draft-idea?agent=scanner&include_states=draft", http.StatusOK},
		{"/api/knowledge/entry/old-way?agent=scanner&include_states=all", http.StatusNotFound},
		{"/api/knowledge/entry/old-way?agent=reviewer&include_states=deprecated", http.StatusOK},
	} {
		if code, _ := tocGet(t, s, tc.target); code != tc.want {
			t.Errorf("%s status = %d, want %d", tc.target, code, tc.want)
		}
	}
}

const kasGoodPut = `{"agent_scopes":{"scanner":{"layers":["project"," "],"repos":["hivecommons/hive"],"include_states":["draft"]},"reviewer":{}}}`

func TestKnowledgeAgentScopes_PutThenGetAndAudit(t *testing.T) {
	s := knowledgeTOCServer(t)
	if rec := doPutRaw(s, "/api/config/knowledge/agent-scopes", kasGoodPut); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	sc, ok := s.deps.Config.KnowledgeAgentScope("scanner")
	if !ok || strings.Join(sc.Layers, ",") != "project" || strings.Join(sc.IncludeStates, ",") != "draft" {
		t.Fatalf("config scope = %+v, %v", sc, ok)
	}

	rec := doOwnerGet(s, "/api/config/knowledge/agent-scopes")
	var got struct {
		AgentScopes map[string]config.KnowledgeAgentScope `json:"agent_scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.AgentScopes) != 2 || strings.Join(got.AgentScopes["scanner"].Repos, ",") != "hivecommons/hive" {
		t.Fatalf("agent_scopes = %+v", got.AgentScopes)
	}

	entries := knowledgeStateAudit(s, "config_knowledge_agent_scopes")
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "agents=reviewer,scanner") {
		t.Fatalf("audit entries = %+v", entries)
	}

	if _, body := tocGet(t, s, "/api/knowledge/toc?agent=scanner"); tocIDs(t, body) != "current-gotcha" {
		t.Fatalf("saved scope not applied: %v", body)
	}

	if rec := doPutRaw(s, "/api/config/knowledge/agent-scopes", `{"agent_scopes":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Knowledge.AgentScopes != nil {
		t.Fatalf("cleared scopes = %+v, want nil", s.deps.Config.Knowledge.AgentScopes)
	}
}

func TestKnowledgeAgentScopes_PutRequiresOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/knowledge/agent-scopes", kasGoodPut); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT = %d, want 403", rec.Code)
	}
	if s.deps.Config.Knowledge.AgentScopes != nil {
		t.Fatal("non-owner PUT changed the config")
	}
}

func TestKnowledgeAgentScopes_PutRejectsInvalid(t *testing.T) {
	s := covApiServer(t)
	cases := map[string]string{
		"unknown layer": `{"agent_scopes":{"scanner":{"layers":["galaxy"]}}}`,
		"unknown state": `{"agent_scopes":{"scanner":{"include_states":["verified"]}}}`,
		"unknown field": `{"agent_scopes":{"scanner":{"work_items":["1"]}}}`,
		"blank agent":   `{"agent_scopes":{" ":{}}}`,
		"missing map":   `{}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := doPutRaw(s, "/api/config/knowledge/agent-scopes", raw); rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400 body=%q", rec.Code, rec.Body.String())
			}
			if s.deps.Config.Knowledge.AgentScopes != nil {
				t.Fatal("rejected PUT changed the config")
			}
		})
	}
}
