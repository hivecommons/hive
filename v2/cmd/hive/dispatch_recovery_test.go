package main

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestWorkflowDispatchRecoveryStatusProvidesExactPlanCommands(t *testing.T) {
	correlation := strings.Repeat("a", 64)
	status := workflowDispatchRecoveryStatus("state path", integrated.WorkflowDispatchIntent{
		CorrelationID: correlation, RequestDigest: strings.Repeat("b", 64), WorkflowFile: "hive-visual-hive.yml",
		Ref: "main", ExpectedDisplayTitle: "Hive Visual Hive Production [" + correlation + "]",
	})
	if status["state"] != "ambiguous_transport_failure" || status["request_digest"] != strings.Repeat("b", 64) {
		t.Fatalf("recovery status lost request identity: %+v", status)
	}
	for action, field := range map[string]string{"retry": "retry_plan_command", "revoke": "revoke_plan_command"} {
		command, ok := status[field].(string)
		if !ok || !strings.Contains(command, `--state-dir "state path"`) ||
			!strings.Contains(command, "--action "+action) || !strings.Contains(command, "--correlation "+correlation) ||
			!strings.Contains(command, "--plan --json") {
			t.Fatalf("%s is not an exact copy-paste plan command: %q", field, command)
		}
	}
}
