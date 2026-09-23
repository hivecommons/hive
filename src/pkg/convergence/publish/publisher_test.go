package publish

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/forge"
)

const (
	testRepo    = "acme/widget"
	testPrivate = "acme/security"
	testOwner   = "maintainer"
)

var testNow = time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)

type fakeIssue struct {
	number int
	body   string
}

// fakeSeam is an in-memory forge issue seam that counts creates, remembers
// bodies for marker lookups, and can drop or fail a create on demand.
type fakeSeam struct {
	mu       sync.Mutex
	issues   map[string][]fakeIssue
	creates  int
	next     int
	failNext error
	dropNext bool
	failFind error
}

func newFakeSeam() *fakeSeam { return &fakeSeam{issues: map[string][]fakeIssue{}, next: 100} }

func (s *fakeSeam) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (forge.IssueRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return forge.IssueRef{}, err
	}
	s.next++
	ref := forge.IssueRef{Number: s.next, URL: "https://forge.test/" + repo + "/issues/" + strconv.Itoa(s.next)}
	if s.dropNext {
		s.dropNext = false
		return ref, nil
	}
	s.issues[repo] = append(s.issues[repo], fakeIssue{number: ref.Number, body: body})
	return ref, nil
}

func (s *fakeSeam) FindIssueByMarker(ctx context.Context, repo, marker string) (forge.IssueRef, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFind != nil {
		return forge.IssueRef{}, false, s.failFind
	}
	for _, is := range s.issues[repo] {
		if strings.Contains(is.body, marker) {
			return forge.IssueRef{Number: is.number, URL: "https://forge.test/" + repo + "/issues/" + strconv.Itoa(is.number)}, true, nil
		}
	}
	return forge.IssueRef{}, false, nil
}

func (s *fakeSeam) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

type auditEntry struct {
	actor, action, agent string
	fields               map[string]any
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []auditEntry
}

func (a *fakeAudit) Record(actor, action, agentName string, fields map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, auditEntry{actor: actor, action: action, agent: agentName, fields: fields})
}

func (a *fakeAudit) last(t *testing.T) auditEntry {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.entries) == 0 {
		t.Fatal("no audit entries recorded")
	}
	return a.entries[len(a.entries)-1]
}

type fakeNotifier struct {
	title, message string
	sent           int
}

func (n *fakeNotifier) Send(title, message string) {
	n.title, n.message, n.sent = title, message, n.sent+1
}

type stores struct {
	ledger   *mutation.Ledger
	journal  *mutation.Journal
	proofs   *proof.Store
	outcomes *outcome.Ledger
}

func openStores(t *testing.T) stores {
	t.Helper()
	dir := t.TempDir()
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "claims.json"), mutation.DefaultMaxWritersPerRepo)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := proof.Open(filepath.Join(dir, "proofs.json"))
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := outcome.Open(filepath.Join(dir, "outcomes.json"), outcome.Options{Principals: []string{testOwner}, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return stores{ledger: ledger, journal: journal, proofs: proofs, outcomes: outcomes}
}

func enforcePolicy() Policy {
	return Policy{Enabled: true, ACMMLevel: config.PublicationMinACMMLevel, Mode: config.ConvergenceModeEnforce}
}

func newPublisher(t *testing.T, st stores, seam forge.IssueSeam, pol Policy) (*Publisher, *fakeAudit) {
	t.Helper()
	audit := &fakeAudit{}
	p := &Publisher{
		Executor:   mutation.Executor{Ledger: st.ledger, Journal: st.journal, Mode: pol.Mode, Now: func() time.Time { return testNow }},
		Issues:     seam,
		Proofs:     st.proofs,
		PolicyFunc: func() Policy { return pol },
		Actor:      testOwner,
		Audit:      audit,
		Now:        func() time.Time { return testNow },
	}
	return p, audit
}

func campaignGrant(t *testing.T, st stores) Grant {
	t.Helper()
	claim := mutation.TaskClaim(testRepo, testRepo+"!fixture")
	entry, err := st.ledger.Acquire(claim, "campaign", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return Grant{ClaimKey: claim.Key(), Epoch: entry.Epoch, Holder: "campaign"}
}

func publicFinding() Finding {
	return Finding{
		Campaign: "fixture", Repo: testRepo, BeadID: "bead-auth", Title: "auth: missing test for token refresh",
		Evidence: "The refresh path has no test.", Files: []string{"pkg/auth/refresh.go"},
		ContentHash: "hash-auth", Predicate: proof.PredicateInspectionRecorded, ReceiptDigest: "digest-auth",
		State: StateValidated, RunKey: "run-7", RunURL: "https://hive.test/runs/run-7",
	}
}

func sensitiveFinding() Finding {
	f := publicFinding()
	f.BeadID, f.Title, f.ContentHash, f.ReceiptDigest = "bead-sec", "billing: unauthenticated refund endpoint", "hash-sec", "digest-sec"
	f.Labels = []string{"security"}
	return f
}

func TestPublishDisabledWithholdsWithoutWrites(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	pol := enforcePolicy()
	pol.Enabled = false
	p, audit := newPublisher(t, st, seam, pol)
	pub, err := p.Publish(context.Background(), publicFinding(), campaignGrant(t, st))
	if err != nil || pub.State != StateWithheld || pub.Reason != ReasonDisabled || pub.Record() != "withheld:disabled" {
		t.Fatalf("disabled publish = %+v err=%v", pub, err)
	}
	if seam.count() != 0 {
		t.Fatalf("disabled publisher performed %d writes", seam.count())
	}
	if e := audit.last(t); e.action != AuditFindingWithheld || e.fields["outcome"] != ReasonDisabled || e.actor != testOwner {
		t.Fatalf("audit = %+v", e)
	}
	if _, ok := st.journal.Get(p.effect(publicFinding(), Grant{ClaimKey: "k"}, mutation.EffectCreateIssue, channelPublic).LogicalID()); ok {
		t.Fatal("disabled publication must not touch the journal")
	}
}

func TestPublishShadowModeWithholdsWithoutWrites(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	pol := enforcePolicy()
	pol.Mode = config.ConvergenceModeShadow
	p, audit := newPublisher(t, st, seam, pol)
	pub, err := p.Publish(context.Background(), publicFinding(), campaignGrant(t, st))
	if err != nil || pub.Record() != "withheld:mode" {
		t.Fatalf("shadow publish = %+v err=%v", pub, err)
	}
	if seam.count() != 0 {
		t.Fatalf("shadow publisher performed %d writes", seam.count())
	}
	if e := audit.last(t); e.action != AuditFindingWithheld || e.fields["mode"] != config.ConvergenceModeShadow {
		t.Fatalf("audit = %+v", e)
	}
}

func TestPublishBelowACMMFloorRefusesAndAudits(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	pol := enforcePolicy()
	pol.ACMMLevel = config.PublicationMinACMMLevel - 1
	p, audit := newPublisher(t, st, seam, pol)
	pub, err := p.Publish(context.Background(), publicFinding(), campaignGrant(t, st))
	if !errors.Is(err, ErrLevelBelowFloor) || pub.Record() != "refused:level" {
		t.Fatalf("low-level publish = %+v err=%v", pub, err)
	}
	if seam.count() != 0 {
		t.Fatalf("refused publisher performed %d writes", seam.count())
	}
	if e := audit.last(t); e.action != AuditFindingRefused || e.fields["outcome"] != ReasonLevel {
		t.Fatalf("audit = %+v", e)
	}
}

func TestPublishReplayFilesOneIssueForTwoExecutions(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p, audit := newPublisher(t, st, seam, enforcePolicy())
	grant := campaignGrant(t, st)
	f := publicFinding()

	first, err := p.Publish(context.Background(), f, grant)
	if err != nil || first.State != StatePublished || first.Replayed || first.Ref.Number == 0 {
		t.Fatalf("first publish = %+v err=%v", first, err)
	}
	second, err := p.Publish(context.Background(), f, grant)
	if err != nil || second.State != StateExisting || second.Ref.Number != first.Ref.Number {
		t.Fatalf("second publish = %+v err=%v (marker dedupe must find the first issue)", second, err)
	}
	if seam.count() != 1 {
		t.Fatalf("two executions created %d issues, want 1", seam.count())
	}

	// With the marker lookup blind, the journal alone must still replay the
	// recorded result rather than filing again.
	seam.failFind = errors.New("search unavailable")
	third, err := p.Publish(context.Background(), f, grant)
	if err != nil || third.State != StatePublished || !third.Replayed || third.Ref.Number != first.Ref.Number {
		t.Fatalf("journal replay = %+v err=%v", third, err)
	}
	if seam.count() != 1 {
		t.Fatalf("journal replay created another issue: %d", seam.count())
	}
	if e := audit.last(t); e.action != AuditFindingPublished || e.fields["replayed"] != true {
		t.Fatalf("replay audit = %+v", e)
	}
	body := seam.issues[testRepo][0].body
	for _, want := range []string{f.Marker(), RunTrailer + ": run-7", f.RunURL, "pkg/auth/refresh.go", "digest-auth"} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue body lacks %q:\n%s", want, body)
		}
	}
	recs := st.proofs.List()
	if len(recs) != 1 || recs[0].Fingerprint.PredicateID != proof.PredicateFindingPublished ||
		recs[0].Fingerprint.IssueNumber != first.Ref.Number || recs[0].Fingerprint.FindingHash != "hash-auth" {
		t.Fatalf("publication proof = %+v", recs)
	}
	if first.Record() != "published:"+strconv.Itoa(first.Ref.Number) {
		t.Fatalf("record = %q", first.Record())
	}
}

func TestPublishSensitiveGoesToPrivateRepoNeverPublic(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	private := newFakeSeam()
	p, audit := newPublisher(t, st, public, enforcePolicy())
	p.Private = RepoChannel{Repo: testPrivate, Issues: private}
	grant := campaignGrant(t, st)
	f := sensitiveFinding()

	pub, err := p.Publish(context.Background(), f, grant)
	if err != nil || pub.State != StatePrivate || pub.Private.Number == 0 {
		t.Fatalf("sensitive publish = %+v err=%v", pub, err)
	}
	if public.count() != 0 {
		t.Fatalf("sensitive finding hit the public issue seam %d times", public.count())
	}
	if private.count() != 1 || !strings.HasPrefix(pub.Record(), "private:repo:"+testPrivate+"#") {
		t.Fatalf("private channel creates=%d record=%q", private.count(), pub.Record())
	}
	if e := audit.last(t); e.action != AuditFindingDisclosed {
		t.Fatalf("audit = %+v", e)
	}
	// Replay through the private finder: one disclosure for two executions.
	again, err := p.Publish(context.Background(), f, grant)
	if err != nil || again.State != StatePrivate || again.Private.Number != pub.Private.Number {
		t.Fatalf("replayed disclosure = %+v err=%v", again, err)
	}
	if private.count() != 1 || public.count() != 0 {
		t.Fatalf("replay disclosed again: private=%d public=%d", private.count(), public.count())
	}
	if recs := st.proofs.List(); len(recs) != 1 || recs[0].Fingerprint.FindingHash != "hash-sec" {
		t.Fatalf("private proof = %+v", recs)
	}
}

func TestPublishSensitiveWithoutChannelRefusesAndAudits(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	p, audit := newPublisher(t, st, public, enforcePolicy())
	pub, err := p.Publish(context.Background(), sensitiveFinding(), campaignGrant(t, st))
	if !errors.Is(err, ErrNoPrivateChannel) || pub.Record() != "refused:no-private-channel" {
		t.Fatalf("no-channel publish = %+v err=%v", pub, err)
	}
	if public.count() != 0 {
		t.Fatalf("refused sensitive finding hit the public seam %d times", public.count())
	}
	if e := audit.last(t); e.action != AuditFindingRefused || e.fields["outcome"] != ReasonNoPrivateChannel {
		t.Fatalf("audit = %+v", e)
	}
}

func TestPublishNotifyChannelSendsTitleNotEvidence(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	n := &fakeNotifier{}
	p, _ := newPublisher(t, st, public, enforcePolicy())
	p.Private = NotifyChannel{Notifier: n}
	grant := campaignGrant(t, st)
	f := sensitiveFinding()
	pub, err := p.Publish(context.Background(), f, grant)
	if err != nil || pub.State != StatePrivate || pub.Record() != "private:notify:hash-sec" {
		t.Fatalf("notify publish = %+v err=%v", pub, err)
	}
	if n.sent != 1 || !strings.Contains(n.message, f.Title) || !strings.Contains(n.message, "hash-sec") || strings.Contains(n.message, f.Evidence) {
		t.Fatalf("notification = %q / %q", n.title, n.message)
	}
	if public.count() != 0 || len(st.proofs.List()) != 0 {
		t.Fatalf("notify channel must neither file publicly nor record an issue proof: creates=%d proofs=%d", public.count(), len(st.proofs.List()))
	}
	// A notify channel has no finder: a crash window stays Unknown and the
	// next attempt refuses rather than notifying twice.
	g := sensitiveFinding()
	g.ContentHash, g.BeadID = "hash-sec-2", "bead-sec-2"
	p.AfterEffect = func(Finding) error { return errors.New("crash before ack") }
	if _, err := p.Publish(context.Background(), g, grant); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatalf("crash must surface uncertainty, got %v", err)
	}
	p.AfterEffect = nil
	if _, err := p.Publish(context.Background(), g, grant); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatalf("unreconcilable disclosure must stay refused, got %v", err)
	}
	if n.sent != 2 {
		t.Fatalf("notifier sent %d, want 2 (one per finding, never a retry)", n.sent)
	}
}

func TestPublishCrashWindowReconcilesByMarkerWithoutDuplicate(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p, _ := newPublisher(t, st, seam, enforcePolicy())
	grant := campaignGrant(t, st)
	f := publicFinding()
	p.AfterEffect = func(Finding) error { return errors.New("process died before RecordResult") }
	pub, err := p.Publish(context.Background(), f, grant)
	if !errors.Is(err, ErrPublicationUncertain) || pub.Reason != ReasonUncertain {
		t.Fatalf("crash publish = %+v err=%v", pub, err)
	}
	op, ok := st.journal.Get(p.effect(f, grant, mutation.EffectCreateIssue, channelPublic).LogicalID())
	if !ok || op.Status != mutation.StatusUnknown {
		t.Fatalf("crash window must leave the operation Unknown, got %+v", op)
	}
	p.AfterEffect = nil
	// The marker lookup is what reconciles; make the pre-publication dedupe
	// blind for the first page so the journal path is exercised too.
	pub, err = p.Publish(context.Background(), f, grant)
	if err != nil || (pub.State != StatePublished && pub.State != StateExisting) {
		t.Fatalf("reconciled publish = %+v err=%v", pub, err)
	}
	if seam.count() != 1 {
		t.Fatalf("reconciliation filed again: %d creates", seam.count())
	}
	op, _ = st.journal.Get(op.LogicalID)
	if op.Status != mutation.StatusApplied || !strings.Contains(op.Result, resultIssueKey) {
		t.Fatalf("journal after reconcile = %+v", op)
	}
}

func TestPublishCrashWindowMissAuthorizesOneRetry(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p, _ := newPublisher(t, st, seam, enforcePolicy())
	grant := campaignGrant(t, st)
	f := publicFinding()
	seam.dropNext = true // the create "succeeded" but nothing is observable
	p.AfterEffect = func(Finding) error { return errors.New("crash") }
	if _, err := p.Publish(context.Background(), f, grant); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatalf("expected uncertainty, got %v", err)
	}
	p.AfterEffect = nil
	pub, err := p.Publish(context.Background(), f, grant)
	if err != nil || pub.State != StatePublished || pub.Replayed {
		t.Fatalf("retry after authoritative miss = %+v err=%v", pub, err)
	}
	if seam.count() != 2 || len(seam.issues[testRepo]) != 1 {
		t.Fatalf("creates=%d visible=%d, want one retry and one visible issue", seam.count(), len(seam.issues[testRepo]))
	}
}

func TestPublishCrashWindowUnknownLookupStaysUncertain(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p, audit := newPublisher(t, st, seam, enforcePolicy())
	grant := campaignGrant(t, st)
	f := publicFinding()
	p.AfterEffect = func(Finding) error { return errors.New("crash") }
	if _, err := p.Publish(context.Background(), f, grant); err == nil {
		t.Fatal("expected uncertainty")
	}
	p.AfterEffect = nil
	seam.failFind = errors.New("forge unreachable")
	pub, err := p.Publish(context.Background(), f, grant)
	if !errors.Is(err, ErrPublicationUncertain) || pub.State != StateRefused {
		t.Fatalf("non-authoritative lookup must keep the operation uncertain: %+v err=%v", pub, err)
	}
	if seam.count() != 1 {
		t.Fatalf("uncertain operation was retried: %d creates", seam.count())
	}
	if e := audit.last(t); e.action != AuditFindingRefused || e.fields["outcome"] != ReasonUncertain {
		t.Fatalf("audit = %+v", e)
	}
}

func TestPublishEffectErrorIsUncertainAndAudited(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	seam.failNext = errors.New("502 from forge")
	p, audit := newPublisher(t, st, seam, enforcePolicy())
	pub, err := p.Publish(context.Background(), publicFinding(), campaignGrant(t, st))
	if !errors.Is(err, ErrPublicationUncertain) || pub.State != StateRefused {
		t.Fatalf("forge error = %+v err=%v", pub, err)
	}
	if e := audit.last(t); e.action != AuditFindingRefused {
		t.Fatalf("audit = %+v", e)
	}
}

func TestPublishRefusesInvalidFindingAndMissingSeam(t *testing.T) {
	st := openStores(t)
	p, audit := newPublisher(t, st, nil, enforcePolicy())
	grant := campaignGrant(t, st)
	bad := publicFinding()
	bad.State = StateRejected
	if pub, err := p.Publish(context.Background(), bad, grant); !errors.Is(err, ErrInvalidFinding) || pub.Record() != "refused:invalid" {
		t.Fatalf("rejected finding = %+v err=%v", pub, err)
	}
	if e := audit.last(t); e.action != AuditFindingRefused || e.fields["outcome"] != ReasonInvalid {
		t.Fatalf("audit = %+v", e)
	}
	if pub, err := p.Publish(context.Background(), publicFinding(), grant); !errors.Is(err, ErrNoIssueSeam) || pub.Record() != "refused:no-issue-seam" {
		t.Fatalf("no seam = %+v err=%v", pub, err)
	}
	p.Private = RepoChannel{Repo: testPrivate}
	sensitive := sensitiveFinding()
	if pub, err := p.Publish(context.Background(), sensitive, grant); !errors.Is(err, ErrNoPrivateChannel) || pub.Record() != "refused:no-private-channel" {
		t.Fatalf("repo channel without seam must refuse, got %+v err=%v", pub, err)
	}
	if _, ok := st.journal.Get(p.effect(sensitive, grant, mutation.EffectPrivateDisclosure, channelPrivate).LogicalID()); ok {
		t.Fatal("a refused disclosure must never enter the journal")
	}
	p.Private = NotifyChannel{}
	if _, err := p.Publish(context.Background(), sensitive, grant); !errors.Is(err, ErrNoPrivateChannel) {
		t.Fatalf("notify channel without notifier must refuse, got %v", err)
	}
	if err := (RepoChannel{Issues: newFakeSeam()}).Ready(); !errors.Is(err, ErrNoPrivateChannel) {
		t.Fatalf("repo channel without a repo must not be ready, got %v", err)
	}
	if _, err := (NotifyChannel{}).Disclose(context.Background(), Disclosure{}); !errors.Is(err, ErrNoPrivateChannel) {
		t.Fatalf("notify channel without notifier must refuse, got %v", err)
	}
	if _, _, err := (RepoChannel{Repo: testPrivate}).FindDisclosure(context.Background(), "m"); !errors.Is(err, ErrNoPrivateChannel) {
		t.Fatalf("repo channel finder without seam must refuse, got %v", err)
	}
}

func TestPublishSelfAcquiresAndReleasesClaim(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p, _ := newPublisher(t, st, seam, enforcePolicy())
	pub, err := p.Publish(context.Background(), publicFinding(), Grant{})
	if err != nil || pub.State != StatePublished {
		t.Fatalf("self-claimed publish = %+v err=%v", pub, err)
	}
	claim := mutation.TaskClaim(testRepo, testRepo+"!fixture"+claimSubjectSuffix)
	entry, ok := st.ledger.Get(claim.Key())
	if !ok || entry.State == mutation.StateActiveMutation {
		t.Fatalf("publisher claim must be released after the effect: %+v ok=%v", entry, ok)
	}
	// Without a ledger there is nothing to claim through.
	p.Executor.Ledger = nil
	other := publicFinding()
	other.ContentHash, other.BeadID = "hash-other", "bead-other"
	if _, err := p.Publish(context.Background(), other, Grant{}); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatalf("ledgerless self-claim must refuse, got %v", err)
	}
	// A held writer slot refuses too.
	p.Executor.Ledger = st.ledger
	campaignGrant(t, st)
	if _, err := p.Publish(context.Background(), other, Grant{}); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatalf("exhausted writer slot must refuse, got %v", err)
	}
}

func TestPublishDefaultsWithoutSeams(t *testing.T) {
	st := openStores(t)
	seam := newFakeSeam()
	p := &Publisher{Executor: mutation.Executor{Ledger: st.ledger, Journal: st.journal}, Issues: seam}
	if pub, err := p.Publish(context.Background(), publicFinding(), Grant{}); err != nil || pub.Record() != "withheld:disabled" {
		t.Fatalf("nil policy must withhold: %+v err=%v", pub, err)
	}
	if p.classify(sensitiveFinding()) != SensitivitySensitive || p.classify(publicFinding()) != SensitivityPublic {
		t.Fatal("nil classifier must fall back to the conservative classifier")
	}
	if p.now().IsZero() || p.logger() == nil {
		t.Fatal("defaults for clock and logger must be usable")
	}
	p.PolicyFunc = enforcePolicy
	if pub, err := p.Publish(context.Background(), publicFinding(), Grant{}); err != nil || pub.State != StatePublished {
		t.Fatalf("publish without proofs or audit = %+v err=%v", pub, err)
	}
}

func TestFindingValidateBodyAndMarker(t *testing.T) {
	f := publicFinding()
	if err := f.Validate(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	if f.Marker() != MarkerPrefix+"hash-auth"+markerSuffix {
		t.Fatalf("marker = %q", f.Marker())
	}
	body := f.Body()
	if !strings.HasSuffix(strings.TrimSpace(body), RunTrailer+": run-7") {
		t.Fatalf("body must end with the Hive-Run trailer:\n%s", body)
	}
	bare := f
	bare.RunKey, bare.RunURL, bare.Files = "", "", nil
	if b := bare.Body(); strings.Contains(b, RunTrailer) || strings.Contains(b, "Files:") {
		t.Fatalf("bare body must omit trailer and files:\n%s", b)
	}
	for name, mutate := range map[string]func(*Finding){
		"campaign":  func(f *Finding) { f.Campaign = " " },
		"repo":      func(f *Finding) { f.Repo = "widget" },
		"bead":      func(f *Finding) { f.BeadID = "" },
		"title":     func(f *Finding) { f.Title = "" },
		"hash":      func(f *Finding) { f.ContentHash = "a b" },
		"receipt":   func(f *Finding) { f.ReceiptDigest = "" },
		"duplicate": func(f *Finding) { f.State = StateDuplicateOf },
	} {
		g := publicFinding()
		mutate(&g)
		if err := g.Validate(); !errors.Is(err, ErrInvalidFinding) {
			t.Fatalf("%s: expected ErrInvalidFinding, got %v", name, err)
		}
	}
}

func TestConservativeClassifier(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Finding)
		want   Sensitivity
	}{
		"plain":            {func(*Finding) {}, SensitivityPublic},
		"security label":   {func(f *Finding) { f.Labels = []string{"Security"} }, SensitivitySensitive},
		"cve in title":     {func(f *Finding) { f.Title = "bump dep for CVE-2026-1" }, SensitivitySensitive},
		"secret predicate": {func(f *Finding) { f.Predicate = "hive.secret.leaked/v1" }, SensitivitySensitive},
	}
	for name, tc := range cases {
		f := publicFinding()
		tc.mutate(&f)
		if got := ConservativeClassifier(f); got != tc.want {
			t.Fatalf("%s: classified %s, want %s", name, got, tc.want)
		}
	}
}

func TestResultRoundTrips(t *testing.T) {
	ref := forge.IssueRef{Number: 12, URL: "https://forge.test/x/issues/12"}
	if got := parseIssueResult(issueResult(ref)); got != ref {
		t.Fatalf("issue result round trip = %+v", got)
	}
	d := DisclosureRef{Ref: "repo:acme/security#4", Number: 4}
	if got := parsePrivateResult(privateResult(d)); got != d {
		t.Fatalf("private result round trip = %+v", got)
	}
	if (Publication{}).Record() != StateNone {
		t.Fatalf("zero publication record = %q", (Publication{}).Record())
	}
}
