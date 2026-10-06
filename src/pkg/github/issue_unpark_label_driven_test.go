package github

import (
	"context"
	"strings"
	"testing"
)

// Label-driven repos (project.repo_policies[].label_driven, hivecommons/hive#10537)
// get no "What to reply" notice and never lose a parking label, whatever a
// maintainer types.
func TestSweepIssueUnparkCommandsLabelDrivenNeverClearsLabels(t *testing.T) {
	cases := []struct {
		name      string
		comments  []unparkWireComment
		wantReply string
	}{
		{"approve", []unparkWireComment{humanComment(71, "maintainer", "/hive approve")}, "label-driven"},
		{"decision", []unparkWireComment{humanComment(72, "maintainer", "/hive decision keep it small")}, "label-driven"},
		{"help", []unparkWireComment{humanComment(73, "maintainer", "/hive help")}, "Other commands:"},
		{"prose", []unparkWireComment{humanComment(74, "maintainer", "approved, go with A")}, ""},
		{"no comment", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := []unparkWireIssue{parkedIssue(20, tc.comments...)}
			server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
			c := newTestClient(t, server, "org", []string{"repo"})

			var asked []string
			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
				LabelDriven: func(repo string) bool {
					asked = append(asked, repo)
					return true
				},
			})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(asked) == 0 {
				t.Fatal("LabelDriven was never consulted")
			}
			if len(result.Unparked) != 0 {
				t.Fatalf("label-driven repo must not un-park, got %+v", result.Unparked)
			}
			if len(rec.removed[20]) != 0 || len(rec.added[20]) != 0 {
				t.Fatalf("labels changed on a label-driven repo: removed=%v added=%v", rec.removed[20], rec.added[20])
			}
			if len(rec.edited) != 0 {
				t.Fatalf("no comment should be edited, got %v", rec.edited)
			}
			for _, body := range rec.posted[20] {
				if strings.Contains(body, unparkNoticeMarker) {
					t.Fatalf("un-park notice posted on a label-driven repo: %q", body)
				}
				if strings.Contains(body, "To start work, reply") {
					t.Fatalf("prose hint posted on a label-driven repo: %q", body)
				}
			}
			if tc.wantReply == "" {
				if len(rec.posted[20]) != 0 || result.Replies != 0 {
					t.Fatalf("expected silence, posted=%v replies=%d", rec.posted[20], result.Replies)
				}
				return
			}
			if result.Replies != 1 || len(rec.posted[20]) != 1 {
				t.Fatalf("expected exactly one reply, replies=%d posted=%v", result.Replies, rec.posted[20])
			}
			reply := rec.posted[20][0]
			if !strings.Contains(reply, unparkReplyMarkerPrefix) {
				t.Fatalf("reply must carry the per-comment marker so it is posted once: %q", reply)
			}
			if !strings.Contains(reply, tc.wantReply) || !strings.Contains(reply, "needs-human") {
				t.Fatalf("expected %q and the label pointer in the reply, got %q", tc.wantReply, reply)
			}
		})
	}
}

// A repo that does not opt in keeps today's behavior even when the callback
// is wired: notice posted, `/hive approve` clears the parking labels.
func TestSweepIssueUnparkCommandsNotLabelDrivenUnchanged(t *testing.T) {
	issues := []unparkWireIssue{parkedIssue(21, humanComment(81, "maintainer", "/hive approve"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		LabelDriven: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 {
		t.Fatalf("expected issue 21 un-parked, got %+v", result.Unparked)
	}
	removed := strings.Join(rec.removed[21], ",")
	if !strings.Contains(removed, "needs-human") || !strings.Contains(removed, "needs-decision") {
		t.Fatalf("expected parking labels removed, got %v", rec.removed[21])
	}
	var notice bool
	for _, body := range rec.posted[21] {
		if strings.Contains(body, unparkNoticeMarker) {
			notice = true
		}
	}
	if !notice {
		t.Fatalf("expected the un-park notice on a default repo, got %v", rec.posted[21])
	}
}

// An already-answered command on a label-driven repo is not answered again.
func TestSweepIssueUnparkCommandsLabelDrivenAnswersOnce(t *testing.T) {
	answered := humanComment(92, "hive[bot]", unparkReplyMarkerPrefix+"91 -->\n"+unparkLabelDrivenReply)
	issues := []unparkWireIssue{parkedIssue(22, humanComment(91, "maintainer", "/hive approve"), answered)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		LabelDriven: func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Replies != 0 || len(rec.posted[22]) != 0 || len(rec.removed[22]) != 0 {
		t.Fatalf("expected no writes, replies=%d posted=%v removed=%v", result.Replies, rec.posted[22], rec.removed[22])
	}
}
