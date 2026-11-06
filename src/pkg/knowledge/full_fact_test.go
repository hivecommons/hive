package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A slug can exist in two vaults, and FullFact reads the first vault that has
// it. The fact it returns must carry that page's lifecycle state, not the
// state of the listing the slug came from.
func TestFullFactCarriesLifecycleOfThePageItRead(t *testing.T) {
	for _, tc := range []struct {
		name, frontmatter string
		want              LifecycleState
	}{
		{"draft", "state: draft\n", StateDraft},
		{"deprecated", "state: deprecated\n", StateDeprecated},
		{"superseded", "superseded_by: newer-note\n", StateSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true, Engine: "file"}, logger: apiTestLogger()}
			for _, v := range []struct{ name, page string }{
				{"first", "---\ntitle: Note\n" + tc.frontmatter + "---\n\nWithheld copy.\n"},
				{"second", "---\ntitle: Note\n---\n\nApproved copy.\n"},
			} {
				dir := filepath.Join(t.TempDir(), v.name)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte(v.page), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := api.ConnectVault(dir, v.name); err != nil {
					t.Fatal(err)
				}
			}
			listed := api.FileStores()[1].ListPages("")
			if len(listed) != 1 || listed[0].EffectiveState() != StateApproved {
				t.Fatalf("test setup: the second vault lists %#v, want one approved fact", listed)
			}
			full := api.FullFact(context.Background(), listed[0])
			if full.Body != "Withheld copy." {
				t.Fatalf("test setup: FullFact read %q, want the first vault's copy", full.Body)
			}
			if got := full.EffectiveState(); got != tc.want {
				t.Fatalf("FullFact returned the %s page's body with state %q", tc.want, got)
			}
		})
	}
}

// A wiki page carries a status but no lifecycle state of its own. A fact that
// another store listed as approved must not keep that state when its body is
// read from a wiki page marked deprecated.
func TestFullFactFromWikiLayerFollowsThePageStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/note", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(pageResponse{Slug: "note", Title: "Note", Body: "Old advice.", Status: "deprecated"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	full := api.FullFact(context.Background(), Fact{Slug: "note", Layer: LayerProject, State: StateApproved})
	if full.Body != "Old advice." {
		t.Fatalf("test setup: FullFact read %q, want the wiki page", full.Body)
	}
	if got := full.EffectiveState(); got != StateDeprecated {
		t.Fatalf("FullFact returned the deprecated page's body with state %q", got)
	}
}

// A same-slug page in a vault of another layer must not supply the body of a
// fact listed under a different layer.
func TestFullFactIgnoresVaultPageOfAnotherLayer(t *testing.T) {
	dir := t.TempDir()
	page := "---\ntitle: Note\nlayer: project\n---\n\nProject copy.\n"
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true, Engine: "file"}, logger: apiTestLogger()}
	if err := api.ConnectVault(dir, "vault"); err != nil {
		t.Fatal(err)
	}
	if got := api.FileStores()[0].ListPages(""); len(got) != 1 || got[0].Layer != LayerProject {
		t.Fatalf("test setup: vault lists %#v, want one project-layer fact", got)
	}

	full := api.FullFact(context.Background(), Fact{Slug: "note", Layer: LayerPersonal, Body: "snippet"})
	if full.Body != "snippet" || full.Layer != LayerPersonal {
		t.Fatalf("FullFact took the %s vault page's body %q under the personal layer", LayerProject, full.Body)
	}

	full = api.FullFact(context.Background(), Fact{Slug: "note", Layer: LayerProject})
	if full.Body != "Project copy." || full.Layer != LayerProject {
		t.Fatalf("FullFact = %q in layer %q, want the project vault page", full.Body, full.Layer)
	}
}
