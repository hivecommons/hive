package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// gitWorkflowBody is longer than the 200-rune snippet a knowledge listing
// carries, and only says "rebase" after that snippet ends.
var gitWorkflowBody = strings.Repeat("This page describes how changes reach the main branch in this repository. ", 4) +
	"Always rebase onto main before pushing; never merge main into a pull request branch."

func taskMCPKnowledge(t *testing.T, s *Server, query string) []taskmcp.KnowledgeItem {
	t.Helper()
	args, err := json.Marshal(map[string]any{"task_id": "task-1", "repo": "owner/repo", "query": query})
	if err != nil {
		t.Fatal(err)
	}
	rec := callMCP(t, s, taskmcp.ToolKnowledge, string(args))
	var env struct {
		Data taskmcp.KnowledgeData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &env); err != nil {
		t.Fatal(err)
	}
	return env.Data.Items
}

func taskMCPRepoConventions(t *testing.T, s *Server) taskmcp.RepoConventionsData {
	t.Helper()
	rec := callMCP(t, s, taskmcp.ToolRepoConventions, `{"task_id":"task-1","repo":"owner/repo"}`)
	var env struct {
		Data taskmcp.RepoConventionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

// writeVaultPage writes one raw markdown page into a vault and reindexes it.
func writeVaultPage(t *testing.T, vault *knowledge.FileStore, name, page string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vault.RootDir(), name), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	vault.Reindex()
}

// shadowedKnowledgeServer puts shadow in the first vault and page in a second
// vault, both under the slug "git". The listing returns both copies, but a
// re-read by slug reaches the first vault's copy, which the test confirms.
func shadowedKnowledgeServer(t *testing.T, shadow, shadowBody, page string) *Server {
	t.Helper()
	s := mcpPhase3Server(t)
	writeVaultPage(t, s.deps.Knowledge.FileStores()[0], "git.md", shadow)
	dir := filepath.Join(t.TempDir(), "second-vault")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.deps.Knowledge.ConnectVault(dir, "second-vault"); err != nil {
		t.Fatal(err)
	}
	writeVaultPage(t, s.deps.Knowledge.FileStores()[1], "git.md", page)
	reread := s.deps.Knowledge.FullFact(context.Background(), knowledge.Fact{Slug: "git", Layer: knowledge.LayerPersonal})
	if reread.Body != shadowBody {
		t.Fatalf("test setup: a re-read by slug returned %q, want the first vault's copy %q", reread.Body, shadowBody)
	}
	return s
}

func TestTaskMCPKnowledgeFindsMultiWordQuery(t *testing.T) {
	s := mcpPhase3Server(t)
	writeKnowledgeFact(t, s, "git.md", "Git workflow", gitWorkflowBody, []string{"repo:owner/repo"})
	for _, query := range []string{"git rebase workflow", "how changes reach main"} {
		items := taskMCPKnowledge(t, s, query)
		if len(items) != 1 || items[0].Slug != "git" {
			t.Errorf("knowledge(%q) = %#v, want the Git workflow fact", query, items)
		}
	}
}

func TestTaskMCPKnowledgeFindsWordPastTheSnippet(t *testing.T) {
	s := mcpPhase3Server(t)
	writeKnowledgeFact(t, s, "git.md", "Git workflow", gitWorkflowBody, []string{"repo:owner/repo"})
	if at := utf8.RuneCountInString(gitWorkflowBody[:strings.Index(gitWorkflowBody, "rebase")]); at <= 200 {
		t.Fatalf("test setup: \"rebase\" is at rune %d, want it past the 200-rune snippet", at)
	}
	items := taskMCPKnowledge(t, s, "rebase")
	if len(items) != 1 || items[0].Slug != "git" {
		t.Fatalf("knowledge(\"rebase\") = %#v, want the Git workflow fact", items)
	}
}

func TestTaskMCPKnowledgeServesFullBody(t *testing.T) {
	s := mcpPhase3Server(t)
	writeKnowledgeFact(t, s, "git.md", "Git workflow", gitWorkflowBody, []string{"repo:owner/repo"})
	for _, query := range []string{"", "Git workflow", "rebase"} {
		items := taskMCPKnowledge(t, s, query)
		if len(items) != 1 {
			t.Errorf("knowledge(%q) returned %d items, want 1", query, len(items))
			continue
		}
		if got := items[0].Data.Body; got != gitWorkflowBody {
			t.Errorf("knowledge(%q) served %d of %d body bytes: %q", query, len(got), len(gitWorkflowBody), got)
		}
	}
}

func TestTaskMCPKnowledgeCutsOversizedBodyWithMarker(t *testing.T) {
	tail := func(s string) string {
		r := []rune(s)
		return string(r[max(0, len(r)-12):])
	}
	// Three-byte runes at three alignments, so the cut would split a rune in
	// at least two of the cases if it were made on a plain byte count.
	for _, lead := range []string{"", "a", "ab"} {
		s := mcpPhase3Server(t)
		body := lead + strings.Repeat("界", taskmcp.MaxTextBytes)
		writeKnowledgeFact(t, s, "big.md", "Big page", body, []string{"repo:owner/repo"})
		items := taskMCPKnowledge(t, s, "")
		if len(items) != 1 {
			t.Fatalf("lead %q: knowledge returned %d items, want 1", lead, len(items))
		}
		got := items[0].Data.Body
		if len(got) > taskmcp.MaxTextBytes {
			t.Errorf("lead %q: served body is %d bytes, cap is %d", lead, len(got), taskmcp.MaxTextBytes)
		}
		at := strings.LastIndex(got, "\n\n[... ")
		if at < 0 {
			t.Fatalf("lead %q: served body has no truncation marker, ends %q", lead, tail(got))
		}
		kept, marker := got[:at], got[at:]
		if !strings.HasPrefix(body, kept) || len(kept) < taskmcp.MaxTextBytes-200 {
			t.Errorf("lead %q: kept %d bytes that are not the start of the page (cap %d), ends %q", lead, len(kept), taskmcp.MaxTextBytes, tail(kept))
		}
		for _, want := range []string{"truncated by hive", strconv.Itoa(len(body)), strconv.Itoa(taskmcp.MaxTextBytes)} {
			if !strings.Contains(marker, want) {
				t.Errorf("lead %q: marker %q does not say %q", lead, marker, want)
			}
		}
	}
}

// The knowledge store's search scores a page as a whole, so one mention in a
// long page falls under its floor. The opening text still names the word, and
// a lookup that found the page by it before must keep finding it.
func TestTaskMCPKnowledgeKeepsLiteralMatchStoreSearchMisses(t *testing.T) {
	s := mcpPhase3Server(t)
	filler := strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu")
	var body strings.Builder
	body.WriteString("Always rebase before pushing.")
	for i := 0; i < 3000; i++ {
		body.WriteString(" " + filler[i%len(filler)])
	}
	writeKnowledgeFact(t, s, "handbook.md", "Contributor handbook", body.String(), []string{"repo:owner/repo"})
	items := taskMCPKnowledge(t, s, "pushing")
	if len(items) != 1 || items[0].Slug != "handbook" {
		t.Fatalf("knowledge(\"pushing\") = %d items, want the handbook whose opening text says it", len(items))
	}
}

func TestTaskMCPKnowledgeQueryNeverServesOtherRepoFact(t *testing.T) {
	s := mcpPhase3Server(t)
	writeKnowledgeFact(t, s, "other.md", "Rebase rules", "Rebase on the staging branch. Rebase daily.", []string{"repo:other/repo"})
	writeKnowledgeFact(t, s, "prefix.md", "Rebase rules for forks", "Rebase on the staging branch. Rebase daily.", []string{"repo:owner/repo2"})
	writeKnowledgeFact(t, s, "owner.md", "Rebase workflow", "Rebase onto main before pushing.", []string{"repo:owner/repo"})

	stored := map[string]bool{}
	for _, f := range s.deps.Knowledge.SearchAllWithVaults(context.Background(), "rebase staging", "", 100) {
		stored[f.Slug] = true
	}
	if !stored["other"] || !stored["prefix"] {
		t.Fatalf("test setup: the store's search for \"rebase staging\" returned %v, want the other repositories' facts in it", stored)
	}
	for _, query := range []string{"rebase", "rebase staging", "staging branch", "rules"} {
		for _, item := range taskMCPKnowledge(t, s, query) {
			if item.Slug != "owner" {
				t.Errorf("knowledge(%q) served %q (tags %v) to a task in owner/repo", query, item.Slug, item.Tags)
			}
		}
	}
	if items := taskMCPKnowledge(t, s, "rebase"); len(items) != 1 {
		t.Errorf("knowledge(\"rebase\") returned %d items, want the task repository's one", len(items))
	}
}

func TestTaskMCPKnowledgeNeverServesUnapprovedFact(t *testing.T) {
	s := mcpPhase3Server(t)
	vault := s.deps.Knowledge.FileStores()[0]
	const tags = "tags: [kind:conventions, repo:owner/repo]\n"
	writeVaultPage(t, vault, "approved.md", "---\ntitle: Rebase rules\n"+tags+"---\n\nAlways rebase onto main before pushing.\n")
	for name, state := range map[string]string{
		"draft":      "state: draft\n",
		"deprecated": "state: deprecated\n",
		"superseded": "superseded_by: approved\n",
		"legacy":     "status: deprecated\n",
	} {
		writeVaultPage(t, vault, name+".md", "---\ntitle: Rebase rules ("+name+")\n"+state+tags+"---\n\nRebase onto main once a week.\n")
	}

	const query = "rebase onto main"
	if hits := s.deps.Knowledge.SearchAllWithVaults(context.Background(), query, "", 100); len(hits) != 5 {
		t.Fatalf("test setup: the store's search for %q returned %d of the 5 facts", query, len(hits))
	}
	for _, q := range []string{"", "rebase", query, "week"} {
		for _, item := range taskMCPKnowledge(t, s, q) {
			if item.Slug != "approved" {
				t.Errorf("knowledge(%q) served the %s fact %q", q, item.Slug, item.Data.Title)
			}
		}
	}
	if items := taskMCPKnowledge(t, s, "rebase"); len(items) != 1 {
		t.Errorf("knowledge(\"rebase\") returned %d items, want the approved one", len(items))
	}
	if data := taskMCPRepoConventions(t, s); len(data.Conventions) != 1 || data.Conventions[0].Slug != "approved" {
		t.Errorf("repo conventions = %#v, want the approved fact only", data.Conventions)
	}
}

func TestTaskMCPKnowledgeRepoRuleRunsBeforeLimitOnSearchHits(t *testing.T) {
	s := mcpPhase3Server(t)
	s.deps.Config.Hub.TaskMCPKnowledgeLimit = 1
	writeKnowledgeFact(t, s, "other.md", "Rebase before pushing", "Rebase before pushing. Rebase before pushing.", []string{"repo:other/repo"})
	writeKnowledgeFact(t, s, "prefix.md", "Rebase before pushing to forks", "Rebase before pushing. Rebase before pushing.", []string{"repo:owner/repo2"})
	writeKnowledgeFact(t, s, "git.md", "Git workflow", gitWorkflowBody, []string{"repo:owner/repo"})

	const query = "rebase before main"
	hits := s.deps.Knowledge.SearchAllWithVaults(context.Background(), query, "", 100)
	if len(hits) != 3 || hits[len(hits)-1].Slug != "git" {
		t.Fatalf("test setup: the store ranks %d hits and the task repository's fact is not last", len(hits))
	}
	items := taskMCPKnowledge(t, s, query)
	if len(items) != 1 || items[0].Slug != "git" {
		t.Fatalf("knowledge(%q) with a limit of 1 = %#v, want the task repository's fact", query, items)
	}
}

func TestTaskMCPKnowledgeRereadStaysInScope(t *testing.T) {
	const page = "---\ntitle: Git workflow\ntags: [repo:owner/repo]\n---\n\nAlways rebase onto main before pushing.\n"
	for name, shadow := range map[string]string{
		"copy tagged for another repository": "---\ntitle: Git workflow\ntags: [repo:other/repo]\n---\n\nRebase onto the private staging branch.\n",
		"draft copy":                         "---\ntitle: Git workflow\nstate: draft\ntags: [repo:owner/repo]\n---\n\nRebase onto the private staging branch.\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := shadowedKnowledgeServer(t, shadow, "Rebase onto the private staging branch.", page)
			for _, query := range []string{"", "rebase"} {
				items := taskMCPKnowledge(t, s, query)
				if len(items) != 1 {
					t.Fatalf("knowledge(%q) returned %d items, want the task repository's approved fact", query, len(items))
				}
				if got := items[0].Data.Body; got != "Always rebase onto main before pushing." {
					t.Errorf("knowledge(%q) served %q, want the listed fact's own body", query, got)
				}
				if len(items[0].Tags) != 1 || items[0].Tags[0] != "repo:owner/repo" {
					t.Errorf("knowledge(%q) served tags %v, want the listed fact's own", query, items[0].Tags)
				}
			}
		})
	}
}

func TestTaskMCPRepoConventionsFindsTypedFact(t *testing.T) {
	s := mcpPhase3Server(t)
	writeVaultPage(t, s.deps.Knowledge.FileStores()[0], "house-rules.md",
		"---\ntitle: House rules\ntype: conventions\ntags: [repo:owner/repo]\n---\n\nUse table-driven tests. Wrap errors with %w. Never push straight to main.\n")
	data := taskMCPRepoConventions(t, s)
	if data.Source != "human" || len(data.Conventions) != 1 || data.Conventions[0].Data.Title != "House rules" {
		t.Fatalf("repo conventions = %#v, want the fact typed conventions", data)
	}
}

func TestTaskMCPRepoConventionsLimitCountsConventionsOnly(t *testing.T) {
	s := mcpPhase3Server(t)
	s.deps.Config.Hub.TaskMCPKnowledgeLimit = 1
	writeKnowledgeFact(t, s, "architecture.md", "Architecture notes", "The service is split into three packages.", []string{"repo:owner/repo"})
	writeVaultPage(t, s.deps.Knowledge.FileStores()[0], "house-rules.md",
		"---\ntitle: House rules\ntype: conventions\ntags: [repo:owner/repo]\n---\n\nUse table-driven tests.\n")
	data := taskMCPRepoConventions(t, s)
	if len(data.Conventions) != 1 || data.Conventions[0].Data.Title != "House rules" {
		t.Fatalf("repo conventions with a limit of 1 = %#v, want the one convention fact", data)
	}
}

func TestTaskMCPRepoConventionsRereadStaysInScope(t *testing.T) {
	const page = "---\ntitle: Repository conventions\ntags: [kind:conventions, repo:owner/repo]\n---\n\nAlways run go test.\n"
	for name, shadow := range map[string]string{
		"copy tagged for another repository": "---\ntitle: Repository conventions\ntags: [kind:conventions, repo:other/repo]\n---\n\nDeploy from the private staging branch.\n",
		"draft copy":                         "---\ntitle: Repository conventions\nstate: draft\ntags: [kind:conventions, repo:owner/repo]\n---\n\nDeploy from the private staging branch.\n",
		"copy that is not a convention":      "---\ntitle: Meeting notes\ntags: [repo:owner/repo]\n---\n\nDeploy from the private staging branch.\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := shadowedKnowledgeServer(t, shadow, "Deploy from the private staging branch.", page)
			data := taskMCPRepoConventions(t, s)
			if len(data.Conventions) != 1 {
				t.Fatalf("repo conventions = %#v, want the task repository's approved convention", data)
			}
			if got := data.Conventions[0].Data; got.Title != "Repository conventions" || got.Body != "Always run go test." {
				t.Errorf("repo conventions served %#v, want the listed fact's own title and body", got)
			}
		})
	}
}
