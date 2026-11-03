package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntegrationProtectionRequiresReadableNonBypassingEquivalentPolicy(t *testing.T) {
	for _, scenario := range []string{
		"valid", "organization", "duplicate-ruleset", "unprotected", "branch-unreadable", "rules-unreadable",
		"truncated", "native", "missing-id", "wrong-repository", "wrong-organization", "unsupported-authority",
		"metadata-unreadable", "metadata-inactive", "metadata-incomplete", "ruleset-bypass", "classic-unreadable",
		"classic-bypass", "empty-protection", "metadata-budget",
	} {
		t.Run(scenario, func(t *testing.T) {
			metadataReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("protection observer attempted %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				var value any
				switch {
				case strings.Contains(r.URL.Path, "/rulesets/"):
					metadataReads++
					if scenario == "metadata-unreadable" {
						http.Error(w, "unreadable", http.StatusForbidden)
						return
					}
					metadata := map[string]any{"enforcement": "active", "bypass_actors": []any{}}
					switch scenario {
					case "metadata-inactive":
						metadata["enforcement"] = "evaluate"
					case "metadata-incomplete":
						delete(metadata, "bypass_actors")
					case "ruleset-bypass":
						metadata["bypass_actors"] = []any{map[string]any{"actor_id": 7, "actor_type": "Integration"}}
					}
					value = metadata
				case strings.HasSuffix(r.URL.Path, "/protection"):
					if scenario == "classic-unreadable" {
						http.NotFound(w, r)
						return
					}
					classic := map[string]any{
						"url":                           "https://api.github.com" + r.URL.Path,
						"required_status_checks":        map[string]any{"strict": true, "contexts": []string{"tests"}},
						"required_pull_request_reviews": map[string]any{"required_approving_review_count": 1},
					}
					if strings.Contains(r.URL.Path, "changed-policy") {
						classic["required_status_checks"] = map[string]any{"strict": true, "contexts": []string{"tests", "security"}}
					}
					if scenario == "classic-bypass" {
						classic["required_pull_request_reviews"] = map[string]any{
							"bypass_pull_request_allowances": map[string]any{"apps": []any{map[string]any{"id": 7}}},
						}
					}
					if scenario == "empty-protection" {
						classic = map[string]any{}
					}
					value = classic
				case strings.Contains(r.URL.Path, "/rules/branches/"):
					if scenario == "rules-unreadable" {
						http.Error(w, "unreadable", http.StatusForbidden)
						return
					}
					rule := map[string]any{"type": "required_status_checks", "ruleset_id": 1, "ruleset_source": "o/r", "ruleset_source_type": "Repository"}
					switch scenario {
					case "native":
						rule["type"] = "merge_queue"
					case "missing-id":
						delete(rule, "ruleset_id")
					case "wrong-repository":
						rule["ruleset_source"] = "o/other"
					case "organization":
						rule["ruleset_source"], rule["ruleset_source_type"] = "o", "Organization"
					case "wrong-organization":
						rule["ruleset_source"], rule["ruleset_source_type"] = "other", "Organization"
					case "unsupported-authority":
						rule["ruleset_source_type"] = "Enterprise"
					}
					rules := []any{rule}
					switch scenario {
					case "truncated":
						rules = make([]any, integrationProtectionMaxRules)
					case "duplicate-ruleset":
						rules = append(rules, rule)
					case "empty-protection":
						rules = []any{}
					case "metadata-budget":
						rules = nil
						for index := range integrationReconcileProtectionRuleLimit + 1 {
							rules = append(rules, map[string]any{"type": "required_status_checks", "ruleset_id": index + 1,
								"ruleset_source": "o/r", "ruleset_source_type": "Repository"})
						}
					}
					value = rules
				default:
					if scenario == "branch-unreadable" {
						http.NotFound(w, r)
						return
					}
					value = map[string]any{"protected": scenario != "unprotected"}
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			digest, err := client.ReadIntegrationProtectionDigestForActor(context.Background(), "o/r", "v5", 7)
			valid := scenario == "valid" || scenario == "organization" || scenario == "duplicate-ruleset"
			if (err == nil) != valid || valid && !integrationProducerDigest(digest) {
				t.Fatalf("scenario=%s digest=%q err=%v", scenario, digest, err)
			}
			if scenario == "duplicate-ruleset" && metadataReads != 1 {
				t.Fatalf("shared ruleset metadata was not deduplicated: %d", metadataReads)
			}
			if scenario == "valid" {
				probe, err := client.ReadIntegrationProtectionDigest(context.Background(), "o/r", "hive-integration-qualification/probe")
				if err != nil || probe != digest {
					t.Fatalf("branch-specific URL fabricated a policy difference: %s %s %v", digest, probe, err)
				}
				changed, err := client.ReadIntegrationProtectionDigestForActor(context.Background(), "o/r", "changed-policy", 7)
				if err != nil || changed == digest {
					t.Fatalf("changed required contexts retained qualification: %s %s %v", digest, changed, err)
				}
			}
		})
	}
	var missing *Client
	if _, err := missing.ReadIntegrationProtectionDigest(context.Background(), "o/r", "v5"); err == nil {
		t.Fatal("missing protection reader passed")
	}
	client := testClient(t, "http://127.0.0.1:1")
	if _, err := client.ReadIntegrationProtectionDigest(context.Background(), "invalid", "v5"); err == nil {
		t.Fatal("invalid destination passed")
	}
}
