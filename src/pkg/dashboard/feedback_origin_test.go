package dashboard

import (
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Dashboard-filed feedback carries exactly one origin/* label, origin/feedback
// (hivecommons/hive#12432), for both request types.
func TestFeedbackLabelsCarryExactlyOneOriginFeedback(t *testing.T) {
	cases := map[string]string{
		feedbackTypeBug: "kind/bug,user-feedback," + ghpkg.OriginFeedbackLabel,
		"feature":       "enhancement,user-feedback," + ghpkg.OriginFeedbackLabel,
	}
	for typ, want := range cases {
		got := feedbackLabels(feedbackReportRequest{RequestType: typ})
		if strings.Join(got, ",") != want {
			t.Errorf("feedbackLabels(%q) = %v, want %s", typ, got, want)
		}
		if n := countLabelsWithPrefix(got, ghpkg.OriginLabelPrefix); n != 1 {
			t.Errorf("feedbackLabels(%q) carries %d origin labels", typ, n)
		}
	}
}
