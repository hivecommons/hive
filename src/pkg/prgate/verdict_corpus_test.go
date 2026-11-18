package prgate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusPath is the golden corpus shared with the JS verifier test
// (.github/scripts/integration-pr-gate-corpus.test.cjs).
var corpusPath = filepath.Join("..", "..", "..", ".github", "scripts", "testdata", "pr-gate", "verdict-corpus.json")

const minCorpusCases = 50

type corpusCase struct {
	Title string  `json:"title"`
	Type  Verdict `json:"type"`
	Issue Verdict `json:"issue"`
}

func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read golden corpus: %v", err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode golden corpus: %v", err)
	}
	return cases
}

func TestVerdictsMatchSharedGoldenCorpus(t *testing.T) {
	cases := loadCorpus(t)
	if len(cases) < minCorpusCases {
		t.Fatalf("corpus has %d cases, want at least %d", len(cases), minCorpusCases)
	}
	for _, c := range cases {
		if got := PRTypeVerdict(c.Title); got != c.Type {
			t.Errorf("PRTypeVerdict(%q)\n got  %+v\n want %+v", c.Title, got, c.Type)
		}
		if got := PRIssueVerdict(c.Title); got != c.Issue {
			t.Errorf("PRIssueVerdict(%q)\n got  %+v\n want %+v", c.Title, got, c.Issue)
		}
	}
}
