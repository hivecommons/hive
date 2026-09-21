package main

// Tests for applyHumanDecisionLabels (main.go), the wiring that mirrors the
// review swarm's "a human must decide" holds onto the operator's triage label.
// The pkg/github client method (ApplyHumanDecisionLabel) is already covered;
// these tests pin the glue above it: nil/config gating, the already-labeled
// skip (case-insensitive, whitespace-tolerant, derived from the actionable
// snapshot), the apply path, and the error-continue posture — one refused
// label must not stop the remaining holds from being applied.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// humanLabelServer is a fake GitHub API that defines the given labels on
// acme/widget and records every add-labels call by PR number.
type humanLabelServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	applied []int // PR numbers that received an add-labels call
	fail    map[int]bool
}

func newHumanLabelServer(t *testing.T, definedLabel string) *humanLabelServer {
	t.Helper()
	h := &humanLabelServer{fail: map[int]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/labels/", func(w http.ResponseWriter, r *http.Request) {
		if definedLabel == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": definedLabel})
	})
	mux.HandleFunc("/repos/acme/widget/issues/", func(w http.ResponseWriter, r *http.Request) {
		var number int
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/acme/widget/issues/%d/labels", &number); err != nil {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.mu.Lock()
		fail := h.fail[number]
		if !fail {
			h.applied = append(h.applied, number)
		}
		h.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"` + definedLabel + `"}]`))
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *humanLabelServer) appliedPRs() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.applied...)
}

func humanLabelConfig(label string) *config.Config {
	cfg := &config.Config{}
	cfg.Review.HumanDecisionLabel = label
	return cfg
}

func humanHoldPlan(numbers ...int) review.DispatchPlan {
	var plan review.DispatchPlan
	for _, n := range numbers {
		plan.State.Human = append(plan.State.Human, review.HumanReviewHold{
			Repo: "acme/widget", Number: n, Reason: "review fix cap reached",
		})
	}
	return plan
}

func actionableWithLabels(number int, labels ...string) *github.ActionableResult {
	return &github.ActionableResult{
		PRs: github.PRResult{Items: []github.PullRequest{{
			Repo: "acme/widget", Number: number, Labels: labels,
		}}},
	}
}

// A nil config or client must be an inert no-op — this runs inside the main
// loop where either can legitimately be absent.
func TestApplyHumanDecisionLabels_NilInputsAreNoOps(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	applyHumanDecisionLabels(context.Background(), nil, ghClient, nil, humanHoldPlan(1), restoreTestLogger())
	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), nil, nil, humanHoldPlan(1), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 0 {
		t.Fatalf("nil inputs must not reach the API, got calls for %v", got)
	}
}

// An unset label is the default posture and an empty hold list is the common
// case; neither must touch the API.
func TestApplyHumanDecisionLabels_UnsetLabelOrNoHoldsIsSilent(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	applyHumanDecisionLabels(context.Background(), humanLabelConfig("   "), ghClient, nil, humanHoldPlan(1), restoreTestLogger())
	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, nil, review.DispatchPlan{}, restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 0 {
		t.Fatalf("expected no API calls, got calls for %v", got)
	}
}

// The happy path: an unlabeled hold gets the configured label. A nil
// actionable snapshot means nothing is known to be labeled, so it must still
// apply rather than skip.
func TestApplyHumanDecisionLabels_AppliesToUnlabeledHold(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, nil, humanHoldPlan(42), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("expected one apply for PR 42, got %v", got)
	}
}

// Holds persist across cycles: a PR the enumeration already saw carrying the
// label must be skipped, and the match is case-insensitive and
// whitespace-tolerant so GitHub's rendering of the label cannot defeat it.
func TestApplyHumanDecisionLabels_SkipsAlreadyLabeledCaseInsensitive(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	actionable := actionableWithLabels(42, "bug", "  Queue-Triage  ")
	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, actionable, humanHoldPlan(42), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 0 {
		t.Fatalf("already-labeled hold must be skipped, got calls for %v", got)
	}
}

// Live hives configure governor.repos as bare names under project.org, so the
// enumeration reports Repo "widget" while holds (born from review reports)
// carry "acme/widget". The skip must reconcile the two forms; before it did,
// every cycle re-labeled every hold and the "already labeled" test passed
// only because both sides happened to use the same spelling.
func TestApplyHumanDecisionLabels_SkipMatchesBareRepoAgainstFullHold(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	cfg := humanLabelConfig("queue-triage")
	cfg.Project.Org = "acme"
	actionable := actionableWithLabels(42, "queue-triage")
	actionable.PRs.Items[0].Repo = "widget"

	applyHumanDecisionLabels(context.Background(), cfg, ghClient, actionable, humanHoldPlan(42), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 0 {
		t.Fatalf("bare-repo snapshot must satisfy the full-name hold, got calls for %v", got)
	}
}

// A different label on the PR is not the triage label; it must still apply.
func TestApplyHumanDecisionLabels_OtherLabelsDoNotSuppress(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	actionable := actionableWithLabels(42, "bug", "hold")
	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, actionable, humanHoldPlan(42), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("expected one apply for PR 42, got %v", got)
	}
}

// One refused label must not stop the remaining holds: the loop logs and
// continues, so PR 7 failing leaves PR 9 labeled.
func TestApplyHumanDecisionLabels_ErrorContinuesToNextHold(t *testing.T) {
	h := newHumanLabelServer(t, "queue-triage")
	h.fail[7] = true
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, nil, humanHoldPlan(7, 9), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 1 || got[0] != 9 {
		t.Fatalf("expected the surviving apply for PR 9 only, got %v", got)
	}
}

// A label the repo does not define is refused by the client (never minted);
// the wiring must swallow that per-hold and keep going, applying nothing.
func TestApplyHumanDecisionLabels_MissingRepoLabelIsLoggedAndSkipped(t *testing.T) {
	h := newHumanLabelServer(t, "")
	ghClient := github.NewClientForTest(h.srv.URL, "acme", []string{"widget"}, restoreTestLogger())

	applyHumanDecisionLabels(context.Background(), humanLabelConfig("queue-triage"), ghClient, nil, humanHoldPlan(42), restoreTestLogger())

	if got := h.appliedPRs(); len(got) != 0 {
		t.Fatalf("undefined label must not be applied, got calls for %v", got)
	}
}
