package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// Tests for the org-transfer verdict (#5774), the branch
// classifyGitHubAppRepoCoverage takes before it falls through to
// AppStateRepoNotCovered.
//
// The stakes are the same asymmetry #4360 pinned, one level deeper. A
// not-covered verdict tells an operator to tick the repository in the
// configured org's App installation. After a transfer that instruction cannot
// be followed — the repository has left that account — so the operator spends
// the debugging time this classifier exists to give back, on a settings page
// that can never contain the repo. These tests pin that the transfer shape gets
// its own verdict, and that nothing else does.

// TestClassifyGitHubAppRepoCoverage_TransferGetsItsOwnVerdict is the live
// #5774 shape: the App is installed on the account the repository now lives
// under, while the hive's config still names the account it left.
func TestClassifyGitHubAppRepoCoverage_TransferGetsItsOwnVerdict(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("hivecommons/hive"))

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL),
		"kubestellar", []string{"hive"}, verdictTestLogger())

	if !raise {
		t.Fatal("a hive pointed at an account its repositories have left must raise the banner")
	}
	if state != github.AppStateRepoMoved {
		t.Fatalf("state = %s, want repo-moved — repo-not-covered's remedy cannot be followed after a transfer", state)
	}
	if !strings.Contains(msg, "hivecommons") {
		t.Errorf("msg = %q, want it to name the account the repository moved to", msg)
	}
	if strings.Contains(msg, "repository access") {
		t.Errorf("msg = %q, must not carry the not-covered remedy: there is nothing to tick under 'kubestellar'", msg)
	}
	if !state.UserActionable() {
		t.Error("repo-moved is fixed by repointing the hive's configured org — it must be user-actionable")
	}
	if state.OperatorActionable() {
		t.Error("repo-moved must not be operator-actionable: no key upload can help")
	}
}

// TestClassifyGitHubAppRepoCoverage_OrdinaryScopeGapStillReportsNotCovered is
// the guard that keeps the new branch from swallowing the case #4360 shipped
// for. The installation still covers a repo under the configured org, so the
// account is reachable and one unticked repo is exactly what it looks like.
//
// Without this the transfer branch would be untestably broad: every
// not-covered verdict in the fleet would silently become a "your repo moved"
// accusation naming an unrelated repository that happens to share a name.
func TestClassifyGitHubAppRepoCoverage_OrdinaryScopeGapStillReportsNotCovered(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("acme/widgets", "otherorg/gadgets"))

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL),
		"acme", []string{"widgets", "gadgets"}, verdictTestLogger())

	if !raise {
		t.Fatal("an unticked configured repo must still raise the banner")
	}
	if state != github.AppStateRepoNotCovered {
		t.Fatalf("state = %s, want repo-not-covered — 'acme' is still reachable, so this is a scope gap and not a transfer", state)
	}
	if !strings.Contains(msg, "acme/gadgets") {
		t.Errorf("msg = %q, want it to name the repo that is not ticked", msg)
	}
}

// TestClassifyGitHubAppRepoCoverage_AmbiguousDestinationFallsBack asserts that
// when two accounts own a repository with the configured name, the verdict
// degrades to not-covered rather than picking one.
//
// A coin flip presented as a diagnosis is worse than the vaguer true answer:
// the operator acts on it, and the action is against the wrong org.
func TestClassifyGitHubAppRepoCoverage_AmbiguousDestinationFallsBack(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("hivecommons/hive", "someoneelse/hive"))

	_, _, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL),
		"kubestellar", []string{"hive"}, verdictTestLogger())

	if state != github.AppStateRepoNotCovered {
		t.Fatalf("state = %s, want repo-not-covered — with two candidate destinations, naming one would be a guess", state)
	}
}

// TestClassifyGitHubAppRepoCoverage_FullyCoveredStaysSilentAfterTheChange
// re-pins the healthy path through the new branch. The transfer check runs only
// after Missing has already found something, so a fully covered hive must never
// reach it — and must certainly never be told its repositories moved.
func TestClassifyGitHubAppRepoCoverage_FullyCoveredStaysSilentAfterTheChange(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("kubestellar/hive"))

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL),
		"kubestellar", []string{"hive"}, verdictTestLogger())

	if raise {
		t.Errorf("a fully covered hive raised the banner with %q", msg)
	}
	if state != github.AppStateOK {
		t.Errorf("state = %s, want ok", state)
	}
}
