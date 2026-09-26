package dashboard

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestRepoHoldItemNumberAcrossShapes(t *testing.T) {
	cases := map[string]struct {
		raw  any
		want int
	}{
		"issue":           {github.Issue{Number: 1}, 1},
		"issue ptr":       {&github.Issue{Number: 2}, 2},
		"issue nil":       {(*github.Issue)(nil), 0},
		"pr":              {github.PullRequest{Number: 3}, 3},
		"pr ptr":          {&github.PullRequest{Number: 4}, 4},
		"pr nil":          {(*github.PullRequest)(nil), 0},
		"frontend pr":     {FrontendPR{PullRequest: github.PullRequest{Number: 5}}, 5},
		"frontend pr ptr": {&FrontendPR{PullRequest: github.PullRequest{Number: 6}}, 6},
		"frontend nil":    {(*FrontendPR)(nil), 0},
		"hold item":       {github.HoldItem{Number: 7}, 7},
		"hold item ptr":   {&github.HoldItem{Number: 8}, 8},
		"hold nil":        {(*github.HoldItem)(nil), 0},
		"map int":         {map[string]any{"number": 9}, 9},
		"map int64":       {map[string]any{"number": int64(10)}, 10},
		"map float":       {map[string]any{"number": 11.0}, 11},
		"map json number": {map[string]any{"number": json.Number("12")}, 12},
		"map string":      {map[string]any{"number": "13"}, 0},
		"unknown":         {"nope", 0},
	}
	for name, tc := range cases {
		if got := repoHoldItemNumber(tc.raw); got != tc.want {
			t.Errorf("%s: got %d want %d", name, got, tc.want)
		}
	}
}

func TestRepoHoldItemStringAcrossShapes(t *testing.T) {
	issue := github.Issue{Repo: "o/r", Title: "Issue", URL: "https://i"}
	pr := github.PullRequest{Repo: "o/p", Title: "PR", URL: "https://p"}
	hold := github.HoldItem{Repo: "o/h", Title: "Hold", URL: "https://h"}
	cases := map[string]struct {
		raw   any
		field string
		want  string
	}{
		"issue repo":        {issue, "repo", "o/r"},
		"issue title":       {issue, "title", "Issue"},
		"issue url":         {issue, "url", "https://i"},
		"issue other":       {issue, "body", ""},
		"issue ptr":         {&issue, "title", "Issue"},
		"issue nil":         {(*github.Issue)(nil), "title", ""},
		"pr repo":           {pr, "repo", "o/p"},
		"pr title":          {pr, "title", "PR"},
		"pr url":            {pr, "url", "https://p"},
		"pr other":          {pr, "body", ""},
		"pr ptr":            {&pr, "url", "https://p"},
		"pr nil":            {(*github.PullRequest)(nil), "url", ""},
		"frontend pr":       {FrontendPR{PullRequest: pr}, "repo", "o/p"},
		"frontend pr ptr":   {&FrontendPR{PullRequest: pr}, "title", "PR"},
		"frontend pr nil":   {(*FrontendPR)(nil), "title", ""},
		"hold repo":         {hold, "repo", "o/h"},
		"hold title":        {hold, "title", "Hold"},
		"hold url":          {hold, "url", "https://h"},
		"hold other":        {hold, "body", ""},
		"hold ptr":          {&hold, "repo", "o/h"},
		"hold nil":          {(*github.HoldItem)(nil), "repo", ""},
		"map present":       {map[string]any{"title": "M"}, "title", "M"},
		"map missing":       {map[string]any{}, "title", ""},
		"map wrong type":    {map[string]any{"title": 4}, "title", ""},
		"unsupported shape": {42, "title", ""},
	}
	for name, tc := range cases {
		if got := repoHoldItemString(tc.raw, tc.field); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestRepoHoldItemLabelsAcrossShapes(t *testing.T) {
	labels := []string{"a", "hold"}
	cases := map[string]struct {
		raw  any
		want []string
	}{
		"issue":           {github.Issue{Labels: labels}, labels},
		"issue ptr":       {&github.Issue{Labels: labels}, labels},
		"issue nil":       {(*github.Issue)(nil), nil},
		"pr":              {github.PullRequest{Labels: labels}, labels},
		"pr ptr":          {&github.PullRequest{Labels: labels}, labels},
		"pr nil":          {(*github.PullRequest)(nil), nil},
		"frontend pr":     {FrontendPR{PullRequest: github.PullRequest{Labels: labels}}, labels},
		"frontend pr ptr": {&FrontendPR{PullRequest: github.PullRequest{Labels: labels}}, labels},
		"frontend nil":    {(*FrontendPR)(nil), nil},
		"hold":            {github.HoldItem{Labels: labels}, labels},
		"hold ptr":        {&github.HoldItem{Labels: labels}, labels},
		"hold nil":        {(*github.HoldItem)(nil), nil},
		"map strings":     {map[string]any{"labels": labels}, labels},
		"map any":         {map[string]any{"labels": []any{"a", "hold"}}, labels},
		"map wrong":       {map[string]any{"labels": "hold"}, nil},
		"unknown":         {3.5, nil},
	}
	for name, tc := range cases {
		got := repoHoldItemLabels(tc.raw)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
	// Returned slice must be a copy.
	src := github.Issue{Labels: []string{"x"}}
	got := repoHoldItemLabels(src)
	got[0] = "mutated"
	if src.Labels[0] != "x" {
		t.Fatal("repoHoldItemLabels must copy the label slice")
	}
}

func TestRepoHoldSetItemLabelsAcrossShapes(t *testing.T) {
	want := []string{"hold"}
	check := func(name string, got any) {
		t.Helper()
		if labels := repoHoldItemLabels(got); !reflect.DeepEqual(labels, want) {
			t.Errorf("%s: labels after set = %v want %v", name, labels, want)
		}
	}
	check("issue", repoHoldSetItemLabels(github.Issue{Labels: []string{"old"}}, want))
	check("issue ptr", repoHoldSetItemLabels(&github.Issue{}, want))
	check("pr", repoHoldSetItemLabels(github.PullRequest{}, want))
	check("pr ptr", repoHoldSetItemLabels(&github.PullRequest{}, want))
	check("frontend pr", repoHoldSetItemLabels(FrontendPR{}, want))
	check("frontend pr ptr", repoHoldSetItemLabels(&FrontendPR{}, want))
	check("hold", repoHoldSetItemLabels(github.HoldItem{}, want))
	check("hold ptr", repoHoldSetItemLabels(&github.HoldItem{}, want))

	orig := map[string]any{"number": 1, "labels": []string{"old"}}
	got := repoHoldSetItemLabels(orig, want)
	check("map", got)
	if m, ok := got.(map[string]any); !ok || m["number"] != 1 {
		t.Fatalf("map copy lost fields: %#v", got)
	}
	if reflect.DeepEqual(orig["labels"], want) {
		t.Fatal("repoHoldSetItemLabels must not mutate the source map")
	}
	// Pointer inputs return copies, not the original.
	ptr := &github.Issue{Labels: []string{"old"}}
	_ = repoHoldSetItemLabels(ptr, want)
	if ptr.Labels[0] != "old" {
		t.Fatal("pointer input must not be mutated")
	}
	for name, nilRaw := range map[string]any{
		"issue nil": (*github.Issue)(nil), "pr nil": (*github.PullRequest)(nil),
		"frontend nil": (*FrontendPR)(nil), "hold nil": (*github.HoldItem)(nil), "unknown": "str",
	} {
		if got := repoHoldSetItemLabels(nilRaw, want); !reflect.DeepEqual(got, nilRaw) {
			t.Errorf("%s: expected passthrough, got %#v", name, got)
		}
	}
}

func TestRepoHoldAdjustBreakdownClampsAtZero(t *testing.T) {
	repoHoldAdjustBreakdown(nil, "pr", true)
	repoHoldAdjustBreakdown(&FrontendRepo{}, "pr", true)

	repo := &FrontendRepo{WorkBreakdown: &github.RepoWorkBreakdown{}}
	repoHoldAdjustBreakdown(repo, "pr", true)
	if repo.WorkBreakdown.PRs.Hold != 1 || repo.WorkBreakdown.PRs.Actionable != 0 {
		t.Fatalf("pr hold: %+v", repo.WorkBreakdown.PRs)
	}
	repoHoldAdjustBreakdown(repo, "pr", false)
	repoHoldAdjustBreakdown(repo, "pr", false)
	if repo.WorkBreakdown.PRs.Hold != 0 || repo.WorkBreakdown.PRs.Actionable != 2 {
		t.Fatalf("pr release: %+v", repo.WorkBreakdown.PRs)
	}

	repoHoldAdjustBreakdown(repo, "issue", true)
	if repo.WorkBreakdown.Issues.Hold != 1 || repo.WorkBreakdown.Issues.Actionable != 0 {
		t.Fatalf("issue hold: %+v", repo.WorkBreakdown.Issues)
	}
	repoHoldAdjustBreakdown(repo, "issue", false)
	repoHoldAdjustBreakdown(repo, "issue", false)
	if repo.WorkBreakdown.Issues.Hold != 0 || repo.WorkBreakdown.Issues.Actionable != 2 {
		t.Fatalf("issue release: %+v", repo.WorkBreakdown.Issues)
	}
}

func TestRepoHoldStatusRepoMatches(t *testing.T) {
	repo := FrontendRepo{Full: "Org/Repo", Name: "repo"}
	for _, full := range []string{"org/repo", " ORG/REPO ", "Repo"} {
		if !repoHoldStatusRepoMatches(repo, full) {
			t.Errorf("expected %q to match", full)
		}
	}
	if repoHoldStatusRepoMatches(repo, "other/repo") {
		t.Error("unexpected match")
	}
}
