package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDispatchSubcommandRoutesValidateToConfigCheck(t *testing.T) {
	path := writeCheckConfig(t, nil)

	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{"validate", "-config", path}, &stdout, &stderr)

	if !handled {
		t.Fatal("dispatchSubcommand(validate) handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("dispatchSubcommand(validate) code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("validate did not run the config-check path; stdout: %s", stdout.String())
	}
}

func TestDispatchSubcommandRoutesConfigCheckAlias(t *testing.T) {
	path := writeCheckConfig(t, nil)

	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{"--config-check", "-config", path}, &stdout, &stderr)

	if !handled {
		t.Fatal("dispatchSubcommand(--config-check) handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("dispatchSubcommand(--config-check) code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("--config-check did not run the config-check path; stdout: %s", stdout.String())
	}
}

func TestDashboardDependenciesWireRescanReposFunc(t *testing.T) {
	deps := (&spokeWire{}).dashboardDependencies()

	if deps.RescanReposFunc == nil {
		t.Fatal("dashboardDependencies().RescanReposFunc = nil, want rescanRepos wiring")
	}

	got, err := deps.RescanReposFunc(context.Background())
	if !errors.Is(err, errNoForgeCredentials) {
		t.Fatalf("RescanReposFunc() err = %v, want errNoForgeCredentials", err)
	}
	if got != nil {
		t.Fatalf("RescanReposFunc() result = %+v, want nil without GitHub credentials", got)
	}
}
