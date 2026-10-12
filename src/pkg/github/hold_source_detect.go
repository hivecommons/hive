package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Hold-source detection (hivecommons/hive#11564, spec #11507). For every held
// issue and pull request the status snapshot carries the set of sources
// currently requiring its hold (HoldSource* constants). The detection is
// read-only towards GitHub and reads three things:
//
//   - the item's labels, already in the list payload: the dashboard pause
//     label `hive-pause/<hive-id>` and the reporter-trust extra hold label;
//   - Hive's local record (HoldSourcesPath), written by the code paths that
//     apply a hold leaving no marker on GitHub (the holdguard re-hold, the
//     donated-standby hold and the SHA hold) — a local write and nothing else;
//   - for held pull requests only, the PR's comments (Hive App notices) and
//     hold-label events (who applied the hold). That check is cached in the
//     local record per PR against the PR's updated_at, so a refresh in which
//     no held PR changed makes no GitHub API calls for hold sources.
//
// A held issue is never checked against GitHub: any hold Hive's own record
// does not attribute to a Hive source counts as `person`.

// HoldSourcesFile is Hive's local record of hold sources and their check
// cache. It lives on the durable data dir beside review-links.json.
const HoldSourcesFile = "hold-sources.json"

// HoldSourcesPath is a var so tests can point it at a temp dir.
var HoldSourcesPath = filepath.Join(ReviewLinksDir, HoldSourcesFile)

var holdSourcesMu sync.Mutex

// Hold item kinds, matching HoldItem.Type.
const (
	HoldKindPR    = "pr"
	HoldKindIssue = "issue"
)

const (
	holdActorApp    = "app"
	holdActorPerson = "person"
)

// holdSourceCheck is the cached result of reading one held PR's comments and
// hold-label events. Only facts GitHub alone can tell are cached; the label-
// and record-derived sources are composed on every refresh.
type holdSourceCheck struct {
	UpdatedAt time.Time `json:"updated_at"`
	CheckedAt time.Time `json:"checked_at"`
	// Notices are the sources whose Hive notice is on the PR.
	Notices []string `json:"notices,omitempty"`
	// LatestHoldBy is who made the newest hold-label event: "app", "person",
	// or empty when Hive cannot attribute it.
	LatestHoldBy string `json:"latest_hold_by,omitempty"`
}

type holdSourceLedger struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Applied maps item key → source → when Hive applied that hold.
	Applied map[string]map[string]time.Time `json:"applied,omitempty"`
	// Checks is the per-PR check cache keyed like Applied.
	Checks map[string]holdSourceCheck `json:"checks,omitempty"`
}

func holdSourceKey(kind, repo string, number int) string {
	return kind + ":" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// readHoldSourceLedgerLocked reads the record. A missing or corrupt record is
// an empty one: hold sources are display-only and must never block
// enumeration.
func readHoldSourceLedgerLocked(path string) holdSourceLedger {
	var l holdSourceLedger
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &l) != nil {
			l = holdSourceLedger{}
		}
	}
	if l.Applied == nil {
		l.Applied = map[string]map[string]time.Time{}
	}
	if l.Checks == nil {
		l.Checks = map[string]holdSourceCheck{}
	}
	return l
}

// writeHoldSourceLedgerLocked writes the record atomically. It does not
// create the data dir: a process without one (a test, a dev run) simply keeps
// no record and re-checks held PRs.
func writeHoldSourceLedgerLocked(path string, l holdSourceLedger) error {
	l.GeneratedAt = time.Now().UTC()
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

// RecordHoldSource notes in Hive's local record that Hive applied a hold of
// source to repo#number ("owner/repo"; kind HoldKindPR or HoldKindIssue).
// Called by hold-applying code paths that leave no marker on GitHub, after the
// hold label landed. It is a local write only and never touches GitHub.
func RecordHoldSource(kind, repo string, number int, source string) error {
	if number <= 0 || strings.TrimSpace(repo) == "" || strings.TrimSpace(source) == "" {
		return nil
	}
	holdSourcesMu.Lock()
	defer holdSourcesMu.Unlock()
	path := HoldSourcesPath
	l := readHoldSourceLedgerLocked(path)
	key := holdSourceKey(kind, repo, number)
	if l.Applied[key] == nil {
		l.Applied[key] = map[string]time.Time{}
	}
	l.Applied[key][source] = time.Now().UTC()
	if err := writeHoldSourceLedgerLocked(path, l); err != nil {
		return fmt.Errorf("recording hold source %s for %s: %w", source, key, err)
	}
	return nil
}

// holdSourceDetector computes hold sources for one repository's held items of
// one kind during a single enumeration pass, then flushes the check cache and
// prunes records of items no longer held.
type holdSourceDetector struct {
	c        *Client
	kind     string
	owner    string
	repoName string
	repo     string
	start    time.Time
	ledger   holdSourceLedger
	held     map[string]bool
	fresh    map[string]holdSourceCheck
}

func (c *Client) newHoldSourceDetector(kind, owner, repoName string, start time.Time) *holdSourceDetector {
	holdSourcesMu.Lock()
	ledger := readHoldSourceLedgerLocked(HoldSourcesPath)
	holdSourcesMu.Unlock()
	return &holdSourceDetector{
		c:        c,
		kind:     kind,
		owner:    owner,
		repoName: repoName,
		repo:     owner + "/" + repoName,
		start:    start.UTC(),
		ledger:   ledger,
		held:     map[string]bool{},
		fresh:    map[string]holdSourceCheck{},
	}
}

// holdLabelClass splits an item's labels into the hold sources they carry on
// their own. otherHeld reports a remaining hold label (`hold` and its
// spellings) whose source needs the record or a check. The sentinel label is
// not a hold source.
type holdLabelClass struct {
	paused        bool
	reporterTrust bool
	otherHeld     bool
}

func (c *Client) classifyHoldLabels(labels []string) holdLabelClass {
	var out holdLabelClass
	sentinel := c.sentinelAlertLabel()
	clanker := ""
	pause := ""
	if c != nil {
		if cc, ok := reporterTrustClankerConfig(c.getIssueFilter()); ok {
			clanker = strings.TrimSpace(cc.ReporterTrustClankerRequestedLabel())
		}
		pause = c.ownPauseLabel()
	}
	var rest []string
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		switch {
		case pause != "" && strings.EqualFold(trimmed, pause):
			out.paused = true
		case sentinel != "" && strings.EqualFold(trimmed, sentinel):
		case clanker != "" && strings.EqualFold(trimmed, clanker):
			out.reporterTrust = true
		default:
			rest = append(rest, label)
		}
	}
	out.otherHeld = c.isHeld(rest)
	return out
}

// ownPauseLabel returns this hive's own `hive-pause/<hive-id>` label as set
// through SetHoldLabels, or "" when the client has none. Another hive's pause
// label is not a hold source here: isHeld ignores it too.
func (c *Client) ownPauseLabel() string {
	c.holdLabelsMu.RLock()
	defer c.holdLabelsMu.RUnlock()
	for _, label := range c.holdLabels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(strings.ToLower(label), "hive-pause/") {
			return label
		}
	}
	return ""
}

// isOtherHoldLabel reports whether a single label is a hold label that
// classifyHoldLabels would count as otherHeld.
func (c *Client) isOtherHoldLabel(label string) bool {
	return c.classifyHoldLabels([]string{label}).otherHeld
}

func (d *holdSourceDetector) recordSources(key string) []string {
	var out []string
	for src := range d.ledger.Applied[key] {
		out = append(out, src)
	}
	return out
}

// issueSources returns the hold sources of a held issue. No GitHub calls.
func (d *holdSourceDetector) issueSources(number int, labels []string) []string {
	key := holdSourceKey(HoldKindIssue, d.repo, number)
	d.held[key] = true
	class := d.c.classifyHoldLabels(labels)
	set := map[string]bool{}
	if class.paused {
		set[HoldSourceDashboardPause] = true
	}
	if class.reporterTrust {
		set[HoldSourceReporterTrust] = true
	}
	if class.otherHeld {
		recorded := d.recordSources(key)
		for _, src := range recorded {
			set[src] = true
		}
		if len(recorded) == 0 {
			set[HoldSourcePerson] = true
		}
	}
	return orderedHoldSources(set)
}

// prSources returns the hold sources of a held PR. GitHub is read (comments
// and label events, never written) only when the PR has a hold label the
// labels alone cannot explain and it was never checked or its updatedAt
// changed since the last check.
func (d *holdSourceDetector) prSources(ctx context.Context, number int, updatedAt time.Time, labels []string) []string {
	key := holdSourceKey(HoldKindPR, d.repo, number)
	d.held[key] = true
	class := d.c.classifyHoldLabels(labels)
	set := map[string]bool{}
	if class.paused {
		set[HoldSourceDashboardPause] = true
	}
	if class.reporterTrust {
		set[HoldSourceReporterTrust] = true
	}
	if class.otherHeld {
		attributed := false
		for _, src := range d.recordSources(key) {
			set[src] = true
			attributed = true
		}
		if check, ok := d.check(ctx, key, number, updatedAt); ok {
			for _, src := range check.Notices {
				set[src] = true
				attributed = true
			}
			if check.LatestHoldBy == holdActorPerson {
				set[HoldSourcePerson] = true
				attributed = true
			}
		}
		if !attributed {
			set[HoldSourceUnknown] = true
		}
	}
	return orderedHoldSources(set)
}

func (d *holdSourceDetector) check(ctx context.Context, key string, number int, updatedAt time.Time) (holdSourceCheck, bool) {
	if prev, ok := d.ledger.Checks[key]; ok && !updatedAt.IsZero() && prev.UpdatedAt.Equal(updatedAt) {
		return prev, true
	}
	// Without the App identity no notice or label event can be attributed
	// to Hive; fail closed to `unknown` rather than spend API calls.
	if d.c == nil || d.c.client == nil || strings.TrimSpace(d.c.appBotLogin) == "" {
		return holdSourceCheck{}, false
	}
	got, err := d.c.checkHeldPRHoldSources(ctx, d.owner, d.repoName, number)
	if err != nil {
		if d.c.logger != nil {
			d.c.logger.Warn("hold-source check failed; listing the hold's source as unknown this refresh",
				"repo", d.repo, "pr", number, "error", err)
		}
		return holdSourceCheck{}, false
	}
	got.UpdatedAt = updatedAt
	got.CheckedAt = time.Now().UTC()
	if !updatedAt.IsZero() {
		d.fresh[key] = got
	}
	return got, true
}

// flush stores fresh checks and drops this repository's records for items of
// this kind that were not held in this pass. A record written after the pass
// started survives, so a hold applied mid-pass is never lost.
func (d *holdSourceDetector) flush() {
	if d == nil {
		return
	}
	holdSourcesMu.Lock()
	defer holdSourcesMu.Unlock()
	l := readHoldSourceLedgerLocked(HoldSourcesPath)
	prefix := d.kind + ":" + strings.ToLower(d.repo) + "#"
	changed := false
	for key := range l.Checks {
		if strings.HasPrefix(key, prefix) && !d.held[key] {
			delete(l.Checks, key)
			changed = true
		}
	}
	for key, check := range d.fresh {
		l.Checks[key] = check
		changed = true
	}
	for key, sources := range l.Applied {
		if !strings.HasPrefix(key, prefix) || d.held[key] {
			continue
		}
		for src, at := range sources {
			if at.Before(d.start) {
				delete(sources, src)
				changed = true
			}
		}
		if len(sources) == 0 {
			delete(l.Applied, key)
			changed = true
		}
	}
	if changed {
		_ = writeHoldSourceLedgerLocked(HoldSourcesPath, l)
	}
}

// checkHeldPRHoldSources reads a held PR's comments and hold-label events.
// Read-only: it lists and never writes.
func (c *Client) checkHeldPRHoldSources(ctx context.Context, owner, repo string, number int) (holdSourceCheck, error) {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return holdSourceCheck{}, err
	}
	by, err := c.latestHoldLabelActor(ctx, owner, repo, number)
	if err != nil {
		return holdSourceCheck{}, err
	}
	set := c.holdNoticeSources(comments)
	return holdSourceCheck{Notices: orderedHoldSources(set), LatestHoldBy: by}, nil
}

// holdNoticeSources lists the sources whose Hive notice is among comments.
// Unlike levelHoldMetadataAndCommentsForPR, a reporter-trust or
// self-authorization notice does not hide the level-hold notice: a hold can
// have several sources.
func (c *Client) holdNoticeSources(comments []*gh.IssueComment) map[string]bool {
	set := map[string]bool{}
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		body := comment.GetBody()
		if c.isTrustedLevelHoldNoticeAuthor(comment) {
			if meta, ok := levelHoldMetadataFromNotice(body); ok {
				if strings.EqualFold(meta.Agent, "outreach") {
					set[HoldSourceOutreach] = true
				} else {
					set[HoldSourceLevelGate] = true
				}
			}
			if strings.Contains(body, SelfAuthorizationNoticeMarker) {
				set[HoldSourceSelfAuthorization] = true
			}
			if strings.Contains(body, HumanMergePathMarker) {
				set[HoldSourceHumanMergePath] = true
			}
		}
	}
	if hasReporterTrustNotice(comments, c.appBotLogin) {
		set[HoldSourceReporterTrust] = true
	}
	if hasSelfAuthorizationNotice(comments, c.appBotLogin) {
		set[HoldSourceSelfAuthorization] = true
	}
	return set
}

// latestHoldLabelActor reports who made the newest labeled/unlabeled event of
// a hold label (other than the pause, sentinel and reporter-trust extra
// labels): "app" for the Hive App, "person" for a non-bot user, and "" when
// the event cannot be attributed (no event, an unlabel, another bot, or no
// known App login).
func (c *Client) latestHoldLabelActor(ctx context.Context, owner, repo string, number int) (string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var latest *gh.IssueEvent
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return "", fmt.Errorf("listing issue events for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, event := range events {
			if event == nil {
				continue
			}
			kind := event.GetEvent()
			if kind != "labeled" && kind != "unlabeled" {
				continue
			}
			if !c.isOtherHoldLabel(event.GetLabel().GetName()) {
				continue
			}
			if latest == nil || event.GetCreatedAt().After(latest.GetCreatedAt().Time) {
				latest = event
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if latest == nil || latest.GetEvent() != "labeled" {
		return "", nil
	}
	actor := latest.GetActor()
	login := strings.TrimSpace(safeGetLogin(actor))
	switch {
	case login == "":
		return "", nil
	case strings.TrimSpace(c.appBotLogin) != "" && strings.EqualFold(login, c.appBotLogin):
		return holdActorApp, nil
	case strings.HasSuffix(strings.ToLower(login), "[bot]") || strings.EqualFold(actor.GetType(), "Bot"):
		return "", nil
	default:
		return holdActorPerson, nil
	}
}

// orderedHoldSources returns set's members in HoldSources order, followed by
// any unrecognised value sorted, or nil for an empty set.
func orderedHoldSources(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	seen := map[string]bool{}
	for _, src := range HoldSources {
		if set[src] {
			out = append(out, src)
			seen[src] = true
		}
	}
	var extra []string
	for src := range set {
		if !seen[src] {
			extra = append(extra, src)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}
