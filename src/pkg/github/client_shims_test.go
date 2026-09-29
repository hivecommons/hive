package github

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

// TestExportedClientShims pins the thin exported wrappers the v5 package split
// added for cross-package callers (automerge, convergence). Each simply
// forwards to an internal helper; these tests pin the forwarding and the
// nil-receiver guards so a caller holding a nil *Client cannot panic.
func TestExportedClientShims(t *testing.T) {
	// Package-level pure shims.
	if !IsIgnorableCICheck("Playwright") {
		t.Error("IsIgnorableCICheck should ignore Playwright")
	}
	if IsIgnorableCICheck("test") {
		t.Error("IsIgnorableCICheck must not ignore a real test context")
	}
	if !IsMetaCheck("tide") {
		t.Error("IsMetaCheck(tide) = false, want true")
	}
	if IsMetaCheck("build") {
		t.Error("IsMetaCheck(build) = true, want false")
	}
	if got := ExtractPRLabels([]*gh.Label{{Name: gh.String("bug")}, {Name: gh.String("hold")}}); len(got) != 2 || got[0] != "bug" || got[1] != "hold" {
		t.Errorf("ExtractPRLabels = %v, want [bug hold]", got)
	}
	if got := SafeGetLogin(nil); got != "" {
		t.Errorf("SafeGetLogin(nil) = %q, want empty", got)
	}
	if got := SafeGetLogin(&gh.User{Login: gh.String("octocat")}); got != "octocat" {
		t.Errorf("SafeGetLogin = %q, want octocat", got)
	}
	if MergeableFromState("dirty", nil) != MergeableNo {
		t.Error("MergeableFromState(dirty) should be MergeableNo")
	}

	// Nil-receiver guards: every method must be a safe no-op on a nil client.
	var nilClient *Client
	nilClient.SetMutationBoundary(nil)
	if nilClient.MutationBoundary() != nil {
		t.Error("nil client MutationBoundary should be nil")
	}
	if owner, repo := nilClient.SplitRepo("o/r"); owner != "" || repo != "o/r" {
		t.Errorf("nil client SplitRepo = %q/%q, want \"\"/\"o/r\"", owner, repo)
	}
	if nilClient.AppBotLogin() != "" {
		t.Error("nil client AppBotLogin should be empty")
	}
	nilClient.RecordPRMergedAudit("o/r", 1, "rebase", "sha", PRAuditPathRelay)

	// Real client forwarding.
	c := &Client{org: "defaultorg", repos: []string{"o/r"}, appBotLogin: "hive-bot[bot]", exemptLabels: []string{"exempt-me"}}
	var boundary effects.Boundary
	c.SetMutationBoundary(boundary)
	if c.MutationBoundary() != nil {
		t.Error("MutationBoundary should round-trip the nil boundary")
	}
	if got := c.Repositories(); len(got) != 1 || got[0] != "o/r" {
		t.Errorf("Repositories = %v, want [o/r]", got)
	}
	if owner, repo := c.SplitRepo("owner/name"); owner != "owner" || repo != "name" {
		t.Errorf("SplitRepo(owner/name) = %q/%q", owner, repo)
	}
	if owner, repo := c.SplitRepo("bare"); owner != "defaultorg" || repo != "bare" {
		t.Errorf("SplitRepo(bare) = %q/%q, want defaultorg/bare", owner, repo)
	}
	if got := c.AppBotLogin(); got != "hive-bot[bot]" {
		t.Errorf("AppBotLogin = %q", got)
	}
	if !c.IsExemptLabels([]string{"exempt-me"}) {
		t.Error("IsExemptLabels should match a configured exempt label")
	}
	if c.IsExemptLabels([]string{"ordinary"}) {
		t.Error("IsExemptLabels must not match an ordinary label")
	}
	c.RecordPRMergedAudit("o/r", 7, "rebase", "abc123", PRAuditPathRelay)
}

// TestSelfAuthorizationHelpers pins the pure helpers of the #5117
// self-authorization gate: notice detection (marker and legacy phrase),
// hold-label event classification, and nil-safety.
func TestSelfAuthorizationHelpers(t *testing.T) {
	if !IsSelfAuthorizationHoldNotice(SelfAuthorizationNoticeMarker + " held") {
		t.Error("marker form not recognised")
	}
	if !IsSelfAuthorizationHoldNotice("Held for human sign-off on the direction of this change") {
		t.Error("legacy phrase not recognised")
	}
	if IsSelfAuthorizationHoldNotice("just a normal comment") {
		t.Error("ordinary comment misclassified as hold notice")
	}

	if isHoldLabelEvent(nil) {
		t.Error("nil event classified as hold label event")
	}
	mk := func(event, label string) *gh.IssueEvent {
		return &gh.IssueEvent{Event: gh.String(event), Label: &gh.Label{Name: gh.String(label)}}
	}
	if !isHoldLabelEvent(mk("labeled", "hold")) || !isHoldLabelEvent(mk("unlabeled", "HOLD")) {
		t.Error("hold labeled/unlabeled events not recognised")
	}
	if isHoldLabelEvent(mk("labeled", "bug")) || isHoldLabelEvent(mk("closed", "hold")) {
		t.Error("non-hold event misclassified")
	}

	// Nil-safety of gate plumbing.
	var nilClient *Client
	nilClient.SetSelfAuthorizationHoldEnabled(nil)
	if !nilClient.selfAuthorizationHoldActive("o/r") {
		t.Error("nil client must default the gate to active")
	}
	if _, err := nilClient.HasSelfAuthorizationHoldNotice(context.Background(), "o/r", 1); err == nil {
		t.Error("nil client HasSelfAuthorizationHoldNotice should error")
	}
	c := &Client{}
	c.SetSelfAuthorizationHoldEnabled(func(string) bool { return false })
	if c.selfAuthorizationHoldActive("o/r") {
		t.Error("configured callback must be honored")
	}
}

// TestRetryDelayFromHeaders pins the rate-limit backoff header parsing.
func TestRetryDelayFromHeaders(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if _, ok := retryDelayFromHeaders(nil, now); ok {
		t.Error("nil headers should report no delay")
	}
	h := http.Header{}
	if _, ok := retryDelayFromHeaders(h, now); ok {
		t.Error("empty headers should report no delay")
	}
	h.Set("Retry-After", "30")
	if d, ok := retryDelayFromHeaders(h, now); !ok || d != 30*time.Second {
		t.Errorf("Retry-After seconds = %v/%v, want 30s/true", d, ok)
	}
	h.Set("Retry-After", now.Add(2*time.Minute).UTC().Format(http.TimeFormat))
	if d, ok := retryDelayFromHeaders(h, now); !ok || d <= 0 {
		t.Errorf("Retry-After HTTP date = %v/%v, want positive/true", d, ok)
	}
	h = http.Header{}
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(45*time.Second).Unix(), 10))
	if d, ok := retryDelayFromHeaders(h, now); !ok || d != 45*time.Second {
		t.Errorf("X-RateLimit-Reset = %v/%v, want 45s/true", d, ok)
	}
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10))
	if _, ok := retryDelayFromHeaders(h, now); ok {
		t.Error("past reset epoch should report no delay")
	}
}
