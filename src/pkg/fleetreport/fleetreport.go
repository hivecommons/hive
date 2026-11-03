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

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/hivestate"
)

const (
	TriggerACMMShortfall = "acmm-shortfall"
	TriggerObservation   = "observation"
	// legacyTriggerHiveDefect is the retired standalone "hive-code defect"
	// trigger (#13470); it survives only to recognise old state entries.
	legacyTriggerHiveDefect = "hive-code-defect"

	// SelfReportTitlePrefix is the title every hive-filed issue starts with
	// since #13470: runtime-error classes and ACMM shortfalls alike land in
	// a `[selfreport]` tracker issue.
	SelfReportTitlePrefix = "[selfreport] "
	// LegacyStandaloneTitlePrefix is the retired standalone title format
	// ("Fleet report: <component> blocks <criterion> [<fp>]" and "Fleet
	// report: <component> hive-code defect [<fp>]"). Nothing builds it any
	// more; the hub relay and the governor sweep recognise it so a spoke
	// still running a pre-#13470 build cannot land one.
	LegacyStandaloneTitlePrefix = "Fleet report:"

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
	// HiveName is the hive's human display name (the hub's project_name,
	// else the spoke's project.name) and SpokeURL its public dashboard URL.
	// Both are set only for a hub-managed spoke (HiveSlug != ""), so an
	// anonymous hive stays anonymous; empty values render as the hive id
	// and "—" (#13211).
	HiveName string
	SpokeURL string
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
	// Component is the hive component the issue is about; an ACMM shortfall
	// on the same component rolls up onto an open self-report instead of
	// filing its own issue (#13470). RollupOf is set on the shortfall's own
	// entry when it did: Number/URL then name the self-report issue.
	Component string `json:"component,omitempty"`
	RollupOf  string `json:"rollup_of,omitempty"`
	// Joined is true when this hive added itself to an issue it did not open.
	Joined bool `json:"joined,omitempty"`
	// PostedAt is when this hive last wrote (create/comment) to the issue.
	PostedAt time.Time `json:"posted_at,omitempty"`
	// LastSeenCommentAt is when this hive last posted an "also seen" comment.
	LastSeenCommentAt time.Time `json:"last_seen_comment_at,omitempty"`
	// ClosedAt is set when the upstream issue was found closed; such a
	// fingerprint is not re-filed while ClosedSuppressed holds.
	// ClosedObservedAt is the last time this hive observed the condition
	// while the issue was closed; it keeps the suppression alive for as long
	// as the condition persists (#14256).
	ClosedAt         time.Time `json:"closed_at,omitempty"`
	ClosedObservedAt time.Time `json:"closed_observed_at,omitempty"`
	// ClearedPostedAt is when this hive last posted a runtime "cleared"
	// comment; AllowClearedComment paces the next one.
	ClearedPostedAt time.Time `json:"cleared_posted_at,omitempty"`
	// Deferred is the last reason a filing was held back (rate limit etc).
	Deferred string `json:"deferred,omitempty"`
	// ClearedAttempts / ClearedNextAttemptAt pace retries of a runtime
	// "cleared" comment after a transient failure; ClearedNotifyFailed is
	// the error that made the publisher give up on it (#13827).
	ClearedAttempts      int       `json:"cleared_attempts,omitempty"`
	ClearedNextAttemptAt time.Time `json:"cleared_next_attempt_at,omitempty"`
	ClearedNotifyFailed  string    `json:"cleared_notify_failed,omitempty"`
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
	// RollupOf, when set, names the fingerprint of this hive's open
	// self-report that this ACMM-shortfall report rolls up onto: the
	// publisher posts Body as a comment there instead of filing a standalone
	// issue (#13470).
	RollupOf string `json:"rollup_of,omitempty"`
	// Class and Hive are set for runtime-error and observation reports.
	Class string `json:"class,omitempty"`
	Hive  string `json:"hive,omitempty"`
	// HiveName and SpokeURL are Observation.HiveName / SpokeURL (#13211);
	// they travel on the relay wire so the hub can render or complete them.
	HiveName string `json:"hive_name,omitempty"`
	SpokeURL string `json:"spoke_url,omitempty"`
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

	now := obs.Now
	if now.IsZero() {
		now = time.Now()
	}
	active := map[string]bool{}
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
			open.Component = report.Evidence[0].Component
			if open.Number == 0 || open.RollupOf != "" {
				// Not yet written (or already rolled up): if this hive has an
				// open self-report tracker about the same component, the
				// shortfall is one more symptom of that defect and rolls up
				// onto it as a comment. Otherwise the report below opens the
				// tracker itself (a `[selfreport]` issue carrying the
				// shortfall section) — never a standalone "Fleet report:"
				// issue. A roll-up whose target closed starts over the same
				// way.
				if target, ok := OpenSelfReportFor(state, ev.Component); ok {
					report = RollupReport(report, target, obs.HiveSlug, instance, version)
				} else if open.RollupOf != "" {
					open = OpenIssue{Criterion: open.Criterion, Trigger: open.Trigger, Component: open.Component}
				}
			}
			state.Open[fp] = open
			if dryRun || wasRecovered || open.Number == 0 || open.BodyHash != StableBodyHash(report.Body) {
				res.Reports = append(res.Reports, report)
			}
		}
	}

	for _, ev := range obs.Evidence {
		if ev.Class == "" || !IsKnownClass(ev.Class) {
			continue
		}
		fp := RuntimeFingerprint(ev.Class, ev.Message)
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
		open.Component = ev.Component
		state.Open[fp] = open
		// A runtime report is re-emitted while the hive has not yet written
		// to an issue for it; once posted, the publisher owns the once-per-24h
		// "also seen" cadence, so the body hash is not compared here.
		if dryRun || wasRecovered || open.Number == 0 || open.PostedAt.IsZero() {
			res.Reports = append(res.Reports, report)
		}
	}

	for fp, open := range state.Open {
		if openTrigger(open) == legacyTriggerHiveDefect {
			// Retired trigger (#13470): forget the entry so the state file
			// does not carry dead fingerprints forever.
			delete(state.Open, fp)
			continue
		}
		if open.Recovered || active[fp] {
			continue
		}
		trigger := openTrigger(open)
		switch trigger {
		case TriggerACMMShortfall:
			if _, stillUnmet := unmetKeys[open.Criterion]; !stillUnmet {
				res.Recoveries = append(res.Recoveries, RecoveryReport(open, Report{Fingerprint: fp, InstanceID: instance, Criterion: open.Criterion, Trigger: trigger}))
			}
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

// BuildReport renders an ACMM-shortfall report as this hive's self-report
// tracker for the component (#13470): a `[selfreport]` issue with the same
// identity rows, label set, Affected-hives block and diagnostics a
// runtime-error report carries, plus an "ACMM shortfall" section naming the
// unmet criterion. It is what gets filed when no tracker about the component
// is open yet; with one open the shortfall rolls up onto it (RollupReport).
// There is no standalone "Fleet report:" format any more.
func BuildReport(obs Observation, criterionKey, criterionName string, ev Evidence, fingerprint, instance, version, commit string) Report {
	hive := strings.TrimSpace(obs.HiveSlug)
	if hive == "" {
		hive = instance
	}
	labels := SelfReportLabels(TriggerACMMShortfall, ev.Severity, "")
	labels = append(labels, "criterion:"+criterionKey)
	sort.Strings(labels)
	title := fmt.Sprintf("%s%s: %s blocks %s", SelfReportTitlePrefix, TriggerACMMShortfall, ev.Component, criterionName)
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n%s\n\n", fingerprint, instance, SelfReportMarker(fingerprint, hive))
	fmt.Fprintf(&b, "A hive cannot satisfy an ACMM criterion because of a hive-attributable condition in `%s`, and is opening its self-report tracker for that component so the same defect across the fleet lands in one issue.\n\n", ev.Component)
	b.WriteString("## Evidence\n\n| Field | Value |\n|---|---|\n")
	name, spokeURL := writeIdentityRows(&b, obs, hive, instance, version, commit)
	fmt.Fprintf(&b, "| ACMM level | L%d |\n", obs.ACMMLevel)
	fmt.Fprintf(&b, "| unmet criterion | %s (%s) |\n", criterionName, criterionKey)
	fmt.Fprintf(&b, "| component | %s |\n", ev.Component)
	fmt.Fprintf(&b, "| agent/lane | %s / %s |\n", emptyDash(ev.Agent), emptyDash(ev.Lane))
	fmt.Fprintf(&b, "| error class | %s |\n", ev.ErrorClass)
	fmt.Fprintf(&b, "| count + window | %s |\n", countWindow(ev.Count, ev.Window))
	fmt.Fprintf(&b, "| detected periodicity | %s |\n", emptyDash(ev.Periodicity))
	repos := NormalizeRepos(obs.Repos)
	fmt.Fprintf(&b, "| repos | %s |\n", repoListCell(repos))
	fmt.Fprintf(&b, "| owners | %s |\n", repoListCell(OwnersOf(repos)))
	b.WriteString("| self-recovered | no |\n\n")
	owners := reportOwners(obs)
	// No timestamps in the body: an ACMM report is re-emitted only when its
	// body changes, so the hive's last-seen is left to the join path.
	b.WriteString(AffectedBlock{Hives: []AffectedHive{{Hive: hive, Name: name, URL: spokeURL, Instance: instance, Owners: owners, OwnersHidden: obs.OwnersHidden, Seen: 1}}, Repos: repos, Seen: 1}.Render())
	b.WriteString("\n\n## ACMM shortfall\n\n")
	b.WriteString(shortfallSummary(hive, version, ev, criterionKey))
	b.WriteString("\n")
	if ev.Diagnostics != "" {
		fmt.Fprintf(&b, "\n<details><summary>Diagnostics (scrubbed)</summary>\n\n```\n%s\n```\n</details>\n", ev.Diagnostics)
	}
	return Report{Fingerprint: fingerprint, InstanceID: instance, Title: safeText(title), Body: safeText(b.String()), Labels: labels, Criterion: criterionKey, Trigger: TriggerACMMShortfall, Hive: hive, HiveName: name, SpokeURL: spokeURL, Evidence: []Evidence{ev}, Repos: repos, Owners: owners, OwnersHidden: obs.OwnersHidden, HiveState: obs.HiveState}
}

// shortfallSummary is the one-line statement of an ACMM shortfall shared by
// the tracker body and the roll-up comment.
func shortfallSummary(hive, version string, ev Evidence, criterion string) string {
	return fmt.Sprintf("ACMM shortfall on **%s** (%s): `%s` (%s) is blocking criterion `%s` — %s.",
		labelValue(hive), displayVersion(version), ev.Component, emptyDash(ev.ErrorClass), criterion, countWindow(ev.Count, ev.Window))
}

// IsLegacyStandaloneTitle reports whether title is the retired standalone
// "Fleet report: …" format (#13470). The hub relay refuses to file it and the
// governor sweep closes any that still land.
func IsLegacyStandaloneTitle(title string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), strings.ToLower(LegacyStandaloneTitlePrefix))
}

// OpenSelfReportFor finds this hive's open self-report tracker about
// component: a runtime-error entry, or an ACMM-shortfall entry that opened
// the tracker itself (not a roll-up), with an issue number, not recovered,
// not found closed. Ties resolve to the lowest fingerprint so the choice is
// stable across cycles.
func OpenSelfReportFor(st State, component string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(component))
	if want == "" {
		return "", false
	}
	best := ""
	for fp, open := range st.Open {
		if open.Number <= 0 || open.Recovered || !open.ClosedAt.IsZero() {
			continue
		}
		switch open.Trigger {
		case TriggerRuntimeError:
		case TriggerACMMShortfall:
			if open.RollupOf != "" {
				continue
			}
		default:
			continue
		}
		if strings.ToLower(strings.TrimSpace(open.Component)) != want {
			continue
		}
		if best == "" || fp < best {
			best = fp
		}
	}
	return best, best != ""
}

// RollupReport turns an ACMM-shortfall report into a roll-up onto the open
// self-report target: Body becomes the short comment the publisher posts
// there and RollupOf names the target fingerprint. The report's own
// fingerprint, criterion and labels are kept so state bookkeeping is
// unchanged. The body carries no timestamp so an unchanged shortfall is not
// re-posted every cycle.
func RollupReport(report Report, target, hive, instance, version string) Report {
	if strings.TrimSpace(hive) == "" {
		hive = instance
	}
	var ev Evidence
	if len(report.Evidence) > 0 {
		ev = report.Evidence[0]
	}
	var b strings.Builder
	b.WriteString(SeenMarker(report.Fingerprint, hive))
	b.WriteString("\n")
	b.WriteString(strings.Replace(shortfallSummary(hive, version, ev, report.Criterion), "ACMM shortfall on", "ACMM shortfall rolled up from", 1))
	b.WriteString(" Folded onto this self-report because it is about the same component; no separate issue was filed.")
	report.Body = safeText(b.String())
	report.RollupOf = target
	return report
}

func RecoveryReport(open OpenIssue, report Report) Report {
	report.Recovered = true
	report.Body = safeText(fmt.Sprintf("<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n\nRecovery: %s\n\nself-recovered: yes\n", report.Fingerprint, report.InstanceID, recoverySubject(report)))
	return report
}

// recoverySubject names what stopped being observed.
func recoverySubject(report Report) string {
	if criterion := strings.TrimSpace(report.Criterion); criterion != "" {
		return fmt.Sprintf("this spoke no longer observes the ACMM shortfall for `%s`.", criterion)
	}
	if report.Trigger == TriggerRuntimeError {
		return fmt.Sprintf("this hive no longer observes runtime error class `%s`.", report.Class)
	}
	return "this spoke no longer observes the reported condition."
}

func Fingerprint(errorClass, component, version, criterion string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{strings.ToLower(strings.TrimSpace(errorClass)), strings.ToLower(strings.TrimSpace(component)), strings.ToLower(strings.TrimSpace(version)), strings.ToLower(strings.TrimSpace(criterion))}, "|")))
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
	return benignIOTimeout(strings.ToLower(e.Class), strings.ToLower(e.Component))
}

// benignIOTimeout is the one exclusion both the ACMM classifier and the
// generic log feed share: a read/keepalive i/o timeout is network weather,
// not a hive defect.
func benignIOTimeout(class, component string) bool {
	return strings.Contains(class, "i/o timeout") && (strings.Contains(component, "read") || strings.Contains(component, "keepalive"))
}

// benignSubstrings are lower-case fragments whose presence marks a log line
// as noise for the generic repeated-error feed (see IsBenignMessage). Keep
// each entry specific enough that it cannot swallow a real defect.
var benignSubstrings = []string{
	// Shutdown and per-request deadlines log cancellation on purpose.
	"context canceled",
	"context cancelled",
	// The permissions watcher's "fixed ..." lines (file/directory/mode
	// permissions, ownership) record a repair that already succeeded:
	// self-healing housekeeping, whatever level the producer logs at.
	"permissions watcher: fixed ",
	// Request watchers log a policy denial (e.g. repo not allowlisted) as an
	// expected outcome, not a runtime error.
	"request watcher: denied (policy)",
	// Self-healing, informational lines: a cached GitHub 404 suppressing
	// repeat fetches, and a rate-limited Copilot token validation that the
	// manager retries itself after a cooldown.
	"cached; suppressing repeated fetches",
	"token validation rate-limited by github; relaunching after cooldown",
	// Request watchers log a policy denial (e.g. repo not allowlisted) as an
	// expected outcome, not a runtime error.
	"request watcher: denied (policy)",
	// A reviewer's abbreviated verdict head that prefixes neither the dispatch
	// head nor the PR's current head names an older revision; the relay keeps
	// it as written on purpose (pkg/github/review_verdict_relay.go), so it is
	// an expected outcome of a push landing mid-review, not a runtime defect.
	"abbreviated verdict head is not the pr's current head; recorded as written",
	// ReconcileSeededDefaults (pkg/policies/seed_reconcile.go) warns once per
	// policy template whose on-disk copy diverges from the embedded default
	// with no seed-manifest record, already deduped per path per process
	// (hivecommons/hive#13825). A hive whose policy directory predates that
	// manifest tracking (hivecommons/hive#9428) has every template diverge
	// on its first post-upgrade reconcile — up to one WARN per template
	// within the same boot — and a burst across unrelated templates is not
	// a repeated runtime defect (hivecommons/hive#14005).
	"seeded policy differs from the embedded default with no seed record",
	// The agent poller (src/crux/agent/manager_poll.go) warns once per agent
	// per (requested, running) tuple when the Copilot CLI rejects --model and
	// runs another model (#13659), and surfaces it on the agent's LastError.
	// A hive whose copilot agents share one model the account cannot use has
	// every agent warn once in the same boot — an operator configuration
	// condition, not a repeated runtime defect (hivecommons/hive#14020).
	"copilot cli substituted the configured model",
	// The scheduler (src/pkg/scheduler/kick_priming.go) warns when the primer
	// render exceeds knowledge.primer.max_bytes (#12124). The render already
	// carries its own truncation marker. A hive whose vault outgrows the cap
	// has every kick warn once — an operator configuration condition (raise
	// the cap or tighten the scope), not a repeated runtime defect
	// (hivecommons/hive#14173).
	"knowledge primer: dropped facts over the byte cap",
	// The contribute-ws task selector (src/pkg/dashboard/contribute_select.go)
	// warns on every poll from an identity whose claims are paused because
	// its recent assignments all died within seconds (#6450). The pause is
	// the protection working as designed and the refusal is a policy
	// outcome, re-logged for as long as the paused agent keeps polling —
	// not a repeated runtime defect (hivecommons/hive#14178). Only this
	// refusal is benign: "refusing task: lease could not be persisted" is a
	// real failure and must remain reportable.
	"refusing task: contributor failure streak",
	// Both automerge CI gates warn when configured fallback checks are stale,
	// while continuing to use the repository's actual protection checks.
	// This is operator configuration drift, not a runtime defect (#14351).
	"automerge required_checks override does not match repo protection",
	// The mutation boundary (src/pkg/convergence/mutation/boundary.go) warns
	// in shadow mode (fencing disabled) when the ledger Acquire would have
	// denied an external mutation. Shadow mode observes rather than denies:
	// nothing was blocked and Stats.IncFenced() is the telemetry channel. A
	// burst from concurrent agents on one repo is expected observational
	// output, not a repeated runtime defect (hivecommons/hive#14377). The
	// enforce-mode denial (effects.ErrDenied) stays reportable.
	"shadow fence would have denied external mutation",
	// The forward-merge guard (src/pkg/github/pullrequest.go) warns each time
	// it overrides a squash request on a forward-merge PR with a merge commit.
	// That is the merge-safety policy working as designed, re-logged per
	// merge attempt, not a repeated runtime defect (hivecommons/hive#14727).
	"forward-merge guard: refusing to squash",
	// The stage labeller (src/pkg/github/automerge/automerge_stage_labels.go)
	// warns once per PR per quiet period when a PR waiting on the hive
	// (review pending, checks running) sits past its stage dwell, and
	// surfaces the same condition as the dashboard's hive-held count
	// (hivecommons/hive#12534). The feed keys on the message alone, so a
	// busy hive whose reviewer lane is backed up has every held PR warn once
	// and the burst across unrelated PRs reads as one repeated defect — it
	// is a per-hive operational signal, not a runtime error
	// (hivecommons/hive#15009).
	"stage labels: pr waits on the hive past the stage dwell",
	// The kick-undeliverable WARN (src/crux/agent/busy_visibility.go) says in
	// its own message that it is not self-reporting: until busyNoActivityStall
	// has restarted the pane the condition is a dashboard warning and a log
	// line only (decision B on hivecommons/hive#12787). The WARN repeats on
	// every refused kick, so the generic feed filed it as a repeated runtime
	// defect anyway (hivecommons/hive#15313). Kicks that keep failing after
	// the restart are reported under kick_failed_repeatedly instead.
	"busy-no-activity restart not yet attempted, not self-reporting",
}

// rateLimitSubstrings are lower-case fragments that mark a GitHub rate-limit
// refusal or the hive's own pacing shedding a request while the caution
// window is engaged (see IsGitHubRateLimited). One rate-limit episode makes
// every caller log its own WARN — check runs, statuses, permissions, repo
// enumeration, advisories — and each distinct message used to cross the
// repeat threshold as its own unclassified_error_repeated issue
// (hivecommons/hive#15106). The condition is self-limiting: the client
// backs off and the callers retry on their next tick.
var rateLimitSubstrings = []string{
	"api secondary rate limit exceeded",
	"exceeded a secondary rate limit",
	"api rate limit exceeded",
	"api rate limit of",
	"not making remote request",
	"429 too many requests",
	"github pacing: request shed",
	"github pacing: slot would miss request deadline",
	"rate-limit caution window is engaged",
}

// IsGitHubRateLimited reports whether s (a log message or an error string)
// is a GitHub rate-limit refusal or a pacing shed. The generic repeated-error
// feed treats such lines as benign; the WARN itself stays in local logs.
func IsGitHubRateLimited(s string) bool {
	m := strings.ToLower(s)
	for _, sub := range rateLimitSubstrings {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// upstreamUnavailableSubstrings are lower-case fragments that mark a GitHub
// gateway-class transient failure (see IsGitHubUpstreamUnavailable). The
// status codes are anchored to go-github's "<METHOD> <url>: 503 ..." shape so
// a PR number or page=503 cannot match; 500 is deliberately absent. The
// status phrases are anchored to the same ": " separator so prose that merely
// mentions them does not match.
var upstreamUnavailableSubstrings = []string{
	": 502 ",
	": 503 ",
	": 504 ",
	": bad gateway",
	": service unavailable",
	": gateway timeout",
	": gateway time-out",
}

// IsGitHubUpstreamUnavailable reports whether s (a log message or an error
// string) is a GitHub 502/503/504 gateway-class failure. The caller retries
// on its next tick, so the generic repeated-error feed treats such lines as
// benign (hivecommons/hive#15365); the WARN stays in local logs.
func IsGitHubUpstreamUnavailable(s string) bool {
	m := strings.ToLower(s) + " "
	for _, sub := range upstreamUnavailableSubstrings {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// IsBenignMessage reports whether a raw log message is noise the generic
// repeated-error feed must not report: the read/keepalive i/o timeout rule
// above applied to the whole line, GitHub rate-limit and pacing refusals
// (IsGitHubRateLimited), gateway failures (IsGitHubUpstreamUnavailable),
// every benignSubstrings entry, and the
// exact-message exclusions matched in the function body below.
func IsBenignMessage(msg string) bool {
	m := strings.ToLower(msg)
	// A CI gate refusal is the merge safety policy working as designed, even
	// when a required check remains unsuccessful across repeated polls (#14364).
	// Keep the operator warning, but do not turn it into a runtime defect.
	if strings.TrimSpace(m) == "merge-request watcher: refused to merge, ci not confirmed" {
		return true
	}
	// Pending expiry is a bounded retry, logged per perspective/PR. A burst
	// across unrelated reviews is not a repeated runtime defect (#13850).
	// Match only the retry notification: "review dispatch exhausted; holding
	// PR for a human" and failures during expiry must remain reportable.
	if strings.TrimSpace(m) == "review pending expired" {
		return true
	}
	// Per-agent quota enforcement is expected backpressure, not a runtime
	// defect. Different agents/windows can cross the repeat threshold even
	// though the proxy already warns only once per agent per window (#14346).
	switch strings.TrimSpace(m) {
	case "agent github api hourly cap reached", "agent github api hourly budget above soft threshold":
		return true
	}
	// Contributor tier limits are normal policy enforcement on each poll,
	// not runtime failures (#14414). Match the complete notices so failures
	// to enforce or persist a limit remain reportable.
	switch strings.TrimSpace(m) {
	case "[contribute-ws] refusing task: concurrency limit reached",
		"[contribute-ws] refusing task: hourly rate limit reached",
		"[contribute-ws] refusing task: daily rate limit reached":
		return true
	}
	if benignIOTimeout(m, m) || IsGitHubRateLimited(m) || IsGitHubUpstreamUnavailable(m) {
		return true
	}
	for _, s := range benignSubstrings {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// openTrigger is the trigger of a persisted entry; entries written before
// the field existed were ACMM shortfalls when they carried a criterion, and
// hive-code-defect issues (a trigger retired by #13470) otherwise — those are
// left alone rather than recovered under a trigger that no longer exists.
func openTrigger(open OpenIssue) string {
	if open.Trigger != "" {
		return open.Trigger
	}
	if open.Criterion != "" {
		return TriggerACMMShortfall
	}
	return legacyTriggerHiveDefect
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
