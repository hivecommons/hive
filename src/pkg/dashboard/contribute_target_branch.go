package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Contributor dispatch honours the target branch an issue declares
// (hivecommons/hive#13146).
//
// A scanner-filed issue says where its code lives: a `[v6]` title prefix or a
// `**Target: v6.**` body line. Dispatch used to read only the title's `[vN]`
// prefix and otherwise force this hive's own build branch, so a `Target: v6`
// issue was offered as v5 work, the agent found nothing to fix on v5, and the
// only verdict it had for that was already_done — which labelled the issue
// `hive/already-done` for code that was simply on another branch (#13090).
//
// The dispatcher now reads both spellings, checks the declared branch exists
// in the repository (branch list, cached per repo per sweep), and tells the
// agent to base on and open the PR against it. A declared branch the repo
// does not have falls back to the default and the prompt says so, so the
// agent never silently inherits a wrong base or fails at `gh pr create`.

// targetBranchCacheTTL is how long one repository's branch list is reused
// across dispatches before it is fetched again — long enough to cover a
// selection sweep, short enough that a branch created today is seen today.
const targetBranchCacheTTL = 5 * time.Minute

// targetBranchListTimeout bounds the one API round trip the validation costs.
const targetBranchListTimeout = 20 * time.Second

// Where a task's declared target came from.
const (
	targetSourceTitle   = "title"
	targetSourceBody    = "body"
	targetSourceVerdict = "verdict"
)

// targetBranchBodyPattern matches the scanner convention `**Target: v6.**`
// and the plain `Target: v6` / `Target branch: release-1.2` on a line of its
// own. Markdown emphasis and backticks around the branch are tolerated.
var targetBranchBodyPattern = regexp.MustCompile("(?mi)^\\s*(?:[*_]{1,2})?\\s*target(?:\\s+branch)?\\s*:\\s*(?:[*_]{1,2})?\\s*`?([A-Za-z0-9][A-Za-z0-9._/-]*)`?")

// maxBranchNameLen bounds a declared branch name; GitHub refs are far shorter
// than this in practice and nothing longer is a plausible target.
const maxBranchNameLen = 100

// taskTargetBranch is the resolved base for one dispatch: Branch is what the
// prompt names (empty keeps the ask-the-repository wording), Declared and
// Source say what the issue asked for, and Invalid records that the declared
// branch was not found so the prompt can say the base fell back.
type taskTargetBranch struct {
	Branch   string
	Declared string
	Source   string
	Invalid  bool
}

// promptNote renders the one sentence the prompt adds when a target branch
// was declared, so the agent knows why the base differs from the default (or
// why it does not, when the declared branch is missing). Empty when nothing
// was declared.
func (t taskTargetBranch) promptNote(repoFull string) string {
	if t.Declared == "" {
		return ""
	}
	how := map[string]string{
		targetSourceTitle:   "its title prefix",
		targetSourceBody:    "a 'Target:' line in its body",
		targetSourceVerdict: "a previous contributor's wrong_base verdict",
	}[t.Source]
	if how == "" {
		how = "its metadata"
	}
	if t.Invalid {
		return fmt.Sprintf(
			"This issue declares its target branch as '%s' (%s), but %s has no such branch, "+
				"so the base above is the repository's default instead; if the code the issue "+
				"describes is not on that base, report wrong_base as described below rather "+
				"than treating the issue as already done. ",
			t.Declared, how, repoFull)
	}
	return fmt.Sprintf(
		"This issue declares its target branch as '%s' (%s), and that branch exists in %s, "+
			"so the base above is that branch rather than the repository's default; the PR "+
			"must target it too. ",
		t.Declared, how, repoFull)
}

// isPlausibleBranchName reports whether s could be a git branch name worth
// checking against the repository: ref-safe characters only, no `..`, no
// leading `-` or `/`, no trailing `/` or `.lock`.
func isPlausibleBranchName(s string) bool {
	if s == "" || len(s) > maxBranchNameLen {
		return false
	}
	if strings.Contains(s, "..") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".lock") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '/', r == '-':
		default:
			return false
		}
	}
	return true
}

// titlePrefixTags returns every `[tag]` in the title's structured prefix run
// (case preserved, since branch names are case-sensitive), using the same
// prefix grammar as releaseLineFromTitle: brackets and decorative runes are
// skipped, the first ordinary word ends the run.
func titlePrefixTags(title string) []string {
	var tags []string
	rest := strings.TrimSpace(title)
	const maxPrefixTokens = 8
	for i := 0; rest != "" && i < maxPrefixTokens; i++ {
		r, size := utf8.DecodeRuneInString(rest)
		switch {
		case r == '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				return tags
			}
			if tag := strings.TrimSpace(rest[1:end]); tag != "" {
				tags = append(tags, tag)
			}
			rest = strings.TrimSpace(rest[end+1:])
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return tags
		default:
			rest = strings.TrimSpace(rest[size:])
		}
	}
	return tags
}

// declaredTargetBranchFromBody returns the branch a `Target: <branch>` body
// line declares, or "" when the body has none.
func declaredTargetBranchFromBody(body string) string {
	m := targetBranchBodyPattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	b := strings.TrimRight(m[1], ".")
	if !isPlausibleBranchName(b) {
		return ""
	}
	return b
}

// declaredTargetBranch returns what the issue itself declares as its target
// branch and where it said so: the title's `[vN]` release-line prefix first
// (the established convention), then a `Target:` body line. A non-release
// bracket prefix such as `[quality]` is a classifier lane, not a branch, and
// is only ever considered by the caller when it can be checked against the
// repository's real branch list (see resolveTaskTargetBranch).
func declaredTargetBranch(title, body string) (branch, source string) {
	if line := releaseLineFromTitle(title); line != "" {
		return line, targetSourceTitle
	}
	if b := declaredTargetBranchFromBody(body); b != "" {
		return b, targetSourceBody
	}
	return "", ""
}

// branchListCache memoizes one repository's branch names for
// targetBranchCacheTTL, so a sweep that dispatches several issues for the
// same repository pays for the list once.
type branchListCache struct {
	mu      sync.Mutex
	entries map[string]branchListEntry
}

type branchListEntry struct {
	names     map[string]bool
	fetchedAt time.Time
}

func (c *branchListCache) get(repo string, now time.Time) (map[string]bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[repo]
	if !ok || now.Sub(e.fetchedAt) >= targetBranchCacheTTL {
		return nil, false
	}
	return e.names, true
}

func (c *branchListCache) put(repo string, names []string, now time.Time) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]branchListEntry{}
	}
	c.entries[repo] = branchListEntry{names: set, fetchedAt: now}
	return set
}

// canValidateBranches reports whether this hub can ask the repository for
// its branch list at all (a lister seam or a GitHub client).
func (h *ContributeWSHub) canValidateBranches() bool {
	if h == nil {
		return false
	}
	if h.branchLister != nil {
		return true
	}
	return h.server != nil && h.server.deps != nil && h.server.deps.GHClient != nil
}

// repoBranches returns repo's branch set, from the per-sweep cache when
// fresh. ok is false when the hub cannot list branches (no client) or the
// list failed; callers then treat the declared branch as unverified.
func (h *ContributeWSHub) repoBranches(ctx context.Context, repo string) (map[string]bool, bool) {
	if !h.canValidateBranches() || strings.TrimSpace(repo) == "" {
		return nil, false
	}
	now := time.Now()
	if names, ok := h.branchCache.get(repo, now); ok {
		return names, true
	}
	lister := h.branchLister
	if lister == nil {
		lister = h.server.deps.GHClient.ListBranchNames
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, targetBranchListTimeout)
	defer cancel()
	names, err := lister(ctx, repo)
	if err != nil {
		h.logger.Warn("[contribute-ws] target branch: listing repository branches failed; declared target left unverified",
			"repo", repo, "error", err.Error())
		return nil, false
	}
	return h.branchCache.put(repo, names, now), true
}

// branchExists reports whether repo has branch. checked is false when the
// hub could not consult the repository at all.
func (h *ContributeWSHub) branchExists(ctx context.Context, repo, branch string) (exists, checked bool) {
	branch = strings.TrimSpace(branch)
	if !isPlausibleBranchName(branch) {
		return false, true
	}
	names, ok := h.repoBranches(ctx, repo)
	if !ok {
		return false, false
	}
	return names[branch], true
}

// honourTargetBranch resolves the hub "honour target branch" knob (default
// true); nil-safe like the other config readers on this hub.
func (h *ContributeWSHub) honourTargetBranch() bool {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return true
	}
	return h.server.deps.Config.Hub.ContributeHonourTargetBranchEnabled()
}

// resolveTaskTargetBranch decides the base branch one dispatch names. In
// order: a target recorded by an earlier wrong_base verdict on this item
// (contribute_wrong_base.go), the issue's own declaration (title `[vN]`,
// then a body `Target:` line, then — only when the repository's branch list
// is available to confirm it — any other bracket prefix), then the
// pre-#13146 answer (taskBaseBranch: title `[vN]` or this hive's build
// branch for its own repository, else the ask-the-repository wording).
//
// A declared branch is accepted when the repository has it. When the
// repository cannot be consulted, a title `[vN]`, body `Target:` or verdict
// target is accepted unverified — the same trust the title prefix always
// had — and a generic bracket is ignored. A declared branch the repository
// does NOT have falls back to the default and is reported Invalid so the
// prompt says so.
func (h *ContributeWSHub) resolveTaskTargetBranch(ctx context.Context, repoFull, key, title, body string) taskTargetBranch {
	fallback := taskBaseBranch(title, repoFull, upstreamBranch())
	if !h.honourTargetBranch() {
		return taskTargetBranch{Branch: fallback}
	}
	declared, source := "", ""
	if ov, ok := h.targetBranchOverride(key); ok {
		declared, source = ov.Branch, targetSourceVerdict
	} else {
		declared, source = declaredTargetBranch(title, body)
	}
	canCheck := h.canValidateBranches()
	if declared == "" && canCheck {
		for _, tag := range titlePrefixTags(title) {
			if isPlausibleBranchName(tag) {
				if exists, checked := h.branchExists(ctx, repoFull, tag); checked && exists {
					declared, source = tag, targetSourceTitle
				}
				break
			}
		}
	}
	if declared == "" {
		return taskTargetBranch{Branch: fallback}
	}
	exists, checked := h.branchExists(ctx, repoFull, declared)
	if !checked {
		return taskTargetBranch{Branch: declared, Declared: declared, Source: source}
	}
	if exists {
		return taskTargetBranch{Branch: declared, Declared: declared, Source: source}
	}
	// The declared branch is not in the repository: the title's own
	// release-line prefix is the very thing that failed, so the fallback is
	// computed from a blank title — this hive's build branch for its own
	// repo, the ask-the-repository wording for anyone else's.
	h.logger.Warn("[contribute-ws] target branch: declared branch not found in repository; dispatching against the default base",
		"repo", repoFull, "key", key, "declared", declared, "source", source)
	return taskTargetBranch{Branch: taskBaseBranch("", repoFull, upstreamBranch()), Declared: declared, Source: source, Invalid: true}
}

// Target-branch overrides (#13146): a contributor's wrong_base verdict named
// the branch the issue's code actually lives on and the hub confirmed it
// exists, so the next offer of the same item bases on it. The contribute
// ledger is the durable place for it — the queue's other per-item facts
// (cooldowns, verdicts, streaks) already live there, and nothing in the
// labelling convention carries a branch.

const targetBranchOverridesFileName = "target-branch-overrides.json"

// targetBranchOverrideTTL expires an override nobody acted on, so a branch
// that is deleted or merged away does not pin the item forever.
const targetBranchOverrideTTL = 30 * 24 * time.Hour

type targetBranchOverride struct {
	Branch         string    `json:"branch"`
	DispatchedBase string    `json:"dispatched_base,omitempty"`
	Reporter       string    `json:"reporter,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
}

func (h *ContributeWSHub) targetBranchOverridesPath() string {
	if h != nil && h.targetBranchOverridesFile != "" {
		return h.targetBranchOverridesFile
	}
	return filepath.Join(getContributorsDir(), targetBranchOverridesFileName)
}

func (h *ContributeWSHub) targetBranchOverride(key string) (targetBranchOverride, bool) {
	if h == nil || key == "" {
		return targetBranchOverride{}, false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	ov, ok := h.targetBranchOverrides[key]
	if !ok {
		return targetBranchOverride{}, false
	}
	if time.Since(ov.RecordedAt) >= targetBranchOverrideTTL {
		delete(h.targetBranchOverrides, key)
		return targetBranchOverride{}, false
	}
	return ov, true
}

func (h *ContributeWSHub) recordTargetBranchOverride(key string, ov targetBranchOverride) {
	if h == nil || key == "" || strings.TrimSpace(ov.Branch) == "" {
		return
	}
	if ov.RecordedAt.IsZero() {
		ov.RecordedAt = time.Now()
	}
	h.completedMu.Lock()
	if h.targetBranchOverrides == nil {
		h.targetBranchOverrides = map[string]targetBranchOverride{}
	}
	h.targetBranchOverrides[key] = ov
	h.completedMu.Unlock()
	h.saveTargetBranchOverrides()
}

func (h *ContributeWSHub) loadTargetBranchOverrides() {
	if h == nil || !h.persistTaskLedgers {
		return
	}
	data, err := os.ReadFile(h.targetBranchOverridesPath())
	if err != nil {
		return
	}
	records := map[string]targetBranchOverride{}
	if json.Unmarshal(data, &records) != nil {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	if h.targetBranchOverrides == nil {
		h.targetBranchOverrides = map[string]targetBranchOverride{}
	}
	for k, ov := range records {
		if ov.RecordedAt.IsZero() || time.Since(ov.RecordedAt) >= targetBranchOverrideTTL || strings.TrimSpace(ov.Branch) == "" {
			continue
		}
		h.targetBranchOverrides[k] = ov
	}
}

func (h *ContributeWSHub) saveTargetBranchOverrides() {
	if h == nil || !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]targetBranchOverride, len(h.targetBranchOverrides))
	for k, ov := range h.targetBranchOverrides {
		if time.Since(ov.RecordedAt) >= targetBranchOverrideTTL {
			continue
		}
		saved[k] = ov
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] target-branch overrides marshal failed", "error", err)
		return
	}
	path := h.targetBranchOverridesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] target-branch overrides directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] target-branch overrides write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] target-branch overrides rename failed", "error", err)
	}
}
