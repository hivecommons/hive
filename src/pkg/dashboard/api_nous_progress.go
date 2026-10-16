package dashboard

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
)

type nousBaselineProgress struct {
	Snapshots           int      `json:"snapshots"`
	Target              int      `json:"target"`
	Percent             float64  `json:"percent"`
	EvalIntervalSeconds int      `json:"evalIntervalSeconds"`
	RemainingSnapshots  int      `json:"remainingSnapshots"`
	ETASeconds          int      `json:"etaSeconds"`
	Complete            bool     `json:"complete"`
	Collecting          []string `json:"collecting"`
	Retention           string   `json:"retention"`
}

type nousExperimentProgress struct {
	ID                   string                 `json:"id"`
	Title                string                 `json:"title,omitempty"`
	Hypothesis           string                 `json:"hypothesis,omitempty"`
	Repo                 string                 `json:"repo,omitempty"`
	CampaignID           int                    `json:"campaignId,omitempty"`
	Status               string                 `json:"status"`
	Phase                string                 `json:"phase"`
	StartedAt            string                 `json:"startedAt,omitempty"`
	PhaseEndsAt          string                 `json:"phaseEndsAt,omitempty"`
	ProgressPct          int                    `json:"progressPct"`
	ElapsedSeconds       int                    `json:"elapsedSeconds,omitempty"`
	RemainingSeconds     int                    `json:"remainingSeconds,omitempty"`
	SnapshotsInPhase     int                    `json:"snapshotsInPhase"`
	SnapshotCounts       map[string]int         `json:"snapshotCounts,omitempty"`
	MetricsSoFar         []nousExperimentMetric `json:"metricsSoFar,omitempty"`
	NextOutputETA        string                 `json:"nextOutputEta,omitempty"`
	NextOutputETASeconds int                    `json:"nextOutputEtaSeconds,omitempty"`
	Verdict              string                 `json:"verdict,omitempty"`
	Delta                string                 `json:"delta,omitempty"`
	Outputs              []nousExperimentOutput `json:"outputs,omitempty"`
}

type nousExperimentMetric struct {
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Baseline   *float64 `json:"baseline,omitempty"`
	Experiment *float64 `json:"experiment,omitempty"`
	Delta      *float64 `json:"delta,omitempty"`
	Unit       string   `json:"unit,omitempty"`
}

type nousExperimentOutput struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	ID    string `json:"id,omitempty"`
}

type nousBucketLite struct {
	Start              time.Time
	Samples            int
	QueueIssues        []int
	QueuePRs           []int
	QueueHold          []int
	SLAViolatedSamples int
	Kicks              int
	Tokens             int64
}

func (s *Server) addNousProgressStatus(status map[string]interface{}, config map[string]interface{}, ledger []map[string]interface{}, principles []NousPrinciple) {
	count := intFromAny(status["snapshotCount"])
	if count == 0 {
		count = intFromAny(status["snapshots"])
	}
	// With a campaign store the baseline is the oldest running campaign's
	// window (#12501); snapshots/snapshotCount keep reporting the whole
	// history, which is also published as "observation". Every campaign —
	// running, paused and archived — is listed in "campaigns" with its own
	// baseline count (#12532); rows are dropped only when they predate every
	// open campaign.
	var since time.Time
	var ns *NousState
	if s != nil {
		ns = s.deps.Nous
	}
	open := ns.OpenCampaigns()
	if len(open) > 0 {
		since = open[0].StartedAt
		status["campaigns"] = s.nousCampaignViews(ns)
		status["observation"] = ns.Observation()
		status["baselineTargetExplainer"] = NousBaselineTargetExplainer
		if active, ok := ns.ActiveCampaign(); ok {
			count = ns.CampaignSnapshots(active)
			status["campaign"] = nousCampaignToView(ns, active)
			status["baseline_pct"] = NousBaselinePct(count)
		}
	} else if ns.Campaigns() != nil {
		status["campaigns"] = s.nousCampaignViews(ns)
		status["observation"] = ns.Observation()
		status["baselineTargetExplainer"] = NousBaselineTargetExplainer
	}
	eval := 120
	if s != nil && s.deps.Config != nil && s.deps.Config.Governor.EvalIntervalS > 0 {
		eval = s.deps.Config.Governor.EvalIntervalS
	}
	remaining := NousBaselineTarget - count
	if remaining < 0 {
		remaining = 0
	}
	status["baseline"] = nousBaselineProgress{
		Snapshots:           count,
		Target:              NousBaselineTarget,
		Percent:             NousBaselinePct(count),
		EvalIntervalSeconds: eval,
		RemainingSnapshots:  remaining,
		ETASeconds:          remaining * eval,
		Complete:            remaining == 0,
		Collecting: []string{
			"queue issues", "queue PRs", "queue on hold", "SLA violations",
			"agents kicked", "agent states", "total tokens",
		},
		Retention: "24 h raw snapshots, then hourly buckets for 30 days",
	}
	experiments, results := s.nousExperimentProgress(status, config, ledger, principles, since)
	nousAssignCampaigns(experiments, open)
	nousAssignCampaigns(results, open)
	status["experiments"] = experiments
	status["results"] = results
	mode := stringFromAny(status["mode"])
	if c, ok := status["campaign"].(nousCampaignView); ok {
		c.Experiments = nousCountForCampaign(experiments, c.ID)
		c.Principles = len(principles)
		nousCampaignStage(&c, mode, experiments)
		status["campaign"] = c
	}
	if views, ok := status["campaigns"].([]nousCampaignView); ok {
		status["principlesExplainer"] = NousPrinciplesExplainer
		for i := range views {
			nousCampaignStage(&views[i], mode, experiments)
			if views[i].Status == nous.StatusArchived {
				continue
			}
			views[i].Experiments = nousCountForCampaign(experiments, views[i].ID)
			views[i].Principles = len(principles)
			for _, r := range results {
				if r.CampaignID != views[i].ID {
					continue
				}
				var m map[string]interface{}
				if data, err := json.Marshal(r); err == nil && json.Unmarshal(data, &m) == nil {
					views[i].Results = append(views[i].Results, m)
				}
			}
		}
		status["campaigns"] = views
	}
}

// nousExperimentProgress computes experiment rows; rows with a start time
// before since (the active campaign's start) belong to an archived campaign
// and are dropped. Rows without a timestamp are kept.
func (s *Server) nousExperimentProgress(status map[string]interface{}, config map[string]interface{}, ledger []map[string]interface{}, principles []NousPrinciple, since time.Time) ([]nousExperimentProgress, []nousExperimentProgress) {
	baseHours, expHours, coolHours := nousScheduleHours(config)
	now := time.Now().UTC()
	buckets := s.nousLoadBuckets(now.Add(-45 * 24 * time.Hour))
	seen := map[string]bool{}
	var rows []nousExperimentProgress
	add := func(src map[string]interface{}, fallback string) {
		if len(src) == 0 {
			return
		}
		row := nousExperimentRow(src, fallback, baseHours, expHours, coolHours, now, buckets, principles)
		if row.ID == "" {
			row.ID = fallback
		}
		if !since.IsZero() && row.StartedAt != "" && parseTime(row.StartedAt).Before(since) {
			return
		}
		key := strings.ToLower(row.ID + ":" + row.Status)
		if seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, row)
	}
	if p, ok := status["pending"].(map[string]interface{}); ok {
		add(p, "pending")
	}
	if a, ok := status["activeExperiment"].(map[string]interface{}); ok {
		add(a, "active")
	}
	for _, entry := range ledger {
		typ := strings.ToLower(stringFromAny(entry["type"]))
		if typ == "dry_run" || typ == "" {
			continue
		}
		add(entry, typ)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt < rows[j].StartedAt })
	var results []nousExperimentProgress
	for _, row := range rows {
		if row.Phase == "judged" || row.Verdict != "" || len(row.Outputs) > 0 {
			results = append(results, row)
		}
	}
	return rows, results
}

func nousExperimentRow(src map[string]interface{}, fallback string, baseHours, expHours, coolHours float64, now time.Time, buckets []nousBucketLite, principles []NousPrinciple) nousExperimentProgress {
	status := strings.ToLower(firstString(src, "status", "state", "type"))
	if status == "" {
		status = fallback
	}
	id := firstString(src, "id", "experiment_id", "experimentId", "name")
	title := firstString(src, "title", "name")
	hypothesis := firstString(src, "hypothesis", "description", "summary")
	repo := firstString(src, "repo", "target_repo", "targetRepo", "repository")
	start := firstTime(src, "startedAt", "started_at", "start", "start_time", "ts", "timestamp", "created_at")
	row := nousExperimentProgress{
		ID:             id,
		Title:          title,
		Hypothesis:     hypothesis,
		Repo:           repo,
		Status:         status,
		Phase:          "defined",
		SnapshotCounts: map[string]int{},
		Verdict:        firstString(src, "verdict", "outcome", "result"),
		Delta:          firstString(src, "delta", "effect", "change"),
	}
	if row.Title == "" {
		row.Title = row.ID
	}
	if start.IsZero() {
		if status == "completed" || status == "judged" || status == "analysis" {
			row.Phase = "judged"
		}
		row.Outputs = nousOutputs(src, principles, row.ID)
		return row
	}
	row.StartedAt = start.Format(time.RFC3339)
	baseEnd := start.Add(hoursDuration(baseHours))
	expEnd := baseEnd.Add(hoursDuration(expHours))
	coolEnd := expEnd.Add(hoursDuration(coolHours))
	total := coolEnd.Sub(start)
	switch {
	case status == "completed" || status == "judged" || status == "analysis" || now.After(coolEnd):
		row.Phase = "judging"
		row.PhaseEndsAt = now.Format(time.RFC3339)
		row.ProgressPct = 100
	case now.Before(baseEnd):
		row.Phase = "baseline"
		row.PhaseEndsAt = baseEnd.Format(time.RFC3339)
	case now.Before(expEnd):
		row.Phase = "experiment"
		row.PhaseEndsAt = expEnd.Format(time.RFC3339)
	case now.Before(coolEnd):
		row.Phase = "cooldown"
		row.PhaseEndsAt = coolEnd.Format(time.RFC3339)
	default:
		row.Phase = "judging"
	}
	row.Outputs = nousOutputs(src, principles, row.ID)
	if row.Phase == "judging" && (row.Verdict != "" || len(row.Outputs) > 0) {
		row.Phase = "judged"
	}
	if total > 0 && row.ProgressPct == 0 {
		row.ProgressPct = int(math.Round(100 * now.Sub(start).Seconds() / total.Seconds()))
		if row.ProgressPct < 0 {
			row.ProgressPct = 0
		}
		if row.ProgressPct > 99 && row.Phase != "judged" {
			row.ProgressPct = 99
		}
	}
	row.ElapsedSeconds = int(now.Sub(start).Seconds())
	if ends := parseTime(row.PhaseEndsAt); !ends.IsZero() && ends.After(now) {
		row.RemainingSeconds = int(ends.Sub(now).Seconds())
		row.NextOutputETASeconds = int(coolEnd.Sub(now).Seconds())
		if row.NextOutputETASeconds < 0 {
			row.NextOutputETASeconds = 0
		}
		row.NextOutputETA = humanDuration(time.Duration(row.NextOutputETASeconds) * time.Second)
	}
	row.SnapshotCounts = nousSnapshotCounts(buckets, start, baseEnd, expEnd, coolEnd)
	row.SnapshotsInPhase = row.SnapshotCounts[row.Phase]
	row.MetricsSoFar = nousMetrics(buckets, start, baseEnd, expEnd)
	return row
}

func (s *Server) nousLoadBuckets(since time.Time) []nousBucketLite {
	if s == nil || s.deps.Nous == nil || s.deps.Nous.HiveState() == nil {
		return nil
	}
	buckets, err := hivestate.Load(s.deps.Nous.SnapshotDir, since)
	if err != nil {
		return nil
	}
	out := make([]nousBucketLite, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, nousBucketLite{
			Start:              b.Start,
			Samples:            b.Samples,
			QueueIssues:        b.QueueIssues,
			QueuePRs:           b.QueuePRs,
			QueueHold:          b.QueueHold,
			SLAViolatedSamples: b.SLAViolatedSamples,
			Kicks:              b.Kicks,
			Tokens:             b.Tokens,
		})
	}
	return out
}

func nousScheduleHours(cfg map[string]interface{}) (float64, float64, float64) {
	base, exp, cool := 4.0, 4.0, 2.0
	if sched, ok := cfg["schedule"].(map[string]interface{}); ok {
		base = positiveFloat(sched["baseline_hours"], base)
		exp = positiveFloat(sched["experiment_duration_hours"], exp)
		cool = positiveFloat(sched["cooldown_hours"], cool)
	}
	return base, exp, cool
}

func positiveFloat(v interface{}, fallback float64) float64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return x
		}
	case int:
		if x > 0 {
			return float64(x)
		}
	case int64:
		if x > 0 {
			return float64(x)
		}
	}
	return fallback
}

func hoursDuration(hours float64) time.Duration {
	return time.Duration(hours * float64(time.Hour))
}

func nousSnapshotCounts(buckets []nousBucketLite, start, baseEnd, expEnd, coolEnd time.Time) map[string]int {
	counts := map[string]int{"baseline": 0, "experiment": 0, "cooldown": 0, "judging": 0}
	for _, b := range buckets {
		switch {
		case b.Start.Before(start):
			continue
		case b.Start.Before(baseEnd):
			counts["baseline"] += b.Samples
		case b.Start.Before(expEnd):
			counts["experiment"] += b.Samples
		case b.Start.Before(coolEnd):
			counts["cooldown"] += b.Samples
		default:
			counts["judging"] += b.Samples
		}
	}
	return counts
}

func nousMetrics(buckets []nousBucketLite, start, baseEnd, expEnd time.Time) []nousExperimentMetric {
	base, exp := splitBuckets(buckets, start, baseEnd, expEnd)
	out := []nousExperimentMetric{
		metricMedian("queue_issues_p50", "Queue issues p50", base.QueueIssues, exp.QueueIssues, ""),
		metricMedian("queue_prs_p50", "Queue PRs p50", base.QueuePRs, exp.QueuePRs, ""),
		metricMedian("queue_hold_p50", "Queue on hold p50", base.QueueHold, exp.QueueHold, ""),
		metricRate("sla_violation_rate", "SLA violation rate", base.SLAViolatedSamples, base.Samples, exp.SLAViolatedSamples, exp.Samples, "%"),
		metricSum("agents_kicked", "Agents kicked", base.Kicks, exp.Kicks, ""),
		metricSumInt64("tokens", "Tokens", base.Tokens, exp.Tokens, ""),
	}
	filtered := out[:0]
	for _, m := range out {
		if m.Baseline != nil || m.Experiment != nil {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

func splitBuckets(buckets []nousBucketLite, start, baseEnd, expEnd time.Time) (nousBucketLite, nousBucketLite) {
	var base, exp nousBucketLite
	for _, b := range buckets {
		if b.Start.Before(start) || !b.Start.Before(expEnd) {
			continue
		}
		dst := &exp
		if b.Start.Before(baseEnd) {
			dst = &base
		}
		dst.Samples += b.Samples
		dst.QueueIssues = append(dst.QueueIssues, b.QueueIssues...)
		dst.QueuePRs = append(dst.QueuePRs, b.QueuePRs...)
		dst.QueueHold = append(dst.QueueHold, b.QueueHold...)
		dst.SLAViolatedSamples += b.SLAViolatedSamples
		dst.Kicks += b.Kicks
		dst.Tokens += b.Tokens
	}
	return base, exp
}

func metricMedian(name, label string, baseVals, expVals []int, unit string) nousExperimentMetric {
	m := nousExperimentMetric{Name: name, Label: label, Unit: unit}
	if len(baseVals) > 0 {
		v := float64(nousMedianInt(baseVals))
		m.Baseline = &v
	}
	if len(expVals) > 0 {
		v := float64(nousMedianInt(expVals))
		m.Experiment = &v
	}
	if m.Baseline != nil && m.Experiment != nil {
		d := *m.Experiment - *m.Baseline
		m.Delta = &d
	}
	return m
}

func metricRate(name, label string, baseNum, baseDen, expNum, expDen int, unit string) nousExperimentMetric {
	m := nousExperimentMetric{Name: name, Label: label, Unit: unit}
	if baseDen > 0 {
		v := math.Round(float64(baseNum)*10000/float64(baseDen)) / 100
		m.Baseline = &v
	}
	if expDen > 0 {
		v := math.Round(float64(expNum)*10000/float64(expDen)) / 100
		m.Experiment = &v
	}
	if m.Baseline != nil && m.Experiment != nil {
		d := *m.Experiment - *m.Baseline
		m.Delta = &d
	}
	return m
}

func metricSum(name, label string, baseVal, expVal int, unit string) nousExperimentMetric {
	return metricSumInt64(name, label, int64(baseVal), int64(expVal), unit)
}

func metricSumInt64(name, label string, baseVal, expVal int64, unit string) nousExperimentMetric {
	m := nousExperimentMetric{Name: name, Label: label, Unit: unit}
	if baseVal > 0 {
		v := float64(baseVal)
		m.Baseline = &v
	}
	if expVal > 0 {
		v := float64(expVal)
		m.Experiment = &v
	}
	if m.Baseline != nil && m.Experiment != nil {
		d := *m.Experiment - *m.Baseline
		m.Delta = &d
	}
	return m
}

func nousMedianInt(vals []int) int {
	sorted := append([]int(nil), vals...)
	sort.Ints(sorted)
	return sorted[(len(sorted)-1)/2]
}

func nousOutputs(src map[string]interface{}, principles []NousPrinciple, experimentID string) []nousExperimentOutput {
	var out []nousExperimentOutput
	addURL := func(kind, title, url string) {
		url = strings.TrimSpace(url)
		if url != "" {
			out = append(out, nousExperimentOutput{Type: kind, Title: title, URL: url})
		}
	}
	for _, key := range []string{"pr_url", "prURL", "pull_request_url", "pullRequestURL"} {
		addURL("pr", "Pull request", firstString(src, key))
	}
	for _, key := range []string{"issue_url", "issueURL"} {
		addURL("issue", "Issue", firstString(src, key))
	}
	for _, key := range []string{"pr_urls", "issue_urls"} {
		if vals, ok := src[key].([]interface{}); ok {
			kind := "issue"
			title := "Issue"
			if strings.Contains(key, "pr_") {
				kind = "pr"
				title = "Pull request"
			}
			for _, v := range vals {
				addURL(kind, title, stringFromAny(v))
			}
		}
	}
	for _, p := range principles {
		if experimentID != "" && strings.EqualFold(p.Source, experimentID) {
			out = append(out, nousExperimentOutput{Type: "principle", Title: p.Text, ID: p.ID})
		}
	}
	return out
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if s := strings.TrimSpace(stringFromAny(m[key])); s != "" {
			return s
		}
	}
	return ""
}

func firstTime(m map[string]interface{}, keys ...string) time.Time {
	for _, key := range keys {
		if t := parseTime(m[key]); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func parseTime(v interface{}) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x.UTC()
	case string:
		if x == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UTC()
			}
		}
	case float64:
		if x > 0 {
			return time.Unix(int64(x), 0).UTC()
		}
	case int64:
		if x > 0 {
			return time.Unix(x, 0).UTC()
		}
	case int:
		if x > 0 {
			return time.Unix(int64(x), 0).UTC()
		}
	}
	return time.Time{}
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	h := int(math.Ceil(d.Hours()))
	if h >= 1 {
		return "~" + strconv.Itoa(h) + "h"
	}
	m := int(math.Ceil(d.Minutes()))
	if m < 1 {
		m = 1
	}
	return "~" + strconv.Itoa(m) + "m"
}

func nousCountForCampaign(rows []nousExperimentProgress, id int) int {
	n := 0
	for _, r := range rows {
		if r.CampaignID == id {
			n++
		}
	}
	return n
}
