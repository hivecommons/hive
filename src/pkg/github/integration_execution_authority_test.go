package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestIntegrationAuthorityRequiresExactImmutableDesignatedExecutor(t *testing.T) {
	for _, scenario := range []string{
		"valid", "observer", "repository-unreadable", "repository-changed", "epoch-unreadable", "epoch-changed",
		"commit-unreadable", "commit-changed", "missing-record", "ambiguous-record", "corrupt-record", "disabled",
		"wrong-scope", "wrong-policy", "wrong-executor", "wrong-qualification", "unsupported-model",
	} {
		t.Run(scenario, func(t *testing.T) {
			scope := config.IntegrationExecutionScope{
				Repo: "o/r", Base: "v5", RepositoryID: 1, ExecutorID: strings.Repeat("a", 64),
				Policy: strings.Repeat("b", 64), QualificationID: "isolated-probe",
				AuthorityRef: "refs/heads/hive-integration-authority/v5", AuthorityCommit: strings.Repeat("c", 40),
			}
			record := IntegrationExecutionAuthority{Version: 2, Kind: "integration-executor-single-v1",
				Repo: scope.Repo, Base: scope.Base, Enabled: true, ExecutorID: scope.ExecutorID,
				Policy: scope.Policy, QualificationID: scope.QualificationID, Model: "designated-single-executor-no-failover"}
			switch scenario {
			case "disabled":
				record.Enabled = false
			case "wrong-scope":
				record.Base = "other"
			case "wrong-policy":
				record.Policy = strings.Repeat("d", 64)
			case "wrong-executor":
				record.ExecutorID = strings.Repeat("d", 64)
			case "wrong-qualification":
				record.QualificationID = "unproved"
			case "unsupported-model":
				record.Model = "ttl-failover"
			}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			message := "Operator enrollment\n" + string(body)
			switch scenario {
			case "missing-record":
				message = "No record"
			case "ambiguous-record":
				message += "\n" + string(body)
			case "corrupt-record":
				message = "{"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("authority observer attempted %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				var value any
				switch {
				case strings.Contains(r.URL.Path, "/git/commits/"):
					if scenario == "commit-unreadable" {
						http.NotFound(w, r)
						return
					}
					sha := scope.AuthorityCommit
					if scenario == "commit-changed" {
						sha = strings.Repeat("d", 40)
					}
					value = map[string]any{"sha": sha, "message": message}
				case strings.Contains(r.URL.Path, "/git/ref/"):
					if scenario == "epoch-unreadable" {
						http.NotFound(w, r)
						return
					}
					sha := scope.AuthorityCommit
					if scenario == "epoch-changed" {
						sha = strings.Repeat("d", 40)
					}
					value = map[string]any{"object": map[string]any{"sha": sha}}
				default:
					if scenario == "repository-unreadable" {
						http.NotFound(w, r)
						return
					}
					name := scope.Repo
					if scenario == "repository-changed" {
						name = "o/renamed"
					}
					value = map[string]any{"id": scope.RepositoryID, "full_name": name}
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			executor := scope.ExecutorID
			if scenario == "observer" {
				executor = strings.Repeat("d", 64)
			}
			allowed, err := client.ReadIntegrationExecutionAuthority(context.Background(), scope, executor)
			if scenario == "valid" {
				if err != nil || !allowed {
					t.Fatalf("designated executor refused: %v %v", allowed, err)
				}
			} else if allowed || scenario != "observer" && err == nil {
				t.Fatalf("unsupported authority acquired execution: %v %v", allowed, err)
			}
		})
	}
	var missing *Client
	if _, err := missing.ReadIntegrationExecutionAuthority(context.Background(), config.IntegrationExecutionScope{}, ""); err == nil {
		t.Fatal("missing forge reader passed")
	}
	if _, err := testClient(t, "http://127.0.0.1:1").ReadIntegrationExecutionAuthority(context.Background(), config.IntegrationExecutionScope{}, ""); err == nil {
		t.Fatal("incomplete enrollment passed")
	}
	actor := &Client{appAuth: &AppAuth{appID: 1, installationID: 2}}
	if actor.VerifyIntegrationPromotionActor(1, 2) != nil || actor.VerifyIntegrationPromotionActor(1, 3) == nil ||
		(&Client{}).VerifyIntegrationPromotionActor(1, 2) == nil {
		t.Fatal("promotion actor accepted PAT/substituted installation or refused pinned App")
	}
}
