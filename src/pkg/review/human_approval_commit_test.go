package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFailedNativeApprovalRecordDoesNotBecomeAppliedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	store := NewHumanChangeRequests(path)
	original := HumanChangeRequest{Repo: "org/repo", Number: 1, HeadSHA: "head", Source: HumanChangeRequestSourceDecision, SourceID: 1, Actor: "owner", Body: "fix it", Kind: HumanChangeRequestKindChanges}
	if err := store.Record(original); err != nil {
		t.Fatal(err)
	}
	// A directory at the replacement path deterministically refuses a write,
	// even on root CI runners; no filesystem permission assumptions.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	approval := original
	approval.Kind, approval.Body, approval.BodyDigest = HumanChangeRequestKindApprove, "approve", PullRequestBodyDigest("title", "body")
	if err := store.Record(approval); err == nil {
		t.Fatal("broken persistent write was acknowledged")
	}
	if got, ok := store.For("org/repo", 1, "head"); !ok || got.IsApproval() || got.Body != original.Body {
		t.Fatalf("failed approval leaked into applied-replay lookup: %+v %v", got, ok)
	}
	if got, ok := NewHumanChangeRequests(path).For("org/repo", 1, "head"); !ok || got.IsApproval() {
		t.Fatalf("failed approval lost the prior disk record: %+v %v", got, ok)
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(approval); err != nil {
		t.Fatal(err)
	}
	if got, ok := NewHumanChangeRequests(path).For("org/repo", 1, "head"); !ok || !got.IsApproval() || got.BodyDigest != approval.BodyDigest {
		t.Fatalf("healthy retry did not persist native approval identity: %+v %v", got, ok)
	}
}
