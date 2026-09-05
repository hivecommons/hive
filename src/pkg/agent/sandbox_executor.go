package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/sandbox"
)

const (
	DefaultSandboxKickTimeout = 45 * time.Minute
	defaultSandboxNetworkMode = sandbox.NetworkModeRestricted
	sandboxPromptRelPath      = ".hive/kick-prompt.txt"
	sandboxTranscriptRelPath  = ".hive/sandbox-transcript.log"
)

type sandboxCommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type sandboxExecRunner struct{}

func (sandboxExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

type PRCreator interface {
	CreatePR(ctx context.Context, repo, head, base, title, body string) (ghpkg.CreatePRResult, error)
}

type SandboxKickSpec struct {
	Agent        string
	AgentConfig  configSnapshot
	Message      string
	Org          string
	Repo         string
	BaseRef      string
	Branch       string
	WorkspaceDir string
	Image        string
	EnvAllowlist []string
	NetworkMode  string
	Timeout      time.Duration
}

type configSnapshot struct {
	Backend   string
	Model     string
	LaunchCmd string
}

type SandboxKickResult struct {
	Workspace      string
	Branch         string
	BaseSHA        string
	HeadSHA        string
	CommitCount    int
	ReportPath     string
	Report         *outputschema.AgentReport
	TranscriptPath string
	Sandbox        sandbox.Result
	Broker         *pushbroker.Result
	PR             *ghpkg.CreatePRResult
	TimedOut       bool
	Error          string
}

type SandboxExecutor struct {
	Launcher    sandbox.Launcher
	Runner      sandboxCommandRunner
	CloneMinter pushbroker.TokenMinter
	Minter      pushbroker.TokenMinter
	PushEnabled bool
	PRClient    PRCreator
	Logger      *slog.Logger
	Now         func() time.Time
	Mutation    effects.Boundary
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
	if err := writeSandboxPrompt(workspace, spec.Message); err != nil {
		res.Error = err.Error()
		return res, err
	}
	command, err := sandboxCommand(spec.AgentConfig, sandboxPromptRelPath)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	sres, runErr := e.launcher().Run(runCtx, sandbox.LaunchSpec{
		Image:          spec.Image,
		Name:           "hive-" + sanitizeBranchPart(spec.Agent) + "-sandbox",
		Workspace:      workspace,
		WorkspaceMount: sandbox.DefaultWorkspaceMount,
		WorkDir:        sandbox.DefaultWorkspaceMount,
		Command:        command,
		EnvAllowlist:   spec.EnvAllowlist,
		NetworkMode:    spec.NetworkMode,
	})
	// SECURITY (#3940, CWE-532): the agent CLI (notably one supplied via a
	// direct launch_cmd override, which never passes through agent-launch.sh's
	// stderr scrubber) can emit token-shaped values on stdout/stderr — e.g.
	// during an auth failure. Scrub with the canonical logscrub patterns at
	// this single boundary, BEFORE the output is persisted to the transcript
	// or returned in the kick result, so both the normal backend path and the
	// launch_cmd path are covered structurally.
	sres.Stdout = logscrub.ScrubString(sres.Stdout)
	sres.Stderr = logscrub.ScrubString(sres.Stderr)
	res.Sandbox = sres
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		runErr = fmt.Errorf("sandbox kick timed out after %s", spec.Timeout)
	}
	res.TranscriptPath = filepath.Join(workspace, sandboxTranscriptRelPath)
	_ = writeTranscript(res.TranscriptPath, sres)
	res.ReportPath, res.Report = collectAgentReport(workspace)
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
	if count == 0 {
		return res, nil
	}
	if !e.PushEnabled {
		return res, nil
	}
	if e.Minter == nil {
		err := errors.New("sandbox push broker minter is not configured")
		res.Error = err.Error()
		return res, err
	}
	broker := &pushbroker.Broker{
		Workspace: workspace,
		Branch:    spec.Branch,
		BaseRef:   baseSHA,
		Repo:      spec.Org + "/" + spec.Repo,
		Remote:    pushbroker.DefaultRemote,
		Minter:    e.Minter,
		Runner:    e.runner(),
		Logger:    e.Logger,
		Mutation:  e.Mutation,
	}
	bres, brokerErr := broker.Run(ctx)
	res.Broker = &bres
	if brokerErr != nil {
		res.Error = brokerErr.Error()
		return res, brokerErr
	}
	if e.PRClient != nil && bres.Pushed {
		title := fmt.Sprintf("[%s] sandbox changes", spec.Agent)
		body := fmt.Sprintf("Sandboxed credential-free execution result for agent `%s`.\n\nPart of #2804.", spec.Agent)
		pr, prErr := e.PRClient.CreatePR(ctx, spec.Org+"/"+spec.Repo, spec.Branch, spec.BaseRef, title, body)
		res.PR = &pr
		if prErr != nil {
			res.Error = prErr.Error()
			return res, prErr
		}
	}
	return res, nil
}

// prepareWorkspace clones the target repo and checks out the work branch off
// the base ref. When spec.BaseRef is empty it is RESOLVED from the clone's own
// refs/remotes/origin/HEAD and written back into spec, so the caller opens its
// PR against the same ref it branched from (kubestellar/hive#4928). If that
// resolution fails, prepareWorkspace fails the kick rather than silently
// falling back to "main" — a wrong base is worse than a failed kick, and the
// error surfaces through res.Error into the kick result the operator sees.
func (e *SandboxExecutor) prepareWorkspace(ctx context.Context, spec *SandboxKickSpec) (string, string, error) {
	if strings.TrimSpace(spec.Org) == "" || strings.TrimSpace(spec.Repo) == "" {
		return "", "", errors.New("sandbox workspace prep requires org and repo")
	}
	if strings.TrimSpace(spec.WorkspaceDir) == "" {
		return "", "", errors.New("sandbox workspace root is required")
	}
	if err := os.MkdirAll(spec.WorkspaceDir, 0o770); err != nil {
		return "", "", err
	}
	workspace := filepath.Join(spec.WorkspaceDir, sanitizeBranchPart(spec.Agent)+"-"+fmt.Sprint(e.now().UnixNano()))
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", spec.Org, spec.Repo)
	authArgs, cleanupAuth, err := e.cloneAuthArgs(ctx, spec.Org+"/"+spec.Repo, spec.WorkspaceDir)
	if err != nil {
		return "", "", err
	}
	defer cleanupAuth()
	cloneArgs := append(append([]string{}, authArgs...), "clone", "--no-checkout", repoURL, workspace)
	if out, err := e.runner().Run(ctx, "", pushbroker.PushEnv(os.Environ()), "git", cloneArgs...); err != nil {
		return "", "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(spec.BaseRef) == "" {
		ref, err := e.resolveBaseRef(ctx, workspace)
		if err != nil {
			return "", "", fmt.Errorf("sandbox workspace prep: could not resolve %s/%s's default branch and no explicit base ref was given (refusing to guess %q): %w", spec.Org, spec.Repo, FallbackSandboxBaseRef, err)
		}
		spec.BaseRef = ref
	}
	fetchArgs := append(append([]string{}, authArgs...), "fetch", "origin", spec.BaseRef)
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", fetchArgs...); err != nil {
		return "", "", fmt.Errorf("git fetch %s: %w: %s", spec.BaseRef, err, strings.TrimSpace(string(out)))
	}
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "checkout", "-B", spec.Branch, "FETCH_HEAD"); err != nil {
		return "", "", fmt.Errorf("git checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	base, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return workspace, strings.TrimSpace(string(base)), nil
}

// FallbackSandboxBaseRef names the branch resolveBaseRef would have used had
// it not been made an error instead (kubestellar/hive#4928) — kept as a named
// constant purely so the "refusing to guess" error message above never has a
// bare string literal to drift out of sync with intent.
const FallbackSandboxBaseRef = "main"

// resolveBaseRef reports the default branch of the freshly cloned repo. `git
// clone` records the remote's HEAD in refs/remotes/origin/HEAD, so the answer
// is already on disk — no API call, no extra token scope. An unreadable or
// empty answer is returned as an error so the caller can fail the kick rather
// than silently branching off an assumed "main" (kubestellar/hive#4928).
func (e *SandboxExecutor) resolveBaseRef(ctx context.Context, workspace string) (string, error) {
	out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()),
		"git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("reading refs/remotes/origin/HEAD: %w", err)
	}
	ref := strings.TrimSpace(string(out))
	// symbolic-ref --short renders it as "origin/<branch>".
	if _, branch, found := strings.Cut(ref, "/"); found {
		ref = branch
	}
	if ref == "" {
		return "", errors.New("refs/remotes/origin/HEAD resolved to an empty branch name")
	}
	return ref, nil
}

func (e *SandboxExecutor) cloneAuthArgs(ctx context.Context, repo, dir string) ([]string, func(), error) {
	minter := e.CloneMinter
	if minter == nil {
		minter = e.Minter
	}
	if minter == nil {
		return nil, func() {}, nil
	}
	token, err := minter.MintPushToken(ctx, repo)
	if err != nil {
		return nil, func() {}, fmt.Errorf("minting clone token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, func() {}, errors.New("sandbox workspace prep: minter returned empty clone token")
	}
	path := filepath.Join(dir, ".hive-git-credentials-"+sanitizeBranchPart(repo)+"-"+fmt.Sprint(e.now().UnixNano()))
	line := "https://x-access-token:" + token + "@github.com\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		return nil, func() {}, fmt.Errorf("writing sandbox clone credential helper: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	return []string{"-c", "credential.helper=store --file=" + path}, cleanup, nil
}

func (e *SandboxExecutor) commitsSince(ctx context.Context, workspace, baseSHA string) (string, int, error) {
	head, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-parse", "HEAD")
	if err != nil {
		return "", 0, err
	}
	headSHA := strings.TrimSpace(string(head))
	if headSHA == baseSHA {
		return headSHA, 0, nil
	}
	countOut, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-list", "--count", baseSHA+"..HEAD")
	if err != nil {
		return headSHA, 0, err
	}
	var count int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(countOut)), "%d", &count)
	return headSHA, count, nil
}

func sandboxCommand(cfg configSnapshot, promptRel string) ([]string, error) {
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
			cmd += " --model " + shellQuote(CanonicalizeCopilotModel(cfg.Model))
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

func writeSandboxPrompt(workspace, message string) error {
	path := filepath.Join(workspace, sandboxPromptRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(message), 0o660)
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
	return os.WriteFile(path, []byte(b.String()), 0o660)
}

func collectAgentReport(workspace string) (string, *outputschema.AgentReport) {
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
			return path, report
		}
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil {
			return path, nil
		}
	}
	return "", nil
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
	return "hive/" + sanitizeBranchPart(agentName) + "/" + e.now().Format("20060102150405")
}

func sanitizeBranchPart(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
