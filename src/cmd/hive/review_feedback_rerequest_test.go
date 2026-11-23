package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// rerequestingFeedbackSource is a fakeFeedbackSource that can also
// re-request reviews, recording each (reviewer, head).
type rerequestingFeedbackSource struct {
	fakeFeedbackSource
	rerequests []string
	err        error
}

func (f *rerequestingFeedbackSource) RerequestReview(_ context.Context, _ string, _ int, reviewer, head string) error {
	if f.err != nil {
		return f.err
	}
	f.rerequests = append(f.rerequests, reviewer+"@"+head)
	return nil
}

func addressedFeedback(head string, rerequested bool) github.PRFeedback {
	return github.PRFeedback{State: "open", HeadSHA: head, Source: "graphql", AddressedChangeRequests: []github.AddressedChangeRequest{{
		Reviewer: "Danathar", ReviewID: "review:900", ReviewURL: "https://github.com/acme/widget/pull/7#pullrequestreview-900",
		Points: []github.AddressedPoint{
			{ID: "review:900", Excerpt: "please drop the privileged flag", ReplyURL: "https://github.com/acme/widget/pull/7#issuecomment-1"},
			{ID: "inline:901", Path: "sandbox/podman.go", Line: 42, Excerpt: "check the containment posture"},
		},
		Rerequested: rerequested,
	}}}
}

// A change request the hive fully addressed re-requests the reviewer once
// per head and posts one note listing what changed; a second pass on the
// same head does neither again, and a new head acts once more.
func TestReviewFeedbackSweep_RerequestsAddressedReviewerOncePerHead(t *testing.T) {
	src := &rerequestingFeedbackSource{fakeFeedbackSource: fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: addressedFeedback("abc1234def", false)}}}
	s := feedbackSweepFor(t, &src.fakeFeedbackSource, &recordingKicker{}, routeTo("quality"), time.Now())
	s.Source = src
	s.Rerequests = &reviewRerequestMemory{}

	res := s.run(context.Background(), parseReviewFeedbackKey)
	if res.Rerequested != 1 || len(src.rerequests) != 1 || src.rerequests[0] != "Danathar@abc1234def" {
		t.Fatalf("res=%+v rerequests=%v, want one re-request of Danathar on the head", res, src.rerequests)
	}
	if len(src.comments) != 1 {
		t.Fatalf("notes = %d, want 1", len(src.comments))
	}
	note := src.comments[0]
	for _, want := range []string{github.ReviewRerequestedMarker("Danathar", "abc1234def"), "please drop the privileged flag", "sandbox/podman.go:42", "never dismisses"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "@Danathar") {
		t.Errorf("note must not @-mention the reviewer:\n%s", note)
	}

	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 0 || len(src.rerequests) != 1 || len(src.comments) != 1 {
		t.Fatalf("second pass on the same head acted again: res=%+v rerequests=%v", res, src.rerequests)
	}

	src.byKey[feedbackTestKey] = addressedFeedback("fff0000aaa", false)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 1 || len(src.rerequests) != 2 {
		t.Fatalf("new head: res=%+v rerequests=%v, want a second re-request", res, src.rerequests)
	}
}

// The marker already on the PR for this head (a restarted process) stops the
// re-request; a failed re-request posts no note and is retried.
func TestReviewFeedbackSweep_RerequestMarkerAndFailure(t *testing.T) {
	src := &rerequestingFeedbackSource{fakeFeedbackSource: fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: addressedFeedback("abc1234def", true)}}}
	s := feedbackSweepFor(t, &src.fakeFeedbackSource, &recordingKicker{}, routeTo("quality"), time.Now())
	s.Source = src
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 0 || len(src.rerequests) != 0 || len(src.comments) != 0 {
		t.Fatalf("marker on the head must stop the re-request: res=%+v rerequests=%v", res, src.rerequests)
	}

	src.byKey[feedbackTestKey] = addressedFeedback("abc1234def", false)
	src.err = errors.New("boom")
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 0 || len(src.comments) != 0 {
		t.Fatalf("failed re-request must post no note: res=%+v comments=%v", res, src.comments)
	}
	src.err = nil
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 1 || len(src.comments) != 1 {
		t.Fatalf("re-request must be retried: res=%+v", res)
	}
}

// A source that cannot re-request reviews skips the step quietly.
func TestReviewFeedbackSweep_RerequestNeedsCapableSource(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: addressedFeedback("abc1234def", false)}}
	s := feedbackSweepFor(t, src, &recordingKicker{}, routeTo("quality"), time.Now())
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Rerequested != 0 || len(src.comments) != 0 {
		t.Fatalf("res=%+v comments=%v, want nothing", res, src.comments)
	}
}
