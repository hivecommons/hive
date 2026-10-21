package dashboard

// Tests for the #13379 webhook routing: a `pull_request` opened/synchronize/
// reopened delivery hands the PR to the sentinel hook; the organization-
// scoped `projects_v2_item` delivery is accepted (it carries no repository)
// and routed to the projects work-source patch hook.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGitHubWebhookRoutesPullRequestToSentinel(t *testing.T) {
	s := newGitHubWebhookTestServer(t)
	t.Setenv(githubWebhookSecretEnvVar, "s3cr3t")
	var got []string
	s.deps.SentinelPREvent = func(repo string, number int) { got = append(got, repo+"#"+itoa(number)) }

	for _, tc := range []struct {
		action string
		want   int
	}{
		{"opened", 1}, {"synchronize", 2}, {"reopened", 3}, {"labeled", 3}, {"closed", 3},
	} {
		body := []byte(`{"action":"` + tc.action + `","number":12,"pull_request":{"number":12,"head":{"sha":"abc"}},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
		rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("s3cr3t", body), body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", tc.action, rec.Code, rec.Body.String())
		}
		if len(got) != tc.want {
			t.Fatalf("%s: sentinel calls=%v want %d", tc.action, got, tc.want)
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if _, has := resp["sentinel"]; has != (tc.action == "opened" || tc.action == "synchronize" || tc.action == "reopened") {
			t.Fatalf("%s: response=%v", tc.action, resp)
		}
	}
	for _, g := range got {
		if g != "webhookrepo#12" {
			t.Fatalf("sentinel got %q; want the configured repo slug", g)
		}
	}

	// An unknown repository never reaches the sentinel.
	body := []byte(`{"action":"opened","number":1,"repository":{"full_name":"other/repo"}}`)
	postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("s3cr3t", body), body)
	if len(got) != 3 {
		t.Fatalf("unknown repo reached sentinel: %v", got)
	}
}

func TestGitHubWebhookRoutesProjectsV2Item(t *testing.T) {
	s := newGitHubWebhookTestServer(t)
	t.Setenv(githubWebhookSecretEnvVar, "s3cr3t")
	body := []byte(`{"action":"edited","projects_v2_item":{"id":7,"node_id":"PVTI_x","project_node_id":"PVT_y","content_type":"Issue"},"organization":{"login":"webhookorg"}}`)
	sig := signGitHubWebhookForTest("s3cr3t", body)

	// No hook: accepted and ignored, not "unknown repository".
	rec := postGitHubWebhook(s, "projects_v2_item", sig, body)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp["status"] != "ignored" || resp["reason"] != "no projects v2 work source" {
		t.Fatalf("no hook: status=%d body=%s", rec.Code, rec.Body.String())
	}

	var seen [][]byte
	s.deps.ProjectsV2ItemEvent = func(payload []byte) WebhookPatchOutcome {
		seen = append(seen, payload)
		return WebhookPatchOutcome{Applied: true, Detail: "edited: upserted"}
	}
	rec = postGitHubWebhook(s, "projects_v2_item", sig, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(seen) != 1 || string(seen[0]) != string(body) {
		t.Fatalf("hook got %d payloads", len(seen))
	}
	resp = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	patch, _ := resp["projects_patch"].(map[string]any)
	if resp["status"] != "ok" || patch["applied"] != true || patch["detail"] != "edited: upserted" {
		t.Fatalf("response=%s", rec.Body.String())
	}

	// Still signature-gated.
	if rec := postGitHubWebhook(s, "projects_v2_item", signGitHubWebhookForTest("wrong", body), body); rec.Code != http.StatusUnauthorized || len(seen) != 1 {
		t.Fatalf("bad signature: status=%d calls=%d", rec.Code, len(seen))
	}
}
