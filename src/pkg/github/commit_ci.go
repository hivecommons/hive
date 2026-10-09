package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"gopkg.in/yaml.v3"
)

const (
	requiredChecksForbiddenTTLEnv     = "HIVE_REQUIRED_CHECKS_FORBIDDEN_TTL"
	defaultRequiredChecksForbiddenTTL = time.Hour
)

type requiredChecksForbiddenCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	now     func() time.Time
	ttl     time.Duration
}

var sharedRequiredChecksForbiddenCache = newRequiredChecksForbiddenCache()

func newRequiredChecksForbiddenCache() *requiredChecksForbiddenCache {
	return &requiredChecksForbiddenCache{
		entries: map[string]time.Time{},
		now:     time.Now,
		ttl:     durationFromEnv(requiredChecksForbiddenTTLEnv, defaultRequiredChecksForbiddenTTL),
	}
}

func (c *requiredChecksForbiddenCache) get(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.entries[key]
	if !ok {
		return false
	}
	if c.now().After(until) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *requiredChecksForbiddenCache) put(key string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[key] = c.now().Add(c.ttl)
	c.mu.Unlock()
}

// CommitCIState is the shared CI-evaluation result behind the self-merge
// sweep's commitGreen (pkg/github/automerge) and the merge-request watcher's
// positive-confirmation gate (merge_ci_gate.go, #6173). It walks the commit
// statuses and check runs on a SHA exactly as commitGreen always has, and
// additionally reports HOW MUCH evidence it saw and which required checks
// never reported at all, so a caller that must fail closed on "no CI" can do
// so without a second round of API calls.
//
// It lives in this package rather than in automerge because automerge imports
// this package (the transport, the ignore lists) and the watcher lives here;
// a core both can call has to sit below both.
type CommitCIState struct {
	// Green is true when every gating status/check run has succeeded (or is
	// ignorable). Reason names the first blocker when false: "status-pending",
	// "check-pending", "status-<state>" or "check-<conclusion>". Zero statuses
	// and zero check runs is Green HERE (the sweep runs behind GitHub's own
	// mergeable gate); only the watcher treats that as unverified.
	Green  bool
	Reason string
	// Evidence counts every commit status and check run observed on the SHA,
	// gating or not, in any state. Zero means GitHub has NOTHING to say about
	// this commit: no workflow ever produced a job for it.
	Evidence int
	// MissingRequired lists the config/protection-declared required check
	// names (when that set is known) for which NO status and NO check run
	// exists on the SHA. A required check that has not been created yet is
	// "expected" in GitHub's vocabulary: not failed, not passed, not present.
	MissingRequired []string
}

// CommitCIOptions carries policy facts that are known to callers before the
// commit-status/check-run walk starts.
type CommitCIOptions struct {
	Required                map[string]bool
	RequiredKnown           bool
	RequiredKnownFromConfig bool
	ExpectedChecks          map[string]bool
	MinHeadAge              time.Duration
	HeadPushedAt            time.Time
	Now                     func() time.Time
}

func RequiredStatusCheckContexts(ctx context.Context, client *gh.Client, owner, repo, branch string, configSet map[string]bool, configKnown bool) (map[string]bool, bool) {
	required, known, _, _ := RequiredStatusCheckContextsDetailed(ctx, client, owner, repo, branch, configSet, configKnown)
	return required, known
}

// RequiredStatusCheckContextsDetailed returns the branch-protection-required
// status/check contexts. GitHub branch protection is authoritative; the
// config set is only a fallback when protection cannot be read.
func RequiredStatusCheckContextsDetailed(ctx context.Context, client *gh.Client, owner, repo, branch string, configSet map[string]bool, configKnown bool) (required map[string]bool, known bool, fromConfig bool, fallback bool) {
	required, known, fromConfig, fallback, _ = RequiredStatusCheckContextsDetailedWithSource(ctx, client, owner, repo, branch, configSet, configKnown)
	return required, known, fromConfig, fallback
}

func RequiredStatusCheckContextsDetailedWithSource(ctx context.Context, client *gh.Client, owner, repo, branch string, configSet map[string]bool, configKnown bool) (required map[string]bool, known bool, fromConfig bool, fallback bool, source string) {
	if client == nil || strings.TrimSpace(branch) == "" {
		if configKnown {
			return configSet, true, true, true, "config"
		}
		return nil, false, false, false, "unknown"
	}
	cacheKey := requiredChecksForbiddenCacheKey(client, owner, repo, branch)
	if sharedRequiredChecksForbiddenCache.get(cacheKey) {
		if configKnown {
			return configSet, true, true, true, "config"
		}
		return nil, false, false, false, "unknown"
	}
	rsc, _, err := client.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
	if err != nil {
		if errors.Is(err, gh.ErrBranchNotProtected) {
			return map[string]bool{}, true, false, false, "rest-unprotected"
		}
		if isRequiredChecksForbiddenCacheable(err) {
			if !configKnown && graphQLProtectionFallbackAllowed(client) {
				if gqlRequired, gqlKnown, gqlErr := requiredStatusChecksFromGraphQL(ctx, client, owner, repo, branch); gqlErr == nil && gqlKnown {
					return gqlRequired, true, false, false, "graphql"
				}
			}
			sharedRequiredChecksForbiddenCache.put(cacheKey)
		}
		if configKnown {
			return configSet, true, true, true, "config"
		}
		return nil, false, false, false, "unknown"
	}
	if rsc == nil {
		return map[string]bool{}, true, false, false, "rest"
	}
	required = make(map[string]bool)
	if rsc.Contexts != nil {
		for _, name := range *rsc.Contexts {
			required[name] = true
		}
	}
	if rsc.Checks != nil {
		for _, check := range *rsc.Checks {
			if check == nil {
				continue
			}
			required[check.Context] = true
		}
	}
	return required, true, false, false, "rest"
}

func graphQLProtectionFallbackAllowed(client *gh.Client) bool {
	if client == nil || client.BaseURL == nil {
		return true
	}
	host := strings.ToLower(client.BaseURL.Hostname())
	return host != "127.0.0.1" && host != "localhost" && host != "::1"
}

func requiredStatusChecksFromGraphQL(ctx context.Context, client *gh.Client, owner, repo, branch string) (map[string]bool, bool, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return nil, false, nil
	}
	const query = `query($owner:String!,$name:String!,$branch:String!){
repository(owner:$owner,name:$name){
  branchProtectionRules(first:100){
    nodes{
      pattern
      requiresStatusChecks
      matchingRefs(first:20, query:$branch){nodes{name}}
      requiredStatusCheckContexts
      requiredStatusChecks{context app{id slug name}}
    }
  }
}}`
	payload := map[string]any{
		"query": query,
		"variables": map[string]any{
			"owner":  owner,
			"name":   repo,
			"branch": branch,
		},
	}
	req, err := client.NewRequest("POST", graphQLEndpoint(client.BaseURL), payload)
	if err != nil {
		return nil, false, err
	}
	var resp struct {
		Data struct {
			Repository struct {
				BranchProtectionRules struct {
					Nodes []struct {
						Pattern                     string `json:"pattern"`
						RequiresStatusChecks        bool   `json:"requiresStatusChecks"`
						RequiredStatusCheckContexts []string
						RequiredStatusChecks        []struct {
							Context string `json:"context"`
						} `json:"requiredStatusChecks"`
						MatchingRefs struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"matchingRefs"`
					} `json:"nodes"`
				} `json:"branchProtectionRules"`
			} `json:"repository"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if _, err := client.Do(ctx, req, &resp); err != nil {
		return nil, false, err
	}
	if len(resp.Errors) > 0 {
		return nil, false, &graphQLErrors{Errors: resp.Errors}
	}
	for _, rule := range resp.Data.Repository.BranchProtectionRules.Nodes {
		if !branchProtectionRuleMatchesRef(rule.Pattern, branch, rule.MatchingRefs.Nodes) {
			continue
		}
		required := make(map[string]bool)
		if rule.RequiresStatusChecks {
			for _, name := range rule.RequiredStatusCheckContexts {
				if strings.TrimSpace(name) != "" {
					required[name] = true
				}
			}
			for _, check := range rule.RequiredStatusChecks {
				if strings.TrimSpace(check.Context) != "" {
					required[check.Context] = true
				}
			}
		}
		return required, true, nil
	}
	return nil, false, nil
}

func branchProtectionRuleMatchesRef(pattern, branch string, refs []struct {
	Name string `json:"name"`
}) bool {
	if strings.EqualFold(strings.TrimSpace(pattern), strings.TrimSpace(branch)) {
		return true
	}
	for _, ref := range refs {
		if strings.EqualFold(strings.TrimSpace(ref.Name), strings.TrimSpace(branch)) {
			return true
		}
	}
	return false
}

func requiredChecksForbiddenCacheKey(client *gh.Client, owner, repo, branch string) string {
	host := ""
	if client != nil && client.BaseURL != nil {
		host = strings.ToLower(client.BaseURL.Host)
	}
	clientID := fmt.Sprintf("%p", client)
	return clientID + ":" + host + ":" +
		strings.ToLower(strings.TrimSpace(owner)) + "/" +
		strings.ToLower(strings.TrimSpace(repo)) + "#" +
		strings.TrimSpace(branch)
}

func isRequiredChecksForbiddenCacheable(err error) bool {
	var er *gh.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return false
	}
	switch er.Response.StatusCode {
	case http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func resetRequiredChecksForbiddenCacheForTest(now func() time.Time, ttl time.Duration) func() {
	old := sharedRequiredChecksForbiddenCache
	sharedRequiredChecksForbiddenCache = &requiredChecksForbiddenCache{entries: map[string]time.Time{}, now: now, ttl: ttl}
	return func() { sharedRequiredChecksForbiddenCache = old }
}

// EvaluateCommitCI walks every commit status and check run on sha and
// reports the CommitCIState. required/requiredKnown come from
// RequiredStatusCheckContexts. When requiredKnown is false the fail-closed
// allowlist fallback applies: meta checks are skipped and only the
// isIgnorableCICheck names may be non-green without blocking. Later blockers
// are still walked after the first so that Evidence and MissingRequired are
// complete for the caller. A non-nil error means the evidence could not be
// gathered; Reason then names the failing API ("status-check" or
// "check-runs").
func EvaluateCommitCI(ctx context.Context, client *gh.Client, owner, repo, sha string, opts CommitCIOptions) (CommitCIState, error) {
	var st CommitCIState
	if client == nil {
		st.Reason = "status-check"
		return st, ErrNoGitHubClient
	}
	required := opts.Required
	requiredKnown := opts.RequiredKnown
	seen := make(map[string]bool)
	requiredSuccess := make(map[string]bool)
	// block records the first blocker; later ones are still walked so that
	// Evidence/seen are complete for the caller.
	block := func(reason string) {
		if st.Reason == "" {
			st.Reason = reason
		}
	}

	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			st.Reason = "status-check"
			return st, err
		}
		for _, s := range status.Statuses {
			ctxName := s.GetContext()
			st.Evidence++
			seen[ctxName] = true
			if requiredKnown {
				// Required-checks-only gating: skip anything not on the
				// branch's actual required list, no matter its state.
				if !required[ctxName] {
					continue
				}
			} else if isMetaCheck(ctxName) {
				// Fail-closed fallback path (required set unavailable).
				continue
			}
			switch s.GetState() {
			case "success":
				if requiredKnown && required[ctxName] {
					requiredSuccess[ctxName] = true
				}
			case "pending":
				if requiredKnown {
					block("required-check-pending:" + ctxName)
				} else {
					block("status-pending")
				}
			default: // "failure", "error"
				if !requiredKnown && isIgnorableCICheck(ctxName) {
					continue
				}
				if requiredKnown {
					block("required-check-failing:" + ctxName)
				} else {
					block("status-" + s.GetState())
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	checkOpts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var allCheckRuns []*gh.CheckRun
	for {
		checkRuns, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, checkOpts)
		if err != nil {
			st.Reason = "check-runs"
			return st, err
		}
		allCheckRuns = append(allCheckRuns, checkRuns.CheckRuns...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}
	for _, cr := range latestCheckRunsByNameAndApp(allCheckRuns) {
		name := cr.GetName()
		st.Evidence++
		seen[name] = true
		if requiredKnown {
			if !required[name] {
				continue
			}
		} else if isMetaCheck(name) {
			continue
		}
		if cr.GetStatus() != "completed" {
			if !requiredKnown && isIgnorableCICheck(name) {
				continue
			}
			if requiredKnown {
				block("required-check-pending:" + name)
			} else {
				block("check-pending")
			}
			continue
		}
		switch cr.GetConclusion() {
		case "success":
			if requiredKnown && required[name] {
				requiredSuccess[name] = true
			}
		case "neutral", "skipped":
		default:
			if !requiredKnown && isIgnorableCICheck(name) {
				continue
			}
			if requiredKnown {
				block("required-check-failing:" + name)
			} else {
				block("check-" + cr.GetConclusion())
			}
		}
	}

	if requiredKnown {
		for name := range required {
			if !seen[name] {
				st.MissingRequired = append(st.MissingRequired, name)
			}
		}
		sort.Strings(st.MissingRequired)
		if len(st.MissingRequired) > 0 {
			block("required-check-missing:" + st.MissingRequired[0])
		}
	}

	if !requiredKnown && st.Reason == "" {
		missingExpected := make([]string, 0)
		for name := range opts.ExpectedChecks {
			if !seen[name] {
				missingExpected = append(missingExpected, name)
			}
		}
		sort.Strings(missingExpected)
		if len(missingExpected) > 0 {
			block("pending: " + missingExpected[0] + " has not started")
		}
	}

	if st.Reason == "" && opts.MinHeadAge > 0 && !opts.HeadPushedAt.IsZero() {
		now := opts.Now
		if now == nil {
			now = time.Now
		}
		age := now().Sub(opts.HeadPushedAt)
		if age < 0 {
			age = 0
		}
		configRequiredAllGreen := opts.RequiredKnownFromConfig && len(required) > 0 && len(requiredSuccess) == len(required) && len(st.MissingRequired) == 0
		if age < opts.MinHeadAge && !configRequiredAllGreen {
			block(fmt.Sprintf("pending: head pushed %s ago (< min_head_age)", age.Round(time.Second)))
		}
	}
	st.Green = st.Reason == ""
	return st, nil
}

func commitCIReasonIsPending(reason string) bool {
	return strings.HasSuffix(reason, "-pending") || strings.HasPrefix(reason, "pending: ") || strings.HasPrefix(reason, "required-check-missing:")
}

// ExpectedCommitChecksFromRef returns the check-run names that should appear on
// a candidate commit when the required-checks set is not known. It intentionally
// ignores meta/allowed-noise checks and reference check-runs that completed as
// skipped/neutral, because those do not establish work that must start on every
// candidate head.
func ExpectedCommitChecksFromRef(ctx context.Context, client *gh.Client, owner, repo, ref string) (map[string]bool, error) {
	if client == nil {
		return nil, ErrNoGitHubClient
	}
	if strings.TrimSpace(ref) == "" {
		return nil, nil
	}
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var allCheckRuns []*gh.CheckRun
	for {
		checkRuns, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, err
		}
		if checkRuns != nil {
			allCheckRuns = append(allCheckRuns, checkRuns.CheckRuns...)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	expected := make(map[string]bool)
	for _, cr := range latestCheckRunsByNameAndApp(allCheckRuns) {
		name := cr.GetName()
		if name == "" || len(cr.PullRequests) == 0 || isMetaCheck(name) || isIgnorableCICheck(name) {
			continue
		}
		if cr.GetStatus() == "completed" {
			switch cr.GetConclusion() {
			case "neutral", "skipped":
				continue
			}
		}
		expected[name] = true
	}
	return filterPostMergeOnlyWorkflowChecks(ctx, client, owner, repo, ref, expected), nil
}

// postMergeOnlyPullRequestTypes are pull_request activity types that can only
// ever fire after a PR has already been merged or closed (#9794). A workflow
// whose sole trigger is pull_request restricted to these types can never
// produce a check run on a PR's pre-merge head, so treating one of its checks
// as "expected" deadlocks the merge CI gate waiting for something that can
// never start (hivecommons/hotshot's close-linked-issues.yml, triggered only
// by `pull_request: types: [closed]`, is the case that surfaced this).
var postMergeOnlyPullRequestTypes = map[string]bool{
	"closed": true,
}

// filterPostMergeOnlyWorkflowChecks removes any check name from expected whose
// originating workflow's only trigger is a pull_request event restricted to
// postMergeOnlyPullRequestTypes. Discovering the originating workflow and
// parsing it both call the GitHub API; any failure along that path (listing
// workflow runs/jobs, fetching the workflow file, or parsing its YAML) leaves
// the affected name(s) in expected untouched. Fail-safe here means "keep
// waiting", never "stop waiting", since a wrongly-dropped check would let the
// merge gate pass CI that never actually ran.
func filterPostMergeOnlyWorkflowChecks(ctx context.Context, client *gh.Client, owner, repo, ref string, expected map[string]bool) map[string]bool {
	if len(expected) == 0 || client == nil {
		return expected
	}
	jobPaths, err := workflowPathsForJobNames(ctx, client, owner, repo, ref, expected)
	if err != nil || len(jobPaths) == 0 {
		return expected
	}
	postMergeOnlyPath := make(map[string]bool, len(jobPaths))
	for _, path := range jobPaths {
		if _, done := postMergeOnlyPath[path]; done {
			continue
		}
		fc, _, _, cerr := client.Repositories.GetContents(ctx, owner, repo, path, &gh.RepositoryContentGetOptions{Ref: ref})
		if cerr != nil || fc == nil {
			postMergeOnlyPath[path] = false
			continue
		}
		doc, derr := fc.GetContent()
		if derr != nil {
			postMergeOnlyPath[path] = false
			continue
		}
		only, perr := workflowIsPostMergeOnly([]byte(doc))
		postMergeOnlyPath[path] = perr == nil && only
	}
	filtered := make(map[string]bool, len(expected))
	for name := range expected {
		if path, ok := jobPaths[name]; ok && postMergeOnlyPath[path] {
			continue
		}
		filtered[name] = true
	}
	return filtered
}

// workflowPathsForJobNames maps each check name in names to the repository
// path of the workflow file that produced it, by cross-referencing the
// workflow runs and jobs on ref. Names with no matching job (e.g. checks
// reported by something other than an Actions workflow) are simply absent
// from the result, which filterPostMergeOnlyWorkflowChecks treats as
// "leave unchanged".
func workflowPathsForJobNames(ctx context.Context, client *gh.Client, owner, repo, ref string, names map[string]bool) (map[string]string, error) {
	runs, _, err := client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     ref,
		ListOptions: gh.ListOptions{PerPage: workflowRunsPerPage},
	})
	if err != nil {
		return nil, err
	}
	if runs == nil || len(runs.WorkflowRuns) == 0 {
		return nil, nil
	}
	paths := make(map[string]string)
	for _, run := range runs.WorkflowRuns {
		if run == nil || run.GetPath() == "" {
			continue
		}
		remaining := false
		for name := range names {
			if _, have := paths[name]; !have {
				remaining = true
				break
			}
		}
		if !remaining {
			break
		}
		jobsOpts := &gh.ListWorkflowJobsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			jobs, resp, jerr := client.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), jobsOpts)
			if jerr != nil {
				// Leave this run's jobs unmapped rather than failing the whole
				// lookup; any of its names simply stay in the expected set.
				break
			}
			if jobs != nil {
				for _, job := range jobs.Jobs {
					if job == nil {
						continue
					}
					if name := job.GetName(); names[name] {
						paths[name] = run.GetPath()
					}
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			jobsOpts.Page = resp.NextPage
		}
	}
	return paths, nil
}

// workflowTriggerEvent is one entry of a workflow file's top-level `on:`
// block: the event name, plus its `types:` list when present (only
// pull_request's types are consulted today).
type workflowTriggerEvent struct {
	name  string
	types []string
}

// parseWorkflowTriggerEvents extracts the top-level `on:` trigger events (and,
// for pull_request, its `types:` list) from a workflow file's raw YAML. It
// supports all three shapes GitHub accepts: a bare scalar (`on: push`), a
// sequence (`on: [push, pull_request]`), and a mapping with per-event config
// (`on: {pull_request: {types: [closed]}}`).
func parseWorkflowTriggerEvents(doc []byte) ([]workflowTriggerEvent, error) {
	var root struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	node := root.On
	switch node.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		return []workflowTriggerEvent{{name: node.Value}}, nil
	case yaml.SequenceNode:
		events := make([]workflowTriggerEvent, 0, len(node.Content))
		for _, item := range node.Content {
			events = append(events, workflowTriggerEvent{name: item.Value})
		}
		return events, nil
	case yaml.MappingNode:
		events := make([]workflowTriggerEvent, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			ev := workflowTriggerEvent{name: node.Content[i].Value}
			if val := node.Content[i+1]; val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					if val.Content[j].Value == "types" && val.Content[j+1].Kind == yaml.SequenceNode {
						for _, t := range val.Content[j+1].Content {
							ev.types = append(ev.types, t.Value)
						}
					}
				}
			}
			events = append(events, ev)
		}
		return events, nil
	default:
		return nil, fmt.Errorf("unsupported \"on:\" node kind %v", node.Kind)
	}
}

// workflowIsPostMergeOnly reports whether the workflow described by doc has
// exactly one trigger, pull_request, restricted to activity types that only
// fire after a PR is already merged/closed (postMergeOnlyPullRequestTypes).
// `on: pull_request` with no explicit types (GitHub's default of opened/
// synchronize/reopened) and any workflow with an additional trigger both
// return false: this must be conservative, since a false positive here would
// silence a real, currently-waitable check.
func workflowIsPostMergeOnly(doc []byte) (bool, error) {
	events, err := parseWorkflowTriggerEvents(doc)
	if err != nil {
		return false, err
	}
	if len(events) != 1 || events[0].name != "pull_request" || len(events[0].types) == 0 {
		return false, nil
	}
	for _, t := range events[0].types {
		if !postMergeOnlyPullRequestTypes[strings.TrimSpace(t)] {
			return false, nil
		}
	}
	return true, nil
}

func ExpectedCommitChecksFromLatestMergedPR(ctx context.Context, client *gh.Client, owner, repo, base string) (map[string]bool, error) {
	ref, err := LatestMergedPRHead(ctx, client, owner, repo, base)
	if err != nil || ref == "" {
		return nil, err
	}
	return ExpectedCommitChecksFromRef(ctx, client, owner, repo, ref)
}

func LatestMergedPRHead(ctx context.Context, client *gh.Client, owner, repo, base string) (string, error) {
	if client == nil {
		return "", ErrNoGitHubClient
	}
	if strings.TrimSpace(base) == "" {
		return "", nil
	}
	opts := &gh.PullRequestListOptions{
		State:     "closed",
		Base:      base,
		Sort:      "updated",
		Direction: "desc",
		ListOptions: gh.ListOptions{
			PerPage: 30,
		},
	}
	for {
		prs, resp, err := client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return "", err
		}
		for _, pr := range prs {
			if pr == nil || pr.GetMergedAt().IsZero() {
				continue
			}
			if pr.GetHead() != nil && strings.TrimSpace(pr.GetHead().GetSHA()) != "" {
				return pr.GetHead().GetSHA(), nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return "", nil
		}
		opts.Page = resp.NextPage
	}
}

type checkRunIdentity struct {
	name  string
	appID int64
}

func latestCheckRunsByNameAndApp(checks []*gh.CheckRun) []*gh.CheckRun {
	if len(checks) == 0 {
		return nil
	}
	latest := make(map[checkRunIdentity]*gh.CheckRun, len(checks))
	for _, cr := range checks {
		if cr == nil {
			continue
		}
		key := checkRunIdentity{name: cr.GetName()}
		if app := cr.GetApp(); app != nil {
			key.appID = app.GetID()
		}
		if prev := latest[key]; prev == nil || checkRunIsNewer(cr, prev) {
			latest[key] = cr
		}
	}
	out := make([]*gh.CheckRun, 0, len(latest))
	for _, cr := range latest {
		out = append(out, cr)
	}
	// Map iteration order is randomized, so callers that build a failure
	// excerpt from the first few entries (fetchFailureExcerpt) would see a
	// different, arbitrary subset of failing checks on every call. Sort by
	// name (with app ID as a tie-break for same-named checks from different
	// apps) so the same PR head always yields the same ordering.
	sort.Slice(out, func(i, j int) bool {
		ni, nj := out[i].GetName(), out[j].GetName()
		if ni != nj {
			return ni < nj
		}
		var ai, aj int64
		if app := out[i].GetApp(); app != nil {
			ai = app.GetID()
		}
		if app := out[j].GetApp(); app != nil {
			aj = app.GetID()
		}
		return ai < aj
	})
	return out
}

func checkRunIsNewer(candidate, current *gh.CheckRun) bool {
	candidateStarted := candidate.GetStartedAt().Time
	currentStarted := current.GetStartedAt().Time
	switch {
	case candidateStarted.After(currentStarted):
		return true
	case currentStarted.After(candidateStarted):
		return false
	default:
		return candidate.GetID() > current.GetID()
	}
}
