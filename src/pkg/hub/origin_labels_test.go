package hub

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/originlabel"
)

// Hub-filed issues carry exactly one origin/* label (hivecommons/hive#12432):
// origin/feedback for user feedback and origin/selfreport for relayed
// self-reports, next to the hive/selfreport dedupe signal.
func TestHubFeedbackLabelsCarryExactlyOneOriginFeedback(t *testing.T) {
	for typ, want := range map[string]string{
		feedbackTypeBug: "kind/bug,user-feedback," + originlabel.Feedback,
		"feature":       "enhancement,user-feedback," + originlabel.Feedback,
	} {
		got := hubFeedbackLabels(feedbackReportRequest{RequestType: typ})
		if strings.Join(got, ",") != want || countOriginLabels(got) != 1 {
			t.Errorf("hubFeedbackLabels(%q) = %v, want %s", typ, got, want)
		}
	}
}

func TestSelfReportRelayLabelsCarryExactlyOneOriginSelfReport(t *testing.T) {
	for _, in := range [][]string{
		nil,
		{"fleet-report", "kind/bug"},
		{"fleet-report", selfReportRelayLabel, originlabel.SelfReport},
		{"from-review", "origin/agent", selfReportRelayLabel},
	} {
		got := originlabel.With(in, originlabel.SelfReport)
		if !containsString(got, selfReportRelayLabel) {
			got = append(got, selfReportRelayLabel)
		}
		if countOriginLabels(got) != 1 || !containsString(got, originlabel.SelfReport) || !containsString(got, selfReportRelayLabel) {
			t.Errorf("relay labels for %v = %v, want exactly one %s plus %s", in, got, originlabel.SelfReport, selfReportRelayLabel)
		}
	}
}

func countOriginLabels(labels []string) int {
	n := 0
	for _, l := range labels {
		if originlabel.Is(l) {
			n++
		}
	}
	return n
}
