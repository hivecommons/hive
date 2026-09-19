package dashboard

import (
	"strings"
	"testing"
)

// contribute_omp_staging_test.go pins the container-mode half of
// hivecommons/hive#7678.
//
// contribute-hive stages each backend's host config into an ephemeral
// directory and mounts THAT into the container (the H6 / CWE-668 boundary).
// There was no omp case: nothing reached /home/dev/.omp, so the container's
// omp started as a fresh install — its first-run setup wizard, no provider
// signed in — while local mode, which runs the host's omp against the real
// ${HOME}, picked up the sign-in with no extra steps. omp keeps that sign-in
// in ${HOME}/.omp/agent (config.yml plus the agent.db SQLite store; there is
// no auth.json), and bin/omp-backend.js is what copies the allowlisted part
// of it and narrows the credential rows to the selected provider.

// contributeHiveOmpStagingBlock returns the omp branch of contribute-hive's
// CLI-staging case statement.
func contributeHiveOmpStagingBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	// The staging case sits between pi's (which ends by granting host
	// networking) and agy's.
	anchor := `NET_FLAGS="--network host"`
	start := strings.Index(src, anchor)
	if start < 0 {
		t.Fatal("the pi CLI-staging case (the anchor before omp's) was not found in the Justfile")
	}
	rest := src[start:]
	ompStart := strings.Index(rest, "omp)")
	if ompStart < 0 {
		t.Fatal("contribute-hive has no omp) CLI-staging case: nothing reaches /home/dev/.omp and the container's omp starts at its setup wizard (#7678)")
	}
	end := strings.Index(rest[ompStart:], "agy)")
	if end < 0 {
		t.Fatal("the end of the omp staging case was not found in the Justfile")
	}
	return rest[ompStart : ompStart+end]
}

// TestOmpStagingMountsTheHostSignIn proves the case exists and does the two
// things that make container mode sign in the way local mode does: it stages
// ${HOME}/.omp through the helper that narrows credentials, and it mounts the
// staged copy where omp in the container looks for it.
func TestOmpStagingMountsTheHostSignIn(t *testing.T) {
	block := contributeHiveOmpStagingBlock(t)

	if !strings.Contains(block, `node bin/omp-backend.js --stage "${HOME}/.omp" "${CLI_STAGE}/.omp" "${AGENT_MODEL:-}"`) {
		t.Error("the omp case must stage ${HOME}/.omp through bin/omp-backend.js --stage, which is what keeps only the selected provider's credential rows (#7678)")
	}
	if !strings.Contains(block, `-v ${CLI_STAGE}/.omp:/home/dev/.omp${VOLSUF}`) {
		t.Error("the staged copy must be mounted at /home/dev/.omp — omp reads ${HOME}/.omp/agent, and any other path leaves it at the wizard")
	}
	// It must mount the STAGED copy, never the host directory: the container
	// writes to a throwaway, and the contributor's real ~/.omp stays untouched.
	if strings.Contains(block, `-v ${HOME}/.omp:`) {
		t.Error("the omp case bind-mounts the host's real ~/.omp; H6 requires the ephemeral staging copy")
	}
}

// TestOmpStagingFailureDoesNotStartTheContainerOrLeakTheCopy: when the helper
// cannot narrow the credential store, mounting every provider's credential
// would violate least privilege and mounting none would recreate the wizard,
// so the recipe must stop — and, because the cleanup trap is registered only
// just before the container starts, it must remove the staging copy itself.
func TestOmpStagingFailureDoesNotStartTheContainerOrLeakTheCopy(t *testing.T) {
	block := contributeHiveOmpStagingBlock(t)

	idx := strings.Index(block, "if ! node bin/omp-backend.js --stage")
	if idx < 0 {
		t.Fatal("the omp staging helper's exit status is not checked, so a failed narrowing would still start the container")
	}
	failure := block[idx:]
	if !strings.Contains(failure, `rm -rf "${CLI_STAGE}"`) {
		t.Error("a failed staging must remove ${CLI_STAGE} itself: the cleanup trap is not registered yet, and the copy holds the credential store")
	}
	if !strings.Contains(failure, "exit 1") {
		t.Error("a failed staging must stop the recipe rather than start a container with no credential")
	}
}

// TestOmpMissingSignInIsExplainedBeforeTheContainerStarts pins the message for
// a host that has never set omp up. The wizard is then inevitable; what the
// contributor needs to hear is that a sign-in done inside the container is
// discarded with it, and that signing in on the host once is the fix — the
// same lesson #5088 taught for claude.
func TestOmpMissingSignInIsExplainedBeforeTheContainerStarts(t *testing.T) {
	block := contributeHiveOmpStagingBlock(t)

	for _, want := range []string{
		`${HOME}/.omp/agent does not exist`, // what was checked
		"setup wizard",                      // what the contributor will see
		"discarded with the container",      // the cost of signing in there
		"run omp on the host",               // the fix
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the missing-sign-in notice does not mention %q", want)
		}
	}
}

// TestOmpPreflightReportsWhatContainerModeStages covers item 3 of #7678:
// `just contribute-setup omp` used to say only that the host CLI was
// detected, so a missing sign-in surfaced as the setup wizard inside a
// container whose relay was waiting at it. The preflight now reports what
// container mode will stage.
func TestOmpPreflightReportsWhatContainerModeStages(t *testing.T) {
	src := justfileSource(t)
	start := strings.Index(src, "OMP CLI detected")
	if start < 0 {
		t.Fatal("the omp preflight was not found in the Justfile")
	}
	end := strings.Index(src[start:], "ERROR: OMP CLI not found")
	if end < 0 {
		t.Fatal("the end of the omp preflight was not found in the Justfile")
	}
	block := src[start : start+end]
	if !strings.Contains(block, `node bin/omp-backend.js --describe "${HOME}/.omp" "${AGENT_MODEL:-}"`) {
		t.Error("the omp preflight must describe what container mode will stage from ~/.omp (signed-in providers, which of them the selection keeps) so a missing sign-in is caught before the container starts (#7678)")
	}
	// The old text sent contributors to "OMP's documented environment or
	// profile" — advice that never produced a staged sign-in.
	if strings.Contains(block, "documented environment or profile") {
		t.Error("the omp preflight still tells contributors to authenticate through an environment or profile instead of signing in on the host")
	}
}
