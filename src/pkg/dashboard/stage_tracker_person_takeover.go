package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerPersonTakeover is the person-PR takeover timer's view of a
// person's PR waiting on its author at 071 / 072 in a repository an owner
// opted in (governor.labels.stage.adoption.person_after,
// hivecommons/hive#19395). The decision row says when the hive takes the PR
// over instead of "the hive never rewrites a person's PR".
type StageTrackerPersonTakeover struct {
	// After / Grace are person_after and person_grace ("24h").
	After string `json:"after"`
	Grace string `json:"grace"`
	// NoticeAt is when the hive posted its takeover notice; At is when the
	// grace after it ends. Both are absent before the notice.
	NoticeAt *time.Time `json:"notice_at,omitempty"`
	At       *time.Time `json:"at,omitempty"`
	// Text is the sentence the decision row carries.
	Text string `json:"text"`
}

// stageTrackerPersonTakeover builds the `person_takeover` field: nil unless
// the row is a person's PR (author/human, or one of this hive's owners) at
// 071 / 072 in a repository whose list for that kind is opted in at ACMM L6
// (config.Config.PersonTakeoverAllowed).
func (s *Server) stageTrackerPersonTakeover(it StageTrackerItem, rec stagelabels.Record, hasRec bool, now time.Time) *StageTrackerPersonTakeover {
	if s == nil || s.deps == nil || s.deps.Config == nil || it.Kind != "pr" {
		return nil
	}
	if it.Stage != stagelabels.NeedsAuthor.Number() && it.Stage != stagelabels.ReturnedToContributor.Number() {
		return nil
	}
	human := it.AuthorKind == authorKindHuman
	if !human && !s.isAuthorizedUserLogin(it.Author) {
		return nil
	}
	cfg := s.deps.Config
	if !cfg.PersonTakeoverAllowed(it.Repo, human) {
		return nil
	}
	adoption := cfg.Governor.Labels.Stage.Adoption
	after, grace := adoption.EffectivePersonAfter(), adoption.EffectivePersonGrace()
	out := &StageTrackerPersonTakeover{After: stageTrackerShortDuration(after), Grace: stageTrackerShortDuration(grace)}
	if hasRec && !rec.PersonNoticeAt.IsZero() {
		noticed, at := rec.PersonNoticeAt, rec.PersonNoticeAt.Add(grace)
		out.NoticeAt, out.At = &noticed, &at
		when := "on its next pass"
		if left := at.Sub(now); left >= time.Minute {
			when = "in " + stageTrackerShortDuration(left)
		}
		out.Text = fmt.Sprintf("The hive posted its takeover notice and takes the PR over %s unless someone pushes, comments or adds hold.", when)
		return out
	}
	out.Text = fmt.Sprintf("This repository's takeover timer is on: after %s with no push, comment or review while it waits on work the hive can do (a conflict, a failed check, a review bot's change request), the hive posts a notice and takes the PR over %s later unless someone pushes, comments or adds hold.", out.After, out.Grace)
	return out
}
