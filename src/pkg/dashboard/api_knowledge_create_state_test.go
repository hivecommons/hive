package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// TestHandleKnowledgeCreate_StateAndDrainProvenance covers the create-time
// lifecycle state and the CLI-memory provenance guard (#17056): a fact
// claiming drain provenance enters only as a draft with the drain's own
// tags, a draft lands in a local channel with `state: draft`, and a draft
// never overwrites an existing entry.
func TestHandleKnowledgeCreate_StateAndDrainProvenance(t *testing.T) {
	s, deps, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	vault := t.TempDir()
	if err := deps.Knowledge.ConnectVault(vault, "drafts"); err != nil {
		t.Fatal(err)
	}
	drained := []string{"cli-memory", "agent:scanner", "cli:codex", "memhash:0123456789abcdef", "repo:hivecommons/hive"}

	rejected := []struct {
		name string
		req  knowledge.CreateFactRequest
		code int
	}{
		{"deprecated is not a create state", knowledge.CreateFactRequest{Title: "t", Body: "b", State: "deprecated"}, http.StatusBadRequest},
		{"unknown state", knowledge.CreateFactRequest{Title: "t", Body: "b", State: "live"}, http.StatusBadRequest},
		{"drain tag without state", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "drafts", Tags: drained}, http.StatusBadRequest},
		{"drain tag approved", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "drafts", Tags: drained, State: "approved"}, http.StatusBadRequest},
		{"drain tag, case folded", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "drafts", Tags: []string{"CLI-Memory"}, State: "approved"}, http.StatusBadRequest},
		{"drain tag with a foreign tag", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "drafts", Tags: append([]string{"security"}, drained...), State: "draft"}, http.StatusBadRequest},
		{"drain tag with a path-like agent", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "drafts", Tags: []string{"cli-memory", "agent:../x"}, State: "draft"}, http.StatusBadRequest},
		{"draft into a layer with no channel", knowledge.CreateFactRequest{Title: "t", Body: "b", Layer: "project", State: "draft"}, http.StatusBadRequest},
	}
	for _, tc := range rejected {
		if rec := doPost(s, "/api/knowledge/create", tc.req); rec.Code != tc.code {
			t.Errorf("%s: status = %d (%s), want %d", tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.code)
		}
	}
	if entries, _ := os.ReadDir(vault); len(entries) != 0 {
		t.Fatalf("rejected requests wrote %d files into the channel", len(entries))
	}

	req := knowledge.CreateFactRequest{Title: "Codex memory note", Body: "Run the failing job log first.", Layer: "drafts", Tags: drained, State: "draft"}
	rec := doPost(s, "/api/knowledge/create", req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"draft"`) {
		t.Fatalf("draft create: %d %s", rec.Code, rec.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(vault, "*.md"))
	if len(files) != 1 {
		t.Fatalf("channel has %v, want one entry", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nstate: draft\n") {
		t.Fatalf("entry is not a draft:\n%s", data)
	}
	store := deps.Knowledge.GetVaultStore(vault)
	store.Reindex()
	if got := knowledge.FilterByLifecycle(store.ListPages(""), nil); len(got) != 0 {
		t.Fatalf("a created draft passed the approved-only filter: %+v", got)
	}

	// Same title again: never overwrite, whatever the new state.
	if rec := doPost(s, "/api/knowledge/create", req); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate draft: status = %d, want 409", rec.Code)
	}

	// No state keeps the old default path (the remote project layer).
	if rec := doPost(s, "/api/knowledge/create", knowledge.CreateFactRequest{Title: "plain", Body: "b"}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"approved"`) {
		t.Fatalf("default create: %d %s", rec.Code, rec.Body.String())
	}
}
