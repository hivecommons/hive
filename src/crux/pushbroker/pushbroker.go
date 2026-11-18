// Package pushbroker provides trusted Git inspection and credentialed post-steps for sandboxed agents.
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
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/authz"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/gitidentity"
	"github.com/hivecommons/hive/crux/logscrub"
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

type TokenMinter interface {
	MintPushToken(ctx context.Context, repo string) (string, error)
}

type CommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

// Run returns stdout alone on success. Callers treat a successful command's
// output as data — `git show HEAD:<path>` output is committed as file content
// by the normaliser — so exit-0 stderr noise (config warnings, ambiguous-ref
// warnings, traces) must never be mixed into it. On failure stderr is appended
// so error messages still carry git's diagnostics.
func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out = append(out, exitErr.Stderr...)
		}
	}
	return out, err
}

// TokenScoper mints an installation token narrowed to a permission tier and,
// when non-empty, to the named repositories. It is the one method the broker
// needs from the host's GitHub App credential (*github.AppAuth in hive), kept
// as an interface so this package stays inside the Crux ring.
type TokenScoper interface {
	ScopedTokenForRepos(ctx context.Context, tier string, repos []string) (string, error)
}

// GitHubAppMinter mints a per-push token through the host's TokenScoper.
// Auth must be a non-nil interface holding a non-nil implementation; hosts
// building it from a possibly-nil pointer should leave Auth nil instead of
// storing a typed nil (see cmd/hive's appMinter).
type GitHubAppMinter struct {
	Auth TokenScoper
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
	// Shadow, when set, observes the changed-files guard in shadow mode
	// (RFC #18988 phase 0/1): the authz evaluator is compared with this
	// guard's own verdict and an event is recorded. It enforces nothing. Nil
	// uses authz.DefaultShadow(), which is off unless HIVE_AUTHZ_SHADOW is true.
	Shadow *authz.Shadow
	// LeaseSHA, when set, makes the push an update of an EXISTING branch: a
	// hive-opened PR's head that a sandboxed kick checked out
	// (hivecommons/hive#18574). The outgoing commits must descend from it,
	// and the push carries --force-with-lease=refs/heads/<Branch>:<LeaseSHA>,
	// so it lands only while the remote branch still points at the commit
	// the kick started from. Together the two make the push a fast-forward
	// of exactly that commit: it can never drop a commit someone else
	// pushed. Set BaseRef to the same commit so the scans judge only the
	// kick's own commits.
	LeaseSHA string
}

// leaseSHARE is a full object name; anything else is refused before it can
// reach git's argv.
var leaseSHARE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

type Result struct {
	Workspace       string   `json:"workspace"`
	Repo            string   `json:"repo"`
	Branch          string   `json:"branch"`
	Remote          string   `json:"remote"`
	Commit          string   `json:"commit,omitempty"`
	ChangedFiles    []string `json:"changed_files,omitempty"`
	ProtectedReject []string `json:"protected_reject,omitempty"`
	SecretRejected  bool     `json:"secret_rejected"`
	Pushed          bool     `json:"pushed"`
	// Lease is the commit the remote branch had to still point at
	// (Broker.LeaseSHA); empty for a new branch.
	Lease      string    `json:"lease,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func (b *Broker) Run(ctx context.Context) (res Result, err error) {
	res = Result{Workspace: b.Workspace, Repo: b.Repo, Branch: b.Branch, Remote: b.remote(), Lease: strings.TrimSpace(b.LeaseSHA), StartedAt: b.now()}
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
	if res.Lease != "" {
		if err := b.ensureDescendsFromLease(ctx, res.Lease); err != nil {
			return b.fail(res, err)
		}
	}
	if err := b.rejectForgedLaneSignoffs(ctx, baseRef, baseExists); err != nil {
		return b.fail(res, err)
	}

	if err := b.guardChangedFiles(ctx, &res); err != nil {
		return b.fail(res, err)
	}
	// The coding CLI running inside the sandbox writes files with its own
	// tools, not hive's — hive has no writer of its own in this path, so it
	// cannot fix a raw-output defect at the source. It can still normalise
	// the one thing every formatter gate (gofmt, black, prettier, cargo fmt)
	// agrees on before the diff leaves the sandbox: no blank line(s) trailing
	// the final newline (kubestellar/hive#5116). Fixing it here, once, covers
	// every backend and every target-repo language instead of teaching each
	// coding CLI's own formatter to run first.
	amended, err := b.stripTrailingBlankLines(ctx, res.ChangedFiles)
	if err != nil {
		return b.fail(res, fmt.Errorf("normalising trailing newlines: %w", err))
	}
	// The commit message travels inside the pushed objects, out of the
	// egress proxy's reach, so the broker applies the outbound dash rule to
	// it here (hivecommons/hive#19103).
	msgAmended, err := b.sanitizeHeadCommitMessage(ctx)
	if err != nil {
		return b.fail(res, fmt.Errorf("normalising commit message dashes: %w", err))
	}
	if amended || msgAmended {
		commit, err = b.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return b.fail(res, fmt.Errorf("reading HEAD after newline normalisation: %w", err))
		}
		res.Commit = strings.TrimSpace(string(commit))
		if err := b.rejectEmptyOutgoingCommits(ctx, res.Commit); err != nil {
			return b.fail(res, err)
		}
		// The amend rewrote HEAD, so judge the commit that actually leaves.
		if err := b.guardChangedFiles(ctx, &res); err != nil {
			return b.fail(res, err)
		}
	}
	diff, err := b.outgoingDiff(ctx)
	if err != nil {
		return b.fail(res, err)
	}
	if category, found := logscrub.FindSecret(diff); found {
		res.SecretRejected = true
		return b.fail(res, fmt.Errorf("pushbroker: outgoing diff contains a %s-shaped secret", category))
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
		"push", "--no-verify",
	}
	inputs := map[string]string{"commit": res.Commit, "remote": b.remote()}
	if res.Lease != "" {
		args = append(args, "--force-with-lease=refs/heads/"+b.Branch+":"+res.Lease)
		inputs["lease"] = res.Lease
	}
	args = append(args, b.remote(), "HEAD:refs/heads/"+b.Branch)
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
		Inputs: inputs,
	}, func(ctx context.Context) (effects.Result, error) {
		_, runErr := b.runner().Run(ctx, b.Workspace, env, "git", args...)
		return effects.Result{Provenance: res.Commit}, runErr
	})
	if err != nil {
		if res.Lease != "" {
			return b.fail(res, fmt.Errorf("git push to existing branch %q refused or failed; with the lease, a branch that moved off %s since the kick checked it out is never overwritten: %w", b.Branch, shortSHA(res.Lease), err))
		}
		return b.fail(res, fmt.Errorf("git push failed: %w", err))
	}
	res.Pushed = true
	return res, nil
}

func (b *Broker) validate() error {
	if strings.TrimSpace(b.Workspace) == "" || strings.TrimSpace(b.Branch) == "" || strings.TrimSpace(b.Repo) == "" {
		return errors.New("pushbroker: workspace, branch, and repo are required")
	}
	if lease := strings.TrimSpace(b.LeaseSHA); lease != "" && !leaseSHARE.MatchString(lease) {
		return fmt.Errorf("pushbroker: lease %q is not a full commit SHA", lease)
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

// changedFiles lists the paths the outgoing commits touch. Every git call uses
// -z to preserve exact paths: Git's C-quoting of non-ASCII/control bytes, `"` or
// `\` would hide protected prefixes (e.g. `".github/workflows/d\303\251pl.yml"`
// does not match `.github/workflows/`). --no-renames lists moves as deletions
// plus additions, checking both paths even when config enables rename detection.
func (b *Broker) changedFiles(ctx context.Context) ([]string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--no-renames", "--name-only", "-z", base+"...HEAD")
			return splitNUL(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--no-renames", "--name-only", "-z", base+"...HEAD")
		return splitNUL(out), err
	}
	out, err := b.git(ctx, "diff-tree", "--root", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", "HEAD")
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

// ensureDescendsFromLease refuses a push to an existing branch whose
// outgoing commits do not build on the commit the kick checked out: such a
// push would rewrite the PR's history.
func (b *Broker) ensureDescendsFromLease(ctx context.Context, lease string) error {
	if _, err := b.git(ctx, "merge-base", "--is-ancestor", lease, "HEAD"); err != nil {
		return fmt.Errorf("pushbroker: refusing to push to existing branch %q: HEAD does not descend from %s, the head the kick checked out; add commits on top instead of rewriting the PR: %w", b.Branch, shortSHA(lease), err)
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

	// Merge commits (e.g. `git merge origin/<base>` for conflict resolution)
	// are skipped, as the DCO App and Prow's plugin skip them.
	var logArgs []string
	if baseExists {
		rangeSpec := base + "..HEAD"
		logArgs = []string{"log", "--no-merges", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e", rangeSpec}
	} else {
		logArgs = []string{"log", "-1", "--no-merges", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e", "HEAD"}
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
		laneSigned := false
		for _, match := range signedOffByRE.FindAllStringSubmatch(msg, -1) {
			if len(match) == 3 && sameIdentity(strings.TrimSpace(match[1]), strings.TrimSpace(match[2]), laneName, laneEmail) {
				laneSigned = true
				break
			}
		}
		if authorMatches && committerMatches {
			if !laneSigned {
				return fmt.Errorf("pushbroker: refusing to push commit %s authored and committed by %s <%s> without %s's Signed-off-by trailer; use git commit -s or amend with --signoff before pushing", shortSHA(sha), authorName, authorEmail, laneName)
			}
			continue
		}
		if !authorMatches && b.Logger != nil {
			b.Logger.Warn("outgoing commit author differs from pushing lane identity",
				"commit", shortSHA(sha),
				"author", fmt.Sprintf("%s <%s>", authorName, authorEmail),
				"lane", fmt.Sprintf("%s <%s>", laneName, laneEmail))
		}
		if laneSigned {
			return fmt.Errorf("pushbroker: refusing to push commit %s authored by %s <%s> and committed by %s <%s> with %s's Signed-off-by trailer; leave DCO remediation to the author", shortSHA(sha), authorName, authorEmail, committerName, committerEmail, laneName)
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

// outgoingDiff returns the text the secret gate scans. --text and
// --no-textconv keep binary classification (a NUL byte in the first bytes, or a
// committed .gitattributes "-diff"/"binary") from hiding content behind
// "Binary files differ".
func (b *Broker) outgoingDiff(ctx context.Context) (string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--text", "--no-textconv", "--no-ext-diff", base+"...HEAD")
			return string(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--text", "--no-textconv", "--no-ext-diff", base+"...HEAD")
		return string(out), err
	}
	out, err := b.git(ctx, "show", "--text", "--no-textconv", "--format=", "--no-ext-diff", "HEAD")
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
	// only GIT_CONFIG_* git sees and their indices cannot collide. GIT_TRACE*
	// goes too as defence in depth: the runner already keeps stderr out of a
	// successful command's output, but `git show HEAD:<path>` output is
	// committed as file content by the normaliser, so trace output must not
	// get anywhere near it.
	env = slices.DeleteFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") ||
			strings.HasPrefix(entry, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(entry, "GIT_CONFIG_VALUE_") ||
			strings.HasPrefix(entry, "GIT_CONFIG_PARAMETERS=") ||
			strings.HasPrefix(entry, "GIT_TRACE")
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
