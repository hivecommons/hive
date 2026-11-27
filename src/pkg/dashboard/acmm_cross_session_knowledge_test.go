package dashboard

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func crossSessionKnowledgeCriterion(t *testing.T) *ACMMCriterion {
	t.Helper()
	for i := range universalCriteria {
		if universalCriteria[i].ID == acmmCrossSessionKnowledgeID {
			return &universalCriteria[i]
		}
	}
	t.Fatalf("%s missing from universalCriteria", acmmCrossSessionKnowledgeID)
	return nil
}

// The zero-config knowledge path Hive seeds at boot must pass the
// criterion (#20128).
func TestCrossSessionKnowledgeCreditsDocsKnowledge(t *testing.T) {
	c := crossSessionKnowledgeCriterion(t)
	for _, want := range []string{"docs/knowledge/", "src/docs/knowledge/"} {
		found := false
		for _, p := range c.Patterns {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("patterns %v missing %q", c.Patterns, want)
		}
	}
}

// Every directory the criterion credits must hold facts Hive's file store
// can index when a store is built on it (#20128).
func TestCrossSessionKnowledgeDirectoryPatternsAreIndexable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, p := range crossSessionKnowledgeCriterion(t).Patterns {
		if !strings.HasSuffix(p, "/") {
			continue
		}
		t.Run(p, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), filepath.FromSlash(strings.TrimSuffix(p, "/")))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("---\ntitle: A\n---\n# A\nFact."), 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := knowledge.NewFileStore(dir, "test", logger)
			if err != nil {
				t.Fatal(err)
			}
			if got := store.Stats().TotalPages; got != 1 {
				t.Errorf("TotalPages = %d, want 1", got)
			}
		})
	}
}

func TestACMMIssueContent_CrossSessionKnowledgeNamesIndexedPath(t *testing.T) {
	c := crossSessionKnowledgeCriterion(t)
	_, body := acmmIssueContent(c)
	for _, want := range []string{
		"Create `docs/knowledge/`",
		"`knowledge.git_sources` is unset",
		"Markdown",
		"front matter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cross-session-knowledge gap issue missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Create one of the files listed above") {
		t.Errorf("cross-session-knowledge gap issue kept the generic how-to-fix text:\n%s", body)
	}
}
