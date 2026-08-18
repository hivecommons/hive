package repair

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetirePullRequestForVerificationTransitionsExactCheckpointIdempotently(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "repair"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	head := strings.Repeat("b", 40)
	retirement := PullRequestRetirement{
		RepositoryFingerprint: fingerprint,
		PRNumber:              17,
		PRURL:                 "https://github.com/owner/repo/pull/17",
		Branch:                "hive/repair-proof-a1",
		CommitSHA:             head,
	}
	if err := store.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: retirement.Branch, Worktree: "worktree", Stage: StagePROpen, Provider: "codex",
		LifecycleStarted: true, AttemptCounted: true, LifecyclePROpen: true,
		CommitSHA: head, PRNumber: retirement.PRNumber, PRURL: retirement.PRURL,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidatePullRequestRetirement(retirement); err != nil {
		t.Fatal(err)
	}
	if err := store.RetirePullRequestForVerification(retirement); err != nil {
		t.Fatal(err)
	}
	retired, exists := store.Get(fingerprint)
	if !exists || retired.Stage != StageNoChange || retired.LifecyclePROpen ||
		retired.PRNumber != retirement.PRNumber || retired.PRURL != retirement.PRURL ||
		retired.Branch != retirement.Branch || retired.CommitSHA != head {
		t.Fatalf("retired checkpoint = %+v, exists=%t", retired, exists)
	}
	if err := store.ValidatePullRequestRetirement(retirement); err != nil {
		t.Fatalf("validate replay: %v", err)
	}
	if err := store.RetirePullRequestForVerification(retirement); err != nil {
		t.Fatalf("retirement replay: %v", err)
	}
}

func TestRetirePullRequestForVerificationRejectsIdentityDrift(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "repair"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("c", 64)
	head := strings.Repeat("d", 40)
	if err := store.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: "hive/repair-proof-a1", Worktree: "worktree", Stage: StagePROpen, Provider: "codex",
		LifecycleStarted: true, AttemptCounted: true, LifecyclePROpen: true,
		CommitSHA: head, PRNumber: 17, PRURL: "https://github.com/owner/repo/pull/17",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	wrong := PullRequestRetirement{
		RepositoryFingerprint: fingerprint, PRNumber: 18,
		PRURL: "https://github.com/owner/repo/pull/18", Branch: "hive/repair-proof-a1", CommitSHA: head,
	}
	if err := store.ValidatePullRequestRetirement(wrong); err == nil {
		t.Fatal("identity drift was accepted")
	}
	active, _ := store.Get(fingerprint)
	if active.Stage != StagePROpen || !active.LifecyclePROpen || active.PRNumber != 17 {
		t.Fatalf("rejected retirement mutated checkpoint: %+v", active)
	}
}
