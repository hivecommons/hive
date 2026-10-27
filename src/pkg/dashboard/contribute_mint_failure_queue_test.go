package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// #14003: the public status and queue must project the exclusion assignment
// already recorded, without discovering or refreshing failures by minting.
func TestMintFailureQueue_StatusAndDiagnostics(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "off")
	for _, tc := range []struct {
		name         string
		excluded     []string
		expired      bool
		wantReady    int
		wantWithheld int
	}{
		{name: "no recorded failure", wantReady: 2},
		{name: "mixed repositories", excluded: []string{"myorg/review"}, wantReady: 1, wantWithheld: 1},
		{name: "all repositories excluded", excluded: []string{"myorg/review", "myorg/testsuite"}, wantWithheld: 2},
		{name: "cooldown expired", excluded: []string{"myorg/review", "myorg/testsuite"}, expired: true, wantReady: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := covK2Hub(t)
			twoRepoStatus(s)
			// Route the HTTP handlers through the same hub as assignment.
			originalHub := s.contributeHub
			s.contributeHub = hub
			t.Cleanup(func() { s.contributeHub = originalHub })
			auth, mints := newRepoRejectingAppAuth(t, "review")
			s.deps.GHAppAuth = auth
			recorded := time.Now()
			if tc.expired {
				recorded = recorded.Add(-mintFailureRepoCooldown - time.Second)
			}
			for _, repo := range tc.excluded {
				hub.markRepoUnmintable(repo, recorded)
			}

			statusRec := httptest.NewRecorder()
			s.handleContributeStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil))
			status := decodeContributeResponse[contributeStatusCounts](t, statusRec)
			if status.ActionableItems != tc.wantReady || status.CandidateItems != 2 {
				t.Fatalf("status = %+v, want actionable %d, candidates 2", status, tc.wantReady)
			}
			for _, target := range []string{"/api/contribute/queue", "/api/contribute/queue?withheld=1"} {
				rec := httptest.NewRecorder()
				s.handleContributeQueue(rec, httptest.NewRequest(http.MethodGet, target, nil))
				queue := decodeContributeResponse[struct {
					contributeQueueCounts
					Withheld      []AdmissionWithheldItem `json:"withheld"`
					WithheldTotal int                     `json:"withheld_total"`
				}](t, rec)
				if queue.QueueTotal != tc.wantReady || len(queue.Queue) != tc.wantReady || queue.HeldTotal != 0 {
					t.Fatalf("%s queue = %+v, want %d ready rows", target, queue, tc.wantReady)
				}
				if tc.wantReady == 1 && queue.Queue[0].Repo != "myorg/testsuite" {
					t.Fatalf("the healthy repository must remain ready: %+v", queue.Queue)
				}
				if !strings.Contains(target, "withheld=1") {
					if queue.Withheld != nil {
						t.Fatalf("diagnostics must remain opt-in: %+v", queue.Withheld)
					}
					continue
				}
				if queue.WithheldTotal != tc.wantWithheld || len(queue.Withheld) != tc.wantWithheld {
					t.Fatalf("withheld = %+v, total %d, want %d", queue.Withheld, queue.WithheldTotal, tc.wantWithheld)
				}
				for _, item := range queue.Withheld {
					if item.Reason != taskUnavailableRepoUnmintable || !strings.Contains(item.Detail, "token mint failure") {
						t.Errorf("missing actionable mint failure explanation: %+v", item)
					}
					wantExpiry := recorded.Add(mintFailureRepoCooldown).UTC().Format(time.RFC3339)
					if item.CooldownUntil != wantExpiry {
						t.Errorf("cooldown_until = %q, want recorded expiry %q", item.CooldownUntil, wantExpiry)
					}
					if item.Key == "" || item.Title == "" || item.URL == "" {
						t.Errorf("missing issue metadata: %+v", item)
					}
				}
			}
			shadow := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldConvergence)
			if shadow.offerableTotal != tc.wantReady || len(shadow.withheld) != 0 {
				t.Fatalf("mint failures must gate the queue without becoming convergence diagnostics: %+v", shadow)
			}
			if got := mints.Load(); got != 0 {
				t.Fatalf("read-only projections attempted %d speculative token mints", got)
			}
		})
	}
}
