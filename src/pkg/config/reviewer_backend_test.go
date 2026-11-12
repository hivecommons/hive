package config

import (
	"strings"
	"testing"
)

// reviewerTurnGovernor is a governor block with an optional reviewer
// endpoint and model, and an optional governor.reviewer_backend.
func reviewerTurnGovernor(t *testing.T, endpoint, model, backend string) *GovernorConfig {
	t.Helper()
	t.Setenv(LiteLLMEndpointEnv, "")
	g := &GovernorConfig{ReviewerBackend: backend}
	g.Trajectory.Endpoint = endpoint
	g.Trajectory.Model = model
	return g
}

// The selection matrix: each row would fail on the old code, where every
// reviewer turn took the endpoint or nothing, except the endpoint-set row,
// which pins that the HTTP path is unchanged.
func TestResolveReviewerTurnSelection(t *testing.T) {
	cases := []struct {
		name               string
		endpoint, model    string
		backend            string
		signedIn           bool
		wantPath           string
		wantReady          bool
		wantReasonContains string
	}{
		{name: "endpoint set, auto: http", endpoint: "https://llm.example.com/", model: "m1", backend: "", signedIn: true,
			wantPath: ReviewerPathHTTP, wantReady: true},
		{name: "no endpoint, signed in, auto: claude-cli", backend: "auto", signedIn: true,
			wantPath: ReviewerPathClaudeCLI, wantReady: true},
		{name: "no endpoint, not signed in, auto: none", backend: "", signedIn: false,
			wantPath: ReviewerPathNone, wantReasonContains: "Claude is not signed in"},
		{name: "explicit http, no endpoint: none, never the CLI", backend: "http", signedIn: true,
			wantPath: ReviewerPathNone, wantReasonContains: "no reviewer endpoint"},
		{name: "explicit claude-cli, endpoint set: claude-cli", endpoint: "https://llm.example.com", model: "m1", backend: "claude-cli", signedIn: true,
			wantPath: ReviewerPathClaudeCLI, wantReady: true},
		{name: "explicit claude-cli, not signed in", backend: "Claude-CLI", signedIn: false,
			wantPath: ReviewerPathClaudeCLI, wantReasonContains: "not signed in"},
		{name: "endpoint without a model: http, not ready", endpoint: "https://llm.example.com", backend: "auto", signedIn: true,
			wantPath: ReviewerPathHTTP, wantReasonContains: "no model"},
		{name: "unknown backend: none", backend: "copilot-cli", signedIn: true,
			wantPath: ReviewerPathNone, wantReasonContains: "copilot-cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := reviewerTurnGovernor(t, tc.endpoint, tc.model, tc.backend)
			got := g.ResolveReviewerTurn("", tc.signedIn)
			if got.Path != tc.wantPath || got.Ready != tc.wantReady {
				t.Fatalf("route = %+v, want path %s ready %v", got, tc.wantPath, tc.wantReady)
			}
			if tc.wantReady && got.Reason != "" {
				t.Fatalf("a ready route carries no reason: %+v", got)
			}
			if !strings.Contains(got.Reason, tc.wantReasonContains) {
				t.Fatalf("reason %q does not say %q", got.Reason, tc.wantReasonContains)
			}
			if got.Path != ReviewerPathHTTP && (got.Endpoint != "" || got.APIKey != "") {
				t.Fatalf("only the http path carries the endpoint and key: %+v", got)
			}
		})
	}
}

// The HTTP route is the one ResolveReviewer gave before: the trimmed
// endpoint, the reviewer model, and the feature's override over it.
func TestResolveReviewerTurnHTTPRouteAndModel(t *testing.T) {
	g := reviewerTurnGovernor(t, "https://llm.example.com/", "reviewer-model", "")
	got := g.ResolveReviewerTurn("", false)
	if got.Endpoint != "https://llm.example.com" || got.Model != "reviewer-model" {
		t.Fatalf("http route = %+v", got)
	}
	if got := g.ResolveReviewerTurn("  feature-model ", false); got.Model != "feature-model" {
		t.Fatalf("override not applied: %+v", got)
	}

	cli := reviewerTurnGovernor(t, "", "", "claude-cli")
	if got := cli.ResolveReviewerTurn("", true); got.Model != "" {
		t.Fatalf("no model on the CLI path keeps the CLI default, got %q", got.Model)
	}
	if got := cli.ResolveReviewerTurn("sonnet", true); got.Model != "sonnet" {
		t.Fatalf("CLI path model = %q, want the override", got.Model)
	}
}

// ResolveReviewerTurnVia forces a path whatever governor.reviewer_backend says.
func TestResolveReviewerTurnViaForcesHTTP(t *testing.T) {
	g := reviewerTurnGovernor(t, "", "", "claude-cli")
	if got := g.ResolveReviewerTurnVia(ReviewerBackendHTTP, "", true); got.Path != ReviewerPathNone || got.Ready {
		t.Fatalf("an HTTP-only turn with no endpoint must not run: %+v", got)
	}
}

func TestReviewerBackendValidation(t *testing.T) {
	for _, ok := range []string{"", "auto", "http", "claude-cli", " HTTP "} {
		g := &GovernorConfig{ReviewerBackend: ok}
		if err := g.validateReviewerBackend(); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	g := &GovernorConfig{ReviewerBackend: "copilot-cli"}
	err := g.validateReviewerBackend()
	if err == nil || !strings.Contains(err.Error(), "governor.reviewer_backend") || !strings.Contains(err.Error(), "claude-cli") {
		t.Fatalf("copilot-cli must be refused with the accepted values, got %v", err)
	}
	if got := (&GovernorConfig{}).ReviewerBackendOrDefault(); got != ReviewerBackendAuto {
		t.Fatalf("default = %q, want auto", got)
	}
}

// Validate refuses a bad governor.reviewer_backend at load.
func TestValidateRefusesUnknownReviewerBackend(t *testing.T) {
	cfg := validatableConfig(nil)
	cfg.Governor.ReviewerBackend = "copilot-cli"
	err := cfg.ValidateWithOptions(ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "governor.reviewer_backend") {
		t.Fatalf("Validate = %v, want the reviewer_backend error", err)
	}
}
