package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Pin-only exception provenance (#16399): once the classifier accepts a diff
// as pin replacements, every App merge path (the automerge sweep lanes,
// /approve and the merge-request relay) must also prove that every commit
// behind the head is the trusted author's and that every new action SHA is
// anchored in its upstream repository. The helpers live here so the sweep and
// the relay share one implementation and can never disagree.

// pinOnlyMaxTagDepth bounds annotated-tag dereferencing (a tag of a tag).
const pinOnlyMaxTagDepth = 3

// PinOnlyWebFlowLogin is the login GitHub attaches as committer to commits it
// creates and signs itself (web UI, Apps such as Dependabot and Renovate).
const PinOnlyWebFlowLogin = "web-flow"

// pinOnlyRESTCaller labels the REST reads the provenance checks make.
const pinOnlyRESTCaller = "hive:automerge_pin_only"

// PinOnlyTrustedLogin reports whether login is one of trusted (case-insensitive,
// whitespace-trimmed). An empty login is never trusted.
func PinOnlyTrustedLogin(trusted []string, login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	for _, t := range trusted {
		if strings.EqualFold(strings.TrimSpace(t), login) {
			return true
		}
	}
	return false
}

// PinOnlyTrustedCommitter accepts GitHub's own committer identity or a
// trusted author committing (and signing) locally; the signature check then
// binds to this identity.
func PinOnlyTrustedCommitter(trusted []string, login string) bool {
	return strings.EqualFold(strings.TrimSpace(login), PinOnlyWebFlowLogin) || PinOnlyTrustedLogin(trusted, login)
}

// PinOnlyCommitsTrusted returns "" when every commit of pr is attributed to a
// trusted author, was committed by a trusted login or by GitHub itself
// (web-flow), carries a signature GitHub verified, and the current head is
// the newest listed commit. GitHub verifies a signature against the
// committer's email only, so a verified commit proves the committer, not the
// author: a write-access principal with their own signing key can craft a
// commit whose author is a bot's noreply address and sign it as themselves
// (see pr_merge_state.go on verification reasons). Requiring the committer to
// be web-flow (GitHub-created and GitHub-signed, so GitHub set the author) or
// a trusted login (a human trusted author signing locally) closes that; a
// spoofed committer email fails verification because it is not on the
// signer's account. Any listing error, an empty or truncated list, or a head
// not in the list fails closed with a reason.
func PinOnlyCommitsTrusted(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest, trusted []string) string {
	head := pr.GetHead().GetSHA()
	if head == "" {
		return "PR head SHA unknown"
	}
	ctx = WithRESTCaller(ctx, pinOnlyRESTCaller)
	all, complete, err := ListPRRepositoryCommits(ctx, client, owner, repo, pr.GetNumber())
	if err != nil {
		return fmt.Sprintf("PR commits could not be listed: %v", err)
	}
	if !complete {
		return "PR commit list too long to verify"
	}
	if len(all) == 0 {
		return "PR has no commits to verify"
	}
	if all[len(all)-1].GetSHA() != head {
		return "PR commit list does not end at the current head"
	}
	for _, rc := range all {
		sha := rc.GetSHA()
		if sha == "" {
			return "PR commit without a SHA"
		}
		if login := SafeGetLogin(rc.GetAuthor()); !PinOnlyTrustedLogin(trusted, login) {
			return fmt.Sprintf("commit %s author %q is not in trusted_authors", stagelabels.ShortSHA(sha), login)
		}
		if committer := SafeGetLogin(rc.GetCommitter()); !PinOnlyTrustedCommitter(trusted, committer) {
			return fmt.Sprintf("commit %s committer %q is neither GitHub nor in trusted_authors", stagelabels.ShortSHA(sha), committer)
		}
		if !rc.GetCommit().GetVerification().GetVerified() {
			return fmt.Sprintf("commit %s has no GitHub-verified signature", stagelabels.ShortSHA(sha))
		}
	}
	return ""
}

// PinOnlyActionPinsUpstream returns "" when every accepted action-sha pin is
// a commit of its upstream action repository. GitHub resolves a `uses:` SHA
// through the whole fork network, so a 40-hex commit pushed to any fork of
// owner/repo is a valid pin; the classifier sees syntax only. A pin with a
// version comment must be exactly what tags/<version> (dereferenced through
// annotated tags) points at; one without must be an ancestor of the upstream
// default branch. Any API error, 404 or mismatch fails closed with a reason.
// Image digests are content-addressed and need no such anchor.
func PinOnlyActionPinsUpstream(ctx context.Context, client *gh.Client, pins []intent.ActionPin) string {
	if len(pins) == 0 {
		return ""
	}
	ctx = WithRESTCaller(ctx, pinOnlyRESTCaller)
	for _, pin := range pins {
		if why := pinOnlyActionPinUpstream(ctx, client, pin); why != "" {
			return fmt.Sprintf("action %s@%s: %s", pin.Repo, stagelabels.ShortSHA(pin.SHA), why)
		}
	}
	return ""
}

func pinOnlyActionPinUpstream(ctx context.Context, client *gh.Client, pin intent.ActionPin) string {
	owner, repo, ok := strings.Cut(pin.Repo, "/")
	if !ok || owner == "" || repo == "" || pin.SHA == "" {
		return "upstream repository or commit unknown"
	}
	if pin.Version != "" {
		ref, _, err := client.Git.GetRef(ctx, owner, repo, "tags/"+pin.Version)
		if err != nil {
			return fmt.Sprintf("tag %s could not be resolved upstream: %v", pin.Version, err)
		}
		obj := ref.GetObject()
		for depth := 0; obj.GetType() == "tag"; depth++ {
			if depth == pinOnlyMaxTagDepth {
				return fmt.Sprintf("tag %s nests too deeply", pin.Version)
			}
			tag, _, err := client.Git.GetTag(ctx, owner, repo, obj.GetSHA())
			if err != nil {
				return fmt.Sprintf("annotated tag %s could not be resolved upstream: %v", pin.Version, err)
			}
			obj = tag.GetObject()
		}
		if obj.GetType() != "commit" || !strings.EqualFold(obj.GetSHA(), pin.SHA) {
			return fmt.Sprintf("upstream tag %s does not point at the pinned commit", pin.Version)
		}
		return ""
	}
	upstream, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Sprintf("upstream repository could not be read: %v", err)
	}
	branch := upstream.GetDefaultBranch()
	if branch == "" {
		return "upstream default branch unknown"
	}
	// base=pin, head=default: "ahead" or "identical" means the pinned commit
	// is an ancestor of (reachable from) the upstream default branch.
	cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, pin.SHA, branch, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return fmt.Sprintf("pinned commit could not be compared with upstream %s: %v", branch, err)
	}
	if status := cmp.GetStatus(); status != "ahead" && status != "identical" {
		return fmt.Sprintf("pinned commit is not on upstream %s (compare status %q)", branch, status)
	}
	return ""
}
