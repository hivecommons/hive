package config

import (
	"slices"
	"testing"
)

func resetExtraCredentialEnvVars(t *testing.T) {
	t.Helper()
	reset := func() {
		extraCredentialEnvMu.Lock()
		extraCredentialEnvVars = nil
		extraCredentialEnvMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestAddExtraCredentialEnvVarsAccumulatesAcrossCalls(t *testing.T) {
	resetExtraCredentialEnvVars(t)

	AddExtraCredentialEnvVars("NAME_A")
	AddExtraCredentialEnvVars("NAME_B")

	if got, want := ExtraCredentialEnvVars(), []string{"NAME_A", "NAME_B"}; !slices.Equal(got, want) {
		t.Fatalf("ExtraCredentialEnvVars() = %v, want %v", got, want)
	}
}

func TestAddExtraCredentialEnvVarsIgnoresBlanksAndDuplicates(t *testing.T) {
	resetExtraCredentialEnvVars(t)

	AddExtraCredentialEnvVars("  NAME_A ", "", "   ", "NAME_A")
	AddExtraCredentialEnvVars("NAME_A", "NAME_B")
	AddExtraCredentialEnvVars()

	if got, want := ExtraCredentialEnvVars(), []string{"NAME_A", "NAME_B"}; !slices.Equal(got, want) {
		t.Fatalf("ExtraCredentialEnvVars() = %v, want %v", got, want)
	}
}

func TestExtraCredentialEnvVarsReturnsCopy(t *testing.T) {
	resetExtraCredentialEnvVars(t)

	AddExtraCredentialEnvVars("NAME_A")
	got := ExtraCredentialEnvVars()
	got[0] = "MUTATED"

	if again := ExtraCredentialEnvVars(); !slices.Equal(again, []string{"NAME_A"}) {
		t.Fatalf("registry changed through returned slice: %v", again)
	}
}
