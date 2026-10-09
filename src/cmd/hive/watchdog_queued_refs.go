package main

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// watchdogQueuedRefs names up to limit items of the actionable queue the
// governor counted (issues first, then PRs, in enumeration order), so the
// watchdog's "alive but not producing" alert can link what is waiting rather
// than only how much (hivecommons/hive#11223). A nil snapshot names nothing.
func watchdogQueuedRefs(act *github.ActionableResult, org string, limit int) []watchdog.QueuedRef {
	if act == nil || limit <= 0 {
		return nil
	}
	refs := make([]watchdog.QueuedRef, 0, limit)
	for _, iss := range act.Issues.Items {
		if len(refs) == limit {
			return refs
		}
		refs = append(refs, watchdog.QueuedRef{Ref: queuedItemRef(org, iss.Repo, iss.Number, iss.URL), Kind: "issue", URL: iss.URL})
	}
	for _, pr := range act.PRs.Items {
		if len(refs) == limit {
			return refs
		}
		refs = append(refs, watchdog.QueuedRef{Ref: queuedItemRef(org, pr.Repo, pr.Number, pr.URL), Kind: "pr", URL: pr.URL})
	}
	return refs
}

// queuedItemRef renders "owner/repo#N" for a queued item. The forge URL is
// preferred because it carries the real owner; otherwise the item's repo is
// qualified with the configured org, since actionable items usually carry
// org-less repo names.
func queuedItemRef(org, repo string, number int, rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && rawURL != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && (parts[2] == "issues" || parts[2] == "pull") && parts[3] == strconv.Itoa(number) {
			return parts[0] + "/" + parts[1] + "#" + parts[3]
		}
	}
	repo = strings.TrimSpace(repo)
	if !strings.Contains(repo, "/") && org != "" {
		repo = org + "/" + repo
	}
	return repo + "#" + strconv.Itoa(number)
}
