package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestValidateSetupBaselineWorkflowRunUsesCorrelationBoundTitleAndImmutableWorkflowID(t *testing.T) {
	head := strings.Repeat("a", 40)
	request := SetupBaselineArtifactRequest{
		WorkflowRunID:        77,
		CaptureHeadSHA:       head,
		DefaultBranch:        "main",
		ExpectedWorkflowName: "Hive Visual Hive Production",
		ExpectedRunName:      "Hive Visual Hive Production [correlation]",
	}
	valid := func() *gh.WorkflowRun {
		return &gh.WorkflowRun{
			ID: gh.Ptr(int64(77)), Event: gh.Ptr("workflow_dispatch"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success"),
			HeadSHA: gh.Ptr(head), HeadBranch: gh.Ptr("main"), DisplayTitle: gh.Ptr(request.ExpectedRunName),
			Name: gh.Ptr(request.ExpectedRunName), WorkflowID: gh.Ptr(int64(311323607)),
		}
	}
	if err := validateSetupBaselineWorkflowRun(valid(), request); err != nil {
		t.Fatalf("live correlation-bound run name was rejected before immutable workflow-definition verification: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*gh.WorkflowRun)
	}{
		{name: "id", mutate: func(run *gh.WorkflowRun) { run.ID = gh.Ptr(int64(78)) }},
		{name: "event", mutate: func(run *gh.WorkflowRun) { run.Event = gh.Ptr("push") }},
		{name: "status", mutate: func(run *gh.WorkflowRun) { run.Status = gh.Ptr("in_progress") }},
		{name: "conclusion", mutate: func(run *gh.WorkflowRun) { run.Conclusion = gh.Ptr("failure") }},
		{name: "head", mutate: func(run *gh.WorkflowRun) { run.HeadSHA = gh.Ptr(strings.Repeat("b", 40)) }},
		{name: "branch", mutate: func(run *gh.WorkflowRun) { run.HeadBranch = gh.Ptr("other") }},
		{name: "display title", mutate: func(run *gh.WorkflowRun) { run.DisplayTitle = gh.Ptr("other") }},
		{name: "workflow id", mutate: func(run *gh.WorkflowRun) { run.WorkflowID = gh.Ptr(int64(0)) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			run := valid()
			mutation.mutate(run)
			if err := validateSetupBaselineWorkflowRun(run, request); err == nil {
				t.Fatalf("%s mutation retained setup baseline run authority", mutation.name)
			}
		})
	}
	if err := validateSetupBaselineWorkflowRun(nil, request); err == nil {
		t.Fatal("nil setup baseline workflow run retained authority")
	}
}

func TestValidArtifactExtractionPrefixIncludesSetupBaselineWithoutAcceptingPaths(t *testing.T) {
	for _, prefix := range []string{"artifact", "source-artifact", "pr-artifact", "setup-baseline-artifact"} {
		if !validArtifactExtractionPrefix(prefix) {
			t.Fatalf("required extraction prefix %q was rejected", prefix)
		}
	}
	for _, prefix := range []string{"", ".", "..", "setup-baseline", "setup-baseline-artifact/child", `setup-baseline-artifact\\child`} {
		if validArtifactExtractionPrefix(prefix) {
			t.Fatalf("unsafe or unknown extraction prefix %q was accepted", prefix)
		}
	}
}

func TestValidateSetupBaselineJobInventoryRejectsTruncatedPage(t *testing.T) {
	head := strings.Repeat("a", 40)
	job := func(name, conclusion string) *gh.WorkflowJob {
		return &gh.WorkflowJob{
			RunID: gh.Ptr(int64(77)), Name: gh.Ptr(name), HeadSHA: gh.Ptr(head), HeadBranch: gh.Ptr("main"),
			Status: gh.Ptr("completed"), Conclusion: gh.Ptr(conclusion), Labels: []string{"ubuntu-latest"},
		}
	}
	visible := []*gh.WorkflowJob{
		job("Hive setup baseline capture (isolated target)", "success"),
		job("Hive setup baseline artifact verifier", "success"),
	}
	jobs := &gh.Jobs{TotalCount: gh.Ptr(101), Jobs: visible}
	if err := validateSetupBaselineJobInventory(jobs, 77, head, "main"); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected a truncated/paginated inventory rejection, got %v", err)
	}
	jobs.TotalCount = gh.Ptr(len(visible))
	if err := validateSetupBaselineJobInventory(jobs, 77, head, "main"); err != nil {
		t.Fatalf("exact complete inventory rejected: %v", err)
	}
}

func TestListAllSetupBaselineWorkflowJobsFindsLaterPageNonSkippedJob(t *testing.T) {
	head := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/repos/owner/repo/actions/runs/77/jobs" {
			http.Error(writer, "unexpected", http.StatusNotFound)
			return
		}
		if request.URL.Query().Get("filter") != "all" || request.URL.Query().Get("per_page") != "100" {
			http.Error(writer, "job enumeration did not request every attempt", http.StatusBadRequest)
			return
		}
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprintf(writer, `{"total_count":101,"jobs":[{"id":101,"run_id":77,"name":"unexpected-later-page","head_sha":%q,"head_branch":"main","status":"completed","conclusion":"success","labels":["ubuntu-latest"]}]}`, head)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, request.Host, request.URL.Path))
		jobs := strings.Builder{}
		for index := 0; index < 100; index++ {
			if index > 0 {
				jobs.WriteByte(',')
			}
			name, conclusion := fmt.Sprintf("skipped-%03d", index), "skipped"
			if index == 0 {
				name, conclusion = SetupBaselineCaptureJobDisplayName, "success"
			}
			if index == 1 {
				name, conclusion = SetupBaselineVerifyJobDisplayName, "success"
			}
			_, _ = fmt.Fprintf(&jobs, `{"id":%d,"run_id":77,"name":%q,"head_sha":%q,"head_branch":"main","status":"completed","conclusion":%q,"labels":["ubuntu-latest"]}`, index+1, name, head, conclusion)
		}
		_, _ = io.WriteString(writer, `{"total_count":101,"jobs":[`+jobs.String()+`]}`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	jobs, err := client.listAllSetupBaselineWorkflowJobs(context.Background(), "owner", "repo", 77)
	if err != nil || len(jobs.Jobs) != 101 {
		t.Fatalf("paginated job inventory was not complete: count=%d err=%v", len(jobs.Jobs), err)
	}
	if err := validateSetupBaselineJobInventory(jobs, 77, head, "main"); err == nil {
		t.Fatal("non-skipped later-page job was hidden from the exact verifier inventory")
	}
}

func TestListAllSetupBaselineWorkflowJobsRejectsDuplicateRerunAttempts(t *testing.T) {
	head := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("filter") != "all" {
			http.Error(writer, "latest filter is unsafe", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(writer, `{"total_count":3,"jobs":[{"id":1,"run_id":77,"name":%q,"head_sha":%q,"head_branch":"main","status":"completed","conclusion":"failure","labels":["ubuntu-latest"]},{"id":2,"run_id":77,"name":%q,"head_sha":%q,"head_branch":"main","status":"completed","conclusion":"success","labels":["ubuntu-latest"]},{"id":3,"run_id":77,"name":%q,"head_sha":%q,"head_branch":"main","status":"completed","conclusion":"success","labels":["ubuntu-latest"]}]}`,
			SetupBaselineCaptureJobDisplayName, head, SetupBaselineCaptureJobDisplayName, head, SetupBaselineVerifyJobDisplayName, head)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	jobs, err := client.listAllSetupBaselineWorkflowJobs(context.Background(), "owner", "repo", 77)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSetupBaselineJobInventory(jobs, 77, head, "main"); err == nil {
		t.Fatal("duplicate rerun attempt was accepted as a unique setup baseline topology")
	}
}

func TestFindUniqueSetupBaselineArtifactRejectsPageTwoDuplicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(writer, `{"total_count":101,"artifacts":[{"id":202,"name":"exact-artifact"}]}`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, request.Host, request.URL.Path))
		artifacts := strings.Builder{}
		for index := 0; index < 100; index++ {
			if index > 0 {
				artifacts.WriteByte(',')
			}
			name := fmt.Sprintf("other-%03d", index)
			if index == 0 {
				name = "exact-artifact"
			}
			_, _ = fmt.Fprintf(&artifacts, `{"id":%d,"name":%q}`, index+1, name)
		}
		_, _ = io.WriteString(writer, `{"total_count":101,"artifacts":[`+artifacts.String()+`]}`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := client.FindUniqueWorkflowRunArtifactByName(context.Background(), "owner", "repo", 77, "exact-artifact"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("page-two artifact duplicate was not rejected: %v", err)
	}
}

func TestFindUniqueSetupBaselineArtifactRejectsIncompleteInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"total_count":2,"artifacts":[{"id":1,"name":"exact-artifact"}]}`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := client.FindUniqueWorkflowRunArtifactByName(context.Background(), "owner", "repo", 77, "exact-artifact"); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete artifact inventory was accepted: %v", err)
	}
}

func TestFindUniqueSetupBaselineArtifactRejectsChangedTotalAcrossPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(writer, `{"total_count":1,"artifacts":[]}`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, request.Host, request.URL.Path))
		_, _ = io.WriteString(writer, `{"total_count":2,"artifacts":[{"id":1,"name":"exact-artifact"}]}`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := client.FindUniqueWorkflowRunArtifactByName(context.Background(), "owner", "repo", 77, "exact-artifact"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed artifact total was accepted: %v", err)
	}
}

func TestValidateSetupBaselineJobInventoryScaleBoundaries(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, count := range []int{98, 99, 100, 101, 512} {
		jobs := make([]*gh.WorkflowJob, 0, count)
		for index := 0; index < count; index++ {
			name, conclusion := "repository-test-skipped", "skipped"
			if index == 0 {
				name, conclusion = SetupBaselineCaptureJobDisplayName, "success"
			} else if index == 1 {
				name, conclusion = SetupBaselineVerifyJobDisplayName, "success"
			}
			jobs = append(jobs, &gh.WorkflowJob{ID: gh.Ptr(int64(index + 1)), RunID: gh.Ptr(int64(77)), Name: gh.Ptr(name), HeadSHA: gh.Ptr(head), HeadBranch: gh.Ptr("main"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr(conclusion), Labels: []string{"ubuntu-latest"}})
		}
		inventory := &gh.Jobs{TotalCount: gh.Ptr(count), Jobs: jobs}
		if err := validateSetupBaselineJobInventory(inventory, 77, head, "main"); err != nil {
			t.Fatalf("legal %d-job generated inventory rejected: %v", count, err)
		}
		if count == 101 {
			inventory.Jobs[100].Conclusion = gh.Ptr("success")
			if err := validateSetupBaselineJobInventory(inventory, 77, head, "main"); err == nil {
				t.Fatal("non-skipped job beyond the first API page was hidden")
			}
		}
	}
	tooMany := &gh.Jobs{TotalCount: gh.Ptr(513), Jobs: make([]*gh.WorkflowJob, 513)}
	if err := validateSetupBaselineJobInventory(tooMany, 77, head, "main"); err == nil {
		t.Fatal("513-job inventory exceeded the strict bound without rejection")
	}
}
