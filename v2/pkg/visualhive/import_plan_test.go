package visualhive

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestBuildImportPlanUsesVerifiedObservationsNotProducerPreview(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Beads) != 1 || bundle.Beads[0].Title == bundle.Manifest.Observations[0].Title {
		t.Fatal("fixture must contain a conflicting producer bead preview")
	}
	bundle.Beads[0].Title = "caller-forged preview"
	bundle.Beads[0].ExternalRef = "visual-hive://forged/preview"

	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	works := plan.Works()
	if len(works) != 1 {
		t.Fatalf("works = %d, want 1", len(works))
	}
	work := works[0]
	if work.Title != "Manifest-owned visual regression" || work.Bead.Title != work.Title {
		t.Fatalf("producer preview changed imported title: work=%q bead=%q", work.Title, work.Bead.Title)
	}
	if !strings.HasPrefix(work.SourceExternalRef, "visual-hive://owner/repo/") || work.SourceExternalRef != work.Bead.ExternalRef {
		t.Fatalf("source external ref = %q", work.SourceExternalRef)
	}
	if work.Role != SpecialistQuality || !work.RoutingAllowed || len(work.ValidationCommands) != 1 || len(work.ReproductionCommands) != 1 || work.ReproductionSource != "verified_observation_validation_command" || len(work.EvidenceArtifacts) != 1 {
		t.Fatalf("verified work lost route/validation/evidence: %+v", work)
	}
	if len(work.KnowledgeKeywords) != 0 || work.KnowledgeKeywordState != "unavailable_no_verified_facts" {
		t.Fatalf("import plan fabricated knowledge: %+v", work.KnowledgeKeywords)
	}
	if len(work.AllowedPaths) != 0 {
		t.Fatalf("import plan invented repair paths: %v", work.AllowedPaths)
	}
	if !reflect.DeepEqual(work.AffectedFiles, []string{"src/App.tsx"}) {
		t.Fatalf("import plan lost verified affected files: %v", work.AffectedFiles)
	}
	lifecycle, err := NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ApplyImportPlan(plan, store, ApplyLifecycleOptions{DisableIssuePublication: true}); err != nil {
		t.Fatal(err)
	}
	imported := store.FindByExternalRef(work.SourceExternalRef)
	if imported == nil || imported.Status != beads.StatusBlocked || imported.Metadata["visual_hive_controller_owned"] != true {
		t.Fatalf("sealed controller import was not admission-gated: %+v", imported)
	}
	keywords, keywordsExist := imported.Metadata["visual_hive_knowledge_keywords"]
	if imported.Metadata["visual_hive_knowledge_keyword_state"] != "unavailable_no_verified_facts" || !keywordsExist ||
		reflect.ValueOf(keywords).Kind() != reflect.Slice || reflect.ValueOf(keywords).Len() != 0 {
		t.Fatalf("normal bead did not preserve the explicit no-fabricated-knowledge state: %+v", imported.Metadata)
	}
}

func TestApplyImportPlanProjectsExactLegacyReplayIntoNormalStoreOnce(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := beads.NewStore(filepath.Join(t.TempDir(), "legacy-quality"))
	normal, _ := beads.NewStore(filepath.Join(t.TempDir(), "normal-quality"))
	first, err := lifecycle.ApplyBundle(bundle, legacy, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil || first.Created != 1 || legacy.Count() != 1 || normal.Count() != 0 {
		t.Fatalf("legacy apply = %+v err=%v legacy=%d normal=%d", first, err, legacy.Count(), normal.Count())
	}
	before := lifecycle.Snapshot()
	migrated, err := lifecycle.ApplyImportPlan(plan, normal, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	if err != nil || !migrated.Idempotent || migrated.BeadsCreated != 1 || normal.Count() != 1 {
		t.Fatalf("native activation replay = %+v err=%v normal=%d", migrated, err, normal.Count())
	}
	work := plan.Works()[0]
	projected := normal.FindByExternalRef(work.SourceExternalRef)
	if projected == nil || projected.Status != beads.StatusBlocked || projected.Metadata["visual_hive_admission_state"] != "pending" {
		t.Fatalf("normal controller projection = %+v", projected)
	}
	after := lifecycle.Snapshot()
	if len(after.Findings) != len(before.Findings) || len(after.Outbox) != len(before.Outbox) || after.ReplayKeys[plan.Packet().ReplayKey] != plan.Packet().PacketDigest {
		t.Fatalf("activation duplicated lifecycle effects: before=%+v after=%+v", before, after)
	}
	replay, err := lifecycle.ApplyImportPlan(plan, normal, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	if err != nil || !replay.Idempotent || replay.BeadsCreated != 0 || replay.BeadsSkipped != 1 || normal.Count() != 1 {
		t.Fatalf("native activation replay duplicated projection: %+v err=%v normal=%d", replay, err, normal.Count())
	}
}

func TestControllerProjectionUsesTargetStoreForNewPacketAndRejectsStaleHistoricalReplay(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	oldPlan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, _ := NewLifecycleStore(t.TempDir())
	legacy, _ := beads.NewStore(filepath.Join(t.TempDir(), "legacy"))
	normal, _ := beads.NewStore(filepath.Join(t.TempDir(), "normal"))
	if _, err := lifecycle.ApplyBundle(bundle, legacy, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true}); err != nil {
		t.Fatal(err)
	}
	newBundle := *oldPlan.seal.bundle
	newManifest, _ := cloneCanonicalManifest(newBundle.Manifest)
	newManifest.BundleID = "verified-bundle-new-packet"
	newManifest.ReplayProtection.Nonce = newManifest.BundleID
	newManifest.ReplayProtection.Key = strings.Repeat("7", 64)
	newManifest.OverallDigest = strings.Repeat("8", 64)
	newManifest.Provenance.SubjectDigest = newManifest.OverallDigest
	newBundle.Manifest, newBundle.validatedManifest = newManifest, &newManifest
	newPlan, err := newBundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	newResult, err := lifecycle.ApplyImportPlan(newPlan, normal, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	if err != nil || newResult.BeadsCreated != 1 || normal.Count() != 1 {
		t.Fatalf("new packet did not project existing legacy finding into target normal store: %+v err=%v", newResult, err)
	}
	fingerprint := newPlan.Works()[0].RepositoryFingerprint
	current, _ := lifecycle.Finding(fingerprint)
	staleTarget, _ := beads.NewStore(filepath.Join(t.TempDir(), "stale-target"))
	stale, err := lifecycle.ApplyImportPlan(oldPlan, staleTarget, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	after, _ := lifecycle.Finding(fingerprint)
	if err != nil || !stale.Idempotent || staleTarget.Count() != 0 || after.BeadID != current.BeadID || after.LastBundleID != newPlan.Packet().PacketID || after.LastBundleDigest != newPlan.Packet().PacketDigest {
		t.Fatalf("historical replay projected stale work or rebound lifecycle: result=%+v err=%v before=%+v after=%+v beads=%d", stale, err, current, after, staleTarget.Count())
	}
}

func TestControllerImportProjectsLegacyTombstonesForExplicitAndInferredAbsence(t *testing.T) {
	for _, test := range []struct {
		name     string
		inferred bool
	}{
		{name: "explicit absence"},
		{name: "exhaustive omitted absence", inferred: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
			bundle, err := ValidateBundle(manifestPath, ValidationOptions{
				Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
				ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
				ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
				t.Fatal(err)
			}
			presentPlan, _ := bundle.BuildImportPlan()
			lifecycle, _ := NewLifecycleStore(t.TempDir())
			legacy, _ := beads.NewStore(filepath.Join(t.TempDir(), "legacy"))
			normal, _ := beads.NewStore(filepath.Join(t.TempDir(), "normal"))
			if _, err := lifecycle.ApplyBundle(bundle, legacy, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true}); err != nil {
				t.Fatal(err)
			}
			fingerprint := presentPlan.Works()[0].RepositoryFingerprint
			if err := lifecycle.MarkIssueOpened(fingerprint, 81, "https://example.invalid/issues/81"); err != nil {
				t.Fatal(err)
			}
			absenceBundle := *presentPlan.seal.bundle
			manifest, _ := cloneCanonicalManifest(absenceBundle.Manifest)
			manifest.BundleID = "absence-" + strings.ReplaceAll(test.name, " ", "-")
			manifest.ReplayProtection.Nonce = manifest.BundleID
			manifest.ReplayProtection.Key = digest([]byte("replay:" + manifest.BundleID))
			manifest.Scan = Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"contract/app-shell"}}
			if test.inferred {
				manifest.Observations = nil
			} else {
				manifest.Observations[0].State = "absent"
			}
			manifest.OverallDigest = digest([]byte("packet:" + manifest.BundleID))
			manifest.Provenance.SubjectDigest = manifest.OverallDigest
			absenceBundle.Manifest, absenceBundle.validatedManifest = manifest, &manifest
			absencePlan, err := absenceBundle.BuildImportPlan()
			if err != nil {
				t.Fatal(err)
			}
			works, err := lifecycle.ResolveImportPlanWorks(absencePlan, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
			if err != nil || len(works) != 1 || works[0].ObservationState != "absent" || works[0].Bead.ExternalRef == "" {
				t.Fatalf("absence work did not pre-project durable owner: works=%+v err=%v", works, err)
			}
			result, err := lifecycle.ApplyImportPlan(absencePlan, normal, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
			finding, _ := lifecycle.Finding(fingerprint)
			if err != nil || result.Resolved != 1 || normal.Count() != 1 || normal.FindByExternalRef(works[0].SourceExternalRef) == nil || finding.Status != StatusResolved || finding.PendingIssueAction != OutboxCloseIssue {
				t.Fatalf("legacy absence migration = %+v err=%v finding=%+v normal=%+v", result, err, finding, normal.List(beads.ListFilter{}))
			}
			replay, err := lifecycle.ApplyImportPlan(absencePlan, normal, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
			if err != nil || !replay.Idempotent || replay.BeadsCreated != 0 || normal.Count() != 1 || len(lifecycle.PendingOutbox()) != 0 {
				t.Fatalf("absence replay duplicated tombstone or outbox: %+v err=%v beads=%d outbox=%+v", replay, err, normal.Count(), lifecycle.PendingOutbox())
			}
		})
	}
}

func TestBuildImportPlanRejectsPublicFieldForgery(t *testing.T) {
	forged := &ValidatedBundle{Validation: Validation{Trusted: true, Status: "passed"}}
	if _, err := forged.BuildImportPlan(); err == nil || !strings.Contains(err.Error(), "privately verified") {
		t.Fatalf("public-field forgery error = %v", err)
	}
}

func TestVerifiedImportPlanWorksReturnsDeepClonedNativeBeads(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	manifest := readManifest(t, manifestPath)
	root := manifest.Observations[0]
	aggregate := root
	aggregate.Fingerprint = "external-onboarding/aggregate"
	aggregate.RepositoryFingerprint = digest([]byte("owner/repo\x00" + aggregate.Fingerprint))
	aggregate.PublicationRole = "aggregate"
	aggregate.RootCauseKey = "aggregate/readiness"
	aggregate.BlockedByRootKeys = []string{root.RootCauseKey}
	aggregate.IssueKind = "external_repo_onboarding"
	aggregate.OwningAgentHint = "hive/operations"
	aggregate.Title = "Aggregate verified readiness"
	manifest.Observations = append(manifest.Observations, aggregate)
	manifest.BundleID = "deep-clone-native-beads"
	manifest.ReplayProtection.Nonce = manifest.BundleID
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)

	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Works()
	var returned *AdmittedVisualWork
	for index := range first {
		if first[index].RepositoryFingerprint == aggregate.RepositoryFingerprint {
			returned = &first[index]
			break
		}
	}
	if returned == nil || returned.Bead.Title != aggregate.Title || returned.Bead.Type != beads.TypeTask ||
		len(returned.Bead.DependsOn) != 1 || returned.Bead.DependsOn[0] != root.RepositoryFingerprint || returned.Bead.Metadata == nil {
		t.Fatalf("returned native bead lost payload or dependencies: %+v", returned)
	}
	original := cloneBatchInput(returned.Bead)
	returned.Bead.Title = "caller mutation"
	returned.Bead.DependsOn[0] = "caller dependency"
	returned.Bead.Metadata["visual_hive_route_reason"] = "caller route"
	artifacts, ok := returned.Bead.Metadata["visual_hive_evidence_artifacts"].([]map[string]interface{})
	if !ok || len(artifacts) == 0 {
		t.Fatalf("fixture lost nested artifact metadata: %#v", returned.Bead.Metadata["visual_hive_evidence_artifacts"])
	}
	artifacts[0]["sha256"] = "caller digest"
	dependencyRefs := returned.Bead.Metadata["visual_hive_dependency_external_refs"].([]string)
	dependencyRefs[0] = "caller external ref"

	second := plan.Works()
	var preserved *AdmittedVisualWork
	for index := range second {
		if second[index].RepositoryFingerprint == aggregate.RepositoryFingerprint {
			preserved = &second[index]
			break
		}
	}
	if preserved == nil || !reflect.DeepEqual(preserved.Bead, original) || !plan.Valid() {
		t.Fatalf("caller mutation changed the sealed import plan: preserved=%+v original=%+v valid=%t", preserved, original, plan.Valid())
	}
}

func TestApplyImportPlanRecordsZeroFindingRunAndReplayIsNoOp(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3Bundle(t, nil)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	beadStore, err := beads.NewStore(filepath.Join(t.TempDir(), "beads"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := lifecycle.ApplyImportPlan(plan, beadStore, ApplyLifecycleOptions{DisableIssuePublication: true})
	if err != nil || first.Idempotent {
		t.Fatalf("first apply = %+v, %v", first, err)
	}
	snapshot := lifecycle.Snapshot()
	if snapshot.LastImport == nil || snapshot.LastImport.Observations != 0 || snapshot.LastImport.PacketDigest != plan.Packet().PacketDigest {
		t.Fatalf("zero-finding receipt = %+v", snapshot.LastImport)
	}
	updatedAt := snapshot.UpdatedAt
	second, err := lifecycle.ApplyImportPlan(plan, beadStore, ApplyLifecycleOptions{DisableIssuePublication: true})
	if err != nil || !second.Idempotent {
		t.Fatalf("replay = %+v, %v", second, err)
	}
	if after := lifecycle.Snapshot().UpdatedAt; !after.Equal(updatedAt) {
		t.Fatalf("replay mutated lifecycle timestamp: before=%s after=%s", updatedAt, after)
	}
}

func TestApplyImportPlanPreservesAdmittedUpdateCloseAndRecurrenceActions(t *testing.T) {
	lifecycle, err := NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	apply := func(plan VerifiedImportPlan) ApplyLifecycleResult {
		result, applyErr := lifecycle.ApplyImportPlan(plan, store, ApplyLifecycleOptions{
			TargetRef: "main", CurrentTargetCommitSHA: plan.Packet().BaseSHA, VerificationRunID: plan.Packet().WorkflowRunID,
			VerificationCommitSHA: plan.Packet().BaseSHA, DisableIssuePublication: true, MaxActiveIssues: 3,
		})
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		if len(lifecycle.PendingOutbox()) != 0 {
			t.Fatalf("ApplyImportPlan created pre-admission outbox: %+v", lifecycle.PendingOutbox())
		}
		return result
	}

	present := testObservationImportPlan(t, "packet-present", "present", "Manifest-owned visual regression", "42")
	first := apply(present)
	work := present.Works()[0]
	if first.Created != 1 || first.BeadsCreated != 1 {
		t.Fatalf("initial apply = %+v", first)
	}
	assertControllerHeldBead(t, store, work.SourceExternalRef)
	if queued, queueErr := lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, present.Packet()); queueErr != nil || !queued {
		t.Fatalf("queue admitted open: queued=%t err=%v", queued, queueErr)
	}
	entry := lifecycle.PendingOutbox()[0]
	if entry.Action != OutboxOpenIssue {
		t.Fatalf("initial action = %s", entry.Action)
	}
	if err := lifecycle.MarkIssueOpened(work.RepositoryFingerprint, 101, "https://example.invalid/issues/101"); err != nil {
		t.Fatal(err)
	}
	if queued, queueErr := lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, present.Packet()); queueErr != nil || queued || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("crash replay between issue sync and outbox completion duplicated or stranded intent: queued=%t err=%v pending=%+v", queued, queueErr, lifecycle.PendingOutbox())
	}
	if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
		t.Fatal(err)
	}
	initialBead := store.FindByExternalRef(work.SourceExternalRef)
	if err := store.Update(initialBead.ID, func(bead *beads.Bead) {
		bead.Metadata["visual_hive_admission_state"] = "admitted_dispatch_pending"
		bead.Metadata["visual_hive_admission_decision_json"] = `{"old":true}`
		bead.Metadata["visual_hive_dispatch_envelope_json"] = `{"old":true}`
	}); err != nil {
		t.Fatal(err)
	}

	updated := testObservationImportPlan(t, "packet-updated", "present", "Updated verified title", "43")
	if result := apply(updated); result.Updated != 1 || result.BeadsCreated != 0 {
		t.Fatalf("update apply = %+v", result)
	}
	assertControllerHeldBead(t, store, work.SourceExternalRef)
	updatedBead := store.FindByExternalRef(work.SourceExternalRef)
	if _, exists := updatedBead.Metadata["visual_hive_admission_decision_json"]; exists {
		t.Fatal("changed verified packet reused an old Governor decision")
	}
	if _, exists := updatedBead.Metadata["visual_hive_dispatch_envelope_json"]; exists {
		t.Fatal("changed verified packet reused an old dispatch intent")
	}
	if queued, queueErr := lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, updated.Packet()); queueErr != nil || !queued {
		t.Fatalf("queue admitted update: queued=%t err=%v", queued, queueErr)
	}
	entry = lifecycle.PendingOutbox()[0]
	if entry.Action != OutboxUpdateIssue || entry.Title != "Updated verified title" {
		t.Fatalf("update action = %+v", entry)
	}
	if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
		t.Fatal(err)
	}

	absent := testObservationImportPlan(t, "packet-absent", "absent", "Updated verified title", "44")
	if result := apply(absent); result.Resolved != 1 {
		t.Fatalf("absence apply = %+v", result)
	}
	assertControllerHeldBead(t, store, work.SourceExternalRef)
	if queued, queueErr := lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, absent.Packet()); queueErr != nil || !queued {
		t.Fatalf("queue admitted close: queued=%t err=%v", queued, queueErr)
	}
	entry = lifecycle.PendingOutbox()[0]
	if entry.Action != OutboxCloseIssue {
		t.Fatalf("close action = %+v", entry)
	}
	if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkIssueClosed(work.RepositoryFingerprint, store); err != nil {
		t.Fatal(err)
	}
	closedBead := store.FindByExternalRef(work.SourceExternalRef)
	if closedBead == nil || closedBead.Status != beads.StatusClosed || len(store.Ready(closedBead.Actor)) != 0 {
		t.Fatalf("authoritative close did not retire normal WIP: bead=%+v ready=%+v", closedBead, store.Ready(closedBead.Actor))
	}

	recurrence := testObservationImportPlan(t, "packet-recurrence", "present", "Recurring verified title", "45")
	if result := apply(recurrence); result.Reopened != 1 || result.BeadsCreated != 0 {
		t.Fatalf("recurrence apply = %+v", result)
	}
	assertControllerHeldBead(t, store, work.SourceExternalRef)
	if finding, _ := lifecycle.Finding(work.RepositoryFingerprint); finding.Recurrences != 1 || finding.PendingIssueAction != OutboxReopenIssue {
		t.Fatalf("recurrence finding = %+v", finding)
	}
	if replay, replayErr := lifecycle.ApplyImportPlan(recurrence, store, ApplyLifecycleOptions{DisableIssuePublication: true}); replayErr != nil || !replay.Idempotent || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("exact recurrence replay = %+v err=%v outbox=%+v", replay, replayErr, lifecycle.PendingOutbox())
	}
}

func testObservationImportPlan(t *testing.T, packetID, state, title, runID string) VerifiedImportPlan {
	t.Helper()
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	manifest := readManifest(t, manifestPath)
	manifest.BundleID = packetID
	manifest.GeneratedAt = manifest.GeneratedAt.Add(time.Duration(len(packetID)) * time.Millisecond)
	manifest.ExpiresAt = manifest.ExpiresAt.Add(time.Duration(len(packetID)) * time.Millisecond)
	manifest.Source.CommitSHA = strings.Repeat("d", 40)
	manifest.Source.WorkflowRunID = runID
	manifest.Scan.EvaluatedContracts = []string{"contract/app-shell"}
	manifest.Observations[0].State = state
	manifest.Observations[0].Title = title
	manifest.ReplayProtection.Nonce = packetID
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: manifest.GeneratedAt.Add(time.Minute), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: runID, ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func assertControllerHeldBead(t *testing.T, store *beads.Store, externalRef string) {
	t.Helper()
	bead := store.FindByExternalRef(externalRef)
	if bead == nil || bead.Status != beads.StatusBlocked || bead.Metadata["visual_hive_admission_state"] != "pending" {
		t.Fatalf("controller-owned bead is actionable or missing: %+v", bead)
	}
}
