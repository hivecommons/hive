package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doKnowledgeStatePut(s *Server, id, user string, owner bool, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/knowledge/entry/"+id+"/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if owner {
		markOwnerRequest(req)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func knowledgeStateAudit(s *Server, action string) []AuditEntry {
	var out []AuditEntry
	for _, e := range s.audit.Recent(0) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestKnowledgeEntryStateSetsStateAndAudits(t *testing.T) {
	s := knowledgeTOCServer(t)

	rec := doKnowledgeStatePut(s, "current-gotcha", "alice", true, `{"state":"deprecated","reason":"relay\nremoved in v5"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["id"] != "current-gotcha" || resp["channel"] != "vault" || resp["previous_state"] != "approved" || resp["state"] != "deprecated" || resp["fact"] == nil {
		t.Fatalf("response = %v", resp)
	}

	entries := knowledgeStateAudit(s, "knowledge_set_state")
	if len(entries) != 1 {
		t.Fatalf("knowledge_set_state audit entries = %+v", s.audit.Recent(0))
	}
	e := entries[0]
	for _, want := range []string{"id=current-gotcha", "channel=vault", "from=approved", "to=deprecated", "reason=relay removed in v5"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("audit detail %q missing %q", e.Detail, want)
		}
	}
	if e.User != "alice" {
		t.Errorf("audit user = %q, want alice", e.User)
	}

	_, toc := tocGet(t, s, "/api/knowledge/toc")
	if got := tocIDs(t, toc); strings.Contains(got, "current-gotcha") {
		t.Fatalf("deprecated entry still in default TOC: %s", got)
	}

	rec = doKnowledgeStatePut(s, "current-gotcha", "alice", true, `{"state":"approved"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d body=%s", rec.Code, rec.Body.String())
	}
	_, toc = tocGet(t, s, "/api/knowledge/toc")
	if got := tocIDs(t, toc); !strings.Contains(got, "current-gotcha") {
		t.Fatalf("restored entry missing from TOC: %s", got)
	}
}

func TestKnowledgeEntryStateSupersedes(t *testing.T) {
	s := knowledgeTOCServer(t)

	rec := doKnowledgeStatePut(s, "old-way", "bob", true, `{"state":"superseded","superseded_by":"current-gotcha","reason":"replaced"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["previous_state"] != "deprecated" || resp["state"] != "superseded" || resp["superseded_by"] != "current-gotcha" {
		t.Fatalf("response = %v", resp)
	}

	entries := knowledgeStateAudit(s, "knowledge_supersede")
	if len(entries) != 1 || entries[0].User != "bob" ||
		!strings.Contains(entries[0].Detail, "superseded_by=current-gotcha") || !strings.Contains(entries[0].Detail, "reason=replaced") {
		t.Fatalf("knowledge_supersede audit = %+v", s.audit.Recent(0))
	}

	code, entry := tocGet(t, s, "/api/knowledge/entry/old-way?include_states=all")
	if code != http.StatusOK || entry["status"] != "superseded" {
		t.Fatalf("entry after supersede = %d %v", code, entry)
	}
}

func TestKnowledgeEntryStateRejectsBadRequests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    string
		owner bool
		body  string
		want  int
	}{
		{"non-owner", "current-gotcha", false, `{"state":"deprecated"}`, http.StatusForbidden},
		{"bad json", "current-gotcha", true, `{`, http.StatusBadRequest},
		{"invalid state", "current-gotcha", true, `{"state":"archived"}`, http.StatusBadRequest},
		{"superseded without target", "current-gotcha", true, `{"state":"superseded"}`, http.StatusBadRequest},
		{"target without superseded", "current-gotcha", true, `{"state":"approved","superseded_by":"old-way"}`, http.StatusBadRequest},
		{"self supersede", "old-way", true, `{"state":"superseded","superseded_by":"old-way"}`, http.StatusBadRequest},
		{"missing replacement", "old-way", true, `{"state":"superseded","superseded_by":"no-such-entry"}`, http.StatusBadRequest},
		{"missing entry", "no-such-entry", true, `{"state":"deprecated"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := knowledgeTOCServer(t)
			rec := doKnowledgeStatePut(s, tc.id, "", tc.owner, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if n := len(knowledgeStateAudit(s, "knowledge_set_state")) + len(knowledgeStateAudit(s, "knowledge_supersede")); n != 0 {
				t.Fatalf("rejected request was audited: %+v", s.audit.Recent(0))
			}
		})
	}
}
