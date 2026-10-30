package automerge

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// TestExecuteMergeEffectRecordsMergeAttempt: every lane's merge call runs
// through executeMergeEffect, which registers the attempt before the call
// and its outcome after (hivecommons/hive#15245) — so the tracker can show
// "Merging" even when the PR merges within one poll.
func TestExecuteMergeEffectRecordsMergeAttempt(t *testing.T) {
	engine := New(nil, Options{MutationBoundary: &recordingBoundary{}})
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge, Target: "41"}

	var during hgithub.MergeAttempt
	var seen bool
	out, err := engine.executeMergeEffect(context.Background(), claim, "acme", "widget", 41, "", func(context.Context) (effects.Result, error) {
		during, seen = hgithub.MergeAttemptFor("acme/widget", 41)
		return effects.Result{Provenance: "merge-sha"}, nil
	})
	if err != nil || out.Provenance != "merge-sha" {
		t.Fatalf("executeMergeEffect: %+v %v", out, err)
	}
	if !seen || !during.InFlight() || during.Path != hgithub.PRAuditPathSweep {
		t.Fatalf("attempt must be in flight during the merge call: %+v seen=%v", during, seen)
	}
	after, ok := hgithub.MergeAttemptFor("acme/widget", 41)
	if !ok || after.InFlight() || !after.Merged {
		t.Fatalf("attempt must read merged after: %+v", after)
	}

	// A refusal ends the attempt with its words.
	_, err = engine.executeMergeEffect(context.Background(), claim, "acme", "widget", 42, "", func(context.Context) (effects.Result, error) {
		return effects.Result{}, errors.New("405 Pull Request is not mergeable")
	})
	if err == nil {
		t.Fatal("expected the refusal back")
	}
	if a, ok := hgithub.MergeAttemptFor("acme/widget", 42); !ok || !a.Failed() || a.Error != "405 Pull Request is not mergeable" {
		t.Fatalf("refused attempt: %+v", a)
	}
	// A call that returns without a merge SHA did not merge.
	if _, err = engine.executeMergeEffect(context.Background(), claim, "acme", "widget", 43, "", func(context.Context) (effects.Result, error) {
		return effects.Result{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if a, ok := hgithub.MergeAttemptFor("acme/widget", 43); !ok || !a.Failed() || a.Error != "merge-not-applied" {
		t.Fatalf("not-applied attempt: %+v", a)
	}
}
