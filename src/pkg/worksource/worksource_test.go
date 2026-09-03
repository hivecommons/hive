package worksource_test

import (
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

func TestTaskKey_GitHub(t *testing.T) {
	issue := worksource.Issue{
		SourceType: "github",
		Repo:       "my-org/my-repo",
		Number:     42,
		ExternalID: "42",
	}
	got := worksource.TaskKey(issue)
	want := "my-org/my-repo#42"
	if got != want {
		t.Errorf("TaskKey = %q, want %q", got, want)
	}
}

func TestTaskKey_Linear(t *testing.T) {
	issue := worksource.Issue{
		SourceType: "linear",
		Repo:       "my-org/my-repo",
		ExternalID: "ENG-123",
	}
	got := worksource.TaskKey(issue)
	want := "my-org/my-repo!ENG-123"
	if got != want {
		t.Errorf("TaskKey = %q, want %q", got, want)
	}
}

func TestTaskKey_NoCollision(t *testing.T) {
	// Two teams both have issue 42 — keys must be distinct.
	eng := worksource.Issue{SourceType: "linear", Repo: "org/repo", ExternalID: "ENG-42"}
	ops := worksource.Issue{SourceType: "linear", Repo: "org/repo", ExternalID: "OPS-42"}
	if worksource.TaskKey(eng) == worksource.TaskKey(ops) {
		t.Errorf("TaskKey collision: ENG-42 and OPS-42 produced the same key %q", worksource.TaskKey(eng))
	}
}

func TestTaskKey_Jira(t *testing.T) {
	issue := worksource.Issue{
		SourceType: "jira",
		Repo:       "my-org/my-repo",
		ExternalID: "ENG-42",
	}
	got := worksource.TaskKey(issue)
	want := "my-org/my-repo!ENG-42"
	if got != want {
		t.Errorf("TaskKey = %q, want %q", got, want)
	}
}
