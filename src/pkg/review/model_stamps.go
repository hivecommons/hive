package review

import "strings"

// ModelStamp is what the planner knew about one PR head when it dispatched
// its reviews: the author's model and, per perspective (and per review head
// when the perspective went to several heads), the model the reviewer was
// told to run with.
type ModelStamp struct {
	AuthorModel  string
	ReviewModels map[perspectiveStampKey]string
}

// perspectiveStampKey is one review_model stamp's slot: the perspective, and
// the review head it was dispatched to, or "" outside review_heads. Two heads
// judging the same perspective run different models, so a stamp keyed by
// perspective alone would let the last-recorded head's model overwrite the
// other's and be applied to both heads' reports (hivecommons/hive#13542).
type perspectiveStampKey struct {
	Perspective Perspective
	Head        string
}

// ModelStamps is keyed by reviewKey(repo, number, head, "") — the body digest
// is deliberately left out, since a report may omit it (foldDigestlessReports)
// while the models it should carry do not depend on it.
type ModelStamps map[string]ModelStamp

// ModelStampsFromState collects stamps from the dispatch state's pending and
// recent reviews. Pending wins over recent for the same head and perspective,
// since it is the dispatch the next report answers.
func ModelStampsFromState(state DispatchState) ModelStamps {
	out := ModelStamps{}
	for _, r := range state.Recent {
		out.record(r.Repo, r.Number, r.HeadSHA, r.Perspective, r.Head, r.AuthorModel, r.ReviewModel)
	}
	for _, p := range state.Pending {
		out.record(p.Repo, p.Number, p.HeadSHA, p.Perspective, p.Head, p.AuthorModel, p.ReviewModel)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m ModelStamps) record(repo string, number int, head string, perspective Perspective, reviewHead, authorModel, reviewModel string) {
	authorModel, reviewModel = strings.TrimSpace(authorModel), strings.TrimSpace(reviewModel)
	if authorModel == "" && reviewModel == "" {
		return
	}
	key := reviewKey(repo, number, head, "")
	stamp := m[key]
	if authorModel != "" {
		stamp.AuthorModel = authorModel
	}
	if reviewModel != "" {
		if stamp.ReviewModels == nil {
			stamp.ReviewModels = map[perspectiveStampKey]string{}
		}
		stamp.ReviewModels[perspectiveStampKey{perspective, strings.TrimSpace(reviewHead)}] = reviewModel
	}
	m[key] = stamp
}

// apply fills a report's empty author_model / review_model from the stamp
// for its head SHA, and for its review head when the relay bound one. Values
// the reviewer wrote are never overwritten: a sub-agent that reports the
// model it actually ran with knows better than the plan.
func (m ModelStamps) apply(report *PerspectiveReport) {
	if m == nil || report == nil {
		return
	}
	stamp, ok := m[reviewKey(report.Repo, report.Number, report.HeadSHA, "")]
	if !ok {
		return
	}
	if strings.TrimSpace(report.AuthorModel) == "" {
		report.AuthorModel = stamp.AuthorModel
	}
	if strings.TrimSpace(report.ReviewModel) == "" {
		report.ReviewModel = stamp.ReviewModels[perspectiveStampKey{report.Perspective, strings.TrimSpace(report.Head)}]
	}
}
