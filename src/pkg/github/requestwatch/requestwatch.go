package requestwatch

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

type PRRequestAuthorizer = github.PRRequestAuthorizer
type IssueRequestAuthorizer = github.IssueRequestAuthorizer

type Watcher struct {
	client     *github.Client
	prAuthz    PRRequestAuthorizer
	issueAuthz IssueRequestAuthorizer
	holdLabel  func(agent string) bool
	nowFn      func() time.Time
}

func New(client *github.Client, prAuthz PRRequestAuthorizer, issueAuthz IssueRequestAuthorizer, holdLabel func(agent string) bool, nowFn func() time.Time) *Watcher {
	return &Watcher{client: client, prAuthz: prAuthz, issueAuthz: issueAuthz, holdLabel: holdLabel, nowFn: nowFn}
}

func (w *Watcher) Run(ctx context.Context) error {
	if w == nil || w.client == nil {
		return nil
	}
	w.client.StartPRRequestWatcher(ctx, w.prAuthz, w.holdLabel, w.nowFn)
	w.client.StartIssueRequestWatcher(ctx, w.issueAuthz, w.nowFn)
	<-ctx.Done()
	return ctx.Err()
}
