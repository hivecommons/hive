package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// A sandboxed fix kick can add commits to an existing PR
// (hivecommons/hive#18574).
//
// Without this, a sandboxed kick could only open a NEW PR: a kick that fixed
// the repo side of a red PR came back as a replacement PR, or as a patch in
// the report. Now a kick that names one of the agent's own open PRs gets that
// PR's head branch checked out instead of the base, and after the run the
// push broker adds the kick's commits to it. Every gate fails the kick with
// the reason and pushes nothing:
//
//   - the PR is open, its head branch lives in the kick's repository (not a
//     fork), and the host's PR-authorship record credits it to this agent
//     (the agent_pr_created entry, hivecommons/hive#18575);
//   - the head is still the commit hive checked out, both when GitHub is
//     asked again before the push and in the push itself
//     (--force-with-lease);
//   - the broker's secret, protected-path, sign-off and empty-commit scans
//     pass on the kick's own commits.

// ErrSandboxTargetPRRefused wraps every refusal to work on, or push to, a
// kick's target PR.
var ErrSandboxTargetPRRefused = errors.New("sandbox target PR refused")

// Target sources, in the order sandboxTargetPRFromKickMessage tries them.
const (
	sandboxTargetSourceExplicit    = "explicit"
	sandboxTargetSourceRedPRKick   = "red-pr-kick"
	sandboxTargetSourceCIFailure   = "ci-failure-kick"
	sandboxTargetSourceFixFirst    = "fix-before-new"
	sandboxTargetSourceTopicClaim  = "topic-claim"
	sandboxTargetPromptMarker      = "[hive:sandbox-target-pr]"
	sandboxTargetAgentHeaderPrefix = "[agent:"
)

// The shapes below are hive-written lines of the kick message. They mirror
// what the hive renders (cmd/hive redPRFixKick, pkg/scheduler
// CIFailureKickMessage, the FIX-BEFORE-NEW block and the topic-claims
// block); crux cannot import those packages, so the formats are restated
// here and pinned by tests. A line a PR author could plant in quoted text
// gains nothing: the target is only a candidate, and the executor still
// refuses any PR this agent did not open.
var (
	// The explicit target an operator or planner puts on a kick's first line.
	sandboxTargetExplicitRE = regexp.MustCompile(`^\[hive:target-pr ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)\]$`)
	sandboxRedPRKickRE      = regexp.MustCompile(`^🔴 FIX-BEFORE-NEW — ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+) is red and has not moved$`)
	sandboxCIFailureKickRE  = regexp.MustCompile(`^CI failed on ([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)?)#([0-9]+) \(`)
	sandboxFixFirstRowRE    = regexp.MustCompile(`^  #([0-9]+) ([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)?)(?: \[topic: [^\]]*\])? — `)
	sandboxTopicClaimRowRE  = regexp.MustCompile(`^  [^ ]+ → ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)(?: \(branch [^)]*\))?$`)
)

const (
	sandboxFixFirstHeaderPrefix = "## 🔴 FIX-BEFORE-NEW — your open PRs with failing CI or merge conflicts"
	sandboxTopicClaimsHeader    = "## 📌 Topic claims (one PR per topic)"
	sandboxAdoptedRowPrefix     = "    ADOPTED contributor PR"
	sandboxSectionPrefix        = "## "
)

// sandboxTargetPRFromKickMessage returns the PR in repo (owner/name) a kick
// should add its commits to, and where the kick named it, or 0 when it names
// none. In order: an explicit "[hive:target-pr <repo>#<n>]" first line; the
// targeted red-PR kick's first line; the CI-failure kick's first line; the
// first row of the FIX-BEFORE-NEW block that is not an adopted contributor
// PR; the agent's topic claim when it holds exactly one in repo. A first
// line of "[agent:<name>]" is skipped. A PR in another repository is never a
// target: the workspace clones repo only.
func sandboxTargetPRFromKickMessage(message, repo string) (int, string) {
	lines := strings.Split(strings.TrimLeft(message, " \t\r\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	first := 0
	if strings.HasPrefix(lines[0], sandboxTargetAgentHeaderPrefix) && len(lines) > 1 {
		first = 1
	}
	head := lines[first]
	for _, h := range []struct {
		re     *regexp.Regexp
		source string
	}{
		{sandboxTargetExplicitRE, sandboxTargetSourceExplicit},
		{sandboxRedPRKickRE, sandboxTargetSourceRedPRKick},
		{sandboxCIFailureKickRE, sandboxTargetSourceCIFailure},
	} {
		if m := h.re.FindStringSubmatch(head); m != nil {
			if n := sandboxTargetNumber(m[1], m[2], repo); n > 0 {
				return n, h.source
			}
		}
	}
	if n := sandboxFixFirstTarget(lines, repo); n > 0 {
		return n, sandboxTargetSourceFixFirst
	}
	if n := sandboxTopicClaimTarget(lines, repo); n > 0 {
		return n, sandboxTargetSourceTopicClaim
	}
	return 0, ""
}

func sandboxTargetNumber(targetRepo, num, repo string) int {
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || !sandboxSameRepo(targetRepo, repo) {
		return 0
	}
	return n
}

// sandboxSameRepo matches owner/name whole when both carry an owner, else by
// name (the FIX-BEFORE-NEW block prints whatever governor.repos says).
func sandboxSameRepo(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return a == b
	}
	return a[strings.LastIndex(a, "/")+1:] == b[strings.LastIndex(b, "/")+1:]
}

// sectionLines returns the lines after the header line that starts with
// prefix, up to the next "## " header.
func sectionLines(lines []string, prefix string) []string {
	for i, l := range lines {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], sandboxSectionPrefix) {
				end = j
				break
			}
		}
		return lines[i+1 : end]
	}
	return nil
}

func sandboxFixFirstTarget(lines []string, repo string) int {
	rows := sectionLines(lines, sandboxFixFirstHeaderPrefix)
	for i, l := range rows {
		m := sandboxFixFirstRowRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		// An adopted contributor PR is pushed through the contributor's
		// own branch, which a sandbox push never targets.
		if i+1 < len(rows) && strings.HasPrefix(rows[i+1], sandboxAdoptedRowPrefix) {
			continue
		}
		if n := sandboxTargetNumber(m[2], m[1], repo); n > 0 {
			return n
		}
	}
	return 0
}

func sandboxTopicClaimTarget(lines []string, repo string) int {
	found := 0
	for _, l := range sectionLines(lines, sandboxTopicClaimsHeader) {
		m := sandboxTopicClaimRowRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n := sandboxTargetNumber(m[1], m[2], repo)
		if n == 0 {
			continue
		}
		if found != 0 && found != n {
			// Several topics: the kick does not say which one this
			// run is for, so it opens a new PR as before.
			return 0
		}
		found = n
	}
	return found
}

// sandboxTargetRefusal returns why the kick may not work on t for agent in
// repo, or "" when it may.
func sandboxTargetRefusal(t crux.PRPushTarget, repo, agent string, want int) string {
	switch {
	case t.Number != 0 && t.Number != want:
		return fmt.Sprintf("GitHub returned PR #%d when asked for %s#%d", t.Number, repo, want)
	case !strings.EqualFold(strings.TrimSpace(t.State), "open"):
		state := strings.TrimSpace(t.State)
		if state == "" {
			state = "in an unknown state"
		}
		return fmt.Sprintf("%s#%d is %s, not open", repo, want, state)
	case t.Fork || !strings.EqualFold(strings.TrimSpace(t.HeadRepo), repo):
		where := strings.TrimSpace(t.HeadRepo)
		if where == "" {
			where = "an unknown repository"
		}
		return fmt.Sprintf("%s#%d is a fork PR (head branch in %s); hive pushes only to branches in %s", repo, want, where, repo)
	case strings.TrimSpace(t.AuthorAgent) == "":
		return fmt.Sprintf("%s#%d was not opened by this hive for an agent (no agent_pr_created record credits it)", repo, want)
	case !strings.EqualFold(strings.TrimSpace(t.AuthorAgent), strings.TrimSpace(agent)):
		return fmt.Sprintf("%s#%d was opened by agent %s, not %s; an agent pushes only to its own PRs", repo, want, strings.TrimSpace(t.AuthorAgent), agent)
	case !validSandboxTargetBranch(t.HeadRef):
		return fmt.Sprintf("%s#%d has head branch %q, which hive will not check out", repo, want, t.HeadRef)
	}
	return ""
}

// validSandboxTargetBranch keeps the head ref to a plain branch name before it
// reaches git's argv: no option-looking, relative or ref-syntax names.
func validSandboxTargetBranch(ref string) bool {
	if ref == "" || ref != strings.TrimSpace(ref) || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return false
	}
	for _, r := range ref {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return false
		}
	}
	return true
}

func sandboxTargetRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSandboxTargetPRRefused, fmt.Sprintf(format, args...))
}

// prepareTargetPRCheckout checks out the head branch of spec.TargetPR and
// returns the head commit, which is both the base the run's own commits are
// counted from and the lease the push must find. It reads the PR first and
// refuses one this kick may not push to, before the sandbox spends a run on
// it.
func (e *SandboxExecutor) prepareTargetPRCheckout(ctx context.Context, spec *SandboxKickSpec, workspace string, authArgs []string) (string, error) {
	repo := spec.Org + "/" + spec.Repo
	n := spec.TargetPR
	if e.PRTargetReader == nil {
		return "", sandboxTargetRefused("the hive's PR client cannot read %s#%d, so the kick cannot check that it may push there; nothing was run", repo, n)
	}
	t, err := e.PRTargetReader.PRPushTarget(ctx, repo, n)
	if err != nil {
		return "", sandboxTargetRefused("reading %s#%d: %v", repo, n, err)
	}
	if reason := sandboxTargetRefusal(t, repo, spec.Agent, n); reason != "" {
		return "", sandboxTargetRefused("%s", reason)
	}
	branch := t.HeadRef
	tracking := "refs/remotes/origin/" + branch
	fetchArgs := append(append([]string{}, authArgs...), "fetch", "origin", "+refs/heads/"+branch+":"+tracking)
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", fetchArgs...); err != nil {
		return "", fmt.Errorf("sandbox target PR: could not fetch %s for %s#%d with the clone token: %w: %s", branch, repo, n, err, strings.TrimSpace(string(out)))
	}
	shaOut, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-parse", tracking)
	if err != nil {
		return "", fmt.Errorf("sandbox target PR: git rev-parse %s: %w", tracking, err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if t.HeadSHA != "" && sha != t.HeadSHA {
		return "", sandboxTargetRefused("%s#%d moved while hive checked it out (GitHub reported head %s, the fetch found %s)", repo, n, t.HeadSHA, sha)
	}
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "checkout", "-B", branch, tracking); err != nil {
		return "", fmt.Errorf("sandbox target PR: git checkout %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	spec.Branch = branch
	if base := strings.TrimSpace(t.BaseRef); base != "" {
		spec.BaseRef = base
	}
	spec.targetHead = sha
	spec.targetURL = t.URL
	spec.Message = sandboxTargetPromptSection(repo, n, branch, sha, spec.TargetPRSource) + spec.Message
	return sha, nil
}

// recheckTargetPR asks GitHub again, just before the push, whether the kick
// may still push to its target and whether the head is still the one it
// checked out.
func (e *SandboxExecutor) recheckTargetPR(ctx context.Context, spec SandboxKickSpec) error {
	repo := spec.Org + "/" + spec.Repo
	n := spec.TargetPR
	if e.PRTargetReader == nil {
		return sandboxTargetRefused("the hive's PR client cannot read %s#%d before the push; nothing pushed", repo, n)
	}
	t, err := e.PRTargetReader.PRPushTarget(ctx, repo, n)
	if err != nil {
		return sandboxTargetRefused("re-reading %s#%d before the push: %v; nothing pushed", repo, n, err)
	}
	if reason := sandboxTargetRefusal(t, repo, spec.Agent, n); reason != "" {
		return sandboxTargetRefused("%s; nothing pushed", reason)
	}
	if t.HeadRef != spec.Branch {
		return sandboxTargetRefused("%s#%d's head branch changed from %s to %s during the run; nothing pushed", repo, n, spec.Branch, t.HeadRef)
	}
	if t.HeadSHA != spec.targetHead {
		return sandboxTargetRefused("%s#%d's head moved since checkout (%s, now %s): someone else pushed to it during the run; nothing pushed, and hive never forces over another push", repo, n, shortCommit(spec.targetHead), shortCommit(t.HeadSHA))
	}
	return nil
}

func shortCommit(sha string) string {
	const shortLen = 12
	if len(sha) > shortLen {
		return sha[:shortLen]
	}
	return sha
}

// sandboxTargetPromptSection is the hive-written block prepended to a kick
// whose commits go to an existing PR.
func sandboxTargetPromptSection(repo string, n int, branch, sha, source string) string {
	var b strings.Builder
	b.WriteString(sandboxTargetPromptMarker + "\n")
	fmt.Fprintf(&b, "SANDBOX WORKSPACE ON AN EXISTING PR. Hive checked out %s#%d for you: branch %s at %s", repo, n, branch, sha)
	if source != "" {
		fmt.Fprintf(&b, " (named by this kick's %s)", source)
	}
	b.WriteString(".\n")
	b.WriteString("This sandbox holds no GitHub credential, so `gh pr checkout` and `git push` fail here, and you do not need them. Commit your fix on top of this branch with git commit -s. When the run ends, hive pushes your new commits to the PR.\n")
	b.WriteString("Do not rebase, amend or reset the commits already on the branch: hive pushes only commits that build on the checked-out head, and refuses the push if anyone else pushed in the meantime. Do not open a new PR, and do not write .hive/pr-title.txt or .hive/pr-body.md for this kick.\n")
	b.WriteString("---\n\n")
	return b.String()
}
