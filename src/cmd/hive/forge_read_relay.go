package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// The forge read relay (hivecommons/hive#20230).
//
// On a forge-only hive an agent has no tool that reads a pull request or an
// issue: hive-gh fronts the GitHub gh wrapper, and the agent holds no forge
// credential. This relay serves `hive-gh pr view` / `issue view` / `pr
// checks` for a Forgejo repo, with the clone relay's pattern
// (forge_clone_relay.go):
//
//   - hive-gh drops {agent, repo, kind, number} in the read-requests queue.
//     The request file's owner UID must be the agent named
//     (AuthorizeForgeClone, the same gate a clone passes: reading the change
//     is no more than cloning it), and the repo must be managed, off GitHub,
//     not paused, in the agent's scope and on Forgejo.
//   - The hive reads through the forge adapter (forge.ReadAPI) with the
//     token work discovery already uses; the token never leaves the hive.
//   - The answer is the compact JSON projection hive-gh prints for GitHub,
//     with the same key names where it carries them, written into the queue as
//     a new file of mode 0600 owned by the requesting agent's UID, so no
//     other agent can read it. It is a subset: a pull request carries no
//     mergeStateStatus, reviewDecision, additions, deletions or
//     changedFiles, and an issue no stateReason or milestone.
//     The result next to the request only names that file. A view the agent
//     leaves behind is removed after forgeReadViewTTL.

const (
	// forgeReadRequestDir is the read-request queue. hive-gh writes it.
	forgeReadRequestDir = "/var/run/hive-metrics/read-requests"
	// forgeReadViewSuffix ends every view file the relay writes. It does
	// not end in .json, so a view is never taken for a request.
	forgeReadViewSuffix = ".view"
	// forgeReadViewTTL is how long a view may stay in the queue.
	forgeReadViewTTL = 10 * time.Minute
	// forgeReadMaxPerScan bounds the forge reads served in one scan.
	forgeReadMaxPerScan = 8
)

// forgeReadRequest is what hive-gh writes.
type forgeReadRequest struct {
	Agent  string `json:"agent"`
	Repo   string `json:"repo"`
	Kind   string `json:"kind"`
	Number int    `json:"number"`
}

// forgeReadResponse is written next to the request as .result.json.
type forgeReadResponse struct {
	OK       bool   `json:"ok"`
	Repo     string `json:"repo,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Number   int    `json:"number,omitempty"`
	View     string `json:"view,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

// forgeReadAPIForRepo builds the read adapter for a Forgejo repo, with the
// URL, token and token-host rules of forgeListerForRepo. It is a variable so
// tests can substitute a fake.
var forgeReadAPIForRepo = func(cfg *config.Config, repo string) (forge.ReadAPI, error) {
	l, err := forgeListerForRepo(cfg, repo)
	if err != nil {
		return nil, err
	}
	r, ok := l.(forge.ReadAPI)
	if !ok {
		return nil, fmt.Errorf("the %s adapter cannot read a single pull request or issue", cfg.ForgeKindForRepo(repo))
	}
	return r, nil
}

// serveReadQueue serves pending read requests, at most forgeReadMaxPerScan
// forge reads per scan, and removes expired views.
func (r *forgeRequestRelay) serveReadQueue(ctx context.Context) {
	if r.readDir == "" {
		return
	}
	r.expireReadViews()
	reads := 0
	for _, path := range pendingRequestFiles(r.readDir) {
		if reads >= forgeReadMaxPerScan || ctx.Err() != nil {
			return
		}
		if r.handleRead(ctx, path) {
			reads++
		}
	}
}

// handleRead serves one read request. It reports whether the request reached
// the forge, which is what the per-scan budget counts.
func (r *forgeRequestRelay) handleRead(ctx context.Context, path string) (read bool) {
	var req forgeReadRequest
	result := func(resp forgeReadResponse) {
		if resp.Repo == "" {
			resp.Repo = req.Repo
		}
		resp.Kind, resp.Number, resp.At = req.Kind, req.Number, r.stamp()
		writeRelayResult(path, resp)
	}
	fail := func(msg string) { result(forgeReadResponse{Error: msg}) }
	data, fileUID, err := forgeCloneReadRequest(path)
	if err != nil {
		if github.IsRequestFileRejected(err) {
			_ = os.Rename(path, path+".rejected")
			r.logger.Warn("forge request relay: read REJECTED (unsafe file)", "path", path, "reason", err.Error())
		}
		return false
	}
	if err := json.Unmarshal(data, &req); err != nil {
		if github.RequestFileTorn(path, r.now()) {
			return false
		}
		fail("invalid JSON: " + err.Error())
		_ = os.Rename(path, path+".bad")
		return false
	}
	deny := func(reason string) {
		fail("authorization denied: " + reason)
		r.settle(path, ".denied", false)
		r.logger.Warn("forge request relay: read DENIED (policy)", "agent", req.Agent, "repo", req.Repo, "reason", reason)
	}
	reject := func(reason string) {
		fail("read rejected: " + reason)
		r.settle(path, ".rejected", false)
		r.logger.Warn("forge request relay: read REJECTED", "agent", req.Agent, "repo", req.Repo, "reason", reason)
	}
	switch req.Kind {
	case "pr", "issue", "checks":
	default:
		reject(fmt.Sprintf("kind %q is not one of pr, issue, checks", req.Kind))
		return false
	}
	if req.Number <= 0 {
		reject("number must be a positive pull request or issue number")
		return false
	}
	if r.cloneAuthz == nil {
		deny("no authorizer configured (fail closed)")
		return false
	}
	if fileUID <= 0 {
		deny(fmt.Sprintf("the read relay needs a request file owned by the agent's own UID, not uid %d", fileUID))
		return false
	}
	if _, err := r.cloneAuthz(req.Agent, fileUID); err != nil {
		deny(err.Error())
		return false
	}
	cfg := r.cfg()
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		reject(reason)
		return false
	}
	repo := canonicalManagedRepo(cfg, req.Repo)
	if repo == "" {
		reject(fmt.Sprintf("repo %q is not managed by this hive", req.Repo))
		return false
	}
	if cfg.ForgeKindForRepo(repo) != config.ForgeForgejo {
		reject(fmt.Sprintf("repo %s is on %s; the read relay serves forgejo repos only", repo, cfg.ForgeKindForRepo(repo)))
		return false
	}
	attempt := r.attempts[path] + 1
	api, err := forgeReadAPIForRepo(cfg, repo)
	if err != nil {
		reject(err.Error())
		return false
	}
	read = true
	view, err := forgeReadView(ctx, api, repo, req.Kind, req.Number)
	if err != nil {
		if isPermanentForgeRefusal(err) {
			reject(err.Error())
			return read
		}
		result(forgeReadResponse{Repo: repo, Attempts: attempt, Error: err.Error()})
		r.settle(path, "", true)
		return read
	}
	out, err := r.deliverReadView(path, view, fileUID)
	if err != nil {
		result(forgeReadResponse{Repo: repo, Attempts: attempt, Error: "handing the view to the agent: " + err.Error()})
		r.settle(path, "", true)
		return read
	}
	result(forgeReadResponse{OK: true, Repo: repo, View: out, Attempts: attempt})
	r.settle(path, "", false)
	return read
}

// forgeReadView reads one pull request, issue or check list and returns it
// as the compact projection hive-gh prints on GitHub (bin/hive-gh.sh), with
// the same key names, so a policy or agent reading those keys works on either
// forge. It is a subset of that projection: Forgejo's API has no
// counterpart this adapter reads for mergeStateStatus, reviewDecision,
// additions, deletions, changedFiles (pull request) or stateReason and
// milestone (issue), so those keys are absent and a gate reading them gets
// null.
func forgeReadView(ctx context.Context, api forge.ReadAPI, repo, kind string, number int) ([]byte, error) {
	var view any
	switch kind {
	case "issue":
		is, err := api.IssueView(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		view = map[string]any{
			"number": is.Number, "title": is.Title, "state": strings.ToUpper(is.State), "url": is.URL,
			"author": is.Author, "labels": nonNil(is.Labels), "assignees": nonNil(is.Assignees),
			"createdAt": is.CreatedAt, "updatedAt": is.UpdatedAt, "body": is.Body, "forge": string(forge.KindForgejo),
		}
	case "pr", "checks":
		pr, err := api.PullView(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		combined, statuses := "", []forge.CommitStatus(nil)
		if pr.HeadSHA != "" {
			if combined, statuses, err = api.CommitStatuses(ctx, repo, pr.HeadSHA); err != nil {
				return nil, err
			}
		}
		if kind == "checks" {
			checks := make([]map[string]any, 0, len(statuses))
			for _, s := range statuses {
				checks = append(checks, map[string]any{
					"name": s.Context, "bucket": forgeCheckBucket(s.State), "state": strings.ToUpper(s.State),
					"workflow": "", "link": s.TargetURL,
				})
			}
			view = checks
			break
		}
		rollup := make([]map[string]any, 0, len(statuses))
		for _, s := range statuses {
			rollup = append(rollup, map[string]any{"name": s.Context, "status": strings.ToUpper(s.State), "conclusion": strings.ToUpper(s.State)})
		}
		state := strings.ToUpper(pr.State)
		if pr.Merged {
			state = "MERGED"
		}
		// Forgejo's mergeable is a bare bool that is also false on a merged
		// or closed pull request, where GitHub reports UNKNOWN; only an open
		// pull request's false means a conflict.
		mergeable := "UNKNOWN"
		switch {
		case pr.Mergeable:
			mergeable = "MERGEABLE"
		case state == "OPEN":
			mergeable = "CONFLICTING"
		}
		view = map[string]any{
			"number": pr.Number, "title": pr.Title, "state": state, "isDraft": pr.Draft, "url": pr.URL,
			"author": pr.Author, "headRefName": pr.HeadRef, "headRefOid": pr.HeadSHA, "baseRefName": pr.Base,
			"labels": nonNil(pr.Labels), "mergeable": mergeable, "statusCheckRollup": rollup,
			"statusCheckState": strings.ToUpper(combined), "updatedAt": pr.UpdatedAt, "body": pr.Body,
			"forge": string(forge.KindForgejo),
		}
	}
	return json.Marshal(view)
}

// forgeCheckBucket maps a Forgejo commit status state onto gh's check
// buckets (pass, fail, pending).
func forgeCheckBucket(state string) string {
	switch strings.ToLower(state) {
	case "success", "warning":
		return "pass"
	case "failure", "error":
		return "fail"
	}
	return "pending"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// deliverReadView writes view into the queue as a new file (random name,
// created exclusively) of mode 0600 and gives it to uid, as the clone relay
// delivers a bundle (deliverCloneBundle), including the su-exec write when
// the hive cannot chown. It returns the file's path.
func (r *forgeRequestRelay) deliverReadView(reqPath string, view []byte, uid int) (string, error) {
	base := strings.TrimSuffix(filepath.Base(reqPath), ".json")
	dst, err := os.CreateTemp(r.readDir, base+".*"+forgeReadViewSuffix)
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
	if _, err := dst.Write(view); err != nil {
		return "", err
	}
	if err := forgeCloneChown(dst, uid); err != nil {
		if !errors.Is(err, syscall.EPERM) {
			return "", fmt.Errorf("giving the view to uid %d: %w", uid, err)
		}
		_ = dst.Close()
		if err := os.Remove(name); err != nil {
			return "", err
		}
		err = forgeCloneWriteAsAgent(context.Background(), uid, name, bytes.NewReader(view))
		if err == nil {
			err = forgeCloneCheckAgentBundle(name, uid, int64(len(view)))
		}
		if err != nil {
			_ = os.Remove(name)
			return "", fmt.Errorf("giving the view to uid %d: chown: %v; writing it as the agent: %w", uid, syscall.EPERM, err)
		}
		ok = true
		return name, nil
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// expireReadViews removes views older than forgeReadViewTTL. Only regular
// files are removed, and a symlink is never followed.
func (r *forgeRequestRelay) expireReadViews() {
	entries, err := os.ReadDir(r.readDir)
	if err != nil {
		return
	}
	cutoff := r.now().Add(-forgeReadViewTTL)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), forgeReadViewSuffix) {
			continue
		}
		p := filepath.Join(r.readDir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(p)
	}
}
