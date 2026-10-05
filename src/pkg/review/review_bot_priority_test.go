package review

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDispatchedPriorityFilterMatchesMonitor(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	bodies := []string{"![P0 Badge](url)", "![P1 Badge](url)", "![P2 Badge](url)", "![P3 Badge](url)", "unknown", "![P4 Badge](url)", "![P1 Badge](url) ![P3 Badge](url)"}
	for _, threshold := range []string{"", "P0", "P1", "P2", "P3", "typo"} {
		for _, combined := range []bool{true, false} {
			plan := PlanDispatch([]PullRequest{{Repo: "o/r", Number: 1, HeadSHA: "sha"}}, Artifact{}, DispatchState{}, DispatchOptions{
				RequireApproval: true, FanOut: true, MaxParallelReviews: 5, AllAuthors: true,
				Agents: []AgentCapability{reviewer("r1")}, CombinedPerspectives: combined,
				ReviewBotLogins: []string{"Copilot"}, ReviewBotMinPriority: threshold,
			})
			if len(plan.ReviewKicks) == 0 {
				t.Fatal("no kicks")
			}
			for _, kick := range plan.ReviewKicks {
				_, after, ok := strings.Cut(kick.Message, "--jq '")
				if !ok {
					t.Fatal("missing jq")
				}
				filter, _, ok := strings.Cut(after, "'\n")
				if !ok {
					t.Fatal("unterminated jq")
				}
				for _, body := range bodies {
					node := map[string]any{"isResolved": false, "path": "x.go", "line": 1, "comments": map[string]any{"nodes": []any{map[string]any{"author": map[string]string{"login": "copilot"}, "body": body}}}}
					page := map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"nodes": []any{node}}}}}}
					data, err := json.Marshal(page)
					if err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command(jq, "-c", filter)
					cmd.Stdin = strings.NewReader(string(data))
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("jq: %v %s", err, out)
					}
					want := (config.ReviewBotsConfig{MinPriority: threshold}).IncludesPriority(body)
					if got := len(strings.TrimSpace(string(out))) > 0; got != want {
						t.Errorf("combined=%v threshold=%q body=%q: selected=%v want=%v", combined, threshold, body, got, want)
					}
				}
			}
		}
	}
}
