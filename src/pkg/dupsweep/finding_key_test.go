package dupsweep

import (
	"testing"

	"github.com/hivecommons/hive/pkg/findingidentity"
)

func findingKey(subject, predicate, location string) string {
	return findingidentity.Key(findingidentity.Record{
		SubjectDigest: subject,
		Predicate:     predicate,
		Location:      location,
	})
}

func TestFindClustersFindingIdentityAcrossLineNumbers(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"src/a.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"src/a.go"}},
	}
	keys := map[int]string{
		1: findingKey("sha256:abc", "hive.audit.missing-guard/v1", "src/a.go:12"),
		2: findingKey("sha256:abc", "hive.audit.missing-guard/v1", "src/a.go:98"),
	}
	got := Find(prs, Options{FindingKey: func(pr PR) string { return keys[pr.Number] }})
	if len(got) != 1 {
		t.Fatalf("want 1 identity cluster, got %d", len(got))
	}
	if got[0].Confidence != ConfidenceFindingIdentity {
		t.Fatalf("confidence = %q, want %q", got[0].Confidence, ConfidenceFindingIdentity)
	}
	if got[0].FindingKey == "" {
		t.Fatal("identity cluster did not retain its finding key")
	}
}

func TestFindDoesNotClusterDifferentPredicates(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"src/a.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"src/b.go"}},
	}
	keys := map[int]string{
		1: findingKey("sha256:abc", "hive.audit.missing-guard/v1", "src/a.go:12"),
		2: findingKey("sha256:abc", "hive.audit.bad-retry/v1", "src/a.go:98"),
	}
	if got := Find(prs, Options{FindingKey: func(pr PR) string { return keys[pr.Number] }}); len(got) != 0 {
		t.Fatalf("different predicates must not cluster, got %+v", got)
	}
}

func TestUncorroboratedFileSetClusterStaysUnknown(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"src/a.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"src/a.go"}},
	}
	got := Find(prs, Options{})
	if len(got) != 1 {
		t.Fatalf("want uncertain cluster surfaced for review, got %d", len(got))
	}
	if got[0].Confidence != ConfidenceUnknown {
		t.Fatalf("confidence = %q, want %q", got[0].Confidence, ConfidenceUnknown)
	}
}
