// Package fleetreport decides when a spoke should report hive-attributable
// ACMM shortfalls upstream to hivecommons/hive.
//
// The package is deterministic and side-effect free. Callers provide weekly
// observations, persisted state, and any runtime-error evidence; this package
// returns the report(s) that should be written or previewed.
package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
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
}

type State struct {
	Criteria map[string]CriterionState `json:"criteria,omitempty"`
	Open     map[string]OpenIssue      `json:"open,omitempty"`
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
	BodyHash     string `json:"body_hash,omitempty"`
}

type Report struct {
	Fingerprint string     `json:"fingerprint"`
	InstanceID  string     `json:"instance_id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Labels      []string   `json:"labels"`
	Criterion   string     `json:"criterion"`
	Evidence    []Evidence `json:"evidence"`
	Recovered   bool       `json:"recovered,omitempty"`
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
	for fp, open := range state.Open {
		if open.Criterion != "" && !open.Recovered {
			if _, stillUnmet := unmetKeys[open.Criterion]; !stillUnmet {
				res.Recoveries = append(res.Recoveries, RecoveryReport(open, Report{Fingerprint: fp, InstanceID: instance, Criterion: open.Criterion}))
			}
		}
	}

	if len(obs.Evidence) == 0 {
		for _, name := range unmetKeys {
			res.OperatorCriteria = append(res.OperatorCriteria, name)
		}
		sort.Strings(res.OperatorCriteria)
		return res
	}
	for key, name := range unmetKeys {
		if len(state.Criteria[key].Epochs) < PersistentUnmetEpochs {
			continue
		}
		for _, ev := range obs.Evidence {
			if !ev.Attributable {
				continue
			}
			fp := Fingerprint(ev.ErrorClass, ev.Component, version, key)
			report := BuildReport(obs, key, name, ev, fp, instance, version, commit)
			open := state.Open[fp]
			if open.Criterion == "" {
				open.Criterion = key
				state.Open[fp] = open
			}
			if dryRun || open.Number == 0 || open.BodyHash != StableBodyHash(report.Body) {
				res.Reports = append(res.Reports, report)
			}
		}
	}
	sort.Slice(res.Reports, func(i, j int) bool { return res.Reports[i].Fingerprint < res.Reports[j].Fingerprint })
	sort.Slice(res.Recoveries, func(i, j int) bool { return res.Recoveries[i].Fingerprint < res.Recoveries[j].Fingerprint })
	res.State = state
	return res
}

func BuildReport(obs Observation, criterionKey, criterionName string, ev Evidence, fingerprint, instance, version, commit string) Report {
	criterionLabel := "criterion:" + criterionKey
	componentLabel := "component:" + labelValue(ev.Component)
	severityLabel := "severity:" + labelValue(ev.Severity)
	versionLabel := "version:" + labelValue(version)
	instanceLabel := "instance:" + instance
	labels := []string{"fleet-report", componentLabel, severityLabel, versionLabel, instanceLabel, criterionLabel}
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
| count + window | %d in %s |
| detected periodicity | %s |
| self-recovered | no |
`, fingerprint, instance, instance, version, commit, safeToken(obs.Mode), obs.ACMMLevel, criterionName, criterionKey, ev.Component, emptyDash(ev.Agent), emptyDash(ev.Lane), ev.ErrorClass, ev.Count, ev.Window, emptyDash(ev.Periodicity))
	return Report{Fingerprint: fingerprint, InstanceID: instance, Title: safeText(title), Body: safeText(body), Labels: labels, Criterion: criterionKey, Evidence: []Evidence{ev}}
}

func RecoveryReport(open OpenIssue, report Report) Report {
	report.Recovered = true
	report.Body = safeText(fmt.Sprintf("<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n\nRecovery: this spoke no longer observes the ACMM shortfall/evidence for `%s`.\n\nself-recovered: yes\n", report.Fingerprint, report.InstanceID, report.Criterion))
	return report
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
	class := strings.ToLower(e.Class)
	component := strings.ToLower(e.Component)
	return strings.Contains(class, "i/o timeout") && (strings.Contains(component, "read") || strings.Contains(component, "keepalive"))
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
