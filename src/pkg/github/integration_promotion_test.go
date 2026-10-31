package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

type integrationPromotionJournal struct {
	unresolved bool
	applied    bool
	fail       bool
	result     effects.Result
	claim      effects.Claim
}

func (j *integrationPromotionJournal) ExecuteJournaled(ctx context.Context, claim effects.Claim, apply func(context.Context) (effects.Result, error)) (effects.Result, error) {
	return j.Execute(ctx, claim, apply)
}

func (j *integrationPromotionJournal) Execute(ctx context.Context, claim effects.Claim, apply func(context.Context) (effects.Result, error)) (effects.Result, error) {
	j.claim = claim
	if j.unresolved {
		return effects.Result{}, effects.ErrNeedsReconciliation
	}
	if j.applied {
		return j.result, nil
	}
	out, err := apply(ctx)
	j.unresolved = err != nil && !errors.Is(err, effects.ErrNotApplied)
	j.applied, j.result = err == nil, out
	return out, err
}

func (j *integrationPromotionJournal) Reconcile(_ context.Context, _ effects.Claim, state effects.ExternalState) error {
	if j.fail {
		return errors.New("journal unavailable")
	}
	j.unresolved, j.applied = false, state.Applied
	j.result = effects.Result{Provenance: state.Provenance}
	return nil
}

func (j *integrationPromotionJournal) LookupApplied(context.Context, effects.Claim) (effects.Result, bool) {
	return j.result, j.applied
}

type integrationPromotionFixture struct {
	request   IntegrationPromotion
	ref       string
	headRef   string
	authority string
	tree      string
	parents   []string
	repoID    string
	repoName  string
	errorAt   string
	reply     string
	mu        sync.Mutex
	input     map[string]any
	writes    atomic.Int32
}

func integrationPromotionRequest() IntegrationPromotion {
	return IntegrationPromotion{
		Repo: "o/r", Base: "v5", ExpectedSHA: strings.Repeat("a", 40),
		TestedSHA: strings.Repeat("b", 40), TestedTree: strings.Repeat("c", 40),
		AuthorHead: strings.Repeat("d", 40), SubjectID: "subject-1",
		HeadRepo: "o/r", HeadRef: "feature/member-1",
		Policy: "policy-1", Generation: 1,
	}
}

func promotionFixture() *integrationPromotionFixture {
	p := integrationPromotionRequest()
	return &integrationPromotionFixture{
		request: p, ref: p.ExpectedSHA, tree: p.TestedTree,
		headRef: p.AuthorHead,
		parents: []string{p.ExpectedSHA, p.AuthorHead}, repoID: "R_repository", repoName: p.Repo,
	}
}

func (f *integrationPromotionFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == f.errorAt {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r":
			_ = json.NewEncoder(w).Encode(map[string]any{"node_id": f.repoID, "full_name": f.repoName})
		case "/repos/o/r/git/commits/" + f.request.TestedSHA:
			parents := make([]map[string]string, 0, len(f.parents))
			for _, parent := range f.parents {
				parents = append(parents, map[string]string{"sha": parent})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha": f.request.TestedSHA, "tree": map[string]string{"sha": f.tree}, "parents": parents,
			})
		case "/repos/o/r/git/ref/heads/v5":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": f.ref}})
		case "/graphql":
			f.writes.Add(1)
			var body struct {
				Query     string `json:"query"`
				Variables struct {
					Input map[string]any `json:"input"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !strings.Contains(body.Query, "updateRefs") || strings.Contains(body.Query, "createCommitOnBranch") {
				t.Errorf("not the supported existing-object CAS mutation: %s", body.Query)
			}
			f.input = body.Variables.Input
			if f.reply != "" {
				_, _ = w.Write([]byte(f.reply))
				return
			}
			updates, ok := f.input["refUpdates"].([]any)
			if !ok {
				http.Error(w, "missing ref updates", http.StatusBadRequest)
				return
			}
			refs := map[string]string{
				"refs/heads/" + f.request.Base:    f.ref,
				"refs/heads/" + f.request.HeadRef: f.headRef,
			}
			if f.request.AuthorityRef != "" {
				refs[f.request.AuthorityRef] = f.authority
			}
			for _, raw := range updates {
				update, valid := raw.(map[string]any)
				name, named := update["name"].(string)
				current, exists := refs[name]
				if !valid || !named || !exists || current != update["beforeOid"] || update["force"] != false {
					_, _ = w.Write([]byte(`{"errors":[{"type":"UNPROCESSABLE","message":"beforeOid mismatch"}]}`))
					return
				}
			}
			for _, raw := range updates {
				update, _ := raw.(map[string]any)
				name, _ := update["name"].(string)
				after, valid := update["afterOid"].(string)
				if !valid {
					http.Error(w, "invalid target", http.StatusBadRequest)
					return
				}
				refs[name] = after
			}
			f.ref, f.headRef = refs["refs/heads/"+f.request.Base], refs["refs/heads/"+f.request.HeadRef]
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"updateRefs": map[string]any{"clientMutationId": f.input["clientMutationId"]},
			}})
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestIntegrationPromotionExactCASAndJournalReplay(t *testing.T) {
	f := promotionFixture()
	server := f.server(t)
	defer server.Close()
	c := testClient(t, server.URL)
	journal := new(integrationPromotionJournal)
	guards := 0
	guard := func(_ context.Context, got IntegrationPromotion) error {
		guards++
		if got != f.request {
			t.Fatal("live guard was not bound to the exact candidate")
		}
		return nil
	}
	for range 2 {
		out, err := c.PromoteIntegrationStep(context.Background(), f.request, journal, guard)
		if err != nil || out.Provenance != f.request.TestedSHA {
			t.Fatalf("promotion: %+v %v", out, err)
		}
	}
	if guards != 1 || f.writes.Load() != 1 {
		t.Fatalf("replayed external effect: guards=%d mutations=%d", guards, f.writes.Load())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	updates, ok := f.input["refUpdates"].([]any)
	if !ok || len(updates) != 2 {
		t.Fatalf("not a bounded destination/head CAS: %#v", f.input)
	}
	update, ok := updates[0].(map[string]any)
	if !ok || update["beforeOid"] != f.request.ExpectedSHA ||
		update["afterOid"] != f.request.TestedSHA || update["force"] != false ||
		update["name"] != "refs/heads/v5" || f.input["repositoryId"] != "R_repository" {
		t.Fatalf("unsafe provider mutation: %#v", f.input)
	}
	head, ok := updates[1].(map[string]any)
	if !ok || head["name"] != "refs/heads/"+f.request.HeadRef ||
		head["beforeOid"] != f.request.AuthorHead || head["afterOid"] != f.request.AuthorHead || head["force"] != false {
		t.Fatalf("missing atomic author-head fence: %#v", f.input)
	}
	if journal.claim.Kind != effects.KindBranchUpdate || journal.claim.Actor != "integration-aware" ||
		journal.claim.Inputs["subject"] != f.request.SubjectID ||
		journal.claim.Inputs["head_repo"] != f.request.HeadRepo ||
		journal.claim.Inputs["head_ref"] != f.request.HeadRef ||
		journal.claim.Inputs["protocol"] != IntegrationPromotionProtocol {
		t.Fatalf("missing mutation provenance: %+v", journal.claim)
	}
}

func TestIntegrationPromotionRefusesIncompleteOrChangedSubjects(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*integrationPromotionFixture)
	}{
		{"wrong-tree", func(f *integrationPromotionFixture) { f.tree = strings.Repeat("e", 40) }},
		{"reversed-parents", func(f *integrationPromotionFixture) { f.parents[0], f.parents[1] = f.parents[1], f.parents[0] }},
		{"wrong-parent-count", func(f *integrationPromotionFixture) { f.parents = f.parents[:1] }},
		{"unknown-repository", func(f *integrationPromotionFixture) { f.repoID = "" }},
		{"renamed-repository", func(f *integrationPromotionFixture) { f.repoName = "another/repository" }},
		{"repository-read-failed", func(f *integrationPromotionFixture) { f.errorAt = "/repos/o/r" }},
		{"commit-read-failed", func(f *integrationPromotionFixture) { f.errorAt = "/repos/o/r/git/commits/" + f.request.TestedSHA }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := promotionFixture()
			test.change(f)
			server := f.server(t)
			defer server.Close()
			called := false
			_, err := testClient(t, server.URL).PromoteIntegrationStep(context.Background(), f.request, new(integrationPromotionJournal),
				func(context.Context, IntegrationPromotion) error { called = true; return nil })
			if err == nil || called || f.writes.Load() != 0 {
				t.Fatalf("invalid subject reached promotion: err=%v guard=%v writes=%d", err, called, f.writes.Load())
			}
		})
	}
}

func TestIntegrationPromotionLiveGuardAndProviderFailures(t *testing.T) {
	for _, reply := range []string{
		`{"data":{"updateRefs":null}}`,
		`{"data":{"updateRefs":{"clientMutationId":"another-request"}}}`,
		`{"errors":[{"type":"FORBIDDEN","message":"branch protection refused"}]}`,
	} {
		f := promotionFixture()
		f.reply = reply
		server := f.server(t)
		c := testClient(t, server.URL)
		journal := new(integrationPromotionJournal)
		_, err := c.PromoteIntegrationStep(context.Background(), f.request, journal,
			func(context.Context, IntegrationPromotion) error { return errors.New("live hold") })
		if err == nil || journal.unresolved || f.writes.Load() != 0 {
			t.Fatalf("live hold was not a definite refusal: %v", err)
		}
		_, err = c.PromoteIntegrationStep(context.Background(), f.request, journal,
			func(context.Context, IntegrationPromotion) error { return nil })
		if err == nil || !journal.unresolved || f.writes.Load() != 1 {
			t.Fatalf("provider rejection/ambiguity became success: %v", err)
		}
		server.Close()
	}
}

func TestIntegrationPromotionDestinationMovesAfterLiveGuard(t *testing.T) {
	f := promotionFixture()
	server := f.server(t)
	defer server.Close()
	journal := new(integrationPromotionJournal)
	other := strings.Repeat("e", 40)
	_, err := testClient(t, server.URL).PromoteIntegrationStep(context.Background(), f.request, journal,
		func(context.Context, IntegrationPromotion) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ref = other
			return nil
		})
	if err == nil || !journal.unresolved || f.writes.Load() != 1 {
		t.Fatalf("destination race was not refused: err=%v unresolved=%v", err, journal.unresolved)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ref != other {
		t.Fatal("a read-then-write overwrote the competing destination")
	}
}

func TestIntegrationPromotionAuthorHeadMovesAfterLiveGuard(t *testing.T) {
	f := promotionFixture()
	server := f.server(t)
	defer server.Close()
	journal := new(integrationPromotionJournal)
	_, err := testClient(t, server.URL).PromoteIntegrationStep(context.Background(), f.request, journal,
		func(context.Context, IntegrationPromotion) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.headRef = strings.Repeat("e", 40)
			return nil
		})
	if err == nil || !journal.unresolved || f.writes.Load() != 1 {
		t.Fatalf("author head race was not refused: err=%v unresolved=%v", err, journal.unresolved)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ref != f.request.ExpectedSHA {
		t.Fatal("moved author head did not atomically prevent destination promotion")
	}
}

func TestIntegrationPromotionReconcilesWithoutBlindTTLRetry(t *testing.T) {
	for _, state := range []string{"applied", "not-applied", "changed", "unavailable", "journal-failed"} {
		t.Run(state, func(t *testing.T) {
			f := promotionFixture()
			journal := &integrationPromotionJournal{unresolved: true}
			switch state {
			case "applied":
				f.ref = f.request.TestedSHA
			case "changed":
				f.ref = strings.Repeat("e", 40)
			case "unavailable":
				f.errorAt = "/repos/o/r/git/ref/heads/v5"
			case "journal-failed":
				journal.fail = true
			}
			server := f.server(t)
			defer server.Close()
			calls := 0
			out, err := testClient(t, server.URL).PromoteIntegrationStep(context.Background(), f.request, journal,
				func(context.Context, IntegrationPromotion) error { calls++; return nil })
			wantSuccess := state == "applied" || state == "not-applied"
			if (err == nil) != wantSuccess || wantSuccess && out.Provenance != f.request.TestedSHA {
				t.Fatalf("reconciliation state=%s result=%+v err=%v", state, out, err)
			}
			wantCalls := 0
			if state == "not-applied" {
				wantCalls = 1
			}
			if calls != wantCalls || int(f.writes.Load()) != wantCalls {
				t.Fatalf("unexpected blind retry: guards=%d writes=%d", calls, f.writes.Load())
			}
			if state == "changed" && !errors.Is(err, ErrIntegrationDestinationChanged) {
				t.Fatalf("changed destination lost explicit invalidation reason: %v", err)
			}
		})
	}
}

func TestIntegrationPromotionInvalidRequestsHaveNoEffects(t *testing.T) {
	for _, repo := range []string{"r", "o/r/extra", "o/r?query", "o/r#fragment", "o/r%2fother", "../r", "o/..", "o/.", "o/r\x01"} {
		if integrationPromotionRepo(repo) {
			t.Errorf("accepted ambiguous repository %q", repo)
		}
	}
	if !integrationPromotionRepo("Owner/repo.name_1") {
		t.Fatal("valid repository rejected")
	}
	fork := integrationPromotionRequest()
	fork.HeadRepo = "fork/r"
	if !errors.Is(fork.validate(), ErrIntegrationHeadFenceUnavailable) {
		t.Fatal("fork head lost its explicit unsupported atomic-fence reason")
	}
	for _, branch := range []string{"", "@", "/v5", "v5/", "v5.", "v5..next", ".hidden", "a/.hidden", "a.lock", "a//b", "v5@{1}", "a b", "a\x01b", "a\x7fb"} {
		if integrationPromotionBranch(branch) {
			t.Errorf("accepted invalid branch %q", branch)
		}
	}
	if !integrationPromotionBranch("release/v5") {
		t.Fatal("valid scoped branch rejected")
	}
	for _, change := range []func(*IntegrationPromotion){
		func(p *IntegrationPromotion) { p.Repo = "r" },
		func(p *IntegrationPromotion) { p.Base = "" },
		func(p *IntegrationPromotion) { p.HeadRepo = "fork/r" },
		func(p *IntegrationPromotion) { p.HeadRef = p.Base },
		func(p *IntegrationPromotion) { p.HeadRef = "" },
		func(p *IntegrationPromotion) { p.ExpectedSHA = strings.Repeat("0", 40) },
		func(p *IntegrationPromotion) { p.TestedSHA = strings.Repeat("B", 40) },
		func(p *IntegrationPromotion) { p.TestedTree = "short" },
		func(p *IntegrationPromotion) { p.AuthorHead = p.ExpectedSHA },
		func(p *IntegrationPromotion) { p.TestedSHA = p.ExpectedSHA },
		func(p *IntegrationPromotion) { p.SubjectID = "" },
		func(p *IntegrationPromotion) { p.Policy = "" },
		func(p *IntegrationPromotion) { p.Policy = "policy\nsubject=another" },
		func(p *IntegrationPromotion) { p.SubjectID = strings.Repeat("x", integrationPromotionMaxLabelBytes+1) },
		func(p *IntegrationPromotion) { p.Generation = 0 },
	} {
		p := integrationPromotionRequest()
		change(&p)
		if p.validate() == nil {
			t.Errorf("accepted incomplete request: %+v", p)
		}
	}
	var missing *Client
	if _, err := missing.PromoteIntegrationStep(context.Background(), integrationPromotionRequest(), nil, nil); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: %v", err)
	}
	server := promotionFixture().server(t)
	defer server.Close()
	c := testClient(t, server.URL)
	p := integrationPromotionRequest()
	guard := func(context.Context, IntegrationPromotion) error { return nil }
	var missingLogging *effects.LoggingBoundary
	for _, boundary := range []effects.Boundary{
		nil, effects.NoopBoundary{}, &effects.NoopBoundary{},
		effects.LoggingBoundary{}, &effects.LoggingBoundary{Next: effects.NoopBoundary{}}, missingLogging,
	} {
		if _, err := c.PromoteIntegrationStep(context.Background(), p, boundary, guard); err == nil {
			t.Fatal("accepted missing/unfenced boundary")
		}
		if !integrationPromotionJournaled(effects.LoggingBoundary{Next: &effects.LoggingBoundary{Next: new(integrationPromotionJournal)}}) {
			t.Fatal("a real journal wrapped in logging lost its capabilities")
		}
	}
	if _, err := c.PromoteIntegrationStep(context.Background(), p, new(integrationPromotionJournal), nil); err == nil {
		t.Fatal("accepted missing live guard")
	}
	p.SubjectID = ""
	if _, err := c.PromoteIntegrationStep(context.Background(), p, new(integrationPromotionJournal), guard); err == nil {
		t.Fatal("accepted invalid request")
	}
}
