package automerge

import (
	"context"
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// foldHumanReturnLabel honours a person's hand-placed return of a PR to
// its author. hivecommons/hive#15159: the owner left a CHANGES_REQUESTED
// review, removed stage/085 and added stage/072-returned-to-contributor;
// the next pass re-derived 085 from the sentinel park and logged
// `added=[stage/085] removed=[stage/072-returned-to-contributor]`. The
// hive is the single writer of the stage family, so it must read a stage
// label it did not write as what it is — a trusted human's decision on
// this head — rather than as drift to repair.
//
// When the PR wears 071/072, the hive's own record does not say it put
// that label there, and no human change request stands against the head,
// the newest label event for that stage is read once per head; if a
// trusted merger added it, the request is recorded against the head in
// the durable store (source stage-label) and folded into the pass like a
// review, so classification routes to 071/072 ahead of the sentinel,
// hold and workflow-path parks. A new head carries no request and
// re-arms the park (per head, as hivecommons/hive#15216). The hive's own
// label events and untrusted actors are ignored.
func (c *Engine) foldHumanReturnLabel(ctx context.Context, store *stagelabels.Store, list []stageObservation) {
	if c == nil || c.humanChangeRequests == nil || len(list) == 0 {
		return
	}
	var o *stageObservation
	for i := range list {
		if strings.TrimSpace(list[i].HeadSHA) != "" && len(list[i].Labels) > 0 {
			o = &list[i]
			break
		}
	}
	if o == nil {
		return
	}
	worn := stagelabels.Current(o.Labels)
	if worn != stagelabels.NeedsAuthor && worn != stagelabels.ReturnedToContributor {
		return
	}
	head := strings.TrimSpace(o.HeadSHA)
	repo := o.Owner + "/" + o.Repo
	if _, ok := c.humanChangeRequests.For(repo, o.Number, head); ok {
		return
	}
	key := stageKey(o.Owner, o.Repo, o.Number)
	if rec, ok := store.Get(key); ok && rec.Stage == worn {
		// The hive wrote this stage itself; nothing to attribute.
		return
	}
	if c.humanReturnAlreadyChecked(key, head) {
		return
	}
	ev, err := c.latestLabelEvent(ctx, o.Owner, o.Repo, o.Number, worn.Label())
	if err != nil {
		c.debug("stage labels: return label provenance unreadable", "repo", o.Display, "pr", o.Number, "error", err)
		return
	}
	c.markHumanReturnChecked(key, head)
	if ev == nil || ev.GetEvent() != "labeled" {
		return
	}
	actor := strings.TrimSpace(hgithub.SafeGetLogin(ev.GetActor()))
	if actor == "" || normalizeBotLogin(actor) == normalizeBotLogin(c.transport.AppBotLogin()) {
		return
	}
	if trusted, _ := c.isTrustedMerger(actor); !trusted {
		c.debug("stage labels: return label set by an untrusted actor is not honoured", "repo", o.Display, "pr", o.Number, "stage", worn.String(), "by", actor)
		return
	}
	req := review.HumanChangeRequest{
		Repo:       repo,
		Number:     o.Number,
		HeadSHA:    head,
		Source:     review.HumanChangeRequestSourceStageLabel,
		SourceID:   ev.GetID(),
		Actor:      actor,
		URL:        o.URL,
		RecordedAt: ev.GetCreatedAt().Time,
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("stage labels: could not record hand-placed return", "repo", o.Display, "pr", o.Number, "error", err)
		return
	}
	c.info("stage labels: honouring a hand-placed return to author", "repo", o.Display, "pr", o.Number, "stage", worn.String(), "by", actor, "head", head)
	for i := range list {
		list[i].Detail.HumanChangeRequester, list[i].Detail.HumanChangeRequestHead = actor, head
	}
}

func (c *Engine) humanReturnAlreadyChecked(key, head string) bool {
	c.humanReturnCheckedMu.Lock()
	defer c.humanReturnCheckedMu.Unlock()
	return c.humanReturnChecked[key] == head
}

func (c *Engine) markHumanReturnChecked(key, head string) {
	c.humanReturnCheckedMu.Lock()
	defer c.humanReturnCheckedMu.Unlock()
	if c.humanReturnChecked == nil {
		c.humanReturnChecked = map[string]string{}
	}
	c.humanReturnChecked[key] = head
}
