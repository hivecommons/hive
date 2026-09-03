package hooks

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestReviewRejectedEndToEndCarriesModelMetadata is the acceptance test for the
// first shipped hook (RFC #4001's bluefin fleet-owner ask). It drives the FULL
// pipeline exactly as production would — operator YAML → validated registry →
// emitter → post-commit dispatch → notification → audit — and asserts the thing
// the ask is actually about: the notification names the model that produced the
// rejected output and links its tuning knob, so the owner does not have to hunt
// through the admin UI.
func TestReviewRejectedEndToEndCarriesModelMetadata(t *testing.T) {
	// 1. The operator's config, as it would appear in the PVC hive.yaml.
	cfg := &config.Config{Hooks: []config.HookRule{{
		Name:   "review-rejected-notify",
		On:     "review_rejected",
		Action: "notify",
		Params: map[string]string{"priority": "high"},
	}}}

	reg, err := CompileFromConfig(cfg)
	if err != nil {
		t.Fatalf("the shipped default hook must validate: %v", err)
	}

	notifier := &fakeNotifier{}
	audit := &fakeAudit{}
	d := NewDispatcher(reg, quietLogger(), WithNotifier(notifier), WithAuditSink(audit))

	// 2. A human rejects a review whose output came from a stale pinned model.
	EmitReviewRejected(context.Background(), d, ReviewRejection{
		Agent:            "reviewer",
		Repo:             "hivecommons/hive",
		PRNumber:         4001,
		Actor:            "fleet-owner",
		Reason:           "hallucinated the API surface",
		Backend:          "anthropic",
		Model:            "claude-opus-4",
		Pin:              "20240229",
		ACMMLevel:        4,
		DashboardBaseURL: "https://hive.example.com",
	})
	d.Wait()

	// 3. Exactly one notification, at the configured priority.
	sent := notifier.all()
	if len(sent) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(sent))
	}
	n := sent[0]
	if n.priority != "high" {
		t.Errorf("priority param not honored: %q", n.priority)
	}
	if !strings.Contains(n.title, "reviewer") {
		t.Errorf("title should name the agent, got %q", n.title)
	}

	// 4. THE POINT OF THE HOOK: the body carries the producing model, its pin,
	// and a working deep link to the knob.
	for _, want := range []string{
		"claude-opus-4",                // the model that produced the bad output
		"anthropic",                    // its backend
		"20240229",                     // the stale pin
		"hallucinated the API surface", // the rejection reason
		"hivecommons/hive#4001",        // what was rejected
		"fleet-owner",                  // who rejected it
	} {
		if !strings.Contains(n.message, want) {
			t.Errorf("notification body missing %q\nbody:\n%s", want, n.message)
		}
	}

	wantKnob := "https://hive.example.com/agents?agent=reviewer&focus=model"
	if !strings.Contains(n.message, wantKnob) {
		t.Errorf("notification must deep-link the model knob %q\nbody:\n%s", wantKnob, n.message)
	}

	// 5. The firing is audited WITH the model metadata, so the audit log alone
	// answers "which model produced the rejected output".
	entries := audit.withAction(AuditHookFired)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	for k, want := range map[string]string{
		"model": "claude-opus-4", "backend": "anthropic", "pin": "20240229",
		"transition": "review_rejected", "hook": "review-rejected-notify",
	} {
		if got, _ := entries[0].fields[k].(string); got != want {
			t.Errorf("audit field %q: got %q, want %q", k, got, want)
		}
	}
}

// TestReviewRejectedPayloadShape pins the emitter's mapping, including that a
// human rejection is world-caused (depth 0) and so is allowed to fire hooks.
func TestReviewRejectedPayloadShape(t *testing.T) {
	p := NewReviewRejectedPayload(ReviewRejection{
		Agent: "reviewer", Repo: "o/r", PRNumber: 7, Actor: "human",
		Reason: "wrong", Backend: "anthropic", Model: "m", Pin: "p", ACMMLevel: 3,
		DashboardBaseURL: "https://h.example.com",
	})

	if p.Transition != TransitionReviewRejected {
		t.Errorf("wrong transition: %q", p.Transition)
	}
	if p.Causation.Depth != 0 {
		t.Errorf("a human rejection is world-caused; depth must be 0, got %d", p.Causation.Depth)
	}
	if p.Model != "m" || p.Backend != "anthropic" || p.Pin != "p" || p.ACMMLevel != 3 {
		t.Errorf("model metadata lost: %+v", p)
	}
	if p.attr(AttrPR) != "7" {
		t.Errorf("PR number should be in attrs, got %q", p.attr(AttrPR))
	}
	if p.attr(AttrModelKnobURL) == "" {
		t.Error("knob URL should be precomputed into attrs")
	}
}

// TestModelKnobURLEscapesAgentName: an agent name with URL-significant
// characters must not corrupt or truncate the link.
func TestModelKnobURLEscapesAgentName(t *testing.T) {
	got := ModelKnobURL("https://h.example.com", "weird name&focus=evil")
	if strings.Contains(got, "weird name") {
		t.Errorf("agent name must be escaped, got %q", got)
	}
	if strings.Count(got, "focus=model") != 1 || strings.Contains(got, "focus=evil") {
		t.Errorf("unescaped input altered the query, got %q", got)
	}
}

func TestModelKnobURLDegradesGracefully(t *testing.T) {
	if got := ModelKnobURL("", "reviewer"); got != "" {
		t.Errorf("no base URL should yield no link, got %q", got)
	}
	if got := ModelKnobURL("https://h.example.com", ""); got != "" {
		t.Errorf("no agent should yield no link, got %q", got)
	}
	// A trailing slash must not double up.
	if got := ModelKnobURL("https://h.example.com/", "a"); strings.Contains(got, "com//") {
		t.Errorf("trailing slash mishandled: %q", got)
	}
}

// TestReviewRejectedWithoutDashboardURLStillNotifies: missing config degrades
// the notification, it does not break it.
func TestReviewRejectedWithoutDashboardURLStillNotifies(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "n", On: TransitionReviewRejected, Action: ActionNotify,
		}),
		quietLogger(), WithNotifier(notifier))

	EmitReviewRejected(context.Background(), d, ReviewRejection{
		Agent: "reviewer", Model: "claude-opus-4", Pin: "20240229",
		// No DashboardBaseURL.
	})
	d.Wait()

	sent := notifier.all()
	if len(sent) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(sent))
	}
	if !strings.Contains(sent[0].message, "claude-opus-4") {
		t.Errorf("model should still be named without a base URL:\n%s", sent[0].message)
	}
	if strings.Contains(sent[0].message, "Adjust the model pin") {
		t.Errorf("no link should be offered when none can be built:\n%s", sent[0].message)
	}
}

func TestEmitReviewRejectedNilDispatcherIsSafe(t *testing.T) {
	EmitReviewRejected(context.Background(), nil, ReviewRejection{Agent: "a"})
}

func TestDescribeModelRendering(t *testing.T) {
	cases := []struct {
		name string
		p    Payload
		want string
	}{
		{"full", Payload{Backend: "anthropic", Model: "m", Pin: "p"}, "anthropic/m (pin: p)"},
		{"no pin", Payload{Backend: "anthropic", Model: "m"}, "anthropic/m"},
		{"model only", Payload{Model: "m"}, "m"},
		{"backend only", Payload{Backend: "anthropic"}, "anthropic"},
		{"empty", Payload{}, ""},
	}
	for _, tc := range cases {
		if got := describeModel(tc.p); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestGenericNotificationBodyForOtherTransitions checks the non-review
// rendering path still produces something useful.
func TestGenericNotificationBodyForOtherTransitions(t *testing.T) {
	body := defaultNotificationBody(Payload{
		Transition: TransitionGovernorModeChange,
		From:       "normal", To: "conserve", Reason: "budget",
	})
	for _, want := range []string{"governor_mode_change", "normal", "conserve", "budget"} {
		if !strings.Contains(body, want) {
			t.Errorf("generic body missing %q:\n%s", want, body)
		}
	}

	// A one-sided transition still reads.
	oneSided := defaultNotificationBody(Payload{Transition: TransitionUpgradePause, To: "on"})
	if !strings.Contains(oneSided, "—") {
		t.Errorf("expected a dash for the empty side:\n%s", oneSided)
	}
}
