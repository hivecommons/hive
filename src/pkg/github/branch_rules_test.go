package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func branchRulesClient(t *testing.T, classicStatus int, classicBody string, rulesStatus int, rulesBody string) *gh.Client {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection/required_status_checks":
			w.WriteHeader(classicStatus)
			_, _ = w.Write([]byte(classicBody))
		case "/repos/o/r/rules/branches/main":
			w.WriteHeader(rulesStatus)
			_, _ = w.Write([]byte(rulesBody))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	client := gh.NewClient(nil)
	base, err := url.Parse(api.URL + "/")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	client.BaseURL = base
	return client
}

const (
	classicNotProtected = `{"message":"Branch not protected"}`
	classicOnly         = `{"strict":true,"contexts":["lint"],"checks":[{"context":"build"}]}`
	rulesetStrict       = `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"strict_required_status_checks_policy":true,"required_status_checks":[{"context":"test"}]}}]`
	rulesetLoose        = `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"test"}]}}]`
	rulesetMergeQueue   = `[{"type":"merge_queue","ruleset_id":2,"parameters":{"check_response_timeout_minutes":60,"grouping_strategy":"ALLGREEN","max_entries_to_build":5,"max_entries_to_merge":5,"merge_method":"MERGE","min_entries_to_merge":1,"min_entries_to_merge_wait_minutes":5}}]`
)

func TestReadBranchRules(t *testing.T) {
	tests := []struct {
		name         string
		classicCode  int
		classicBody  string
		rulesCode    int
		rulesBody    string
		config       map[string]bool
		configKnown  bool
		wantKnown    bool
		wantRequired []string
		wantUpToDate UpToDateEnforcement
		wantQueue    bool
		wantQueueOK  bool
		wantReason   string
	}{
		{
			name: "ruleset only", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 200, rulesBody: rulesetStrict,
			wantKnown: true, wantRequired: []string{"test"}, wantUpToDate: UpToDateServerEnforced, wantQueueOK: true,
		},
		{
			name: "ruleset not strict", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 200, rulesBody: rulesetLoose,
			wantKnown: true, wantRequired: []string{"test"}, wantUpToDate: UpToDateHiveChecked, wantQueueOK: true,
		},
		{
			name: "classic only", classicCode: 200, classicBody: classicOnly,
			rulesCode: 200, rulesBody: `[]`,
			wantKnown: true, wantRequired: []string{"build", "lint"}, wantUpToDate: UpToDateUnknown, wantQueueOK: true,
		},
		{
			name: "both plus config", classicCode: 200, classicBody: classicOnly,
			rulesCode: 200, rulesBody: rulesetStrict,
			config: map[string]bool{"extra": true}, configKnown: true,
			wantKnown: true, wantRequired: []string{"build", "extra", "lint", "test"}, wantUpToDate: UpToDateServerEnforced, wantQueueOK: true,
		},
		{
			name: "none is empty", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 200, rulesBody: `[]`,
			wantUpToDate: UpToDateUnknown, wantQueueOK: true,
			wantReason: "no required checks found in config, branch protection or rulesets",
		},
		{
			name: "classic error", classicCode: 500, classicBody: `{"message":"boom"}`,
			rulesCode: 200, rulesBody: rulesetStrict,
			wantRequired: []string{"test"}, wantUpToDate: UpToDateServerEnforced, wantQueueOK: true,
			wantReason: "classic branch protection unreadable: ",
		},
		{
			name: "rules error", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 500, rulesBody: `{"message":"boom"}`,
			wantUpToDate: UpToDateUnknown,
			wantReason:   "branch rulesets unreadable: ",
		},
		{
			name: "merge queue rule", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 200, rulesBody: rulesetMergeQueue,
			config: map[string]bool{"ci": true}, configKnown: true,
			wantKnown: true, wantRequired: []string{"ci"}, wantUpToDate: UpToDateUnknown, wantQueue: true, wantQueueOK: true,
		},
		{
			name: "null rules body", classicCode: 404, classicBody: classicNotProtected,
			rulesCode: 200, rulesBody: `null`,
			config: map[string]bool{"ci": true}, configKnown: true,
			wantKnown: true, wantRequired: []string{"ci"}, wantUpToDate: UpToDateUnknown, wantQueueOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := branchRulesClient(t, tt.classicCode, tt.classicBody, tt.rulesCode, tt.rulesBody)
			got := ReadBranchRules(context.Background(), client, "o", "r", "main", tt.config, tt.configKnown)
			if got.Known != tt.wantKnown {
				t.Fatalf("Known = %v, want %v (reason %q)", got.Known, tt.wantKnown, got.Reason)
			}
			if tt.wantKnown && got.Reason != "" {
				t.Fatalf("Reason = %q, want empty", got.Reason)
			}
			if !tt.wantKnown && (got.Reason == "" || len(got.Reason) < len(tt.wantReason) || got.Reason[:len(tt.wantReason)] != tt.wantReason) {
				t.Fatalf("Reason = %q, want prefix %q", got.Reason, tt.wantReason)
			}
			want := tt.wantRequired
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got.SortedRequired(), want) {
				t.Fatalf("Required = %v, want %v", got.SortedRequired(), want)
			}
			if got.UpToDate != tt.wantUpToDate {
				t.Fatalf("UpToDate = %q, want %q", got.UpToDate, tt.wantUpToDate)
			}
			if got.MergeQueue != tt.wantQueue || got.MergeQueueKnown != tt.wantQueueOK {
				t.Fatalf("MergeQueue = (%v,%v), want (%v,%v)", got.MergeQueue, got.MergeQueueKnown, tt.wantQueue, tt.wantQueueOK)
			}
		})
	}
}

func TestReadBranchRulesNilClientOrBranch(t *testing.T) {
	if got := ReadBranchRules(context.Background(), nil, "o", "r", "main", nil, false); got.Known || got.Reason == "" {
		t.Fatalf("nil client = %+v, want unknown with reason", got)
	}
	if got := ReadBranchRules(context.Background(), gh.NewClient(nil), "o", "r", " ", nil, false); got.Known || got.Reason == "" {
		t.Fatalf("blank branch = %+v, want unknown with reason", got)
	}
}

func TestClassifyRequiredCheck(t *testing.T) {
	tests := []struct {
		status, conclusion string
		want               CheckOutcome
	}{
		{"completed", "success", CheckPassed},
		{"completed", "skipped", CheckPassed},
		{"completed", "neutral", CheckPassed},
		{"COMPLETED", "SUCCESS", CheckPassed},
		{"completed", "failure", CheckFailed},
		{"completed", "cancelled", CheckFailed},
		{"completed", "timed_out", CheckFailed},
		{"completed", "action_required", CheckFailed},
		{"completed", "", CheckWait},
		{"completed", "stale", CheckWait},
		{"queued", "", CheckWait},
		{"in_progress", "", CheckWait},
		{"queued", "success", CheckWait},
		{"", "", CheckWait},
	}
	for _, tt := range tests {
		if got := ClassifyRequiredCheck(tt.status, tt.conclusion); got != tt.want {
			t.Errorf("ClassifyRequiredCheck(%q,%q) = %q, want %q", tt.status, tt.conclusion, got, tt.want)
		}
	}
}
