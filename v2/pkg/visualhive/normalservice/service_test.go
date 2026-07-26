package normalservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

func TestNormalServiceCrashReplayKeepsOneImportProposalPRAndVerdict(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.failCompletion = true
	service := fixture.service(t, fixture.verifier)

	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "completion checkpoint interrupted") {
		t.Fatalf("first cycle error = %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumes != 0 {
		t.Fatalf("first cycle duplicated or skipped work: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	ledger, exists, err := service.loadLedger()
	if err != nil || !exists || !bytes.Equal(ledger.VerdictReceipt, fixture.verifier.receipt.Receipt) {
		t.Fatalf("durable exact verdict bytes changed across JSON persistence: exists=%t err=%v receipt=%q", exists, err, ledger.VerdictReceipt)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("replay cycle: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumes != 1 {
		t.Fatalf("durable replay repeated import/proposal/PR/verdict: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	if fixture.intake.completion.VerdictStatus != "failed" || fixture.intake.completion.VerdictHeadSHA != fixture.repairer.outcome.Result.CommitSHA {
		t.Fatalf("red exact-head receipt was not recorded without reinterpretation: %+v", fixture.intake.completion)
	}
}

func TestNormalServiceAmbiguousConsumeRecoversWithoutNewWorkflowOrPR(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.source.failConsumeAfterSideEffect = true
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "consume response lost") {
		t.Fatalf("ambiguous consume error = %v", err)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("consume recovery: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("ambiguous consume replay duplicated work or deletion: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceRepairResponseLossRevalidatesWithoutRefetchOrReimport(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.repairer.failAfterSideEffect = true
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "Worker response lost") {
		t.Fatalf("ambiguous Worker response error = %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.sideEffects != 1 || fixture.verifier.calls != 0 {
		t.Fatalf("first ambiguous Worker cycle = source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("ambiguous Worker recovery: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.intake.revalidates != 1 ||
		fixture.repairer.runs != 2 || fixture.repairer.sideEffects != 1 || fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("Worker recovery refetched, reimported, or repeated a side effect: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceMultiFindingReplayDefersEveryUnselectedFindingBeforeOneWorkerPR(t *testing.T) {
	fixture := newServiceFixture(t)
	refs := []string{
		"visual-hive://owner/repo/z-last",
		"visual-hive://owner/repo/a-first",
		"visual-hive://owner/repo/m-middle",
	}
	fixture.intake.dispatch = []visualcontroller.DispatchEnvelope{
		{SourceExternalRef: refs[0], BaseSHA: strings.Repeat("9", 40), Work: visualhive.AdmittedVisualWork{RepositoryFingerprint: strings.Repeat("1", 64)}},
		{SourceExternalRef: refs[1], BaseSHA: strings.Repeat("9", 40), Work: visualhive.AdmittedVisualWork{RepositoryFingerprint: strings.Repeat("2", 64)}},
		{SourceExternalRef: refs[2], BaseSHA: strings.Repeat("9", 40), Work: visualhive.AdmittedVisualWork{RepositoryFingerprint: strings.Repeat("3", 64)}},
	}
	fixture.repairer.outcome.Result.RepositoryFingerprint = strings.Repeat("2", 64)
	fixture.intake.failDeferralAfterSideEffect = true
	service := fixture.service(t, fixture.verifier)

	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "deferral response lost") {
		t.Fatalf("first multi-finding cycle error = %v", err)
	}
	ledger, exists, err := service.loadLedger()
	if err != nil || !exists || ledger.SourceExternalRef != refs[1] || ledger.DeferralsRecorded ||
		!reflect.DeepEqual(ledger.DeferredSourceExternalRefs, []string{refs[2], refs[0]}) {
		t.Fatalf("durable deterministic selection = %+v exists=%t err=%v", ledger, exists, err)
	}
	if fixture.repairer.runs != 0 || fixture.verifier.calls != 0 || fixture.source.consumes != 0 || fixture.intake.imports != 1 {
		t.Fatalf("Worker or consume ran before peer deferrals: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}

	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("multi-finding replay: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.intake.deferralCalls != 2 ||
		fixture.repairer.runs != 1 || fixture.repairer.sideEffects != 1 || fixture.repairer.lastSourceExternalRef != refs[1] ||
		fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("multi-finding replay duplicated selection, Worker, PR, verdict, or deletion: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	completed, exists, err := service.loadLedger()
	if err != nil || !exists || !completed.DeferralsRecorded || !completed.Consumed ||
		!reflect.DeepEqual(completed.DeferredSourceExternalRefs, []string{refs[2], refs[0]}) {
		t.Fatalf("completed ledger lost deferred packet peers: %+v exists=%t err=%v", completed, exists, err)
	}
	for _, ref := range refs {
		if fixture.intake.represented[ref] != 1 {
			t.Fatalf("finding %s representation count = %d, want exactly one", ref, fixture.intake.represented[ref])
		}
	}
	for _, ref := range []string{refs[2], refs[0]} {
		if fixture.intake.deferred[ref] != 1 {
			t.Fatalf("unselected finding %s durable deferral count = %d, want exactly one", ref, fixture.intake.deferred[ref])
		}
	}
	if len(fixture.intake.deferred) != 2 || fixture.intake.deferred[refs[1]] != 0 {
		t.Fatalf("selected or unknown findings were deferred: %+v", fixture.intake.deferred)
	}
}

func TestSelectDispatchSetPrefersCanonicalRepairSignalOverAggregateAndCoverage(t *testing.T) {
	coverageRef := "visual-hive://owner/repo/a-coverage"
	aggregateRef := "visual-hive://owner/repo/b-aggregate"
	componentRef := "visual-hive://owner/repo/c-component"
	repairRef := "visual-hive://owner/repo/z-repair"
	values := []visualcontroller.DispatchEnvelope{
		{
			SourceExternalRef: coverageRef,
			Work: visualhive.AdmittedVisualWork{
				PublicationRole: "canonical",
				Severity:        "medium",
				IssueKind:       "missing_visual_coverage",
				RootCauseKey:    "finding/missing_visual_coverage/generic",
			},
		},
		{
			SourceExternalRef: aggregateRef,
			Work: visualhive.AdmittedVisualWork{
				PublicationRole: "aggregate",
				Severity:        "critical",
				IssueKind:       "provider_governance",
				RootCauseKey:    "aggregate/provider/playwright",
			},
		},
		{
			SourceExternalRef: repairRef,
			Work: visualhive.AdmittedVisualWork{
				PublicationRole: "canonical",
				Severity:        "high",
				IssueKind:       "selector_contract_failure",
				RootCauseKey:    "contract/localPreview/guarded-repair-policy",
			},
		},
		{
			SourceExternalRef: componentRef,
			Work: visualhive.AdmittedVisualWork{
				PublicationRole: "canonical",
				Severity:        "high",
				IssueKind:       "selector_contract_failure",
				RootCauseKey:    "contract/componentLibrary/component-lab-storybook",
			},
		},
	}

	selected, deferred, ok, err := selectDispatchSet(values)
	if err != nil || !ok {
		t.Fatalf("select dispatch set: ok=%t err=%v", ok, err)
	}
	if selected.SourceExternalRef != repairRef {
		t.Fatalf("selected source ref = %q, want canonical repair signal %q", selected.SourceExternalRef, repairRef)
	}
	if !reflect.DeepEqual(deferred, []string{coverageRef, aggregateRef, componentRef}) {
		t.Fatalf("deferred source refs = %v, want stable lexical peers", deferred)
	}
}

func TestNormalServiceLeavesWorkerPROpenUntilExactHeadVerifierExists(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.service(t, nil)
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrFinalVerdictPending) {
		t.Fatalf("cycle error = %v, want pending verdict", err)
	}
	if fixture.repairer.runs != 1 || fixture.source.consumes != 0 || fixture.intake.completes != 0 {
		t.Fatalf("missing verifier consumed or completed PR: source=%+v intake=%+v repair=%+v", fixture.source, fixture.intake, fixture.repairer)
	}
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrFinalVerdictPending) {
		t.Fatalf("replay error = %v, want pending verdict", err)
	}
	if fixture.repairer.runs != 1 || fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.intake.revalidates != 0 || fixture.source.consumes != 0 {
		t.Fatalf("pending exact PR was refetched, reimported, duplicated, or consumed: source=%+v intake=%+v repair=%+v", fixture.source, fixture.intake, fixture.repairer)
	}
}

func TestNormalServiceExistingWorkerPRVerdictIgnoresLaterLaunchPolicyDrift(t *testing.T) {
	fixture := newServiceFixture(t)
	stateDir := t.TempDir()
	withoutVerifier := fixture.serviceAt(t, stateDir, nil)
	if err := withoutVerifier.RunCycle(context.Background()); !errors.Is(err, ErrFinalVerdictPending) {
		t.Fatalf("initial Worker PR cycle error = %v, want pending verdict", err)
	}
	if fixture.repairer.runs != 1 || fixture.verifier.calls != 0 || fixture.intake.revalidates != 0 {
		t.Fatalf("initial Worker PR cycle unexpectedly replayed launch checks: intake=%+v repair=%+v verifier=%+v", fixture.intake, fixture.repairer, fixture.verifier)
	}

	fixture.intake.revalidateErr = errors.New("current Governor mode no longer enables the selected role")
	withVerifier := fixture.serviceAt(t, stateDir, fixture.verifier)
	if err := withVerifier.RunCycle(context.Background()); err != nil {
		t.Fatalf("existing Worker PR verdict was blocked by launch-only policy drift: %v", err)
	}
	if fixture.intake.revalidates != 0 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("existing Worker PR replay relaunched work or skipped exact verdict: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceNoDispatchIsIdleAndNeverRunsWorker(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrNoDispatch) {
		t.Fatalf("cycle error = %v, want idle", err)
	}
	if fixture.repairer.runs != 0 || fixture.verifier.calls != 0 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("pause/WIP/no-work path launched specialist or failed to retire exact report")
	}
}

func TestNormalServiceNoDispatchAmbiguousConsumeDoesNotStartAnotherWorkflow(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	fixture.source.failConsumeAfterSideEffect = true
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "consume response lost") {
		t.Fatalf("ambiguous no-dispatch consume error = %v", err)
	}
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrNoDispatch) {
		t.Fatalf("ambiguous no-dispatch recovery = %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.source.consumeSideEffects != 1 ||
		fixture.source.consumes != 2 || fixture.repairer.runs != 0 || fixture.verifier.calls != 0 {
		t.Fatalf("no-dispatch recovery started new work: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceRetainsConsumedLedgerUntilExactWorkerPullRequestCloses(t *testing.T) {
	fixture := newServiceFixture(t)
	stateDir := t.TempDir()
	service := fixture.serviceAt(t, stateDir, fixture.verifier)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A fresh process must inherit the same durable gate before any Fetch.
	service = fixture.serviceAt(t, stateDir, fixture.verifier)
	fixture.verifier.stateErr = errors.New("live PR state unavailable")
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "state unavailable") {
		t.Fatalf("PR observation failure did not fail closed: %v", err)
	}
	fixture.verifier.stateErr = nil
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrOpenPullRequest) {
		t.Fatalf("open Worker PR gate = %v, want %v", err, ErrOpenPullRequest)
	}
	ledger, exists, err := service.loadLedger()
	if err != nil || !exists || !ledger.Consumed || ledger.PullRequestNumber != fixture.repairer.outcome.Result.PRNumber {
		t.Fatalf("open Worker PR lost its durable serialization ledger: ledger=%+v exists=%t err=%v", ledger, exists, err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.verifier.stateCalls != 2 {
		t.Fatalf("open Worker PR allowed a competing packet or proposal: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}

	fixture.verifier.pullRequestOpen = false
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("closed Worker PR did not release the next packet: %v", err)
	}
	if fixture.source.fetches != 2 || fixture.intake.imports != 2 || fixture.repairer.runs != 2 || fixture.verifier.calls != 2 || fixture.verifier.stateCalls != 3 {
		t.Fatalf("closed Worker PR did not release exactly one next proposal: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceAcceptsClosedWorkerPullRequestWhetherMergedOrUnmerged(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "merged"}[merged], func(t *testing.T) {
			fixture := newServiceFixture(t)
			service := fixture.service(t, fixture.verifier)
			if err := service.RunCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			fixture.verifier.pullRequestOpen = false
			fixture.verifier.pullRequestMerged = merged
			if err := service.RunCycle(context.Background()); err != nil {
				t.Fatalf("terminal Worker PR did not release next packet: %v", err)
			}
			if fixture.source.fetches != 2 || fixture.repairer.runs != 2 || fixture.verifier.stateCalls != 1 {
				t.Fatalf("terminal Worker PR released an inexact number of proposals: source=%+v repair=%+v verifier=%+v", fixture.source, fixture.repairer, fixture.verifier)
			}
		})
	}
}

func TestNormalServiceRejectsInconsistentWorkerPullRequestStateWithoutNewProposal(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.verifier.stateObservation = &PullRequestStateObservation{State: "closed", Open: true}
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "inconsistent state") {
		t.Fatalf("inconsistent PR state did not fail closed: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 {
		t.Fatalf("inconsistent PR state reached a competing proposal: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceConsumedNoDispatchLedgerBypassesPullRequestObserver(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrNoDispatch) {
		t.Fatal(err)
	}
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrNoDispatch) {
		t.Fatal(err)
	}
	if fixture.verifier.stateCalls != 0 || fixture.source.fetches != 2 || fixture.repairer.runs != 0 {
		t.Fatalf("green/no-dispatch ledger incorrectly entered PR gate: source=%+v repair=%+v verifier=%+v", fixture.source, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceLeaseContentionDoesNotRunCycleUntilOwnership(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claims, released := 0, false
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, LeaseRetry: time.Millisecond,
		AcquireLease: func() (func(), error) {
			claims++
			if claims == 1 {
				return nil, integrated.ErrRunInProgress
			}
			return func() { released = true }, nil
		},
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycle: func(err error) {
			if errors.Is(err, ErrNoDispatch) {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Run(ctx)
	if claims != 2 || !released || fixture.source.fetches != 1 {
		t.Fatalf("lease reconciliation = claims=%d released=%t fetches=%d", claims, released, fixture.source.fetches)
	}
}

func TestNormalServiceTriggerRunsImmediateCycleThroughExistingLeaseEpoch(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	firstCycle := make(chan struct{}, 1)
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour,
		AcquireLease:  func() (func(), error) { return func() {}, nil },
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycle: func(error) {
			select {
			case firstCycle <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()
	select {
	case <-firstCycle:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("normal service did not complete its initial cycle")
	}
	triggerCtx, triggerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer triggerCancel()
	if err := service.Trigger(triggerCtx); !errors.Is(err, ErrNoDispatch) {
		cancel()
		t.Fatalf("triggered cycle result = %v", err)
	}
	if fixture.source.fetches != 2 {
		cancel()
		t.Fatalf("trigger did not run exactly one immediate cycle: fetches=%d", fixture.source.fetches)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not stop after trigger test")
	}
}

func TestNormalServiceTriggerDoesNotBypassQuiescence(t *testing.T) {
	fixture := newServiceFixture(t)
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, QuiesceInterval: time.Millisecond,
		AcquireLease:  func() (func(), error) { t.Fatal("quiesced service acquired a lease"); return nil, nil },
		ShouldQuiesce: func() (bool, error) { return true, nil },
		Source:        fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(runCtx)
		close(done)
	}()
	triggerCtx, triggerCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer triggerCancel()
	if err := service.Trigger(triggerCtx); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("quiesced trigger result = %v", err)
	}
	if fixture.source.fetches != 0 {
		cancel()
		t.Fatalf("quiesced trigger touched evidence: fetches=%d", fixture.source.fetches)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("quiesced service did not stop")
	}
}

func TestNormalServicePublishesBoundedCycleStartBeforeRunCycle(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	started := make(chan struct{})
	observedDeadline := make(chan time.Time, 1)
	source := &cycleWindowArtifactSource{inner: fixture.source, started: started, deadline: observedDeadline}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const maximum = 20 * time.Minute
	var callbackStart, callbackDeadline time.Time
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, MaxCycleDuration: maximum,
		AcquireLease:  func() (func(), error) { return func() {}, nil },
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycleStart: func(start, deadline time.Time) {
			callbackStart, callbackDeadline = start, deadline
			close(started)
		},
		OnCycle: func(err error) {
			if errors.Is(err, ErrNoDispatch) {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Run(ctx)
	if callbackStart.IsZero() || !callbackDeadline.Equal(callbackStart.Add(maximum)) {
		t.Fatalf("cycle callback window = %v .. %v", callbackStart, callbackDeadline)
	}
	select {
	case deadline := <-observedDeadline:
		if !deadline.Equal(callbackDeadline) {
			t.Fatalf("RunCycle context deadline = %v, callback = %v", deadline, callbackDeadline)
		}
	default:
		t.Fatal("RunCycle did not observe the published bounded deadline")
	}
}

func TestNormalServiceMaximumCycleDurationCancelsBlockedCycle(t *testing.T) {
	fixture := newServiceFixture(t)
	cycleResult := make(chan error, 1)
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, MaxCycleDuration: 20 * time.Millisecond,
		AcquireLease:  func() (func(), error) { return func() {}, nil },
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        blockingArtifactSource{}, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycle: func(err error) {
			select {
			case cycleResult <- err:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()
	select {
	case err := <-cycleResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded cycle result = %v", err)
		}
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("bounded cycle did not reach its deadline")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not stop after bounded cycle test")
	}
}

func TestNormalServiceParentCancellationUnwindsCycleBeforeLeaseRelease(t *testing.T) {
	fixture := newServiceFixture(t)
	repairer := &pauseReplayRepairer{
		outcome: fixture.repairer.outcome, firstStarted: make(chan struct{}), firstCanceled: make(chan struct{}), secondStarted: make(chan struct{}),
	}
	released := make(chan struct{})
	releasedBeforeUnwind := make(chan struct{}, 1)
	cycleResults := make(chan error, 1)
	inactive := make(chan struct{}, 1)
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, LeaseRetry: time.Millisecond, QuiesceInterval: time.Millisecond,
		AcquireLease: func() (func(), error) {
			return func() {
				select {
				case <-repairer.firstCanceled:
				default:
					releasedBeforeUnwind <- struct{}{}
				}
				close(released)
			}, nil
		},
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        fixture.source, Intake: fixture.intake, Repairer: repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycle: func(err error) { cycleResults <- err },
		OnInactive: func() {
			select {
			case inactive <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()
	select {
	case <-repairer.firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not reach cancellable Worker")
	}
	cancel()
	select {
	case <-repairer.firstCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("active Worker did not observe parent cancellation")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("lease was not released after parent cancellation")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not return after parent cancellation")
	}
	select {
	case <-releasedBeforeUnwind:
		t.Fatal("lease released before the active Worker context unwound")
	default:
	}
	select {
	case err := <-cycleResults:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("parent-canceled cycle result = %v", err)
		}
	default:
		t.Fatal("parent cancellation did not complete the cycle callback")
	}
	select {
	case <-inactive:
	default:
		t.Fatal("parent cancellation did not mark the service loop inactive")
	}
}

func TestNormalServicePauseReleasesRealLeaseAndResumesExactLedgerWithoutDuplicateWorkerSideEffect(t *testing.T) {
	stateDir := t.TempDir()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{Repository: "owner/repo", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	fixture := newServiceFixture(t)
	cycles := make(chan error, 8)
	inactive := make(chan struct{}, 8)
	repairer := &pauseReplayRepairer{
		outcome: fixture.repairer.outcome, firstStarted: make(chan struct{}), firstCanceled: make(chan struct{}), secondStarted: make(chan struct{}),
	}
	service, err := New(Options{
		StateDir: filepath.Join(stateDir, "visual-hive"), PollInterval: time.Hour, LeaseRetry: 10 * time.Millisecond, QuiesceInterval: 10 * time.Millisecond,
		AcquireLease: func() (func(), error) { return integrated.AcquireNormalVisualWorkLease(stateDir) },
		ShouldQuiesce: func() (bool, error) {
			requested, err := integrated.PauseRequested(stateDir)
			if err != nil || requested {
				return requested, err
			}
			config, err := store.Load()
			return config.Paused, err
		},
		Source: fixture.source, Intake: fixture.intake, Repairer: repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
		OnCycle: func(err error) {
			select {
			case cycles <- err:
			default:
			}
		},
		OnInactive: func() {
			select {
			case inactive <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("normal service did not stop")
		}
	}()

	select {
	case <-repairer.firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not reach the first Worker attempt")
	}
	if release, err := integrated.AcquireNormalVisualWorkLease(stateDir); !errors.Is(err, integrated.ErrRunInProgress) {
		if err == nil {
			release()
		}
		t.Fatalf("legacy production writer overlapped the active normal epoch: %v", err)
	}

	pauseCtx, pauseCancel := context.WithTimeout(context.Background(), 3*time.Second)
	paused, err := integrated.SetPaused(pauseCtx, stateDir, true)
	pauseCancel()
	if err != nil || !paused.Paused {
		t.Fatalf("pause did not quiesce the normal service: paused=%t err=%v", paused.Paused, err)
	}
	select {
	case <-repairer.firstCanceled:
	default:
		t.Fatal("pause completed before the active Worker context unwound")
	}
	select {
	case <-inactive:
	default:
		t.Fatal("pause did not mark the normal service loop inactive")
	}
	select {
	case <-repairer.secondStarted:
		t.Fatal("normal service resumed a Worker while durable pause was active")
	case <-time.After(150 * time.Millisecond):
	}
	pausedLease, err := integrated.AcquireNormalVisualWorkLease(stateDir)
	if err != nil {
		t.Fatalf("paused service retained the production lease: %v", err)
	}
	pausedLease()

	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	resumed, err := integrated.SetPaused(resumeCtx, stateDir, false)
	resumeCancel()
	if err != nil || resumed.Paused {
		t.Fatalf("resume failed: paused=%t err=%v", resumed.Paused, err)
	}
	select {
	case <-repairer.secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not reacquire ownership and resume the exact Worker")
	}
	waitForSuccessfulCycle(t, cycles, 3*time.Second)
	runs, sideEffects := repairer.counts()
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || runs != 2 || sideEffects != 1 || fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("pause replay refetched or duplicated durable work: source=%+v intake=%+v runs=%d sideEffects=%d verifier=%+v", fixture.source, fixture.intake, runs, sideEffects, fixture.verifier)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not release its resumed epoch")
	}
	finalLease, err := integrated.AcquireNormalVisualWorkLease(stateDir)
	if err != nil {
		t.Fatalf("stopped normal service retained the production lease: %v", err)
	}
	finalLease()
}

func TestNormalServiceSetupBaselinePROpenReleasesLeaseAndApprovedResumes(t *testing.T) {
	stateDir := t.TempDir()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	prOpen := normalServiceSetupBaselineIntent(t, integrated.SetupBaselinePROpen)
	starts := make(chan int, 4)
	source := &setupBaselineQuiescenceSource{
		starts: starts,
		onFirst: func() error {
			return store.SaveSetupBaselineIntent(prOpen)
		},
	}
	fixture := newServiceFixture(t)
	service, err := New(Options{
		StateDir: filepath.Join(stateDir, "visual-hive"), PollInterval: time.Hour, LeaseRetry: 10 * time.Millisecond, QuiesceInterval: 10 * time.Millisecond,
		AcquireLease:  func() (func(), error) { return integrated.AcquireNormalVisualWorkLease(stateDir) },
		ShouldQuiesce: func() (bool, error) { return integrated.NormalVisualWorkRequiresSetupQuiescence(stateDir) },
		Source:        source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier, PullRequestState: fixture.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("normal service did not stop")
		}
	}()

	select {
	case call := <-starts:
		if call != 1 {
			t.Fatalf("first setup-baseline service fetch = %d", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not reach setup-baseline reconciliation")
	}
	approvalLease := acquireNormalServiceTestLease(t, stateDir, 3*time.Second)
	approvalLease()
	time.Sleep(50 * time.Millisecond)
	stillFree := acquireNormalServiceTestLease(t, stateDir, time.Second)
	stillFree()

	approved := prOpen
	approved.Phase = integrated.SetupBaselineApproved
	approved.ApprovalPlanDigest = strings.Repeat("2", 64)
	approved.ApprovalActorID = approved.AuthorizerID
	approved.ApprovalReason = "reviewed"
	if err := store.SaveSetupBaselineIntent(approved); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-starts:
		if call != 2 {
			t.Fatalf("resumed setup-baseline service fetch = %d", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("normal service did not reacquire after exact approval")
	}
	if release, err := integrated.AcquireNormalVisualWorkLease(stateDir); !errors.Is(err, integrated.ErrRunInProgress) {
		if err == nil {
			release()
		}
		t.Fatalf("approved service did not reacquire the production lease: %v", err)
	}
}

func acquireNormalServiceTestLease(t *testing.T, stateDir string, timeout time.Duration) func() {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		release, err := integrated.AcquireNormalVisualWorkLease(stateDir)
		if err == nil {
			return release
		}
		if !errors.Is(err, integrated.ErrRunInProgress) {
			t.Fatalf("acquire normal service test lease: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("normal service retained the production lease past the setup-baseline approval hold")
	return nil
}

func normalServiceSetupBaselineIntent(t *testing.T, phase string) integrated.SetupBaselineIntent {
	t.Helper()
	now := time.Now().UTC()
	correlation := strings.Repeat("c", 64)
	candidate := integrated.SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("e", 64), Bytes: 9}
	candidatesJSON, err := json.Marshal([]integrated.SetupBaselineCandidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest := sha256.Sum256(candidatesJSON)
	emptyJSON, err := json.Marshal([]integrated.SetupBaselineCandidate{})
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest := sha256.Sum256(emptyJSON)
	return integrated.SetupBaselineIntent{
		SchemaVersion: integrated.SetupBaselineSchema, Phase: phase, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 1, SetupPRURL: "https://example.test/pull/1", SetupHeadSHA: strings.Repeat("a", 40),
		VisualHiveConfigDigest: strings.Repeat("8", 64), ScreenshotContractDigest: strings.Repeat("9", 64), InitialBaselineDigest: hex.EncodeToString(emptyDigest[:]),
		CaptureCorrelation: correlation, CaptureHeadSHA: strings.Repeat("a", 40), CaptureRunID: 77, CaptureRunURL: "https://example.test/run/77",
		ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root", CaptureCandidateDigest: hex.EncodeToString(candidateDigest[:]), CaptureCandidates: []integrated.SetupBaselineCandidate{candidate},
		CandidateDigest: hex.EncodeToString(candidateDigest[:]), Candidates: []integrated.SetupBaselineCandidate{candidate}, Branch: "hive/setup-baseline-123", CommitSHA: strings.Repeat("f", 40),
		Marker: "<!-- hive-setup-baseline: owner/repo:" + correlation + " -->", PRNumber: 2, PRURL: "https://example.test/pull/2", BaseSHA: strings.Repeat("a", 40),
		DiffDigest: strings.Repeat("1", 64), CreatedAt: now, UpdatedAt: now, DispatchAttemptedAt: now, DispatchAcknowledgedAt: now,
	}
}

func waitForSuccessfulCycle(t *testing.T, cycles <-chan error, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case err := <-cycles:
			if err == nil {
				return
			}
		case <-timer.C:
			t.Fatal("normal service did not durably consume the resumed workflow")
		}
	}
}

func TestNormalServiceRejectsCorruptLedgerStateMachineBeforeSideEffects(t *testing.T) {
	fixture := newServiceFixture(t)
	key, err := workflowKey(fixture.work)
	if err != nil {
		t.Fatal(err)
	}
	base := workLedger{
		SchemaVersion: ledgerSchema, WorkflowKey: key, Workflow: fixture.work.Workflow,
		Repository: fixture.work.Config.Repository, BaseBranch: fixture.work.Config.DefaultBranch, PacketDigest: fixture.work.Artifact.BundleSHA256,
	}
	for name, mutate := range map[string]func(*workLedger){
		"workflow key": func(ledger *workLedger) { ledger.WorkflowKey = strings.Repeat("0", 64) },
		"consumed without checkpoint": func(ledger *workLedger) {
			ledger.Consumed = true
		},
		"PR without order": func(ledger *workLedger) {
			ledger.SourceExternalRef = "visual-hive://owner/repo/finding"
			ledger.Branch, ledger.CommitSHA = "hive/repair-one", strings.Repeat("f", 40)
			ledger.PullRequestNumber, ledger.PullRequestURL = 9, "https://example.test/pr/9"
		},
		"unsorted deferred sources": func(ledger *workLedger) {
			ledger.SourceExternalRef = "visual-hive://owner/repo/selected"
			ledger.DeferredSourceExternalRefs = []string{"visual-hive://owner/repo/z", "visual-hive://owner/repo/a"}
		},
		"side effect before deferral checkpoint": func(ledger *workLedger) {
			ledger.SourceExternalRef = "visual-hive://owner/repo/selected"
			ledger.DeferredSourceExternalRefs = []string{"visual-hive://owner/repo/peer"}
			ledger.WorkOrderID, ledger.RequestSHA256 = "swo-"+strings.Repeat("e", 64), strings.Repeat("e", 64)
		},
		"verdict digest": func(ledger *workLedger) {
			ledger.SourceExternalRef = "visual-hive://owner/repo/finding"
			ledger.WorkOrderID, ledger.RequestSHA256 = "swo-"+strings.Repeat("e", 64), strings.Repeat("e", 64)
			ledger.Branch, ledger.CommitSHA = "hive/repair-one", strings.Repeat("f", 40)
			ledger.PullRequestNumber, ledger.PullRequestURL = 9, "https://example.test/pr/9"
			ledger.VerdictHeadSHA, ledger.VerdictStatus = ledger.CommitSHA, "failed"
			ledger.VerdictReceipt = []byte(`{"status":"failed"}`)
			ledger.VerdictReceiptSHA256 = strings.Repeat("1", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			service := fixture.service(t, fixture.verifier)
			corrupt := base
			mutate(&corrupt)
			encoded, marshalErr := json.Marshal(corrupt)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if err := os.WriteFile(service.ledgerPath(), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "ledger is corrupt") {
				t.Fatalf("corrupt ledger was accepted: %v", err)
			}
			if fixture.source.fetches != 0 || fixture.intake.imports != 0 || fixture.repairer.runs != 0 || fixture.verifier.calls != 0 {
				t.Fatalf("corrupt ledger reached side effects: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
			}
		})
	}
}

type serviceFixture struct {
	work     integrated.NormalVisualWork
	source   *fakeArtifactSource
	intake   *fakeIntake
	repairer *fakeRepairer
	verifier *fakeVerdictVerifier
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	head := strings.Repeat("b", 40)
	bundle := strings.Repeat("a", 64)
	work := integrated.NormalVisualWork{
		Config:   integrated.Config{Repository: "owner/repo", DefaultBranch: "main"},
		Workflow: integrated.WorkflowRunEvidence{CorrelationID: strings.Repeat("c", 64), RunID: 42, HeadSHA: head, BundleArtifact: 7, EvidenceArtifact: 8},
		Artifact: hivegithub.VerifiedVisualHiveArtifact{BundleSHA256: bundle},
	}
	envelope := visualcontroller.DispatchEnvelope{
		SourceExternalRef: "visual-hive://owner/repo/finding",
		BaseSHA:           strings.Repeat("9", 40),
		Work:              visualhive.AdmittedVisualWork{RepositoryFingerprint: strings.Repeat("d", 64)},
	}
	outcome := RepairOutcome{
		WorkOrderID: "swo-" + strings.Repeat("e", 64), RequestSHA256: strings.Repeat("e", 64),
		Result: repair.Result{RepositoryFingerprint: envelope.Work.RepositoryFingerprint, Branch: "hive/repair-one", CommitSHA: strings.Repeat("f", 40), PRNumber: 9, PRURL: "https://example.test/pr/9"},
	}
	receipt := json.RawMessage(`{"schema_version":"visual-hive.pr-verdict.v1","status":"failed"}`)
	digest := sha256.Sum256(receipt)
	return &serviceFixture{
		work:     work,
		source:   &fakeArtifactSource{work: work},
		intake:   &fakeIntake{dispatch: []visualcontroller.DispatchEnvelope{envelope}},
		repairer: &fakeRepairer{outcome: outcome},
		verifier: &fakeVerdictVerifier{pullRequestOpen: true, receipt: PullRequestVerdictReceipt{HeadSHA: outcome.Result.CommitSHA, Status: "failed", Receipt: receipt, ReceiptSHA256: hex.EncodeToString(digest[:])}},
	}
}

func (fixture *serviceFixture) service(t *testing.T, verifier PullRequestVerdictVerifier) *Service {
	return fixture.serviceAt(t, t.TempDir(), verifier)
}

func (fixture *serviceFixture) serviceAt(t *testing.T, stateDir string, verifier PullRequestVerdictVerifier) *Service {
	t.Helper()
	service, err := New(Options{
		StateDir: stateDir, AcquireLease: func() (func(), error) { return func() {}, nil },
		ShouldQuiesce: func() (bool, error) { return false, nil },
		Source:        fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: verifier, PullRequestState: fixture.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type fakeArtifactSource struct {
	mu                         sync.Mutex
	work                       integrated.NormalVisualWork
	fetches                    int
	consumes                   int
	consumeSideEffects         int
	failConsumeAfterSideEffect bool
}

type cycleWindowArtifactSource struct {
	inner    *fakeArtifactSource
	started  <-chan struct{}
	deadline chan<- time.Time
}

func (source *cycleWindowArtifactSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	select {
	case <-source.started:
	default:
		return integrated.NormalVisualWork{}, errors.New("RunCycle started before OnCycleStart")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return integrated.NormalVisualWork{}, errors.New("RunCycle context has no bounded deadline")
	}
	source.deadline <- deadline
	return source.inner.Fetch(ctx)
}

func (source *cycleWindowArtifactSource) Consume(workflow integrated.WorkflowRunEvidence, allow bool) error {
	return source.inner.Consume(workflow, allow)
}

type blockingArtifactSource struct{}

func (blockingArtifactSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	<-ctx.Done()
	return integrated.NormalVisualWork{}, ctx.Err()
}

func (blockingArtifactSource) Consume(integrated.WorkflowRunEvidence, bool) error {
	return nil
}

type setupBaselineQuiescenceSource struct {
	mu      sync.Mutex
	calls   int
	starts  chan<- int
	onFirst func() error
}

func (source *setupBaselineQuiescenceSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	source.mu.Lock()
	source.calls++
	call := source.calls
	source.mu.Unlock()
	if call == 1 && source.onFirst != nil {
		if err := source.onFirst(); err != nil {
			return integrated.NormalVisualWork{}, err
		}
	}
	source.starts <- call
	if call == 1 {
		return integrated.NormalVisualWork{}, errors.New("setup baseline review requires exact approval")
	}
	<-ctx.Done()
	return integrated.NormalVisualWork{}, ctx.Err()
}

func (*setupBaselineQuiescenceSource) Consume(integrated.WorkflowRunEvidence, bool) error {
	return nil
}

func (source *fakeArtifactSource) Fetch(context.Context) (integrated.NormalVisualWork, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.fetches++
	return source.work, nil
}

func (source *fakeArtifactSource) Consume(_ integrated.WorkflowRunEvidence, allow bool) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.consumes++
	if source.consumeSideEffects == 0 {
		source.consumeSideEffects++
		if source.failConsumeAfterSideEffect {
			source.failConsumeAfterSideEffect = false
			return errors.New("consume response lost")
		}
		return nil
	}
	if !allow {
		return errors.New("duplicate consume without recovery authority")
	}
	return nil
}

type fakeIntake struct {
	dispatch                    []visualcontroller.DispatchEnvelope
	imports                     int
	revalidates                 int
	revalidateErr               error
	deferralCalls               int
	completes                   int
	failCompletion              bool
	failDeferralAfterSideEffect bool
	represented                 map[string]int
	deferred                    map[string]int
	deferralSelection           string
	deferralCorrelation         string
	completion                  visualcontroller.SpecialistPullRequestCompletion
}

func (intake *fakeIntake) Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	intake.imports++
	if intake.represented == nil {
		intake.represented = map[string]int{}
	}
	for _, envelope := range intake.dispatch {
		if intake.represented[envelope.SourceExternalRef] == 0 {
			intake.represented[envelope.SourceExternalRef] = 1
		}
	}
	return visualcontroller.Result{DispatchPending: append([]visualcontroller.DispatchEnvelope(nil), intake.dispatch...)}, nil
}

func (intake *fakeIntake) RevalidateSpecialistBoundary(sourceExternalRef, _, _ string) (visualcontroller.DispatchEnvelope, error) {
	intake.revalidates++
	if intake.revalidateErr != nil {
		return visualcontroller.DispatchEnvelope{}, intake.revalidateErr
	}
	for _, envelope := range intake.dispatch {
		if envelope.SourceExternalRef == sourceExternalRef {
			return envelope, nil
		}
	}
	return visualcontroller.DispatchEnvelope{}, errors.New("exact durable dispatch is unavailable")
}

func (intake *fakeIntake) DeferUnselectedSpecialistDispatches(_ context.Context, selected string, refs []string, correlation string) error {
	intake.deferralCalls++
	if intake.deferred == nil {
		intake.deferred = map[string]int{}
	}
	if intake.deferralSelection != "" && (intake.deferralSelection != selected || intake.deferralCorrelation != correlation) {
		return errors.New("deferral replay changed exact selection or correlation")
	}
	intake.deferralSelection, intake.deferralCorrelation = selected, correlation
	for _, ref := range refs {
		if ref == selected {
			return errors.New("selected dispatch cannot be deferred")
		}
		if intake.deferred[ref] == 0 {
			intake.deferred[ref] = 1
		}
	}
	if intake.failDeferralAfterSideEffect {
		intake.failDeferralAfterSideEffect = false
		return errors.New("deferral response lost after exact durable bead updates")
	}
	return nil
}

func (intake *fakeIntake) CompleteSpecialistPullRequest(_ string, completion visualcontroller.SpecialistPullRequestCompletion) error {
	intake.completes++
	if intake.failCompletion {
		intake.failCompletion = false
		return errors.New("completion checkpoint interrupted")
	}
	intake.completion = completion
	return nil
}

type fakeRepairer struct {
	runs                  int
	sideEffects           int
	failAfterSideEffect   bool
	lastSourceExternalRef string
	outcome               RepairOutcome
}

func (repairer *fakeRepairer) Run(_ context.Context, envelope visualcontroller.DispatchEnvelope) (RepairOutcome, error) {
	repairer.runs++
	repairer.lastSourceExternalRef = envelope.SourceExternalRef
	if repairer.sideEffects == 0 {
		repairer.sideEffects++
		if repairer.failAfterSideEffect {
			repairer.failAfterSideEffect = false
			return RepairOutcome{}, errors.New("Worker response lost after one durable proposal and PR")
		}
	}
	return repairer.outcome, nil
}

type pauseReplayRepairer struct {
	mu            sync.Mutex
	outcome       RepairOutcome
	runs          int
	sideEffects   int
	firstStarted  chan struct{}
	firstCanceled chan struct{}
	secondStarted chan struct{}
}

func (repairer *pauseReplayRepairer) Run(ctx context.Context, _ visualcontroller.DispatchEnvelope) (RepairOutcome, error) {
	repairer.mu.Lock()
	repairer.runs++
	run := repairer.runs
	if repairer.sideEffects == 0 {
		repairer.sideEffects++
	}
	repairer.mu.Unlock()
	if run == 1 {
		close(repairer.firstStarted)
		<-ctx.Done()
		close(repairer.firstCanceled)
		return RepairOutcome{}, ctx.Err()
	}
	if run == 2 {
		close(repairer.secondStarted)
	}
	return repairer.outcome, nil
}

func (repairer *pauseReplayRepairer) counts() (int, int) {
	repairer.mu.Lock()
	defer repairer.mu.Unlock()
	return repairer.runs, repairer.sideEffects
}

type fakeVerdictVerifier struct {
	calls             int
	stateCalls        int
	pullRequestOpen   bool
	pullRequestMerged bool
	stateErr          error
	stateObservation  *PullRequestStateObservation
	receipt           PullRequestVerdictReceipt
}

func (verifier *fakeVerdictVerifier) VerifyPullRequest(_ context.Context, request PullRequestVerdictRequest) (PullRequestVerdictReceipt, error) {
	verifier.calls++
	if request.PullRequestNumber <= 0 || request.PullRequestURL == "" || request.HeadSHA != verifier.receipt.HeadSHA || request.IdempotencyKey == "" ||
		request.RepositoryFingerprint == "" || request.BaseSHA != strings.Repeat("9", 40) {
		return PullRequestVerdictReceipt{}, errors.New("verifier received an inexact PR request")
	}
	return verifier.receipt, nil
}

func (verifier *fakeVerdictVerifier) ObservePullRequestState(_ context.Context, request PullRequestStateRequest) (PullRequestStateObservation, error) {
	verifier.stateCalls++
	if request.PullRequestNumber <= 0 || request.PullRequestURL == "" || request.VerdictReceiptSHA256 != verifier.receipt.ReceiptSHA256 || request.HeadSHA != verifier.receipt.HeadSHA || request.IdempotencyKey == "" ||
		request.RepositoryFingerprint == "" || request.BaseBranch != "main" {
		return PullRequestStateObservation{}, errors.New("state observer received an inexact PR request")
	}
	if verifier.stateErr != nil {
		return PullRequestStateObservation{}, verifier.stateErr
	}
	if verifier.stateObservation != nil {
		return *verifier.stateObservation, nil
	}
	if verifier.pullRequestOpen {
		return PullRequestStateObservation{State: "open", Open: true}, nil
	}
	return PullRequestStateObservation{State: "closed", Merged: verifier.pullRequestMerged}, nil
}
