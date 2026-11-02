package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMergeFailureAlertSuccessfulMergeProofScope(t *testing.T) {
	for _, tt := range []struct {
		name, trigger, requested, merged string
		wantClear                        bool
	}{
		{"repository permission", appPermissionErr, "", "squash", true},
		{"same refused method", "squash commits are not allowed", "", "squash", true},
		{"different method insufficient", "squash commits are not allowed", "", "merge", false},
		{"requested method known", "merge method is not allowed", "rebase", "rebase", true},
		{"unknown method retained", "merge method is not allowed", "", "squash", false},
		{"actual error method beats requested method", "merge commits are not allowed", "squash", "squash", false},
		{"another PR review not proven", "At least 1 approving review is required", "", "squash", false},
		{"another head missing CI not proven", "required check was never created", "", "squash", false},
		{"another head missing check not proven", missingMergeCheckErr, "", "squash", false},
		{"another head run approval not proven", forkApprovalErr, "", "squash", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, nil)
			sink := &recordingMergeAlertSink{}
			c.SetMergeFailureAlertSink(sink)
			c.raiseMergeFailureAlert("o/r", 7, tt.trigger, "old", tt.requested)
			c.RecordPRMergedAudit("O/R", 9, tt.merged, "head9", PRAuditPathSweep)
			if got := len(sink.clears) == 1; got != tt.wantClear {
				t.Fatalf("cleared=%v, want %v", got, tt.wantClear)
			}
		})
	}
}

func TestMergeMethodAlertRevalidatesOriginalMethod(t *testing.T) {
	for _, tt := range []struct {
		name, trigger, flags string
		repoError            bool
		wantClear            bool
	}{
		{"squash enabled", "squash commits are not allowed", `"allow_squash_merge":true`, false, true},
		{"rebase enabled", "rebase commits are not allowed", `"allow_rebase_merge":true`, false, true},
		{"merge enabled", "merge commits are not allowed", `"allow_merge_commit":true`, false, true},
		{"wrong method enabled", "squash commits are not allowed", `"allow_merge_commit":true`, false, false},
		{"disabled retains", "squash commits are not allowed", `"allow_squash_merge":false`, false, false},
		{"missing metadata retains", "squash commits are not allowed", "", false, false},
		{"API error retains", "squash commits are not allowed", "", true, false},
		{"unknown method retains", "merge method is not allowed", `"allow_squash_merge":true`, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pulls/7") {
					_, _ = fmt.Fprint(w, `{"number":7,"state":"open","head":{"sha":"new"},"base":{"ref":"v5"}}`)
				} else if r.URL.Path == "/repos/o/r" {
					if tt.repoError {
						w.WriteHeader(http.StatusForbidden)
					} else {
						_, _ = fmt.Fprintf(w, `{%s}`, tt.flags)
					}
				} else {
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := testMergeClient(t, srv.URL)
			sink := &recordingMergeAlertSink{}
			c.SetMergeFailureAlertSink(sink)
			c.raiseMergeFailureAlert("o/r", 7, tt.trigger, "old")
			c.revalidateMergeFailureAlerts(context.Background(), nil)
			if got := len(sink.clears) == 1; got != tt.wantClear {
				t.Fatalf("cleared=%v, want %v", got, tt.wantClear)
			}
		})
	}
}

func TestMergeFailureAlertOriginRetirementDistinguishesBannerRetention(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, nil)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("o/r", 7, missingMergeCheckErr, "head7")
	c.raiseMergeFailureAlert("o/r", 8, missingMergeCheckErr, "head8")
	c.mergeAlertMu.Lock()
	defer c.mergeAlertMu.Unlock()
	for member, entry := range c.mergeAlertIDsByRepo["o/r"] {
		retired, cleared := c.clearMergeFailureAlertEntryLocked("o/r", member, entry)
		if !retired || cleared != (len(c.mergeAlertIDsByRepo["o/r"]) == 0) {
			t.Fatalf("retired=%v bannerCleared=%v", retired, cleared)
		}
		if retiredAgain, _ := c.clearMergeFailureAlertEntryLocked("o/r", member, entry); retiredAgain {
			t.Fatal("old token retired twice")
		}
	}
	if len(sink.clears) != 1 {
		t.Fatalf("aggregate must clear only once: %v", sink.clears)
	}
}
