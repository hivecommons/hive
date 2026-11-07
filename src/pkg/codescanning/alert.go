// Package codescanning turns a repository's open GitHub code scanning alerts
// (CodeQL, OSSF Scorecard, Trivy, any SARIF uploader) into a small number of
// hive issues sized for one PR each, and keeps those issues in step with the
// alerts: new alerts are added, fixed or dismissed ones are marked, and an
// issue whose alerts are all gone is closed.
//
// The package is GitHub-free: Run talks to a Forge, which pkg/github
// implements with the App client (every write gated by the write-back gate).
// Dedupe state is the hidden marker on the hive's own issues, so a restart or
// a lost volume rebuilds it from GitHub.
package codescanning

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Alert is the subset of a GitHub code scanning alert
// (GET /repos/{owner}/{repo}/code-scanning/alerts) the sweep reads. The json
// tags match the API, so a response page decodes straight into []Alert.
type Alert struct {
	Number          int      `json:"number"`
	HTMLURL         string   `json:"html_url,omitempty"`
	State           string   `json:"state,omitempty"`
	DismissedReason string   `json:"dismissed_reason,omitempty"`
	Rule            Rule     `json:"rule"`
	Tool            Tool     `json:"tool"`
	Instance        Instance `json:"most_recent_instance"`
}

// Rule is the alert's rule. SecuritySeverityLevel is set only for rules
// tagged security (critical, high, medium, low); Severity is the SARIF level
// every rule has (error, warning, note).
type Rule struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name,omitempty"`
	Severity              string   `json:"severity,omitempty"`
	SecuritySeverityLevel string   `json:"security_severity_level,omitempty"`
	Description           string   `json:"description,omitempty"`
	HelpURI               string   `json:"help_uri,omitempty"`
	Tags                  []string `json:"tags,omitempty"`
}

// Tool names the analyser that raised the alert ("CodeQL", "Scorecard",
// "Trivy").
type Tool struct {
	Name string `json:"name"`
}

// Instance is the alert's most recent instance on the default branch.
type Instance struct {
	Ref       string   `json:"ref,omitempty"`
	CommitSHA string   `json:"commit_sha,omitempty"`
	Message   Message  `json:"message"`
	Location  Location `json:"location"`
}

// Message is the analyser's text for this instance.
type Message struct {
	Text string `json:"text,omitempty"`
}

// Location is where the instance points.
type Location struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// noFilePath is what OSSF Scorecard reports as the path of a repo-level
// check ("score is 0: project is not fuzzed").
const noFilePath = "no file associated with this alert"

// Path is the file the alert points at, or "" for a repo-level alert.
func (a Alert) Path() string {
	p := strings.TrimSpace(a.Instance.Location.Path)
	if p == "" || strings.EqualFold(p, noFilePath) {
		return ""
	}
	return strings.TrimPrefix(p, "./")
}

// Level is the sweep's one severity scale.
type Level int

// The scale, lowest first. See Alert.Level for how GitHub's two severity
// fields map onto it.
const (
	LevelNone Level = iota
	LevelNote
	LevelLow
	LevelMedium
	LevelHigh
	LevelCritical
)

var levelNames = map[Level]string{LevelNote: "note", LevelLow: "low", LevelMedium: "medium", LevelHigh: "high", LevelCritical: "critical"}

// String is the level's config spelling.
func (l Level) String() string {
	if s, ok := levelNames[l]; ok {
		return s
	}
	return "none"
}

// LevelNames lists the scale highest first, for status payloads.
func LevelNames() []string {
	return []string{"critical", "high", "medium", "low", "note"}
}

// ParseLevel reads a min_severity value: one of critical, high, medium, low,
// note, or the SARIF names error (= medium) and warning (= low).
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return LevelCritical, true
	case "high":
		return LevelHigh, true
	case "medium", "moderate", "error":
		return LevelMedium, true
	case "low", "warning":
		return LevelLow, true
	case "note":
		return LevelNote, true
	}
	return LevelNone, false
}

// Level maps an alert onto the scale. A security rule's
// security_severity_level wins (critical, high, medium, low map to
// themselves). A rule without one is a quality finding and uses its SARIF
// severity one step lower than its name suggests: error → medium,
// warning → low, note → note. So min_severity: high selects exactly the
// alerts GitHub's Security tab shows as High or Critical.
func (a Alert) Level() Level {
	switch strings.ToLower(strings.TrimSpace(a.Rule.SecuritySeverityLevel)) {
	case "critical":
		return LevelCritical
	case "high":
		return LevelHigh
	case "medium":
		return LevelMedium
	case "low":
		return LevelLow
	}
	switch strings.ToLower(strings.TrimSpace(a.Rule.Severity)) {
	case "error":
		return LevelMedium
	case "warning":
		return LevelLow
	case "note":
		return LevelNote
	}
	return LevelNone
}

// Dependency is the vulnerable package a dependency scanner (Trivy, Grype)
// reports in its message ("Package: golang.org/x/net\nInstalled Version: …").
type Dependency struct {
	Package   string
	Installed string
	Fixed     string
}

var (
	depPackageRE   = regexp.MustCompile(`(?m)^Package:\s*(\S+)\s*$`)
	depInstalledRE = regexp.MustCompile(`(?m)^Installed Version:\s*(.+?)\s*$`)
	depFixedRE     = regexp.MustCompile(`(?m)^Fixed Version:\s*(.+?)\s*$`)
)

// Dependency returns the vulnerable package, if the alert is a dependency
// finding.
func (a Alert) Dependency() (Dependency, bool) {
	text := a.Instance.Message.Text
	m := depPackageRE.FindStringSubmatch(text)
	if m == nil {
		return Dependency{}, false
	}
	d := Dependency{Package: m[1]}
	if m := depInstalledRE.FindStringSubmatch(text); m != nil {
		d.Installed = m[1]
	}
	if m := depFixedRE.FindStringSubmatch(text); m != nil {
		d.Fixed = m[1]
	}
	return d, true
}

// DefaultMinLevel is min_severity when unset.
const DefaultMinLevel = LevelHigh

// DefaultMaxNewIssues is the per-repo, per-pass cap on new issues when unset.
const DefaultMaxNewIssues = 5

// DefaultExcludeRules are OSSF Scorecard checks that score repository
// settings or project policy, not code: branch protection, code review,
// maintenance, a security policy, fuzzing, SAST, the CII badge, signed
// releases, and so on. No PR an agent can write fixes them, so filing them
// would only park issues for the owner. VulnerabilitiesID repeats the
// dependency alerts the repo's dependency scanners already raise.
// BinaryArtifactsID asks to delete checked-in binaries, which is a project
// decision. They are still counted on the dashboard. An operator re-includes
// one with a "!" entry in exclude_rules ("!SASTID").
var DefaultExcludeRules = []string{
	"BinaryArtifactsID",
	"BranchProtectionID",
	"CIIBestPracticesID",
	"CITestsID",
	"CodeReviewID",
	"ContributorsID",
	"DependencyUpdateToolID",
	"FuzzingID",
	"LicenseID",
	"MaintainedID",
	"PackagingID",
	"SASTID",
	"SecurityPolicyID",
	"SignedReleasesID",
	"VulnerabilitiesID",
	"WebhooksID",
}

// Policy is one repo's effective filter.
type Policy struct {
	// MinLevel is the lowest level filed. LevelNone means DefaultMinLevel.
	MinLevel Level
	// IncludeTools, when set, files only alerts from these tools (by
	// name, case-insensitive).
	IncludeTools []string
	// ExcludeRules are rule-ID patterns (path.Match syntax, case-insensitive)
	// added to DefaultExcludeRules; a "!pattern" entry takes a default back
	// out of the list.
	ExcludeRules []string
	// MaxNewIssues caps new issues per pass. 0 means DefaultMaxNewIssues.
	MaxNewIssues int
}

// ValidateRulePatterns rejects a malformed exclude_rules pattern.
func ValidateRulePatterns(patterns []string) error {
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.TrimSpace(p), "!")
		if p == "" {
			return errors.New("empty rule pattern")
		}
		if _, err := path.Match(strings.ToLower(p), ""); err != nil {
			return errors.New("bad rule pattern " + p + ": " + err.Error())
		}
	}
	return nil
}

func (p Policy) minLevel() Level {
	if p.MinLevel == LevelNone {
		return DefaultMinLevel
	}
	return p.MinLevel
}

func (p Policy) maxNewIssues() int {
	if p.MaxNewIssues <= 0 {
		return DefaultMaxNewIssues
	}
	return p.MaxNewIssues
}

// excludeList is DefaultExcludeRules plus ExcludeRules, minus "!" entries.
func (p Policy) excludeList() []string {
	unexclude := map[string]bool{}
	var extra []string
	for _, r := range p.ExcludeRules {
		r = strings.TrimSpace(r)
		if strings.HasPrefix(r, "!") {
			unexclude[strings.ToLower(strings.TrimPrefix(r, "!"))] = true
			continue
		}
		if r != "" {
			extra = append(extra, r)
		}
	}
	var out []string
	for _, r := range DefaultExcludeRules {
		if !unexclude[strings.ToLower(r)] {
			out = append(out, r)
		}
	}
	return append(out, extra...)
}

// Verdict is why an alert is or is not filed.
type Verdict int

// Verdicts, in the order Policy.Judge checks them.
const (
	Qualifies Verdict = iota
	BelowThreshold
	Excluded
)

// Judge decides whether the policy files an alert.
func (p Policy) Judge(a Alert) Verdict {
	if len(p.IncludeTools) > 0 {
		ok := false
		for _, t := range p.IncludeTools {
			if strings.EqualFold(strings.TrimSpace(t), a.Tool.Name) {
				ok = true
				break
			}
		}
		if !ok {
			return Excluded
		}
	}
	rule := strings.ToLower(a.Rule.ID)
	for _, pat := range p.excludeList() {
		if ok, _ := path.Match(strings.ToLower(pat), rule); ok {
			return Excluded
		}
	}
	if a.Level() < p.minLevel() {
		return BelowThreshold
	}
	return Qualifies
}

// sortAlerts orders alerts by path, line, number.
func sortAlerts(alerts []Alert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.Path() != b.Path() {
			return a.Path() < b.Path()
		}
		if a.Instance.Location.StartLine != b.Instance.Location.StartLine {
			return a.Instance.Location.StartLine < b.Instance.Location.StartLine
		}
		return a.Number < b.Number
	})
}
