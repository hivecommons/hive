package dashboard

import (
	"os/exec"
	"testing"
)

func TestDashboardProgressiveLoading(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — progressive dashboard behavior not executed")
	}
	if out, err := exec.Command(node, "--test", "progressive_loading.test.js").CombinedOutput(); err != nil {
		t.Fatalf("progressive dashboard behavior failed: %v\n%s", err, out)
	}
}
