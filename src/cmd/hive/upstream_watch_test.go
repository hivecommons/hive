package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

func TestUpstreamWatchFork(t *testing.T) {
	tests := []struct {
		org, key    string
		owner, repo string
		ok          bool
	}{
		{"acme", "widgets", "acme", "widgets", true},
		{"acme", "acme/widgets", "acme", "widgets", true},
		{"", "other/widgets", "other", "widgets", true},
		{"", "widgets", "", "", false},
		{"acme", "a/b/c", "", "", false},
	}
	for _, tc := range tests {
		owner, repo, ok := upstreamWatchFork(tc.org, tc.key)
		if owner != tc.owner || repo != tc.repo || ok != tc.ok {
			t.Errorf("upstreamWatchFork(%q, %q) = %q %q %v, want %q %q %v", tc.org, tc.key, owner, repo, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestRunUpstreamWatch_DisabledIsNoop(t *testing.T) {
	prev := upstreamWatchLastRun
	t.Cleanup(func() { upstreamWatchLastRun = prev })
	for _, cfg := range []*config.Config{
		nil,
		{},
		{UpstreamWatch: config.UpstreamWatchConfig{Enabled: true}},
		{UpstreamWatch: config.UpstreamWatchConfig{Repos: map[string]config.UpstreamWatchRepo{"widgets": {}}}},
	} {
		// A nil GitHub client would panic if the watch got past its gate.
		runUpstreamWatch(context.Background(), cfg, nil, slog.Default())
	}
	if !upstreamWatchLastRun.Equal(prev) {
		t.Fatal("a disabled watch recorded a run")
	}
}

func TestRunUpstreamWatch_SingleFlight(t *testing.T) {
	prev := upstreamWatchLastRun
	t.Cleanup(func() { upstreamWatchLastRun = prev })
	cfg := &config.Config{UpstreamWatch: config.UpstreamWatchConfig{
		Enabled: true,
		Repos:   map[string]config.UpstreamWatchRepo{"widgets": {}},
	}}
	upstreamWatchMu.Lock()
	defer upstreamWatchMu.Unlock()
	// A pass already in flight makes this call return before it reaches the
	// (nil) GitHub client or the timer.
	runUpstreamWatch(context.Background(), cfg, nil, slog.Default())
	if !upstreamWatchLastRun.Equal(prev) {
		t.Fatal("a concurrent call touched the pass timer")
	}
}

type recordingFiler struct{ got upstreamwatch.Issue }

func (r *recordingFiler) FindMarker(context.Context, string) (upstreamwatch.Existing, bool, error) {
	return upstreamwatch.Existing{}, false, nil
}

func (r *recordingFiler) File(_ context.Context, issue upstreamwatch.Issue) (int, error) {
	r.got = issue
	return 1, nil
}

func (r *recordingFiler) GetIssue(context.Context, int) (upstreamwatch.IssueOutcome, bool, error) {
	return upstreamwatch.IssueOutcome{}, false, nil
}

func TestUpstreamWatchFiler_NeutralizesMentions(t *testing.T) {
	rec := &recordingFiler{}
	f := upstreamWatchFiler{Filer: rec}
	marker := "<!-- upstream-ref: up/widgets@v1.0.0 -->"
	if _, err := f.File(context.Background(), upstreamwatch.Issue{Title: "upstream: thanks @alice", Body: "cc @bob\n" + marker}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.got.Title, " @alice") || strings.Contains(rec.got.Body, " @bob") {
		t.Fatalf("mentions survived: %+v", rec.got)
	}
	if !strings.Contains(rec.got.Body, marker) {
		t.Fatalf("marker was rewritten: %q", rec.got.Body)
	}
}
