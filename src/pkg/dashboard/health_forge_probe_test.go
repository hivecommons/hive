package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// stubForgeProbe installs healthTokenProbeFn and a short forgeProbeBudget for
// one test, restoring both on cleanup.
func stubForgeProbe(t *testing.T, budget time.Duration, fn func(ctx context.Context) error) {
	t.Helper()
	origFn, origBudget := healthTokenProbeFn, forgeProbeBudget
	healthTokenProbeFn = fn
	forgeProbeBudget = budget
	t.Cleanup(func() {
		healthTokenProbeFn = origFn
		forgeProbeBudget = origBudget
	})
}

func TestProbeGitHubToken_ClassifiesResult(t *testing.T) {
	transportErr := &url.Error{Op: "Post", URL: "https://github.example.com/api/v3/app/installations/1/access_tokens", Err: errors.New("dial tcp: i/o timeout")}
	tests := []struct {
		name            string
		probeErr        error
		wantErr         bool
		wantUnreachable bool
	}{
		{name: "success", probeErr: nil, wantErr: false, wantUnreachable: false},
		{name: "auth error is reachable", probeErr: errors.New("creating installation token: 401 Bad credentials"), wantErr: true, wantUnreachable: false},
		{name: "transport error", probeErr: transportErr, wantErr: true, wantUnreachable: true},
		{name: "deadline exceeded", probeErr: context.DeadlineExceeded, wantErr: true, wantUnreachable: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubForgeProbe(t, 2*time.Second, func(context.Context) error { return tc.probeErr })
			s := &Server{}
			got := s.probeGitHubToken()
			if !got.Known || got.TimedOut {
				t.Fatalf("probe result = %+v, want Known and not TimedOut", got)
			}
			if (got.Err != nil) != tc.wantErr {
				t.Errorf("Err = %v, wantErr %v", got.Err, tc.wantErr)
			}
			if s.ForgeUnreachable() != tc.wantUnreachable {
				t.Errorf("ForgeUnreachable() = %v, want %v", s.ForgeUnreachable(), tc.wantUnreachable)
			}
		})
	}
}

// TestProbeGitHubToken_DeadForgeBoundedAndCached is the #17427 guard: a forge
// probe that never answers must not hold the caller past forgeProbeBudget,
// must flag the forge unreachable, must not pile up probe goroutines across
// beats, and must clear once the probe completes.
func TestProbeGitHubToken_DeadForgeBoundedAndCached(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	stubForgeProbe(t, 50*time.Millisecond, func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})
	s := &Server{}

	for i := 0; i < 3; i++ {
		start := time.Now()
		got := s.probeGitHubToken()
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("probe %d blocked %v, want bounded by forgeProbeBudget", i, elapsed)
		}
		if !got.TimedOut || got.Known {
			t.Fatalf("probe %d = %+v, want TimedOut with no known result", i, got)
		}
		if !s.ForgeUnreachable() {
			t.Fatalf("probe %d: ForgeUnreachable() = false, want true while the probe hangs", i)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1 (single in-flight probe)", n)
	}

	close(release)
	testutil.Eventually(t, 5*time.Second, func() bool { return !s.ForgeUnreachable() },
		"ForgeUnreachable() never cleared after the probe completed")
	got := s.probeGitHubToken()
	if !got.Known || got.Err != nil || got.TimedOut {
		t.Fatalf("after recovery probe = %+v, want known success", got)
	}
}

// TestHealthSummary_GitHubAuthBoundedWhenForgeHangs pins the heartbeat-facing
// surface: HealthSummary must return within the sub-budget with a github_auth
// warn naming the unreachable forge, instead of blocking on the token mint.
func TestHealthSummary_GitHubAuthBoundedWhenForgeHangs(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubForgeProbe(t, 50*time.Millisecond, func(context.Context) error {
		<-release
		return nil
	})
	srv := newFullServer(t)
	srv.MarkReady()
	srv.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_forge_probe_17427")

	start := time.Now()
	sum := srv.HealthSummary()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("HealthSummary blocked %v behind a hung forge probe", elapsed)
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var parsed struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	found := false
	for _, c := range parsed.Checks {
		if c.Name == "github_auth" {
			found = true
			if c.Status != "warn" {
				t.Errorf("github_auth status = %q, want warn while the forge probe hangs", c.Status)
			}
		}
	}
	if !found {
		t.Fatalf("github_auth check missing: %s", raw)
	}
	if !srv.ForgeUnreachable() {
		t.Error("ForgeUnreachable() = false, want true after the probe exceeded its budget")
	}
}
