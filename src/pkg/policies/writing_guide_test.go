package policies

import (
	"regexp"
	"strings"
	"testing"
)

// filesIssueOrPR matches the commands a default policy uses to file an issue
// or open a PR. Review comments (`hive-review … --body-file`) are deliberately
// not in the set: the writing guide is about issue and PR bodies.
var filesIssueOrPR = regexp.MustCompile(`gh issue create|hive-open-pr --|gh pr create`)

const writingGuideVar = "${WRITING_GUIDE}"

// TestEveryFilingTemplateCarriesTheWritingGuide pins hivecommons/hive#7667's
// contract: a hive owner sets project.writing_guide ONCE and every default
// policy that files an issue or PR renders it, immediately ahead of the body
// template the agent is told to fill in. A new lane that copies a filing
// block without the variable would silently exempt itself from the owner's
// guide — the exact "the resident agent did not change at all" the issue
// reports — so the sweep is over every embedded template, not a hand list.
func TestEveryFilingTemplateCarriesTheWritingGuide(t *testing.T) {
	entries, err := DefaultPolicies.ReadDir("defaults")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	carriers := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := DefaultPolicies.ReadFile("defaults/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(data)
		cmd := filesIssueOrPR.FindStringIndex(body)
		if cmd == nil {
			if strings.Contains(body, writingGuideVar) {
				t.Errorf("%s carries %s but files no issue or PR — the variable belongs next to a body template", e.Name(), writingGuideVar)
			}
			continue
		}
		carriers++
		n := strings.Count(body, writingGuideVar)
		if n != 1 {
			t.Errorf("%s files issues/PRs and must carry %s exactly once (found %d)", e.Name(), writingGuideVar, n)
			continue
		}
		// Ahead of the FIRST filing command: the guide is read before the
		// template it governs, not after the agent has already been told
		// what the body looks like.
		if at := strings.Index(body, writingGuideVar); at > cmd[0] {
			t.Errorf("%s places %s after its first filing command (var@%d, cmd@%d); it must precede the body template", e.Name(), writingGuideVar, at, cmd[0])
		}
	}
	if carriers < 15 {
		t.Fatalf("only %d embedded templates file issues/PRs — the sweep matched fewer than expected, check filesIssueOrPR", carriers)
	}
}
