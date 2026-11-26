package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/internal/textutil"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/outputschema"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/sandbox"
)

const (
	DefaultSandboxKickTimeout = kubejob.DefaultKickTimeout
	defaultSandboxNetworkMode = sandbox.NetworkModeRestricted
	sandboxPromptRelPath      = ".hive/kick-prompt.txt"
	sandboxTranscriptRelPath  = sandbox.TranscriptRelPath
	// The sandboxed agent may leave the PR title and body it wants in the
	// workspace; the executor (which alone can open the PR) picks them up.
	sandboxPRTitleRelPath = ".hive/pr-title.txt"
	sandboxPRBodyRelPath  = ".hive/pr-body.md"
	sandboxPRTitleMaxLen  = 200
	sandboxPRBodyMaxLen   = 60000
	// sandboxWithheldReasonMax bounds the report summary quoted in a
	// publish-gate refusal, which lands in the run record and the audit log.
	sandboxWithheldReasonMax = 300
)

type sandboxCommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type sandboxExecRunner struct{}

// Run delegates to pushbroker.ExecRunner so the production broker gets the
// same stdout-only-on-success contract the package's own runner has: the
// normaliser commits `git show HEAD:<path>` output as file content, and a
// CombinedOutput runner would amend exit-0 stderr warnings into the blob.
func (sandboxExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	return pushbroker.ExecRunner{}.Run(ctx, dir, env, name, args...)
}

// PRCreator is crux.PRCreator, the host-provided pull-request capability.
type PRCreator = crux.PRCreator

type SandboxKickSpec struct {
	Agent       string
	AgentConfig configSnapshot
	Message     string
	Org         string
	Repo        string
	// ForgeBaseURL is the instance root of the forge Org/Repo lives on; the
	// workspace is cloned from it and the broker pushes only to it. Empty is
	// github.com. The minters must mint tokens for this forge.
	ForgeBaseURL string
	BaseRef      string
	Branch       string
	WorkspaceDir string
	RunKey       string
	Stage        string
	Generation   uint64
	MaxWorktrees int
	Image        string
	Runtime      string
	EnvAllowlist []string
	NetworkMode  string
	Timeout      time.Duration
	// PidsLimit is passed to every launch (agent run and publish.verify);
	// zero keeps sandbox.DefaultPidsLimit (config agent_sandbox.pids_limit).
	PidsLimit int
	// PublishVerify are the deterministic preflight commands run in the
	// sandbox before the run's commits are pushed (config
	// sandbox.publish.verify). Any non-zero exit withholds the push and the PR.
	PublishVerify []string
	// VerifyTimeout bounds each PublishVerify command; zero uses the default.
	VerifyTimeout time.Duration
	// ReviewSpoolDir is where the kick registers its review spool with the
	// relay (crux.SandboxReviewSpoolsDir). Empty disables spooling: the
	// sandbox then has no way to post a review.
	ReviewSpoolDir string
	// ReviewPRs are the PR numbers (in Org/Repo) this kick reviews, taken
	// from the review kick's hive-written header (hivecommons/hive#18513).
	// Non-empty makes the kick review-only: workspace prep checks each PR
	// head out on pr-<n> and writes .hive/pr/<n>.json, and nothing pushes.
	ReviewPRs []int
	// TargetPR is an existing PR (in Org/Repo) whose head branch the kick
	// checks out and adds its commits to instead of opening a new PR
	// (hivecommons/hive#18574). Zero opens a new PR as before. The manager
	// resolves it from the kick message (sandboxTargetPRFromKickMessage);
	// the executor refuses any PR this agent may not push to.
	TargetPR int
	// TargetPRSource says where the kick named TargetPR, for the prompt and
	// the run record.
	TargetPRSource string
	// Environment is the hardware hive requested for a Job-runtime kick,
	// from config (hivecommons/hive#19549). Non-nil prepends the
	// [hive:sandbox-environment] section to the prompt.
	Environment *sandboxEnvironment
	// InferenceRouted marks a Job kick on a model gateway backend whose CLI
	// reaches hive's translator through the inference listener (#19619).
	InferenceRouted bool

	// targetHead and targetURL are set by workspace prep for a TargetPR
	// kick: the head commit checked out (the push's lease) and the PR URL.
	targetHead string
	targetURL  string
}

// reviewOnly reports whether the kick reviews PRs and so must never push.
func (s SandboxKickSpec) reviewOnly() bool { return len(s.ReviewPRs) > 0 }

type configSnapshot struct {
	Backend         string
	Model           string
	LaunchCmd       string
	ReasoningEffort string
}

type SandboxKickResult struct {
	Workspace   string
	Branch      string
	BaseSHA     string
	HeadSHA     string
	CommitCount int
	ReportPath  string
	Report      *outputschema.AgentReport
	// ReportStatus is the agent's own verdict from the report's status field,
	// read leniently so a report that fails schema validation can still say
	// "failed" and keep its commits from being published.
	ReportStatus outputschema.RunStatus
	// PRWithheld names why commits the run left were NOT pushed or turned
	// into a PR (the agent reported failure, or the sandbox exited non-zero).
	// Empty when nothing was withheld.
	PRWithheld string
	// Verify records each publish.verify command that ran, in order. The
	// first failure stops the list and withholds the PR.
	Verify         []SandboxVerifyResult
	TranscriptPath string
	Sandbox        sandbox.Result
	Broker         *pushbroker.Result
	PR             *crux.PRResult
	// PRAttributed is true when the PR client credited a newly opened PR to
	// the kick's agent itself (crux.AgentPRCreator). When it is false and a
	// PR was opened, the manager records the credit through the sandbox audit
	// callback instead (hivecommons/hive#18575).
	PRAttributed bool
	// TargetPR is the existing PR the run's commits were pushed to, or 0.
	TargetPR int
	TimedOut bool
	Error    string
}

// SandboxVerifyResult is one publish.verify command's outcome.
type SandboxVerifyResult struct {
	Command  string
	ExitCode int
	Output   string
	Err      string
	TimedOut bool
}

// Passed reports whether the command ran to completion with exit 0. A
// launcher that returned no error but could not read the exit code back
// (a Complete Job whose pod is already gone) counts as passed too: the
// Job's own success condition is the fact, the missing code is not a failure.
func (v SandboxVerifyResult) Passed() bool {
	if v.Err != "" || v.TimedOut {
		return false
	}
	return v.ExitCode == 0 || v.ExitCode == kubejob.ExitCodeUnknown
}

type SandboxExecutor struct {
	Launcher    sandbox.Launcher
	Runner      sandboxCommandRunner
	CloneMinter pushbroker.TokenMinter
	Minter      pushbroker.TokenMinter
	PushEnabled bool
	PRClient    PRCreator
	// PRReader reads a review target's metadata, diff and comments for the
	// PR file a review kick gets. Nil still checks the PR head out.
	PRReader crux.PRReviewContextReader
	// PRTargetReader reads an existing PR a kick pushes to (spec.TargetPR):
	// before the checkout and again before the push. Nil refuses every
	// TargetPR kick.
	PRTargetReader crux.PRPushTargetReader
	Logger         *slog.Logger
	Now            func() time.Time
	Mutation       effects.Boundary
	// GitHubReadRelay, when set, routes a Job-runtime kick's GitHub reads
	// through hive's read relay (#18865). Nil leaves the Job on its own
	// network path, exactly as before.
	GitHubReadRelay SandboxGitHubReadRelay
	// GitHubReadRelayNoProxy is extra NO_PROXY entries for relayed Jobs.
	GitHubReadRelayNoProxy []string
	// InferenceRelay, when set, points a Job kick with InferenceRouted at
	// hive's inference translator through a per-kick key (#19619).
	InferenceRelay SandboxInferenceRelay
}

func (e *SandboxExecutor) Run(ctx context.Context, spec SandboxKickSpec) (SandboxKickResult, error) {
	var res SandboxKickResult
	if spec.Timeout <= 0 {
		spec.Timeout = DefaultSandboxKickTimeout
	}
	if spec.NetworkMode == "" {
		spec.NetworkMode = defaultSandboxNetworkMode
	}
	if spec.Branch == "" {
		spec.Branch = e.branchName(spec.Agent)
	}
	// prepareWorkspace resolves spec.BaseRef from the clone's own
	// refs/remotes/origin/HEAD when the caller did not pin one, so the PR
	// opened below is based on the repo's own default branch rather than an
	// assumed "main" (kubestellar/hive#4928). It fails the kick outright when
	// the base cannot be determined and none was pinned — a wrong base is
	// worse than a failed kick.
	workspace, baseSHA, err := e.prepareWorkspace(ctx, &spec)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	res.Workspace, res.Branch, res.BaseSHA = workspace, spec.Branch, baseSHA
	if strings.TrimSpace(spec.Stage) != "" {
		source := runStageSourceRepo(spec)
		defer func() {
			mgr := sandbox.WorktreeManager{Runner: func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
				return e.runner().Run(ctx, dir, pushbroker.PushEnv(os.Environ()), name, args...)
			}}
			if err := mgr.Remove(context.Background(), source, workspace); err != nil && e.Logger != nil {
				e.Logger.Warn("sandbox run-stage worktree cleanup failed", "workspace", workspace, "error", err)
			}
		}()
	}
	// Prepended after workspace prep, so it opens the prompt ahead of any
	// review or target-PR section.
	spec.Message = sandboxEnvironmentPromptSection(spec.Environment) + spec.Message
	if err := writeSandboxPrompt(workspace, spec.Message); err != nil {
		res.Error = err.Error()
		return res, err
	}
	command, err := sandboxKickCommand(spec, sandboxPromptRelPath)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	var provider *cline.ProviderFile
	if spec.AgentConfig.Backend == "cline" && spec.Runtime != config.SandboxRuntimeJob {
		provider, err = cline.StageProviderFile(os.Getenv("HIVE_CLINE_CONFIG_DIR"), spec.AgentConfig.Model, workspace)
		if err != nil {
			res.Error = err.Error()
			return res, err
		}
		defer provider.Close()
	}
	// A spool the relay cannot see is a lost review, not a failed kick: the
	// rest of the run is still worth doing, so log and go on without one.
	spool, spoolErr := e.openReviewSpool(spec.ReviewSpoolDir, spec.Agent, workspace, spoolTargetOf(spec))
	if spoolErr != nil && e.Logger != nil {
		e.Logger.Warn("sandbox review spool unavailable; the sandbox cannot post reviews this kick", "agent", spec.Agent, "error", spoolErr)
	}
	defer func() {
		if err := spool.end(e.now()); err != nil && e.Logger != nil {
			e.Logger.Warn("sandbox review spool: could not mark the kick ended", "agent", spec.Agent, "error", err)
		}
	}()
	relayEnv, releaseRelay := e.enrollGitHubReadRelay(spec, workspace)
	defer releaseRelay()
	inferenceEnv, releaseInference := e.enrollInferenceRelay(spec, workspace)
	defer releaseInference()
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	sres, runErr := e.launcher().Run(runCtx, sandbox.LaunchSpec{
		Image:          spec.Image,
		Name:           sandbox.LaunchName(spec.Agent),
		Workspace:      workspace,
		WorkspaceMount: sandbox.DefaultWorkspaceMount,
		WorkDir:        sandbox.DefaultWorkspaceMount,
		Command:        command,
		Env:            mergeSandboxEnv(mergeSandboxEnv(spool.sandboxEnv(), inferenceEnv), relayEnv),
		EnvAllowlist:   spec.EnvAllowlist,
		NetworkMode:    spec.NetworkMode,
		PidsLimit:      spec.PidsLimit,
		ClineProvider:  provider,
		// hive-review and the other sandbox-safe relays come from hive, not
		// the project image (#18512).
		Relays: true,
	})
	if provider != nil {
		sres.Stdout = provider.Redact(sres.Stdout)
		sres.Stderr = provider.Redact(sres.Stderr)
		if err := provider.Close(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	if ctx.Err() == nil && !spool.awaitDrain(ctx) && e.Logger != nil {
		e.Logger.Warn("sandbox review spool: requests still pending after the drain wait", "agent", spec.Agent, "spool", spool.entry.Spool)
	}
	// SECURITY (#3940, CWE-532): the agent CLI (notably one supplied via a
	// direct launch_cmd override, which never passes through agent-launch.sh's
	// stderr scrubber) can emit token-shaped values on stdout/stderr — e.g.
	// during an auth failure. Scrub with the canonical logscrub patterns at
	// this single boundary, BEFORE the output is persisted to the transcript
	// or returned in the kick result. Use typed markers here because this is
	// agent-facing text: the model must know a span was masked, not reason about
	// a generic replacement as if it were repository content. The normal backend
	// path and the launch_cmd path are both covered structurally.
	sres.Stdout = logscrub.ScrubString(sres.Stdout, logscrub.WithMarkers())
	sres.Stderr = logscrub.ScrubString(sres.Stderr, logscrub.WithMarkers())
	res.Sandbox = sres
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		timeoutErr := fmt.Errorf("sandbox kick timed out after %s", spec.Timeout)
		if provider != nil {
			runErr = errors.Join(timeoutErr, runErr)
		} else {
			runErr = timeoutErr
		}
	}
	res.TranscriptPath = filepath.Join(workspace, sandboxTranscriptRelPath)
	_ = writeTranscript(res.TranscriptPath, sres)
	res.ReportPath, res.Report, res.ReportStatus = collectAgentReport(workspace)
	headSHA, count, commitErr := e.commitsSince(ctx, workspace, baseSHA)
	res.HeadSHA, res.CommitCount = headSHA, count
	if runErr != nil {
		res.Error = runErr.Error()
		return res, runErr
	}
	if commitErr != nil {
		res.Error = commitErr.Error()
		return res, commitErr
	}
	// Publish gate: commits alone do not earn a PR. The agent's own verdict
	// (report status failed/blocked) or a non-zero sandbox exit means the
	// work was not proven, and a PR that only fails CI is worse than none.
	if reason := sandboxPublishWithheld(res); reason != "" {
		if count > 0 {
			res.PRWithheld = reason
			err := fmt.Errorf("%w: %s; %d commit(s) not pushed, no PR opened", ErrSandboxRunReportedFailure, reason, count)
			res.Error = err.Error()
			return res, err
		}
		err := fmt.Errorf("%w: %s", ErrSandboxRunReportedFailure, reason)
		res.Error = err.Error()
		return res, err
	}
	// A review kick never pushes (hivecommons/hive#18513), whatever the
	// caller wired: commits a reviewer leaves on pr-<n> are discarded.
	if spec.reviewOnly() {
		if count > 0 {
			res.PRWithheld = fmt.Sprintf("review-only kick: %d commit(s) left on %s were not pushed", count, spec.Branch)
		}
		return res, nil
	}
	if count == 0 {
		return res, nil
	}
	// Deterministic preflight: the report gate above trusts the agent; the
	// policy-declared verify commands do not. They run in the same sandbox
	// image against the run's workspace, and a non-zero exit withholds the
	// push and the PR with the command's tail on the card.
	if reason := e.runPublishVerify(ctx, spec, workspace, baseSHA, &res); reason != "" {
		res.PRWithheld = reason
		err := fmt.Errorf("%w: %s; %d commit(s) not pushed, no PR opened", ErrSandboxRunReportedFailure, reason, count)
		res.Error = err.Error()
		return res, err
	}
	if !e.PushEnabled {
		return res, nil
	}
	if e.Minter == nil {
		err := errors.New("sandbox push broker minter is not configured")
		res.Error = err.Error()
		return res, err
	}
	if spec.TargetPR > 0 {
		// The PR may have closed, changed hands or moved while the sandbox
		// ran: ask again, and push nothing unless it is still this agent's
		// open PR at the head the kick checked out.
		if err := e.recheckTargetPR(ctx, spec); err != nil {
			res.PRWithheld = err.Error()
			res.Error = err.Error()
			return res, err
		}
	}
	broker := &pushbroker.Broker{
		Workspace: workspace,
		Branch:    spec.Branch,
		BaseRef:   baseSHA,
		Repo:      spec.Org + "/" + spec.Repo,
		AgentName: spec.Agent,
		Remote:    pushbroker.DefaultRemote,
		Minter:    e.Minter,
		Runner:    e.runner(),
		Logger:    e.Logger,
		Mutation:  e.Mutation,
		LeaseSHA:  spec.targetHead,
		// The workspace was cloned from this forge; the broker pushes to it
		// and nowhere else.
		RemoteBaseURL: spec.ForgeBaseURL,
	}
	bres, brokerErr := broker.Run(ctx)
	res.Broker = &bres
	if brokerErr != nil {
		res.Error = brokerErr.Error()
		return res, brokerErr
	}
	if spec.TargetPR > 0 {
		// The commits landed on the PR's own branch: there is no PR to
		// open, and the PR's credit was recorded when it was opened.
		res.TargetPR = spec.TargetPR
		res.PR = &crux.PRResult{Number: spec.TargetPR, URL: spec.targetURL, AlreadyExisted: true}
		return res, nil
	}
	if e.PRClient != nil && bres.Pushed {
		title, body := e.sandboxPRText(ctx, workspace, spec.Agent)
		var pr crux.PRResult
		var prErr error
		if creator, ok := e.PRClient.(crux.AgentPRCreator); ok {
			pr, prErr = creator.CreateAgentPR(ctx, spec.Agent, spec.Org+"/"+spec.Repo, spec.Branch, spec.BaseRef, title, body)
			res.PRAttributed = prErr == nil
		} else {
			pr, prErr = e.PRClient.CreatePR(ctx, spec.Org+"/"+spec.Repo, spec.Branch, spec.BaseRef, title, body)
		}
		res.PR = &pr
		if prErr != nil {
			res.Error = prErr.Error()
			return res, prErr
		}
	}
	return res, nil
}

// sandboxPRText resolves the title and body for the PR the executor opens on
// the agent's behalf. Precedence: files the agent wrote into .hive/ in the
// workspace, then the HEAD commit's subject and body, then a generic
// placeholder naming the agent. The Job never holds a GitHub credential, so
// this is the only channel through which the agent's own PR description can
// reach the PR (hivecommons/hive#14691 follow-up).
func (e *SandboxExecutor) sandboxPRText(ctx context.Context, workspace, agent string) (string, string) {
	title := readWorkspaceText(filepath.Join(workspace, sandboxPRTitleRelPath), sandboxPRTitleMaxLen)
	body := readWorkspaceText(filepath.Join(workspace, sandboxPRBodyRelPath), sandboxPRBodyMaxLen)
	if title == "" || body == "" {
		if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "log", "-1", "--format=%s%n%n%b", "HEAD"); err == nil {
			subject, rest, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			if title == "" {
				title = truncateRunes(strings.TrimSpace(subject), sandboxPRTitleMaxLen)
			}
			if body == "" {
				body = truncateRunes(strings.TrimSpace(rest), sandboxPRBodyMaxLen)
			}
		}
	}
	if title == "" {
		title = fmt.Sprintf("[%s] sandbox changes", agent)
	}
	if body == "" {
		body = fmt.Sprintf("Changes produced by hive agent `%s` in a credential-free sandbox run.", agent)
	}
	return title, body
}

// readWorkspaceText returns the trimmed contents of path, bounded to max
// runes, or "" when the file is absent or unreadable. The title file is
// reduced to its first line so a stray paragraph cannot become a PR title.
func readWorkspaceText(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if filepath.Base(path) == filepath.Base(sandboxPRTitleRelPath) {
		text, _, _ = strings.Cut(text, "\n")
		text = strings.TrimSpace(text)
	}
	return truncateRunes(text, max)
}

func truncateRunes(s string, max int) string { return textutil.Runes(s, max) }

func sandboxCommand(cfg configSnapshot, promptRel string) ([]string, error) {
	if cfg.Backend == "cline" {
		return nil, fmt.Errorf("Cline requires scoped provider-file staging or Job Secret references through the canonical sandbox kick path; direct command overrides are unsupported")
	}
	if strings.TrimSpace(cfg.LaunchCmd) != "" {
		return []string{"sh", "-lc", cfg.LaunchCmd + " < " + shellQuote(promptRel)}, nil
	}
	backend := cfg.Backend
	if backend == "" {
		backend = "claude"
	}
	binary := sandboxBackendBinary(backend)
	var cmd string
	switch backend {
	case "goose", "pi":
		cmd = binary + " run -s"
		if cfg.Model != "" {
			cmd += " --model " + shellQuote(cfg.Model)
		}
	case "copilot":
		cmd = binary + " --no-auto-update --allow-all"
		if cfg.Model != "" {
			// Canonicalize separator drift (claude-fable.5 -> claude-fable-5)
			// so a stored bad id never reaches --model (#4262).
			cmd += " --model " + shellQuote(CopilotLaunchModel(cfg.Model))
		}
	default:
		cmd = binary
		if cfg.Model != "" {
			cmd += " --model " + shellQuote(cfg.Model)
		}
		if backend == "claude" || IsInferenceBackend(backend) {
			cmd += " --dangerously-skip-permissions"
		}
	}
	return []string{"sh", "-lc", cmd + " < " + shellQuote(promptRel)}, nil
}

func sandboxBackendBinary(backend string) string {
	switch backend {
	case "copilot":
		return "copilot"
	case "gemini":
		return "gemini"
	case "goose", "pi":
		return "goose"
	case "bob":
		return "bob"
	default:
		return "claude"
	}
}

// writeSandboxPrompt writes the kick prompt confined to the workspace: on a
// review kick the tree is the PR author's, and a .hive symlink in it must not
// redirect the host's write (hivecommons/hive#18513).
func writeSandboxPrompt(workspace, message string) error {
	return writeWorkspaceFileConfined(workspace, sandboxPromptRelPath, []byte(message))
}

func writeTranscript(path string, res sandbox.Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("stdout:\n")
	b.WriteString(res.Stdout)
	b.WriteString("\n\nstderr:\n")
	b.WriteString(res.Stderr)
	// Trailing status line, read back by the dashboard's job-runtime
	// terminal pane once the Job pod is gone (hivecommons/hive#14798).
	b.WriteString("\n\n" + sandbox.TranscriptExitPrefix)
	b.WriteString(strconv.Itoa(res.ExitCode))
	b.WriteString("\n")
	return os.WriteFile(path, []byte(b.String()), 0o660)
}

// ErrSandboxRunReportedFailure wraps every publish-gate refusal so callers
// can tell "the agent said its run failed" from an infrastructure error.
var ErrSandboxRunReportedFailure = errors.New("sandbox run reported failure")

// sandboxPublishWithheld returns why the run's commits must not be published,
// or "" when the gate is open. The order matters for the message an operator
// reads: the agent's own words first, the exit code second.
const (
	// sandboxVerifyTailMax bounds the verify output quoted on the card and in
	// the audit record; the full output lands in the kick log archive.
	sandboxVerifyTailMax = 600
	// sandboxVerifyOutputMax bounds what one verify command's combined output
	// keeps in memory and in the kick log.
	sandboxVerifyOutputMax      = 64 << 10
	defaultSandboxVerifyTimeout = 15 * time.Minute
)

// runPublishVerify runs spec.PublishVerify in order and returns the
// withhold reason for the first failure, or "" when every command passed
// (or none were declared). Results are appended to res.Verify either way.
func (e *SandboxExecutor) runPublishVerify(ctx context.Context, spec SandboxKickSpec, workspace, baseSHA string, res *SandboxKickResult) string {
	if len(spec.PublishVerify) == 0 {
		return ""
	}
	changed := e.changedFiles(ctx, workspace, baseSHA)
	env := map[string]string{
		"HIVE_CHANGED_FILES": strings.Join(changed, "\n"),
		"HIVE_CHANGED_TESTS": strings.Join(changedTestFiles(changed), "\n"),
		"HIVE_BASE_SHA":      baseSHA,
	}
	timeout := spec.VerifyTimeout
	if timeout <= 0 {
		timeout = defaultSandboxVerifyTimeout
	}
	for i, cmd := range spec.PublishVerify {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		vres := SandboxVerifyResult{Command: cmd}
		vctx, cancel := context.WithTimeout(ctx, timeout)
		sres, err := e.launcher().Run(vctx, sandbox.LaunchSpec{
			Image:          spec.Image,
			Name:           "hive-" + sanitizeBranchPart(spec.Agent) + "-verify",
			Workspace:      workspace,
			WorkspaceMount: sandbox.DefaultWorkspaceMount,
			WorkDir:        sandbox.DefaultWorkspaceMount,
			Command:        []string{"sh", "-lc", cmd},
			Env:            env,
			EnvAllowlist:   spec.EnvAllowlist,
			NetworkMode:    spec.NetworkMode,
			PidsLimit:      spec.PidsLimit,
		})
		vres.TimedOut = errors.Is(vctx.Err(), context.DeadlineExceeded)
		cancel()
		out := logscrub.ScrubString(sres.Stdout, logscrub.WithMarkers())
		if sres.Stderr != "" {
			out += "\n" + logscrub.ScrubString(sres.Stderr, logscrub.WithMarkers())
		}
		vres.Output = tailRunes(strings.TrimSpace(out), sandboxVerifyOutputMax)
		vres.ExitCode = sres.ExitCode
		if err != nil {
			vres.Err = err.Error()
		}
		res.Verify = append(res.Verify, vres)
		if vres.Passed() {
			continue
		}
		reason := fmt.Sprintf("verify failed (%d/%d): %s", i+1, len(spec.PublishVerify), cmd)
		switch {
		case vres.TimedOut:
			reason += fmt.Sprintf(" timed out after %s", timeout)
		case vres.Err != "":
			reason += ": " + vres.Err
		default:
			reason += fmt.Sprintf(" (exit %d)", vres.ExitCode)
		}
		if tail := tailRunes(vres.Output, sandboxVerifyTailMax); tail != "" {
			reason += "\n" + tail
		}
		return reason
	}
	return ""
}

// changedFiles lists paths touched by the run's commits (base..HEAD). An
// error yields nil so the verify commands still run, with empty env.
func (e *SandboxExecutor) changedFiles(ctx context.Context, workspace, baseSHA string) []string {
	out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "diff", "--name-only", "--diff-filter=AMR", baseSHA, "HEAD")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// changedTestFiles keeps the pytest/go/js-style test files out of a changed
// list so a verify command can run exactly the tests the run added.
func changedTestFiles(files []string) []string {
	var tests []string
	for _, f := range files {
		base := filepath.Base(f)
		switch {
		case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
			strings.HasSuffix(base, "_test.py"),
			strings.HasSuffix(base, "_test.go"),
			strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.ts"),
			strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.ts"):
			tests = append(tests, f)
		}
	}
	return tests
}

// tailRunes keeps the last max runes of s.
func tailRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}

func sandboxPublishWithheld(res SandboxKickResult) string {
	if res.ReportStatus.PublishBlocked() {
		reason := "agent report status=" + string(res.ReportStatus)
		if res.Report != nil && strings.TrimSpace(res.Report.Summary) != "" {
			reason += ": " + truncateRunes(strings.TrimSpace(res.Report.Summary), sandboxWithheldReasonMax)
		}
		return reason
	}
	// A completed Job whose pod could not be read has no exit code; that is
	// not a failure, and the guessed 0 it used to carry was not a fact either.
	if code := res.Sandbox.ExitCode; code != 0 && code != kubejob.ExitCodeUnknown {
		return fmt.Sprintf("sandbox exited %d", res.Sandbox.ExitCode)
	}
	return ""
}

// collectAgentReport finds the agent's report in the workspace. The third
// value is the report's status: from the validated report when it parses,
// else read straight from the JSON so an agent that wrote
// {"status":"failed"} with an otherwise malformed report still stops the
// publish gate — the one field that must survive a sloppy report is the one
// that says "do not ship this".
func collectAgentReport(workspace string) (string, *outputschema.AgentReport, outputschema.RunStatus) {
	candidates := []string{
		filepath.Join(workspace, ".hive", "agent-report.json"),
		filepath.Join(workspace, "agent-report.json"),
	}
	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, outputschema.AgentReportFilePrefix) && strings.HasSuffix(name, outputschema.AgentReportFileSuffix) {
			candidates = append(candidates, path)
			return filepath.SkipAll
		}
		return nil
	})
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		report, err := outputschema.Validate(data)
		if err == nil {
			return path, report, report.Status
		}
		var raw struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(data, &raw) == nil {
			return path, nil, outputschema.RunStatus(strings.ToLower(strings.TrimSpace(raw.Status)))
		}
	}
	return "", nil, ""
}

func (e *SandboxExecutor) launcher() sandbox.Launcher {
	if e.Launcher != nil {
		return e.Launcher
	}
	return sandbox.PodmanLauncher{}
}

func (e *SandboxExecutor) runner() sandboxCommandRunner {
	if e.Runner != nil {
		return e.Runner
	}
	return sandboxExecRunner{}
}

func (e *SandboxExecutor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *SandboxExecutor) branchName(agentName string) string {
	return sandbox.WorkBranch(agentName, e.now().Format(sandbox.WorkBranchTimeLayout))
}

func sanitizeBranchPart(s string) string { return sandbox.AgentSlug(s) }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
