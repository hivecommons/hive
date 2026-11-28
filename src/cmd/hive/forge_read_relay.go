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
//   - The same queue serves `hive-gh pr reviews`, `pr comments` / `issue
//     comments` and `run view --log` / `--log-failed` / `--job <id> --log`
//     (#20277) with the kinds reviews, comments, run-log, run-log-failed and
//     job-log. A log view carries at most the last forgeReadJobLogLimit
//     bytes of a job, and forgeReadRunLogLimit over a run.

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
	// forgeReadMaxReviewCommentReads bounds the per-review comment reads
	// of one reviews request; a review past it reports commentsRead false.
	forgeReadMaxReviewCommentReads = 30
	// forgeReadJobLogLimit is the most of one job's log a view carries:
	// its last bytes, where a failure usually is.
	forgeReadJobLogLimit = 256 << 10
	// forgeReadRunLogLimit is the most log one run view carries over all
	// its jobs.
	forgeReadRunLogLimit = 512 << 10
	// forgeReadMaxRunJobs bounds the jobs whose logs one run view reads.
	forgeReadMaxRunJobs = 20
)

// forgeReadKinds are the kinds the relay serves. pr, issue and checks
// (#20230) carry a pull request or issue number, as do reviews and comments
// (#20277); run-log and run-log-failed carry a Forgejo Actions run ID, and
// job-log a job ID.
var forgeReadKinds = map[string]bool{
	"pr": true, "issue": true, "checks": true,
	"reviews": true, "comments": true,
	"run-log": true, "run-log-failed": true, "job-log": true,
}

const forgeReadKindList = "pr, issue, checks, reviews, comments, run-log, run-log-failed, job-log"

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
	if !forgeReadKinds[req.Kind] {
		reject(fmt.Sprintf("kind %q is not one of %s", req.Kind, forgeReadKindList))
		return false
	}
	if req.Number <= 0 {
		reject("number must be a positive pull request, issue, run or job number")
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
	case "reviews", "comments", "run-log", "run-log-failed", "job-log":
		return forgeReadDiscussionView(ctx, api, repo, kind, number)
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
		// Forgejo's mergeable is a bare bool. It is false on a merged or
		// closed pull request and on a draft (WIP) one, so those report
		// GitHub's UNKNOWN. Only a false on an open, non-draft pull request
		// is reported as CONFLICTING. A false while Forgejo is still running
		// its own conflict check cannot be told apart from a real conflict.
		mergeable := "UNKNOWN"
		switch {
		case pr.Mergeable:
			mergeable = "MERGEABLE"
		case strings.EqualFold(pr.State, "open") && !pr.Merged && !pr.Draft:
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

// forgeReadDiscussionView serves the reviews, comments and log kinds
// (#20277). Reviews and comments carry the key names of `gh pr view --json
// reviews` / `--json comments` after hive-gh's projection (author is the
// login); a log is JSON too, one entry per job with its log text.
func forgeReadDiscussionView(ctx context.Context, api forge.ReadAPI, repo, kind string, number int) ([]byte, error) {
	fj := string(forge.KindForgejo)
	switch kind {
	case "reviews":
		reviews, err := api.PullReviews(ctx, repo, number, forgeReadMaxReviewCommentReads)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(reviews))
		for _, r := range reviews {
			comments := make([]map[string]any, 0, len(r.Comments))
			for _, c := range r.Comments {
				comments = append(comments, map[string]any{
					"id": c.ID, "author": c.Author, "path": c.Path, "line": c.Line, "body": c.Body,
					"commit": c.CommitID, "url": c.URL, "createdAt": c.CreatedAt,
				})
			}
			out = append(out, map[string]any{
				"id": r.ID, "author": r.Author, "state": forgeReviewState(r.State, r.Dismissed), "body": r.Body,
				"submittedAt": r.SubmittedAt, "commit": r.CommitID, "url": r.URL, "stale": r.Stale,
				"dismissed": r.Dismissed, "official": r.Official, "commentCount": r.CommentCount,
				"commentsRead": r.CommentsRead, "comments": comments,
			})
		}
		return json.Marshal(map[string]any{"number": number, "reviews": out, "forge": fj})
	case "comments":
		comments, err := api.IssueComments(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(comments))
		for _, c := range comments {
			out = append(out, map[string]any{
				"id": c.ID, "author": c.Author, "body": c.Body, "url": c.URL, "createdAt": c.CreatedAt, "updatedAt": c.UpdatedAt,
			})
		}
		return json.Marshal(map[string]any{"number": number, "comments": out, "forge": fj})
	case "job-log":
		log, truncated, err := api.JobLog(ctx, repo, int64(number), forgeReadJobLogLimit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"jobId": number, "log": log, "logTruncated": truncated, "forge": fj})
	}
	// run-log, run-log-failed
	jobs, err := api.RunJobs(ctx, repo, int64(number))
	if err != nil {
		return nil, err
	}
	budget, read := forgeReadRunLogLimit, 0
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		failed := forgeCheckBucket(j.Conclusion) == "fail"
		entry := map[string]any{
			"databaseId": j.ID, "name": j.Name, "status": strings.ToUpper(j.Status),
			"conclusion": strings.ToUpper(j.Conclusion), "url": j.URL,
		}
		switch {
		case kind == "run-log-failed" && !failed:
			// Listed without a log, as gh's --log-failed prints none for it.
		case read >= forgeReadMaxRunJobs || budget <= 0:
			entry["logOmitted"] = "the run's log limit was reached; read it with hive-gh run view --job <databaseId> --log"
		default:
			read++
			log, truncated, err := api.JobLog(ctx, repo, j.ID, min(forgeReadJobLogLimit, budget))
			if err != nil {
				return nil, err
			}
			budget -= len(log)
			entry["log"], entry["logTruncated"] = log, truncated
		}
		out = append(out, entry)
	}
	return json.Marshal(map[string]any{"runId": number, "jobs": out, "forge": fj})
}

// forgeReviewState maps a Forgejo review state onto gh's review states
// (APPROVED, CHANGES_REQUESTED, COMMENTED, PENDING, DISMISSED). Forgejo
// keeps a dismissed review's state and flags it, where GitHub reports
// DISMISSED.
func forgeReviewState(state string, dismissed bool) string {
	if dismissed {
		return "DISMISSED"
	}
	switch strings.ToUpper(state) {
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	case "COMMENT":
		return "COMMENTED"
	}
	return strings.ToUpper(state)
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
