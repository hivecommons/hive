package scheduler

import (
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestKickSkipsExplainIdleLaneAndRecheckForNewWork(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerNoneEscalatedFixture)
	target := config.CadenceTargetKey("adjudicator", "test-org/console")
	for i := 0; i < 2; i++ {
		messages, skipped := s.BuildKickMessagesWithSkips(&github.ActionableResult{}, []string{target, "scanner"})
		if len(messages) != 1 || messages[0].Agent != "scanner" {
			t.Fatalf("idle lane should leave scanner's kick intact: %+v", messages)
		}
		if len(skipped) != 1 || skipped[0] != target+" (nothing to adjudicate)" {
			t.Fatalf("skips = %v, want scoped adjudicator idle reason", skipped)
		}
	}
	if err := os.WriteFile(ciFailingPath, []byte(reviewerOneEscalatedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	messages, skipped := s.BuildKickMessagesWithSkips(&github.ActionableResult{}, []string{target})
	if len(skipped) != 0 || len(messages) != 1 || messages[0].Agent != "adjudicator" {
		t.Fatalf("new adjudication work must be eligible immediately: messages=%+v skips=%v", messages, skipped)
	}
}
