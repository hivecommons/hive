package dashboard

import "testing"

// hive/in-progress is the agent-start signal; the band logic must file it
// with the other claimed markers so a started issue reads as in-progress.
func TestIssueClaimed_InProgressLabel(t *testing.T) {
	cases := map[string]struct {
		labels []string
		want   bool
	}{
		"in-progress":       {[]string{"hive/in-progress"}, true},
		"upper":             {[]string{"HIVE/In-Progress"}, true},
		"claimed":           {[]string{"claimed"}, true},
		"claimed-by":        {[]string{"hive/claimed-by-scanner"}, true},
		"covered-by-pr":     {[]string{"hive/covered-by-pr"}, false},
		"covered-by-pr-bee": {[]string{"🐝 covered by PR"}, false},
		"none":              {nil, false},
	}
	for name, tc := range cases {
		if got := issueClaimed(tc.labels); got != tc.want {
			t.Errorf("%s: issueClaimed(%v) = %v, want %v", name, tc.labels, got, tc.want)
		}
	}
}
