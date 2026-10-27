package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// A merge through the GitHub forge adapter is audited as path=forge, not
// path=relay: MergePR's relay attribution belongs to the merge-request watcher
// alone (hivecommons/hive#14139).
func TestGitHubForgeMerge_AuditsForgePath(t *testing.T) {
	const prPath = "/repos/o/r/pulls/7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == prPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"state":  "open",
				"head":   map[string]any{"sha": "headsha", "ref": "topic"},
				"base":   map[string]any{"ref": "main"},
			})
		case r.Method == http.MethodPut && r.URL.Path == prPath+"/merge":
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "mergesha"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := newGitHubForge("token", Options{BaseURL: srv.URL + "/", Org: "o"})
	client, ok := f.merger.(*github.Client)
	if !ok {
		t.Fatalf("merger is %T, want *github.Client", f.merger)
	}
	var details []string
	client.SetAttributionAudit(func(action, detail, agent string) {
		if action == github.AuditActionPRMerged {
			details = append(details, detail)
		}
	})

	if err := f.Merge(context.Background(), "o/r", 7, MergeOptions{Strategy: string(MergeStrategySquash)}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("pr_merged entries = %q, want exactly one", details)
	}
	if !strings.Contains(details[0], "path="+github.PRAuditPathForge) {
		t.Errorf("pr_merged detail %q missing path=%s", details[0], github.PRAuditPathForge)
	}
	if strings.Contains(details[0], "path="+github.PRAuditPathRelay) {
		t.Errorf("pr_merged detail %q attributes the forge merge to the relay", details[0])
	}
}
