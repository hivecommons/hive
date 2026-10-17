// Package fleetreport decides when a spoke should report hive-attributable
// ACMM shortfalls, unusual runtime errors, and Nous observations upstream to
// hivecommons/hive.
//
// The package is deterministic and side-effect free. Callers provide weekly
// observations, persisted state, and any runtime-error evidence; this package
// returns the report(s) that should be written or previewed.
package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	TriggerACMMShortfall = "acmm-shortfall"
	TriggerHiveDefect    = "hive-code-defect"
	TriggerObservation   = "observation"

	PersistentUnmetEpochs = 2
	DefaultEvidenceWindow = 10 * time.Minute
	periodicMinEvents     = 3
	periodTolerance       = 5 * time.Second
)

type ErrorEvent struct {
	At        time.Time
	Component string
	Agent     string
	Lane      string
	Class     string
}

type Evidence struct {
	Component    string        `json:"component"`
	Agent        string        `json:"agent,omitempty"`
	Lane         string        `json:"lane,omitempty"`
	ErrorClass   string        `json:"error_class"`
	Count        int           `json:"count"`
	Window       time.Duration `json:"window"`
	Periodicity  string        `json:"periodicity,omitempty"`
	Severity     string        `json:"severity"`
	Attributable bool          `json:"attributable"`
	// Runtime-error fields (TriggerRuntimeError). Class is empty for the two
	// older triggers.
	Class       string    `json:"class,omitempty"`
	Message     string    `json:"message,omitempty"`
	Diagnostics string    `json:"diagnostics,omitempty"`
	FirstSeen   time.Time `json:"first_seen,omitempty"`
	// Repo and Ref name the item the error was scoped to, when the class
	// carries one (owner/repo and e.g. "#123"). Rendered in the evidence
	// table and merged into the affected-repos union; never fingerprinted.
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type Observation struct {
	EpochStart time.Time
	HiveID     string
	Version    string
	Commit     string
	Mode       string
	ACMMLevel  int
	Unmet      []acmmadvisor.Criterion
	Evidence   []Evidence
	// HiveSlug is the hub-assigned slug for a hub-managed spoke. Empty keeps
	// the hive anonymous (instance digest only) in runtime-error reports.
	HiveSlug string
	// ProjectOrg is the project owner only. Observation reports deliberately
	// never carry the watched repository names or paths.
	ProjectOrg string
	Cluster    string
	// Repos are the owner/repo names this hive watches (project.repos), as
	// the operator's governor.fleet_report.include_repos policy allows them
	// to leave the hive. They render in the evidence table and feed the
	// affected-repos / affected-owners union on the fleet issue (#12338).
	Repos []string
	// Owners are the GitHub logins of the hive's owner(s): authorized users
	// holding the owner role plus the operator the spoke authenticated via
	// the device flow. They name WHO is affected on the fleet issue.
	// OwnersHidden is governor.fleet_report.include_owners: false.
	Owners       []string
	OwnersHidden bool
	// Now is the evaluation clock; zero means time.Now().
	Now time.Time
	// HiveState is the 24 h / 7 d hive state trends (#12339), copied onto
	// every report; nil when the hive has no recent snapshot.
	HiveState *hivestate.Trends
}

// ObservationFinding is the sink-facing shape produced by Nous detectors.
// It contains aggregate evidence only: the relay body must not expose project
// repository files or issue identifiers.
type ObservationFinding struct {
	Kind           string    `json:"kind"`
	Title          string    `json:"title"`
	Evidence       string    `json:"evidence"`
	SuspectedCause string    `json:"suspected_cause"`
	NextStep       string    `json:"next_step"`
	Fingerprint    string    `json:"fingerprint"`
	Series         []float64 `json:"series,omitempty"`
}

// ObservationMetadata is the hive-owned context rendered into an observation
// issue. ProjectOrg is intentionally the only project identity included.
type ObservationMetadata struct {
	HiveID     string
	HiveSlug   string
	ProjectOrg string
	ACMMLevel  int
	Version    string
	ClusterID  string
	HiveState  *hivestate.Trends
	Now        time.Time
}

type State struct {
	Criteria map[string]CriterionState `json:"criteria,omitempty"`
	Open     map[string]OpenIssue      `json:"open,omitempty"`
	// Filings records NEW issue creations (for the daily cap / min gap).
	Filings []time.Time `json:"filings,omitempty"`
}

type CriterionState struct {
	Epochs []time.Time `json:"epochs"`
}

type OpenIssue struct {
	Number       int    `json:"number"`
	URL          string `json:"url,omitempty"`
	OpenedByHive bool   `json:"opened_by_hive"`
	Recovered    bool   `json:"recovered,omitempty"`
	Criterion    string `json:"criterion,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
	BodyHash     string `json:"body_hash,omitempty"`
	Class        string `json:"class,omitempty"`
	// Joined is true when this hive added itself to an issue it did not open.
	Joined bool `json:"joined,omitempty"`
	// PostedAt is when this hive last wrote (create/comment) to the issue.
	PostedAt time.Time `json:"posted_at,omitempty"`
	// LastSeenCommentAt is when this hive last posted an "also seen" comment.
	LastSeenCommentAt time.Time `json:"last_seen_comment_at,omitempty"`
	// ClosedAt is set when the upstream issue was found closed; such a
	// fingerprint is not re-filed inside the closed cooldown.
	ClosedAt time.Time `json:"closed_at,omitempty"`
	// Deferred is the last reason a filing was held back (rate limit etc).
	Deferred string `json:"deferred,omitempty"`
}

type Report struct {
	Fingerprint string     `json:"fingerprint"`
	InstanceID  string     `json:"instance_id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Labels      []string   `json:"labels"`
	Criterion   string     `json:"criterion,omitempty"`
	Trigger     string     `json:"trigger"`
	Evidence    []Evidence `json:"evidence"`
	Recovered   bool       `json:"recovered,omitempty"`
	// Class and Hive are set for runtime-error and observation reports.
	Class string `json:"class,omitempty"`
	Hive  string `json:"hive,omitempty"`
	// Repos are the owner/repo names this report attributes to the hive:
	// its watched repos plus the triggering item's repo, already filtered by
	// the operator's include_repos policy. Joining merges them into the
	// issue's affected block.
	Repos []string `json:"repos,omitempty"`
	// Owners are the GitHub logins of this hive's owner(s) so a maintainer
	// can see WHO is affected; OwnersHidden is the operator's opt-out
	// (governor.fleet_report.include_owners: false), rendered "(anonymous)".
	Owners       []string `json:"owners,omitempty"`
	OwnersHidden bool     `json:"owners_hidden,omitempty"`
	// HiveState is Observation.HiveState. It is kept out of Body so trends
	// moving do not count as a changed report.
	HiveState *hivestate.Trends `json:"hive_state,omitempty"`
	// Observation carries the detector evidence used to build an observation
	// report. It is also sent on the relay wire so future sinks can render it
	// without parsing Markdown.
	Observation *ObservationFinding `json:"observation,omitempty"`
}

type Result struct {
	DryRun           bool     `json:"dry_run"`
	Reports          []Report `json:"reports,omitempty"`
	Recoveries       []Report `json:"recoveries,omitempty"`
	OperatorCriteria []string `json:"operator_criteria,omitempty"`
	State            State    `json:"state"`
}

func ClassifyErrors(events []ErrorEvent, now time.Time, window time.Duration) []Evidence {
	if window <= 0 {
		window = DefaultEvidenceWindow
	}
	start := now.Add(-window)
	groups := map[string][]ErrorEvent{}
	for _, e := range events {
		if e.At.Before(start) || e.At.After(now) || isBenign(e) || strings.TrimSpace(e.Class) == "" {
			continue
		}
		e.Component = safeToken(e.Component)
		e.Agent = safeToken(e.Agent)
		e.Lane = safeToken(e.Lane)
		e.Class = safeText(e.Class)
		key := strings.Join([]string{e.Component, e.Agent, e.Lane, strings.ToLower(e.Class)}, "\x00")
		groups[key] = append(groups[key], e)
	}
	out := make([]Evidence, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].At.Before(g[j].At) })
		period := periodicity(g)
		if period == "" && len(g) < periodicMinEvents {
			continue
		}
		sev := "medium"
		if period != "" {
			sev = "high"
		}
		out = append(out, Evidence{Component: g[0].Component, Agent: g[0].Agent, Lane: g[0].Lane, ErrorClass: g[0].Class, Count: len(g), Window: window, Periodicity: period, Severity: sev, Attributable: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity < out[j].Severity
		}
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].ErrorClass < out[j].ErrorClass
	})
	return out
}

func Evaluate(obs Observation, prev State, dryRun bool) Result {
	state := cloneState(prev)
	if state.Criteria == nil {
		state.Criteria = map[string]CriterionState{}
	}
	if state.Open == nil {
		state.Open = map[string]OpenIssue{}
	}
	instance := AnonymousInstanceID(obs.HiveID)
	version := safeToken(obs.Version)
	if version == "" {
		version = "unknown"
	}
	commit := shortCommit(obs.Commit)

	unmetKeys := map[string]string{}
	for _, c := range obs.Unmet {
		key := CriterionKey(c)
		if key == "" {
			continue
		}
		unmetKeys[key] = c.Name
		cs := state.Criteria[key]
		if !containsEpoch(cs.Epochs, obs.EpochStart) {
			cs.Epochs = append(cs.Epochs, obs.EpochStart.UTC())
			sort.Slice(cs.Epochs, func(i, j int) bool { return cs.Epochs[i].Before(cs.Epochs[j]) })
		}
		state.Criteria[key] = cs
	}

	res := Result{DryRun: dryRun, State: state}
	for key := range state.Criteria {
		if _, ok := unmetKeys[key]; !ok {
			delete(state.Criteria, key)
		}
	}
	if len(obs.Evidence) == 0 {
		for _, name := range unmetKeys {
			res.OperatorCriteria = append(res.OperatorCriteria, name)
		}
		sort.Strings(res.OperatorCriteria)
	}

	active := map[string]bool{}
	coveredByACMM := map[string]bool{}
	for key, name := range unmetKeys {
		if len(state.Criteria[key].Epochs) < PersistentUnmetEpochs {
			continue
		}
		for _, ev := range obs.Evidence {
			if !ev.Attributable || ev.Class != "" {
				continue
			}
			fp := Fingerprint(ev.ErrorClass, ev.Component, version, key)
			report := BuildReport(obs, key, name, ev, fp, instance, version, commit)
			active[fp] = true
			coveredByACMM[evidenceKey(ev)] = true
			open := state.Open[fp]
			wasRecovered := open.Recovered
			if wasRecovered {
				open.Recovered = false
				open.BodyHash = ""
			}
			if open.Criterion == "" {
				open.Criterion = key
			}
			if open.Trigger == "" {
				open.Trigger = TriggerACMMShortfall
			}
			state.Open[fp] = open
			if dryRun || wasRecovered || open.Number == 0 || open.BodyHash != StableBodyHash(report.Body) {
				res.Reports = append(res.Reports, report)
			}
		}
	}

	for _, ev := range obs.Evidence {
		if ev.Class != "" || !ev.Attributable || !isHiveCodeEvidence(ev) || coveredByACMM[evidenceKey(ev)] {
			continue
		}
		fp := DefectFingerprint(ev.ErrorClass, ev.Component, ev.Agent, version)
		report := BuildDefectReport(obs, ev, fp, instance, version, commit)
		active[fp] = true
		open := state.Open[fp]
		wasRecovered := open.Recovered
		if wasRecovered {
			open.Recovered = false
			open.BodyHash = ""
		}
		if open.Trigger == "" {
			open.Trigger = TriggerHiveDefect
		}
		state.Open[fp] = open
		if dryRun || wasRecovered || open.Number == 0 || open.BodyHash != StableBodyHash(report.Body) {
			res.Reports = append(res.Reports, report)
		}
	}

	now := obs.Now
	if now.IsZero() {
		now = time.Now()
	}
	for _, ev := range obs.Evidence {
		if ev.Class == "" || !IsKnownClass(ev.Class) {
			continue
		}
		fp := RuntimeFingerprint(ev.Class, ev.Message, version)
		report := BuildRuntimeReport(obs, ev, fp, instance, obs.HiveSlug, version, commit, now)
		active[fp] = true
		open := state.Open[fp]
		wasRecovered := open.Recovered
		if wasRecovered {
			open.Recovered = false
			open.BodyHash = ""
		}
		open.Trigger = TriggerRuntimeError
		open.Class = ev.Class
		state.Open[fp] = open
		// A runtime report is re-emitted while the hive has not yet written
		// to an issue for it; once posted, the publisher owns the once-per-24h
		// "also seen" cadence, so the body hash is not compared here.
		if dryRun || wasRecovered || open.Number == 0 || open.PostedAt.IsZero() {
			res.Reports = append(res.Reports, report)
		}
	}

	for fp, open := range state.Open {
		if open.Recovered || active[fp] {
			continue
		}
		trigger := openTrigger(open)
		switch trigger {
		case TriggerACMMShortfall:
			if _, stillUnmet := unmetKeys[open.Criterion]; !stillUnmet {
				res.Recoveries = append(res.Recoveries, RecoveryReport(open, Report{Fingerprint: fp, InstanceID: instance, Criterion: open.Criterion, Trigger: trigger}))
			}
		case TriggerHiveDefect:
			res.Recoveries = append(res.Recoveries, RecoveryReport(open, Report{Fingerprint: fp, InstanceID: instance, Trigger: trigger}))
		case TriggerRuntimeError:
			res.Recoveries = append(res.Recoveries, RecoveryReport(open, Report{Fingerprint: fp, InstanceID: instance, Trigger: trigger, Class: open.Class, Hive: obs.HiveSlug}))
		}
	}

	sort.Slice(res.Reports, func(i, j int) bool { return res.Reports[i].Fingerprint < res.Reports[j].Fingerprint })
	sort.Slice(res.Recoveries, func(i, j int) bool { return res.Recoveries[i].Fingerprint < res.Recoveries[j].Fingerprint })
	res.State = state
	return res
}

// BuildObservationReports converts detector findings into reports consumed by
// the existing self-report publisher. The detector remains pure; filing and
// deduplication stay in the publisher and hub relay.
func BuildObservationReports(obs Observation, findings []ObservationFinding) []Report {
	meta := ObservationMetadata{
		HiveID:     obs.HiveID,
		HiveSlug:   obs.HiveSlug,
		ProjectOrg: obs.ProjectOrg,
		ACMMLevel:  obs.ACMMLevel,
		Version:    obs.Version,
		ClusterID:  obs.Cluster,
		HiveState:  obs.HiveState,
		Now:        obs.Now,
	}
	reports := make([]Report, 0, len(findings))
	for _, finding := range findings {
		reports = append(reports, BuildObservationReport(finding, meta))
	}
	return reports
}

// BuildObservationReport renders one Nous detector finding without any watched
// repository identity. The daily series is JSON in the body so a downstream
// issue renderer can draw a sparkline without scraping prose.
func BuildObservationReport(finding ObservationFinding, meta ObservationMetadata) Report {
	kind := safeToken(finding.Kind)
	if kind == "" {
		kind = TriggerObservation
	}
	fingerprint := safeToken(finding.Fingerprint)
	if fingerprint == "" {
		fingerprint = safeToken(meta.HiveID) + ":" + kind
	}
	if fingerprint == ":"+kind {
		fingerprint = kind
	}
	title := safeText(strings.TrimSpace(finding.Title))
	if title == "" {
		title = "Nous observation: " + kind
	}
	series := observationSeriesJSON(finding.Series)
	labels := []string{
		LabelFleetReport,
		LabelObservation,
		LabelSelfReport,
		"observation:" + labelValue(kind),
		"trigger:" + TriggerObservation,
	}
	sort.Strings(labels)

	now := meta.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	hiveID := observationCell(meta.HiveID)
	hiveSlug := observationCell(meta.HiveSlug)
	projectOrg := observationCell(meta.ProjectOrg)
	version := observationCell(meta.Version)
	clusterID := observationCell(meta.ClusterID)
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- hive-fleet-fingerprint:%s -->\n", fingerprint)
	fmt.Fprintf(&b, "<!-- hive-observation-kind:%s -->\n\n", kind)
	fmt.Fprintf(&b, "Nous observed **%s** across the hive state series.\n\n", observationCell(title))
	b.WriteString("## Evidence\n\n| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| hive id | %s |\n", hiveID)
	fmt.Fprintf(&b, "| hive slug | %s |\n", hiveSlug)
	fmt.Fprintf(&b, "| project organization | %s |\n", projectOrg)
	fmt.Fprintf(&b, "| ACMM level | L%d |\n", meta.ACMMLevel)
	fmt.Fprintf(&b, "| hive version | %s |\n", version)
	fmt.Fprintf(&b, "| cluster_id | %s |\n", clusterID)
	fmt.Fprintf(&b, "| evidence | %s |\n", observationCell(finding.Evidence))
	fmt.Fprintf(&b, "| suspected cause | %s |\n", observationCell(finding.SuspectedCause))
	fmt.Fprintf(&b, "| next step | %s |\n", observationCell(finding.NextStep))
	fmt.Fprintf(&b, "| series | `%s` |\n", series)
	fmt.Fprintf(&b, "| reported at | %s |\n", now.UTC().Format(time.RFC3339))

	findingCopy := finding
	findingCopy.Kind = kind
	findingCopy.Fingerprint = fingerprint
	findingCopy.Series = append([]float64(nil), finding.Series...)
	return Report{
		Fingerprint: fingerprint,
		Title:       title,
		Body:        safeText(b.String()),
		Labels:      labels,
		Trigger:     TriggerObservation,
		Class:       TriggerObservation,
		Hive:        safeToken(meta.HiveSlug),
		HiveState:   meta.HiveState,
		Observation: &findingCopy,
	}
}

func observationSeriesJSON(series []float64) string {
	raw, err := json.Marshal(series)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func observationCell(value string) string {
	value = strings.Join(strings.Fields(safeText(value)), " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if value == "" {
		return "—"
	}
	return value
}

func BuildReport(obs Observation, criterionKey, criterionName string, ev Evidence, fingerprint, instance, version, commit string) Report {
	criterionLabel := "criterion:" + criterionKey
	componentLabel := "component:" + labelValue(ev.Component)
	severityLabel := "severity:" + labelValue(ev.Severity)
	versionLabel := "version:" + labelValue(version)
	instanceLabel := "instance:" + instance
	triggerLabel := "trigger:" + TriggerACMMShortfall
	labels := []string{"fleet-report", componentLabel, severityLabel, versionLabel, instanceLabel, criterionLabel, triggerLabel}
	sort.Strings(labels)
	title := fmt.Sprintf("Fleet report: %s blocks %s [%s]", ev.Component, criterionName, fingerprint)
	body := fmt.Sprintf(`<!-- hive-fleet-fingerprint:%s -->
<!-- hive-fleet-instance:%s -->

A spoke reports that a hive-attributable condition is preventing it from satisfying an ACMM criterion.

## Evidence

| Field | Value |
|---|---|
| anonymous instance | %s |
| hive version | %s |
| hive commit | %s |
| mode | %s |
| ACMM level | L%d |
| unmet criterion | %s (%s) |
| component | %s |
| agent/lane | %s / %s |
| error class | %s |
| count + window | %s |
| detected periodicity | %s |
| self-recovered | no |
`, fingerprint, instance, instance, version, commit, safeToken(obs.Mode), obs.ACMMLevel, criterionName, criterionKey, ev.Component, emptyDash(ev.Agent), emptyDash(ev.Lane), ev.ErrorClass, countWindow(ev.Count, ev.Window), emptyDash(ev.Periodicity))
	return Report{Fingerprint: fingerprint, InstanceID: instance, Title: safeText(title), Body: safeText(body), Labels: labels, Criterion: criterionKey, Trigger: TriggerACMMShortfall, Evidence: []Evidence{ev}, HiveState: obs.HiveState}
}

func BuildDefectReport(obs Observation, ev Evidence, fingerprint, instance, version, commit string) Report {
	componentLabel := "component:" + labelValue(ev.Component)
	severityLabel := "severity:" + labelValue(ev.Severity)
	versionLabel := "version:" + labelValue(version)
	instanceLabel := "instance:" + instance
	triggerLabel := "trigger:" + TriggerHiveDefect
	labels := []string{"fleet-report", componentLabel, severityLabel, versionLabel, instanceLabel, triggerLabel}
	sort.Strings(labels)
	title := fmt.Sprintf("Fleet report: %s hive-code defect [%s]", ev.Component, fingerprint)
	body := fmt.Sprintf(`<!-- hive-fleet-fingerprint:%s -->
<!-- hive-fleet-instance:%s -->

A spoke reports a symptom attributable to hive's own code paths. No ACMM shortfall is required for this trigger.

## Evidence

| Field | Value |
|---|---|
| trigger | %s |
| anonymous instance | %s |
| hive version | %s |
| hive commit | %s |
| mode | %s |
| ACMM level | L%d |
| component | %s |
| agent/lane | %s / %s |
| error class | %s |
| count + window | %s |
| detected periodicity | %s |
| self-recovered | no |
`, fingerprint, instance, TriggerHiveDefect, instance, version, commit, safeToken(obs.Mode), obs.ACMMLevel, ev.Component, emptyDash(ev.Agent), emptyDash(ev.Lane), ev.ErrorClass, countWindow(ev.Count, ev.Window), emptyDash(ev.Periodicity))
	return Report{Fingerprint: fingerprint, InstanceID: instance, Title: safeText(title), Body: safeText(body), Labels: labels, Trigger: TriggerHiveDefect, Evidence: []Evidence{ev}, HiveState: obs.HiveState}
}

func RecoveryReport(open OpenIssue, report Report) Report {
	report.Recovered = true
	report.Body = safeText(fmt.Sprintf("<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n\nRecovery: %s\n\nself-recovered: yes\n", report.Fingerprint, report.InstanceID, recoverySubject(report)))
	return report
}

// recoverySubject names what stopped being observed. A hive-code defect has no
// ACMM criterion attached, so naming one would render an empty reference.
func recoverySubject(report Report) string {
	if criterion := strings.TrimSpace(report.Criterion); criterion != "" {
		return fmt.Sprintf("this spoke no longer observes the ACMM shortfall for `%s`.", criterion)
	}
	if report.Trigger == TriggerRuntimeError {
		return fmt.Sprintf("this hive no longer observes runtime error class `%s`.", report.Class)
	}
	return "this spoke no longer observes the reported hive-code defect."
}

func Fingerprint(errorClass, component, version, criterion string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{strings.ToLower(strings.TrimSpace(errorClass)), strings.ToLower(strings.TrimSpace(component)), strings.ToLower(strings.TrimSpace(version)), strings.ToLower(strings.TrimSpace(criterion))}, "|")))
	return hex.EncodeToString(h[:])[:24]
}

func DefectFingerprint(errorClass, component, agent, version string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{TriggerHiveDefect, strings.ToLower(strings.TrimSpace(errorClass)), strings.ToLower(strings.TrimSpace(component)), strings.ToLower(strings.TrimSpace(agent)), strings.ToLower(strings.TrimSpace(version))}, "|")))
	return hex.EncodeToString(h[:])[:24]
}

func AnonymousInstanceID(hiveID string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(hiveID)))
	return hex.EncodeToString(h[:])[:16]
}

func StableBodyHash(body string) string {
	h := sha256.Sum256([]byte(logscrub.ScrubString(body)))
	return hex.EncodeToString(h[:])[:16]
}

func CriterionKey(c acmmadvisor.Criterion) string { return labelValue(c.Name) }

func isBenign(e ErrorEvent) bool {
	class := strings.ToLower(e.Class)
	component := strings.ToLower(e.Component)
	return strings.Contains(class, "i/o timeout") && (strings.Contains(component, "read") || strings.Contains(component, "keepalive"))
}

func evidenceKey(ev Evidence) string {
	return strings.Join([]string{strings.ToLower(strings.TrimSpace(ev.Component)), strings.ToLower(strings.TrimSpace(ev.Agent)), strings.ToLower(strings.TrimSpace(ev.Lane)), strings.ToLower(strings.TrimSpace(ev.ErrorClass))}, "\x00")
}

func isHiveCodeEvidence(ev Evidence) bool {
	component := strings.ToLower(strings.TrimSpace(ev.Component))
	if component == "" || strings.Contains(component, "fleet-report") || strings.Contains(component, "reporting") {
		return false
	}
	owned := []string{"proxy", "scheduler", "request-watcher", "watcher", "dashboard", "agent-runtime", "agent-lifecycle", "backend-auth", "inference-gateway", "merge", "pr-title", "github-client"}
	for _, prefix := range owned {
		if strings.Contains(component, prefix) {
			return true
		}
	}
	return false
}

func openTrigger(open OpenIssue) string {
	if open.Trigger != "" {
		return open.Trigger
	}
	if open.Criterion != "" {
		return TriggerACMMShortfall
	}
	return TriggerHiveDefect
}

func periodicity(events []ErrorEvent) string {
	if len(events) < periodicMinEvents {
		return ""
	}
	base := events[1].At.Sub(events[0].At)
	if base <= 0 {
		return ""
	}
	for i := 2; i < len(events); i++ {
		d := events[i].At.Sub(events[i-1].At)
		if d <= 0 || absDuration(d-base) > periodTolerance {
			return ""
		}
	}
	return base.Round(time.Second).String()
}

func cloneState(in State) State {
	out := State{Criteria: map[string]CriterionState{}, Open: map[string]OpenIssue{}}
	for k, v := range in.Criteria {
		out.Criteria[k] = CriterionState{Epochs: append([]time.Time(nil), v.Epochs...)}
	}
	for k, v := range in.Open {
		out.Open[k] = v
	}
	out.Filings = append([]time.Time(nil), in.Filings...)
	return out
}

func containsEpoch(epochs []time.Time, epoch time.Time) bool {
	for _, e := range epochs {
		if e.Equal(epoch.UTC()) {
			return true
		}
	}
	return false
}

func safeToken(s string) string { return strings.TrimSpace(logscrub.ScrubString(s)) }
func safeText(s string) string  { return logscrub.ScrubString(s) }

func labelValue(s string) string {
	s = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(s)))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortCommit(s string) string {
	s = safeToken(s)
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}

// countWindow renders the count/window cell. A non-positive window means the
// count is not windowed, so no observation period is claimed for it.
func countWindow(count int, window time.Duration) string {
	if window <= 0 {
		return fmt.Sprintf("%d (cumulative)", count)
	}
	return fmt.Sprintf("%d in %s", count, window)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
