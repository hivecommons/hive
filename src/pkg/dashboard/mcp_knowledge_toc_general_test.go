package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// publicKnowledgeTOC calls knowledge_toc and returns the total and the type
// each listed entry is shown with.
func publicKnowledgeTOC(t *testing.T, s *Server, args string) (int, map[string]string) {
	t.Helper()
	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_toc","arguments":`+args+`}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("knowledge_toc %s: HTTP %d body=%q", args, rec.Code, rec.Body.String())
	}
	text, isErr := rpcToolText(t, decodeRPC(t, rec))
	if isErr {
		t.Fatalf("knowledge_toc %s: tool error %s", args, text)
	}
	var toc struct {
		Total   int `json:"total"`
		Entries []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(text), &toc); err != nil {
		t.Fatalf("knowledge_toc %s: %v in %s", args, err, text)
	}
	types := map[string]string{}
	for _, e := range toc.Entries {
		types[e.ID] = e.Type
	}
	return toc.Total, types
}

// A fact with no type is listed with type "general". Asking the table of
// contents for that type compared "general" with the empty type and returned
// nothing, so the type the listing showed could not be used to filter it.
func TestPublicKnowledgeTOCGeneralFilterFindsUntypedFacts(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	// A wiki layer page with no type: file stores give every page one.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{
				map[string]interface{}{"slug": "house-style", "title": "House style", "snippet": "Write short sentences."},
				map[string]interface{}{"slug": "nil-map", "title": "Nil map", "type": "gotcha", "snippet": "Initialise maps."},
			},
			"total": 2,
		})
	})
	wiki := httptest.NewServer(mux)
	t.Cleanup(wiki.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	s.RegisterAPI(testDeps(t))
	s.deps.Knowledge = knowledge.NewKnowledgeAPI(
		[]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: wiki.URL}},
		knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)

	total, types := publicKnowledgeTOC(t, s, `{}`)
	if total != 2 || types["house-style"] != "general" || types["nil-map"] != "gotcha" {
		t.Fatalf("fixture: unfiltered toc = %d entries %v, want house-style listed as general and nil-map as gotcha", total, types)
	}

	total, types = publicKnowledgeTOC(t, s, `{"type":"general"}`)
	if total != 1 || types["house-style"] != "general" {
		t.Errorf("toc type=general = %d entries %v, want the one fact the toc lists as general", total, types)
	}

	// The other types are still exact.
	total, types = publicKnowledgeTOC(t, s, `{"type":"gotcha"}`)
	if total != 1 || types["nil-map"] != "gotcha" {
		t.Errorf("toc type=gotcha = %d entries %v, want only nil-map", total, types)
	}
}
