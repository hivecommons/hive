package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prosefilter"
)

// The forge request relay (hivecommons/hive#19670, #19494 item 5).
//
// Agents never write to a forge directly: they drop a request file and a hive
// relay performs the write. On a GitHub hive those relays are methods of
// *github.Client. A forge-only hive has no GitHub client, so they never start
// and every `hive-open-pr` / `hive-issue` request waits forever. This relay
// serves the same request directories on such a hive, through pkg/forge.
//
// It is deliberately narrower than the GitHub relays and fails closed where
// a GitHub gate cannot be reproduced:
//
//   - Every request is authorized with the manager's own gate (the file's
//     owning UID must be the claimed agent, and the agent must be allowed the
//     write at the hive's ACMM level). A nil authorizer denies everything.
//   - The repo must be one of project.repos, off GitHub, not paused, and in
//     the agent's scope.
//   - A pull request must be allowed by the repo's open-PRs mode
//     (repo_policies[].open_prs, repo_open_prs.go), as on GitHub.
//   - Every pull request opened here carries the ready-for-review gate label,
//     so a human releases it. The GitHub relay's self-authorization,
//     reporter-trust and duplicate-claim checks read GitHub data and are not
//     ported; the label holds the PR in their place.
//   - Issue kinds served: issue, comment and label. No request may add or
//     remove a hive-controlled label (the hold labels and
//     github.ReservedLabelForAgents). Other kinds are rejected with a reason
//     so the agent does not wait on them.
//   - A forge 4xx (other than 408 and 429) is final; other failures are
//     retried a bounded number of times.
//   - Push-branch requests are served, and review and merge requests are
//     answered with a reason (forge_push_relay.go, #19916).
//
// Agent-written titles and bodies pass through prosefilter before they are
// sent, as on the GitHub relays.
//
// Results are written next to the request as <name>.result.json with the
// GitHub relays' response shapes, and a consumed request is removed (success)
// or renamed .denied / .rejected / .bad / .failed, as on GitHub.

const (
	// forgeRelayPollInterval matches the GitHub PR relay's cadence.
	forgeRelayPollInterval = 10 * time.Second
	// forgeRelayMaxAttempts bounds retries of a transient forge failure
	// before the request is quarantined as .failed.
	forgeRelayMaxAttempts = 5
)

// forgeRelayWriter is what the relay needs from a forge adapter.
type forgeRelayWriter interface {
	forge.IssueWriter
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
	forge.IssueCreator
	forge.ChangeRequestCreator
}

// forgeRelayWriterForRepo builds the writer for a repo on a non-GitHub forge,
// with the same URL, token and token-host rules as forgeListerForRepo. It is
// a variable so tests can substitute a fake.
var forgeRelayWriterForRepo = func(cfg *config.Config, repo string) (forgeRelayWriter, error) {
	l, err := forgeListerForRepo(cfg, repo)
	if err != nil {
		return nil, err
	}
	w, ok := l.(forgeRelayWriter)
	if !ok {
		return nil, fmt.Errorf("the %s adapter cannot create issues or pull requests", cfg.ForgeKindForRepo(repo))
	}
	return w, nil
}

type forgeRequestRelay struct {
	cfg        func() *config.Config
	prAuthz    github.PRRequestAuthorizer
	issueAuthz github.IssueRequestAuthorizer
	prDir      string
	issueDir   string
	now        func() time.Time
	logger     *slog.Logger
	attempts   map[string]int
	// openPRs is the per-repo open-PRs gate (repo_open_prs.go): the repo's
	// mode and, when the PR may not open, why.
	openPRs func(agent, repo string, issues []int, via string) (string, string)

	// The push-branch, review and merge queues (forge_push_relay.go, #19916).
	pushAuthz   github.PushBranchRequestAuthorizer
	pushRemote  pushbroker.ForgeRemoteResolver
	pushAudit   func(github.AuditRecord)
	pushDir     string
	reviewDir   string
	mergeDir    string
	queuesReady bool

	// The clone queue (forge_clone_relay.go, #19997).
	cloneAuthz forgeCloneAuthorizer
	cloneDir   string
}

func newForgeRequestRelay(cfg func() *config.Config, prAuthz github.PRRequestAuthorizer, issueAuthz github.IssueRequestAuthorizer, logger *slog.Logger) *forgeRequestRelay {
	return &forgeRequestRelay{
		cfg:        cfg,
		prAuthz:    prAuthz,
		issueAuthz: issueAuthz,
		prDir:      github.PRRequestDir,
		issueDir:   github.IssueRequestDir,
		now:        time.Now,
		logger:     logger,
		attempts:   map[string]int{},
		openPRs:    hiveOpenPRsGate(cfg, logger),
		pushRemote: forgeRemoteResolver(cfg),
		pushDir:    github.PushBranchRequestDir,
		reviewDir:  github.ReviewRequestDir,
		mergeDir:   github.MergeRequestDir,
		cloneDir:   forgeCloneRequestDir,
	}
}

// startForgeRequestRelay runs the relay until ctx is done. The returned
// channel closes when the loop has exited.
func startForgeRequestRelay(ctx context.Context, r *forgeRequestRelay) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(forgeRelayPollInterval)
		defer t.Stop()
		r.logger.Debug("forge request relay started; idle unless every repo is off GitHub", "pr_dir", r.prDir, "issue_dir", r.issueDir)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				r.scanOnce(ctx)
			}
		}
	}()
	return done
}

// scanOnce handles every pending request in every queue once.
//
// It does nothing unless every repo is off GitHub, read on each scan, so a
// reload that puts a repo on GitHub hands the queues back to the GitHub
// relays instead of this relay refusing their requests.
func (r *forgeRequestRelay) scanOnce(ctx context.Context) {
	if cfg := r.cfg(); cfg == nil || !cfg.AllReposOffGitHub() {
		return
	}
	// Pushes first: an agent that drops a push and then a PR for the same
	// branch in one tick gets the branch published before the PR is opened.
	r.ensureQueueDirs()
	for _, path := range pendingRequestFiles(r.pushDir) {
		r.handlePushBranch(ctx, path)
	}
	r.serveCloneQueue(ctx)
	for _, path := range pendingRequestFiles(r.prDir) {
		r.handlePR(ctx, path)
	}
	for _, path := range pendingRequestFiles(r.issueDir) {
		r.handleIssue(ctx, path)
	}
	for _, path := range pendingRequestFiles(r.reviewDir) {
		r.handleReviewRequest(path)
	}
	for _, path := range pendingRequestFiles(r.mergeDir) {
		r.handleMergeRequest(path)
	}
}

func pendingRequestFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// readRequest reads and decodes a request file. ok=false means the caller
// stops: the file vanished, was unsafe (quarantined), is a torn write (left
// for the next scan), or was malformed (quarantined with a result).
func (r *forgeRequestRelay) readRequest(path string, into any, writeBad func(string)) bool {
	data, err := github.ReadRequestFile(path)
	if err != nil {
		if github.IsRequestFileRejected(err) {
			_ = os.Rename(path, path+".rejected")
			r.logger.Warn("forge request relay: REJECTED (unsafe file)", "path", path, "reason", err.Error())
		}
		return false
	}
	if err := json.Unmarshal(data, into); err != nil {
		if github.RequestFileTorn(path, r.now()) {
			return false
		}
		writeBad("invalid JSON: " + err.Error())
		_ = os.Rename(path, path+".bad")
		r.logger.Warn("forge request relay: bad request file quarantined", "path", path, "error", err.Error())
		return false
	}
	return true
}

// repoRefusal is why repo may not receive agent writes through this relay,
// or "" when it may.
func repoRefusal(cfg *config.Config, agent, repo string) string {
	repo = strings.TrimSpace(repo)
	managed := false
	for _, r := range cfg.Project.Repos {
		if strings.EqualFold(r, repo) || strings.EqualFold(cfg.Project.Org+"/"+r, repo) {
			managed = true
			break
		}
	}
	switch {
	case !managed:
		return fmt.Sprintf("repo %q is not managed by this hive", repo)
	case cfg.RepoOnGitHub(repo):
		return fmt.Sprintf("repo %q is on GitHub; the forge relay serves only repos on another forge", repo)
	case cfg.IsRepoPaused(repo):
		return github.RepoPausedReason(repo)
	case !cfg.AgentServesRepo(agent, repo):
		return github.AgentRepoScopeReason(agent, repo)
	}
	return ""
}

func (r *forgeRequestRelay) stamp() string { return r.now().UTC().Format(time.RFC3339) }

func writeRelayResult(reqPath string, resp any) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = github.WriteRequestResult(out, b)
	}
}

// settle records the outcome of a request: success removes it, a refusal
// renames it with suffix, and a transient failure leaves it for a bounded
// number of retries.
func (r *forgeRequestRelay) settle(path, suffix string, transient bool) {
	switch {
	case suffix == "" && !transient:
		delete(r.attempts, path)
		_ = os.Remove(path)
	case transient:
		r.attempts[path]++
		if r.attempts[path] >= forgeRelayMaxAttempts {
			delete(r.attempts, path)
			_ = os.Rename(path, path+".failed")
			r.logger.Error("forge request relay: request exceeded its retries, quarantined", "path", path)
		}
	default:
		delete(r.attempts, path)
		_ = os.Rename(path, path+suffix)
	}
}

func (r *forgeRequestRelay) handlePR(ctx context.Context, path string) {
	var req github.PRRequest
	fail := func(msg string) { writeRelayResult(path, github.PRResponse{OK: false, Error: msg, At: r.stamp()}) }
	if !r.readRequest(path, &req, fail) {
		return
	}
	if r.prAuthz == nil {
		fail("authorization denied: no authorizer configured (fail closed)")
		r.settle(path, ".denied", false)
		return
	}
	if err := r.prAuthz(req.Agent, github.RequestFileOwnerUID(path)); err != nil {
		fail("authorization denied: " + err.Error())
		r.settle(path, ".denied", false)
		r.logger.Warn("forge request relay: PR DENIED (policy)", "agent", req.Agent, "repo", req.Repo, "reason", err.Error())
		return
	}
	cfg := r.cfg()
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		fail("PR repo rejected: " + reason)
		r.settle(path, ".rejected", false)
		return
	}
	if r.openPRs != nil {
		if mode, reason := r.openPRs(req.Agent, req.Repo, req.IssueN, "forge-relay"); reason != "" {
			auditOpenPRsRefusal(req.Agent, req.Repo, mode, "forge-relay", reason)
			fail("PR " + github.GateOpenPRs + " rejected: " + reason)
			r.settle(path, ".rejected", false)
			r.logger.Warn("forge request relay: PR REJECTED (open-prs)", "agent", req.Agent, "repo", req.Repo, "mode", mode, "reason", reason)
			return
		}
	}
	if reason := github.InvalidPRHeadReason(req.Head); reason != "" {
		fail("PR head rejected: " + reason)
		r.settle(path, ".rejected", false)
		return
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Body) == "" {
		fail("PR body rejected: a pull request needs a title and a body")
		r.settle(path, ".rejected", false)
		return
	}
	w, err := forgeRelayWriterForRepo(cfg, req.Repo)
	if err != nil {
		fail("PR forge unavailable: " + err.Error())
		r.settle(path, ".rejected", false)
		return
	}
	ref, err := w.CreateChangeRequest(ctx, req.Repo, forge.ChangeRequestSpec{
		Head:   req.Head,
		Base:   req.Base,
		Title:  prosefilter.SanitizeTitle(req.Title),
		Body:   prosefilter.Sanitize(req.Body),
		Labels: []string{cfg.Governor.Labels.ReadyForReviewLabelOrDefault()},
	})
	if err != nil && !errors.Is(err, forge.ErrLabelsNotApplied) {
		fail(err.Error())
		if isPermanentForgeRefusal(err) {
			r.settle(path, ".rejected", false)
			return
		}
		r.settle(path, "", true)
		r.logger.Warn("forge request relay: PR open failed, will retry", "repo", req.Repo, "head", req.Head, "error", err.Error())
		return
	}
	resp := github.PRResponse{OK: true, Number: ref.Number, URL: ref.URL, AlreadyExisted: ref.AlreadyExisted, At: r.stamp()}
	if err != nil {
		// Opened, but not held: say so, and never report it as a clean open.
		resp.Error = err.Error()
		r.logger.Error("forge request relay: PR opened without its review gate label", "repo", req.Repo, "number", ref.Number, "error", err.Error())
	}
	writeRelayResult(path, resp)
	r.settle(path, "", false)
	r.logger.Info("forge request relay: PR opened", "agent", req.Agent, "repo", req.Repo, "number", ref.Number, "already_existed", ref.AlreadyExisted)
}

// reservedLabels returns the labels in labels an agent may not set or clear
// through the relay: the hive's hold and gate labels and every label the
// GitHub label relay reserves (github.ReservedLabelForAgents).
func reservedLabels(cfg *config.Config, labels []string) []string {
	extra := holdLabelsForConfig(cfg)
	autoMerge := normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge)
	var out []string
	for _, l := range labels {
		if github.HasHoldLabelWith([]string{l}, extra) || github.ReservedLabelForAgents(l, autoMerge) {
			out = append(out, l)
		}
	}
	return out
}

// forgeStatusPrefix precedes the HTTP status in the adapters' error text.
const forgeStatusPrefix = "returned status "

// isPermanentForgeRefusal reports whether err is a forge 4xx that no retry
// can fix (the forge rejected the request itself). 408 and 429 are
// throttling and stay transient.
func isPermanentForgeRefusal(err error) bool {
	// The forge took the call but left a label off, after the adapter
	// looked the label up and tried to create it: a retry sends the same
	// call (#19910).
	if errors.Is(err, forge.ErrLabelsMissing) {
		return true
	}
	msg := err.Error()
	i := strings.Index(msg, forgeStatusPrefix+"4")
	if i < 0 {
		return false
	}
	code := msg[i+len(forgeStatusPrefix) : min(len(msg), i+len(forgeStatusPrefix)+3)]
	return code != "408" && code != "429"
}

func (r *forgeRequestRelay) handleIssue(ctx context.Context, path string) {
	var req github.IssueRequest
	fail := func(msg string) { writeRelayResult(path, github.IssueResponse{OK: false, Error: msg, At: r.stamp()}) }
	if !r.readRequest(path, &req, fail) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "issue"
	}
	cfg := r.cfg()

	var shape string
	switch kind {
	case "issue":
		if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.Title) == "" {
			shape = "issue request requires repo and title"
		} else if bad := reservedLabels(cfg, req.Labels); len(bad) > 0 {
			shape = "issue request names hive-controlled label(s) " + strings.Join(bad, ", ") + "; an agent may not set them"
		}
	case "comment":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Body) == "" {
			shape = "comment request requires repo, number, and body"
		}
	case "label":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || len(req.Labels)+len(req.RemoveLabels) == 0 {
			shape = "label request requires repo, number, and at least one label"
		} else if bad := reservedLabels(cfg, append(append([]string{}, req.Labels...), req.RemoveLabels...)); len(bad) > 0 {
			shape = "label request names hive-controlled label(s) " + strings.Join(bad, ", ") + "; an agent may not add or remove them"
		}
	default:
		fail(fmt.Sprintf("%q requests are not supported on %s yet", kind, cfg.ForgeKindForRepo(req.Repo)))
		r.settle(path, ".rejected", false)
		return
	}
	if shape != "" {
		fail(shape)
		r.settle(path, ".bad", false)
		return
	}
	if r.issueAuthz == nil {
		fail("authorization denied: no authorizer configured (fail closed)")
		r.settle(path, ".denied", false)
		return
	}
	if err := r.issueAuthz(req.Agent, github.RequestFileOwnerUID(path), kind); err != nil {
		fail("authorization denied: " + err.Error())
		r.settle(path, ".denied", false)
		r.logger.Warn("forge request relay: issue request DENIED (policy)", "agent", req.Agent, "kind", kind, "repo", req.Repo, "reason", err.Error())
		return
	}
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		fail("repo rejected: " + reason)
		r.settle(path, ".rejected", false)
		return
	}
	w, err := forgeRelayWriterForRepo(cfg, req.Repo)
	if err != nil {
		fail("forge unavailable: " + err.Error())
		r.settle(path, ".rejected", false)
		return
	}

	resp := github.IssueResponse{OK: true, Number: req.Number, At: r.stamp()}
	switch kind {
	case "issue":
		labels := append(append([]string{}, req.Labels...), config.OriginLabelForAgent(req.Agent))
		var ref forge.IssueRef
		ref, err = w.CreateIssue(ctx, req.Repo, prosefilter.SanitizeTitle(req.Title), prosefilter.Sanitize(req.Body), labels) // writeback:solicited the agent's own issue request
		if err == nil || errors.Is(err, forge.ErrLabelsNotApplied) {
			resp.Number, resp.URL = ref.Number, ref.URL
			if err != nil {
				resp.Error, err = err.Error(), nil
			}
		}
	case "comment":
		err = w.CreateIssueComment(ctx, req.Repo, req.Number, prosefilter.Sanitize(req.Body)) // writeback:solicited the agent's own comment request
	case "label":
		if err = w.AddLabels(ctx, req.Repo, req.Number, req.Labels); err == nil { // writeback:solicited the agent's own label request
			for _, l := range req.RemoveLabels {
				if err = w.RemoveLabel(ctx, req.Repo, req.Number, l); err != nil { // writeback:solicited the agent's own label request
					break
				}
			}
		}
	}
	if err != nil {
		fail(err.Error())
		if isPermanentForgeRefusal(err) {
			r.settle(path, ".rejected", false)
			return
		}
		r.settle(path, "", true)
		r.logger.Warn("forge request relay: issue request failed, will retry", "kind", kind, "repo", req.Repo, "error", err.Error())
		return
	}
	writeRelayResult(path, resp)
	r.settle(path, "", false)
	r.logger.Info("forge request relay: issue request done", "agent", req.Agent, "kind", kind, "repo", req.Repo, "number", resp.Number)
}
