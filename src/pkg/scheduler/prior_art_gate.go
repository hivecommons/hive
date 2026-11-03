package scheduler

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/priorart"
)

// PriorArtGateMarker tags the prior-art gate's park comment
// (hivecommons/hive#13575). github.SweepPriorArtGateResume reads it back to
// lift the park once the author answers.
const PriorArtGateMarker = priorart.Marker

// priorArtGateTimeout bounds the GitHub calls the gate makes for one issue
// (the search itself has its own budget inside the Searcher).
const priorArtGateTimeout = 45 * time.Second

// priorArtDiscoverabilityAgent is the bead "agent" a discoverability note
// is recorded under.
const priorArtDiscoverabilityAgent = "triage"

// PriorArtSearcher is the search half of the gate (priorart.Searcher).
type PriorArtSearcher interface {
	Find(ctx context.Context, issue priorart.Issue) priorart.Report
}

// SetPriorArtSearcher attaches the prior-art searcher. Without one the gate
// is a no-op whatever the config says.
func (s *Scheduler) SetPriorArtSearcher(p PriorArtSearcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priorArtSearcher = p
}

// priorArtGateState is the gate's process memory, keyed owner/repo#n. It
// mirrors clarityGateState: parkedAt withholds an issue the snapshot still
// lists, pendingLabel retries a park label that failed after the comment
// went out, asked marks issues whose author was asked and un-parked since
// (or hive-authored issues whose findings note is already attached).
type priorArtGateState struct {
	mu           sync.Mutex
	parkedAt     map[string]time.Time
	pendingLabel map[string]priorArtPending
	asked        map[string]bool
	now          func() time.Time
}

type priorArtPending struct {
	kind priorart.AuthorKind
}

func (p *priorArtGateState) init() {
	if p.parkedAt == nil {
		p.parkedAt = map[string]time.Time{}
		p.pendingLabel = map[string]priorArtPending{}
		p.asked = map[string]bool{}
	}
	if p.now == nil {
		p.now = time.Now
	}
}

// applyPriorArtGate withholds every feature / RFC / epic issue whose best
// prior-art finding scores at or above runs.triage.prior_art.threshold
// (hivecommons/hive#13575). What happens next depends on who filed it:
//
//   - a person: one marker comment quoting the findings and the two
//     replies, the stage/017-possible-existing park label, and a
//     discoverability note (the docs, not the person, are the gap);
//   - a clanker (another hive, a contributor CLI, an external bot): the
//     same park, with notice that silence closes it after the TTL;
//   - the hive's own agent: never parked. A findings note is attached once
//     and the issue carries on; the claiming agent must state the gap.
//
// Default on; without a searcher, a commenter or a labeler the issue list
// is returned as is.
func (s *Scheduler) applyPriorArtGate(issues []github.Issue) []github.Issue {
	gate := s.cfg.Runs.Triage.PriorArt
	if !gate.IsEnabled() || len(issues) == 0 {
		return issues
	}
	s.mu.RLock()
	searcher := s.priorArtSearcher
	s.mu.RUnlock()
	_, commenter := s.runTriageDeps()
	labeler, ok := commenter.(TriageLabeler)
	if searcher == nil || commenter == nil || !ok {
		return issues
	}
	threshold := gate.EffectiveThreshold()
	kinds := gate.EffectiveKinds()
	budget := gate.EffectiveMaxParksPerCycle()
	out := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if !priorArtGated(issue, kinds) {
			out = append(out, issue)
			continue
		}
		withheld, parkedNow := s.judgePriorArt(searcher, commenter, labeler, issue, threshold, budget)
		if parkedNow {
			budget--
		}
		if !withheld {
			out = append(out, issue)
		}
	}
	return out
}

// priorArtGated reports whether the gate judges issue at all: an unclaimed
// GitHub issue of a gated kind that is not already parked and carries no
// `approved-direction` from a maintainer.
func priorArtGated(issue github.Issue, kinds []string) bool {
	if issue.Repo == "" || issue.Number <= 0 {
		return false
	}
	if issue.SourceType != "" && !strings.EqualFold(issue.SourceType, "github") {
		return false
	}
	if strings.TrimSpace(issue.ClaimedBy) != "" || issue.IsTracker {
		return false
	}
	if issueHasLabel(issue, github.HumanAckLabel) || issueHasLabel(issue, github.AlreadyExistsLabel) {
		return false
	}
	return priorart.IsFeatureKind(issue.Title, issue.Labels, kinds)
}

// priorArtAuthorKind classifies the issue's author from the enumerator's
// view: the `author/*` label, the hive's own App login, the agent/origin
// provenance labels, then the human/bot verdict.
func (s *Scheduler) priorArtAuthorKind(issue github.Issue) priorart.AuthorKind {
	botLogin := ""
	if s.cfg != nil {
		botLogin = s.cfg.GitHub.BotLogin()
	}
	return priorart.ClassifyAuthor(priorart.Authorship{
		Login:         issue.Author,
		IsHuman:       issue.AuthorIsHuman,
		IsHiveAccount: botLogin != "" && strings.EqualFold(issue.Author, botLogin),
		Labels:        issue.Labels,
	})
}

// judgePriorArt runs the search for one gated issue and acts on it. It
// returns whether the issue is withheld from this kick and whether it was
// parked in this call (for the per-cycle budget).
func (s *Scheduler) judgePriorArt(searcher PriorArtSearcher, commenter TriageCommenter, labeler TriageLabeler, issue github.Issue, threshold, budget int) (withheld, parkedNow bool) {
	key := issue.Repo + "#" + strconv.Itoa(issue.Number)
	st := &s.priorArt
	st.mu.Lock()
	st.init()
	now := st.now()
	parkedAt, parked := st.parkedAt[key]
	pending, hasPending := st.pendingLabel[key]
	asked := st.asked[key]
	st.mu.Unlock()

	if issueHasLabel(issue, github.PossiblyExistsStageLabel) {
		return true, false
	}
	if parked {
		fresh := issue.UpdatedAt.After(parkedAt) || (issue.UpdatedAt.IsZero() && now.Sub(parkedAt) > clarityStaleSnapshotWindow)
		if !fresh {
			if !hasPending {
				return true, false
			}
		} else {
			st.mu.Lock()
			delete(st.parkedAt, key)
			st.mu.Unlock()
		}
	}
	if asked {
		return false, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), priorArtGateTimeout)
	defer cancel()
	if hasPending {
		s.addPriorArtParkLabel(ctx, labeler, issue, key, pending.kind, now)
		return true, false
	}

	report := searcher.Find(ctx, priorart.Issue{Repo: issue.Repo, Number: issue.Number, Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if len(report.Findings) == 0 {
		if len(report.Coverage) > 0 {
			// Not proof of no prior art: leave a trace, nothing is posted.
			s.priorArtInfo("prior-art gate found nothing but coverage was incomplete", issue, "coverage", strings.Join(report.Coverage, "; "))
			if audit := s.auditLogger(); audit != nil {
				audit("prior-art-gate-coverage-gap", "repo="+issue.Repo+", issue="+strconv.Itoa(issue.Number)+", gaps="+strings.Join(report.Coverage, "; "), priorArtDiscoverabilityAgent)
			}
		}
		return false, false
	}
	if report.Confidence < threshold {
		return false, false
	}
	kind := s.priorArtAuthorKind(issue)
	marker := PriorArtGateMarker
	if kind == priorart.AuthorHive {
		marker = priorart.FindingsMarker
	}
	seen, err := commenter.IssueCommentsContain(ctx, issue.Repo, issue.Number, marker)
	if err != nil {
		s.priorArtWarn("prior-art gate comment scan failed", issue, "error", err)
		return kind != priorart.AuthorHive, false
	}
	if kind == priorart.AuthorHive {
		if !seen {
			if err := commenter.CreateIssueComment(ctx, issue.Repo, issue.Number, priorart.RenderFindingsNote(report)); err != nil {
				s.priorArtWarn("prior-art gate findings note failed", issue, "error", err)
				return false, false
			}
			s.priorArtInfo("prior-art gate attached findings to a hive-filed issue", issue, "confidence", report.Confidence, "threshold", threshold)
		}
		st.mu.Lock()
		st.asked[key] = true
		st.mu.Unlock()
		return false, false
	}
	if seen {
		landed, err := s.clarityParkLabelLanded(ctx, labeler, issue, github.PossiblyExistsStageLabel)
		if err != nil {
			s.priorArtWarn("prior-art gate park history read failed", issue, "error", err)
			return true, false
		}
		if !landed {
			// The ask went out but the park never did, and the process
			// that would have retried it is gone: park now.
			s.addPriorArtParkLabel(ctx, labeler, issue, key, kind, now)
			return true, false
		}
		st.mu.Lock()
		st.asked[key] = true
		st.mu.Unlock()
		return false, false
	}
	if budget <= 0 {
		// Over this cycle's park budget: hold the issue back without
		// commenting; the next cycle asks.
		return true, false
	}
	body := priorart.RenderParkComment(report)
	if kind == priorart.AuthorClanker {
		body = priorart.RenderClankerParkComment(report, s.cfg.Runs.Triage.PriorArt.EffectiveBotCloseTTL())
	}
	if err := commenter.CreateIssueComment(ctx, issue.Repo, issue.Number, body); err != nil {
		s.priorArtWarn("prior-art gate comment failed", issue, "error", err)
		return true, false
	}
	s.addPriorArtParkLabel(ctx, labeler, issue, key, kind, now)
	s.priorArtInfo("prior-art gate parked issue", issue, "author_kind", kind, "confidence", report.Confidence, "threshold", threshold, "findings", len(report.Findings))
	if kind == priorart.AuthorHuman {
		if advise := s.advisoryLogger(); advise != nil {
			advise(priorart.DiscoverabilityTitle(issue.Title), priorart.DiscoverabilityDetail(issue.URL, report), priorArtDiscoverabilityAgent)
		}
	}
	if audit := s.auditLogger(); audit != nil {
		audit("prior-art-gate-parked", "repo="+issue.Repo+", issue="+strconv.Itoa(issue.Number)+", author_kind="+string(kind)+", confidence="+strconv.Itoa(report.Confidence), priorArtDiscoverabilityAgent)
	}
	return true, true
}

func (s *Scheduler) addPriorArtParkLabel(ctx context.Context, labeler TriageLabeler, issue github.Issue, key string, kind priorart.AuthorKind, now time.Time) {
	err := labeler.AddLabels(ctx, issue.Repo, issue.Number, []string{github.PossiblyExistsStageLabel})
	st := &s.priorArt
	st.mu.Lock()
	defer st.mu.Unlock()
	st.parkedAt[key] = now
	if err != nil {
		st.pendingLabel[key] = priorArtPending{kind: kind}
		if s.logger != nil {
			s.logger.Warn("prior-art gate park label failed", "repo", issue.Repo, "number", issue.Number, "error", err)
		}
		return
	}
	delete(st.pendingLabel, key)
}

func (s *Scheduler) priorArtWarn(msg string, issue github.Issue, kv ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, append([]any{"repo", issue.Repo, "number", issue.Number}, kv...)...)
	}
}

func (s *Scheduler) priorArtInfo(msg string, issue github.Issue, kv ...any) {
	if s.logger != nil {
		s.logger.Info(msg, append([]any{"repo", issue.Repo, "number", issue.Number}, kv...)...)
	}
}
