package integrated

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestHostedOperatorAuthorityRequiresExactWorkflowRunAndGeneratedBytes(t *testing.T) {
	workflowConfig := hostedWorkflowFixture()
	config := Config{
		Repository: workflowConfig.Repository, RepositoryID: workflowConfig.RepositoryID, DefaultBranch: workflowConfig.DefaultBranch,
		ExecutionMode: ExecutionHosted, HostedSchedule: workflowConfig.ScheduleCron, HostedStateBranch: workflowConfig.StateBranch,
		Automation:            AutomationRepairPR,
		HiveReleaseRepository: workflowConfig.ReleaseRepository, HiveReleaseVersion: workflowConfig.ReleaseTag,
		HiveCommit: workflowConfig.HiveCommit, VisualHiveRef: workflowConfig.VisualHiveCommit, DistributionManifestSHA256: workflowConfig.DistributionManifestSHA256,
		HostedControllerProtocol: HostedControllerProtocol,
	}
	expectedWorkflow, err := GenerateHostedControllerWorkflow(workflowConfig)
	if err != nil {
		t.Fatal(err)
	}
	config = bindHostedWorkflowDigest(t, config)
	actorType := "User"
	workflowBytes := expectedWorkflow
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/repos/"+config.Repository+"/actions/runs/777":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 777, "event": "workflow_dispatch", "run_attempt": 1, "head_branch": config.DefaultBranch,
				"head_sha": strings.Repeat("1", 40), "path": HostedControllerWorkflowPath,
				"display_title":    "Hive approve-merge-plan request-777",
				"repository":       map[string]any{"id": 123456789, "full_name": config.Repository},
				"actor":            map[string]any{"id": 42, "login": "operator", "type": actorType},
				"triggering_actor": map[string]any{"id": 42, "login": "operator", "type": actorType},
			})
		case strings.Contains(request.URL.Path, "/contents/"):
			if request.URL.Query().Get("ref") != strings.Repeat("1", 40) {
				t.Errorf("workflow ref = %q", request.URL.Query().Get("ref"))
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"type": "file", "path": HostedControllerWorkflowPath, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(workflowBytes))})
		case request.URL.Path == "/repos/"+config.Repository+"/collaborators/operator/permission":
			_, _ = io.WriteString(writer, `{"permission":"maintain","user":{"id":42,"login":"operator","type":"User"}}`)
		case request.URL.Path == "/users/operator":
			_, _ = io.WriteString(writer, `{"id":42,"login":"operator","type":"User"}`)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClient("token", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), server.URL)
	proof := HostedOperatorProof{
		Config: config, Operation: "approve-merge-plan", RequestID: "request-777", RequestSHA256: strings.Repeat("2", 64),
		DefaultHeadSHA: strings.Repeat("1", 40), RunHeadSHA: strings.Repeat("1", 40), RunID: 777, EventActorID: 42, EventActorLogin: "operator",
		RequestedActor: HostedOperatorIdentity{ID: 42, Login: "operator"},
	}
	authority, err := VerifyHostedOperatorAuthority(context.Background(), client, proof)
	if err != nil {
		t.Fatalf("verify exact hosted authority: %v", err)
	}
	identity, err := DescribeHostedOperatorAuthority(authority)
	if err != nil || identity.ID != 42 || identity.Login != "operator" {
		t.Fatalf("describe authority: %+v %v", identity, err)
	}
	resolved, err := resolveOperatorIdentity(context.Background(), client, authority, config.Repository, proof.Operation)
	if err != nil || resolved.ID != 42 {
		t.Fatalf("consume operation-bound authority: %+v %v", resolved, err)
	}
	if _, err := resolveOperatorIdentity(context.Background(), client, authority, config.Repository, "approve-merge-apply"); err == nil {
		t.Fatal("opaque authority was accepted for a different operation")
	}

	actorType = "Bot"
	if _, err := VerifyHostedOperatorAuthority(context.Background(), client, proof); err == nil {
		t.Fatal("bot workflow actor minted hosted human authority")
	}
	actorType = "User"
	workflowBytes += "\n# changed"
	if _, err := VerifyHostedOperatorAuthority(context.Background(), client, proof); err == nil {
		t.Fatal("changed workflow bytes minted hosted authority")
	}
}
