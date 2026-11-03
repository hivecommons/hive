package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestCommitCISelectionAllPermutations(t *testing.T) {
	first := time.Date(2026, 11, 2, 12, 5, 0, 0, time.UTC)
	earlier := first.Add(-time.Minute)
	run := func(id int64, status, conclusion string, started *time.Time) *gh.CheckRun {
		check := &gh.CheckRun{ID: gh.Ptr(id), Name: gh.Ptr("validate"), HeadSHA: gh.Ptr("head"),
			App: &gh.App{ID: gh.Ptr(int64(11))}, Status: gh.Ptr(status), Conclusion: gh.Ptr(conclusion)}
		if started != nil {
			check.StartedAt = &gh.Timestamp{Time: *started}
		}
		return check
	}
	permutations := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, mode := range []string{"mixed-starts", "missing-starts", "tied-starts", "missing-id", "missing-ids", "tied-ids", "wrong-head", "latest-success", "superseded-tie", "independent-app"} {
		t.Run(mode, func(t *testing.T) {
			checks := []*gh.CheckRun{
				run(101, "completed", "success", &first),
				run(102, "queued", "", nil),
				run(103, "completed", "failure", &earlier),
			}
			wantGreen, wantDCO := false, false
			switch mode {
			case "missing-starts":
				for _, check := range checks {
					check.StartedAt = nil
				}
			case "tied-starts", "tied-ids":
				for _, check := range checks {
					check.StartedAt = &gh.Timestamp{Time: first}
					if mode == "tied-ids" {
						check.ID = gh.Ptr(int64(101))
					}
				}
			case "missing-id":
				checks[1].ID = nil
			case "missing-ids":
				for _, check := range checks {
					check.ID = nil
				}
			case "wrong-head":
				checks[0].ID, checks[0].HeadSHA = gh.Ptr(int64(999)), gh.Ptr("old-head")
			case "latest-success":
				checks[0].Conclusion, checks[2].Conclusion = gh.Ptr("failure"), gh.Ptr("success")
				wantGreen, wantDCO = true, true
			case "superseded-tie":
				checks[1].ID, checks[1].StartedAt = gh.Ptr(int64(101)), &gh.Timestamp{Time: first}
				checks[1].Status, checks[1].Conclusion = gh.Ptr("completed"), gh.Ptr("failure")
				checks[2].Conclusion = gh.Ptr("success")
				wantGreen, wantDCO = true, true
			case "independent-app":
				checks[1].App.ID = gh.Ptr(int64(22))
				checks[1].Status, checks[1].Conclusion = gh.Ptr("completed"), gh.Ptr("success")
				// Canonical DCO accepts an independent producer's success;
				// required CI still sees the required-name failure.
				wantDCO = true
			}
			var firstSelection, firstActivity string
			for i, permutation := range permutations {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					ordered := []*gh.CheckRun{checks[permutation[0]], checks[permutation[1]], checks[permutation[2]]}
					selected := latestCheckRunsByNameAndApp(checkRunsOnHead(ordered, "head"))
					serialized, err := json.Marshal(selected)
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						firstSelection = string(serialized)
					} else if string(serialized) != firstSelection {
						t.Fatalf("selection changed with input order: %s vs %s", serialized, firstSelection)
					}
					activity, err := json.Marshal(activityCheckRunsByNameAndApp(checkRunsOnHead(ordered, "head")))
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						firstActivity = string(activity)
					} else if string(activity) != firstActivity {
						t.Fatalf("activity changed with input order: %s vs %s", activity, firstActivity)
					}
					var dco []*gh.CheckRun
					for _, check := range ordered {
						copy := *check
						copy.Name = gh.Ptr("DCO")
						dco = append(dco, &copy)
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/check-runs") {
							payload := ordered
							if strings.HasPrefix(r.URL.Path, "/dco/") {
								payload = dco
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": payload})
						} else {
							fmt.Fprint(w, `{"sha":"head","statuses":[]}`)
						}
					}))
					defer srv.Close()
					client := gh.NewClient(srv.Client())
					client.BaseURL, _ = url.Parse(srv.URL + "/")
					got, err := EvaluateCommitCI(context.Background(), client, "org", "repo", "head",
						CommitCIOptions{Required: map[string]bool{"validate": true}, RequiredKnown: true})
					if err != nil || got.Green != wantGreen {
						t.Fatalf("EvaluateCommitCI = %+v, %v; want green=%v", got, err, wantGreen)
					}
					if got := dcoContextSucceeded(nil, checkRunsOnHead(dco, "head")); got != wantDCO {
						t.Fatalf("canonical DCO = %v, want %v", got, wantDCO)
					}
					dcoClient := gh.NewClient(srv.Client())
					dcoClient.BaseURL, _ = url.Parse(srv.URL + "/dco/")
					dcoState, err := EvaluateCommitCI(context.Background(), dcoClient, "org", "repo", "head",
						CommitCIOptions{Required: map[string]bool{"DCO": true}, RequiredKnown: true})
					if err != nil || dcoState.Green != wantGreen {
						t.Fatalf("EvaluateCommitCI DCO = %+v, %v; want green=%v", dcoState, err, wantGreen)
					}
				})
			}
		})
	}
}
