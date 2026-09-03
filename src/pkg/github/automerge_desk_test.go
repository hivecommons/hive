package github

// The wired producer: the self-authored auto-merge sweep's desk consultation.
//
// The most important property here is the DEFAULT: shipping this code must not
// change any running hive. That is what TestNoDeskInstalledIsNoOp pins.

import (
	"context"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// TestNoDeskInstalledIsNoOp is the default-OFF guarantee. With no hook
// installed, the consultation allows unconditionally, so the sweep behaves
// exactly as it did before the desk existed.
func TestNoDeskInstalledIsNoOp(t *testing.T) {
	c := &Client{}
	allow, reason := c.consultApprovalDesk(context.Background(), ApprovalDeskRequest{
		Kind: ApprovalDeskKindSelfMerge, Repo: "hivecommons/hive", Number: 1,
	})
	if !allow {
		t.Fatalf("with no desk installed the sweep must proceed unchanged; got allow=false reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("no-op consultation produced a reason: %q", reason)
	}

	// A nil client is also safe — the sweep's own nil guards rely on it.
	var nilClient *Client
	if allow, _ := nilClient.consultApprovalDesk(context.Background(), ApprovalDeskRequest{}); !allow {
		t.Error("nil client consultation returned allow=false")
	}
}

// TestSetApprovalDeskInstallsAndRemoves pins the setter, including that passing
// nil restores pre-desk behavior (the operator's escape hatch).
func TestSetApprovalDeskInstallsAndRemoves(t *testing.T) {
	c := &Client{}
	called := false
	c.SetApprovalDesk(func(context.Context, ApprovalDeskRequest) (bool, string) {
		called = true
		return false, "withheld-by-test"
	})

	allow, reason := c.consultApprovalDesk(context.Background(), ApprovalDeskRequest{Number: 1})
	if !called {
		t.Fatal("installed hook was not called")
	}
	if allow || reason != "withheld-by-test" {
		t.Errorf("hook result not honored: allow=%v reason=%q", allow, reason)
	}

	// Removing it restores the no-op.
	c.SetApprovalDesk(nil)
	if allow, _ := c.consultApprovalDesk(context.Background(), ApprovalDeskRequest{Number: 1}); !allow {
		t.Error("SetApprovalDesk(nil) did not restore pre-desk behavior")
	}
}

// TestWithheldHookAlwaysProducesAReason pins that a refusal is always
// actionable in the sweep's skip log, never a mystery blank.
func TestWithheldHookAlwaysProducesAReason(t *testing.T) {
	c := &Client{}
	c.SetApprovalDesk(func(context.Context, ApprovalDeskRequest) (bool, string) {
		return false, "" // a sloppy hook
	})
	allow, reason := c.consultApprovalDesk(context.Background(), ApprovalDeskRequest{Number: 1})
	if allow {
		t.Fatal("a refusing hook was treated as allowing")
	}
	if reason == "" {
		t.Error("a refusal with no reason produced an empty skip reason — the sweep log would be unactionable")
	}
}

// TestApprovalDeskReceivesRequestFields pins that the sweep hands the desk the
// fields operator rules are written against — a rule over checks_green or
// author is useless if the producer never populates them.
func TestApprovalDeskReceivesRequestFields(t *testing.T) {
	c := &Client{}
	var got ApprovalDeskRequest
	c.SetApprovalDesk(func(_ context.Context, in ApprovalDeskRequest) (bool, string) {
		got = in
		return true, ""
	})

	want := ApprovalDeskRequest{
		Kind:        ApprovalDeskKindSelfMerge,
		Repo:        "hivecommons/hive",
		Number:      4000,
		Author:      "hive-app[bot]",
		Title:       "fix: a thing",
		Labels:      []string{"kind/bug"},
		ChecksGreen: true,
		HeadSHA:     "deadbeef",
	}
	c.consultApprovalDesk(context.Background(), want)

	if got.Repo != want.Repo || got.Number != want.Number || got.Author != want.Author {
		t.Errorf("identity fields not passed through: %+v", got)
	}
	if !got.ChecksGreen {
		t.Error("ChecksGreen was not passed through — the canonical dependabot rule depends on it")
	}
	if got.HeadSHA != want.HeadSHA {
		t.Error("HeadSHA was not passed through — a verdict must be attributable to a specific tree")
	}
	if len(got.Labels) != 1 || got.Labels[0] != "kind/bug" {
		t.Errorf("Labels not passed through: %v", got.Labels)
	}
}

// TestLabelNames pins the label flattening, including the nil-element case that
// sparse API responses can produce.
func TestLabelNames(t *testing.T) {
	if got := labelNames(nil); got != nil {
		t.Errorf("labelNames(nil) = %v, want nil", got)
	}

	labels := []*gh.Label{
		{Name: gh.Ptr("kind/bug")},
		nil, // must not panic
		{Name: gh.Ptr("")},
		{Name: gh.Ptr("hold")},
	}
	got := labelNames(labels)
	if len(got) != 2 || got[0] != "kind/bug" || got[1] != "hold" {
		t.Errorf("labelNames = %v, want [kind/bug hold] (nil and empty skipped)", got)
	}
}
