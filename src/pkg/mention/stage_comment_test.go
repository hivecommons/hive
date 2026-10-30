package mention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

type stageCommentGH struct {
	comments []IssueComment
	creates  []string
	edits    []string
}

type gatedStageCommentGH struct {
	stageCommentGH
	gate *writeback.Gate
}

func (g *gatedStageCommentGH) WritebackItemForNumber(_ context.Context, _ string, number int) (writeback.Item, error) {
	return writeback.Item{Number: number, Author: "visitor"}, nil
}

func (g *gatedStageCommentGH) WritebackAllowed(repo string, item writeback.Item, kind writeback.Kind, body string) writeback.Decision {
	return g.gate.AllowedPreview(repo, item, kind, body)
}

func TestStageCommenterGateRefusalLeavesTransitionRetryable(t *testing.T) {
	for _, editing := range []bool{false, true} {
		t.Run(fmt.Sprint(editing), func(t *testing.T) {
			store, _ := NewStore("")
			g := &gatedStageCommentGH{gate: writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")}
			if editing {
				g.comments = []IssueComment{{ID: 1, Body: stageCommentMarker("run-gated")}}
			}
			c := &StageCommenter{
				Store: store, GitHub: func() GitHub { return g },
				Agents: func() []AgentInfo { return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}} },
			}
			tr := StageTransition{Run: "run-gated", Repo: "org/repo", Number: 42, Agent: "scanner", To: "review", Gen: 1}
			if err := c.Post(context.Background(), tr); !errors.Is(err, writeback.ErrSuppressed) || len(g.creates)+len(g.edits) != 0 {
				t.Fatalf("refusal err=%v creates=%q edits=%q", err, g.creates, g.edits)
			}
			yes := true
			g.gate.SetConfig(writeback.Config{
				Unsolicited: writeback.UnsolicitedConfig{Backfill: &yes},
				PerRepo:     map[string]writeback.RepoOverride{"org/repo": {Mode: writeback.RepoModeFull}},
			})
			if err := c.Post(context.Background(), tr); err != nil || len(g.creates)+len(g.edits) != 1 {
				t.Fatalf("retry err=%v creates=%q edits=%q", err, g.creates, g.edits)
			}
		})
	}
}

func (g *stageCommentGH) AppBotLogin() string { return "hive[bot]" }
func (g *stageCommentGH) ListMentionComments(context.Context, string, time.Time) ([]Event, error) {
	return nil, nil
}
func (g *stageCommentGH) CreateMentionAck(context.Context, Event, string) error { return nil }
func (g *stageCommentGH) CountMentionReplies(context.Context, string, int) (int, error) {
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
	if err := store.Advance(repo, before); err != nil {
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
