package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestQueuedSweepDestinationGuardReasons(t *testing.T) {
	testSweepDestinationGuardReasons(t, func(c *Engine, ctx context.Context) (AutoMergeSweepEvent, string, error) {
		return c.trySweepQueuedPR(ctx, "acme/widget", "acme", "widget", 2, hgithub.AutoMergeQueuedLabel)
	})
}

func TestSelfAuthoredSweepDestinationGuardReasons(t *testing.T) {
	testSweepDestinationGuardReasons(t, func(c *Engine, ctx context.Context) (AutoMergeSweepEvent, string, error) {
		return c.trySweepSelfAuthoredPR(ctx, "acme/widget", "acme", "widget", 2, true)
	})
}

func TestTrustedAuthorSweepDestinationGuardReasons(t *testing.T) {
	testSweepDestinationGuardReasons(t, func(c *Engine, ctx context.Context) (AutoMergeSweepEvent, string, error) {
		return c.trySweepTrustedAuthorPR(ctx, "acme/widget", "acme", "widget", 2, TrustedAuthorPolicy{})
	})
}

func testSweepDestinationGuardReasons(t *testing.T, sweep func(*Engine, context.Context) (AutoMergeSweepEvent, string, error)) {
	t.Helper()
	for _, scenario := range []struct {
		name        string
		status      int
		failFetch   int
		busy        bool
		retargeted  bool
		wantReason  string
		wantFetches int
	}{
		{name: "busy", busy: true, wantReason: basePreparationPrefix + "merge-guard", wantFetches: 1},
		{name: "missing", status: http.StatusNotFound, failFetch: 1, wantReason: "gone", wantFetches: 1},
		{name: "initial-fetch-failed", status: http.StatusServiceUnavailable, failFetch: 1, wantReason: "fetch-pr", wantFetches: 1},
		{name: "guarded-fetch-failed", status: http.StatusServiceUnavailable, failFetch: 2, wantReason: "fetch-pr", wantFetches: 2},
		{name: "retargeted", retargeted: true, wantReason: "fetch-pr", wantFetches: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fetches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/2" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fetches++
				if fetches == scenario.failFetch {
					http.Error(w, `{"message":"fixture fetch failure"}`, scenario.status)
					return
				}
				pr := preparationPR(2, "main", "head2")
				if scenario.retargeted && fetches == 2 {
					pr.Base.Ref = gh.Ptr("release")
				}
				if err := json.NewEncoder(w).Encode(pr); err != nil {
					t.Errorf("encode pull request: %v", err)
				}
			}))
			defer srv.Close()
			c := preparationEngine(srv.URL, NewBasePreparationStore(filepath.Join(t.TempDir(), "preparation")))
			ctx := context.Background()
			if scenario.busy {
				_, release, err := hgithub.AcquireDestinationMerge(ctx, destinationDirectory(c), "acme/widget", "main")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			_, reason, err := sweep(c, ctx)
			if reason != scenario.wantReason {
				t.Fatalf("reason=%q, want %q (err=%v)", reason, scenario.wantReason, err)
			}
			if (err != nil) != (scenario.wantReason != "gone") ||
				errors.Is(err, hgithub.ErrDestinationMergeBusy) != scenario.busy {
				t.Fatalf("unexpected error classification: %v", err)
			}
			if fetches != scenario.wantFetches {
				t.Fatalf("fetches=%d, want %d", fetches, scenario.wantFetches)
			}
		})
	}
}

func TestGuardedPRRevalidatesDestinationAndReleasesFailedLease(t *testing.T) {
	for _, scenario := range []string{"success", "initial-fetch-failed", "guarded-fetch-failed", "retargeted"} {
		t.Run(scenario, func(t *testing.T) {
			fetches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/2" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fetches++
				if scenario == "initial-fetch-failed" || scenario == "guarded-fetch-failed" && fetches == 2 {
					http.Error(w, `{"message":"fixture not found"}`, http.StatusNotFound)
					return
				}
				pr := preparationPR(2, "main", "head2")
				if scenario == "retargeted" && fetches == 2 {
					pr.Base.Ref = gh.Ptr("release")
				}
				if err := json.NewEncoder(w).Encode(pr); err != nil {
					t.Errorf("encode pull request: %v", err)
				}
			}))
			defer srv.Close()
			c := preparationEngine(srv.URL, NewBasePreparationStore(filepath.Join(t.TempDir(), "preparation")))
			ctx := context.Background()
			pr, guarded, unlock, err := c.guardedPR(ctx, "acme", "widget", 2)
			if scenario == "success" {
				if err != nil || unlock == nil || pr.GetHead().GetSHA() != "head2" ||
					!hgithub.DestinationMergeHeld(guarded, destinationDirectory(c), "acme/widget", "main") {
					if unlock != nil {
						unlock()
					}
					t.Fatalf("successful reread lacks destination lease: pr=%+v err=%v", pr, err)
				}
				unlock()
			} else {
				if err == nil || pr != nil || unlock != nil {
					if unlock != nil {
						unlock()
					}
					t.Fatalf("failed reread returned a usable admission: pr=%+v err=%v", pr, err)
				}
				if scenario == "retargeted" && !strings.Contains(err.Error(), "destination changed") {
					t.Fatalf("retargeting did not fail at the destination guard: %v", err)
				}
			}
			wantFetches := 2
			if scenario == "initial-fetch-failed" {
				wantFetches = 1
			}
			if fetches != wantFetches {
				t.Fatalf("fetches=%d, want %d", fetches, wantFetches)
			}
			_, release, err := hgithub.AcquireDestinationMerge(ctx, destinationDirectory(c), "acme/widget", "main")
			if err != nil {
				t.Fatalf("destination lease leaked after %s: %v", scenario, err)
			}
			release()
		})
	}
}
