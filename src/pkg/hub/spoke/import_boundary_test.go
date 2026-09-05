package spoke

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSpokeBoundaryDoesNotDependOnHubPackage(t *testing.T) {
	for _, pkg := range []string{"./pkg/hub/spoke", "./pkg/dashboard"} {
		cmd := exec.Command("go", "list", "-deps", pkg)
		cmd.Dir = "../../.."
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if dep == "github.com/hivecommons/hive/pkg/hub" {
				t.Fatalf("%s must not depend on pkg/hub", pkg)
			}
		}
	}
}
