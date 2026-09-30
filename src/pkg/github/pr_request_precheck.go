package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	defaultPRPrecheckTimeout       = 15 * time.Minute
	defaultPRPrecheckConcurrency   = 1
	defaultPRPrecheckOutputLineCap = 12
	defaultPRPrecheckOutputByteCap = 4096
	defaultPRPrecheckRoot          = "/data/pr-precheck"
)

var prPrecheckGoLimiter = struct {
	mu    sync.Mutex
	inUse int
}{}

type prPrecheckExecFunc func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)
type prPrecheckLookPathFunc func(string) (string, error)

// PRPrecheckOptions configures the PR-request watcher's Tier B/C prechecks.
// The bool/timeout callbacks are read per request so hive.yaml reloads affect
// the next PR without rebuilding the GitHub client.
type PRPrecheckOptions struct {
	DocsEnabled    func() bool
	GoTestsEnabled func() bool
	Timeout        func() time.Duration
	MaxConcurrent  func() int
	CacheDir       string
	CloneBaseURL   string
	WorkRoot       string
	Exec           prPrecheckExecFunc
	LookPath       prPrecheckLookPathFunc
}

type prPrecheckPlan struct {
	docs       bool
	goPackages []string
}

type prPrecheckCommand struct {
	name string
	dir  string
	env  []string
	args []string
	kind string
}

type prPrecheckOutcome struct {
	Reject  []string
	Skipped []string
}

func prPrecheckRequestKey(req PRRequest) string {
	return strings.Join([]string{
		strings.TrimSpace(req.Repo),
		strings.TrimSpace(req.Base),
		strings.TrimSpace(req.Head),
	}, "\x00")
}

func (c *Client) recordPRPrecheckSkipped(req PRRequest, skipped []string) {
	if c == nil {
		return
	}
	c.prPrecheckMu.Lock()
	defer c.prPrecheckMu.Unlock()
	key := prPrecheckRequestKey(req)
	if len(skipped) == 0 {
		delete(c.prPrecheckSkippedBy, key)
		return
	}
	if c.prPrecheckSkippedBy == nil {
		c.prPrecheckSkippedBy = map[string][]string{}
	}
	c.prPrecheckSkippedBy[key] = append([]string(nil), skipped...)
}

func (c *Client) consumePRPrecheckSkipped(req PRRequest) []string {
	if c == nil {
		return nil
	}
	c.prPrecheckMu.Lock()
	defer c.prPrecheckMu.Unlock()
	key := prPrecheckRequestKey(req)
	skipped := append([]string(nil), c.prPrecheckSkippedBy[key]...)
	delete(c.prPrecheckSkippedBy, key)
	return skipped
}

func (c *Client) logPRPrecheckSkip(reason string) {
	if c != nil && c.logger != nil {
		c.logger.Warn("pr-request precheck skipped", slog.String("reason", reason))
		return
	}
	slog.Warn("pr-request precheck skipped", slog.String("reason", reason))
}

// SetPRPrecheckOptions installs or clears the Tier B/C precheck runner.
func (c *Client) SetPRPrecheckOptions(opts *PRPrecheckOptions) {
	if c == nil {
		return
	}
	c.prPrecheckMu.Lock()
	defer c.prPrecheckMu.Unlock()
	if opts == nil {
		c.prPrecheckOptions = nil
		c.prPrecheckSkippedBy = nil
		return
	}
	copy := *opts
	c.prPrecheckOptions = &copy
}

func (c *Client) prPrecheckOptionsSnapshot() *PRPrecheckOptions {
	if c == nil {
		return nil
	}
	c.prPrecheckMu.RLock()
	defer c.prPrecheckMu.RUnlock()
	if c.prPrecheckOptions == nil {
		return nil
	}
	copy := *c.prPrecheckOptions
	return &copy
}

func (o *PRPrecheckOptions) docsEnabled() bool {
	return o != nil && (o.DocsEnabled == nil || o.DocsEnabled())
}

func (o *PRPrecheckOptions) goTestsEnabled() bool {
	return o != nil && (o.GoTestsEnabled == nil || o.GoTestsEnabled())
}

func (o *PRPrecheckOptions) timeout() time.Duration {
	if o != nil && o.Timeout != nil {
		if d := o.Timeout(); d > 0 {
			return d
		}
	}
	return defaultPRPrecheckTimeout
}

func (o *PRPrecheckOptions) maxConcurrent() int {
	if o != nil && o.MaxConcurrent != nil {
		if n := o.MaxConcurrent(); n > 0 {
			return n
		}
	}
	return defaultPRPrecheckConcurrency
}

func (o *PRPrecheckOptions) cacheDir() string {
	if o != nil && strings.TrimSpace(o.CacheDir) != "" {
		return o.CacheDir
	}
	return filepath.Join(defaultPRPrecheckRoot, "gocache")
}

func (o *PRPrecheckOptions) execFunc() prPrecheckExecFunc {
	if o != nil && o.Exec != nil {
		return o.Exec
	}
	return runPRPrecheckCommand
}

func (o *PRPrecheckOptions) lookPathFunc() prPrecheckLookPathFunc {
	if o != nil && o.LookPath != nil {
		return o.LookPath
	}
	return exec.LookPath
}

func runPRPrecheckCommand(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func planPRPrechecks(files []*gh.CommitFile, citedFiles map[string]bool) prPrecheckPlan {
	pkgSet := map[string]bool{}
	plan := prPrecheckPlan{}
	for _, file := range files {
		if file == nil {
			continue
		}
		name := strings.TrimSpace(filepath.ToSlash(file.GetFilename()))
		if name == "" || file.GetStatus() == "removed" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), ".md") || citedFiles[name] {
			plan.docs = true
		}
		if strings.HasSuffix(name, ".go") && strings.HasPrefix(name, "src/") {
			dir := path.Dir(strings.TrimPrefix(name, "src/"))
			if dir != "." && dir != "" {
				pkgSet["./"+dir] = true
			}
		}
	}
	for pkg := range pkgSet {
		plan.goPackages = append(plan.goPackages, pkg)
	}
	sort.Strings(plan.goPackages)
	return plan
}

func (c *Client) runPRRequestExternalPrechecks(ctx context.Context, owner, repo, head string, files []*gh.CommitFile) prPrecheckOutcome {
	opts := c.prPrecheckOptionsSnapshot()
	if opts == nil {
		return prPrecheckOutcome{}
	}
	if !opts.docsEnabled() && !opts.goTestsEnabled() {
		return prPrecheckOutcome{}
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	checkout, cleanup, err := c.checkoutPRHeadForPrecheck(runCtx, opts, owner, repo, head)
	if err != nil {
		reason := fmt.Sprintf("checkout skipped (%v)", err)
		c.logPRPrecheckSkip(reason)
		return prPrecheckOutcome{Skipped: []string{reason}}
	}
	defer cleanup()

	cited := map[string]bool{}
	if opts.docsEnabled() {
		cited = citedFilesFromDocs(filepath.Join(checkout, "src", "docs"))
	}
	plan := planPRPrechecks(files, cited)

	var outcome prPrecheckOutcome
	var commands []prPrecheckCommand
	if opts.docsEnabled() && plan.docs {
		lookPath := opts.lookPathFunc()
		var docsSkipped []string
		if _, err := lookPath("python3"); err != nil {
			docsSkipped = append(docsSkipped, "docs guards skipped (python3 not found)")
		}
		if _, err := lookPath("bash"); err != nil {
			docsSkipped = append(docsSkipped, "docs guards skipped (bash not found)")
		}
		if len(docsSkipped) == 0 {
			commands = append(commands,
				prPrecheckCommand{name: "python3", kind: "docs", dir: checkout, args: []string{"src/scripts/check-docs-links.py", "src/docs"}},
				prPrecheckCommand{name: "python3", kind: "docs", dir: checkout, args: []string{"src/scripts/check-docs-links.py", "docs"}},
				prPrecheckCommand{name: "python3", kind: "docs", dir: checkout, args: []string{"src/scripts/check-docs-links.py", ".", "--no-recurse"}},
				prPrecheckCommand{name: "python3", kind: "docs", dir: checkout, args: []string{"src/scripts/check-docs-links.py", "src/deploy/data/wiki", "--vault-root"}},
				prPrecheckCommand{name: "python3", kind: "docs", dir: checkout, args: []string{"src/scripts/check-docs-citations.py", "src/docs"}},
				prPrecheckCommand{name: "bash", kind: "docs", dir: checkout, args: []string{"src/scripts/check-api-reference-citations.sh"}},
			)
		}
		for _, skipped := range docsSkipped {
			c.logPRPrecheckSkip(skipped)
			outcome.Skipped = append(outcome.Skipped, skipped)
		}
	}
	if opts.goTestsEnabled() && len(plan.goPackages) > 0 {
		release, ok := acquirePRPrecheckGoSlot(runCtx, opts.maxConcurrent())
		if !ok {
			reason := fmt.Sprintf("go tests skipped (timed out waiting for precheck slot after %s)", opts.timeout())
			c.logPRPrecheckSkip(reason)
			outcome.Skipped = append(outcome.Skipped, reason)
		} else if goEnv, raceSkipped, ok := goPrecheckEnv(opts); !ok {
			defer release()
			reason := "go tests skipped (go not found)"
			c.logPRPrecheckSkip(reason)
			outcome.Skipped = append(outcome.Skipped, reason)
		} else {
			defer release()
			if err := os.MkdirAll(opts.cacheDir(), 0o700); err != nil {
				reason := fmt.Sprintf("go cache skipped (%v)", err)
				c.logPRPrecheckSkip(reason)
				outcome.Skipped = append(outcome.Skipped, reason)
			}
			if raceSkipped != "" {
				c.logPRPrecheckSkip(raceSkipped)
				outcome.Skipped = append(outcome.Skipped, raceSkipped)
			}
			for _, pkg := range plan.goPackages {
				commands = append(commands, goPrecheckCommand(checkout, pkg, nil, goEnv, raceSkipped == ""))
			}
			commands = append(commands,
				goPrecheckCommand(checkout, "./pkg/dashboard/webstatic", nil, goEnv, raceSkipped == ""),
				goPrecheckCommand(checkout, "./internal/testutil", nil, goEnv, raceSkipped == ""),
				goPrecheckCommand(checkout, "./pkg/dashboard", []string{"TestOpenAPISpecCoversEveryRegisteredRoute", "TestOpenAPISchemaFieldsExistOnGoTypes", "TestOpenAPIDeclaredTypeMatchesEmittedJSON"}, goEnv, raceSkipped == ""),
			)
		}
	}

	execFn := opts.execFunc()
	for _, command := range commands {
		if runCtx.Err() != nil {
			reason := fmt.Sprintf("%s skipped (timed out after %s)", command.kind, opts.timeout())
			c.logPRPrecheckSkip(reason)
			outcome.Skipped = append(outcome.Skipped, reason)
			break
		}
		output, err := execFn(runCtx, command.dir, command.env, command.name, command.args...)
		if err != nil {
			if reason, infra := classifyPRPrecheckInfraFailure(command, output, err, runCtx.Err()); infra {
				c.logPRPrecheckSkip(reason)
				outcome.Skipped = append(outcome.Skipped, reason)
				continue
			}
			outcome.Reject = append(outcome.Reject, formatPRPrecheckCommandFailure(command, output, err))
		}
	}
	return outcome
}

func goPrecheckCommand(checkout, pkg string, tests []string, env []string, race bool) prPrecheckCommand {
	args := []string{"test"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "-count=1")
	if len(tests) > 0 {
		args = append(args, "-run", "^("+strings.Join(tests, "|")+")$")
	}
	args = append(args, pkg)
	return prPrecheckCommand{name: "go", kind: "go tests", dir: filepath.Join(checkout, "src"), env: env, args: args}
}

func acquirePRPrecheckGoSlot(ctx context.Context, max int) (func(), bool) {
	if max <= 0 {
		max = defaultPRPrecheckConcurrency
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		prPrecheckGoLimiter.mu.Lock()
		if prPrecheckGoLimiter.inUse < max {
			prPrecheckGoLimiter.inUse++
			prPrecheckGoLimiter.mu.Unlock()
			return func() {
				prPrecheckGoLimiter.mu.Lock()
				if prPrecheckGoLimiter.inUse > 0 {
					prPrecheckGoLimiter.inUse--
				}
				prPrecheckGoLimiter.mu.Unlock()
			}, true
		}
		prPrecheckGoLimiter.mu.Unlock()
		select {
		case <-ctx.Done():
			return func() {}, false
		case <-ticker.C:
		}
	}
}

func goPrecheckEnv(opts *PRPrecheckOptions) (env []string, raceSkipped string, ok bool) {
	lookPath := opts.lookPathFunc()
	if _, err := lookPath("go"); err != nil {
		return nil, "", false
	}
	cacheDir := opts.cacheDir()
	env = []string{
		"GOCACHE=" + filepath.Join(cacheDir, "build"),
		"GOMODCACHE=" + filepath.Join(cacheDir, "mod"),
	}
	for _, compiler := range []string{"gcc", "cc"} {
		if _, err := lookPath(compiler); err == nil {
			return append(env, "CGO_ENABLED=1"), "", true
		}
	}
	return append(env, "CGO_ENABLED=0"), "go tests skipped (race detector unavailable (no C compiler); running non-race go test)", true
}

func classifyPRPrecheckInfraFailure(command prPrecheckCommand, output string, err error, ctxErr error) (string, bool) {
	if ctxErr != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Sprintf("%s skipped (timed out or cancelled: %v)", command.kind, firstNonEmpty(ctxErr, err)), true
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Sprintf("%s skipped (tool %s not found)", command.kind, command.name), true
	}
	lower := strings.ToLower(output + "\n" + err.Error())
	infraNeedles := []string{
		"c compiler", "gcc\" not found", "exec: \"gcc\"", "go: downloading",
		"i/o timeout", "tls handshake timeout", "connection reset", "connection refused",
		"no such host", "proxyconnect", "dial tcp", "temporary failure in name resolution",
		"permission denied",
	}
	for _, needle := range infraNeedles {
		if strings.Contains(lower, needle) {
			return fmt.Sprintf("%s skipped (%s)", command.kind, firstOutputLines(output, 3)), true
		}
	}
	if command.kind == "docs" {
		return "", false
	}
	if isGenuineGoFailure(output) {
		return "", false
	}
	return fmt.Sprintf("%s skipped (inconclusive tool failure: %s)", command.kind, firstNonEmptyString(firstOutputLines(output, 3), err.Error())), true
}

func isGenuineGoFailure(output string) bool {
	return strings.Contains(output, "\n--- FAIL:") ||
		strings.HasPrefix(output, "--- FAIL:") ||
		strings.Contains(output, "[build failed]") ||
		regexp.MustCompile(`(?m)^# github\.com/hivecommons/hive/`).MatchString(output) ||
		regexp.MustCompile(`(?m)^FAIL\tgithub\.com/hivecommons/hive/`).MatchString(output)
}

func firstNonEmpty(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (c *Client) checkoutPRHeadForPrecheck(ctx context.Context, opts *PRPrecheckOptions, owner, repo, head string) (string, func(), error) {
	root := filepath.Join(defaultPRPrecheckRoot, "checkouts")
	if opts != nil && strings.TrimSpace(opts.WorkRoot) != "" {
		root = opts.WorkRoot
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, err
	}
	dir, err := os.MkdirTemp(root, "head-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	cloneURL := prPrecheckCloneURL(opts, owner, repo)
	execFn := opts.execFunc()
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if header, ok := c.prPrecheckAuthHeader(ctx); ok {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+strings.TrimRight(cloneURL, "/")+".extraHeader",
			"GIT_CONFIG_VALUE_0="+header,
		)
	}
	out, err := execFn(ctx, root, env, "git", "clone", "--depth=1", "--branch", head, "--single-branch", cloneURL, dir)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git clone %s@%s: %w: %s", owner+"/"+repo, head, err, firstOutputLines(out, 3))
	}
	return dir, cleanup, nil
}

func (c *Client) prPrecheckAuthHeader(ctx context.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	if c.appAuth != nil {
		token, err := c.appAuth.Token(ctx)
		if err == nil && token != "" {
			return "Authorization: Bearer " + token, true
		}
		return "", false
	}
	if c.authToken != "" {
		return "Authorization: Bearer " + c.authToken, true
	}
	return "", false
}

func prPrecheckCloneURL(opts *PRPrecheckOptions, owner, repo string) string {
	base := "https://github.com"
	if opts != nil && strings.TrimSpace(opts.CloneBaseURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(opts.CloneBaseURL), "/")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://github.com/" + owner + "/" + repo + ".git"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + owner + "/" + repo + ".git"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func citedFilesFromDocs(docsDir string) map[string]bool {
	result := map[string]bool{}
	_ = filepath.WalkDir(docsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, cited := range repoLocalCitations(string(data)) {
			result[cited] = true
		}
		return nil
	})
	return result
}

var repoLocalCitationRE = regexp.MustCompile(`(?:^|[^\w./@:-])((?:\.github/|src/|docs/|bin/|changelog\.d/|[A-Za-z0-9_./-]+/)[A-Za-z0-9_.-]+\.(?:go|sh|py|md|yaml|yml|json|toml|mod|sum|ts|tsx|js|jsx|css|html|service|container|timer|conf|env|txt)):\d+(?:[-–]\d+)?(?:,\d+(?:[-–]\d+)?)*`)

func repoLocalCitations(text string) []string {
	matches := repoLocalCitationRE.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		cited := filepath.ToSlash(match[1])
		for _, candidate := range normalizedCitedPaths(cited) {
			if !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	sort.Strings(out)
	return out
}

func normalizedCitedPaths(cited string) []string {
	switch {
	case strings.HasPrefix(cited, "src/"), strings.HasPrefix(cited, ".github/"), strings.HasPrefix(cited, "changelog.d/"):
		return []string{cited}
	case strings.HasPrefix(cited, "pkg/"), strings.HasPrefix(cited, "cmd/"), strings.HasPrefix(cited, "internal/"),
		strings.HasPrefix(cited, "scripts/"), strings.HasPrefix(cited, "deploy/"), strings.HasPrefix(cited, "docs/"),
		strings.HasPrefix(cited, "test/"), strings.HasPrefix(cited, "bin/"):
		return []string{"src/" + cited}
	default:
		return nil
	}
}

func formatPRPrecheckCommandFailure(command prPrecheckCommand, output string, err error) string {
	label := command.name
	if len(command.args) > 0 {
		label += " " + strings.Join(command.args, " ")
	}
	summary := summarizePrecheckOutput(output)
	if summary == "" {
		summary = err.Error()
	}
	return fmt.Sprintf("%s failed: %s", label, summary)
}

func summarizePrecheckOutput(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var picked []string
	failRE := regexp.MustCompile(`^(--- FAIL:|FAIL\b|panic:|fatal error:|DRIFT\b|::error::|Error:|error:)`)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "===") {
			continue
		}
		if failRE.MatchString(trimmed) {
			picked = append(picked, trimmed)
		}
		if len(picked) >= defaultPRPrecheckOutputLineCap {
			break
		}
	}
	if len(picked) == 0 {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "ok ") || strings.HasPrefix(trimmed, "? ") {
				continue
			}
			picked = append(picked, trimmed)
			if len(picked) >= defaultPRPrecheckOutputLineCap {
				break
			}
		}
	}
	return truncatePrecheckSummary(strings.Join(picked, "\n"))
}

func firstOutputLines(output string, n int) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var picked []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		picked = append(picked, trimmed)
		if len(picked) >= n {
			break
		}
	}
	return strings.Join(picked, "\n")
}

func truncatePrecheckSummary(s string) string {
	if len(s) <= defaultPRPrecheckOutputByteCap {
		return s
	}
	return s[:defaultPRPrecheckOutputByteCap] + "\n…"
}
