package review

import "strings"

// ModelStamp is what the planner knew about one PR head when it dispatched
// its reviews: the author's model and, per perspective, the model the
// reviewer was told to run with.
type ModelStamp struct {
	AuthorModel  string
	ReviewModels map[Perspective]string
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
		out.record(r.Repo, r.Number, r.HeadSHA, r.Perspective, r.AuthorModel, r.ReviewModel)
	}
	for _, p := range state.Pending {
		out.record(p.Repo, p.Number, p.HeadSHA, p.Perspective, p.AuthorModel, p.ReviewModel)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m ModelStamps) record(repo string, number int, head string, perspective Perspective, authorModel, reviewModel string) {
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
			stamp.ReviewModels = map[Perspective]string{}
		}
		stamp.ReviewModels[perspective] = reviewModel
	}
	m[key] = stamp
}

// apply fills a report's empty author_model / review_model from the stamp
// for its head. Values the reviewer wrote are never overwritten: a sub-agent
// that reports the model it actually ran with knows better than the plan.
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
		report.ReviewModel = stamp.ReviewModels[report.Perspective]
	}
}
