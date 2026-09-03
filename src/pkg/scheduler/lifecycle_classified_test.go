package scheduler

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/timeline"
)

// TestBuildKickMessagesRecordsClassifiedJourneys pins the classified-stage
// producer (#5656): the classifier pass inside BuildKickMessages is where lane
// routing decides an issue's lane, so that is where KindClassified must land —
// with the lane/tier the routing actually used.
func TestBuildKickMessagesRecordsClassifiedJourneys(t *testing.T) {
	s := workTrackerScheduler(t, "github", "BODY\n${ISSUE_LIST}")
	store := timeline.NewStore()
	s.SetLifecycleRecorder(store)

	issues := []github.Issue{
		{Repo: "acme/widget", Number: 1, Title: "fix typo in docs"},
		{Repo: "acme/widget", Number: 2, Title: "redesign the scheduler"},
	}
	s.BuildKickMessages(&github.ActionableResult{Issues: github.IssueResult{Items: issues}}, []string{"quality"})

	journeys := store.Journeys(0)
	if len(journeys) != 2 {
		t.Fatalf("journeys = %d, want 2 classified", len(journeys))
	}
	j, ok := store.Journey("acme/widget#1")
	if !ok {
		t.Fatalf("classified journey missing: %+v", journeys)
	}
	st := j.Stages[timeline.KindClassified]
	if st == nil {
		t.Fatal("classified stage missing")
	}
	if st.Attrs["lane"] == "" || st.Attrs["tier"] == "" {
		t.Fatalf("classified stage must carry the routing decision, got attrs %v", st.Attrs)
	}

	// A second pass (next eval cycle) refreshes rather than duplicating.
	s.BuildKickMessages(&github.ActionableResult{Issues: github.IssueResult{Items: issues}}, []string{"quality"})
	if got := len(store.Journeys(0)); got != 2 {
		t.Fatalf("re-classification made %d journeys, want still 2", got)
	}
}

// TestRecordClassifiedNilRecorderIsNoOp: an unwired scheduler classifies
// silently — no panic, no requirement to attach the timeline.
func TestRecordClassifiedNilRecorderIsNoOp(t *testing.T) {
	s := workTrackerScheduler(t, "github", "BODY\n${ISSUE_LIST}")
	s.recordClassified([]github.Issue{{Repo: "acme/widget", Number: 3}})
}
