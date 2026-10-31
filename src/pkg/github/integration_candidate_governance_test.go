package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

func governanceReceiptFixture() (integrationCandidateReceipt, *gh.WorkflowRun, []*gh.WorkflowJob, []*gh.CheckRun) {
	receipt := integrationCandidateReceipt{Schema: 2, Protocol: "integration-producer-receipt-v2",
		MetadataDigest: strings.Repeat("a", 64), Contexts: map[string]forgestate.IntegrationContextReceipt{}}
	run := &gh.WorkflowRun{ID: gh.Int64(1), HeadSHA: gh.String(strings.Repeat("b", 40)), CheckSuiteID: gh.Int64(2)}
	jobs, checks := []*gh.WorkflowJob{}, []*gh.CheckRun{}
	for i, name := range forgestate.IntegrationRequiredContexts() {
		record := forgestate.IntegrationContextReceipt{JobID: int64(10 + i), CheckID: int64(110 + i), AppID: 3, SuiteID: 2, Conclusion: "success"}
		receipt.Contexts[name] = record
		jobs = append(jobs, &gh.WorkflowJob{ID: gh.Int64(record.JobID), RunID: run.ID, HeadSHA: run.HeadSHA, Name: gh.String(name),
			CheckRunURL: gh.String("https://api.github.com/checks/" + fmtID(record.CheckID)), Status: gh.String("completed"), Conclusion: gh.String("success")})
		checks = append(checks, &gh.CheckRun{ID: gh.Int64(record.CheckID), Name: gh.String(name), HeadSHA: run.HeadSHA,
			CheckSuite: &gh.CheckSuite{ID: run.CheckSuiteID}, App: &gh.App{ID: gh.Int64(3)}, Status: gh.String("completed"), Conclusion: gh.String("success")})
	}
	return receipt, run, jobs, checks
}

func fmtID(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestCandidateSignedContextsCannotCopyAnotherJobHeadSuiteAppOrAttempt(t *testing.T) {
	receipt, run, jobs, checks := governanceReceiptFixture()
	if err := bindCandidateContextReceipt(receipt, run, jobs, checks, 3); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*integrationCandidateReceipt, *[]*gh.WorkflowJob, *[]*gh.CheckRun){
		func(r *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			delete(r.Contexts, "DCO")
		},
		func(r *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			r.Contexts["DCO"] = r.Contexts["gate"]
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) { *j = (*j)[1:] },
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			*j = append(*j, (*j)[0])
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].HeadSHA = gh.String("other")
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].RunID = gh.Int64(9)
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].ID = gh.Int64(99)
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].Status = gh.String("queued")
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].Conclusion = gh.String("skipped")
		},
		func(_ *integrationCandidateReceipt, j *[]*gh.WorkflowJob, _ *[]*gh.CheckRun) {
			(*j)[0].CheckRunURL = gh.String("wrong")
		},
		func(_ *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, c *[]*gh.CheckRun) { *c = (*c)[1:] },
		func(_ *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, c *[]*gh.CheckRun) {
			(*c)[0].App.ID = gh.Int64(99)
		},
		func(_ *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, c *[]*gh.CheckRun) {
			(*c)[0].CheckSuite.ID = gh.Int64(99)
		},
		func(_ *integrationCandidateReceipt, _ *[]*gh.WorkflowJob, c *[]*gh.CheckRun) {
			(*c)[0].Conclusion = gh.String("failure")
		},
	} {
		r, actual, j, c := governanceReceiptFixture()
		mutate(&r, &j, &c)
		if err := bindCandidateContextReceipt(r, actual, j, c, 3); err == nil {
			t.Fatal("false-green/missing/wrong physical context accepted")
		}
	}
	if err := bindCandidateContextReceipt(integrationCandidateReceipt{Schema: 1}, nil, nil, nil, 0); err != nil {
		t.Fatal("historical contract unreadable", err)
	}
}

func TestCandidateGovernanceBindsMutableMetadataButAllowsExactDocumentedPrefix(t *testing.T) {
	subject := forgestate.IntegrationCandidateSubject{Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "o/r", Base: "v5",
		BaseSHA: strings.Repeat("a", 40), Policy: strings.Repeat("b", 64), WorkflowTree: strings.Repeat("c", 40), ID: strings.Repeat("d", 64),
		Members:  []forgestate.IntegrationCandidateMember{{PR: 7, Head: strings.Repeat("e", 40)}},
		Prefixes: []forgestate.IntegrationCandidatePrefix{{Commit: strings.Repeat("f", 40), Tree: strings.Repeat("1", 40)}}}
	makePR := func() *gh.PullRequest {
		repo := &gh.Repository{FullName: gh.String("o/r")}
		return &gh.PullRequest{Number: gh.Int(7), State: gh.String("open"), Title: gh.String("✨ title"), Body: gh.String("body <>&\u2028"),
			User: &gh.User{ID: gh.Int64(42), Login: gh.String("author")},
			Base: &gh.PullRequestBranch{Ref: gh.String("v5"), Repo: repo}, Head: &gh.PullRequestBranch{SHA: gh.String(subject.Members[0].Head), Ref: gh.String("feature"), Repo: repo},
			Labels: []*gh.Label{{Name: gh.String("approved")}}}
	}
	current := makePR()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(current)
	}))
	defer server.Close()
	api := gh.NewClient(server.Client())
	api.BaseURL, _ = url.Parse(server.URL + "/")
	client := &Client{client: api}
	receipt, _, _, _ := governanceReceiptFixture()
	receipt.Subject = subject
	row, _ := json.Marshal([][]any{integrationGovernanceRow(current)})
	receipt.MetadataDigest = integrationGovernanceHash(string(row))
	payload, _ := json.Marshal(receipt)
	if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, payload); err != nil {
		t.Fatal(err)
	}
	current.State, current.Merged, current.MergeCommitSHA = gh.String("closed"), gh.Bool(true), gh.String(subject.Prefixes[0].Commit)
	if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, payload); err != nil {
		t.Fatal("exact documented prefix spuriously invalidated", err)
	}
	for _, mutate := range []func(*gh.PullRequest){
		func(pr *gh.PullRequest) { pr.Title = gh.String("changed") },
		func(pr *gh.PullRequest) { pr.Body = gh.String("changed") },
		func(pr *gh.PullRequest) { pr.Labels = nil },
		func(pr *gh.PullRequest) { pr.Head.Ref = gh.String("sync/v5") },
		func(pr *gh.PullRequest) { pr.Head.SHA = gh.String("other") },
		func(pr *gh.PullRequest) { pr.Base.Ref = gh.String("other") },
		func(pr *gh.PullRequest) { pr.Draft = gh.Bool(true) },
		func(pr *gh.PullRequest) { pr.State = gh.String("closed") },
		func(pr *gh.PullRequest) {
			pr.State = gh.String("closed")
			pr.Merged = gh.Bool(true)
			pr.MergeCommitSHA = gh.String("wrong")
		},
	} {
		current = makePR()
		mutate(current)
		if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, payload); err == nil {
			t.Fatal("changed member policy accepted")
		}
	}
	current = nil
	if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, payload); err == nil {
		t.Fatal("missing API proof accepted")
	}
	for _, invalid := range [][]byte{[]byte("{"), []byte(`{"schema":3}`), []byte(`{"schema":2,"protocol":"wrong"}`)} {
		if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, invalid); err == nil {
			t.Fatal("invalid contract accepted")
		}
	}
	if err := client.VerifyIntegrationCandidateGovernance(context.Background(), subject, []byte(`{"schema":1}`)); err != nil {
		t.Fatal(err)
	}
}
