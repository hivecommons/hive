package config

import "testing"

// ResolveKey and ResolveKeySource are the wrappers callers actually use (the
// backup writer wants only the key; the API/status paths want only the safe
// source label). Pin that each wrapper forwards the right half of
// ResolveKeyWithSource, including nil-receiver env fallback.

func TestResolveKeyWrapperReturnsKeyOnly(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	p := writeBackupKeyFile(t, backupCfgTestKey+"\n")
	bc := &BackupConfig{KeyFile: p}
	if got := bc.ResolveKey(); got != backupCfgTestKey {
		t.Fatalf("ResolveKey = %q, want file key", got)
	}
	if got := bc.ResolveKeySource(); got != "file:"+p {
		t.Fatalf("ResolveKeySource = %q, want %q", got, "file:"+p)
	}
}

func TestResolveKeyWrapperNilReceiverEnvFallback(t *testing.T) {
	t.Setenv(BackupKeyEnv, backupCfgTestKey)
	var bc *BackupConfig
	if got := bc.ResolveKey(); got != backupCfgTestKey {
		t.Fatalf("nil-receiver ResolveKey = %q, want env key", got)
	}
	if got := bc.ResolveKeySource(); got != "env:"+BackupKeyEnv {
		t.Fatalf("nil-receiver ResolveKeySource = %q, want env source", got)
	}
}

func TestResolveKeyWrapperUnconfigured(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	var bc BackupConfig
	if got := bc.ResolveKey(); got != "" {
		t.Fatalf("unconfigured ResolveKey = %q, want empty", got)
	}
	if got := bc.ResolveKeySource(); got != "" {
		t.Fatalf("unconfigured ResolveKeySource = %q, want empty", got)
	}
}

// ConvergenceModes feeds settings UIs and validation errors; every listed
// mode must round-trip through NormalizeConvergenceMode, and the rollout
// order (off → shadow → enforce) is part of the contract.
func TestConvergenceModesListsValidModesInRolloutOrder(t *testing.T) {
	modes := ConvergenceModes()
	want := []string{ConvergenceModeOff, ConvergenceModeShadow, ConvergenceModeEnforce}
	if len(modes) != len(want) {
		t.Fatalf("ConvergenceModes = %v, want %v", modes, want)
	}
	for i, m := range want {
		if modes[i] != m {
			t.Fatalf("ConvergenceModes[%d] = %q, want %q", i, modes[i], m)
		}
	}
	for _, m := range modes {
		norm, ok := NormalizeConvergenceMode(m)
		if !ok || norm != m {
			t.Fatalf("mode %q does not round-trip through NormalizeConvergenceMode (%q, %v)", m, norm, ok)
		}
	}
}
