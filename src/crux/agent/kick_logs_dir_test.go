package agent

// Tests for Manager.SetKickLogDir (kick_logs.go): explicit override, and the
// documented contract that an empty dir restores the env/default resolution
// NewManager performs via kickLogSettingsFromEnv.

import "testing"

func TestSetKickLogDir_ExplicitOverride(t *testing.T) {
	m, _, orig := kickLogTestManager(t, "output")
	if m.kickLogDir != orig {
		t.Fatalf("precondition: kickLogDir = %q, want %q", m.kickLogDir, orig)
	}

	next := t.TempDir()
	m.SetKickLogDir(next)
	if m.kickLogDir != next {
		t.Fatalf("kickLogDir = %q, want %q", m.kickLogDir, next)
	}
}

func TestSetKickLogDir_EmptyRestoresEnvResolution(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv(kickLogDirEnv, envDir)

	m, _, _ := kickLogTestManager(t, "output")
	m.SetKickLogDir("")
	if m.kickLogDir != envDir {
		t.Fatalf("kickLogDir after empty reset = %q, want env dir %q", m.kickLogDir, envDir)
	}
}

func TestSetKickLogDir_EmptyFallsBackToDefaultWithoutEnv(t *testing.T) {
	t.Setenv(kickLogDirEnv, "")

	m, _, _ := kickLogTestManager(t, "output")
	m.SetKickLogDir("")
	if m.kickLogDir != defaultKickLogDir {
		t.Fatalf("kickLogDir after empty reset = %q, want default %q", m.kickLogDir, defaultKickLogDir)
	}
}
