package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func setScannerMode(srv *Server, mode, owner string) {
	sc := srv.deps.Config.Agents["scanner"]
	sc.Mode = mode
	sc.ModeOwner = owner
	srv.deps.Config.Agents["scanner"] = sc
}

func TestApplyPackPreservesOperatorMode(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	setScannerMode(srv, "NO_GITHUB", config.FieldOwnerOperator)

	for i := 0; i < 2; i++ {
		if _, err := srv.ApplyPack(3); err != nil {
			t.Fatalf("ApplyPack(3) restart %d: %v", i, err)
		}
	}
	if got := srv.deps.Config.Agents["scanner"].Mode; got != "NO_GITHUB" {
		t.Errorf("operator mode reverted: got %q", got)
	}
}

func TestApplyPackReconcilesPackOwnedMode(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	setScannerMode(srv, "NO_GITHUB", config.FieldOwnerPack)

	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3) re-apply: %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"].Mode; got == "NO_GITHUB" {
		t.Error("pack-owned mode was not reconciled to the pack value")
	}
}

func TestApplyPackForceResetsOperatorModeOwnership(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	setScannerMode(srv, "NO_GITHUB", config.FieldOwnerOperator)

	if _, err := srv.ApplyPackForce(3); err != nil {
		t.Fatalf("ApplyPackForce(3): %v", err)
	}
	ac := srv.deps.Config.Agents["scanner"]
	if ac.Mode == "NO_GITHUB" || ac.ModeIsOperatorOwned() {
		t.Errorf("level change did not reset mode ownership: mode=%q owner=%q", ac.Mode, ac.ModeOwner)
	}
}
