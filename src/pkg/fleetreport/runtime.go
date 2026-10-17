package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// TriggerRuntimeError is the fleet-report trigger for the "unusual runtime
// error" allow-list below (hivecommons/hive#11868). Unlike the two older
// triggers it needs no ACMM shortfall and no component allowlist: the class
// itself is the attribution.
const TriggerRuntimeError = "runtime-error"

// Runtime error classes. Each is an explicit allow-list entry: an error site
// opts in by calling Observe with one of these, so an arbitrary logged error
// never becomes an upstream issue by accident.
const (
	ClassRestartStorm             = "restart_storm"
	ClassKickFailedRepeatedly     = "kick_failed_repeatedly"
	ClassPanicRecovered           = "panic_recovered"
	ClassModelDiscoveryDegraded   = "model_discovery_degraded"
	ClassInferenceAuthFailed      = "inference_auth_failed"
	ClassAdvisoryPostFailed       = "advisory_post_failed"
	ClassUpgradeStalled           = "upgrade_stalled"
	ClassDataDiskPressure         = "data_disk_pressure"
	ClassGitHubAppTokenMintFailed = "github_app_token_mint_failed"
	ClassUnexpectedHub5xx         = "unexpected_http_5xx_from_hub"
	ClassUnclassifiedRepeated     = "unclassified_error_repeated"
)

const (
	// unclassifiedRepeatCount / unclassifiedRepeatWindow: the same unclassified
	// error string must recur this many times inside the window before it
	// qualifies as unusual.
	unclassifiedRepeatCount  = 5
	unclassifiedRepeatWindow = 10 * time.Minute
	// kickFailureStreak is the consecutive-failure count that makes repeated
	// kick failures reportable.
	kickFailureStreak = 3
	// degradedMinDuration is how long model discovery must stay degraded.
	degradedMinDuration = time.Hour
	// hub5xxStreak is how many consecutive 5xx answers from the hub (the
	// condition is cleared on the next accepted request) make it reportable,
	// so a single blip during a hub rollout is not filed upstream.
	hub5xxStreak = 3
	// runtimeEventTTL bounds how long a condition that is never cleared keeps
	// producing evidence; anything older is dropped on the next snapshot.
	runtimeEventTTL = 24 * time.Hour
	// maxDiagnosticsBytes caps the scrubbed diagnostics excerpt attached to a
	// report so an issue body stays readable.
	maxDiagnosticsBytes = 8 * 1024
	// maxTrackedConditions bounds the tracker so a misbehaving caller cannot
	// grow memory without limit.
	maxTrackedConditions = 256
)

// ClassSpec describes one runtime error class.
type ClassSpec struct {
	// Component is the hive-owned component the class is attributed to.
	Component string
	Severity  string
	// MinCount is how many observations (within TTL) are needed before the
	// condition is reportable. 1 = immediately.
	MinCount int
	// MinDuration is how long the condition must persist before it is
	// reportable; zero = no persistence requirement.
	MinDuration time.Duration
	// Window, when set, requires MinCount observations inside this window
	// rather than cumulatively.
	Window time.Duration
	// Keywords are the human phrasings used to find an existing issue about
	// the same symptom (join-first search). Lower-case.
	Keywords []string
}

// Classes is the runtime error allow-list.
var Classes = map[string]ClassSpec{
	ClassRestartStorm:             {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"restart storm", "restart-storm"}},
	ClassKickFailedRepeatedly:     {Component: "agent-runtime", Severity: "medium", MinCount: kickFailureStreak, Keywords: []string{"kick failed", "undeliverable kick", "kicks undeliverable"}},
	ClassPanicRecovered:           {Component: "dashboard", Severity: "high", MinCount: 1, Keywords: []string{"panic recovered", "recovered panic"}},
	ClassModelDiscoveryDegraded:   {Component: "inference-gateway", Severity: "medium", MinCount: 1, MinDuration: degradedMinDuration, Keywords: []string{"model discovery degraded", "model discovery failed"}},
	ClassInferenceAuthFailed:      {Component: "backend-auth", Severity: "high", MinCount: 1, Keywords: []string{"litellm 401", "inference 401", "inference 403", "authentication failure"}},
	ClassAdvisoryPostFailed:       {Component: "github-client", Severity: "medium", MinCount: 1, Keywords: []string{"advisory post failed", "advisory digest"}},
	ClassUpgradeStalled:           {Component: "agent-lifecycle", Severity: "high", MinCount: 1, Keywords: []string{"upgrade stalled", "upgrade stall"}},
	ClassDataDiskPressure:         {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"disk full", "disk pressure", "no space left"}},
	ClassGitHubAppTokenMintFailed: {Component: "github-client", Severity: "high", MinCount: 1, Keywords: []string{"token mint failed", "installation token"}},
	ClassUnexpectedHub5xx:         {Component: "dashboard", Severity: "medium", MinCount: hub5xxStreak, Keywords: []string{"hub 5", "hub returned 5"}},
	ClassUnclassifiedRepeated:     {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow},
}

// IsKnownClass reports whether class is on the allow-list.
func IsKnownClass(class string) bool {
	_, ok := Classes[strings.TrimSpace(class)]
	return ok
}

// RuntimeCondition is one tracked (class, key) condition.
type RuntimeCondition struct {
	Class       string
	Key         string
	Message     string
	Diagnostics string
	FirstSeen   time.Time
	LastSeen    time.Time
	Count       int
	// Repo and Ref are the most recent triggering item (see ObserveItem).
	Repo   string
	Ref    string
	events []time.Time
}

// RuntimeTracker accumulates runtime error observations and turns the ones
// that meet their class thresholds into Evidence. It is safe for concurrent
// use; the clock is injectable so tests never sleep.
type RuntimeTracker struct {
	mu    sync.Mutex
	now   func() time.Time
	conds map[string]*RuntimeCondition
	// cleared records conditions that were active and then cleared, so the
	// publisher can post a single "cleared" comment.
	cleared map[string]RuntimeCondition
}

// NewRuntimeTracker returns a tracker using now (nil = time.Now).
func NewRuntimeTracker(now func() time.Time) *RuntimeTracker {
	if now == nil {
		now = time.Now
	}
	return &RuntimeTracker{now: now, conds: map[string]*RuntimeCondition{}, cleared: map[string]RuntimeCondition{}}
}

var (
	defaultTrackerMu sync.RWMutex
	defaultTracker   = NewRuntimeTracker(nil)
)

// DefaultTracker is the process-wide tracker the hook sites report into.
func DefaultTracker() *RuntimeTracker {
	defaultTrackerMu.RLock()
	defer defaultTrackerMu.RUnlock()
	return defaultTracker
}

// SetDefaultTrackerForTest swaps the process-wide tracker for the lifetime
// of t.
func SetDefaultTrackerForTest(t interface {
	Helper()
	Cleanup(func())
}, tr *RuntimeTracker) {
	t.Helper()
	defaultTrackerMu.Lock()
	old := defaultTracker
	defaultTracker = tr
	defaultTrackerMu.Unlock()
	t.Cleanup(func() {
		defaultTrackerMu.Lock()
		defaultTracker = old
		defaultTrackerMu.Unlock()
	})
}

// Observe records one occurrence of class on the default tracker. key scopes
// the condition (agent name, backend, repo…); message is the error text;
// diagnostics is an optional excerpt (scrubbed and bounded before storage).
func Observe(class, key, message, diagnostics string) {
	DefaultTracker().Observe(class, key, message, diagnostics)
}

// ObserveItem is Observe for an error scoped to one repository item: repo is
// the owner/repo and ref the issue/PR reference ("#123", may be empty). The
// item is carried in the report's evidence table and affected-repos union
// (#12338); it never enters the fingerprint.
func ObserveItem(class, key, message, diagnostics, repo, ref string) {
	DefaultTracker().ObserveItem(class, key, message, diagnostics, repo, ref)
}

// Clear marks a condition as no longer present on the default tracker.
func Clear(class, key string) { DefaultTracker().Clear(class, key) }

func condKey(class, key string) string {
	return strings.TrimSpace(class) + "\x00" + strings.TrimSpace(key)
}

// Observe records one occurrence. Unknown classes are ignored: the allow-list
// is the contract.
func (t *RuntimeTracker) Observe(class, key, message, diagnostics string) {
	t.ObserveItem(class, key, message, diagnostics, "", "")
}

// ObserveItem records one occurrence scoped to a repository item; see the
// package-level ObserveItem. Empty repo/ref leave the previous item in place.
func (t *RuntimeTracker) ObserveItem(class, key, message, diagnostics, repo, ref string) {
	if t == nil || !IsKnownClass(class) {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := condKey(class, key)
	c, ok := t.conds[ck]
	if !ok {
		if len(t.conds) >= maxTrackedConditions {
			t.evictOldestLocked()
		}
		c = &RuntimeCondition{Class: strings.TrimSpace(class), Key: safeToken(key), FirstSeen: now}
		t.conds[ck] = c
	}
	delete(t.cleared, ck)
	c.LastSeen = now
	c.Count++
	c.events = append(c.events, now)
	if len(c.events) > unclassifiedRepeatCount*2 {
		c.events = c.events[len(c.events)-unclassifiedRepeatCount*2:]
	}
	if msg := safeToken(logscrub.RedactPaneTokens(message)); msg != "" {
		c.Message = msg
	}
	if d := boundDiagnostics(diagnostics); d != "" {
		c.Diagnostics = d
	}
	if r := NormalizeRepo(repo); r != "" {
		c.Repo = r
		c.Ref = safeToken(ref)
	}
}

// Clear removes a condition; if it was active it is remembered as cleared.
func (t *RuntimeTracker) Clear(class, key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := condKey(class, key)
	c, ok := t.conds[ck]
	if !ok {
		return
	}
	if t.qualifiesLocked(c, t.now()) {
		snap := *c
		snap.events = nil
		snap.LastSeen = t.now()
		t.cleared[ck] = snap
	}
	delete(t.conds, ck)
}

// TakeCleared returns and forgets the conditions that cleared since the last
// call, so a "cleared" comment is posted exactly once.
func (t *RuntimeTracker) TakeCleared() []RuntimeCondition {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RuntimeCondition, 0, len(t.cleared))
	for _, c := range t.cleared {
		out = append(out, c)
	}
	t.cleared = map[string]RuntimeCondition{}
	sort.Slice(out, func(i, j int) bool { return out[i].Class+out[i].Key < out[j].Class+out[j].Key })
	return out
}

func (t *RuntimeTracker) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, c := range t.conds {
		if oldestKey == "" || c.LastSeen.Before(oldest) {
			oldestKey, oldest = k, c.LastSeen
		}
	}
	delete(t.conds, oldestKey)
}

func (t *RuntimeTracker) qualifiesLocked(c *RuntimeCondition, now time.Time) bool {
	spec, ok := Classes[c.Class]
	if !ok {
		return false
	}
	if now.Sub(c.LastSeen) > runtimeEventTTL {
		return false
	}
	if spec.MinDuration > 0 && now.Sub(c.FirstSeen) < spec.MinDuration {
		return false
	}
	min := spec.MinCount
	if min < 1 {
		min = 1
	}
	if spec.Window > 0 {
		inWindow := 0
		for _, at := range c.events {
			if now.Sub(at) <= spec.Window {
				inWindow++
			}
		}
		return inWindow >= min
	}
	return c.Count >= min
}

// Evidence returns one Evidence per condition that currently meets its class
// threshold. Stale conditions (no observation inside the TTL) are dropped.
func (t *RuntimeTracker) Evidence() []Evidence {
	if t == nil {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Evidence
	for ck, c := range t.conds {
		if now.Sub(c.LastSeen) > runtimeEventTTL {
			delete(t.conds, ck)
			continue
		}
		if !t.qualifiesLocked(c, now) {
			continue
		}
		spec := Classes[c.Class]
		window := spec.Window
		if window == 0 {
			window = now.Sub(c.FirstSeen).Round(time.Second)
		}
		out = append(out, Evidence{
			Component:    spec.Component,
			Agent:        c.Key,
			ErrorClass:   c.Class,
			Count:        c.Count,
			Window:       window,
			Severity:     spec.Severity,
			Attributable: true,
			Class:        c.Class,
			Message:      c.Message,
			Diagnostics:  c.Diagnostics,
			FirstSeen:    c.FirstSeen,
			Repo:         c.Repo,
			Ref:          c.Ref,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// NormalizeMessage collapses the volatile parts of an error message (numbers,
// hex ids, quoted paths/URLs) so the same defect produces the same fingerprint
// across agents and runs.
func NormalizeMessage(msg string) string {
	msg = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(msg)))
	var b strings.Builder
	runes := []rune(msg)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r >= '0' && r <= '9' || (r >= 'a' && r <= 'f' && runOfHex(runes, i) >= 8):
			j := i
			for j < len(runes) && ((runes[j] >= '0' && runes[j] <= '9') || (runes[j] >= 'a' && runes[j] <= 'f') || runes[j] == '-' || runes[j] == '.') {
				j++
			}
			b.WriteString("N")
			i = j
		case r == '/' && i+1 < len(runes) && runes[i+1] != ' ':
			// Filesystem paths and URL paths: one placeholder for the whole
			// run so per-agent/per-file directories do not split fingerprints.
			j := i
			for j < len(runes) && runes[j] != ' ' && runes[j] != ',' && runes[j] != ';' && runes[j] != ')' && runes[j] != ']' && runes[j] != ':' && runes[j] != '\n' && runes[j] != '\t' {
				j++
			}
			b.WriteString("P")
			i = j
		case r == '"' || r == '\'' || r == '`':
			j := i + 1
			for j < len(runes) && runes[j] != r {
				j++
			}
			b.WriteString("Q")
			if j < len(runes) {
				j++
			}
			i = j
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
			i++
		default:
			b.WriteRune(r)
			i++
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func runOfHex(runes []rune, i int) int {
	n := 0
	for i+n < len(runes) {
		r := runes[i+n]
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			n++
			continue
		}
		break
	}
	return n
}

// RuntimeFingerprint is class + normalised message + hive version.
func RuntimeFingerprint(class, message, version string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{TriggerRuntimeError, strings.ToLower(strings.TrimSpace(class)), NormalizeMessage(message), strings.ToLower(strings.TrimSpace(version))}, "|")))
	return hex.EncodeToString(h[:])[:24]
}

// ShortMessage trims a message for an issue title.
func ShortMessage(msg string) string {
	msg = strings.Join(strings.Fields(safeToken(msg)), " ")
	const max = 80
	if len(msg) > max {
		return msg[:max-1] + "…"
	}
	if msg == "" {
		return "unusual error"
	}
	return msg
}

// boundDiagnostics scrubs credential shapes (including the sk- API keys
// logscrub.ScrubString leaves alone) and keeps the newest maxDiagnosticsBytes.
func boundDiagnostics(d string) string {
	d = strings.TrimSpace(logscrub.RedactPaneTokens(logscrub.ScrubString(d)))
	if len(d) > maxDiagnosticsBytes {
		d = "…\n" + d[len(d)-maxDiagnosticsBytes:]
	}
	return d
}

// Markers used in issue bodies and comments. The legacy
// hive-fleet-fingerprint marker is kept alongside so older hives still find
// the issue.
const (
	SelfReportMarkerPrefix     = "<!-- hive:selfreport "
	SelfReportSeenMarkerPrefix = "<!-- hive:selfreport-seen "
	AffectedBlockStart         = "<!-- hive:selfreport-affected -->"
	AffectedBlockEnd           = "<!-- /hive:selfreport-affected -->"
	LabelSelfReport            = "hive/selfreport"
	LabelKindBug               = "kind/bug"
	LabelNeedsTriage           = "needs-triage"
	LabelFleetReport           = "fleet-report"
	LabelObservation           = "hive-observation"
)

// SelfReportMarker is the body marker a runtime-error report carries.
func SelfReportMarker(fp, hive string) string {
	return fmt.Sprintf("%sfp=%s hive=%s -->", SelfReportMarkerPrefix, safeToken(fp), labelValue(hive))
}

// SeenMarker is the comment marker a joining hive leaves.
func SeenMarker(fp, hive string) string {
	return fmt.Sprintf("%shive=%s fp=%s -->", SelfReportSeenMarkerPrefix, labelValue(hive), safeToken(fp))
}

// BuildRuntimeReport renders a runtime-error report. hive is the display
// identity (hub slug for hosted spokes, anonymous instance otherwise); the
// owner logins come from obs.Owners unless obs.OwnersHidden.
func BuildRuntimeReport(obs Observation, ev Evidence, fp, instance, hive, version, commit string, now time.Time) Report {
	if strings.TrimSpace(hive) == "" {
		hive = instance
	}
	labels := []string{LabelFleetReport, LabelSelfReport, LabelKindBug, LabelNeedsTriage, "severity:" + labelValue(ev.Severity), "trigger:" + TriggerRuntimeError, "class:" + labelValue(ev.Class)}
	sort.Strings(labels)
	title := fmt.Sprintf("[selfreport] %s: %s", ev.Class, ShortMessage(ev.Message))
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n%s\n\n", fp, instance, SelfReportMarker(fp, hive))
	fmt.Fprintf(&b, "A hive hit an unusual runtime error (class `%s`) and is reporting it so the same defect across the fleet lands in one issue.\n\n", ev.Class)
	b.WriteString("## Evidence\n\n| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| hive | %s |\n", hive)
	fmt.Fprintf(&b, "| anonymous instance | %s |\n", instance)
	fmt.Fprintf(&b, "| owner(s) | %s |\n", OwnerMentions(obs.Owners, obs.OwnersHidden))
	fmt.Fprintf(&b, "| hive version | %s |\n", version)
	fmt.Fprintf(&b, "| hive commit | %s |\n", commit)
	fmt.Fprintf(&b, "| cluster | %s |\n", emptyDash(safeToken(obs.Cluster)))
	fmt.Fprintf(&b, "| mode | %s |\n", emptyDash(safeToken(obs.Mode)))
	fmt.Fprintf(&b, "| component | %s |\n", ev.Component)
	fmt.Fprintf(&b, "| scope | %s |\n", emptyDash(ev.Agent))
	fmt.Fprintf(&b, "| message | %s |\n", emptyDash(strings.ReplaceAll(ev.Message, "|", "\\|")))
	fmt.Fprintf(&b, "| count + window | %s |\n", countWindow(ev.Count, ev.Window))
	fmt.Fprintf(&b, "| first seen | %s |\n", ev.FirstSeen.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| reported at | %s |\n", now.UTC().Format(time.RFC3339))
	watched := NormalizeRepos(obs.Repos)
	fmt.Fprintf(&b, "| repos | %s |\n", repoListCell(watched))
	fmt.Fprintf(&b, "| owners | %s |\n", repoListCell(OwnersOf(watched)))
	itemRepo := NormalizeRepo(ev.Repo)
	fmt.Fprintf(&b, "| repo | %s |\n", emptyDash(itemRepo))
	fmt.Fprintf(&b, "| ref | %s |\n", emptyDash(itemRef(itemRepo, ev.Ref)))
	b.WriteString("| self-recovered | no |\n\n")
	repos := watched
	if itemRepo != "" {
		repos = NormalizeRepos(append(append([]string(nil), watched...), itemRepo))
	}
	owners := NormalizeLogins(obs.Owners)
	if obs.OwnersHidden {
		owners = nil
	}
	b.WriteString(AffectedBlock{Hives: []AffectedHive{{Hive: hive, Instance: instance, Owners: owners, OwnersHidden: obs.OwnersHidden, Seen: 1, Last: now}}, Repos: repos, Seen: 1}.Render())
	if ev.Diagnostics != "" {
		fmt.Fprintf(&b, "\n\n<details><summary>Diagnostics (scrubbed)</summary>\n\n```\n%s\n```\n</details>\n", ev.Diagnostics)
	}
	return Report{Fingerprint: fp, InstanceID: instance, Title: safeText(title), Body: safeText(b.String()), Labels: labels, Trigger: TriggerRuntimeError, Class: ev.Class, Hive: hive, Evidence: []Evidence{ev}, Repos: repos, Owners: owners, OwnersHidden: obs.OwnersHidden, HiveState: obs.HiveState}
}

// maxReposInTable bounds the repos / owners cells of the evidence table; the
// affected block below the table always carries the full union.
const maxReposInTable = 20

// repoListCell renders "N: a/b, c/d" for the evidence table, "—" when empty.
func repoListCell(list []string) string {
	if len(list) == 0 {
		return "—"
	}
	shown := list
	more := 0
	if len(shown) > maxReposInTable {
		more = len(shown) - maxReposInTable
		shown = shown[:maxReposInTable]
	}
	cell := fmt.Sprintf("%d: %s", len(list), strings.Join(shown, ", "))
	if more > 0 {
		cell += fmt.Sprintf(", +%d more", more)
	}
	return cell
}

// itemRef renders the triggering item's reference as owner/repo#N when both
// halves are known, the bare ref otherwise.
func itemRef(repo, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if repo != "" && strings.HasPrefix(ref, "#") {
		return repo + ref
	}
	return ref
}

// NormalizeRepo lower-cases and validates an owner/repo name; anything that
// is not exactly two non-empty path segments of GitHub-safe characters
// renders as "" so a stray URL or token can never reach the issue body.
func NormalizeRepo(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(repo)))
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimSuffix(repo, ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
			if !ok {
				return ""
			}
		}
	}
	return repo
}

// NormalizeRepos normalises, dedupes and sorts a repo list.
func NormalizeRepos(repos []string) []string {
	uniq := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = NormalizeRepo(r)
		if r == "" || uniq[r] {
			continue
		}
		uniq[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// OwnersOf returns the distinct, sorted owners (orgs or users) of repos.
func OwnersOf(repos []string) []string {
	uniq := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = NormalizeRepo(r)
		if r == "" {
			continue
		}
		owner := r[:strings.Index(r, "/")]
		if uniq[owner] {
			continue
		}
		uniq[owner] = true
		out = append(out, owner)
	}
	sort.Strings(out)
	return out
}

// RenderAffectedBlock renders the affected block for hives known by name
// only; see AffectedBlock.Render.
func RenderAffectedBlock(hives []string, seen int) string {
	return RenderAffectedBlockWithRepos(hives, nil, seen)
}

// RenderAffectedBlockWithRepos renders the affected block for hives known by
// name only plus the union of the repos they watch.
func RenderAffectedBlockWithRepos(hives, repos []string, seen int) string {
	return AffectedBlock{Hives: affectedByName(hives), Repos: repos, Seen: seen}.Render()
}

func affectedByName(hives []string) []AffectedHive {
	out := make([]AffectedHive, 0, len(hives))
	for _, h := range hives {
		out = append(out, AffectedHive{Hive: h})
	}
	return out
}

// ParseAffectedBlock extracts the hive names and seen_count from a body; ok
// is false when the body has no block.
func ParseAffectedBlock(body string) (hives []string, seen int, ok bool) {
	hives, _, seen, ok = ParseAffectedBlockWithRepos(body)
	return hives, seen, ok
}

// ParseAffectedBlockWithRepos extracts the hive names, repos and seen_count
// from a body; ok is false when the body has no block. See ParseAffected for
// the per-hive owner / instance / seen details.
func ParseAffectedBlockWithRepos(body string) (hives, repos []string, seen int, ok bool) {
	blk, ok := ParseAffected(body)
	if !ok {
		return nil, nil, 0, false
	}
	for _, h := range blk.Hives {
		hives = append(hives, h.Hive)
	}
	return hives, blk.Repos, blk.Seen, true
}

// affectedLineItems splits the comma list after the ":**" of an affected line.
func affectedLineItems(line string) []string {
	i := strings.Index(line, ":**")
	if i < 0 {
		return nil
	}
	var items []string
	for _, h := range strings.Split(line[i+3:], ",") {
		if h = strings.TrimSpace(h); h != "" {
			items = append(items, h)
		}
	}
	return items
}

// UpdateAffectedBlock returns body with hive added to the affected block (and
// seen_count bumped), appending a block when the body has none. changed is
// false when the body already lists the hive and no bump is requested.
func UpdateAffectedBlock(body, hive string, bump bool) (string, bool) {
	return UpdateAffectedBlockWithRepos(body, hive, nil, bump)
}

// UpdateAffectedBlockWithRepos is UpdateAffectedBlock that also merges repos
// into the block's union. See UpdateAffected for the owner-aware form.
func UpdateAffectedBlockWithRepos(body, hive string, repos []string, bump bool) (string, bool) {
	return UpdateAffected(body, AffectedHive{Hive: hive}, repos, bump)
}

// SeenComment renders the "also seen" comment a joining hive posts.
func SeenComment(fp, hive, version, cluster string, ev Evidence, now time.Time) string {
	var b strings.Builder
	b.WriteString(SeenMarker(fp, hive))
	fmt.Fprintf(&b, "\nAlso seen on **%s** (%s, %s) at %s", labelValue(hive), displayVersion(version), emptyDash(safeToken(cluster)), now.UTC().Format(time.RFC3339))
	if ev.Class != "" {
		fmt.Fprintf(&b, " — class `%s`, %s", ev.Class, countWindow(ev.Count, ev.Window))
	}
	if ev.Message != "" {
		fmt.Fprintf(&b, "\n\n> %s", strings.ReplaceAll(ev.Message, "\n", " "))
	}
	if ev.Diagnostics != "" {
		fmt.Fprintf(&b, "\n\n<details><summary>Diagnostics (scrubbed)</summary>\n\n```\n%s\n```\n</details>", ev.Diagnostics)
	}
	return safeText(b.String())
}

// ClearedComment renders the one-time "condition cleared" comment.
func ClearedComment(fp, hive, version string, now time.Time) string {
	return safeText(fmt.Sprintf("%s\nCleared on **%s** (%s) at %s — the condition is no longer observed there. Leaving the issue open for a human to confirm the fix.", SeenMarker(fp, hive), labelValue(hive), displayVersion(version), now.UTC().Format(time.RFC3339)))
}

// FilingLimits are the rate limits applied to NEW issue creation.
type FilingLimits struct {
	MaxPerDay      int
	MinGap         time.Duration
	ClosedCooldown time.Duration
	SeenGap        time.Duration
}

// DefaultFilingLimits: 5 new issues per hive per day, 1h between filings of
// different classes, closed fingerprints are not re-filed for 7 days, and a
// hive adds itself to an existing issue at most once per 24h.
var DefaultFilingLimits = FilingLimits{MaxPerDay: 5, MinGap: time.Hour, ClosedCooldown: 7 * 24 * time.Hour, SeenGap: 24 * time.Hour}

// WithMaxPerDay returns limits with MaxPerDay overridden when n > 0.
func (l FilingLimits) WithMaxPerDay(n int) FilingLimits {
	if n > 0 {
		l.MaxPerDay = n
	}
	return l
}

// AllowNewFiling reports whether a NEW issue may be created now, with the
// reason when it may not. It does not mutate state.
func AllowNewFiling(st State, now time.Time, limits FilingLimits) (bool, string) {
	recent := 0
	var last time.Time
	for _, at := range st.Filings {
		if now.Sub(at) < 24*time.Hour {
			recent++
		}
		if at.After(last) {
			last = at
		}
	}
	if limits.MaxPerDay > 0 && recent >= limits.MaxPerDay {
		return false, fmt.Sprintf("daily cap reached (%d/%d)", recent, limits.MaxPerDay)
	}
	if limits.MinGap > 0 && !last.IsZero() && now.Sub(last) < limits.MinGap {
		return false, fmt.Sprintf("last filing %s ago < min gap %s", now.Sub(last).Round(time.Second), limits.MinGap)
	}
	return true, ""
}

// RecordFiling appends a new-issue creation to state and prunes entries
// older than a day.
func RecordFiling(st *State, now time.Time) {
	var keep []time.Time
	for _, at := range st.Filings {
		if now.Sub(at) < 24*time.Hour {
			keep = append(keep, at)
		}
	}
	st.Filings = append(keep, now.UTC())
}

// AllowSeenComment reports whether this hive may add itself to issue open
// again (once per SeenGap).
func AllowSeenComment(open OpenIssue, now time.Time, limits FilingLimits) bool {
	return open.LastSeenCommentAt.IsZero() || now.Sub(open.LastSeenCommentAt) >= limits.SeenGap
}

// ClosedRecently reports whether a fingerprint's issue closed inside the
// cooldown: treat as known/fixed-pending-rollout and do not re-file.
func ClosedRecently(closedAt time.Time, now time.Time, limits FilingLimits) bool {
	return !closedAt.IsZero() && now.Sub(closedAt) < limits.ClosedCooldown
}

// displayVersion renders a version with exactly one leading "v".
func displayVersion(v string) string {
	v = safeToken(v)
	if v == "" {
		return "vunknown"
	}
	return "v" + strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
}
