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
	prDone := w.client.StartPRRequestWatcher(ctx, w.prAuthz, w.holdLabel, w.nowFn)
	issueDone := w.client.StartIssueRequestWatcher(ctx, w.issueAuthz, w.nowFn)
	<-ctx.Done()
	// Return only once both loops have finished their in-flight request, so
	// a caller that restarts the watchers on a rebuilt client (#9621) never
	// has two loops consuming the same request directory at once.
	<-prDone
	<-issueDone
	return ctx.Err()
}
