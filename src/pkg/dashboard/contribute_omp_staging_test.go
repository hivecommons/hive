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

// contributeHiveCleanupContainerBlock returns the body of contribute-hive's
// cleanup_container trap, which runs when the recipe exits for any reason.
func contributeHiveCleanupContainerBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, "cleanup_container() {")
	if start < 0 {
		t.Fatal("contribute-hive has no cleanup_container function")
	}
	end := strings.Index(src[start:], "trap cleanup_container EXIT")
	if end < 0 {
		t.Fatal("cleanup_container is never registered as the EXIT trap")
	}
	return src[start : start+end]
}

// TestOmpRefreshedCredentialIsCopiedBackBeforeTheStageIsDeleted pins the
// container-mode half of hivecommons/hive#7922. The staged omp credential is
// a COPY of a single-use OAuth refresh token: once omp inside the container
// refreshes it, the host's row holds a revoked token, and the host's omp —
// and every later launch, which stages that row — is broken until someone
// logs in again. The cleanup trap must therefore copy the container's
// refreshed row back to the host store AFTER the container is gone (its omp
// no longer writing) and BEFORE the staging copy is removed.
func TestOmpRefreshedCredentialIsCopiedBackBeforeTheStageIsDeleted(t *testing.T) {
	block := contributeHiveCleanupContainerBlock(t)

	stop := strings.Index(block, `"$RUNTIME" rm -f "${CONTAINER_NAME}"`)
	sync := strings.Index(block, `node bin/omp-backend.js --sync-back "${HOME}/.omp" "${CLI_STAGE}/.omp"`)
	remove := strings.Index(block, `rm -rf "${CLI_STAGE}"`)
	switch {
	case sync < 0:
		t.Fatal("cleanup_container never runs bin/omp-backend.js --sync-back, so an OAuth credential the container's omp refreshed is deleted with the staging dir and the host keeps a revoked refresh token (#7922)")
	case stop < 0 || remove < 0:
		t.Fatal("cleanup_container no longer stops the container and removes ${CLI_STAGE} in the expected form")
	case sync < stop:
		t.Error("--sync-back must run after the container is stopped: its omp may still be rotating the row")
	case remove < sync:
		t.Error("--sync-back must run before ${CLI_STAGE} is removed: the refreshed credential lives there")
	}
	if !strings.Contains(block, `"${BACKEND}" = "omp"`) && !strings.Contains(block, `"${BACKEND}" == "omp"`) {
		t.Error("the copy-back must be gated on the omp backend; no other backend's stage holds an omp store")
	}
	// The trap must not abort on a failed sync: the staging copy holds the
	// credential and has to be deleted whatever happened. #7923 reports the
	// failure and the fix (/login) instead of swallowing it; either way the
	// rm -rf below must still run.
	if sync >= 0 && !strings.Contains(block[sync:remove], "|| true") && !strings.Contains(block[sync:remove], "|| echo") {
		t.Error("a failed --sync-back must not stop cleanup_container from removing ${CLI_STAGE}")
	}
}

// TestOmpRefreshedCredentialIsCopiedBackWhileTheContainerRuns: the exit-time
// copy alone leaves the host with a revoked token for the whole run — long
// enough for a second container launched from the same host, or the host's
// own omp, to fail on it. A timer narrows that window, and the trap stops the
// timer before its own final sync so the two never race over one row.
func TestOmpRefreshedCredentialIsCopiedBackWhileTheContainerRuns(t *testing.T) {
	src := justfileSource(t)
	start := strings.Index(src, "HIVE_OMP_CREDENTIAL_SYNC_SECONDS")
	if start < 0 {
		t.Fatal("contribute-hive has no HIVE_OMP_CREDENTIAL_SYNC_SECONDS timer for copying a refreshed omp credential back while the container runs (#7922)")
	}
	end := strings.Index(src[start:], "OMP_SYNC_PID=$!")
	if end < 0 {
		t.Fatal("the omp credential sync loop is not started in the background with its pid recorded in OMP_SYNC_PID")
	}
	loop := src[start : start+end]
	if !strings.Contains(loop, `node bin/omp-backend.js --sync-back "${HOME}/.omp" "${CLI_STAGE}/.omp"`) {
		t.Error("the timer loop must run the same --sync-back the cleanup trap does")
	}
	if !strings.Contains(loop, `-gt 0`) {
		t.Error("HIVE_OMP_CREDENTIAL_SYNC_SECONDS=0 must disable the timer (exit-time sync only)")
	}
	cleanup := contributeHiveCleanupContainerBlock(t)
	kill := strings.Index(cleanup, `kill "${OMP_SYNC_PID}"`)
	sync := strings.Index(cleanup, "--sync-back")
	if kill < 0 {
		t.Fatal("cleanup_container must stop the sync timer (kill OMP_SYNC_PID)")
	}
	if sync >= 0 && kill > sync {
		t.Error("cleanup_container must stop the timer before its own final --sync-back, so the two do not race over the same host row")
	}
}
