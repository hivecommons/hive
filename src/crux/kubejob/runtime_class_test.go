package kubejob

import (
	"strings"
	"testing"
)

// TestJobManifestRuntimeClassName (#19694 review): job.runtime_class_name
// reaches the pod spec, and an empty one leaves the field out so the cluster
// default applies as before.
func TestJobManifestRuntimeClassName(t *testing.T) {
	spec := specFor("/data/agents/sandbox/x")
	unset := podSpecOf(t, (&Launcher{Options: Options{WorkspaceClaim: "hive-data"}}).manifest("hive", "hive-kick-x-1", "agents/sandbox/x", spec))
	if _, has := unset["runtimeClassName"]; has {
		t.Errorf("runtimeClassName = %v with none configured, want it absent", unset["runtimeClassName"])
	}
	set := podSpecOf(t, (&Launcher{Options: Options{WorkspaceClaim: "hive-data", RuntimeClassName: " kata "}}).manifest("hive", "hive-kick-x-1", "agents/sandbox/x", spec))
	if got := set["runtimeClassName"]; got != "kata" {
		t.Errorf("runtimeClassName = %v, want kata", got)
	}
}

func TestValidateRejectsABadRuntimeClassName(t *testing.T) {
	if err := (Options{WorkspaceClaim: "hive-data", RuntimeClassName: "Kata VM"}).Validate(); err == nil || !strings.Contains(err.Error(), "runtime_class_name") {
		t.Errorf("Validate with runtime_class_name %q = %v, want a runtime_class_name error", "Kata VM", err)
	}
	if err := (Options{WorkspaceClaim: "hive-data", RuntimeClassName: "kata-qemu"}).Validate(); err != nil {
		t.Errorf("Validate with runtime_class_name kata-qemu = %v, want nil", err)
	}
}
