// Package pushbroker performs the trusted, credentialed post-step for sandboxed agents.
package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/gitidentity"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	DefaultRemote = "origin"
	DefaultTier   = "contributor"
)

var DefaultProtectedPaths = []string{
	".github/workflows/",
	"bin/gh-wrapper.sh",
	"deploy/bin/gh-wrapper.sh",
	"policies/",
	"hive.yaml.dashboard",
	"OWNERS",
	".github/OWNERS",
}

// pushTokenEnvVar carries the minted push token to git out-of-band (audit F5).
// It is deliberately NOT in credentialEnvPrefixes: that list strips INHERITED
// credentials from the push environment, whereas this value is supplied by the
// broker itself after that filter runs.
const pushTokenEnvVar = "HIVE_PUSH_TOKEN"

// pushCredentialHelper is a git credential helper that echoes the token from
// the environment. Only the variable NAME appears here, so nothing secret
// reaches argv (where it would be world-readable via /proc/<pid>/cmdline) or
// disk. The leading "!" makes git run it through the shell.
const pushCredentialHelper = `!f() { echo username=x-access-token; echo "password=$` + pushTokenEnvVar + `"; }; f`

var credentialEnvPrefixes = []string{
	"GITHUB_TOKEN=", "GH_TOKEN=", "HIVE_GITHUB_TOKEN=", "COPILOT_GITHUB_TOKEN=",
	"GIT_ASKPASS=", "SSH_ASKPASS=",
}

// pushRemoteHost is the only host a sandbox workspace is cloned from
// (SandboxExecutor.prepareWorkspace), and therefore the only host the broker
// will hand a minted token to.
const pushRemoteHost = "github.com"

// The broker runs git INSIDE a workspace the sandboxed agent wrote. The
// sandbox exists so that the agent never holds a credential; the broker is the
// one process that does, and it runs every git command below as the hive's
// own UID, outside the sandbox. Git, however, reads the repository's own
// configuration — .git/config, .git/hooks/, .gitattributes — before it reads
// anything the broker says on the command line, and several of those keys make
// git EXECUTE a program or REDIRECT the push. An agent that writes any of them
// into the workspace turns the broker into (a) arbitrary code execution as the
// hive's UID on the host and (b) exfiltration of the push token to a host of
// the agent's choosing. Two layers close that:
//
//  1. workspaceConfigOverrides are applied to EVERY broker git invocation at
//     command scope (GIT_CONFIG_*), which outranks the repository's own
//     files: hooks are routed to /dev/null, the fsmonitor and credential
//     helper lists are reset, commit signing is off, and the remote the
//     broker pushes to is pinned to the repository it was told to push to,
//     not to whatever .git/config says "origin" is now.
//  2. rejectHostileWorkspaceConfig refuses the push outright when the
//     repository-scoped configuration contains any key outside the small set
//     a fresh clone and an ordinary agent session produce. Keys like
//     url.*.insteadOf, http.proxy / http.sslVerify, filter.*.clean and
//     core.askPass have no override that is safe against every spelling, so
//     the only sound answer to seeing them is to not run git at all.
func workspaceConfigOverrides(remote, repo string) [][2]string {
	pairs := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"commit.gpgsign", "false"},
	}
	if url := expectedRemoteURL(repo); url != "" {
		pairs = append(pairs,
			[2]string{"remote." + remote + ".url", url},
			[2]string{"remote." + remote + ".pushurl", url},
		)
	}
	return pairs
}

// expectedRemoteURL is the URL SandboxExecutor cloned repo from, and the only
// URL the broker will push to. Empty when repo is not an "owner/name" pair.
func expectedRemoteURL(repo string) string {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return "https://" + pushRemoteHost + "/" + owner + "/" + name + ".git"
}

// configEnv renders pairs as the GIT_CONFIG_COUNT/KEY/VALUE triplets git reads
// as command-scope configuration. Only the variable names and the values
// above reach the environment; no secret does.
func configEnv(pairs [][2]string) []string {
	env := make([]string, 0, 1+2*len(pairs))
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]),
		)
	}
	return env
}

// allowedWorkspaceConfigKeys are the exact repository-scoped keys a fresh
// `git clone` writes plus the identity keys an agent CLI commonly sets. Keys
// are compared lowercased, as `git config --list` prints them.
var allowedWorkspaceConfigKeys = map[string]struct{}{
	"core.repositoryformatversion": {},
	"core.filemode":                {},
	"core.bare":                    {},
	"core.logallrefupdates":        {},
	"core.ignorecase":              {},
	"core.precomposeunicode":       {},
	"core.symlinks":                {},
	"core.autocrlf":                {},
	"core.eol":                     {},
	"core.safecrlf":                {},
	"core.hidedotfiles":            {},
	"core.protectntfs":             {},
	"extensions.worktreeconfig":    {},
	"extensions.objectformat":      {},
	"user.name":                    {},
	"user.email":                   {},
	"safe.directory":               {},
	"pull.rebase":                  {},
	"push.default":                 {},
	"push.autosetupremote":         {},
	"init.defaultbranch":           {},
	"fetch.prune":                  {},
}

// allowedWorkspaceConfigPrefixes cover the per-branch tracking keys and the
// purely cosmetic sections, none of which can make git execute or redirect.
var allowedWorkspaceConfigPrefixes = []string{"color.", "advice."}

// rejectHostileWorkspaceConfig reads the repository-scoped (local and
// worktree) configuration of the workspace and refuses the push when it holds
// any key outside the allowlist, or a remote URL for the push remote that is
// not the repository the broker was told to push to.
func (b *Broker) rejectHostileWorkspaceConfig(ctx context.Context) error {
	out, err := b.git(ctx, "config", "--list", "-z", "--show-scope")
	if err != nil {
		return fmt.Errorf("reading workspace git config: %w", err)
	}
	remote := strings.ToLower(b.remote())
	want := expectedRemoteURL(b.Repo)
	var offenders []string
	// The record layout is "<scope>\x00<key>\n<value>\x00", so splitting on
	// NUL yields alternating scope and key/value elements.
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		scope := fields[i]
		key, value, _ := strings.Cut(fields[i+1], "\n")
		if scope != "local" && scope != "worktree" {
			continue
		}
		key = strings.ToLower(key)
		if workspaceConfigKeyAllowed(key, remote) {
			if (key == "remote."+remote+".url" || key == "remote."+remote+".pushurl") && !sameRemoteURL(value, want) {
				offenders = append(offenders, key+"="+value)
			}
			continue
		}
		offenders = append(offenders, key)
	}
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("pushbroker: refusing to run git in a workspace whose repository config sets %s; a sandbox workspace may only carry the keys a fresh clone writes", strings.Join(offenders, ", "))
}

func workspaceConfigKeyAllowed(key, remote string) bool {
	if _, ok := allowedWorkspaceConfigKeys[key]; ok {
		return true
	}
	for _, prefix := range allowedWorkspaceConfigPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case "remote." + remote + ".url", "remote." + remote + ".pushurl", "remote." + remote + ".fetch":
		return true
	}
	if rest, ok := strings.CutPrefix(key, "branch."); ok {
		switch {
		case strings.HasSuffix(rest, ".remote"), strings.HasSuffix(rest, ".merge"), strings.HasSuffix(rest, ".rebase"):
			return true
		}
	}
	return false
}

// sameRemoteURL compares two https remote URLs ignoring case, a trailing
// slash and the optional ".git" suffix.
func sameRemoteURL(got, want string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSpace(u))
		u = strings.TrimSuffix(u, "/")
		u = strings.TrimSuffix(u, ".git")
		return u
	}
	return want != "" && norm(got) == norm(want)
}

type TokenMinter interface {
	MintPushToken(ctx context.Context, repo string) (string, error)
}

type CommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

type GitHubAppMinter struct {
	Auth *ghpkg.AppAuth
	Tier string
}

func (m GitHubAppMinter) MintPushToken(ctx context.Context, repo string) (string, error) {
	if m.Auth == nil {
		return "", errors.New("pushbroker: nil GitHub App auth")
	}
	tier := strings.TrimSpace(m.Tier)
	if tier == "" {
		tier = DefaultTier
	}
	repos := []string(nil)
	if _, name, ok := strings.Cut(strings.TrimSpace(repo), "/"); ok && name != "" {
		repos = []string{name}
	}
	return m.Auth.ScopedTokenForRepos(ctx, tier, repos)
}

type Broker struct {
	Workspace      string
	Branch         string
	BaseRef        string
	Repo           string
	AgentName      string
	Remote         string
	ProtectedPaths []string
	Minter         TokenMinter
	Runner         CommandRunner
	Logger         *slog.Logger
	Now            func() time.Time
	Mutation       effects.Boundary
}

type Result struct {
	Workspace       string    `json:"workspace"`
	Repo            string    `json:"repo"`
	Branch          string    `json:"branch"`
	Remote          string    `json:"remote"`
	Commit          string    `json:"commit,omitempty"`
	ChangedFiles    []string  `json:"changed_files,omitempty"`
	ProtectedReject []string  `json:"protected_reject,omitempty"`
	SecretRejected  bool      `json:"secret_rejected"`
	Pushed          bool      `json:"pushed"`
	Error           string    `json:"error,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
}

func (b *Broker) Run(ctx context.Context) (Result, error) {
	res := Result{Workspace: b.Workspace, Repo: b.Repo, Branch: b.Branch, Remote: b.remote(), StartedAt: b.now()}
	defer func() { res.FinishedAt = b.now() }()
	if err := b.validate(); err != nil {
		res.Error = err.Error()
		return res, err
	}
	if err := b.rejectHostileWorkspaceConfig(ctx); err != nil {
		return b.fail(res, err)
	}
	commit, err := b.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return b.fail(res, fmt.Errorf("reading HEAD: %w", err))
	}
	res.Commit = strings.TrimSpace(string(commit))
	if err := b.rejectEmptyOutgoingCommits(ctx, res.Commit); err != nil {
		return b.fail(res, err)
	}
	baseRef, baseExists := b.pushBase(ctx)
	if remoteRef := b.remoteRef(); remoteRef != baseRef {
		if _, err := b.git(ctx, "rev-parse", "--verify", remoteRef); err == nil {
			if err := b.ensureFastForward(ctx, remoteRef); err != nil {
				return b.fail(res, err)
			}
		}
	} else if baseExists {
		if err := b.ensureFastForward(ctx, baseRef); err != nil {
			return b.fail(res, err)
		}
	}
	if err := b.rejectForgedLaneSignoffs(ctx, baseRef, baseExists); err != nil {
		return b.fail(res, err)
	}

	files, err := b.changedFiles(ctx)
	if err != nil {
		return b.fail(res, err)
	}
	res.ChangedFiles = files
	if len(files) == 0 {
		return b.fail(res, errors.New("pushbroker: no committed changes to push"))
	}
	if rejected := ProtectedPathViolations(files, b.protectedPaths()); len(rejected) > 0 {
		res.ProtectedReject = rejected
		return b.fail(res, fmt.Errorf("pushbroker: protected paths changed: %s", strings.Join(rejected, ", ")))
	}
	// The coding CLI running inside the sandbox writes files with its own
	// tools, not hive's — hive has no writer of its own in this path, so it
	// cannot fix a raw-output defect at the source. It can still normalise
	// the one thing every formatter gate (gofmt, black, prettier, cargo fmt)
	// agrees on before the diff leaves the sandbox: no blank line(s) trailing
	// the final newline (kubestellar/hive#5116). Fixing it here, once, covers
	// every backend and every target-repo language instead of teaching each
	// coding CLI's own formatter to run first.
	if amended, err := b.stripTrailingBlankLines(ctx, files); err != nil {
		return b.fail(res, fmt.Errorf("normalising trailing newlines: %w", err))
	} else if amended {
		commit, err = b.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return b.fail(res, fmt.Errorf("reading HEAD after newline normalisation: %w", err))
		}
		res.Commit = strings.TrimSpace(string(commit))
		if err := b.rejectEmptyOutgoingCommits(ctx, res.Commit); err != nil {
			return b.fail(res, err)
		}
		files, err = b.changedFiles(ctx)
		if err != nil {
			return b.fail(res, err)
		}
		res.ChangedFiles = files
		if len(files) == 0 {
			return b.fail(res, errors.New("pushbroker: no committed changes to push"))
		}
	}
	diff, err := b.outgoingDiff(ctx)
	if err != nil {
		return b.fail(res, err)
	}
	if loc := logscrub.TokenPattern.FindString(diff); loc != "" {
		res.SecretRejected = true
		return b.fail(res, errors.New("pushbroker: outgoing diff contains a token-like secret"))
	}
	token, err := b.Minter.MintPushToken(ctx, b.Repo)
	if err != nil {
		return b.fail(res, fmt.Errorf("minting push token: %w", err))
	}
	if strings.TrimSpace(token) == "" {
		return b.fail(res, errors.New("pushbroker: minter returned empty token"))
	}
	// SECURITY (audit F5, CWE-214): the push token must never appear in git's
	// argv. This used to pass it as `-c http.extraHeader=Authorization: Bearer
	// <token>`, which lands in /proc/<pid>/cmdline — world-readable, so any
	// other UID on the box could read a live push credential while the push
	// ran. Agents run as their own UIDs in this container, so that is a real
	// cross-tenant read, not a theoretical one.
	//
	// Instead pass the token through the environment (visible only to the
	// process owner via /proc/<pid>/environ) and have a credential helper echo
	// it back to git. The helper text itself carries only the variable NAME, so
	// the secret stays out of argv and off disk.
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=" + pushCredentialHelper,
		"push", "--no-verify", b.remote(), "HEAD:refs/heads/" + b.Branch,
	}
	// Append AFTER PushEnv: it strips inherited credential variables, and this
	// one is deliberately supplied rather than inherited. gitEnv also carries
	// the workspace config overrides, so the repository's own credential
	// helpers are reset before the -c helper above is appended, and the
	// remote's URL is the pinned one.
	env := append(b.gitEnv(), pushTokenEnvVar+"="+token)
	_, err = effects.Execute(ctx, b.Mutation, effects.Claim{
		Repo:   b.Repo,
		Kind:   effects.KindBranchPush,
		Target: b.Branch,
		Inputs: map[string]string{"commit": res.Commit, "remote": b.remote()},
	}, func(ctx context.Context) (effects.Result, error) {
		_, runErr := b.runner().Run(ctx, b.Workspace, env, "git", args...)
		return effects.Result{Provenance: res.Commit}, runErr
	})
	if err != nil {
		return b.fail(res, fmt.Errorf("git push failed: %w", err))
	}
	res.Pushed = true
	return res, nil
}

func (b *Broker) validate() error {
	if strings.TrimSpace(b.Workspace) == "" || strings.TrimSpace(b.Branch) == "" || strings.TrimSpace(b.Repo) == "" {
		return errors.New("pushbroker: workspace, branch, and repo are required")
	}
	if b.Minter == nil {
		return errors.New("pushbroker: token minter is required")
	}
	info, err := os.Stat(b.Workspace)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("pushbroker: workspace is not a directory: %w", err)
	}
	gitDir := filepath.Join(b.Workspace, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return fmt.Errorf("pushbroker: workspace is not a git repository")
	}
	return nil
}

// changedFiles lists the paths the outgoing commits touch. Every git call
// uses -z: without it git C-quotes any path containing a non-ASCII byte, a
// control character, `"` or `\` and wraps it in double quotes, and that
// leading quote would defeat the prefix match in ProtectedPathViolations
// (`".github/workflows/d\303\251pl.yml"` is not under `.github/workflows/`).
// NUL-separated output carries the exact on-disk path instead.
func (b *Broker) changedFiles(ctx context.Context) ([]string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--name-only", "-z", base+"...HEAD")
			return splitNUL(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--name-only", "-z", base+"...HEAD")
		return splitNUL(out), err
	}
	out, err := b.git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	return splitNUL(out), err
}

func (b *Broker) pushBase(ctx context.Context) (string, bool) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			return base, true
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		return base, true
	}
	return "", false
}

func (b *Broker) rejectEmptyOutgoingCommits(ctx context.Context, head string) error {
	rangeSpec := "HEAD"
	baseExists := false
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			rangeSpec = base + "..HEAD"
			baseExists = true
		}
	} else {
		base := b.remoteRef()
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			rangeSpec = base + "..HEAD"
			baseExists = true
		}
	}
	args := []string{"rev-list", "--reverse", rangeSpec}
	if !baseExists {
		out := strings.TrimSpace(head)
		if out == "" {
			return nil
		}
		for _, commit := range []string{out} {
			if empty, err := b.commitHasEmptyTreeDelta(ctx, commit); err != nil {
				return err
			} else if empty {
				return fmt.Errorf("pushbroker: refusing to push empty commit %s; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch", shortSHA(commit))
			}
		}
		return nil
	}
	out, err := b.git(ctx, args...)
	if err != nil {
		return fmt.Errorf("reading outgoing commits for empty-commit guard: %w", err)
	}
	for _, commit := range splitLines(out) {
		if empty, err := b.commitHasEmptyTreeDelta(ctx, commit); err != nil {
			return err
		} else if empty {
			return fmt.Errorf("pushbroker: refusing to push empty commit %s; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch", shortSHA(commit))
		}
	}
	return nil
}

func (b *Broker) commitHasEmptyTreeDelta(ctx context.Context, commit string) (bool, error) {
	parentsOut, err := b.git(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return false, fmt.Errorf("reading parents for empty-commit guard: %w", err)
	}
	fields := strings.Fields(string(parentsOut))
	if len(fields) <= 1 {
		_, err = b.runner().Run(ctx, b.Workspace, b.gitEnv(), "git", "diff-tree", "--quiet", "--root", commit)
		return err == nil, nil
	}
	_, err = b.runner().Run(ctx, b.Workspace, b.gitEnv(), "git", "diff-tree", "--quiet", fields[1], commit)
	return err == nil, nil
}

func (b *Broker) ensureFastForward(ctx context.Context, base string) error {
	if _, err := b.git(ctx, "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		return fmt.Errorf("pushbroker: refusing non-fast-forward push to existing branch %q; comment on the PR instead of rewriting history: %w", b.Branch, err)
	}
	return nil
}

var signedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(.*?)\s*<([^<>]+)>\s*$`)

// gitIdentRE parses the `git var GIT_AUTHOR_IDENT` output format:
// "Name <email> <timestamp> <tzoffset>". Only the name/email prefix is used.
var gitIdentRE = regexp.MustCompile(`^(.*?)\s*<([^<>]*)>`)

func (b *Broker) rejectForgedLaneSignoffs(ctx context.Context, base string, baseExists bool) error {
	laneName, laneEmail, err := b.laneGitIdentity(ctx)
	if err != nil {
		return err
	}
	if laneName == "" || laneEmail == "" {
		return nil
	}

	var logArgs []string
	if baseExists {
		rangeSpec := base + "..HEAD"
		logArgs = []string{"log", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e", rangeSpec}
	} else {
		logArgs = []string{"log", "-1", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e", "HEAD"}
	}
	out, err := b.git(ctx, logArgs...)
	if err != nil {
		return fmt.Errorf("reading outgoing commits for sign-off guard: %w", err)
	}
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.Trim(record, "\n")
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x00", 6)
		if len(parts) < 6 {
			continue
		}
		sha := strings.TrimSpace(parts[0])
		authorName, authorEmail := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		committerName, committerEmail := strings.TrimSpace(parts[3]), strings.TrimSpace(parts[4])
		msg := parts[5]
		authorMatches := sameIdentity(authorName, authorEmail, laneName, laneEmail)
		committerMatches := sameIdentity(committerName, committerEmail, laneName, laneEmail)
		if authorMatches && committerMatches {
			continue
		}
		if !authorMatches && b.Logger != nil {
			b.Logger.Warn("outgoing commit author differs from pushing lane identity",
				"commit", shortSHA(sha),
				"author", fmt.Sprintf("%s <%s>", authorName, authorEmail),
				"lane", fmt.Sprintf("%s <%s>", laneName, laneEmail))
		}
		for _, match := range signedOffByRE.FindAllStringSubmatch(msg, -1) {
			if len(match) == 3 && sameIdentity(strings.TrimSpace(match[1]), strings.TrimSpace(match[2]), laneName, laneEmail) {
				return fmt.Errorf("pushbroker: refusing to push commit %s authored by %s <%s> and committed by %s <%s> with %s's Signed-off-by trailer; leave DCO remediation to the author", shortSHA(sha), authorName, authorEmail, committerName, committerEmail, laneName)
			}
		}
	}
	return nil
}

func (b *Broker) laneGitIdentity(ctx context.Context) (name, email string, err error) {
	if agentName := strings.TrimSpace(b.AgentName); agentName != "" {
		name, email, ok := gitidentity.AgentIdentity(agentName)
		if !ok {
			return "", "", nil
		}
		return name, email, nil
	}
	// Compatibility fallback for broker callers that predate AgentName. This
	// still avoids `git config user.*`: `git var` honours GIT_AUTHOR_* when the
	// caller has the lane env and otherwise returns git's effective author.
	identOut, err := b.git(ctx, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		return "", "", fmt.Errorf("reading git author identity for sign-off guard: %w", err)
	}
	name, email, ok := parseGitIdent(string(identOut))
	if !ok {
		return "", "", nil
	}
	return name, email, nil
}

// parseGitIdent extracts the name/email prefix from a `git var
// GIT_AUTHOR_IDENT`-shaped string ("Name <email> timestamp tzoffset"). ok is
// false when the ident does not contain a "<...>" email segment at all (e.g.
// a scripted test failure or truly unset identity), so callers can tell that
// apart from a validly-parsed-but-empty name/email.
func parseGitIdent(ident string) (name, email string, ok bool) {
	match := gitIdentRE.FindStringSubmatch(strings.TrimSpace(ident))
	if match == nil {
		return "", "", false
	}
	return strings.TrimSpace(match[1]), strings.TrimSpace(match[2]), true
}

func sameIdentity(name, email, wantName, wantEmail string) bool {
	return strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(wantName)) &&
		strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(wantEmail))
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// stripTrailingBlankLines removes any blank line(s) trailing the final
// newline of each changed, still-present, textual file, leaving exactly one
// trailing newline. It reports whether it amended HEAD.
//
// Scope is deliberately narrow: a file with no trailing newline at all is
// left untouched (that is a different, less universally-enforced style rule,
// not the "...\n\n" defect #5116 reports), a file already ending in exactly
// one newline is untouched, and a file containing a NUL byte in its first 8KB
// — the same binary heuristic git itself uses — is never rewritten as text.
// Only files still present on disk are considered — a changed file that was
// deleted has nothing to normalise. This deliberately makes no additional git
// call: everything it needs comes from the file list Run() already fetched
// and the file content on disk.
func (b *Broker) stripTrailingBlankLines(ctx context.Context, files []string) (bool, error) {
	var touched []string
	for _, rel := range files {
		abs := filepath.Join(b.Workspace, rel)
		// Lstat, not Stat: a committed symlink is a path the agent chose, and
		// following it would read and rewrite whatever hive-owned file it
		// points at. Only a regular file inside the tree is normalised.
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue // deleted, a directory entry from a rename, or a symlink — nothing to normalise
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", rel, err)
		}
		if looksBinary(data) {
			continue
		}
		normalized, changed := trimTrailingBlankLines(data)
		if !changed {
			continue
		}
		if err := os.WriteFile(abs, normalized, info.Mode().Perm()); err != nil {
			return false, fmt.Errorf("writing %s: %w", rel, err)
		}
		touched = append(touched, rel)
	}
	if len(touched) == 0 {
		return false, nil
	}
	addArgs := append([]string{"add", "--"}, touched...)
	if _, err := b.git(ctx, addArgs...); err != nil {
		return false, err
	}
	if _, err := b.git(ctx, "commit", "--amend", "--no-edit"); err != nil {
		if isEmptyAmendError(err) {
			return false, errors.New("pushbroker: refusing to push a commit made empty by broker normalisation; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch")
		}
		return false, err
	}
	if b.Logger != nil {
		b.Logger.Info("pushbroker normalised trailing blank lines", "repo", b.Repo, "branch", b.Branch, "files", touched)
	}
	return true, nil
}

func isEmptyAmendError(err error) bool {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	return strings.Contains(msg, "would make") && strings.Contains(msg, "it empty")
}

// looksBinary reports whether data appears to be non-text, using the same
// "NUL byte in a leading sample" heuristic git itself applies (see git's
// buffer_is_binary), so this classifies files the same way `git diff` would
// without shelling out to ask it.
func looksBinary(data []byte) bool {
	const sample = 8000
	if len(data) > sample {
		data = data[:sample]
	}
	return bytes.IndexByte(data, 0) != -1
}

// trimTrailingBlankLines collapses one-or-more blank lines at end-of-file
// down to a single trailing newline. It leaves data with no trailing newline
// untouched entirely — that is not the defect being fixed here.
func trimTrailingBlankLines(data []byte) ([]byte, bool) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return data, false
	}
	trimmed := bytes.TrimRight(data, "\n")
	// TrimRight on an all-newline file would strip everything; that is not a
	// realistic agent-authored source file, but guard it anyway rather than
	// emit an empty file.
	want := append(append([]byte(nil), trimmed...), '\n')
	if len(trimmed) == 0 {
		want = []byte("\n")
	}
	if bytes.Equal(want, data) {
		return data, false
	}
	return want, true
}

func (b *Broker) outgoingDiff(ctx context.Context) (string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--no-ext-diff", base+"...HEAD")
			return string(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--no-ext-diff", base+"...HEAD")
		return string(out), err
	}
	out, err := b.git(ctx, "show", "--format=", "--no-ext-diff", "HEAD")
	return string(out), err
}

func (b *Broker) git(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.runner().Run(ctx, b.Workspace, b.gitEnv(), "git", args...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (b *Broker) gitEnv() []string {
	env := PushEnv(os.Environ())
	// Drop any inherited command-scope config so the overrides below are the
	// only GIT_CONFIG_* git sees and their indices cannot collide.
	env = slices.DeleteFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") ||
			strings.HasPrefix(entry, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(entry, "GIT_CONFIG_VALUE_") ||
			strings.HasPrefix(entry, "GIT_CONFIG_PARAMETERS=")
	})
	env = append(env, configEnv(workspaceConfigOverrides(b.remote(), b.Repo))...)
	if name, email, ok := gitidentity.AgentIdentity(strings.TrimSpace(b.AgentName)); ok {
		env = append(env,
			"GIT_AUTHOR_NAME="+name,
			"GIT_AUTHOR_EMAIL="+email,
			"GIT_COMMITTER_NAME="+name,
			"GIT_COMMITTER_EMAIL="+email,
		)
	}
	return env
}

func (b *Broker) fail(res Result, err error) (Result, error) {
	res.Error = err.Error()
	res.FinishedAt = b.now()
	if b.Logger != nil {
		b.Logger.Warn("pushbroker rejected workspace", "repo", res.Repo, "branch", res.Branch, "error", err)
	}
	return res, err
}

func (b *Broker) runner() CommandRunner {
	if b.Runner != nil {
		return b.Runner
	}
	return ExecRunner{}
}
func (b *Broker) remote() string {
	if b.Remote != "" {
		return b.Remote
	}
	return DefaultRemote
}
func (b *Broker) remoteRef() string { return "refs/remotes/" + b.remote() + "/" + b.Branch }
func (b *Broker) protectedPaths() []string {
	if len(b.ProtectedPaths) > 0 {
		return b.ProtectedPaths
	}
	return DefaultProtectedPaths
}
func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func PushEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		blocked := false
		for _, prefix := range credentialEnvPrefixes {
			if strings.HasPrefix(entry, prefix) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, entry)
		}
	}
	return out
}

func ProtectedPathViolations(files, protected []string) []string {
	var rejected []string
	for _, file := range files {
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), "./")
		for _, guard := range protected {
			g := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(guard)), "./")
			if strings.HasSuffix(guard, "/") || strings.HasSuffix(g, "/") {
				g = strings.TrimSuffix(g, "/") + "/"
				if strings.HasPrefix(clean, g) {
					rejected = append(rejected, clean)
					break
				}
				continue
			}
			if clean == g || strings.HasPrefix(clean, strings.TrimSuffix(g, "/")+"/") && strings.HasSuffix(guard, "/") {
				rejected = append(rejected, clean)
				break
			}
		}
	}
	return rejected
}

func splitLines(out []byte) []string {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil
	}
	parts := strings.Split(string(trimmed), "\n")
	files := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			files = append(files, p)
		}
	}
	return files
}

// splitNUL splits -z (NUL-terminated) git output into paths. Paths are kept
// byte-for-byte: a path may legitimately contain spaces or newlines, so only
// the NUL separator and empty records are dropped.
func splitNUL(out []byte) []string {
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) != 0 {
			files = append(files, string(p))
		}
	}
	return files
}
