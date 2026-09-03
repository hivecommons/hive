package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/timeline"
)

// fakeLifecycleRecorder is a minimal lifecycleRecorder backed by a real
// timeline.Store, so the recording helpers can be exercised without standing up
// a full dashboard.Server.
type fakeLifecycleRecorder struct {
	store *timeline.Store
}

func (f *fakeLifecycleRecorder) LifecycleTimeline() *timeline.Store { return f.store }

func TestRecordEnumeratedIssues(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{
			Items: []github.Issue{
				{Repo: "org/repo", Number: 1},
				{Repo: "org/repo", Number: 2},
			},
		},
	}

	recordEnumeratedIssues(context.Background(), rec, actionable)

	events := rec.store.Recent(0)
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.Kind != timeline.KindEnumerated {
			t.Fatalf("event kind = %q, want %q", e.Kind, timeline.KindEnumerated)
		}
	}
	// Newest-first; issue #2 was recorded last.
	if events[0].IssueRef != "org/repo#2" {
		t.Fatalf("events[0].IssueRef = %q, want org/repo#2", events[0].IssueRef)
	}
}

func TestRecordEnumeratedIssues_BoundedPerCycle(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	n := maxTimelineEnumeratePerCycle + 25
	items := make([]github.Issue, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, github.Issue{Repo: "org/repo", Number: i})
	}
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: items}}

	recordEnumeratedIssues(context.Background(), rec, actionable)

	if got := len(rec.store.Recent(0)); got != maxTimelineEnumeratePerCycle {
		t.Fatalf("recorded %d events, want cap of %d", got, maxTimelineEnumeratePerCycle)
	}
}

func TestRecordKick(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordKick(context.Background(), rec, "quality")

	events := rec.store.Recent(0)
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Kind != timeline.KindKicked {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, timeline.KindKicked)
	}
	if events[0].Agent != "quality" {
		t.Fatalf("event agent = %q, want quality", events[0].Agent)
	}
}

func TestRecordKickIssueScoped(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordKick(context.Background(), rec, "scanner", "org/repo#1", "org/repo#2")

	events := rec.store.Recent(0)
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(events))
	}
	if events[0].IssueRef != "org/repo#2" || events[1].IssueRef != "org/repo#1" {
		t.Fatalf("issue refs = %q, %q", events[0].IssueRef, events[1].IssueRef)
	}
	for _, e := range events {
		if e.Kind != timeline.KindKicked || e.Agent != "scanner" {
			t.Fatalf("bad kick event: %+v", e)
		}
	}
}

// TestRecordHelpers_NilSafe confirms the guards: nil recorder, nil actionable,
// and a recorder returning a nil store must all be no-ops (never panic).
func TestRecordHelpers_NilSafe(t *testing.T) {
	// nil recorder
	recordEnumeratedIssues(context.Background(), nil, &github.ActionableResult{})
	recordKick(context.Background(), nil, "x")

	// nil actionable
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordEnumeratedIssues(context.Background(), rec, nil)
	if got := len(rec.store.Recent(0)); got != 0 {
		t.Fatalf("nil actionable recorded %d events, want 0", got)
	}

	// recorder that returns a nil store
	nilStore := &fakeLifecycleRecorder{store: nil}
	recordEnumeratedIssues(context.Background(), nilStore, &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{{Repo: "r", Number: 1}}},
	})
	recordKick(context.Background(), nilStore, "x")
}

func TestIssueRef(t *testing.T) {
	if got := issueRef("org/repo", 42); got != "org/repo#42" {
		t.Fatalf("issueRef = %q, want org/repo#42", got)
	}
	if got := issueRef("", 42); got != "" {
		t.Fatalf("issueRef(empty repo) = %q, want empty", got)
	}
}
