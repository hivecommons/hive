package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestFailErrorNamesReasonAndNode(t *testing.T) {
	err := error(&FailError{Reason: FailReasonNodeUnhealthy, Node: "node-a", Message: "device gone"})
	var fe *FailError
	if !errors.As(err, &fe) || fe.Reason != "node_unhealthy" {
		t.Fatalf("errors.As lost the reason: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "node_unhealthy") || !strings.Contains(got, "node-a") || !strings.Contains(got, "device gone") {
		t.Errorf("Error() = %q", got)
	}
	if got := (&FailError{Reason: FailReasonWaitingForNode, Message: "0/3 nodes"}).Error(); strings.Contains(got, "on node") || !strings.Contains(got, "waiting_for_node") {
		t.Errorf("a node-less error = %q", got)
	}
	if ExitCodeNodeUnhealthy != 75 {
		t.Errorf("ExitCodeNodeUnhealthy = %d, want 75", ExitCodeNodeUnhealthy)
	}
}
