package mention

import (
	"context"
	"strings"
	"testing"
	"time"
)

type stageCommentGH struct {
	comments []IssueComment
	creates  []string
	edits    []string
}

func (g *stageCommentGH) AppBotLogin() string { return "hive[bot]" }
func (g *stageCommentGH) ListMentionComments(context.Context, string, time.Time) ([]Event, error) {
	return nil, nil
}
func (g *stageCommentGH) CreateMentionAck(context.Context, Event, string) error { return nil }
func (g *stageCommentGH) CountAppAuthoredComments(context.Context, string, int) (int, error) {
	return 0, nil
}
func (g *stageCommentGH) CreateIssueComment(_ context.Context, _ string, _ int, body string) error {
	g.creates = append(g.creates, body)
	g.comments = append(g.comments, IssueComment{ID: int64(len(g.comments) + 1), Body: body})
	return nil
}
func (g *stageCommentGH) ListIssueComments(context.Context, string, int) ([]IssueComment, error) {
	return g.comments, nil
}
func (g *stageCommentGH) EditIssueComment(_ context.Context, _ string, id int64, body string) error {
	g.edits = append(g.edits, body)
	for i := range g.comments {
		if g.comments[i].ID == id {
			g.comments[i].Body = body
		}
	}
	return nil
}

func TestStageCommenterDedupesAndEditsOneRunComment(t *testing.T) {
	store, _ := NewStore("")
	gh := &stageCommentGH{}
	c := &StageCommenter{Store: store, GitHub: func() GitHub { return gh }, Agents: func() []AgentInfo { return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}} }}
	tr := StageTransition{Run: "run-1", IssueRef: "org/repo#42", Agent: "scanner", From: "plan", To: "implement", Gen: 7, WaitingOn: "agent", DashboardURL: "https://hive/runs/run-1"}
	if err := c.Post(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if err := c.Post(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	tr.Gen = 8
	tr.From = "implement"
	tr.To = "review"
	if err := c.Post(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if len(gh.creates) != 1 {
		t.Fatalf("creates=%d want 1", len(gh.creates))
	}
	if len(gh.edits) != 1 {
		t.Fatalf("edits=%d want 1", len(gh.edits))
	}
	if !strings.Contains(gh.edits[0], "now review") {
		t.Fatalf("edit did not carry latest stage: %q", gh.edits[0])
	}
}

func TestStageCommenterRequiresConverse(t *testing.T) {
	store, _ := NewStore("")
	gh := &stageCommentGH{}
	c := &StageCommenter{Store: store, GitHub: func() GitHub { return gh }, Agents: func() []AgentInfo { return []AgentInfo{{Name: "scanner", Enabled: true, Converse: false}} }}
	if err := c.Post(context.Background(), StageTransition{Run: "run-1", IssueRef: "org/repo#42", Agent: "scanner", To: "review", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if len(gh.creates) != 0 {
		t.Fatalf("Converse-disabled agent posted %d comments", len(gh.creates))
	}
}

func TestStageCommenterDoesNotAdvanceMentionWatermark(t *testing.T) {
	store, _ := NewStore("")
	repo := "org/repo"
	before := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := store.Mark(repo, "mention-comment-1", before); err != nil {
		t.Fatal(err)
	}
	gh := &stageCommentGH{}
	c := &StageCommenter{Store: store, GitHub: func() GitHub { return gh }, Agents: func() []AgentInfo { return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}} }}
	if err := c.Post(context.Background(), StageTransition{Run: "run-1", Repo: repo, Number: 42, Agent: "scanner", To: "review", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if got := store.Watermark(repo); !got.Equal(before) {
		t.Fatalf("watermark advanced to %s, want %s", got, before)
	}
}
