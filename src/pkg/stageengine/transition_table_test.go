package stageengine

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

const diagramDoc = "../../docs/labels-and-control-signals/stage-labels.md"

var edgeLine = regexp.MustCompile(`^\s*s(\d{3})\s*-->\s*s(\d{3})\s*:`)

// diagramEdgesFromDoc parses the stateDiagram-v2 fenced block and returns
// its sNNN --> sMMM edges ([*] edges dropped), deduplicated.
func diagramEdgesFromDoc(t *testing.T) map[Edge]bool {
	t.Helper()
	f, err := os.Open(filepath.Clean(diagramDoc))
	if err != nil {
		t.Fatalf("open diagram doc: %v", err)
	}
	defer f.Close()
	edges := map[Edge]bool{}
	inBlock, sawDiagram := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "```mermaid"):
			inBlock = true
			continue
		case inBlock && strings.HasPrefix(line, "```"):
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		if strings.Contains(line, "stateDiagram-v2") {
			sawDiagram = true
		}
		m := edgeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		from, _ := strconv.Atoi(m[1])
		to, _ := strconv.Atoi(m[2])
		fs, ok := StageByNumber(from)
		if !ok {
			t.Fatalf("diagram names s%03d, not in stagelabels.Table", from)
		}
		ts, ok := StageByNumber(to)
		if !ok {
			t.Fatalf("diagram names s%03d, not in stagelabels.Table", to)
		}
		edges[Edge{fs, ts}] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawDiagram || len(edges) == 0 {
		t.Fatalf("no stateDiagram-v2 edges found in %s", diagramDoc)
	}
	return edges
}

// TestTransitionTableMatchesDiagram: the generated table and the mermaid
// diagram are the same set of edges, both ways.
func TestTransitionTableMatchesDiagram(t *testing.T) {
	doc := diagramEdgesFromDoc(t)
	for e := range doc {
		if !diagramEdges[e] {
			t.Errorf("diagram draws %s → %s but transition_table.go lacks it", e.From, e.To)
		}
	}
	for e := range diagramEdges {
		if !doc[e] {
			t.Errorf("transition_table.go has %s → %s but the diagram does not draw it", e.From, e.To)
		}
	}
	if got, want := len(DiagramEdges()), len(diagramEdges); got != want {
		t.Fatalf("DiagramEdges() = %d edges, table has %d", got, want)
	}
	// Every same-kind diagram edge is expected for that kind; the one
	// cross-kind edge (040 → 050, the PR taking over) is expected for
	// neither, because no item carries both stages.
	cross := 0
	for _, e := range DiagramEdges() {
		for _, k := range []stagelabels.Item{stagelabels.IssueItem, stagelabels.PRItem} {
			same := e.From.Applies(k) && e.To.Applies(k)
			if same && !Expected(k, e.From, e.To) {
				t.Errorf("Expected(%s, %s, %s) = false for a diagram edge", k, e.From, e.To)
			}
		}
		if !(e.From.Applies(stagelabels.IssueItem) && e.To.Applies(stagelabels.IssueItem)) &&
			!(e.From.Applies(stagelabels.PRItem) && e.To.Applies(stagelabels.PRItem)) {
			cross++
			if Expected(stagelabels.IssueItem, e.From, e.To) || Expected(stagelabels.PRItem, e.From, e.To) {
				t.Errorf("cross-kind edge %s → %s must not be expected on either kind", e.From, e.To)
			}
		}
	}
	if cross != 1 {
		t.Fatalf("diagram has %d cross-kind edges, want exactly the 040 → 050 hand-over", cross)
	}
}

func TestExpectedRules(t *testing.T) {
	cases := []struct {
		name     string
		kind     stagelabels.Item
		from, to stagelabels.Stage
		want     bool
	}{
		{"none to anything", stagelabels.PRItem, stagelabels.None, stagelabels.ReviewPending, true},
		{"anything to none", stagelabels.IssueItem, stagelabels.Claimed, stagelabels.None, true},
		{"no-op", stagelabels.PRItem, stagelabels.MergeQueue, stagelabels.MergeQueue, true},
		{"forward skip not drawn", stagelabels.IssueItem, stagelabels.Actionable, stagelabels.CoveredByPR, true},
		{"forward skip not drawn (pr)", stagelabels.PRItem, stagelabels.ReviewPending, stagelabels.MergeQueue, true},
		{"non-regression backward hop", stagelabels.PRItem, stagelabels.ReadyForHumanMerge, stagelabels.ReturnedToContributor, true},
		{"non-regression backward hop not drawn", stagelabels.PRItem, stagelabels.ReadyForHumanMerge, stagelabels.Reviewed, true},
		{"drawn regression", stagelabels.PRItem, stagelabels.MergeQueue, stagelabels.Fixing, true},
		{"drawn hop (issue): claim lapsed, not a regression", stagelabels.IssueItem, stagelabels.Claimed, stagelabels.Actionable, true},
		{"drawn regression (issue): start mark lapsed", stagelabels.IssueItem, stagelabels.PRInProgress, stagelabels.Actionable, true},
		{"covering PR closed, nothing live", stagelabels.IssueItem, stagelabels.CoveredByPR, stagelabels.Actionable, true},
		{"covering PR worked again, not a regression", stagelabels.IssueItem, stagelabels.CoveredByPR, stagelabels.PRInProgress, true},
		{"covering PR closed, claim held", stagelabels.IssueItem, stagelabels.CoveredByPR, stagelabels.Claimed, true},
		{"PR evidence lapsed, claim held", stagelabels.IssueItem, stagelabels.PRInProgress, stagelabels.Claimed, true},
		// 045 never regresses (IsRegression) and every backward hop among
		// 010–040 is now drawn, so a park release is a non-regression hop.
		{"park released to a held claim, not drawn", stagelabels.IssueItem, stagelabels.IssueNeedsDecision, stagelabels.Claimed, true},
		{"undrawn regression (pr)", stagelabels.PRItem, stagelabels.AwaitingMergeAdmission, stagelabels.ReviewPending, false},
		{"pr stage on an issue", stagelabels.IssueItem, stagelabels.Actionable, stagelabels.ReviewPending, false},
		{"issue stage on a pr", stagelabels.PRItem, stagelabels.Claimed, stagelabels.ReviewPending, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Expected(tc.kind, tc.from, tc.to); got != tc.want {
				t.Fatalf("Expected(%s, %s, %s) = %v, want %v", tc.kind, tc.from, tc.to, got, tc.want)
			}
			if a := Anomaly(tc.kind, tc.from, tc.to); (a == "") != tc.want {
				t.Fatalf("Anomaly = %q, expected=%v", a, tc.want)
			}
		})
	}
	if _, ok := StageByNumber(999); ok {
		t.Fatal("StageByNumber(999) must not resolve")
	}
}
