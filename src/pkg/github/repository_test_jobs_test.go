package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const (
	repositoryTestJobsHead    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repositoryTestJobsRunName = "Hive Visual Hive Production [bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb]"
)

type repositoryTestJobsFixture struct {
	t                 *testing.T
	server            *httptest.Server
	runName           string
	runPath           string
	runEvent          string
	runHead           string
	runStatus         string
	runConclusion     string
	definitionName    string
	definitionPath    string
	definitionState   string
	pages             [][]string
	requestedPages    []int
	requestedFilters  []string
	requestedPerPages []int
}

func newRepositoryTestJobsFixture(t *testing.T) *repositoryTestJobsFixture {
	t.Helper()
	fixture := &repositoryTestJobsFixture{
		t: t, runName: repositoryTestJobsRunName, runPath: ".github/workflows/hive-visual-hive.yml@main",
		runEvent: "workflow_dispatch", runHead: repositoryTestJobsHead, runStatus: "completed", runConclusion: "success",
		definitionName: "Hive Visual Hive Production", definitionPath: ".github/workflows/hive-visual-hive.yml", definitionState: "active",
		pages: [][]string{
			{repositoryTestJobJSON(700, "visual-hive-production", "success"), repositoryTestJobJSON(701, "Hive repository test 001", "success")},
			{repositoryTestJobJSON(702, "Hive repository test 002", "failure"), repositoryTestJobJSON(703, "visual-hive", "success"), repositoryTestJobJSON(704, "bundle", "success")},
		},
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	return fixture
}

func (fixture *repositoryTestJobsFixture) serve(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case "/repos/owner/repo/actions/runs/77":
		_, _ = fmt.Fprintf(writer, `{"id":77,"workflow_id":12,"name":%q,"path":%q,"event":%q,"head_sha":%q,"status":%q,"conclusion":%q,"repository":{"full_name":"owner/repo"}}`,
			fixture.runName, fixture.runPath, fixture.runEvent, fixture.runHead, fixture.runStatus, fixture.runConclusion)
	case "/repos/owner/repo/actions/workflows/12":
		_, _ = fmt.Fprintf(writer, `{"id":12,"name":%q,"path":%q,"state":%q}`, fixture.definitionName, fixture.definitionPath, fixture.definitionState)
	case "/repos/owner/repo/actions/runs/77/jobs":
		page, _ := strconv.Atoi(request.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		fixture.requestedPages = append(fixture.requestedPages, page)
		fixture.requestedFilters = append(fixture.requestedFilters, request.URL.Query().Get("filter"))
		perPage, _ := strconv.Atoi(request.URL.Query().Get("per_page"))
		fixture.requestedPerPages = append(fixture.requestedPerPages, perPage)
		if page > len(fixture.pages) {
			http.Error(writer, `{"message":"unexpected page"}`, http.StatusNotFound)
			return
		}
		if page < len(fixture.pages) {
			writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/actions/runs/77/jobs?page=%d&per_page=100&filter=all>; rel="next"`, fixture.server.URL, page+1))
		}
		_, _ = io.WriteString(writer, `{"total_count":`+strconv.Itoa(repositoryTestJobsRowCount(fixture.pages))+`,"jobs":[`+strings.Join(fixture.pages[page-1], ",")+`]}`)
	default:
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}
}

func (fixture *repositoryTestJobsFixture) request() RepositoryTestJobsRequest {
	return RepositoryTestJobsRequest{
		Repository: "owner/repo", WorkflowRunID: 77, ExpectedHeadSHA: repositoryTestJobsHead,
		ExpectedWorkflowName: "Hive Visual Hive Production", ExpectedWorkflowPath: ".github/workflows/hive-visual-hive.yml",
		Commands: [][]string{{"npm", "test"}, {"python", "-m", "pytest", "-q"}}, VisualJobName: "visual-hive-production",
		RequiredSuccessfulJobNames: []string{"visual-hive"},
	}
}

func repositoryTestJobJSON(id int64, name, conclusion string) string {
	steps := ""
	if strings.HasPrefix(name, repositoryTestJobPrefix) {
		steps = `,"steps":[` + strings.Join([]string{
			repositoryTestStepJSON(1, "Set up job", "completed", "success"),
			repositoryTestStepJSON(2, "Run actions/checkout", "completed", "success"),
			repositoryTestStepJSON(3, "Install repository test dependencies", "completed", "success"),
			repositoryTestStepJSON(4, repositoryTestCommandStep, "completed", conclusion),
			repositoryTestStepJSON(5, "Complete job", "completed", "success"),
		}, ",") + `]`
	}
	return fmt.Sprintf(`{"id":%d,"run_id":77,"name":%q,"head_sha":%q,"status":"completed","conclusion":%q,"workflow_name":%q%s}`, id, name, repositoryTestJobsHead, conclusion, repositoryTestJobsRunName, steps)
}

func repositoryTestJobWithStepsJSON(id int64, name, conclusion string, steps ...string) string {
	return fmt.Sprintf(`{"id":%d,"run_id":77,"name":%q,"head_sha":%q,"status":"completed","conclusion":%q,"workflow_name":%q,"steps":[%s]}`, id, name, repositoryTestJobsHead, conclusion, repositoryTestJobsRunName, strings.Join(steps, ","))
}

func repositoryTestStepJSON(number int64, name, status, conclusion string) string {
	return fmt.Sprintf(`{"number":%d,"name":%q,"status":%q,"conclusion":%q}`, number, name, status, conclusion)
}

func repositoryTestJobsRowCount(pages [][]string) int {
	total := 0
	for _, page := range pages {
		total += len(page)
	}
	return total
}

func TestVerifyRepositoryTestJobsBindsPaginatedRunnerOutcomes(t *testing.T) {
	fixture := newRepositoryTestJobsFixture(t)
	defer fixture.server.Close()
	client := NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())

	evidence, err := client.VerifyRepositoryTestJobs(context.Background(), fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Repository != "owner/repo" || evidence.WorkflowRunID != 77 || evidence.WorkflowID != 12 || evidence.HeadSHA != repositoryTestJobsHead || evidence.VisualJobID != 700 ||
		evidence.RequiredSuccessfulJobIDs["visual-hive-production"] != 700 || evidence.RequiredSuccessfulJobIDs["visual-hive"] != 703 {
		t.Fatalf("unexpected bound job evidence: %+v", evidence)
	}
	if len(evidence.Results) != 2 || evidence.Results[0].Command != "npm test" || evidence.Results[0].ExitCode != 0 || evidence.Results[1].Command != "python -m pytest -q" || evidence.Results[1].ExitCode != 1 || evidence.Overall != 1 || len(evidence.EvidenceDigest) != 64 {
		t.Fatalf("unexpected canonical results: %+v", evidence)
	}
	if evidence.PagesRead != 2 || evidence.JobsScanned != 5 || fmt.Sprint(fixture.requestedPages) != "[1 2]" || fmt.Sprint(fixture.requestedFilters) != "[all all]" || fmt.Sprint(fixture.requestedPerPages) != "[100 100]" {
		t.Fatalf("pagination/filter binding = pages=%v filters=%v per_page=%v evidence=%+v", fixture.requestedPages, fixture.requestedFilters, fixture.requestedPerPages, evidence)
	}
}

func TestVerifyRepositoryTestJobsAcceptsZeroCommandPlanWithoutNamespacedJobs(t *testing.T) {
	fixture := newRepositoryTestJobsFixture(t)
	defer fixture.server.Close()
	fixture.pages = [][]string{{repositoryTestJobJSON(700, "visual-hive-production", "success"), repositoryTestJobJSON(703, "visual-hive", "success"), repositoryTestJobJSON(704, "bundle", "success")}}
	request := fixture.request()
	request.Commands = nil
	client := NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())

	evidence, err := client.VerifyRepositoryTestJobs(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Results) != 0 || evidence.Overall != 0 || len(evidence.EvidenceDigest) != 64 {
		t.Fatalf("zero-command evidence = %+v", evidence)
	}
}

func TestVerifyRepositoryTestJobsRejectsJobTopologyAndConclusions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*repositoryTestJobsFixture)
		want   string
	}{
		{name: "missing test job", mutate: func(f *repositoryTestJobsFixture) { f.pages[1] = f.pages[1][1:] }, want: "missing job"},
		{name: "duplicate test job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1] = append(f.pages[1], repositoryTestJobJSON(705, "Hive repository test 001", "success"))
		}, want: "duplicated job"},
		{name: "extra namespaced job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1] = append(f.pages[1], repositoryTestJobJSON(705, "Hive repository test 003", "success"))
		}, want: "unexpected namespaced job"},
		{name: "cancelled test job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1][0] = repositoryTestJobJSON(702, "Hive repository test 002", "cancelled")
		}, want: "non-deterministic conclusion"},
		{name: "red visual job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[0][0] = repositoryTestJobJSON(700, "visual-hive-production", "failure")
		}, want: "exactly one successful required job"},
		{name: "duplicate visual job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1] = append(f.pages[1], repositoryTestJobJSON(705, "visual-hive-production", "success"))
		}, want: "exactly one successful required job"},
		{name: "rerun duplicates exact job identities", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1] = append(f.pages[1],
				repositoryTestJobJSON(705, "visual-hive-production", "success"),
				repositoryTestJobJSON(706, "visual-hive", "success"),
				repositoryTestJobJSON(707, "Hive repository test 001", "success"),
			)
		}, want: "exactly one successful required job"},
		{name: "missing seed job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1] = append(f.pages[1][:1], f.pages[1][2:]...)
		}, want: "missing required successful job"},
		{name: "red seed job", mutate: func(f *repositoryTestJobsFixture) {
			f.pages[1][1] = repositoryTestJobJSON(703, "visual-hive", "failure")
		}, want: "exactly one successful required job"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRepositoryTestJobsFixture(t)
			defer fixture.server.Close()
			test.mutate(fixture)
			client := NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := client.VerifyRepositoryTestJobs(context.Background(), fixture.request()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid job topology was accepted: %v", err)
			}
		})
	}
}

func TestVerifyRepositoryTestJobsRejectsInfrastructureAndStepAmbiguity(t *testing.T) {
	validSetup := func() []string {
		return []string{
			repositoryTestStepJSON(1, "Set up job", "completed", "success"),
			repositoryTestStepJSON(2, "Install repository test dependencies", "completed", "success"),
		}
	}
	for _, test := range []struct {
		name       string
		conclusion string
		steps      func() []string
		want       string
	}{
		{name: "missing command step", conclusion: "success", steps: func() []string {
			return append(validSetup(), repositoryTestStepJSON(3, "Complete job", "completed", "success"))
		}, want: "exact command step is missing"},
		{name: "duplicate command step", conclusion: "success", steps: func() []string {
			return append(validSetup(),
				repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "success"),
				repositoryTestStepJSON(4, repositoryTestCommandStep, "completed", "success"))
		}, want: "exact command step is duplicated"},
		{name: "skipped command step", conclusion: "failure", steps: func() []string {
			return append(validSetup(), repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "skipped"))
		}, want: "non-deterministic conclusion"},
		{name: "incomplete command step", conclusion: "failure", steps: func() []string {
			return append(validSetup(), repositoryTestStepJSON(3, repositoryTestCommandStep, "in_progress", ""))
		}, want: "is not completed"},
		{name: "setup failure", conclusion: "failure", steps: func() []string {
			return []string{
				repositoryTestStepJSON(1, "Set up job", "completed", "success"),
				repositoryTestStepJSON(2, "Install repository test dependencies", "completed", "failure"),
				repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "failure"),
			}
		}, want: "infrastructure step"},
		{name: "post cleanup failure", conclusion: "failure", steps: func() []string {
			return append(validSetup(),
				repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "success"),
				repositoryTestStepJSON(4, "Post Run actions/checkout", "completed", "failure"))
		}, want: "infrastructure step"},
		{name: "job green but command red", conclusion: "success", steps: func() []string {
			return append(validSetup(), repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "failure"))
		}, want: "does not match exact command conclusion"},
		{name: "job red but command green", conclusion: "failure", steps: func() []string {
			return append(validSetup(), repositoryTestStepJSON(3, repositoryTestCommandStep, "completed", "success"))
		}, want: "does not match exact command conclusion"},
		{name: "unordered step identities", conclusion: "success", steps: func() []string {
			return []string{
				repositoryTestStepJSON(2, "Set up job", "completed", "success"),
				repositoryTestStepJSON(1, repositoryTestCommandStep, "completed", "success"),
			}
		}, want: "strictly ordered identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRepositoryTestJobsFixture(t)
			defer fixture.server.Close()
			fixture.pages[0][1] = repositoryTestJobWithStepsJSON(701, "Hive repository test 001", test.conclusion, test.steps()...)
			client := NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := client.VerifyRepositoryTestJobs(context.Background(), fixture.request()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ambiguous repository-test step evidence was accepted: %v", err)
			}
		})
	}
}

func TestVerifyRepositoryTestJobsRejectsRunAndDefinitionSpoofing(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*repositoryTestJobsFixture)
		want   string
	}{
		{name: "wrong event", mutate: func(f *repositoryTestJobsFixture) { f.runEvent = "pull_request" }, want: "workflow_dispatch binding"},
		{name: "wrong head", mutate: func(f *repositoryTestJobsFixture) { f.runHead = strings.Repeat("b", 40) }, want: "workflow_dispatch binding"},
		{name: "run name diverges from job workflow", mutate: func(f *repositoryTestJobsFixture) { f.runName = "spoof" }, want: "run/head/workflow binding"},
		{name: "wrong run path", mutate: func(f *repositoryTestJobsFixture) { f.runPath = ".github/workflows/other.yml" }, want: "workflow_dispatch binding"},
		{name: "disabled definition", mutate: func(f *repositoryTestJobsFixture) { f.definitionState = "disabled_manually" }, want: "active installed workflow"},
		{name: "wrong definition name", mutate: func(f *repositoryTestJobsFixture) { f.definitionName = "spoof" }, want: "active installed workflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRepositoryTestJobsFixture(t)
			defer fixture.server.Close()
			test.mutate(fixture)
			client := NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := client.VerifyRepositoryTestJobs(context.Background(), fixture.request()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("spoofed provenance was accepted: %v", err)
			}
		})
	}
}

func TestVerifyRepositoryTestJobsRejectsUnsafeOrDuplicateCommandIdentityBeforeAPI(t *testing.T) {
	client := NewClientForTest("http://127.0.0.1:1", "owner", []string{"repo"}, slog.Default())
	for _, commands := range [][][]string{
		{{"npm", "test", "&&", "curl", "example.test"}},
		{{"npm", "test"}, {"npm", "test"}},
		{{}},
	} {
		request := RepositoryTestJobsRequest{
			Repository: "owner/repo", WorkflowRunID: 77, ExpectedHeadSHA: repositoryTestJobsHead,
			ExpectedWorkflowName: "Hive Visual Hive Production", ExpectedWorkflowPath: ".github/workflows/hive-visual-hive.yml",
			Commands: commands, VisualJobName: "visual-hive-production",
		}
		if _, err := client.VerifyRepositoryTestJobs(context.Background(), request); err == nil {
			t.Fatalf("unsafe command plan was accepted: %v", commands)
		}
	}
}
