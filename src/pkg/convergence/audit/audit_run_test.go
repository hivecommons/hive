package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/findingidentity"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/outputschema"
)

type stores struct {
	store   *beads.Store
	ledger  *mutation.Ledger
	journal *mutation.Journal
	proofs  *proof.Store
}

func newStores(t *testing.T) stores {
	t.Helper()
	dir := t.TempDir()
	store, err := beads.NewStore(filepath.Join(dir, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "ledger.json"), 1)
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
	return stores{store: store, ledger: ledger, journal: journal, proofs: proofs}
}

func writeScope(t *testing.T, scope Scope) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "components.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// twoComponentScope has one component with two findings and a second
// component whose only finding duplicates the first component's first
// finding by title+files (different file order, different whitespace).
func twoComponentScope() Scope {
	return Scope{Components: []Component{
		{Name: "auth", Files: []string{"src/auth.go"}, Content: "auth body", Findings: []Finding{
			{Title: "Nil token panics", Files: []string{"src/b.go", "src/a.go"}, Labels: []string{"security", "ci"}},
			{Title: "Second finding", Files: []string{"src/auth.go"}},
		}},
		{Name: "gateway", Files: []string{"src/gw.go"}, Content: "gateway body", Findings: []Finding{
			{Title: "  nil   TOKEN panics ", Files: []string{"src/a.go", "src/b.go"}},
		}},
	}}
}

func (s stores) options(t *testing.T, scopeDir string, now time.Time) Options {
	t.Helper()
	return Options{
		CampaignKey: "camp", ScopeDir: scopeDir, Store: s.store, Ledger: s.ledger, Journal: s.journal,
		ProofStore: s.proofs, Mode: config.ConvergenceModeShadow, Generation: 3, Holder: "tester",
		Now: func() time.Time { return now },
	}
}

func beadsOfKind(store *beads.Store, campaign, kind string) []*beads.Bead {
	var out []*beads.Bead
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(metaKind) == kind && b.Meta(metaCampaign) == campaign {
			out = append(out, b)
		}
	}
	return out
}

func findBead(t *testing.T, store *beads.Store, campaign, kind, component string) *beads.Bead {
	t.Helper()
	for _, b := range beadsOfKind(store, campaign, kind) {
		if b.Meta(metaComponent) == component {
			return b
		}
	}
	t.Fatalf("no %s bead for %s/%s", kind, campaign, component)
	return nil
}

type fakeSoak struct {
	mode    string
	gen     uint64
	records int
}

func (f *fakeSoak) RecordAuditSoak(mode string, generation uint64) {
	f.mode, f.gen, f.records = mode, generation, f.records+1
}

type fakePublisher struct {
	campaign publish.Campaign
	findings []publish.Finding
	grant    publish.Grant
	ledger   *outcome.Ledger
	calls    int
	states   func(publish.Finding) publish.Publication
	err      error
}

func (p *fakePublisher) PublishCampaign(_ context.Context, c publish.Campaign, findings []publish.Finding, grant publish.Grant, ledger *outcome.Ledger) (publish.CampaignResult, error) {
	p.calls++
	p.campaign, p.findings, p.grant, p.ledger = c, findings, grant, ledger
	res := publish.CampaignResult{Publications: map[string]publish.Publication{}}
	for _, f := range findings {
		if p.states != nil {
			res.Publications[f.ContentHash] = p.states(f)
		}
	}
	return res, p.err
}

func TestRunRefusesGitHubToken(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "secret")
	s := newStores(t)
	_, err := Run(s.options(t, writeScope(t, twoComponentScope()), time.Now()))
	if err == nil || !strings.Contains(err.Error(), "HIVE_GITHUB_TOKEN") {
		t.Fatalf("Run with token = %v, want HIVE_GITHUB_TOKEN refusal", err)
	}
}

func TestRunRequiresStores(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	for name, opts := range map[string]Options{
		"store":   {Ledger: s.ledger, Journal: s.journal},
		"ledger":  {Store: s.store, Journal: s.journal},
		"journal": {Store: s.store, Ledger: s.ledger},
	} {
		if _, err := Run(opts); err == nil {
			t.Fatalf("Run without %s returned nil error", name)
		}
	}
}

func TestRunPropagatesScopeErrors(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	opts := s.options(t, t.TempDir(), time.Now())
	if _, err := Run(opts); err == nil {
		t.Fatal("Run with missing components.json returned nil error")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "components.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.ScopeDir = dir
	if _, err := Run(opts); err == nil {
		t.Fatal("Run with malformed components.json returned nil error")
	}
}

func TestRunAppliesDefaultsWhenOptionsAreZero(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	res, err := Run(Options{ScopeDir: writeScope(t, twoComponentScope()), Store: s.store, Ledger: s.ledger, Journal: s.journal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ConvergenceModeShadow || res.Generation != 1 {
		t.Fatalf("defaults = mode %q gen %d, want shadow/1", res.Mode, res.Generation)
	}
	if got := beadsOfKind(s.store, "audit-campaign", "campaign"); len(got) != 1 {
		t.Fatalf("campaign beads under default key = %d, want 1", len(got))
	}
	entry, ok := s.ledger.Get(mutation.TaskClaim(auditRepo, auditRepo+"!audit-campaign").Key())
	if !ok || entry.Holder != Actor {
		t.Fatalf("claim holder = %+v, want default holder %q", entry, Actor)
	}
	if res.Publication != nil {
		t.Fatalf("publication = %+v, want nil without publisher", res.Publication)
	}
}

func TestRunRecordsFindingsReceiptsAndBeads(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	scope := twoComponentScope()
	soak := &fakeSoak{}
	opts := s.options(t, writeScope(t, scope), now)
	opts.Soak = soak
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}

	if soak.records != 1 || soak.mode != config.ConvergenceModeShadow || soak.gen != 3 {
		t.Fatalf("soak = %+v, want one shadow/3 record", soak)
	}
	if res.Burndown.ScopeTotal != 2 || res.Burndown.KnownRemainingWork != 0 || res.Burndown.NullReason != "" {
		t.Fatalf("burndown = %+v", res.Burndown)
	}
	if res.Burndown.SatisfiedObligations == nil || *res.Burndown.SatisfiedObligations != 2 {
		t.Fatalf("satisfied = %v, want 2", res.Burndown.SatisfiedObligations)
	}

	if len(res.Findings) != 3 {
		t.Fatalf("findings = %d, want 3", len(res.Findings))
	}
	first, second, dup := res.Findings[0], res.Findings[1], res.Findings[2]
	if first.State != "validated" || second.State != "validated" || first.DuplicateOf != "" {
		t.Fatalf("validated findings = %+v / %+v", first, second)
	}
	if dup.State != "duplicate_of" || dup.DuplicateOf != first.BeadID {
		t.Fatalf("duplicate = %+v, want duplicate_of %s", dup, first.BeadID)
	}

	dupBead, err := s.store.Get(dup.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if dupBead.Title != findingTitlePrefix+"  nil   TOKEN panics " {
		t.Fatalf("duplicate title = %q", dupBead.Title)
	}
	if dupBead.Meta(metaDuplicateOf) != first.BeadID || dupBead.Meta(metaFindingState) != "duplicate_of" {
		t.Fatalf("duplicate meta = %v", dupBead.Metadata)
	}
	if got := dupBead.Meta(metaFileSet); got != "src/a.go,src/b.go" {
		t.Fatalf("duplicate file set = %q, want normalized order", got)
	}

	firstBead, err := s.store.Get(first.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	authHash := contentHash(scope.Components[0])
	wantMeta := map[string]string{
		metaKind: "finding", metaCampaign: "camp", metaComponent: "auth", metaContentHash: authHash,
		metaFindingHash:  findingHash(authHash, duplicateKey(scope.Components[0].Findings[0])),
		metaFindingState: "validated", metaFileSet: "src/a.go,src/b.go", metaLabels: "security,ci",
	}
	for k, want := range wantMeta {
		if got := firstBead.Meta(k); got != want {
			t.Fatalf("finding meta %s = %q, want %q", k, got, want)
		}
	}
	if firstBead.Meta(metaDuplicateOf) != "" || firstBead.Meta(findingidentity.MetaKey) != "" {
		t.Fatalf("validated title-keyed finding carries unexpected meta: %v", firstBead.Metadata)
	}
	secondBead, _ := s.store.Get(second.BeadID)
	if secondBead.Meta(metaLabels) != "" {
		t.Fatalf("finding without labels recorded labels %q", secondBead.Meta(metaLabels))
	}

	if len(res.Receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(res.Receipts))
	}
	for i, c := range scope.Components {
		inspection := findBead(t, s.store, "camp", "inspection", c.Name)
		if inspection.Meta("inspection_state") != "inspected" {
			t.Fatalf("%s inspection_state = %q", c.Name, inspection.Meta("inspection_state"))
		}
		if inspection.Meta(metaContentHash) != contentHash(c) {
			t.Fatalf("%s inspection content hash mismatch", c.Name)
		}
		if inspection.Meta(metaReceiptDigest) != res.Receipts[i].OutputDigest {
			t.Fatalf("%s receipt digest %q != %q", c.Name, inspection.Meta(metaReceiptDigest), res.Receipts[i].OutputDigest)
		}
		if res.Receipts[i].WorkKey != "hivecommons/hive!camp:"+c.Name || res.Receipts[i].Generation != 3 {
			t.Fatalf("receipt = %+v", res.Receipts[i])
		}
	}

	publication := findBead(t, s.store, "camp", "publication", "camp")
	if publication.Meta(metaPublication) != publicationNone {
		t.Fatalf("publication state = %q, want none", publication.Meta(metaPublication))
	}
	findBead(t, s.store, "camp", "campaign", "camp")

	proofs := s.proofs.List()
	if len(proofs) != 2 {
		t.Fatalf("proof records = %d, want one per component", len(proofs))
	}
	for _, rec := range proofs {
		if rec.Fingerprint.PredicateID != proof.PredicateInspectionRecorded || rec.Fingerprint.Producer != proof.ProducerHiveAuditLane || rec.Result != proof.ResultSuccess {
			t.Fatalf("proof record = %+v", rec)
		}
	}
}

func TestRunRecordsSemanticIdentityMetadata(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	f := Finding{Title: "Semantic", Files: []string{"pkg/a.go"}, SubjectDigest: "sha256:abc", Predicate: "review.finding", Location: "pkg/a.go:10"}
	scope := Scope{Components: []Component{{Name: "c", Files: []string{"pkg/a.go"}, Content: "x", Findings: []Finding{f}}}}
	res, err := Run(s.options(t, writeScope(t, scope), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.store.Get(res.Findings[0].BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Meta(findingidentity.MetaKey) != findingKey(f) || b.Meta(findingidentity.MetaSubjectDigest) != "sha256:abc" ||
		b.Meta(findingidentity.MetaPredicate) != "review.finding" || b.Meta(findingidentity.MetaLocation) != "pkg/a.go:10" {
		t.Fatalf("semantic identity meta = %v", b.Metadata)
	}
	if got := storedFinding(b); got.SubjectDigest != f.SubjectDigest || got.Predicate != f.Predicate || got.Location != f.Location || got.Title != f.Title {
		t.Fatalf("storedFinding = %+v, want %+v", got, f)
	}
}

func TestRunRerunReplaysAppliedComponentsWithoutDuplicates(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := writeScope(t, twoComponentScope())
	first, err := Run(s.options(t, dir, now))
	if err != nil {
		t.Fatal(err)
	}

	// The claim is still held for its TTL: a same-time rerun must refuse.
	if _, err := Run(s.options(t, dir, now)); !errors.Is(err, mutation.ErrClaimHeld) {
		t.Fatalf("same-time rerun error = %v, want ErrClaimHeld", err)
	}

	second, err := Run(s.options(t, dir, now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Findings) != 0 {
		t.Fatalf("rerun recorded %d new findings, want 0 (journal replay)", len(second.Findings))
	}
	if len(beadsOfKind(s.store, "camp", "finding")) != 3 {
		t.Fatalf("finding beads after rerun = %d, want 3", len(beadsOfKind(s.store, "camp", "finding")))
	}
	if len(second.Receipts) != 2 || second.Receipts[0].OutputDigest != first.Receipts[0].OutputDigest {
		t.Fatalf("rerun receipts = %+v", second.Receipts)
	}
	if second.Burndown.SatisfiedObligations == nil || *second.Burndown.SatisfiedObligations != 2 {
		t.Fatalf("rerun burndown = %+v", second.Burndown)
	}
	if len(s.proofs.List()) != 2 {
		t.Fatalf("proofs after replay = %d, want 2 (no re-put on replay)", len(s.proofs.List()))
	}
	if n := len(beadsOfKind(s.store, "camp", "inspection")); n != 2 {
		t.Fatalf("inspection beads after rerun = %d, want 2 (ensureBead reuses)", n)
	}
}

func TestRunCrashAfterBeginLeavesUnknownThenReconciles(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := writeScope(t, twoComponentScope())
	opts := s.options(t, dir, now)
	opts.CrashAfterBeginComponent = "auth"
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Burndown.SatisfiedObligations != nil || res.Burndown.NullReason != "unknown inspection evidence" {
		t.Fatalf("burndown after crash = %+v", res.Burndown)
	}
	if !reflect.DeepEqual(res.Burndown.UnknownEvidence, []string{"auth"}) || res.Burndown.KnownRemainingWork != 1 {
		t.Fatalf("burndown after crash = %+v", res.Burndown)
	}
	if got := findBead(t, s.store, "camp", "inspection", "auth").Meta("inspection_state"); got != string(convergence.ConditionUnknown) {
		t.Fatalf("auth inspection_state = %q, want Unknown", got)
	}
	if len(res.Receipts) != 1 || res.Receipts[0].WorkKey != "hivecommons/hive!camp:gateway" {
		t.Fatalf("receipts after crash = %+v", res.Receipts)
	}
	// Only gateway's finding was recorded; nothing existed to dedupe against
	// so it is validated in its own right.
	if len(res.Findings) != 1 || res.Findings[0].State != "validated" {
		t.Fatalf("findings after crash = %+v", res.Findings)
	}

	// Crashing again on the same component must tolerate the journal's
	// needs-reconciliation answer and keep the component Unknown.
	opts.Now = func() time.Time { return now.Add(2 * time.Hour) }
	res, err = Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Burndown.UnknownEvidence, []string{"auth"}) {
		t.Fatalf("second crash burndown = %+v", res.Burndown)
	}

	// Rerun without the crash: the Unknown op has no recorded findings, so
	// it reconciles as not-applied and the effect runs for real.
	opts.CrashAfterBeginComponent = ""
	opts.Now = func() time.Time { return now.Add(4 * time.Hour) }
	res, err = Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Burndown.SatisfiedObligations == nil || *res.Burndown.SatisfiedObligations != 2 {
		t.Fatalf("reconciled burndown = %+v", res.Burndown)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("reconciled run findings = %d, want auth's 2", len(res.Findings))
	}
	// gateway's finding came first, so auth's matching finding is now the duplicate.
	states := map[string]string{}
	for _, f := range res.Findings {
		states[f.Title] = f.State
	}
	if states["Nil token panics"] != "duplicate_of" || states["Second finding"] != "validated" {
		t.Fatalf("reconciled states = %v", states)
	}
	if got := findBead(t, s.store, "camp", "inspection", "auth").Meta("inspection_state"); got != "inspected" {
		t.Fatalf("auth inspection_state after reconcile = %q", got)
	}
}

func TestRunUncertainEffectWithRecordedFindingsReconcilesAsApplied(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := writeScope(t, twoComponentScope())
	opts := s.options(t, dir, now)
	boom := errors.New("after-record crash")
	opts.AfterRecordFindings = func(component string) error {
		if component == "auth" {
			return boom
		}
		return nil
	}
	if _, err := Run(opts); !errors.Is(err, boom) {
		t.Fatalf("Run with failing AfterRecordFindings = %v, want %v", err, boom)
	}
	if n := len(beadsOfKind(s.store, "camp", "finding")); n != 2 {
		t.Fatalf("findings recorded before crash = %d, want 2", n)
	}

	opts.AfterRecordFindings = nil
	opts.Now = func() time.Time { return now.Add(2 * time.Hour) }
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	// auth replays as applied (its findings exist with the same content
	// hash); only gateway runs its effect, and its finding dedupes against
	// auth's already-recorded one.
	if len(res.Findings) != 1 || res.Findings[0].State != "duplicate_of" {
		t.Fatalf("reconciled findings = %+v", res.Findings)
	}
	if n := len(beadsOfKind(s.store, "camp", "finding")); n != 3 {
		t.Fatalf("finding beads after reconcile = %d, want 3 (no duplicate re-record)", n)
	}
	if len(res.Receipts) != 2 || res.Burndown.SatisfiedObligations == nil || *res.Burndown.SatisfiedObligations != 2 {
		t.Fatalf("reconciled result = receipts %d burndown %+v", len(res.Receipts), res.Burndown)
	}
	if got := findBead(t, s.store, "camp", "inspection", "auth").Meta(metaReceiptDigest); got != res.Receipts[0].OutputDigest {
		t.Fatalf("auth receipt digest after reconcile = %q", got)
	}
}

func TestRunModeOffBypassesJournal(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	opts := s.options(t, writeScope(t, twoComponentScope()), time.Now())
	opts.Mode = config.ConvergenceModeOff
	opts.ProofStore = nil
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != config.ConvergenceModeOff || len(res.Findings) != 3 || len(res.Receipts) != 2 {
		t.Fatalf("mode-off result = %+v", res)
	}
	effect := mutation.Effect{
		OutcomeKey: auditRepo + "@camp", DesiredGeneration: 3, Transition: "audit-inspection",
		Subject: auditRepo + "!camp:auth", ClaimKey: mutation.TaskClaim(auditRepo, auditRepo+"!camp").Key(),
		Kind: EffectRecordFinding, Inputs: map[string]string{"campaign": "camp", "component": "auth", "content_hash": contentHash(twoComponentScope().Components[0])},
	}
	if _, ok := s.journal.Get(effect.LogicalID()); ok {
		t.Fatal("mode off must not journal the inspection effect")
	}
	if len(s.proofs.List()) != 0 {
		t.Fatalf("nil ProofStore still produced %d proofs", len(s.proofs.List()))
	}
}

func TestRunHandsFindingsToPublisherAndRecordsVerdicts(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	pub := &fakePublisher{states: func(f publish.Finding) publish.Publication {
		switch f.State {
		case "validated":
			if len(f.Labels) > 0 {
				return publish.Publication{State: publish.StatePublished, Ref: publishRef(42)}
			}
			return publish.Publication{State: publish.StateWithheld, Reason: publish.ReasonMode}
		}
		return publish.Publication{State: publish.StateRefused, Reason: publish.ReasonInvalid}
	}}
	opts := s.options(t, writeScope(t, twoComponentScope()), now)
	opts.Publisher = pub
	opts.RunKey, opts.RunURL = "run-1", "https://example.test/run-1"
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if pub.calls != 1 || res.Publication == nil {
		t.Fatalf("publisher calls = %d, publication = %v", pub.calls, res.Publication)
	}
	if pub.campaign != (publish.Campaign{Key: "camp", Repo: auditRepo, RunKey: "run-1", RunURL: "https://example.test/run-1"}) {
		t.Fatalf("campaign = %+v", pub.campaign)
	}
	claimKey := mutation.TaskClaim(auditRepo, auditRepo+"!camp").Key()
	if pub.grant.ClaimKey != claimKey || pub.grant.Holder != "tester" || pub.grant.Epoch == 0 {
		t.Fatalf("grant = %+v", pub.grant)
	}
	if pub.ledger != nil {
		t.Fatalf("outcome ledger = %v, want nil passthrough", pub.ledger)
	}
	if len(pub.findings) != 3 {
		t.Fatalf("publisher received %d findings, want 3", len(pub.findings))
	}
	for i := 1; i < len(pub.findings); i++ {
		if pub.findings[i-1].ContentHash > pub.findings[i].ContentHash {
			t.Fatalf("findings not sorted by content hash: %+v", pub.findings)
		}
	}
	for _, f := range pub.findings {
		inspection := findBead(t, s.store, "camp", "inspection", componentOf(t, s.store, f.BeadID))
		if f.ReceiptDigest == "" || f.ReceiptDigest != inspection.Meta(metaReceiptDigest) {
			t.Fatalf("finding %s receipt digest %q not bound to inspection %q", f.Title, f.ReceiptDigest, inspection.Meta(metaReceiptDigest))
		}
		if f.Repo != auditRepo || f.Campaign != "camp" || f.Predicate != proof.PredicateInspectionRecorded || !strings.Contains(f.Evidence, "`"+inspection.Meta(metaComponent)+"`") {
			t.Fatalf("projected finding = %+v", f)
		}
		b, _ := s.store.Get(f.BeadID)
		if f.ContentHash != b.Meta(metaFindingHash) {
			t.Fatalf("finding %s content hash %q != bead finding hash %q", f.Title, f.ContentHash, b.Meta(metaFindingHash))
		}
		if f.DuplicateOf != b.Meta(metaDuplicateOf) || f.State != b.Meta(metaFindingState) {
			t.Fatalf("projected state = %+v, bead = %v", f, b.Metadata)
		}
	}

	records := map[string]int{}
	for _, b := range beadsOfKind(s.store, "camp", "finding") {
		records[b.Meta(metaPublication)]++
	}
	if !reflect.DeepEqual(records, map[string]int{"published:42": 1, "withheld:mode": 1, "refused:invalid": 1}) {
		t.Fatalf("finding publication records = %v", records)
	}
	if got := findBead(t, s.store, "camp", "publication", "camp").Meta(metaPublication); got != "published=1 refused=1 withheld=1" {
		t.Fatalf("publication summary = %q", got)
	}
}

func TestRunReturnsPublisherErrorWithPartialResult(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := newStores(t)
	boom := errors.New("publisher down")
	pub := &fakePublisher{err: boom}
	soak := &fakeSoak{}
	opts := s.options(t, writeScope(t, twoComponentScope()), time.Now())
	opts.Publisher, opts.Soak = pub, soak
	res, err := Run(opts)
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want %v", err, boom)
	}
	if res.Publication == nil || len(res.Findings) != 3 || len(res.Receipts) != 2 {
		t.Fatalf("partial result = %+v", res)
	}
	if soak.records != 0 {
		t.Fatal("soak recorded despite publisher failure")
	}
	// No verdicts were returned, so every finding stays unrecorded and the
	// campaign summary stays at none.
	for _, b := range beadsOfKind(s.store, "camp", "finding") {
		if b.Meta(metaPublication) != "" {
			t.Fatalf("finding %s got publication %q from a failed publisher", b.ID, b.Meta(metaPublication))
		}
	}
	if got := findBead(t, s.store, "camp", "publication", "camp").Meta(metaPublication); got != publicationNone {
		t.Fatalf("publication summary = %q, want none", got)
	}
}

func TestEnsureBeadReusesExistingAndAppliesExtraMeta(t *testing.T) {
	s := newStores(t)
	b, err := ensureBead(s.store, "inspection", "camp", "auth", map[string]string{metaContentHash: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "inspection: auth" || b.Type != beads.TypeTask || b.ExternalRef != "camp:auth" || b.Actor != Actor {
		t.Fatalf("bead = %+v", b)
	}
	stored, _ := s.store.Get(b.ID)
	if stored.Meta(metaKind) != "inspection" || stored.Meta(metaCampaign) != "camp" || stored.Meta(metaComponent) != "auth" || stored.Meta(metaContentHash) != "h1" {
		t.Fatalf("meta = %v", stored.Metadata)
	}
	again, err := ensureBead(s.store, "inspection", "camp", "auth", map[string]string{metaContentHash: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != b.ID {
		t.Fatalf("ensureBead created %s, want reuse of %s", again.ID, b.ID)
	}
	if stored, _ = s.store.Get(b.ID); stored.Meta(metaContentHash) != "h1" {
		t.Fatalf("reuse overwrote content hash to %q", stored.Meta(metaContentHash))
	}
	other, err := ensureBead(s.store, "campaign", "camp", "auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == b.ID {
		t.Fatal("different kind must create a different bead")
	}
}

func TestCampaignFindingsFallsBackToDerivedHash(t *testing.T) {
	s := newStores(t)
	b, err := s.store.Create(findingTitlePrefix+"Legacy", beads.TypeAdvisory, beads.PriorityMedium, Actor, "camp:c")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{metaKind: "finding", metaCampaign: "camp", metaComponent: "c", metaContentHash: "chash", metaFindingState: "validated", metaFileSet: "b.go,a.go", metaLabels: "x,y"} {
		if err := s.store.SetMetadata(b.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}
	otherCampaign, _ := s.store.Create(findingTitlePrefix+"Other", beads.TypeAdvisory, beads.PriorityMedium, Actor, "other:c")
	_ = s.store.SetMetadata(otherCampaign.ID, metaKind, "finding")
	_ = s.store.SetMetadata(otherCampaign.ID, metaCampaign, "other")

	got := campaignFindings(s.store, "camp")
	if len(got) != 1 {
		t.Fatalf("campaignFindings = %+v, want exactly the camp finding", got)
	}
	want := findingHash("chash", duplicateKey(Finding{Title: "Legacy", Files: []string{"b.go", "a.go"}}))
	if got[0].ContentHash != want {
		t.Fatalf("derived hash = %q, want %q", got[0].ContentHash, want)
	}
	if !reflect.DeepEqual(got[0].Files, []string{"b.go", "a.go"}) || !reflect.DeepEqual(got[0].Labels, []string{"x", "y"}) {
		t.Fatalf("projected files/labels = %v / %v", got[0].Files, got[0].Labels)
	}
	if got[0].ReceiptDigest != "" {
		t.Fatalf("receipt digest without inspection bead = %q, want empty", got[0].ReceiptDigest)
	}
}

func TestRecordPublicationsWithNoVerdictsWritesNone(t *testing.T) {
	s := newStores(t)
	pubBead, err := s.store.Create("publication: camp", beads.TypeTask, beads.PriorityMedium, Actor, "camp:camp")
	if err != nil {
		t.Fatal(err)
	}
	finding, _ := s.store.Create(findingTitlePrefix+"F", beads.TypeAdvisory, beads.PriorityMedium, Actor, "camp:c")
	_ = s.store.SetMetadata(finding.ID, metaKind, "finding")
	_ = s.store.SetMetadata(finding.ID, metaCampaign, "camp")
	_ = s.store.SetMetadata(finding.ID, metaFindingHash, "fh")

	recordPublications(s.store, "camp", pubBead.ID, publish.CampaignResult{Publications: map[string]publish.Publication{"unrelated": {State: publish.StatePublished}}})
	got, _ := s.store.Get(pubBead.ID)
	if got.Meta(metaPublication) != publicationNone {
		t.Fatalf("summary = %q, want none", got.Meta(metaPublication))
	}
	f, _ := s.store.Get(finding.ID)
	if f.Meta(metaPublication) != "" {
		t.Fatalf("unmatched finding got publication %q", f.Meta(metaPublication))
	}

	recordPublications(s.store, "camp", pubBead.ID, publish.CampaignResult{Publications: map[string]publish.Publication{"fh": {State: publish.StateExisting, Ref: publishRef(7)}}})
	f, _ = s.store.Get(finding.ID)
	got, _ = s.store.Get(pubBead.ID)
	if f.Meta(metaPublication) != "existing:7" || got.Meta(metaPublication) != "existing=1" {
		t.Fatalf("finding = %q summary = %q", f.Meta(metaPublication), got.Meta(metaPublication))
	}
}

func TestExistingFindingKeysOnlyCountsValidated(t *testing.T) {
	s := newStores(t)
	mk := func(title, state string) *beads.Bead {
		b, err := s.store.Create(findingTitlePrefix+title, beads.TypeAdvisory, beads.PriorityMedium, Actor, "camp:c")
		if err != nil {
			t.Fatal(err)
		}
		_ = s.store.SetMetadata(b.ID, metaKind, "finding")
		_ = s.store.SetMetadata(b.ID, metaCampaign, "camp")
		_ = s.store.SetMetadata(b.ID, metaFindingState, state)
		_ = s.store.SetMetadata(b.ID, metaFileSet, "a.go")
		return b
	}
	v := mk("Valid", "validated")
	mk("Dup", "duplicate_of")
	keys := existingFindingKeys(s.store, "camp")
	if len(keys) != 1 || keys[duplicateKey(Finding{Title: "Valid", Files: []string{"a.go"}})] != v.ID {
		t.Fatalf("existingFindingKeys = %v", keys)
	}
	if componentEffectApplied(s.store, "camp", "c", "h") {
		t.Fatal("componentEffectApplied true without matching content hash")
	}
	_ = s.store.SetMetadata(v.ID, metaComponent, "c")
	_ = s.store.SetMetadata(v.ID, metaContentHash, "h")
	if !componentEffectApplied(s.store, "camp", "c", "h") {
		t.Fatal("componentEffectApplied false with matching finding")
	}
}

func TestSplitListAndNormalizeHelpers(t *testing.T) {
	if got := splitList(""); got != nil {
		t.Fatalf("splitList(empty) = %#v, want nil", got)
	}
	if got := splitList("  "); got != nil {
		t.Fatalf("splitList(blank) = %#v, want nil", got)
	}
	if got := splitList("a,b"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitList = %#v", got)
	}
	in := []string{"z.go", "a.go"}
	out := normalizeFiles(in)
	if !reflect.DeepEqual(out, []string{"a.go", "z.go"}) || in[0] != "z.go" {
		t.Fatalf("normalizeFiles = %v (input now %v), want sorted copy", out, in)
	}
	if got := normalizeTitle("  Mixed   CASE\ttitle "); got != "mixed case title" {
		t.Fatalf("normalizeTitle = %q", got)
	}
	if findingKey(Finding{SubjectDigest: "sha256:abc"}) != "" {
		t.Fatal("findingKey without predicate/location must be empty")
	}
}

func TestContentHashAndStableHashAreOrderAndBoundarySensitive(t *testing.T) {
	a := Component{Name: "n", Files: []string{"a", "b"}, Content: "c"}
	if contentHash(a) != contentHash(a) {
		t.Fatal("contentHash is not deterministic")
	}
	if contentHash(a) == contentHash(Component{Name: "n", Files: []string{"b", "a"}, Content: "c"}) {
		t.Fatal("contentHash ignores file order")
	}
	if contentHash(a) == contentHash(Component{Name: "n", Files: []string{"a", "b"}, Content: "d"}) {
		t.Fatal("contentHash ignores content")
	}
	if stableHash("ab", "c") == stableHash("a", "bc") {
		t.Fatal("stableHash does not separate parts")
	}
	if len(stableHash()) != 64 {
		t.Fatalf("stableHash() = %q, want 64 hex chars", stableHash())
	}
	if findingHash("h", "k") != stableHash("h", "k") {
		t.Fatal("findingHash must be stableHash(componentHash, key)")
	}
}

func TestStageReceiptIsDeterministicAndValidates(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 123, time.UTC)
	c := Component{Name: "auth", Files: []string{"a.go"}, Content: "body"}
	hash := contentHash(c)
	r1 := stageReceipt("camp", c, 4, hash, now)
	r2 := stageReceipt("camp", c, 4, hash, now)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("stageReceipt not deterministic:\n%+v\n%+v", r1, r2)
	}
	if r1.SchemaVersion != outputschema.StageReceiptSchemaVersion || r1.Stage != "audit-inspection" || r1.ResultClass != outputschema.ReceiptResultCompleted {
		t.Fatalf("receipt = %+v", r1)
	}
	if r1.WorkKey != "hivecommons/hive!camp:auth" || r1.AssignmentID != "camp:auth" || r1.Generation != 4 || r1.InputRevision != "artifact@"+hash {
		t.Fatalf("receipt identity = %+v", r1)
	}
	if r1.ExecutionKey != mutation.DeriveLogicalID([]string{"camp", "auth", hash}, nil) {
		t.Fatalf("execution key = %q", r1.ExecutionKey)
	}
	if r1.StartedAt != now.Format(time.RFC3339Nano) || r1.EndedAt != r1.StartedAt {
		t.Fatalf("timestamps = %s / %s", r1.StartedAt, r1.EndedAt)
	}
	if r1.Engine == nil || r1.Engine.Name != Actor || r1.Provenance == nil || r1.Provenance.Query != "audit-scope/auth" {
		t.Fatalf("engine/provenance = %+v / %+v", r1.Engine, r1.Provenance)
	}
	if len(r1.Artifacts) != 1 || r1.Artifacts[0].Path != "audit-scope/auth" {
		t.Fatalf("artifacts = %+v", r1.Artifacts)
	}
	other := stageReceipt("camp", c, 4, stableHash("other"), now)
	if other.ExecutionKey == r1.ExecutionKey {
		t.Fatal("execution key ignores input revision")
	}
	if other.OutputDigest != r1.OutputDigest {
		t.Fatal("output digest depends only on artifacts, so it must match for the same component")
	}
	raw, err := json.Marshal(outputschema.AgentReport{Lane: "audit", Kind: outputschema.KindStageReceipt, Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{}, Summary: "audit receipt", Receipt: &r1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outputschema.Validate(raw); err != nil {
		t.Fatalf("receipt failed outputschema validation: %v\n%s", err, raw)
	}
}

func TestOptionsNowDefaultsToUTC(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600))
	if got := (Options{Now: func() time.Time { return fixed }}).now(); !got.Equal(fixed) {
		t.Fatalf("now() = %v, want injected %v", got, fixed)
	}
	got := (Options{}).now()
	if got.Location() != time.UTC || time.Since(got) > time.Minute {
		t.Fatalf("default now() = %v, want recent UTC", got)
	}
}

func publishRef(n int) forge.IssueRef { return forge.IssueRef{Number: n} }

func componentOf(t *testing.T, store *beads.Store, beadID string) string {
	t.Helper()
	b, err := store.Get(beadID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Meta(metaComponent)
}
