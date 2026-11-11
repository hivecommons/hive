package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestOpenShellIntegration runs one real kick through an OpenShell gateway.
// It is skipped unless OPENSHELL_GATEWAY_URL is set, so the default test run
// needs no gateway; set OPENSHELL_WORKSPACE_CLAIM to the attachable PVC and
// OPENSHELL_IMAGE to a digest-pinned image the gateway can pull. The
// workspace path must live under OPENSHELL_WORKSPACE_CLAIM_MOUNT (default
// /data) on the machine running the test, i.e. this is meant to run from
// inside a hive pod or a debug pod with the claim mounted.
func TestOpenShellIntegration(t *testing.T) {
	gateway := os.Getenv("OPENSHELL_GATEWAY_URL")
	if gateway == "" {
		t.Skip("OPENSHELL_GATEWAY_URL not set; skipping live gateway test")
	}
	claim := os.Getenv("OPENSHELL_WORKSPACE_CLAIM")
	image := os.Getenv("OPENSHELL_IMAGE")
	if claim == "" || image == "" {
		t.Skip("OPENSHELL_WORKSPACE_CLAIM and OPENSHELL_IMAGE must be set with OPENSHELL_GATEWAY_URL")
	}
	mount := os.Getenv("OPENSHELL_WORKSPACE_CLAIM_MOUNT")
	if mount == "" {
		mount = OpenShellDefaultWorkspaceClaimMount
	}
	workspace, err := os.MkdirTemp(mount, "openshell-it-")
	if err != nil {
		t.Skipf("cannot create a workspace under %s: %v", mount, err)
	}
	defer os.RemoveAll(workspace)

	l := &OpenShellLauncher{Options: OpenShellOptions{
		GatewayURL:          gateway,
		Workspace:           os.Getenv("OPENSHELL_WORKSPACE"),
		Token:               os.Getenv("OPENSHELL_TOKEN"),
		InsecureTLS:         os.Getenv("OPENSHELL_INSECURE_TLS") == "1",
		WorkspaceClaim:      claim,
		WorkspaceClaimMount: mount,
		Timeout:             5 * time.Minute,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := l.Run(ctx, LaunchSpec{
		Image:     image,
		Name:      "integration",
		Workspace: workspace,
		Command:   []string{"sh", "-c", "echo hello > /workspace/out.txt && cat /workspace/out.txt"},
	})
	t.Logf("sandbox round trip: %s", time.Since(start))
	if err != nil {
		t.Fatalf("Run: %v (stdout=%q stderr=%q)", err, res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("result = %+v", res)
	}
	if b, err := os.ReadFile(workspace + "/out.txt"); err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("workspace write did not round-trip through the shared claim: %v %q", err, b)
	}
}
