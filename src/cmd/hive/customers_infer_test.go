package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

type scriptedModel struct {
	reply string
	err   error
	calls int
	last  string
}

func (m *scriptedModel) Complete(_ context.Context, _ string, prompt string) (string, error) {
	m.calls++
	m.last = prompt
	return m.reply, m.err
}

func inferenceService(t *testing.T, logins ...string) (*customers.Service, map[string]string) {
	t.Helper()
	c := wireTestCustomers(t)
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	ids := map[string]string{}
	for _, login := range logins {
		p, err := c.svc.Add(customersViewer(owner), strings.ToUpper(login[:1])+login[1:], []customers.Identity{{Kind: customers.KindGitHub, Value: login}})
		if err != nil {
			t.Fatal(err)
		}
		ids[login] = p.ID
	}
	return c.svc, ids
}

func TestCustomersEvidenceIsReferencesAndCleanText(t *testing.T) {
	svc, ids := inferenceService(t, "carol", "dave")
	if _, err := svc.Store().AddClaim(ids["dave"], customers.Identity{Kind: customers.KindEmail, Value: "dave@example.com"}, "dave"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store().AddIdentity(ids["carol"], customers.Identity{Kind: customers.KindEmail, Value: "carol@example.com"}, "olivia"); err != nil {
		t.Fatal(err)
	}
	src := customersTestSources()
	src.last = &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "widgets", Number: 7, Author: "carol", AuthorIsHuman: true, State: "open", Title: "Exports time out", Body: strings.Repeat("slow ", 200)},
			{Repo: "widgets", Number: 8, Author: "some-app[bot]", AuthorIsHuman: false, Title: "bot"},
			{Repo: "widgets", Number: 9, Author: "stranger", AuthorIsHuman: true, Title: "not a customer"},
			{Repo: "widgets", Number: 0, Author: "carol", AuthorIsHuman: true, Title: "an external item"},
		}},
		PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "acme/widgets", Number: 20, Author: "carol", State: "closed", MergedAt: time.Now(), Title: "Faster export"},
			{Repo: "widgets", Number: 21, Author: "hive-author", Title: "the hive's own"},
			{Repo: "widgets", Number: 22, Author: "carol", AppAuthored: true, Title: "an app"},
		}},
	}
	src.mail = []inbox.Item{
		{ID: "m1", Sender: "Carol <carol@example.com>", Title: "Re: exports", Excerpt: "still slow"},
		{ID: "m2", Sender: "carol@example.com", Title: "awaiting", AwaitingApproval: true},
		{ID: "m3", Sender: "dave@example.com", Title: "a claimed address is not a key"},
	}
	got := customersEvidence(src, svc)
	if len(got) != 1 {
		t.Fatalf("evidence for %d people, want only carol: %v", len(got), got)
	}
	refs := map[string]string{}
	for _, e := range got[ids["carol"]] {
		refs[e.Ref] = e.Text
	}
	for _, want := range []string{"gh:issue:acme/widgets#7", "gh:pr:acme/widgets#20", "inbox:email:m1"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("missing evidence %s in %v", want, refs)
		}
	}
	if len(refs) != 3 {
		t.Fatalf("evidence = %v: bots, the hive's own author, non-customers, mail awaiting approval and claimed addresses must contribute none", refs)
	}
	if !strings.HasPrefix(refs["gh:pr:acme/widgets#20"], "Pull request (merged)") || len(refs["gh:issue:acme/widgets#7"]) > customersEvidenceBody+60 {
		t.Fatalf("texts = %v", refs)
	}
	if customersEvidence(customersSources{}, svc) == nil || len(customersEvidence(src, nil)) != 0 {
		t.Fatal("no config or no service gives an empty map")
	}
}

func TestRunCustomersInferenceLimitsAndStopsOnBudget(t *testing.T) {
	svc, ids := inferenceService(t, "carol", "dave", "erin", "frank", "gina")
	evidence := map[string][]customers.Evidence{}
	for login, id := range ids {
		evidence[id] = []customers.Evidence{{Ref: customers.IssueRef("o/r", len(login)), Text: "Issue " + login + ": exports time out"}}
	}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	// One pass reads at most InferMaxPerPass people.
	audited := map[string]customers.InferResult{}
	// Every person has a one-item evidence list; the model answers with a goal
	// citing the ref it was shown.
	cite := &citingModel{}
	read := runCustomersInferenceWith(context.Background(), svc, evidence, cite, now, func(id string, res customers.InferResult, err error) {
		audited[id] = res
	}, nil)
	if read != customers.InferMaxPerPass || cite.calls != customers.InferMaxPerPass || len(audited) != customers.InferMaxPerPass {
		t.Fatalf("read %d people, %d model calls, %d audited; want %d", read, cite.calls, len(audited), customers.InferMaxPerPass)
	}
	// The next pass reads the rest and skips the ones already read.
	cite.calls = 0
	read = runCustomersInferenceWith(context.Background(), svc, evidence, cite, now.Add(time.Minute), nil, nil)
	if read != len(ids)-customers.InferMaxPerPass || cite.calls != read {
		t.Fatalf("second pass read %d (calls %d), want %d", read, cite.calls, len(ids)-customers.InferMaxPerPass)
	}
	for login, id := range ids {
		p, _ := svc.Store().Get(id)
		if len(p.Goals) != 1 || p.Goals[0].State != customers.StateSuggested || p.Goals[0].By != "" {
			t.Fatalf("%s: goals = %+v: inference must leave a suggestion and nothing else", login, p.Goals)
		}
	}

	// A spent budget ends the pass at once.
	svc2, ids2 := inferenceService(t, "hank", "ivan")
	budget := &scriptedModel{err: meetllm.ErrBudget}
	ev2 := map[string][]customers.Evidence{}
	for _, id := range ids2 {
		ev2[id] = []customers.Evidence{{Ref: customers.IssueRef("o/r", 1), Text: "Issue: exports time out"}}
	}
	read = runCustomersInferenceWith(context.Background(), svc2, ev2, budget, now, nil, nil)
	if budget.calls != 1 || read != 1 {
		t.Fatalf("a spent budget: %d calls, read %d; want to stop after the first", budget.calls, read)
	}
	// Another error is audited and the pass goes on.
	other := &scriptedModel{err: errors.New("provider down")}
	read = runCustomersInferenceWith(context.Background(), svc2, ev2, other, now, nil, nil)
	if other.calls != 2 || read != 2 {
		t.Fatalf("a transient error: %d calls, read %d; want both people tried", other.calls, read)
	}
	// No evidence, no model, a cancelled context: nothing is asked.
	if runCustomersInferenceWith(context.Background(), svc2, map[string][]customers.Evidence{}, other, now, nil, nil) != 0 {
		t.Fatal("no evidence must ask nothing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if runCustomersInferenceWith(ctx, svc2, ev2, other, now, nil, nil) != 0 {
		t.Fatal("a cancelled pass asked the model")
	}
}

// citingModel answers with one goal that cites the first ref in the prompt.
type citingModel struct{ calls int }

func (m *citingModel) Complete(_ context.Context, _ string, prompt string) (string, error) {
	m.calls++
	i := strings.Index(prompt, "[")
	j := strings.Index(prompt[i:], "]")
	ref := prompt[i+1 : i+j]
	return `{"goals":[{"text":"Wants exports that finish","evidence":["` + ref + `"]}],"satisfaction":{"level":"unknown","evidence":[]}}`, nil
}

func TestCustomersInferenceModelBackends(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	cfg := &config.Config{}
	cfg.Meetings.LLM.Mode = config.MeetingsLLMModeNone
	if m, ok := customersInferenceModel(context.Background(), cfg, dir, now, nil); ok || m != nil {
		t.Fatal("no meetings model, no inference")
	}
	cfg.Meetings.LLM.Mode = config.MeetingsLLMModeOpenAI
	cfg.Meetings.LLM.Endpoint = "http://127.0.0.1:1"
	cfg.Meetings.LLM.Model = "m"
	if m, ok := customersInferenceModel(context.Background(), cfg, dir, now, nil); !ok || m == nil {
		t.Fatal("an OpenAI-compatible meetings model must be usable")
	}
	st, err := os.Stat(filepath.Join(dir, customersInferenceUsageDir))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("usage dir = %v, %v", st, err)
	}
	// Old ledgers are pruned; today's and recent ones stay.
	usage := filepath.Join(dir, customersInferenceUsageDir)
	for _, day := range []string{"2026-11-01", "2026-11-18"} {
		if err := os.WriteFile(filepath.Join(usage, "usage-"+day+".jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	customersInferenceModel(context.Background(), cfg, dir, now, nil)
	if _, err := os.Stat(filepath.Join(usage, "usage-2026-11-01.jsonl")); !os.IsNotExist(err) {
		t.Fatal("an old usage ledger was kept")
	}
	if _, err := os.Stat(filepath.Join(usage, "usage-2026-11-18.jsonl")); err != nil {
		t.Fatal("a recent usage ledger was pruned")
	}
	// An unusable data dir leaves inference off.
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := customersInferenceModel(context.Background(), cfg, blocked, now, nil); ok {
		t.Fatal("an unusable directory must leave inference off")
	}
}

func TestRunCustomersInferenceIsOffByDefault(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "")
	t.Setenv(config.EnvCustomersInferenceEnabled, "")
	b := &boot{}
	b.customers = wireTestCustomers(t)
	cfg := &config.Config{}
	cfg.Meetings.LLM.Mode = config.MeetingsLLMModeNone
	// Off, nil config, nil store: none of these may call anything or panic.
	b.runCustomersInference(context.Background(), cfg)
	b.runCustomersInference(context.Background(), nil)
	b.customers = nil
	on := true
	cfg.Customers.Inference.Enabled = &on
	b.runCustomersInference(context.Background(), cfg)
}
