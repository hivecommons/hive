package scheduler

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestKickSummaryRefs(t *testing.T) {
	s := &Scheduler{}
	s.recordKickSummary("developer", []github.Issue{
		{Repo: "o/r", Number: 1, Title: "one"},
		{Number: 2, Title: "no repo"},
		{Repo: "o/r", Number: 3, Title: "three"},
	})
	got, ok := s.KickSummary("developer")
	if !ok {
		t.Fatal("no kick summary")
	}
	if want := []string{"o/r#1", "o/r#3"}; !reflect.DeepEqual(got.Refs, want) {
		t.Fatalf("refs = %v, want %v", got.Refs, want)
	}
}
