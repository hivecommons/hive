package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The forge clone relay (hivecommons/hive#19997).
//
// A pod-run agent on a forge-only hive has no credential for the forge host
// (its FORGEJO_TOKEN is stripped, #19498, and the proxy never attaches a
// token to a forge host, crux/proxy/forge_hosts.go), so it cannot clone a
// private repo. This relay clones on its behalf, the read-side twin of the
// push relay (forge_push_relay.go):
//
//   - The agent drops {agent, repo} in the clone-requests queue
//     (hive-clone). The request file's owner UID must be a registered agent
//     UID and the agent named (AuthorizeForgeClone); there is no shared-UID
//     fallback. The repo must be managed, off GitHub, not paused and in the
//     agent's scope (repoRefusal), and only its configured name is used to
//     build the URL.
//   - The hive needs forgejo.integration: the fetch carries a one-hour JWT
//     minted for the agent's own token tier, never the long-lived access
//     token. A failed mint fetches nothing; there is no anonymous fallback.
//   - The fetch runs as the hive in a fresh hive-owned GIT_DIR (mode 0700)
//     with a config the hive writes, system and global config off, hooks,
//     fsmonitor and credential helpers off, TLS verification pinned and
//     https only (github.PushBranchGitEnv). The token is only in that git
//     process's environment, as an extraHeader scoped to the one remote
//     URL. It is never in an argument, a config file, the agent's
//     environment or anything the agent can read, and git output is
//     scrubbed of it before it is logged or written to a result.
//   - The agent receives only objects and refs: a git bundle of the repo's
//     branches and tags, written into the queue as a new file of mode 0600
//     owned by the requesting agent's UID, so no other agent can read it.
//     The agent clones from or fetches the bundle as itself; the hive never
//     writes into the agent's workspace. The staging GIT_DIR is removed, and
//     a bundle the agent leaves behind is removed after
//     forgeCloneBundleTTL.

const (
	// forgeCloneRequestDir is the clone-request queue. hive-clone writes it.
	forgeCloneRequestDir = "/var/run/hive-metrics/clone-requests"
	// forgeCloneBundleSuffix ends every bundle the relay writes.
	forgeCloneBundleSuffix = ".bundle"
	// forgeCloneBundleTTL is how long a bundle may stay in the queue before
	// the relay removes it.
	forgeCloneBundleTTL = 30 * time.Minute
	// forgeCloneMaxPerScan bounds the clones served in one scan, so a burst
	// of requests cannot hold the PR and issue queues for long.
	forgeCloneMaxPerScan = 2
	// forgeCloneExecTimeout bounds one git command of the clone relay.
	forgeCloneExecTimeout = 10 * time.Minute
	// forgeCloneMaxBundleBytesEnv overrides forgeCloneDefaultMaxBundleBytes.
	forgeCloneMaxBundleBytesEnv = "HIVE_FORGE_CLONE_MAX_BUNDLE_BYTES"
	// forgeCloneDefaultMaxBundleBytes caps one bundle.
	forgeCloneDefaultMaxBundleBytes int64 = 2 << 30
	// forgeCloneStageDirMode keeps the staging GIT_DIR private to the hive.
	forgeCloneStageDirMode = 0o700
	// forgeCloneStageFileMode is the mode of the files written into it.
	forgeCloneStageFileMode = 0o600
	// forgeCloneRedacted replaces the token in any git output.
	forgeCloneRedacted = "[redacted]"
)

// forgeCloneAuthorizer maps a clone request to the token tier it is minted
// for, or refuses it (agent.Manager.AuthorizeForgeClone).
type forgeCloneAuthorizer func(agentName string, fileUID int) (agentmode.TokenTier, error)

// forgeCloneAuthorizerSource is the part of the agent runtime that
// authorizes clone requests. The in-process Manager implements it; a runtime
// that does not leaves the clone relay without an authorizer, so it denies
// every request.
type forgeCloneAuthorizerSource interface {
	AuthorizeForgeClone(agentName string, fileUID int) (agentmode.TokenTier, error)
}

// forgeCloneRequest is what hive-clone writes.
type forgeCloneRequest struct {
	Agent string `json:"agent"`
	Repo  string `json:"repo"`
}

// forgeCloneResponse is written next to the request as .result.json.
type forgeCloneResponse struct {
	OK            bool   `json:"ok"`
	Repo          string `json:"repo,omitempty"`
	Bundle        string `json:"bundle,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	Error         string `json:"error,omitempty"`
	At            string `json:"at"`
}

// forgeCloneExec runs one git command for the clone relay and returns its
// combined output, like forgePushExec. It is a variable so tests run
// without a git binary or network.
var forgeCloneExec = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, forgeCloneExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(forgeCloneBaseEnv(os.Environ()), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// forgeCloneFileOwnerUID is the request file's owner UID
// (github.RequestFileOwnerUID). It is a variable so tests, whose files are
// all owned by the test's own UID, can present other owners.
var forgeCloneFileOwnerUID = github.RequestFileOwnerUID

// forgeCloneChown gives the bundle to the requesting agent's UID. It is a
// variable so tests, which cannot chown to another UID, can observe it.
var forgeCloneChown = func(f *os.File, uid int) error { return f.Chown(uid, -1) }

// forgeCloneBaseEnv is the hive's environment for a clone git: inherited
// forge and GitHub credentials stripped (pushbroker.PushEnv), and git's
// trace and config variables dropped so none can print the request headers
// or point git at another config.
func forgeCloneBaseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range pushbroker.PushEnv(env) {
		name, _, _ := strings.Cut(e, "=")
		switch {
		case strings.HasPrefix(name, "GIT_TRACE"), name == "GIT_CURL_VERBOSE", name == "GIT_REDACT_COOKIES",
			strings.HasPrefix(name, "GIT_CONFIG"), name == "GIT_DIR", name == "GIT_OBJECT_DIRECTORY",
			name == "GIT_ALTERNATE_OBJECT_DIRECTORIES", name == "GIT_WORK_TREE", name == "GIT_SSL_NO_VERIFY":
			continue
		}
		out = append(out, e)
	}
	return out
}

// forgeCloneMaxBundleBytes is the bundle cap: the env override when it is a
// positive integer, else the default.
func forgeCloneMaxBundleBytes() int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(forgeCloneMaxBundleBytesEnv)), 10, 64); err == nil && v > 0 {
		return v
	}
	return forgeCloneDefaultMaxBundleBytes
}

// withCloneRelay turns on the clone queue with authz, the manager's clone
// authorizer. A relay without it still answers clone requests, and denies
// every one (fail closed).
func (r *forgeRequestRelay) withCloneRelay(authz forgeCloneAuthorizer) *forgeRequestRelay {
	r.cloneAuthz = authz
	return r
}

// serveCloneQueue handles up to forgeCloneMaxPerScan clone requests and
// removes expired bundles.
func (r *forgeRequestRelay) serveCloneQueue(ctx context.Context) {
	if r.cloneDir == "" {
		return
	}
	r.expireCloneBundles()
	for i, path := range pendingRequestFiles(r.cloneDir) {
		if i >= forgeCloneMaxPerScan || ctx.Err() != nil {
			return
		}
		r.handleClone(ctx, path)
	}
}

// expireCloneBundles removes bundles older than forgeCloneBundleTTL. Only
// regular files are removed, and a symlink is never followed.
func (r *forgeRequestRelay) expireCloneBundles() {
	entries, err := os.ReadDir(r.cloneDir)
	if err != nil {
		return
	}
	cutoff := r.now().Add(-forgeCloneBundleTTL)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), forgeCloneBundleSuffix) {
			continue
		}
		p := filepath.Join(r.cloneDir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(p)
	}
}

// canonicalManagedRepo returns repo as "owner/name" spelled as it is
// configured, so the URL is built from the operator's spelling and never
// from the request's.
func canonicalManagedRepo(cfg *config.Config, repo string) string {
	repo = strings.TrimSpace(repo)
	for _, name := range cfg.Project.Repos {
		if strings.EqualFold(name, repo) || strings.EqualFold(cfg.Project.Org+"/"+name, repo) {
			return fullRepoName(name, cfg.Project.Org)
		}
	}
	return ""
}

func (r *forgeRequestRelay) handleClone(ctx context.Context, path string) {
	var req forgeCloneRequest
	result := func(resp forgeCloneResponse) {
		if resp.Repo == "" {
			resp.Repo = req.Repo
		}
		resp.At = r.stamp()
		writeRelayResult(path, resp)
	}
	fail := func(msg string) { result(forgeCloneResponse{Error: msg}) }
	if !r.readRequest(path, &req, fail) {
		return
	}
	deny := func(reason string) {
		fail("authorization denied: " + reason)
		r.settle(path, ".denied", false)
		r.logger.Warn("forge request relay: clone DENIED (policy)", "agent", req.Agent, "repo", req.Repo, "reason", reason)
	}
	reject := func(reason string) {
		fail("clone rejected: " + reason)
		r.settle(path, ".rejected", false)
		r.logger.Warn("forge request relay: clone REJECTED", "agent", req.Agent, "repo", req.Repo, "reason", reason)
	}
	if r.cloneAuthz == nil {
		deny("no authorizer configured (fail closed)")
		return
	}
	fileUID := forgeCloneFileOwnerUID(path)
	if fileUID <= 0 {
		deny(fmt.Sprintf("the clone relay needs a request file owned by the agent's own UID, not uid %d", fileUID))
		return
	}
	tier, err := r.cloneAuthz(req.Agent, fileUID)
	if err != nil {
		deny(err.Error())
		return
	}
	cfg := r.cfg()
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		reject(reason)
		return
	}
	repo := canonicalManagedRepo(cfg, req.Repo)
	if repo == "" {
		reject(fmt.Sprintf("repo %q is not managed by this hive", req.Repo))
		return
	}
	if cfg.ForgeKindForRepo(repo) != config.ForgeForgejo {
		reject(fmt.Sprintf("repo %s is on %s; the clone relay serves forgejo repos only", repo, cfg.ForgeKindForRepo(repo)))
		return
	}
	if cfg.Forgejo.Integration == nil {
		reject("the clone relay needs forgejo.integration, so the clone carries a one-hour token for the agent's tier rather than the long-lived access token")
		return
	}
	if r.pushRemote == nil {
		reject("no forge remote resolver configured (fail closed)")
		return
	}
	remote, ok, err := r.pushRemote(repo)
	if err != nil || !ok {
		reason := fmt.Sprintf("repo %s has no forge remote this hive can clone from", repo)
		if err != nil {
			reason = err.Error()
		}
		reject(reason)
		return
	}
	remoteURL := pushbroker.ForgeRepoURL(remote.BaseURL, repo)
	if remoteURL == "" {
		reject(fmt.Sprintf("repo %s has no usable clone URL on %s", repo, remote.BaseURL))
		return
	}
	attempt := r.attempts[path] + 1
	retry := func(msg string) {
		result(forgeCloneResponse{Repo: repo, Attempts: attempt, Error: msg})
		r.settle(path, "", true)
		r.logger.Warn("forge request relay: clone failed, will retry", "agent", req.Agent, "repo", repo, "attempt", attempt, "error", msg)
	}

	def, err := forgeRepoDefaultBranch(ctx, cfg, repo)
	if err != nil {
		retry("reading the default branch: " + err.Error())
		return
	}
	if !github.ValidPushBranchName(def) {
		reject(fmt.Sprintf("the default branch %q of %s is not a name the relay can use", def, repo))
		return
	}

	// Fail closed: without a token nothing is fetched, and there is no
	// anonymous attempt.
	token, err := pushbroker.WithTier(remote.Minter, tier).MintPushToken(ctx, repo)
	if err != nil {
		// The mint error can name the signing key's path; it goes to the
		// hive log, not to the agent's result.
		r.logger.Warn("forge request relay: clone token mint failed; nothing fetched", "agent", req.Agent, "repo", repo, "tier", tier, "error", err)
		retry("minting the clone token failed; nothing was fetched (the hive log has the reason)")
		return
	}
	if strings.TrimSpace(token) == "" {
		retry("minting the clone token: the minter returned no token")
		return
	}
	stage, cleanup, err := forgeCloneStage(def)
	if err != nil {
		retry("preparing the staging git dir: " + err.Error())
		return
	}
	defer cleanup()
	bare := append(github.PushBranchGitEnv(stage, remoteURL, ""), "GIT_DIR="+stage)
	fetchEnv := append(github.PushBranchGitEnv(stage, remoteURL, token), "GIT_DIR="+stage)
	scrub := forgeCloneScrubber(token, fetchEnv)
	if out, err := forgeCloneExec(ctx, stage, fetchEnv, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		remoteURL, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		retry("git fetch failed: " + scrub(github.FirstOutputLines(out, forgePushOutputLines)))
		return
	}
	if out, err := forgeCloneExec(ctx, stage, bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+def); err != nil {
		reject(fmt.Sprintf("%s has no commit on its default branch %q: %s", repo, def, scrub(github.FirstOutputLines(out, forgePushOutputLines))))
		return
	}
	staged := filepath.Join(stage, "repo"+forgeCloneBundleSuffix)
	if out, err := forgeCloneExec(ctx, stage, bare, "bundle", "create", staged, "--all"); err != nil {
		retry("git bundle failed: " + scrub(github.FirstOutputLines(out, forgePushOutputLines)))
		return
	}
	bundle, err := r.deliverCloneBundle(path, staged, fileUID)
	if err != nil {
		retry("handing the bundle to the agent: " + scrub(err.Error()))
		return
	}
	result(forgeCloneResponse{OK: true, Repo: repo, Bundle: bundle, RemoteURL: remoteURL, DefaultBranch: def, Attempts: attempt})
	r.settle(path, "", false)
	r.logger.Info("forge request relay: repo cloned for agent", "agent", req.Agent, "repo", repo, "tier", tier, "bundle", bundle)
}

// forgeCloneStage creates the hive-owned GIT_DIR the fetch writes into: a
// bare repository whose config the hive writes (no remotes, no hooks, no
// submodule recursion) and whose HEAD names the default branch, so the
// bundle checks it out. It returns the directory and its cleanup.
func forgeCloneStage(defaultBranch string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "hive-clone-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	files := map[string]string{
		"config": "[core]\n\trepositoryformatversion = 0\n\tbare = true\n[fetch]\n\trecurseSubmodules = false\n",
		"HEAD":   "ref: refs/heads/" + defaultBranch + "\n",
	}
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), forgeCloneStageDirMode); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), forgeCloneStageFileMode); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dir, cleanup, nil
}

// deliverCloneBundle copies the staged bundle into the queue as a new file
// (random name, created exclusively, so nothing the agent planted is
// written through) of mode 0600, then gives it to uid. It returns the
// bundle's path. On any failure the new file is removed.
func (r *forgeRequestRelay) deliverCloneBundle(reqPath, staged string, uid int) (string, error) {
	src, err := os.Open(staged)
	if err != nil {
		return "", err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	if limit := forgeCloneMaxBundleBytes(); info.Size() > limit {
		return "", fmt.Errorf("the bundle is %d bytes, over the %d-byte limit (%s)", info.Size(), limit, forgeCloneMaxBundleBytesEnv)
	}
	base := strings.TrimSuffix(filepath.Base(reqPath), ".json")
	dst, err := os.CreateTemp(r.cloneDir, base+".*"+forgeCloneBundleSuffix)
	if err != nil {
		return "", err
	}
	name := dst.Name()
	ok := false
	defer func() {
		if !ok {
			_ = dst.Close()
			_ = os.Remove(name)
		}
	}()
	if err := dst.Chmod(forgeCloneStageFileMode); err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	if err := forgeCloneChown(dst, uid); err != nil {
		return "", fmt.Errorf("giving the bundle to uid %d: %w", uid, err)
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// forgeCloneScrubber returns a function that removes token from text, and
// every form git sends it in: each extraHeader value in env and the
// credential inside it.
func forgeCloneScrubber(token string, env []string) func(string) string {
	secrets := []string{token}
	for _, e := range env {
		_, value, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(e, "GIT_CONFIG_VALUE_") || !strings.HasPrefix(value, "Authorization:") {
			continue
		}
		secrets = append(secrets, value)
		if fields := strings.Fields(value); len(fields) > 0 {
			secrets = append(secrets, fields[len(fields)-1])
		}
	}
	return func(s string) string {
		for _, sec := range secrets {
			if sec != "" {
				s = strings.ReplaceAll(s, sec, forgeCloneRedacted)
			}
		}
		return s
	}
}
