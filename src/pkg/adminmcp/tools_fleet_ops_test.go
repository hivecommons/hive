package adminmcp

import (
	"context"
	"strings"
	"testing"
)

// plan.propose without a title named its target "<nil>" in the summary and in
// the confirmation message the operator approves.
func TestPlanProposeNamesTheIssueWhenUntitled(t *testing.T) {
	preview, err := planProposeOp{}.Preview(context.Background(), map[string]any{"repo": "org/repo", "number": float64(12)})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Target != "org/repo#12" {
		t.Fatalf("target = %q, want org/repo#12", preview.Target)
	}
	for name, text := range map[string]string{"summary": preview.Summary, "confirmation": preview.ConfirmationMessage} {
		if strings.Contains(text, "<nil>") || !strings.Contains(text, "org/repo#12") {
			t.Errorf("%s = %q, want it to name org/repo#12", name, text)
		}
	}
	body, _ := preview.Request.Body.(map[string]any)
	if body["repo"] != "org/repo" || body["number"] != 12 {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	if _, sent := body["title"]; sent {
		t.Fatalf("an untitled proposal sent a title: %#v", body)
	}
}

// The endpoint resolves an untitled proposal by issue url as well as by repo
// and number, so a url alone is a valid proposal and names its target.
func TestPlanProposeAcceptsURLAlone(t *testing.T) {
	const issueURL = "https://github.com/org/repo/issues/12"
	preview, err := planProposeOp{}.Preview(context.Background(), map[string]any{"url": issueURL})
	if err != nil {
		t.Fatalf("url-only proposal refused: %v", err)
	}
	if preview.Target != issueURL || !strings.Contains(preview.ConfirmationMessage, issueURL) {
		t.Fatalf("target = %q, confirmation = %q", preview.Target, preview.ConfirmationMessage)
	}
	if body, _ := preview.Request.Body.(map[string]any); body["url"] != issueURL {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
}

func TestPlanProposeTitleStillWins(t *testing.T) {
	preview, err := planProposeOp{}.Preview(context.Background(), map[string]any{"repo": "org/repo", "number": float64(12), "url": "https://example.test/i/12", "title": "Plan this"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Target != "Plan this" {
		t.Fatalf("target = %q, want the title", preview.Target)
	}
}

func TestPlanProposeRefusesWhatTheEndpointCannotResolve(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"nothing":         {},
		"body only":       {"body": "details"},
		"repo only":       {"repo": "org/repo"},
		"number only":     {"number": float64(12)},
		"zero number":     {"repo": "org/repo", "number": float64(0)},
		"negative number": {"repo": "org/repo", "number": float64(-3)},
		"blank title":     {"title": "   "},
	} {
		if _, err := (planProposeOp{}).Preview(context.Background(), args); err == nil {
			t.Errorf("%s: preview accepted a proposal the endpoint cannot resolve", name)
		}
	}
}
