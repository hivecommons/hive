package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// errMissingHead reports that the PR request named a head ref that does not
// exist on the remote. It stays an ordinary (retryable) error rather than a
// policy rejection: the agent pushing its branch makes the same request valid,
// which is exactly what the bounded retry is for. The wrapped compare 404 is
// deliberately exposed through Unwrap so that signedAPIRefusal on the
// signed-reconciler path can classify it as a bounded refusal.
type errMissingHead struct {
	repo string
	head string
	// repoVisible distinguishes "the branch is missing" from "the whole
	// repository is invisible to this App installation" — different remedies.
	repoVisible bool
	// cause is the compare error that triggered the diagnosis; Unwrap exposes
	// it so the 404 keeps its identity for errors.As classification.
	cause error
}

func (e *errMissingHead) Unwrap() error { return e.cause }

func (e *errMissingHead) Error() string {
	if !e.repoVisible {
		return fmt.Sprintf(
			"cannot open a PR on %s: this hive's GitHub App cannot see that repository (404). Check that the App is installed on it and that the installation grants contents+pull_requests access — this is NOT a problem with branch %q",
			e.repo, e.head)
	}
	return fmt.Sprintf(
		"branch %q is not on %s — the commits exist only in the agent's working copy, or were pushed under another name or to another remote. Check, in order: (1) the push's own outcome (hive-push-branch writes <request>.result.json under %s; a direct `git push` prints it in the agent's pane), (2) that the git credential helper is reachable from the agent's UID (`su -s /bin/sh hive-<agent> -c 'git config --get-regexp credential'` must list /usr/local/bin/git-credential-hive.sh; it is wired system-wide in /etc/gitconfig because agents do not share the dev user's $HOME — see hivecommons/hive#5343), and (3) that the agent's scoped token file ($HIVE_AGENT_TOKEN_CACHE) exists and is readable by that UID. Push the branch and this request succeeds on retry",
		e.head, e.repo, PushBranchRequestDir)
}

// missingHeadReason renders an errMissingHead for the operator-facing surfaces
// (result file, quarantine log) and reports whether err was one.
func missingHeadReason(err error) (string, bool) {
	var missing *errMissingHead
	if !errors.As(err, &missing) {
		return "", false
	}
	return missing.Error(), true
}

// missingHeadRemoteRef names exactly what the watcher asked GitHub for when it
// concluded the head is absent: the ref, on the repository, as the App sees it.
// The agent may have pushed somewhere else (a fork, a differently named
// branch); naming the ref checked lets an operator compare it with the push.
func missingHeadRemoteRef(repo, head string) string {
	return "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(head), "refs/heads/") + " on github.com/" + strings.TrimSpace(repo)
}

// pushRelayOutcomeNone is the push outcome reported when the push relay
// (hive-push-branch, PushBranchRequestDir) has no record of the branch: the
// agent either ran `git push` itself — whose result the hive never saw — or
// never pushed at all.
const pushRelayOutcomeNone = "no push-relay record for this branch: the agent pushed directly (result not visible to the hive) or did not push"

// pushRelayOutcome reports what the hive's own push relay saw for branch, so
// the missing-head report states the push outcome rather than asserting one
// (#16969 reported "the agent's push did not authenticate" for a branch that
// arrived hours later). It reads only the relay's drop box: a queued request
// still waiting for its tick, or the newest result file naming the branch.
func pushRelayOutcome(dir, branch string) string {
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if branch == "" {
		return pushRelayOutcomeNone
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pushRelayOutcomeNone
	}
	var (
		queued  bool
		newest  PushBranchResponse
		haveRes bool
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, _, err := readUntrustedFile(filepath.Join(dir, name), requestMaxBytes)
		if err != nil {
			continue
		}
		if strings.HasSuffix(name, ".result.json") {
			var res PushBranchResponse
			if json.Unmarshal(data, &res) != nil || res.Branch != branch {
				continue
			}
			if !haveRes || res.At > newest.At {
				newest, haveRes = res, true
			}
			continue
		}
		var req PushBranchRequest
		if json.Unmarshal(data, &req) == nil && strings.TrimPrefix(req.Branch, "refs/heads/") == branch {
			queued = true
		}
	}
	switch {
	case queued:
		return "push-relay request for this branch is still queued (not yet pushed)"
	case haveRes && newest.OK:
		return fmt.Sprintf("push-relay pushed %s as %s at %s", branch, newest.SHA, newest.At)
	case haveRes:
		return fmt.Sprintf("push-relay FAILED after %d attempt(s) at %s: %s", newest.Attempts, newest.At, newest.Error)
	}
	return pushRelayOutcomeNone
}

// diagnoseCompareFailure turns a base...head comparison failure into an
// actionable error. Any failure that is not a definitive 404 is returned as-is
// wrapped with ctx — a 403, a rate limit or a 5xx must keep its own identity so
// the existing retry and rate-limit handling still recognise it.
//
// Only a 404 is investigated, and only with READ calls that the same client
// already had to be able to make for the comparison to have been attempted.
func (c *Client) diagnoseCompareFailure(ctx context.Context, owner, repo, base, head string, cause error) error {
	wrapped := fmt.Errorf("comparing %s/%s %s...%s: %w", owner, repo, base, head, cause)
	if c == nil || c.client == nil {
		return wrapped
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(cause, &ghErr) || ghErr.Response == nil ||
		ghErr.Response.StatusCode != http.StatusNotFound {
		return wrapped
	}

	// A 404 on compare has two very different causes. Ask which one.
	//
	// The repository probe comes first: if the App cannot see the repo at all,
	// the ref probe below would 404 for that reason too and we would blame a
	// branch that may well exist. On a probe error (not a 404) we fall back to
	// the wrapped original rather than assert a diagnosis we cannot support.
	if _, _, err := c.client.Repositories.Get(ctx, owner, repo); err != nil {
		if errors.As(err, &ghErr) && ghErr.Response != nil &&
			ghErr.Response.StatusCode == http.StatusNotFound {
			return &errMissingHead{repo: owner + "/" + repo, head: head, repoVisible: false, cause: cause}
		}
		return wrapped
	}

	// The repo is visible. Is the head ref really absent?
	ref := "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(head), "refs/heads/")
	if _, _, err := c.client.Git.GetRef(ctx, owner, repo, ref); err != nil {
		if errors.As(err, &ghErr) && ghErr.Response != nil &&
			ghErr.Response.StatusCode == http.StatusNotFound {
			return &errMissingHead{repo: owner + "/" + repo, head: head, repoVisible: true, cause: cause}
		}
		return wrapped
	}

	// The head ref DOES exist, so the 404 was about something else — most
	// likely the base. Say that rather than inventing a push failure.
	return fmt.Errorf(
		"comparing %s/%s %s...%s: GitHub returned 404 although branch %q exists — check that the base branch %q exists on the remote: %w",
		owner, repo, base, head, head, base, cause)
}
