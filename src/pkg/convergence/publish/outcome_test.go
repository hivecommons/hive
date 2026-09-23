package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
)

func campaignFindings() []Finding {
	dup := publicFinding()
	dup.BeadID, dup.ContentHash, dup.State, dup.DuplicateOf = "bead-dup", "hash-dup", StateDuplicateOf, "bead-auth"
	rejected := publicFinding()
	rejected.BeadID, rejected.ContentHash, rejected.State = "bead-rej", "hash-rej", StateRejected
	return []Finding{sensitiveFinding(), dup, publicFinding(), rejected}
}

func TestPlanAggregatesRoutesDeterministically(t *testing.T) {
	pred := Plan("fixture", testRepo, campaignFindings(), nil)
	if pred.Campaign != "fixture" || pred.Repo != testRepo || len(pred.Findings) != 4 {
		t.Fatalf("prediction = %+v", pred)
	}
	routes := map[string]string{}
	for _, f := range pred.Findings {
		routes[f.ContentHash] = f.Route
	}
	want := map[string]string{"hash-auth": RouteIssue, "hash-sec": RoutePrivate, "hash-dup": RouteNone, "hash-rej": RouteNone}
	for hash, route := range want {
		if routes[hash] != route {
			t.Fatalf("route[%s] = %s, want %s", hash, routes[hash], route)
		}
	}
	if pred.Expected() != 2 {
		t.Fatalf("expected publications = %d, want 2", pred.Expected())
	}
	for i := 1; i < len(pred.Findings); i++ {
		if pred.Findings[i-1].ContentHash > pred.Findings[i].ContentHash {
			t.Fatalf("findings not sorted: %+v", pred.Findings)
		}
	}
	spec := pred.Spec()
	if spec != Plan("fixture", testRepo, campaignFindings(), nil).Spec() {
		t.Fatal("spec must be deterministic for the same findings")
	}
	if !strings.Contains(spec, "none hash-dup duplicate_of=bead-auth") || !strings.Contains(spec, "issue hash-auth") || !strings.Contains(spec, "private hash-sec") {
		t.Fatalf("spec = %q", spec)
	}
	if pred.Ref().Key() != OutcomeProject+"/"+testRepo+"@audit-fixture" {
		t.Fatalf("outcome ref key = %q", pred.Ref().Key())
	}
	// A custom classifier changes the route, proving the seam is honored.
	all := Plan("fixture", testRepo, campaignFindings(), func(Finding) Sensitivity { return SensitivitySensitive })
	for _, f := range all.Findings {
		if f.Route == RouteIssue {
			t.Fatalf("custom classifier ignored: %+v", f)
		}
	}
}

func TestOutcomeSlug(t *testing.T) {
	for in, want := range map[string]string{
		"fixture":            "audit-fixture",
		"Audit Campaign Q3!": "audit-audit-campaign-q3",
		"---":                "audit-campaign",
		"":                   "audit-campaign",
	} {
		if got := OutcomeSlug(in); got != want {
			t.Fatalf("OutcomeSlug(%q) = %q, want %q", in, got, want)
		}
		if err := (outcome.Ref{Project: OutcomeProject, Repo: testRepo, Outcome: OutcomeSlug(in)}).Validate(); err != nil {
			t.Fatalf("slug for %q is not a valid outcome name: %v", in, err)
		}
	}
}

func TestPredictionSatisfied(t *testing.T) {
	pred := Plan("fixture", testRepo, campaignFindings(), nil)
	pubs := map[string]Publication{
		"hash-auth": {State: StatePublished},
		"hash-sec":  {State: StatePrivate},
	}
	if !pred.Satisfied(pubs) {
		t.Fatal("published + private must satisfy the prediction")
	}
	pubs["hash-auth"] = Publication{State: StateExisting}
	if !pred.Satisfied(pubs) {
		t.Fatal("an existing issue satisfies an issue route")
	}
	pubs["hash-sec"] = Publication{State: StateWithheld}
	if pred.Satisfied(pubs) {
		t.Fatal("a withheld private finding cannot satisfy")
	}
	pubs["hash-sec"] = Publication{State: StatePrivate}
	pubs["hash-dup"] = Publication{State: StatePublished}
	if pred.Satisfied(pubs) {
		t.Fatal("a published duplicate violates the prediction")
	}
	delete(pubs, "hash-dup")
	delete(pubs, "hash-auth")
	if pred.Satisfied(pubs) {
		t.Fatal("a missing issue publication cannot satisfy")
	}
}

func TestBookAcceptRetireLifecycle(t *testing.T) {
	st := openStores(t)
	pred := Plan("fixture", testRepo, campaignFindings(), nil)

	rec, err := Book(st.outcomes, pred, testOwner)
	if err != nil || rec.Generation != 1 || rec.State != outcome.StateProposed || rec.Spec != pred.Spec() {
		t.Fatalf("first booking = %+v err=%v", rec, err)
	}
	again, err := Book(st.outcomes, pred, testOwner)
	if err != nil || again.Generation != 1 || len(again.History) != 1 {
		t.Fatalf("unchanged prediction must not supersede: %+v err=%v", again, err)
	}

	// Not yet satisfied: acceptance records nothing.
	rec, accepted, err := Accept(st.outcomes, pred, map[string]Publication{}, testOwner)
	if err != nil || accepted || rec.State != outcome.StateProposed {
		t.Fatalf("unsatisfied accept = %+v accepted=%v err=%v", rec, accepted, err)
	}
	pubs := map[string]Publication{"hash-auth": {State: StatePublished}, "hash-sec": {State: StatePrivate}}
	rec, accepted, err = Accept(st.outcomes, pred, pubs, testOwner)
	if err != nil || !accepted || rec.State != outcome.StateAccepted || rec.Generation != 1 {
		t.Fatalf("satisfied accept = %+v accepted=%v err=%v", rec, accepted, err)
	}
	rec, accepted, err = Accept(st.outcomes, pred, pubs, testOwner)
	if err != nil || accepted || rec.State != outcome.StateAccepted {
		t.Fatalf("second accept must be a no-op: %+v accepted=%v err=%v", rec, accepted, err)
	}

	// A changed prediction supersedes to generation 2, proposed again.
	grown := Plan("fixture", testRepo, append(campaignFindings(), func() Finding {
		f := publicFinding()
		f.BeadID, f.ContentHash = "bead-new", "hash-new"
		return f
	}()), nil)
	rec, err = Book(st.outcomes, grown, testOwner)
	if err != nil || rec.Generation != 2 || rec.State != outcome.StateProposed {
		t.Fatalf("superseded booking = %+v err=%v", rec, err)
	}

	rec, err = Retire(st.outcomes, grown, testOwner)
	if err != nil || rec.State != outcome.StateRetired {
		t.Fatalf("retire = %+v err=%v", rec, err)
	}
	if rec, err = Retire(st.outcomes, grown, testOwner); err != nil || rec.State != outcome.StateRetired {
		t.Fatalf("second retire must be a no-op: %+v err=%v", rec, err)
	}
	if rec, err = Book(st.outcomes, pred, testOwner); err != nil || rec.State != outcome.StateRetired {
		t.Fatalf("booking a retired outcome must leave it retired: %+v err=%v", rec, err)
	}
	if _, _, err := Accept(st.outcomes, grown, pubs, testOwner); err != nil {
		t.Fatalf("accepting a retired outcome must be a no-op, got %v", err)
	}
}

func TestBookAcceptRetireErrors(t *testing.T) {
	st := openStores(t)
	pred := Plan("fixture", testRepo, campaignFindings(), nil)
	if _, err := Book(nil, pred, testOwner); err == nil {
		t.Fatal("nil ledger must be refused by Book")
	}
	if _, _, err := Accept(nil, pred, nil, testOwner); err == nil {
		t.Fatal("nil ledger must be refused by Accept")
	}
	if _, err := Retire(nil, pred, testOwner); err == nil {
		t.Fatal("nil ledger must be refused by Retire")
	}
	if _, _, err := Accept(st.outcomes, pred, nil, testOwner); !errors.Is(err, outcome.ErrOutcomeNotFound) {
		t.Fatalf("accept before booking = %v, want not found", err)
	}
	if rec, err := Retire(st.outcomes, pred, testOwner); err != nil || rec.Generation != 0 {
		t.Fatalf("retire before booking must be a no-op: %+v err=%v", rec, err)
	}
	if _, err := Book(st.outcomes, pred, "intruder"); !errors.Is(err, outcome.ErrUnauthorizedActor) {
		t.Fatalf("unauthorized actor must be refused: %v", err)
	}
	if _, err := Book(st.outcomes, pred, testOwner); err != nil {
		t.Fatal(err)
	}
	pubs := map[string]Publication{"hash-auth": {State: StatePublished}, "hash-sec": {State: StatePrivate}}
	if _, _, err := Accept(st.outcomes, pred, pubs, "intruder"); !errors.Is(err, outcome.ErrUnauthorizedActor) {
		t.Fatalf("unauthorized accept must be refused: %v", err)
	}
	if _, err := Retire(st.outcomes, pred, "intruder"); !errors.Is(err, outcome.ErrUnauthorizedActor) {
		t.Fatalf("unauthorized retire must be refused: %v", err)
	}
}

func TestPublishCampaignBooksAndAcceptsOutcome(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	private := newFakeSeam()
	p, _ := newPublisher(t, st, public, enforcePolicy())
	p.Private = RepoChannel{Repo: testPrivate, Issues: private}
	grant := campaignGrant(t, st)
	c := Campaign{Key: "fixture", Repo: testRepo, RunKey: "run-9", RunURL: "https://hive.test/runs/run-9"}

	res, err := p.PublishCampaign(context.Background(), c, campaignFindings(), grant, st.outcomes)
	if err != nil {
		t.Fatalf("campaign publish: %v", err)
	}
	if !res.Accepted || res.Outcome.State != outcome.StateAccepted || res.Outcome.Generation != 1 {
		t.Fatalf("satisfied campaign must accept generation 1: %+v", res.Outcome)
	}
	if public.count() != 1 || private.count() != 1 {
		t.Fatalf("creates public=%d private=%d, want 1/1", public.count(), private.count())
	}
	if res.Publications["hash-auth"].State != StatePublished || res.Publications["hash-sec"].State != StatePrivate ||
		res.Publications["hash-dup"].State != StateNone || res.Publications["hash-rej"].State != StateNone {
		t.Fatalf("publications = %+v", res.Publications)
	}
	if body := public.issues[testRepo][0].body; !strings.Contains(body, RunTrailer+": run-9") {
		t.Fatalf("campaign run trailer missing:\n%s", body)
	}

	// Rerun: nothing filed, everything existing or replayed, still accepted.
	res, err = p.PublishCampaign(context.Background(), c, campaignFindings(), grant, st.outcomes)
	if err != nil || res.Accepted || res.Outcome.State != outcome.StateAccepted {
		t.Fatalf("rerun = %+v err=%v", res.Outcome, err)
	}
	if public.count() != 1 || private.count() != 1 {
		t.Fatalf("rerun filed again: public=%d private=%d", public.count(), private.count())
	}
	if res.Publications["hash-auth"].State != StateExisting {
		t.Fatalf("rerun public publication = %+v, want existing", res.Publications["hash-auth"])
	}
}

func TestPublishCampaignKeepsOutcomeProposedWhenWithheld(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	pol := enforcePolicy()
	pol.Mode = config.ConvergenceModeShadow
	p, _ := newPublisher(t, st, public, pol)
	c := Campaign{Key: "fixture", Repo: testRepo}
	res, err := p.PublishCampaign(context.Background(), c, campaignFindings(), campaignGrant(t, st), st.outcomes)
	if err != nil {
		t.Fatalf("shadow campaign publish: %v", err)
	}
	if res.Accepted || res.Outcome.State != outcome.StateProposed || res.Outcome.Generation != 1 {
		t.Fatalf("withheld publications must leave the outcome proposed: %+v", res.Outcome)
	}
	if public.count() != 0 {
		t.Fatalf("shadow campaign wrote %d issues", public.count())
	}
	if res.Publications["hash-auth"].Record() != "withheld:mode" {
		t.Fatalf("publication record = %q", res.Publications["hash-auth"].Record())
	}
}

func TestPublishCampaignSurfacesFirstErrorAndWorksWithoutLedger(t *testing.T) {
	st := openStores(t)
	public := newFakeSeam()
	p, _ := newPublisher(t, st, public, enforcePolicy())
	grant := campaignGrant(t, st)
	c := Campaign{Key: "fixture", Repo: testRepo}
	// No private channel: the sensitive finding refuses, the public one files.
	res, err := p.PublishCampaign(context.Background(), c, campaignFindings(), grant, nil)
	if !errors.Is(err, ErrNoPrivateChannel) {
		t.Fatalf("first error must surface: %v", err)
	}
	if res.Outcome.Generation != 0 || res.Accepted {
		t.Fatalf("no ledger means no outcome record: %+v", res.Outcome)
	}
	if res.Publications["hash-auth"].State != StatePublished || res.Publications["hash-sec"].State != StateRefused {
		t.Fatalf("publications = %+v", res.Publications)
	}
	// With a ledger the same partial run books but does not accept.
	res, err = p.PublishCampaign(context.Background(), c, campaignFindings(), grant, st.outcomes)
	if !errors.Is(err, ErrNoPrivateChannel) || res.Accepted || res.Outcome.State != outcome.StateProposed {
		t.Fatalf("partial campaign = %+v err=%v", res.Outcome, err)
	}
	// An unauthorized actor cannot book the prediction at all.
	p.Actor = "intruder"
	if _, err := p.PublishCampaign(context.Background(), Campaign{Key: "other", Repo: testRepo}, campaignFindings(), grant, st.outcomes); !errors.Is(err, outcome.ErrUnauthorizedActor) {
		t.Fatalf("unauthorized booking must fail the campaign: %v", err)
	}
}
