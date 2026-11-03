package github

import (
	"context"
	"strings"
	"testing"
)

// `/hive approve` accepts the recommendation a park names. On
// projectbluefin/server#385 the park ended "maintainers should decide
// whether to adopt this tooling or keep the existing model": no
// recommendation, so approve has nothing to accept and must not un-park the
// issue with the decision "go with the recommendation".

func approveOn(t *testing.T, body string) (*IssueUnparkSweepResult, *unparkServer) {
	t.Helper()
	issue := unparkWireIssue{
		Number:   9,
		Title:    "parked",
		Body:     body,
		User:     wireUser{Login: "reporter"},
		Labels:   []wireLabel{{Name: "stage/045-needs-decision"}},
		Comments: []unparkWireComment{humanComment(31, "maintainer", "/hive approve")},
	}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result, rec
}

func TestApproveWithoutRecommendationKeepsThePark(t *testing.T) {
	result, rec := approveOn(t, "Maintainers should decide whether to adopt this tooling or keep the existing model.")
	if len(result.Unparked) != 0 || len(rec.removed[9]) != 0 || len(rec.added[9]) != 0 {
		t.Fatalf("approve with nothing to approve un-parked: unparked=%+v removed=%v added=%v", result.Unparked, rec.removed[9], rec.added[9])
	}
	var reply string
	for _, body := range rec.posted[9] {
		if strings.Contains(body, unparkReplyMarkerPrefix) {
			reply = body
		}
	}
	for _, want := range []string{"names none", "stays parked", "`/hive decision <your instructions>`"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q: %v", want, rec.posted[9])
		}
	}
	if strings.Contains(reply, unparkAcceptedMarker) {
		t.Fatalf("refusal carries the accepted marker:\n%s", reply)
	}
	// The notice offers no approve here either, so the two agree.
	for _, body := range rec.posted[9] {
		if strings.Contains(body, unparkNoticeMarker) && strings.Contains(body, "/hive approve") {
			t.Fatalf("notice offers /hive approve with no recommendation:\n%s", body)
		}
	}
}

func TestApproveRecordsWhatTheMenuOffers(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"options, no recommendation", "Option A: keep the pin\nOption B: drop it\n", "go with option A: keep the pin"},
		{"recommended option", "Option A: keep the pin\nOption B: drop it\n\nRecommendation: option B.\n", "go with the recommendation (option B: drop it)"},
		{"recommendation, no options", "We recommend pinning 3.14 as the floor.", "go with the recommendation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := approveOn(t, tc.body)
			if len(result.Unparked) != 1 || result.Unparked[0].Decision != tc.want {
				t.Fatalf("decision = %+v, want %q", result.Unparked, tc.want)
			}
		})
	}
}
