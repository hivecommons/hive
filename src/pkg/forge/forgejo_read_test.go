package forge

import (
	"context"
	"reflect"
	"testing"
)

func TestForgejoReadAPI(t *testing.T) {
	sweep, _ := newSweepAPIServer(t, map[string]string{
		"GET " + repoAPI + "/pulls/7": `{"number":7,"title":"Add x","body":"Fixes #1","state":"open","draft":true,"mergeable":true,
			"html_url":"https://fj.example/acme/widgets/pulls/7","user":{"login":"hive-bot"},"labels":[{"name":"lgtm"}],
			"head":{"ref":"feat","sha":"abc"},"base":{"ref":"main"}}`,
		"GET " + repoAPI + "/issues/1": `{"number":1,"title":"Bug","body":"broken","state":"open","html_url":"https://fj.example/acme/widgets/issues/1",
			"user":{"login":"reporter"},"labels":[{"name":"bug"}],"assignees":[{"login":"a1"}]}`,
		"GET " + repoAPI + "/commits/abc/status": `{"state":"failure","total_count":2,"statuses":[
			{"context":"ci/build","status":"success","target_url":"u1"},{"context":"ci/test","status":"failure","description":"2 failed"}]}`,
	})
	api, ok := sweep.(ReadAPI)
	if !ok {
		t.Fatal("the Forgejo adapter does not implement ReadAPI")
	}
	ctx := context.Background()

	pr, err := api.PullView(ctx, "widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := PullView{Number: 7, Title: "Add x", Body: "Fixes #1", State: "open", Draft: true, Mergeable: true, Author: "hive-bot",
		Labels: []string{"lgtm"}, HeadRef: "feat", HeadSHA: "abc", Base: "main", URL: "https://fj.example/acme/widgets/pulls/7"}
	if !reflect.DeepEqual(pr, want) {
		t.Errorf("PullView = %+v, want %+v", pr, want)
	}

	is, err := api.IssueView(ctx, "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "Bug" || is.Body != "broken" || is.Author != "reporter" || is.IsPull ||
		!reflect.DeepEqual(is.Labels, []string{"bug"}) || !reflect.DeepEqual(is.Assignees, []string{"a1"}) {
		t.Errorf("IssueView = %+v", is)
	}

	state, statuses, err := api.CommitStatuses(ctx, "widgets", "abc")
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []CommitStatus{{Context: "ci/build", State: "success", TargetURL: "u1"}, {Context: "ci/test", State: "failure", Description: "2 failed"}}
	if state != "failure" || !reflect.DeepEqual(statuses, wantStatuses) {
		t.Errorf("CommitStatuses = %q %+v", state, statuses)
	}
	if _, _, err := api.CommitStatuses(ctx, "widgets", " "); err == nil {
		t.Error("CommitStatuses with no sha: want an error")
	}
	if _, err := api.PullView(ctx, "widgets", 99); err == nil {
		t.Error("PullView of a missing pull request: want an error")
	}
}
