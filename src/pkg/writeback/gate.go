package writeback

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Item is what the gate needs to know about the target of a write. Callers
// fill it from whatever enumeration they already hold (a list payload, a
// forgestate snapshot, a GET); nothing here costs an extra GitHub call.
type Item struct {
	Number int
	// IsPR distinguishes a pull request from an issue; the opt-in label set
	// is the same for both today.
	IsPR bool
	// Author is the forge login of the item's author.
	Author string
	// HiveAuthored is true when the item is the hive's own: App-authored,
	// an attributed relay PR, or an issue the hive filed. Own items are
	// never gated (except per-repo mode off).
	HiveAuthored bool
	// AgentAuthored is true when the author is an agent rather than a
	// person: a contributor's clanker (author/clanker) or another hive.
	// With SupersededBy it lets the adoption kind close the item by
	// policy (ReasonSupersededAgentPR).
	AgentAuthored bool
	// SupersededBy is the hive's own pull request that carries this item's
	// commits (the supersede adoption's copy), 0 when none. Set only by a
	// caller whose policy closes superseded originals on adopt
	// (governor.labels.stage.adoption.close_superseded: on_adopt).
	SupersededBy int
	// Labels is the item's current label set.
	Labels []string
	// CreatedAt is the item's creation time; the backfill guard compares it
	// with the repo's first_seen.
	CreatedAt time.Time
}

// Decision is the gate's answer.
type Decision struct {
	Allowed bool
	// Reason is a short, stable, log-friendly token (ReasonSolicited, …).
	Reason string
	// Detail is the human-readable explanation shown in the drill-down.
	Detail string
}

// Reasons.
const (
	ReasonOwnItem       = "own-item"
	ReasonRepoFull      = "repo-mode-full"
	ReasonLabelOptIn    = "opted-in"
	ReasonLevel         = "level-unlabeled"
	ReasonNoGate        = "no-gate"
	ReasonIssueFile     = "issue-file"
	ReasonRepoOff       = "repo-mode-off"
	ReasonRepoIssues    = "repo-mode-issues-only"
	ReasonRepoLabels    = "repo-mode-labels-only"
	ReasonOwnOnly       = "own-only-kind"
	ReasonBelowLevel    = "below-min-level"
	ReasonNeedsLabel    = "needs-opt-in-label"
	ReasonBackfill      = "backfill-off"
	ReasonLevelUnknown  = "level-unknown"
	ReasonRepoNotListed = "repo-not-in-project"
	// ReasonRepoPaused refuses EVERY kind on an operator-paused repo
	// (#6203), own items included: a pause means the hive is deliberately
	// quiet on the repo, and the advisory digest edit that kept landing on
	// a paused torch-spyre showed that "unsolicited only" was not enough.
	ReasonRepoPaused = "repo-paused"
	// ReasonSupersededAgentPR allows the adoption kind on an agent-authored
	// PR the hive has re-authored as its own copy (hivecommons/hive#15526
	// stayed open at "superseded" for want of an opt-in label): the pair
	// is one logical PR and the copy carries the author's commits and
	// sign-off, so the close is the hive's own housekeeping, not an
	// unsolicited write on a person's work.
	ReasonSupersededAgentPR = "superseded-agent-pr"
)

// RepoPauseInfo is the provenance of an operator pause, as config records
// it. Both fields may be empty for a pause written by hand into the config.
type RepoPauseInfo struct {
	By     string
	Reason string
}

// ErrSuppressed is returned by writers that surface a refusal as an error.
var ErrSuppressed = errors.New("writeback: unsolicited write suppressed")

// Suppressed is one refused write, kept for the dashboard drill-down.
type Suppressed struct {
	At     time.Time `json:"at"`
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	IsPR   bool      `json:"is_pr"`
	Kind   Kind      `json:"kind"`
	Reason string    `json:"reason"`
	Detail string    `json:"detail"`
	// Preview is what would have been written (first line of the comment,
	// the label names, …) when the caller supplied it.
	Preview string `json:"preview,omitempty"`
}

// RepoStatus is one repo's block in the status payload.
type RepoStatus struct {
	FirstSeen    time.Time `json:"first_seen,omitempty"`
	Writable     bool      `json:"writable"`
	WritableAt   time.Time `json:"writable_at,omitempty"`
	Mode         RepoMode  `json:"mode,omitempty"`
	Level        int       `json:"level"`
	Suppressed24 int       `json:"suppressed_24h"`
	// PreExisting counts distinct items the backfill guard kept the hive
	// off since the repo became writable; it feeds the dashboard banner.
	PreExisting int `json:"pre_existing_untouched"`
}

// Status is the writeback block of /api/status.
type Status struct {
	Enabled       bool                  `json:"enabled"`
	MinLevel      int                   `json:"min_level"`
	RequireLabels bool                  `json:"require_labels"`
	Backfill      bool                  `json:"backfill"`
	OptInLabels   []string              `json:"opt_in_labels"`
	Suppressed24h int                   `json:"suppressed_24h"`
	Repos         map[string]RepoStatus `json:"repos"`
	Kinds         []KindRow             `json:"kinds"`
	// Banners lists repos that became writable with pre-existing items the
	// hive left alone (one banner each).
	Banners []Banner `json:"banners,omitempty"`
	// Dedupe is the content layer: settings plus last-hour counters of
	// duplicate, burst and rate-capped suppressions.
	Dedupe DedupeCounters `json:"dedupe"`
}

// Banner is the "hive can now write to <repo>; N existing PRs were left
// untouched" dashboard notice.
type Banner struct {
	Repo       string    `json:"repo"`
	WritableAt time.Time `json:"writable_at"`
	Untouched  int       `json:"untouched"`
	Message    string    `json:"message"`
}

// repoRecord is the persisted per-repo state.
type repoRecord struct {
	FirstSeen  time.Time `json:"first_seen"`
	WritableAt time.Time `json:"writable_at,omitempty"`
	// LastForbidden is the last 403/404 a write to this repo got, so the
	// first 2xx after it marks the moment the App gained write.
	LastForbidden time.Time `json:"last_forbidden,omitempty"`
	// PreExisting is the set of item numbers the backfill guard refused.
	PreExisting map[int]bool `json:"pre_existing,omitempty"`
}

// Deps are the gate's lookups, all optional.
type Deps struct {
	// LevelFor returns the effective ACMM level of repo (0 = unknown).
	LevelFor func(repo string) int
	// OptInLabels returns the review opt-in labels for repo. nil or empty
	// falls back to DefaultOptInLabel.
	OptInLabels func(repo string) []string
	// RepoListed reports whether repo is in project.repos. nil means every
	// repo is listed.
	RepoListed func(repo string) bool
	// RepoPause reports whether repo is under an operator pause and who
	// paused it. nil means no repo is ever paused. Checked before anything
	// else: a paused repo refuses every kind, own items included.
	RepoPause func(repo string) (RepoPauseInfo, bool)
	// Config, when set, is read on every decision instead of the Config
	// passed to New, so a dashboard edit of the writeback block is live
	// without re-wiring (cmd/hive passes a closure over the shared config).
	Config func() Config
	Logger *slog.Logger
	Now    func() time.Time
}

// Gate is the write-back policy. One per process, built by cmd/hive and
// installed on the GitHub client; tests build their own.
type Gate struct {
	mu      sync.Mutex
	cfg     Config
	deps    Deps
	path    string
	repos   map[string]*repoRecord
	recent  []Suppressed
	summary map[string]bool
	// dedupe is the content layer's table (dedupe.go).
	dedupe *dedupeState
}

// SuppressedRetention bounds the drill-down list (and the 24h counter).
const SuppressedRetention = 24 * time.Hour

// maxRecent bounds memory for the drill-down.
const maxRecent = 2000

// New builds a gate over cfg. path is the JSON file first_seen is persisted
// to (empty = in-memory only); a missing or unreadable file starts empty.
func New(cfg Config, deps Deps, path string) *Gate {
	g := &Gate{cfg: cfg, deps: deps, path: path, repos: map[string]*repoRecord{}, summary: map[string]bool{}, dedupe: newDedupeState()}
	if g.deps.Now == nil {
		g.deps.Now = time.Now
	}
	if g.deps.Logger == nil {
		g.deps.Logger = slog.Default()
	}
	g.load()
	return g
}

// SetConfig swaps the policy (dashboard edits take effect on the next
// decision).
func (g *Gate) SetConfig(cfg Config) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.cfg = cfg
	g.mu.Unlock()
}

// Config returns the current policy.
func (g *Gate) Config() Config {
	if g == nil {
		return Config{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfgLocked()
}

func repoKey(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// cfgLocked is the policy in force: the live provider when one is set.
func (g *Gate) cfgLocked() Config {
	if g.deps.Config != nil {
		return g.deps.Config()
	}
	return g.cfg
}

func (g *Gate) load() {
	if g.path == "" {
		return
	}
	data, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	// Current shape: {"repos": {...}, "dedupe": {...}}. The pre-dedupe
	// file was the bare repos map; its keys are owner/name so "repos"
	// cannot collide with one.
	var file stateFile
	if err := json.Unmarshal(data, &file); err == nil && file.Repos != nil {
		g.repos = file.Repos
		if file.Dedupe != nil {
			file.Dedupe.init()
			g.dedupe = file.Dedupe
		}
		return
	}
	var repos map[string]*repoRecord
	if err := json.Unmarshal(data, &repos); err != nil || repos == nil {
		return
	}
	g.repos = repos
}

// stateFile is the on-disk shape of the gate's state.
type stateFile struct {
	Repos  map[string]*repoRecord `json:"repos"`
	Dedupe *dedupeState           `json:"dedupe,omitempty"`
}

func (g *Gate) saveLocked() {
	if g.path == "" {
		return
	}
	file := stateFile{Repos: g.repos}
	if g.dedupe != nil && !g.dedupe.empty() {
		file.Dedupe = g.dedupe
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(g.path), 0o755)
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, g.path)
}

// recordLocked returns (creating if needed) the record for repo, stamping
// first_seen on creation.
func (g *Gate) recordLocked(repo string) *repoRecord {
	key := repoKey(repo)
	rec, ok := g.repos[key]
	if !ok {
		rec = &repoRecord{FirstSeen: g.deps.Now().UTC()}
		g.repos[key] = rec
		g.saveLocked()
	}
	return rec
}

// ObserveRepo stamps first_seen for repo if the gate has not seen it yet.
// cmd/hive calls it for every project.repos entry at boot so a repo listed
// today is "seen" today even before any sweep reaches it.
func (g *Gate) ObserveRepo(repo string) {
	if g == nil || strings.TrimSpace(repo) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recordLocked(repo)
}

// NoteWriteOutcome records the result of a real GitHub write to repo so the
// gate can detect the App gaining write: a 403/404 marks the repo as not
// writable; the first 2xx after one marks WritableAt now, which the
// backfill guard uses as the cut-off for items created before it.
func (g *Gate) NoteWriteOutcome(repo string, statusCode int) {
	if g == nil || strings.TrimSpace(repo) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.recordLocked(repo)
	now := g.deps.Now().UTC()
	switch {
	case statusCode == 403 || statusCode == 404:
		rec.LastForbidden = now
		g.saveLocked()
	case statusCode >= 200 && statusCode < 300:
		if rec.WritableAt.IsZero() {
			rec.WritableAt = now
			g.saveLocked()
		}
	}
}

// FirstSeen returns when the gate first saw repo (zero if never).
func (g *Gate) FirstSeen(repo string) time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if rec, ok := g.repos[repoKey(repo)]; ok {
		return rec.FirstSeen
	}
	return time.Time{}
}

// backfillCutoff is the moment before which an item counts as pre-existing:
// the later of first_seen and writable_at.
func (rec *repoRecord) backfillCutoff() time.Time {
	cut := rec.FirstSeen
	if rec.WritableAt.After(cut) {
		cut = rec.WritableAt
	}
	return cut
}

// Allowed is the gate. repo is "owner/name" (any case); item describes the
// target; kind names the write. A nil gate allows everything (the client
// without a gate installed behaves as before) but that is never the case
// in cmd/hive, which installs one at boot.
func (g *Gate) Allowed(repo string, item Item, kind Kind) Decision {
	return g.AllowedPreview(repo, item, kind, "")
}

// AllowedPreview is Allowed with what-would-have-been-written text for the
// drill-down.
func (g *Gate) AllowedPreview(repo string, item Item, kind Kind, preview string) Decision {
	if g == nil {
		return Decision{Allowed: true, Reason: ReasonNoGate}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.decideLocked(repo, item, kind)
	if d.Allowed {
		// Content dedupe (dedupe.go) runs only on comment kinds with a
		// body: Allowed() without preview stays policy-only.
		d = g.contentCheckLocked(repo, item, kind, preview, d)
	}
	if !d.Allowed {
		g.suppressLocked(repo, item, kind, d, preview)
	}
	return d
}

func (g *Gate) decideLocked(repo string, item Item, kind Kind) Decision {
	if info, paused := g.repoPauseLocked(repo); paused {
		return Decision{Reason: ReasonRepoPaused, Detail: RepoPausedDetail(repo, info)}
	}
	cfg := g.cfgLocked()
	mode := cfg.RepoModeFor(repo)
	if mode == RepoModeOff {
		return Decision{Reason: ReasonRepoOff, Detail: fmt.Sprintf("writeback.per_repo[%s].mode is off", repo)}
	}
	if kind.IsIssueFileKind() {
		if mode == RepoModeLabelsOnly {
			return Decision{Reason: ReasonRepoLabels, Detail: "repo mode labels-only does not file issues"}
		}
		return g.levelCheckLocked(repo, item, kind, Decision{Allowed: true, Reason: ReasonIssueFile})
	}
	if mode == RepoModeIssuesOnly {
		return Decision{Reason: ReasonRepoIssues, Detail: fmt.Sprintf("writeback.per_repo[%s].mode is issues-only; %s touches an existing item", repo, kind)}
	}
	if mode == RepoModeLabelsOnly && !kind.IsLabelKind() {
		return Decision{Reason: ReasonRepoLabels, Detail: fmt.Sprintf("writeback.per_repo[%s].mode is labels-only; %s is not a label write", repo, kind)}
	}
	if item.HiveAuthored {
		return Decision{Allowed: true, Reason: ReasonOwnItem}
	}
	// Everything below concerns an item the hive did not author. An item
	// carrying the opt-in label was asked for by a human, so the backfill
	// guard (which exists for items nobody asked about) does not apply.
	optedIn := g.optedInLocked(repo, item)
	if !optedIn {
		if backfilled, cut := g.backfillBlockedLocked(repo, item); backfilled {
			return Decision{Reason: ReasonBackfill, Detail: fmt.Sprintf("#%d was created %s, before hive could write to %s (%s); writeback.unsolicited.backfill is off", item.Number, item.CreatedAt.UTC().Format(time.RFC3339), repo, cut.Format(time.RFC3339))}
		}
	}
	if mode == RepoModeFull {
		return Decision{Allowed: true, Reason: ReasonRepoFull}
	}
	if g.deps.RepoListed != nil && !g.deps.RepoListed(repo) {
		return Decision{Reason: ReasonRepoNotListed, Detail: repo + " is not in project.repos"}
	}
	p := cfg.Policy(kind)
	if p.OwnOnly {
		return Decision{Reason: ReasonOwnOnly, Detail: fmt.Sprintf("%s only ever writes on hive-authored items; #%d is by %s", kind, item.Number, item.Author)}
	}
	// An agent's PR the hive re-authored as its own copy: the close is
	// housekeeping on one logical PR of the hive's, so neither the level
	// nor the opt-in label applies (an operator's own_only / repo mode /
	// pause above still do).
	if kind == KindContributorAdopt && item.AgentAuthored && item.SupersededBy > 0 {
		return Decision{Allowed: true, Reason: ReasonSupersededAgentPR, Detail: fmt.Sprintf("#%d is agent-authored and the hive's copy #%d carries its commits; close_superseded is on_adopt", item.Number, item.SupersededBy)}
	}
	level := 0
	if g.deps.LevelFor != nil {
		level = g.deps.LevelFor(repo)
	}
	if level <= 0 {
		return Decision{Reason: ReasonLevelUnknown, Detail: "acmm_level is unset; unsolicited writes need a level"}
	}
	if !p.RequireLabel {
		if level >= p.MinLevel {
			return Decision{Allowed: true, Reason: ReasonLevel, Detail: fmt.Sprintf("L%d ≥ L%d", level, p.MinLevel)}
		}
		return Decision{Reason: ReasonBelowLevel, Detail: fmt.Sprintf("%s needs L%d; %s is at L%d", kind, p.MinLevel, repo, level)}
	}
	if optedIn {
		if level >= p.MinLevel {
			return Decision{Allowed: true, Reason: ReasonLabelOptIn, Detail: fmt.Sprintf("opt-in label present and L%d ≥ L%d", level, p.MinLevel)}
		}
		return Decision{Reason: ReasonBelowLevel, Detail: fmt.Sprintf("%s needs L%d even with the opt-in label; %s is at L%d", kind, p.MinLevel, repo, level)}
	}
	if p.UnlabeledMinLevel > 0 && level >= p.UnlabeledMinLevel {
		return Decision{Allowed: true, Reason: ReasonLevel, Detail: fmt.Sprintf("no opt-in label but L%d ≥ L%d", level, p.UnlabeledMinLevel)}
	}
	labels := g.optInLabelsLocked(repo)
	if p.UnlabeledMinLevel > 0 {
		return Decision{Reason: ReasonNeedsLabel, Detail: fmt.Sprintf("#%d carries none of %s; %s needs the label at L%d or L%d without it (%s is at L%d)", item.Number, strings.Join(labels, ","), kind, p.MinLevel, p.UnlabeledMinLevel, repo, level)}
	}
	return Decision{Reason: ReasonNeedsLabel, Detail: fmt.Sprintf("#%d carries none of %s; %s needs the label at L%d (%s is at L%d)", item.Number, strings.Join(labels, ","), kind, p.MinLevel, repo, level)}
}

// repoPauseLocked asks the configured pause predicate, if any.
func (g *Gate) repoPauseLocked(repo string) (RepoPauseInfo, bool) {
	if g.deps.RepoPause == nil {
		return RepoPauseInfo{}, false
	}
	return g.deps.RepoPause(repo)
}

// RepoPaused reports whether repo is operator-paused, with the pause's
// provenance, for writers that are not a gate Kind (the advisory digest)
// but must still fall silent on a paused repo. A nil gate never pauses.
func (g *Gate) RepoPaused(repo string) (RepoPauseInfo, bool) {
	if g == nil {
		return RepoPauseInfo{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.repoPauseLocked(repo)
}

// RepoPausedDetail is the drill-down/log text for a repo-paused refusal.
func RepoPausedDetail(repo string, info RepoPauseInfo) string {
	d := repo + " is paused by the operator"
	if info.By != "" {
		d += " (by " + info.By
		if info.Reason != "" {
			d += ": " + info.Reason
		}
		d += ")"
	} else if info.Reason != "" {
		d += " (" + info.Reason + ")"
	}
	return d + "; resume the repo to allow hive writes again"
}

// levelCheckLocked applies the kind's min level to an issue-file decision.
func (g *Gate) levelCheckLocked(repo string, _ Item, kind Kind, ok Decision) Decision {
	p := g.cfgLocked().Policy(kind)
	level := 0
	if g.deps.LevelFor != nil {
		level = g.deps.LevelFor(repo)
	}
	// An unset level cannot be judged; filing an issue is the hive's own
	// item and the per-repo mode above is the operator's lever for it.
	if level > 0 && p.MinLevel > 0 && level < p.MinLevel {
		return Decision{Reason: ReasonBelowLevel, Detail: fmt.Sprintf("%s needs L%d; %s is at L%d", kind, p.MinLevel, repo, level)}
	}
	return ok
}

func (g *Gate) backfillBlockedLocked(repo string, item Item) (bool, time.Time) {
	if g.cfgLocked().BackfillEnabled() || item.CreatedAt.IsZero() {
		return false, time.Time{}
	}
	rec := g.recordLocked(repo)
	cut := rec.backfillCutoff()
	if cut.IsZero() || !item.CreatedAt.Before(cut) {
		return false, cut
	}
	if rec.PreExisting == nil {
		rec.PreExisting = map[int]bool{}
	}
	if !rec.PreExisting[item.Number] {
		rec.PreExisting[item.Number] = true
		g.saveLocked()
	}
	return true, cut
}

func (g *Gate) optInLabelsLocked(repo string) []string {
	if g.deps.OptInLabels != nil {
		if ls := g.deps.OptInLabels(repo); len(ls) > 0 {
			return ls
		}
	}
	return []string{DefaultOptInLabel}
}

func (g *Gate) optedInLocked(repo string, item Item) bool {
	for _, want := range g.optInLabelsLocked(repo) {
		for _, have := range item.Labels {
			if strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(want)) {
				return true
			}
		}
	}
	return false
}

func (g *Gate) suppressLocked(repo string, item Item, kind Kind, d Decision, preview string) {
	now := g.deps.Now().UTC()
	g.recent = append(g.recent, Suppressed{At: now, Repo: repo, Number: item.Number, IsPR: item.IsPR, Kind: kind, Reason: d.Reason, Detail: d.Detail, Preview: firstLine(preview)})
	g.pruneLocked(now)
	g.deps.Logger.Debug("writeback: suppressed unsolicited write", "repo", repo, "number", item.Number, "kind", string(kind), "reason", d.Reason)
}

func (g *Gate) pruneLocked(now time.Time) {
	g.pruneDedupeLocked(now)
	cut := now.Add(-SuppressedRetention)
	i := 0
	for i < len(g.recent) && g.recent[i].At.Before(cut) {
		i++
	}
	if i > 0 {
		g.recent = append([]Suppressed(nil), g.recent[i:]...)
	}
	if len(g.recent) > maxRecent {
		g.recent = append([]Suppressed(nil), g.recent[len(g.recent)-maxRecent:]...)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// SweepSummary logs the one "writeback: skipped N pre-existing items on
// <repo> (backfill off)" line a sweep owes after its pass over repo, once
// per repo per process (later passes are silent unless the count grew).
func (g *Gate) SweepSummary(repo string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.repos[repoKey(repo)]
	if !ok || len(rec.PreExisting) == 0 {
		return
	}
	key := repoKey(repo) + "|" + fmt.Sprint(len(rec.PreExisting))
	if g.summary[key] {
		return
	}
	g.summary[key] = true
	g.deps.Logger.Info(fmt.Sprintf("writeback: skipped %d pre-existing items on %s (backfill off)", len(rec.PreExisting), repo))
}

// Recent returns the suppressed writes of the last 24h, newest first.
func (g *Gate) Recent() []Suppressed {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.deps.Now().UTC())
	out := make([]Suppressed, len(g.recent))
	for i, s := range g.recent {
		out[len(g.recent)-1-i] = s
	}
	return out
}

// Snapshot is the /api/status block.
func (g *Gate) Snapshot() Status {
	if g == nil {
		return Status{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.deps.Now().UTC()
	g.pruneLocked(now)
	cfg := g.cfgLocked()
	st := Status{
		Enabled:       true,
		MinLevel:      cfg.MinLevelOrDefault(),
		RequireLabels: cfg.RequireLabelsEnabled(),
		Backfill:      cfg.BackfillEnabled(),
		OptInLabels:   g.optInLabelsLocked(""),
		Suppressed24h: len(g.recent),
		Repos:         map[string]RepoStatus{},
		Kinds:         cfg.EffectiveKinds(),
		Dedupe:        g.dedupeCountersLocked(now),
	}
	per := map[string]int{}
	for _, s := range g.recent {
		per[repoKey(s.Repo)]++
	}
	keys := make([]string, 0, len(g.repos))
	for k := range g.repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rec := g.repos[k]
		level := 0
		if g.deps.LevelFor != nil {
			level = g.deps.LevelFor(k)
		}
		rs := RepoStatus{FirstSeen: rec.FirstSeen, Writable: !rec.WritableAt.IsZero(), WritableAt: rec.WritableAt, Mode: cfg.RepoModeFor(k), Level: level, Suppressed24: per[k], PreExisting: len(rec.PreExisting)}
		st.Repos[k] = rs
		if rs.PreExisting > 0 && !cfg.BackfillEnabled() {
			st.Banners = append(st.Banners, Banner{Repo: k, WritableAt: rec.WritableAt, Untouched: rs.PreExisting,
				Message: fmt.Sprintf("hive can now write to %s; %d existing PRs were left untouched — enable backfill to process them", k, rs.PreExisting)})
		}
	}
	return st
}
