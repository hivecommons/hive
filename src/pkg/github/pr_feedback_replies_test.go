package github

import (
	"strings"
	"testing"
	"time"
)

func TestUnansweredFeedbackCarriesTheAuthorAssociation(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	var raw rawPRConversation
	raw.ReviewThreads.Nodes = []rawPRCommentThread{{ID: "T", Comments: struct {
		Nodes []rawPRComment `json:"nodes"`
	}{Nodes: []rawPRComment{
		rawComment(1, "nate", "User", "member", "a", at),
		rawComment(2, "kim", "User", "CONTRIBUTOR", "b", at.Add(time.Minute)),
	}}}}
	got := detect(c, raw)
	if len(got) != 2 || got[0].AuthorAssociation != "MEMBER" || got[1].AuthorAssociation != "CONTRIBUTOR" {
		t.Fatalf("items = %+v, want upper-case associations carried through", got)
	}
}

func TestHiveRuleRepliesReadsOnlyTheHivesOwnBlocks(t *testing.T) {
	at := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	block := "done\n<!-- " + FeedbackRulesMarker + " {\"rules\":[]} -->"
	isHive := func(a *rawPRCommentAuthor) bool { return a.Login == "hive[bot]" }
	var raw rawPRConversation
	raw.Comments.Nodes = []rawPRComment{
		rawComment(3, "hive[bot]", "Bot", "NONE", block, at.Add(2*time.Minute)),
		rawComment(4, "hive[bot]", "Bot", "NONE", "no block here", at),
		rawComment(5, "nate", "User", "OWNER", block, at), // a person quoting the marker is not the agent
		{DatabaseID: 6, Body: block, CreatedAt: at},       // deleted account
	}
	raw.Reviews.Nodes = []rawPRComment{rawComment(7, "hive[bot]", "Bot", "NONE", block, at.Add(time.Minute))}
	raw.ReviewThreads.Nodes = []rawPRCommentThread{{ID: "T", Comments: struct {
		Nodes []rawPRComment `json:"nodes"`
	}{Nodes: []rawPRComment{rawComment(8, "hive[bot]", "Bot", "NONE", block, at.Add(-time.Minute))}}}}
	got := HiveRuleReplies(raw, isHive)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
		if !m.Hive || !strings.Contains(m.Body, FeedbackRulesMarker) {
			t.Errorf("reply %+v", m)
		}
	}
	if strings.Join(ids, ",") != "inline:8,review:7,conversation:3" {
		t.Fatalf("ids = %v, want the hive's three block-bearing replies, oldest first", ids)
	}
}
