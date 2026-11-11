package config

import (
	"reflect"
	"testing"
)

func TestSandboxPreflightWarningsFor(t *testing.T) {
	t.Cleanup(func() { StoreSandboxGateReport(SandboxGateReport{}) })
	StoreSandboxGateReport(SandboxGateReport{
		OpenShell: []string{"o"},
		Substrate: []string{"s"},
	})
	for _, tc := range []struct {
		runtime string
		want    []string
	}{
		{SandboxRuntimeOpenShell, []string{"o"}},
		{SandboxRuntimeSubstrate, []string{"s"}},
		{SandboxRuntimeJob, nil},
		{"", nil},
	} {
		if got := SandboxPreflightWarningsFor(tc.runtime); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SandboxPreflightWarningsFor(%q) = %v, want %v", tc.runtime, got, tc.want)
		}
	}
}
