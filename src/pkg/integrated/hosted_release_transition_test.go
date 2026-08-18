package integrated

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type hostedReleaseTransitionFixture struct {
	store           *Store
	intent          HostedReleaseTransitionIntent
	candidate       Config
	sourceWasPaused bool
}

func TestHostedReleaseTransitionAtomicRoundTripResumeAndClear(t *testing.T) {
	fixture := newHostedReleaseTransitionFixture(t, false)
	intent := advanceHostedReleaseTransitionToTargetVerified(t, fixture)
	if intent.StatusDefaultHeadSHA == intent.MergeSHA || intent.SetupBaseSHA == intent.PauseDefaultHeadSHA {
		t.Fatal("fixture did not prove descendant status and post-pause source-head bindings")
	}

	intent.Phase = HostedReleaseTransitionPromoted
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	candidate, err := intent.CandidateConfig.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Save(candidate); err != nil {
		t.Fatal(err)
	}
	intent = mustLoadHostedReleaseTransition(t, fixture.store)

	intent.Phase = HostedReleaseTransitionResumePrepared
	intent.ResumeRequestID = "resume-request-1"
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionResumeVerified
	intent.ResumeRunID, intent.ResumeArtifactID = 104, 204
	intent.ResumeArtifactName = "hive-hosted-control-104-1"
	intent.ResumeDefaultHeadSHA, intent.ResumeStateHeadSHA = strings.Repeat("c", 40), strings.Repeat("d", 40)
	intent.ResumeRunning = true
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)

	for index := 0; index < 8; index++ {
		intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	}
	if matches, err := filepath.Glob(filepath.Join(fixture.store.Dir(), "."+hostedReleaseTransitionFile+"-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("atomic saves left temporary files: matches=%v err=%v", matches, err)
	}
	if err := fixture.store.ClearHostedReleaseTransitionIntent(); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := fixture.store.LoadHostedReleaseTransitionIntent(); err != nil || exists {
		t.Fatalf("cleared transition remained: exists=%t err=%v", exists, err)
	}
	if err := fixture.store.ClearHostedReleaseTransitionIntent(); err != nil {
		t.Fatalf("idempotent clear failed: %v", err)
	}
}

func TestHostedReleaseTransitionPreservesAuthoritativePreexistingPause(t *testing.T) {
	fixture := newHostedReleaseTransitionFixture(t, true)
	if fixture.intent.SourceWasPaused || fixture.candidate.Paused {
		t.Fatal("fixture incorrectly sourced preflight pause state from local Config")
	}
	intent := advanceHostedReleaseTransitionToTargetVerified(t, fixture)
	if !intent.SourceWasPaused {
		t.Fatal("authoritative preflight pause state was not retained")
	}
	intent.Phase = HostedReleaseTransitionPromoted
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	candidate, err := intent.CandidateConfig.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Save(candidate); err != nil {
		t.Fatal(err)
	}
	intent = mustLoadHostedReleaseTransition(t, fixture.store)
	intent.Phase = HostedReleaseTransitionPausePreserved
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	if err := fixture.store.ClearHostedReleaseTransitionIntent(); err != nil {
		t.Fatalf("clear pause-preserved transition: %v", err)
	}

	fixture = newHostedReleaseTransitionFixture(t, true)
	intent = advanceHostedReleaseTransitionToTargetVerified(t, fixture)
	intent.Phase = HostedReleaseTransitionPromoted
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	candidate, _ = intent.CandidateConfig.Config()
	if err := fixture.store.Save(candidate); err != nil {
		t.Fatal(err)
	}
	intent.Phase = HostedReleaseTransitionResumePrepared
	intent.ResumeRequestID = "resume-must-not-run"
	if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "source pause state") {
		t.Fatalf("pre-paused source was allowed to resume: %v", err)
	}
}

func TestHostedReleaseTransitionSealsCompleteCandidateAndRedactsPublicJSON(t *testing.T) {
	fixture := newHostedReleaseTransitionFixture(t, false)
	intent := advanceHostedReleaseTransitionToSetupPrepared(t, fixture)
	candidate, err := intent.CandidateConfig.Config()
	if err != nil {
		t.Fatal(err)
	}
	managed := managedSetupFilesForConfig(candidate)
	preimage := candidate.ManagedPathPreimages[managed[0]]
	if !preimage.Existed || string(preimage.Content) != "repository-owned-before-hive\n" {
		t.Fatalf("sealed candidate lost unredacted managed preimage: %+v", preimage)
	}
	persisted, err := os.ReadFile(filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile))
	if err != nil {
		t.Fatal(err)
	}
	encodedPreimage := base64.StdEncoding.EncodeToString(preimage.Content)
	if !bytes.Contains(persisted, []byte(encodedPreimage)) {
		t.Fatal("durable transition omitted sealed managed preimage bytes")
	}
	public, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte(encodedPreimage)) {
		t.Fatal("public transition JSON disclosed managed preimage bytes")
	}
	if candidate.SetupBranch != intent.SetupBranch || candidate.SetupHeadSHA != intent.SetupCommitSHA || intent.SetupCandidateDigest != intent.CandidateConfigDigest {
		t.Fatal("sealed candidate does not bind exact setup identity")
	}

	intent.CandidateConfig.ManagedPathPreimages[managed[0]] = ManagedPathPreimage{Existed: true, GitMode: "100644", Content: []byte("tampered\n"), ContentSHA256: preimage.ContentSHA256}
	if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "preimage ledger") {
		t.Fatalf("tampered candidate preimage was accepted: %v", err)
	}
}

func TestHostedReleaseTransitionCancellationIsDistinctAndPreMergeOnly(t *testing.T) {
	fixture := newHostedReleaseTransitionFixture(t, false)
	_ = advanceHostedReleaseTransitionToPullRecorded(t, fixture)
	if err := fixture.store.ClearHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "cannot be cleared") {
		t.Fatalf("normal clear removed an unfinished transition: %v", err)
	}
	if err := fixture.store.CancelHostedReleaseTransitionIntent(); err != nil {
		t.Fatalf("cancel exact closed-unmerged transition: %v", err)
	}
	if _, exists, err := fixture.store.LoadHostedReleaseTransitionIntent(); err != nil || exists {
		t.Fatalf("cancelled transition remained: exists=%t err=%v", exists, err)
	}
	if err := fixture.store.CancelHostedReleaseTransitionIntent(); err != nil {
		t.Fatalf("idempotent cancel failed: %v", err)
	}

	fixture = newHostedReleaseTransitionFixture(t, false)
	_ = advanceHostedReleaseTransitionToMerged(t, fixture)
	if err := fixture.store.CancelHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "cannot be cancelled") {
		t.Fatalf("merged transition was cancelled: %v", err)
	}
}

func TestHostedReleaseTransitionRejectsTamperMalformedAndRepositoryMismatch(t *testing.T) {
	t.Run("tampered sealed field", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		_ = saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
		path := filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		record["active_config_sha256"] = strings.Repeat("f", 64)
		tampered, _ := json.Marshal(record)
		if err := os.WriteFile(path, append(tampered, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.store.LoadHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "record digest") {
			t.Fatalf("tampered transition was accepted: %v", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		_ = saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
		path := filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile)
		data, _ := os.ReadFile(path)
		data = bytes.Replace(data, []byte("\n}"), []byte(",\n  \"unexpected\": true\n}"), 1)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.store.LoadHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown transition field was accepted: %v", err)
		}
	})

	t.Run("trailing document", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		_ = saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
		path := filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString("{}\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append malformed document: write=%v close=%v", writeErr, closeErr)
		}
		if _, _, err := fixture.store.LoadHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
			t.Fatalf("trailing transition document was accepted: %v", err)
		}
	})

	t.Run("repository mismatch", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		fixture.intent.Repository = "other/repo"
		if err := fixture.store.SaveHostedReleaseTransitionIntent(fixture.intent); err == nil || !strings.Contains(err.Error(), "repository identity") {
			t.Fatalf("cross-repository transition was accepted: %v", err)
		}
	})

	t.Run("oversized state", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		path := filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile)
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(hostedReleaseTransitionMax+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.store.LoadHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized transition was accepted: %v", err)
		}
	})
}

func TestHostedReleaseTransitionRejectsInvalidPhaseTransitionsAndAuthority(t *testing.T) {
	t.Run("prepared omits candidate bytes", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		_ = saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
		data, err := os.ReadFile(filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(`"candidate_config"`)) || bytes.Contains(data, []byte(`"candidate_config_sha256"`)) {
			t.Fatal("prepared transition embedded candidate policy before source quiescence")
		}
	})

	t.Run("preflight request must precede dispatch", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		fixture.intent.PreflightStatusRequestID = ""
		if err := fixture.store.SaveHostedReleaseTransitionIntent(fixture.intent); err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("prepared transition without preflight request was accepted: %v", err)
		}
	})

	t.Run("must start prepared and cannot skip", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		intent := fixture.intent
		intent.Phase = HostedReleaseTransitionPreflightVerified
		setPreflightReceipt(&intent, false)
		if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "must begin") {
			t.Fatalf("non-prepared initial phase was accepted: %v", err)
		}
		intent = saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
		skipped := intent
		skipped.Phase = HostedReleaseTransitionPausePrepared
		setPreflightReceipt(&skipped, false)
		skipped.PauseRequestID = "pause-request"
		if err := fixture.store.SaveHostedReleaseTransitionIntent(skipped); err == nil || !strings.Contains(err.Error(), "cannot move") {
			t.Fatalf("skipped preflight phase was accepted: %v", err)
		}
	})

	t.Run("verified evidence is immutable", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		intent := advanceHostedReleaseTransitionToSourceStatusVerified(t, fixture)
		changed := intent
		changed.Phase = HostedReleaseTransitionSetupPrepared
		changed.SourceStatusRunID++
		setSetupProposal(t, fixture, &changed)
		if err := fixture.store.SaveHostedReleaseTransitionIntent(changed); err == nil || !strings.Contains(err.Error(), "prior phase evidence") {
			t.Fatalf("source status evidence mutation was accepted: %v", err)
		}
	})

	t.Run("branch marker base and candidate are operation bound", func(t *testing.T) {
		fixture := newHostedReleaseTransitionFixture(t, false)
		changedVisual := fixture.intent.TargetRelease
		changedVisual.VisualHiveCommit = strings.Repeat("9", 40)
		if HostedReleaseTransitionSetupMarker(fixture.intent.Operation, fixture.intent.RepositoryID, fixture.intent.ActiveRelease, fixture.intent.TargetRelease) ==
			HostedReleaseTransitionSetupMarker(fixture.intent.Operation, fixture.intent.RepositoryID, fixture.intent.ActiveRelease, changedVisual) {
			t.Fatal("setup marker did not bind the complete target release identity")
		}
		intent := advanceHostedReleaseTransitionToSourceStatusVerified(t, fixture)
		intent.Phase = HostedReleaseTransitionSetupPrepared
		setSetupProposal(t, fixture, &intent)
		intent.SetupBranch = managedOperationBranch("setup", intent.RepositoryID)
		if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "setup evidence") {
			t.Fatalf("generic setup branch was accepted: %v", err)
		}
		setSetupProposal(t, fixture, &intent)
		intent.SetupBaseSHA = intent.PauseDefaultHeadSHA
		if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "setup evidence") {
			t.Fatalf("pre-source-status setup base was accepted: %v", err)
		}
		setSetupProposal(t, fixture, &intent)
		intent.SetupMarker = "<!-- hive-setup: owner/repo -->"
		if err := fixture.store.SaveHostedReleaseTransitionIntent(intent); err == nil || !strings.Contains(err.Error(), "setup evidence") {
			t.Fatalf("generic setup marker was accepted: %v", err)
		}
	})
}

func TestHostedReleaseTransitionRejectsLinkedDestination(t *testing.T) {
	fixture := newHostedReleaseTransitionFixture(t, false)
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.store.Dir(), hostedReleaseTransitionFile)
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := fixture.store.SaveHostedReleaseTransitionIntent(fixture.intent); err == nil || !strings.Contains(err.Error(), "linked") {
		t.Fatalf("linked transition destination was accepted: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged\n" {
		t.Fatalf("linked target changed: data=%q err=%v", data, err)
	}
	if err := fixture.store.CancelHostedReleaseTransitionIntent(); err == nil || !strings.Contains(err.Error(), "linked") {
		t.Fatalf("linked transition destination was cancelled: %v", err)
	}
}

func advanceHostedReleaseTransitionToTargetVerified(t *testing.T, fixture hostedReleaseTransitionFixture) HostedReleaseTransitionIntent {
	t.Helper()
	intent := advanceHostedReleaseTransitionToMerged(t, fixture)
	intent.Phase = HostedReleaseTransitionTargetCheckpointPrepared
	intent.TargetRequestID = "target-pause-request-1"
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionTargetCheckpointVerified
	intent.TargetRunID, intent.TargetArtifactID = 102, 202
	intent.TargetArtifactName = "hive-hosted-control-102-1"
	intent.TargetDefaultHeadSHA, intent.TargetStateHeadSHA = strings.Repeat("8", 40), strings.Repeat("9", 40)
	intent.TargetPaused = true
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionStatusPrepared
	intent.StatusRequestID = "target-status-request-1"
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionTargetVerified
	intent.StatusRunID, intent.StatusArtifactID = 103, 203
	intent.StatusArtifactName = "hive-hosted-status-103-1"
	intent.StatusDefaultHeadSHA, intent.StatusStateHeadSHA = strings.Repeat("a", 40), strings.Repeat("b", 40)
	intent.StatusPaused, intent.StatusQuiescent = true, true
	return saveLoadHostedReleaseTransition(t, fixture.store, intent)
}

func advanceHostedReleaseTransitionToMerged(t *testing.T, fixture hostedReleaseTransitionFixture) HostedReleaseTransitionIntent {
	t.Helper()
	intent := advanceHostedReleaseTransitionToPullRecorded(t, fixture)
	intent.Phase = HostedReleaseTransitionMerged
	intent.MergeSHA = strings.Repeat("6", 40)
	return saveLoadHostedReleaseTransition(t, fixture.store, intent)
}

func advanceHostedReleaseTransitionToPullRecorded(t *testing.T, fixture hostedReleaseTransitionFixture) HostedReleaseTransitionIntent {
	t.Helper()
	intent := advanceHostedReleaseTransitionToSetupPrepared(t, fixture)
	intent.Phase = HostedReleaseTransitionPullRecorded
	intent.SetupPRNumber = 17
	intent.SetupPRURL = "https://github.com/owner/repo/pull/17"
	intent.SetupAuthorizationContext = SetupAuthorizationContextPrefix + strings.Repeat("e", 64)
	intent.SetupAuthorizationStatusID = 701
	intent.SetupAuthorizationCreatorID = 42
	return saveLoadHostedReleaseTransition(t, fixture.store, intent)
}

func TestHostedReleaseTransitionPullBindsAppStatusWriterSeparatelyFromHumanActor(t *testing.T) {
	candidate := HostedReleaseTransitionCandidateConfig(Config{
		SetupAuthorizationActorID: 456, SetupAuthorizationActorLogin: "owner",
		SetupAuthorizationWriterID: 202, SetupAuthorizationWriterLogin: "hive-test[bot]", SetupAuthorizationWriterType: "Bot",
		SetupAuthorizationAppID: 99, SetupAuthorizationInstallationID: 77,
	})
	intent := HostedReleaseTransitionIntent{
		Repository: "owner/repo", SetupPRNumber: 17, SetupPRURL: "https://github.com/owner/repo/pull/17",
		SetupAuthorizationContext:  SetupAuthorizationContextPrefix + strings.Repeat("e", 64),
		SetupAuthorizationStatusID: 701, SetupAuthorizationCreatorID: 202, CandidateConfig: &candidate,
	}
	if !validHostedTransitionPull(intent) {
		t.Fatal("App status writer was incorrectly required to equal the human setup actor")
	}
	intent.SetupAuthorizationCreatorID = 456
	if validHostedTransitionPull(intent) {
		t.Fatal("human setup actor was accepted as the App-written status creator")
	}
}

func advanceHostedReleaseTransitionToSetupPrepared(t *testing.T, fixture hostedReleaseTransitionFixture) HostedReleaseTransitionIntent {
	t.Helper()
	intent := advanceHostedReleaseTransitionToSourceStatusVerified(t, fixture)
	intent.Phase = HostedReleaseTransitionSetupPrepared
	setSetupProposal(t, fixture, &intent)
	return saveLoadHostedReleaseTransition(t, fixture.store, intent)
}

func advanceHostedReleaseTransitionToSourceStatusVerified(t *testing.T, fixture hostedReleaseTransitionFixture) HostedReleaseTransitionIntent {
	t.Helper()
	intent := saveLoadHostedReleaseTransition(t, fixture.store, fixture.intent)
	intent.Phase = HostedReleaseTransitionPreflightVerified
	setPreflightReceipt(&intent, fixture.sourceWasPaused)
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionPausePrepared
	intent.PauseRequestID = "source-pause-request-1"
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionPauseVerified
	intent.PauseRunID, intent.PauseArtifactID = 101, 201
	intent.PauseArtifactName = "hive-hosted-control-101-1"
	intent.PauseDefaultHeadSHA, intent.PauseStateHeadSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionSourceStatusPrepared
	intent.SourceStatusRequestID = "source-status-request-1"
	intent = saveLoadHostedReleaseTransition(t, fixture.store, intent)
	intent.Phase = HostedReleaseTransitionSourceStatusVerified
	intent.SourceStatusRunID, intent.SourceStatusArtifactID = 105, 205
	intent.SourceStatusArtifactName = "hive-hosted-status-105-1"
	intent.SourceStatusDefaultHeadSHA, intent.SourceStatusStateHeadSHA = strings.Repeat("7", 40), strings.Repeat("f", 40)
	intent.SourceStatusPaused, intent.SourceStatusQuiescent = true, true
	return saveLoadHostedReleaseTransition(t, fixture.store, intent)
}

func setPreflightReceipt(intent *HostedReleaseTransitionIntent, sourceWasPaused bool) {
	intent.PreflightRunID, intent.PreflightArtifactID = 100, 200
	intent.PreflightArtifactName = "hive-hosted-status-100-1"
	intent.PreflightDefaultHeadSHA, intent.PreflightStateHeadSHA = strings.Repeat("0", 40), strings.Repeat("e", 40)
	intent.SourceWasPaused = sourceWasPaused
}

func setSetupProposal(t *testing.T, fixture hostedReleaseTransitionFixture, intent *HostedReleaseTransitionIntent) {
	t.Helper()
	intent.SetupBranch = managedOperationBranch(string(intent.Operation), intent.RepositoryID)
	intent.SetupCommitSHA = strings.Repeat("3", 40)
	intent.SetupBaseSHA = intent.SourceStatusDefaultHeadSHA
	intent.SetupDiffDigest = strings.Repeat("4", 64)
	intent.SetupMarker = HostedReleaseTransitionSetupMarker(intent.Operation, intent.RepositoryID, intent.ActiveRelease, intent.TargetRelease)
	candidate := fixture.candidate
	candidate.SetupBranch, candidate.SetupHeadSHA = intent.SetupBranch, intent.SetupCommitSHA
	sealed, digest, err := NewHostedReleaseTransitionCandidateConfig(candidate)
	if err != nil {
		t.Fatal(err)
	}
	intent.CandidateConfig, intent.CandidateConfigDigest = &sealed, digest
	intent.SetupCandidateDigest = digest
	intent.SetupChangedPaths = sortedUniquePaths(managedSetupFilesForConfig(candidate))
}

func newHostedReleaseTransitionFixture(t *testing.T, sourceWasPaused bool) hostedReleaseTransitionFixture {
	t.Helper()
	stateDir := t.TempDir()
	checkout := filepath.Join(stateDir, "integrated", "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	activeRelease := HostedReleaseIdentity{
		Version: "v0.4.1-integrated.19", HiveCommit: strings.Repeat("a", 40), VisualHiveCommit: strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("c", 64), HostedControllerProtocol: HostedControllerProtocol,
	}
	config := Config{
		SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", ExecutionMode: ExecutionHosted,
		RunIntervalSeconds: 900, HostedSchedule: "*/15 * * * *", HostedStateBranch: "hive/state-123",
		HiveReleaseRepository: "DavidDiaz0317/hive", HiveReleaseVersion: activeRelease.Version, HiveCommit: activeRelease.HiveCommit,
		DistributionManifestSHA256: activeRelease.DistributionManifestSHA256, HostedControllerProtocol: HostedControllerProtocol,
		ACMMLevel: 2, MaxActiveIssues: 5, MaxRepairAttempts: 3, VisualHive: true,
		VisualHiveRepo: "DavidDiaz0317/visual-hive", VisualHiveRef: activeRelease.VisualHiveCommit,
		VisualHiveConfigDigest: strings.Repeat("1", 64), StateDir: stateDir, CheckoutDir: checkout,
		SetupAuthorizationActorID: 42, InstalledAt: time.Now().UTC().Add(-time.Hour).Round(0),
	}
	config.ManagedPreimagesVersion = managedPreimagesVersion
	config.ManagedPathPreimages = map[string]ManagedPathPreimage{}
	managed := managedSetupFilesForConfig(config)
	for _, relative := range managed {
		config.ManagedPathPreimages[relative] = ManagedPathPreimage{}
	}
	content := []byte("repository-owned-before-hive\n")
	digest := sha256.Sum256(content)
	config.ManagedPathPreimages[managed[0]] = ManagedPathPreimage{Existed: true, GitMode: "100644", Content: content, ContentSHA256: hex.EncodeToString(digest[:])}
	setTransitionWorkflowDigest(t, &config)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	activeDigest, err := HostedReleaseTransitionConfigDigest(config)
	if err != nil {
		t.Fatal(err)
	}
	targetRelease := HostedReleaseIdentity{
		Version: "v0.4.1-integrated.20", HiveCommit: strings.Repeat("d", 40), VisualHiveCommit: strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("e", 64), HostedControllerProtocol: HostedControllerProtocol,
	}
	candidate := config
	candidate.ManagedPathPreimages = cloneManagedPathPreimages(config.ManagedPathPreimages)
	candidate.HiveReleaseVersion, candidate.HiveCommit = targetRelease.Version, targetRelease.HiveCommit
	candidate.DistributionManifestSHA256 = targetRelease.DistributionManifestSHA256
	candidate.PreviousHostedRelease = &activeRelease
	setTransitionWorkflowDigest(t, &candidate)
	return hostedReleaseTransitionFixture{
		store: store, candidate: candidate, sourceWasPaused: sourceWasPaused,
		intent: HostedReleaseTransitionIntent{
			Operation: HostedReleaseTransitionUpgrade, Phase: HostedReleaseTransitionPrepared,
			Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
			ActiveRelease: activeRelease, TargetRelease: targetRelease, ActiveConfigDigest: activeDigest,
			PreflightStatusRequestID: "preflight-status-request-1",
		},
	}
}

func setTransitionWorkflowDigest(t *testing.T, config *Config) {
	t.Helper()
	workflow, err := GenerateHostedControllerWorkflow(hostedWorkflowConfig(*config))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(workflow))
	config.HostedWorkflowSHA256 = hex.EncodeToString(digest[:])
}

func saveLoadHostedReleaseTransition(t *testing.T, store *Store, intent HostedReleaseTransitionIntent) HostedReleaseTransitionIntent {
	t.Helper()
	if err := store.SaveHostedReleaseTransitionIntent(intent); err != nil {
		t.Fatal(err)
	}
	return mustLoadHostedReleaseTransition(t, store)
}

func mustLoadHostedReleaseTransition(t *testing.T, store *Store) HostedReleaseTransitionIntent {
	t.Helper()
	intent, exists, err := store.LoadHostedReleaseTransitionIntent()
	if err != nil || !exists {
		t.Fatalf("load hosted release transition: exists=%t err=%v", exists, err)
	}
	return intent
}
