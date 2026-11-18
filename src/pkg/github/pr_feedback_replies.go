package github

import (
	"sort"
	"strconv"
	"strings"
)

// FeedbackRulesMarker is the substring that marks a hive reply as carrying a
// structured review-rules block (pkg/reviewrules.BlockMarker holds the same
// value; pkg/github does not import it).
const FeedbackRulesMarker = "hive:review-rules"

// HiveRuleReplies returns the hive's own comments on a PR, in every place a
// comment lives, whose body carries a review-rules block. isHive must accept
// only the hive's own logins: the block is read as the agent's statement, so a
// comment merely quoting the marker from a person is not one.
func HiveRuleReplies(raw rawPRConversation, isHive func(*rawPRCommentAuthor) bool) []FeedbackMessage {
	return hiveComments(raw, isHive, FeedbackRulesMarker)
}

// HiveFeedbackReplies returns the hive's own comments that carry the
// handled-ids marker the kicked agent ends every reply with, so the sweep can
// tell the agent's reply from the hive's own notices (hivecommons/hive#19048).
func HiveFeedbackReplies(raw rawPRConversation, isHive func(*rawPRCommentAuthor) bool) []FeedbackMessage {
	return hiveComments(raw, isHive, FeedbackHandledMarkerPrefix)
}

func hiveComments(raw rawPRConversation, isHive func(*rawPRCommentAuthor) bool, marker string) []FeedbackMessage {
	var out []FeedbackMessage
	add := func(kind PRCommentKind, rc rawPRComment) {
		if rc.Author == nil || !isHive(rc.Author) || !strings.Contains(rc.Body, marker) {
			return
		}
		out = append(out, FeedbackMessage{ID: string(kind) + ":" + strconv.FormatInt(rc.DatabaseID, 10), Author: rc.Author.Login,
			Hive: true, Body: rc.Body, URL: rc.URL, CreatedAt: rc.CreatedAt})
	}
	for _, rc := range raw.Comments.Nodes {
		add(PRCommentConversation, rc)
	}
	for _, rc := range raw.Reviews.Nodes {
		add(PRCommentReview, rc)
	}
	for _, th := range raw.ReviewThreads.Nodes {
		for _, rc := range th.Comments.Nodes {
			add(PRCommentInline, rc)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
