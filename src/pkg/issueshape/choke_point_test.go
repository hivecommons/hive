package issueshape_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/hotspot"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

// hiveTitleRE is the shape every hive-filed issue title has: the filing
// agent in brackets, then a space, then the subject.
var hiveTitleRE = regexp.MustCompile(`^\[[a-z0-9][a-z0-9_.-]*\] \S`)

// Every Go file that creates a GitHub issue directly — a go-github
// Issues.Create call, or a CreateIssueRequest handed to the client — must
// run the title through issueshape.AgentIssueTitle in that same file (the
// client's createIssue does it for every CreateIssueRequest). A new raw
// create that skips the tag fails here, not in production.
func TestEveryIssueCreatePathTagsTheTitle(t *testing.T) {
	root := filepath.Join("..", "..")
	rawCreate := regexp.MustCompile(`\.Issues\.Create\(`)
	var rawSites []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !rawCreate.Match(src) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rawSites = append(rawSites, rel)
		if !strings.Contains(string(src), "issueshape.AgentIssueTitle(") {
			t.Errorf("%s calls Issues.Create without running the title through issueshape.AgentIssueTitle", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scan must have seen the client's own create path, or it walked
	// the wrong tree and proved nothing.
	if !containsSuffix(rawSites, filepath.Join("pkg", "github", "issue_request_watcher.go")) {
		t.Fatalf("scan did not reach the client's createIssue; raw create sites seen: %v", rawSites)
	}
}

func containsSuffix(paths []string, suffix string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// Each title builder a hive filer uses produces a `[<agent>] …` title that
// names the agent its label names.
func TestProducerTitlesStartWithTheirAgentTag(t *testing.T) {
	cases := map[string]struct{ agent, title string }{
		"hotspot (ci-maintainer)": {hotspot.Agent, hotspot.Title(hotspot.Hotspot{File: "src/pkg/a.go"}, 4)},
		"upstream watch (governor)": {"governor", upstreamwatch.RenderIssue("acme/up",
			upstreamwatch.Item{Kind: upstreamwatch.KindPR, Ref: "upstream#9", Title: "fix: thing"},
			upstreamwatch.Judgement{Applicable: true}, "").Title},
		"self report (selfreport)":  {"selfreport", fleetreport.SelfReportTitlePrefix + "unclassified_error_repeated: x"},
		"review backlog (reviewer)": {"reviewer", issueshape.AgentIssueTitle("reviewer", "Review backlog (security): token logged")},
		"dedupe advisory (dedupe)":  {"dedupe", issueshape.AgentIssueTitle("dedupe", "triage: duplicate and overlapping open issues/PRs to consolidate")},
	}
	for name, tc := range cases {
		if !hiveTitleRE.MatchString(tc.title) {
			t.Errorf("%s: title %q does not start with an [agent] tag", name, tc.title)
			continue
		}
		if got := issueshape.AgentPrefix(tc.title); got != tc.agent {
			t.Errorf("%s: title %q is tagged %q, want %q", name, tc.title, got, tc.agent)
		}
		if again := issueshape.AgentIssueTitle(tc.agent, tc.title); again != tc.title {
			t.Errorf("%s: re-tagging changed the title: %q -> %q", name, tc.title, again)
		}
	}
}
