package github

import "testing"

// hivecommons/hive#18199: the remote-item poller needs both hive marks and
// the agent each names.
func TestHiveMarkerAgent(t *testing.T) {
	footer := func(agent string) string {
		s := "Body text\n\n---\n🐝 **Hive Agent**:"
		if agent != "" {
			s += " `" + agent + "`"
		}
		return s + " | **Instance:** `h1` | **SHA:** `abc123`\n<!-- hive:origin hive=h1 at=2026-11-14T00:00:00Z -->"
	}
	cases := []struct {
		name   string
		body   string
		agent  string
		marked bool
	}{
		{"plain", "Just a human issue", "", false},
		{"trailer", "Findings\n\n" + AppendTrailer("", InvocationMeta{Agent: "docs-assessor", Backend: "claude"}), "docs-assessor", true},
		{"governor trailer names no agent", AppendTrailer("x", hiveWriteMeta()), "", true},
		{"footer with display name", footer("Docs Assessor"), "Docs Assessor", true},
		{"footer without name", footer(""), "", true},
		{"trailer wins over footer", "x\n\n-- hive: agent=scanner backend=bob\n\n---\n🐝 **Hive Agent**: `Other` | **SHA:** `a`", "scanner", true},
		{"footer not at the end is not the footer", "---\n🐝 **Hive Agent**: `x` | **SHA:** `a`\nmore prose after", "", false},
		{"crlf footer", "Body\r\n\r\n---\r\n🐝 **Hive Agent**: `Docs Assessor` | **SHA:** `abc`", "Docs Assessor", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, marked := HiveMarkerAgent(tc.body)
			if marked != tc.marked || agent != tc.agent {
				t.Fatalf("HiveMarkerAgent = (%q, %v), want (%q, %v)", agent, marked, tc.agent, tc.marked)
			}
			if HasHiveIdentityFooter(tc.body) != (tc.marked && tc.name != "trailer" && tc.name != "governor trailer names no agent") {
				t.Fatalf("HasHiveIdentityFooter = %v for %q", HasHiveIdentityFooter(tc.body), tc.name)
			}
		})
	}
}
