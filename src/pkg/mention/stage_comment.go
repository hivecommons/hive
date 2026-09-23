package mention

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const stageStatusMarker = "<!-- hive:run-stage-status"

type IssueComment struct {
	ID   int64
	Body string
}

type issueCommentLister interface {
	ListIssueComments(ctx context.Context, repo string, number int) ([]IssueComment, error)
}

type issueCommentEditor interface {
	EditIssueComment(ctx context.Context, repo string, id int64, body string) error
}

type StageTransition struct {
	Run          string
	Repo         string
	IssueRef     string
	Number       int
	Agent        string
	From         string
	To           string
	Gen          uint64
	WaitingOn    string
	DashboardURL string
}

type StageCommenter struct {
	Store  *Store
	GitHub func() GitHub
	Agents AgentFunc
	Audit  AuditFunc
}

func (c *StageCommenter) Post(ctx context.Context, tr StageTransition) error {
	if c == nil || c.Store == nil {
		return nil
	}
	tr.Run = strings.TrimSpace(tr.Run)
	tr.To = strings.TrimSpace(tr.To)
	if tr.Run == "" || tr.To == "" {
		return nil
	}
	if !stageAgentCanConverse(c.Agents, tr.Agent) {
		c.audit(AuditDeclined, "stage-comment:converse", tr.Agent)
		return nil
	}
	repo, number := stageIssueTarget(tr)
	if repo == "" || number <= 0 {
		c.audit(AuditDeclined, "stage-comment:target", tr.Agent)
		return nil
	}
	dedupe := fmt.Sprintf("stage-comment:%s:%d:%s", tr.Run, tr.Gen, tr.To)
	if c.Store.Seen(dedupe) {
		return nil
	}
	body := stageCommentBody(tr)
	gh := GitHub(nil)
	if c.GitHub != nil {
		gh = c.GitHub()
	}
	if gh == nil {
		c.audit(AuditDeclined, "stage-comment:github", tr.Agent)
		return nil
	}
	var commentID int64
	if lister, ok := gh.(issueCommentLister); ok {
		comments, err := lister.ListIssueComments(ctx, repo, number)
		if err != nil {
			return err
		}
		for _, cm := range comments {
			if strings.Contains(cm.Body, stageCommentMarker(tr.Run)) {
				commentID = cm.ID
				break
			}
		}
	}
	if commentID != 0 {
		if editor, ok := gh.(issueCommentEditor); ok {
			if err := editor.EditIssueComment(ctx, repo, commentID, body); err != nil {
				return err
			}
			return c.markStageSeen(repo, dedupe)
		}
	}
	if err := gh.CreateIssueComment(ctx, repo, number, body); err != nil {
		return err
	}
	return c.markStageSeen(repo, dedupe)
}

func (c *StageCommenter) markStageSeen(repo, dedupe string) error {
	// Mark only the idempotency key. Stage comments are not mention events, so
	// they must not advance the shared mention polling watermark.
	return c.Store.Mark(repo, dedupe, time.Time{})
}

func (c *StageCommenter) audit(action, detail, agent string) {
	if c != nil && c.Audit != nil {
		c.Audit(action, detail, agent)
	}
}

func stageAgentCanConverse(agents AgentFunc, agentName string) bool {
	if agents == nil {
		return false
	}
	for _, a := range agents() {
		if a.Name == agentName {
			return a.Enabled && a.Converse
		}
	}
	return false
}

func stageIssueTarget(tr StageTransition) (string, int) {
	if tr.Repo != "" && tr.Number > 0 {
		return tr.Repo, tr.Number
	}
	ref := strings.TrimSpace(tr.IssueRef)
	idx := strings.LastIndex(ref, "#")
	if idx <= 0 || idx == len(ref)-1 {
		return "", 0
	}
	n, err := strconv.Atoi(ref[idx+1:])
	if err != nil {
		return "", 0
	}
	return ref[:idx], n
}

func stageCommentBody(tr StageTransition) string {
	waiting := strings.TrimSpace(tr.WaitingOn)
	if waiting == "" {
		waiting = "unknown"
	}
	details := strings.TrimSpace(tr.DashboardURL)
	if details == "" {
		details = "dashboard run URL unavailable"
	}
	return fmt.Sprintf("Run %s: stage %s complete, now %s (%s). Details: %s\n\n%s -->",
		tr.Run, strings.TrimSpace(tr.From), tr.To, waiting, details, stageCommentMarker(tr.Run))
}

func stageCommentMarker(run string) string {
	return stageStatusMarker + " run=" + url.QueryEscape(strings.TrimSpace(run))
}
