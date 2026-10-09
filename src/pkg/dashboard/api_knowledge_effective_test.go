package dashboard

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// effectiveReasons maps excluded entry id to reason code.
func effectiveReasons(t *testing.T, body map[string]interface{}) map[string]string {
	t.Helper()
	out := map[string]string{}
	excluded, _ := body["excluded"].([]interface{})
	for _, e := range excluded {
		m := e.(map[string]interface{})
		out[m["id"].(string)] = m["reason"].(string)
	}
	return out
}

func effectiveIncluded(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	included, _ := body["included"].([]interface{})
	var ids []string
	for _, e := range included {
		ids = append(ids, e.(map[string]interface{})["id"].(string))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestKnowledgeEffectiveExplainsAgentScope(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	code, body := tocGet(t, s, "/api/knowledge/effective?agent=scanner")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := effectiveIncluded(t, body); got != "current-gotcha" {
		t.Errorf("included = %q, want current-gotcha", got)
	}
	want := map[string]string{
		"draft-idea":       "lifecycle",
		"old-way":          "lifecycle",
		"org-wide-pattern": "layer",
		"other-repo":       "repo",
	}
	got := effectiveReasons(t, body)
	if len(got) != len(want) {
		t.Fatalf("excluded = %v, want %v", got, want)
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Errorf("%s reason = %q, want %q", id, got[id], reason)
		}
	}
	if body["included_total"] != float64(1) || body["excluded_total"] != float64(4) || body["truncated"] != false {
		t.Errorf("counts = %v", body)
	}
	scope, _ := body["scope"].(map[string]interface{})
	if scope["configured"] != true || scope["agent"] != "scanner" {
		t.Errorf("scope = %v", scope)
	}

	// The request may admit drafts, which the scanner's scope also lists.
	_, body = tocGet(t, s, "/api/knowledge/effective?agent=scanner&include_states=all")
	if got := effectiveIncluded(t, body); got != "current-gotcha,draft-idea" {
		t.Errorf("include_states=all included = %q", got)
	}
	if r := effectiveReasons(t, body)["old-way"]; r != "lifecycle" {
		t.Errorf("old-way reason = %q, want lifecycle (not in the agent's include_states)", r)
	}
}

func TestKnowledgeEffectiveTypeAndTagReasons(t *testing.T) {
	s := knowledgeTOCServer(t)
	s.deps.Config.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{
		"typed":  {Types: []string{"gotcha"}},
		"tagged": {Tags: []string{"ci"}},
	}
	_, body := tocGet(t, s, "/api/knowledge/effective?agent=typed")
	if r := effectiveReasons(t, body)["org-wide-pattern"]; r != "type" {
		t.Errorf("typed: org-wide-pattern reason = %q, want type", r)
	}
	_, body = tocGet(t, s, "/api/knowledge/effective?agent=tagged")
	if r := effectiveReasons(t, body)["other-repo"]; r != "tag" {
		t.Errorf("tagged: other-repo reason = %q, want tag", r)
	}
	if got := effectiveIncluded(t, body); got != "current-gotcha" {
		t.Errorf("tagged included = %q", got)
	}
}

func TestKnowledgeEffectiveUnknownAgentIsUnrestricted(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	code, body := tocGet(t, s, "/api/knowledge/effective?agent=nobody")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := effectiveIncluded(t, body); got != "current-gotcha,org-wide-pattern,other-repo" {
		t.Errorf("included = %q", got)
	}
	got := effectiveReasons(t, body)
	if len(got) != 2 || got["draft-idea"] != "lifecycle" || got["old-way"] != "lifecycle" {
		t.Errorf("excluded = %v", got)
	}
	if scope, _ := body["scope"].(map[string]interface{}); scope["configured"] != false {
		t.Errorf("scope = %v", scope)
	}
}

func TestKnowledgeEffectiveLimitAndValidation(t *testing.T) {
	s := scopedKnowledgeTOCServer(t)
	_, body := tocGet(t, s, "/api/knowledge/effective?agent=scanner&limit=1")
	if body["truncated"] != true || body["excluded_returned"] != float64(1) || body["excluded_total"] != float64(4) {
		t.Errorf("limit=1 counts = %v", body)
	}
	for _, target := range []string{
		"/api/knowledge/effective",
		"/api/knowledge/effective?agent=scanner&include_states=bogus",
	} {
		if code, _ := tocGet(t, s, target); code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, code)
		}
	}
}
