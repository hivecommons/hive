package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type gqlCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func decodeGQL(t *testing.T, r *http.Request) gqlCall {
	t.Helper()
	var c gqlCall
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		t.Errorf("decode graphql request: %v", err)
	}
	return c
}

// fakeIssueSink stands in for the GitHub issue sink under the Projects sink.
type fakeIssueSink struct {
	number  int
	err     error
	appends []string
}

func (f *fakeIssueSink) Destination() string { return config.ReviewBacklogGitHubIssue }

func (f *fakeIssueSink) CreateItem(context.Context, github.ReviewBacklogItem) (github.ReviewBacklogRef, error) {
	if f.err != nil {
		return github.ReviewBacklogRef{}, f.err
	}
	return github.ReviewBacklogRef{ID: "77", Key: "#77", Number: f.number, URL: "https://github.test/o/r/issues/77"}, nil
}

func (f *fakeIssueSink) AppendToItem(_ context.Context, _ string, _ github.ReviewBacklogRef, comment string) error {
	f.appends = append(f.appends, comment)
	return nil
}

func TestProjectBacklogSinkAddsIssueToColumn(t *testing.T) {
	var ops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		c := decodeGQL(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(c.Query, "repository(owner"):
			ops = append(ops, "lookup")
			if c.Variables["org"] != "acme" || c.Variables["number"] != float64(3) || c.Variables["owner"] != "o" || c.Variables["repo"] != "r" || c.Variables["issue"] != float64(77) {
				t.Errorf("lookup vars = %v", c.Variables)
			}
			_, _ = io.WriteString(w, `{"data":{"organization":{"projectV2":{"id":"PVT_1","field":{"id":"FLD_status"}}},"repository":{"issue":{"id":"I_77"}}}}`)
		case strings.Contains(c.Query, "addProjectV2ItemById"):
			ops = append(ops, "add")
			if c.Variables["project"] != "PVT_1" || c.Variables["content"] != "I_77" {
				t.Errorf("add vars = %v", c.Variables)
			}
			_, _ = io.WriteString(w, `{"data":{"addProjectV2ItemById":{"item":{"id":"PVTI_9"}}}}`)
		case strings.Contains(c.Query, "updateProjectV2ItemFieldValue"):
			ops = append(ops, "status")
			if c.Variables["item"] != "PVTI_9" || c.Variables["field"] != "FLD_status" || c.Variables["option"] != "opt-todo" {
				t.Errorf("status vars = %v", c.Variables)
			}
			_, _ = io.WriteString(w, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_9"}}}}`)
		default:
			t.Errorf("unexpected query %q", c.Query)
		}
	}))
	defer srv.Close()
	projects := NewGitHubProjectsSource(GitHubProjectsConfig{Token: "tok", Org: "acme", ProjectNumber: 3, BaseURL: srv.URL}).(*githubProjectsSource)
	issues := &fakeIssueSink{number: 77}
	sink := &projectBacklogSink{issues: issues, projects: projects, columnID: "opt-todo"}
	if sink.Destination() != config.ReviewBacklogGitHubProject {
		t.Fatalf("destination = %q", sink.Destination())
	}
	ref, err := sink.CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r", Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if ref.Number != 77 || strings.Join(ops, ",") != "lookup,add,status" {
		t.Fatalf("ref=%+v ops=%v", ref, ops)
	}
	if err := sink.AppendToItem(context.Background(), "o/r", ref, "again"); err != nil || len(issues.appends) != 1 {
		t.Fatalf("append err=%v appends=%v", err, issues.appends)
	}

	if _, err := projects.AddIssueToColumn(context.Background(), "o", "r", 77, " "); err == nil {
		t.Fatal("empty column id must fail")
	}
	issues.err = errors.New("boom")
	if _, err := sink.CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r"}); err == nil {
		t.Fatal("issue create failure must be returned")
	}
}

func TestGitHubProjectsAddIssueToColumnErrors(t *testing.T) {
	cases := map[string]string{
		"no project": `{"data":{"organization":{"projectV2":null},"repository":{"issue":{"id":"I"}}}}`,
		"no field":   `{"data":{"organization":{"projectV2":{"id":"P","field":null}},"repository":{"issue":{"id":"I"}}}}`,
		"no issue":   `{"data":{"organization":{"projectV2":{"id":"P","field":{"id":"F"}}},"repository":{"issue":null}}}`,
		"gql error":  `{"errors":[{"message":"nope"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			p := NewGitHubProjectsSource(GitHubProjectsConfig{Org: "acme", ProjectNumber: 1, BaseURL: srv.URL}).(*githubProjectsSource)
			if _, err := p.AddIssueToColumn(context.Background(), "o", "r", 1, "opt"); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLinearBacklogSinkCreatesInStateAndComments(t *testing.T) {
	var created map[string]any
	var commented gqlCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		c := decodeGQL(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(c.Query, "states(first"):
			if c.Variables["key"] != "ENG" {
				t.Errorf("team key = %v", c.Variables["key"])
			}
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"team-1","key":"ENG",
				"states":{"nodes":[{"id":"st-todo","name":"Todo"},{"id":"st-backlog","name":"Backlog"}]},
				"labels":{"nodes":[{"id":"lb-nit","name":"nit"},{"id":"lb-fr","name":"from-review"}]}}]}}}`)
		case strings.Contains(c.Query, "issueCreate"):
			created, _ = c.Variables["input"].(map[string]any)
			_, _ = io.WriteString(w, `{"data":{"issueCreate":{"success":true,"issue":{"id":"uuid-1","identifier":"ENG-12","number":12,"url":"https://linear.test/ENG-12","title":"t"}}}}`)
		case strings.Contains(c.Query, "commentCreate"):
			commented = c
			_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1"}}}}`)
		default:
			t.Errorf("unexpected query %q", c.Query)
		}
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "lin-key", BaseURL: srv.URL, Teams: []LinearTeamConfig{{Key: "OPS", Repo: "o/other"}, {Key: "ENG", Repo: "o/r"}}}, srv.Client())
	sink := &linearBacklogSink{src: src, state: "backlog"}
	if sink.Destination() != config.ReviewBacklogLinear {
		t.Fatalf("destination = %q", sink.Destination())
	}
	ref, err := sink.CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r", Title: "t", Body: "body\nfiled by Hive review backlog", Labels: []string{"NIT", "missing"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if ref.ID != "uuid-1" || ref.Key != "ENG-12" || ref.URL != "https://linear.test/ENG-12" || ref.Number != 0 {
		t.Fatalf("ref = %+v", ref)
	}
	if created["teamId"] != "team-1" || created["stateId"] != "st-backlog" || !strings.Contains(created["description"].(string), "filed by Hive review backlog") {
		t.Fatalf("issueCreate input = %v", created)
	}
	if ids, _ := created["labelIds"].([]any); len(ids) != 1 || ids[0] != "lb-nit" {
		t.Fatalf("labelIds = %v, want only the existing nit label", created["labelIds"])
	}
	if err := sink.AppendToItem(context.Background(), "o/r", ref, "raised again"); err != nil {
		t.Fatalf("AppendToItem: %v", err)
	}
	if commented.Variables["issueId"] != "uuid-1" || commented.Variables["body"] != "raised again" {
		t.Fatalf("commentCreate vars = %v", commented.Variables)
	}

	// Unknown state is an error naming it; empty state uses the team default.
	if _, err := (&linearBacklogSink{src: src, state: "Nope"}).CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r", Title: "t"}); err == nil || !strings.Contains(err.Error(), `"Nope"`) {
		t.Fatalf("unknown state err = %v", err)
	}
	if _, err := (&linearBacklogSink{src: src}).CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r", Title: "t"}); err != nil {
		t.Fatalf("default state: %v", err)
	}
	if _, ok := created["stateId"]; ok {
		t.Fatalf("empty linear_state must not set stateId: %v", created)
	}
}

func TestLinearBacklogErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decodeGQL(t, r)
		switch {
		case strings.Contains(c.Query, "states(first"):
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[]}}}`)
		case strings.Contains(c.Query, "commentCreate"):
			_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":false}}}`)
		}
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "k", BaseURL: srv.URL, Teams: []LinearTeamConfig{{Key: "ENG", Repo: "o/r"}}}, srv.Client())
	if _, err := src.CreateIssueInState(context.Background(), "ENG", "t", "d", "", nil); err == nil {
		t.Fatal("missing team must fail")
	}
	if _, err := src.CreateIssueInState(context.Background(), " ", "t", "d", "", nil); err == nil {
		t.Fatal("blank team key must fail")
	}
	if err := src.CommentOnIssue(context.Background(), "uuid", "x"); err == nil {
		t.Fatal("success=false must fail")
	}
	if err := src.CommentOnIssue(context.Background(), "", "x"); err == nil {
		t.Fatal("blank issue id must fail")
	}
	noTeams := &linearBacklogSink{src: NewLinearSource(LinearConfig{}, nil)}
	if _, err := noTeams.CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r"}); err == nil {
		t.Fatal("no teams must fail")
	}
}

func TestJiraBacklogSinkCreatesTransitionsAndComments(t *testing.T) {
	var createBody, commentBody map[string]any
	var transitioned string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "me@example.com" || p != "tok" {
			t.Errorf("basic auth = %q/%q/%v", u, p, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"1001","key":"ENG-5","self":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/issue/ENG-5/transitions":
			_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress"}},{"id":"21","name":"Park","to":{"name":"Backlog"}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/ENG-5/transitions":
			var body struct {
				Transition struct {
					ID string `json:"id"`
				} `json:"transition"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			transitioned = body.Transition.ID
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/ENG-5/comment":
			_ = json.NewDecoder(r.Body).Decode(&commentBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, Email: "me@example.com", APIToken: "tok", ProjectKeys: []string{"ENG", "OPS"}}).(*jiraSource)
	sink := &jiraBacklogSink{src: src, status: "backlog"}
	if sink.Destination() != config.ReviewBacklogJira {
		t.Fatalf("destination = %q", sink.Destination())
	}
	ref, err := sink.CreateItem(context.Background(), github.ReviewBacklogItem{Repo: "o/r", Title: "nit title", Body: "line one\n\nfiled by Hive review backlog", Labels: []string{"from review", "nit"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if ref.ID != "ENG-5" || ref.Key != "ENG-5" || ref.URL != srv.URL+"/browse/ENG-5" {
		t.Fatalf("ref = %+v", ref)
	}
	if transitioned != "21" {
		t.Fatalf("transition = %q, want 21 (to Backlog)", transitioned)
	}
	fields, _ := createBody["fields"].(map[string]any)
	if fields["summary"] != "nit title" || fields["project"].(map[string]any)["key"] != "ENG" || fields["issuetype"].(map[string]any)["name"] != "Task" {
		t.Fatalf("create fields = %v", fields)
	}
	if labels, _ := fields["labels"].([]any); len(labels) != 2 || labels[0] != "from-review" || labels[1] != "nit" {
		t.Fatalf("labels = %v", fields["labels"])
	}
	desc, _ := json.Marshal(fields["description"])
	if !strings.Contains(string(desc), `"type":"doc"`) || !strings.Contains(string(desc), "filed by Hive review backlog") || strings.Count(string(desc), `"paragraph"`) != 2 {
		t.Fatalf("cloud description = %s", desc)
	}
	if err := sink.AppendToItem(context.Background(), "o/r", ref, "raised again"); err != nil {
		t.Fatalf("AppendToItem: %v", err)
	}
	if b, _ := json.Marshal(commentBody); !strings.Contains(string(b), "raised again") {
		t.Fatalf("comment body = %s", b)
	}

	// Transition by name when no target status matches; unknown status fails
	// but still returns the created ref.
	if err := src.transitionToStatus(context.Background(), "ENG-5", "start"); err != nil || transitioned != "11" {
		t.Fatalf("by-name transition err=%v id=%q", err, transitioned)
	}
	ref, err = (&jiraBacklogSink{src: src, status: "Done"}).CreateItem(context.Background(), github.ReviewBacklogItem{Title: "t", Body: "b"})
	if err == nil || ref.Key != "ENG-5" {
		t.Fatalf("unknown status: ref=%+v err=%v", ref, err)
	}
	if err := src.transitionToStatus(context.Background(), "ENG-5", ""); err != nil {
		t.Fatalf("empty status must be a no-op: %v", err)
	}
}

func TestJiraBacklogDataCenterAndErrors(t *testing.T) {
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pat" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/rest/api/2/issue" {
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"key":""}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	src := NewJiraSource(JiraConfig{Deployment: "datacenter", BaseURL: srv.URL, APIToken: "pat", ProjectKeys: []string{"ENG"}}).(*jiraSource)
	if _, err := (&jiraBacklogSink{src: src}).CreateItem(context.Background(), github.ReviewBacklogItem{Title: "t", Body: "plain"}); err == nil {
		t.Fatal("missing key must fail")
	}
	if fields, _ := createBody["fields"].(map[string]any); fields["description"] != "plain" {
		t.Fatalf("data center description = %v, want plain text", createBody)
	}
	if _, err := src.createIssue(context.Background(), " ", "t", "d", nil); err == nil {
		t.Fatal("blank project key must fail")
	}
	none := NewJiraSource(JiraConfig{BaseURL: srv.URL}).(*jiraSource)
	if _, err := (&jiraBacklogSink{src: none}).CreateItem(context.Background(), github.ReviewBacklogItem{}); err == nil {
		t.Fatal("no project keys must fail")
	}
	if err := src.transitionToStatus(context.Background(), "ENG-1", "Backlog"); err == nil {
		t.Fatal("transition lookup failure must be returned")
	}
}

func TestNewReviewBacklogSinkPicksDestination(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gh := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"r"}, logger)

	sink, err := NewReviewBacklogSink(config.WorkSourceConfig{}, config.ReviewBacklogConfig{}, gh, "", "acme", logger)
	if err != nil || sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("default: %v %v", sink.Destination(), err)
	}
	// Destination whose work source is not active falls back silently.
	sink, err = NewReviewBacklogSink(config.WorkSourceConfig{Type: "github"}, config.ReviewBacklogConfig{Destination: "jira"}, gh, "", "acme", logger)
	if err != nil || sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("inactive jira: %v %v", sink.Destination(), err)
	}

	lin := config.WorkSourceConfig{Type: "linear"}
	lin.Linear.APIKey = "key"
	lin.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "o/r"}}
	lin.Linear.AssignedOnly = true // enumeration-only; must not block filing
	sink, err = NewReviewBacklogSink(lin, config.ReviewBacklogConfig{Destination: "linear", LinearState: "Backlog"}, gh, "", "acme", logger)
	if err != nil {
		t.Fatalf("linear: %v", err)
	}
	if ls, ok := sink.(*linearBacklogSink); !ok || ls.state != "Backlog" {
		t.Fatalf("linear sink = %T %+v", sink, sink)
	}

	jira := config.WorkSourceConfig{Type: "jira"}
	jira.Jira.BaseURL = "https://jira.test"
	jira.Jira.ProjectKeys = []string{"ENG"}
	sink, err = NewReviewBacklogSink(jira, config.ReviewBacklogConfig{Destination: "jira", JiraStatus: "To Do"}, gh, "", "acme", logger)
	if js, ok := sink.(*jiraBacklogSink); err != nil || !ok || js.status != "To Do" {
		t.Fatalf("jira sink = %T err=%v", sink, err)
	}

	proj := config.WorkSourceConfig{Type: "github_projects"}
	proj.GitHubProjects.ProjectNumber = 4
	sink, err = NewReviewBacklogSink(proj, config.ReviewBacklogConfig{Destination: "github_project", ProjectColumnID: "opt"}, gh, "tok", "acme", logger)
	if ps, ok := sink.(*projectBacklogSink); err != nil || !ok || ps.columnID != "opt" || ps.projects.cfg.Org != "acme" {
		t.Fatalf("project sink = %T err=%v", sink, err)
	}
	sink, err = NewReviewBacklogSink(proj, config.ReviewBacklogConfig{Destination: "github_project"}, gh, "tok", "acme", logger)
	if err == nil || sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("project without column: %v %v", sink.Destination(), err)
	}

	// Active but unbuildable work source: GitHub fallback plus the error.
	broken := config.WorkSourceConfig{Type: "linear"}
	sink, err = NewReviewBacklogSink(broken, config.ReviewBacklogConfig{Destination: "linear"}, gh, "", "acme", logger)
	if err == nil || sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("broken linear: %v %v", sink.Destination(), err)
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	if o, r := splitOwnerRepo("o/r", "acme"); o != "o" || r != "r" {
		t.Fatalf("got %s/%s", o, r)
	}
	if o, r := splitOwnerRepo("r", "acme"); o != "acme" || r != "r" {
		t.Fatalf("got %s/%s", o, r)
	}
}
