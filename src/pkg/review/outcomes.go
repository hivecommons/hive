package review

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// The outcome ledger answers the question the verdict artifact cannot: did
// reviewing a PR change what happened to it? A verdict records a judgement;
// this records what the PR did afterwards — merged, closed, or still open, and
// when — for every PR the governor saw, reviewed or not. The unreviewed PRs
// are the control group: a hive that reviews everything has none, a hive with
// a cap or a scope has plenty, and either way the comparison is between PRs
// the same governor enumerated over the same window, not against a number
// from some other project.
//
// Cohorts are keyed on when the governor FIRST saw the PR, not on when it was
// created. A PR opened a month before the hive was pointed at the repo is not
// evidence about the hive.

const (
	ReviewOutcomesFile = "review-outcomes.json"

	// OutcomeOpen / OutcomeMerged / OutcomeClosed are the three states a PR
	// can be in from the ledger's point of view. "closed" means closed
	// without merge.
	OutcomeOpen   = "open"
	OutcomeMerged = "merged"
	OutcomeClosed = "closed"

	// OutcomeRetention bounds how long a resolved PR stays in the ledger.
	// Open PRs are never aged out — they are still the queue.
	OutcomeRetention = 90 * 24 * time.Hour

	// queueSnapshotInterval is how often a queue-depth sample is appended.
	// Once a day is enough to draw a trend; the eval cycle runs every few
	// minutes and a sample per cycle would be noise.
	queueSnapshotInterval = 23 * time.Hour
	queueSnapshotKeep     = 120

	// OutcomeSummaryDefaultWindow is what the API and metrics report when no
	// window is asked for.
	OutcomeSummaryDefaultWindow = 30 * 24 * time.Hour
)

// ReviewOutcomesPath is a var so tests can point it at a temp dir.
var ReviewOutcomesPath = filepath.Join(DefaultDispatchStateDir, ReviewOutcomesFile)

// PROutcome is one PR's row in the ledger.
type PROutcome struct {
	Repo          string    `json:"repo"`
	Number        int       `json:"number"`
	Author        string    `json:"author,omitempty"`
	AgentAuthored bool      `json:"agent_authored,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	// FirstReviewAt is when the hive first reviewed this PR — the earlier of
	// the first recorded verdict and the first posted review. Zero means the
	// hive never reviewed it, which is what puts it in the control group.
	FirstReviewAt time.Time `json:"first_review_at,omitempty"`
	FirstVerdict  Verdict   `json:"first_verdict,omitempty"`
	LatestVerdict Verdict   `json:"latest_verdict,omitempty"`
	Outcome       string    `json:"outcome"`
	OutcomeAt     time.Time `json:"outcome_at,omitempty"`
}

// Key is the ledger key, the same spelling the verdict artifact and the
// review-links ledger use.
func (o PROutcome) Key() string { return outcomeKey(o.Repo, o.Number) }

// Reviewed reports whether the hive reviewed this PR before its outcome (or
// at all, while it is still open).
func (o PROutcome) Reviewed() bool {
	if o.FirstReviewAt.IsZero() {
		return false
	}
	return o.OutcomeAt.IsZero() || !o.FirstReviewAt.After(o.OutcomeAt)
}

// QueueSnapshot is one daily sample of the open-PR queue.
type QueueSnapshot struct {
	At           time.Time `json:"at"`
	Open         int       `json:"open"`
	OpenReviewed int       `json:"open_reviewed"`
}

// OutcomeLedger is the on-disk shape.
type OutcomeLedger struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Items       map[string]*PROutcome `json:"items"`
	Snapshots   []QueueSnapshot       `json:"snapshots,omitempty"`
}

// OpenPR is what the caller knows about a PR the governor enumerated this
// cycle.
type OpenPR struct {
	Repo          string
	Number        int
	Author        string
	AgentAuthored bool
	CreatedAt     time.Time
}

// ReviewSignal is the caller's evidence that a PR was reviewed: when, and
// with what verdict (empty when only a posted review is known).
type ReviewSignal struct {
	At      time.Time
	Verdict Verdict
}

func outcomeKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// LoadOutcomeLedger reads the ledger; a missing file is an empty ledger.
func LoadOutcomeLedger(path string) (*OutcomeLedger, error) {
	if path == "" {
		path = ReviewOutcomesPath
	}
	l := &OutcomeLedger{Items: map[string]*PROutcome{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return l, nil
		}
		return l, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return &OutcomeLedger{Items: map[string]*PROutcome{}}, err
	}
	if l.Items == nil {
		l.Items = map[string]*PROutcome{}
	}
	return l, nil
}

// Save writes the ledger atomically.
func (l *OutcomeLedger) Save(path string, now time.Time) error {
	if path == "" {
		path = ReviewOutcomesPath
	}
	l.GeneratedAt = now.UTC()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Observe folds one governor cycle into the ledger: every open PR is
// upserted, review evidence is attached, and PRs that were open last cycle
// but are absent from this one are returned so the caller can ask GitHub what
// became of them. The ledger does not guess: a PR that vanished from the list
// stays "open" until Resolve says otherwise, because a transient enumeration
// failure looks exactly like a merge from here.
func (l *OutcomeLedger) Observe(now time.Time, open []OpenPR, reviews map[string]ReviewSignal) (missing []PROutcome) {
	now = now.UTC()
	seen := map[string]bool{}
	for _, pr := range open {
		key := outcomeKey(pr.Repo, pr.Number)
		seen[key] = true
		o, ok := l.Items[key]
		if !ok {
			o = &PROutcome{Repo: pr.Repo, Number: pr.Number, FirstSeenAt: now, Outcome: OutcomeOpen}
			l.Items[key] = o
		}
		if o.Outcome != OutcomeOpen {
			// Reopened. Treat it as a fresh observation of an open PR.
			o.Outcome = OutcomeOpen
			o.OutcomeAt = time.Time{}
		}
		if o.Author == "" {
			o.Author = pr.Author
		}
		o.AgentAuthored = o.AgentAuthored || pr.AgentAuthored
		if o.CreatedAt.IsZero() {
			o.CreatedAt = pr.CreatedAt.UTC()
		}
	}
	for key, sig := range reviews {
		o, ok := l.Items[key]
		if !ok || sig.At.IsZero() {
			continue
		}
		at := sig.At.UTC()
		if o.FirstReviewAt.IsZero() || at.Before(o.FirstReviewAt) {
			o.FirstReviewAt = at
			if sig.Verdict != "" {
				o.FirstVerdict = sig.Verdict
			}
		}
		if sig.Verdict != "" {
			o.LatestVerdict = sig.Verdict
		}
	}
	for key, o := range l.Items {
		if o.Outcome == OutcomeOpen && !seen[key] {
			missing = append(missing, *o)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Key() < missing[j].Key() })
	return missing
}

// Resolve records what GitHub said became of a PR that left the open list.
// state is GitHub's "open"/"closed"; mergedAt non-zero means merged.
func (l *OutcomeLedger) Resolve(repo string, number int, state string, mergedAt, closedAt time.Time) {
	o, ok := l.Items[outcomeKey(repo, number)]
	if !ok {
		return
	}
	switch {
	case !mergedAt.IsZero():
		o.Outcome = OutcomeMerged
		o.OutcomeAt = mergedAt.UTC()
	case state == "closed":
		o.Outcome = OutcomeClosed
		if closedAt.IsZero() {
			closedAt = time.Now()
		}
		o.OutcomeAt = closedAt.UTC()
	default:
		// Still open on GitHub: the enumeration missed it this cycle.
	}
}

// Snapshot appends a queue-depth sample when the last one is old enough, and
// ages resolved rows out past OutcomeRetention.
func (l *OutcomeLedger) Snapshot(now time.Time) {
	now = now.UTC()
	for key, o := range l.Items {
		if o.Outcome != OutcomeOpen && !o.OutcomeAt.IsZero() && now.Sub(o.OutcomeAt) > OutcomeRetention {
			delete(l.Items, key)
		}
	}
	if n := len(l.Snapshots); n > 0 && now.Sub(l.Snapshots[n-1].At) < queueSnapshotInterval {
		return
	}
	snap := QueueSnapshot{At: now}
	for _, o := range l.Items {
		if o.Outcome == OutcomeOpen {
			snap.Open++
			if o.Reviewed() {
				snap.OpenReviewed++
			}
		}
	}
	l.Snapshots = append(l.Snapshots, snap)
	if len(l.Snapshots) > queueSnapshotKeep {
		l.Snapshots = l.Snapshots[len(l.Snapshots)-queueSnapshotKeep:]
	}
}

// OutcomeGroup is the outcome statistics for one cohort of PRs.
type OutcomeGroup struct {
	PRs    int `json:"prs"`
	Merged int `json:"merged"`
	Closed int `json:"closed"`
	Open   int `json:"open"`
	// MergedWithin24h / 72h count from when the hive first saw the PR.
	MergedWithin24h int `json:"merged_within_24h"`
	MergedWithin72h int `json:"merged_within_72h"`
	// MedianHoursSeenToMerge is first-seen → merged, the number both cohorts
	// share. MedianHoursReviewToMerge is first-review → merged and is only
	// meaningful for the reviewed cohort (zero otherwise).
	MedianHoursSeenToMerge   float64 `json:"median_hours_seen_to_merge"`
	MedianHoursReviewToMerge float64 `json:"median_hours_review_to_merge,omitempty"`
	// MedianHoursOpenAge is how old the still-open PRs are, from first seen.
	MedianHoursOpenAge float64 `json:"median_hours_open_age"`
}

// OutcomeSummary is what the API and the metrics endpoint publish.
type OutcomeSummary struct {
	GeneratedAt time.Time `json:"generated_at"`
	WindowDays  int       `json:"window_days"`
	// Reviewed and Unreviewed split the cohort by whether the hive reviewed
	// the PR before its outcome. Unreviewed is the control group.
	Reviewed   OutcomeGroup `json:"reviewed"`
	Unreviewed OutcomeGroup `json:"unreviewed"`
	// ByVerdict splits the reviewed cohort by the first verdict recorded.
	ByVerdict map[Verdict]OutcomeGroup `json:"by_verdict,omitempty"`
	// AgentAuthored / HumanAuthored split the reviewed cohort by author kind.
	AgentAuthored OutcomeGroup `json:"agent_authored"`
	HumanAuthored OutcomeGroup `json:"human_authored"`
	// Snapshots is the daily open-queue trend, oldest first.
	Snapshots []QueueSnapshot `json:"snapshots,omitempty"`
}

// Summary computes the statistics for PRs first seen within window.
func (l *OutcomeLedger) Summary(now time.Time, window time.Duration) OutcomeSummary {
	now = now.UTC()
	if window <= 0 {
		window = OutcomeSummaryDefaultWindow
	}
	since := now.Add(-window)
	s := OutcomeSummary{GeneratedAt: now, WindowDays: int(window.Hours() / 24), ByVerdict: map[Verdict]OutcomeGroup{}}
	var reviewed, unreviewed, agent, human []PROutcome
	byVerdict := map[Verdict][]PROutcome{}
	for _, o := range l.Items {
		if o.FirstSeenAt.Before(since) {
			continue
		}
		if o.Reviewed() {
			reviewed = append(reviewed, *o)
			if o.AgentAuthored {
				agent = append(agent, *o)
			} else {
				human = append(human, *o)
			}
			if o.FirstVerdict != "" {
				byVerdict[o.FirstVerdict] = append(byVerdict[o.FirstVerdict], *o)
			}
		} else {
			unreviewed = append(unreviewed, *o)
		}
	}
	s.Reviewed = groupStats(reviewed, now)
	s.Unreviewed = groupStats(unreviewed, now)
	s.AgentAuthored = groupStats(agent, now)
	s.HumanAuthored = groupStats(human, now)
	for v, rows := range byVerdict {
		s.ByVerdict[v] = groupStats(rows, now)
	}
	if len(s.ByVerdict) == 0 {
		s.ByVerdict = nil
	}
	s.Snapshots = append(s.Snapshots, l.Snapshots...)
	return s
}

func groupStats(rows []PROutcome, now time.Time) OutcomeGroup {
	g := OutcomeGroup{PRs: len(rows)}
	var seenToMerge, reviewToMerge, openAge []float64
	for _, o := range rows {
		switch o.Outcome {
		case OutcomeMerged:
			g.Merged++
			d := o.OutcomeAt.Sub(o.FirstSeenAt)
			if d < 0 {
				d = 0
			}
			seenToMerge = append(seenToMerge, d.Hours())
			if d <= 24*time.Hour {
				g.MergedWithin24h++
			}
			if d <= 72*time.Hour {
				g.MergedWithin72h++
			}
			if !o.FirstReviewAt.IsZero() {
				r := o.OutcomeAt.Sub(o.FirstReviewAt)
				if r < 0 {
					r = 0
				}
				reviewToMerge = append(reviewToMerge, r.Hours())
			}
		case OutcomeClosed:
			g.Closed++
		default:
			g.Open++
			openAge = append(openAge, now.Sub(o.FirstSeenAt).Hours())
		}
	}
	g.MedianHoursSeenToMerge = median(seenToMerge)
	g.MedianHoursReviewToMerge = median(reviewToMerge)
	g.MedianHoursOpenAge = median(openAge)
	return g
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return round1(xs[n/2])
	}
	return round1((xs[n/2-1] + xs[n/2]) / 2)
}

func round1(x float64) float64 {
	return float64(int64(x*10+0.5)) / 10
}
