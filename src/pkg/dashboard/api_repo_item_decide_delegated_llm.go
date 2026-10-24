package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// The LLM step of "Let hive decide" (hivecommons/hive#13985). The rules in
// delegatedecision stay authoritative: only an item they leave at the
// reversible default is shown to the owner-delegate decider turn, and its
// validated {verdict, rationale, actions} goes through the same ApplyRails
// and executeDelegatedDecision as a rule decision. Any failure keeps the
// reversible default and is noted on the job. The step is gated by
// decisions.delegate.llm.enabled (default off) and decisions.delegate.llm.model
// overrides the reviewer model used for the decider turn.

const (
	delegatedLLMMaxComments = 50
	delegatedLLMMaxLinked   = 10
)

var delegatedItemRefRE = regexp.MustCompile(`(?:^|[^\w/#])#(\d{1,7})\b`)

// delegateDecider is the decider turn runner when the LLM step is enabled
// and one is wired; nil otherwise.
func (s *Server) delegateDecider() delegatedecision.TurnRunner {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.DelegateDecider == nil {
		return nil
	}
	if !s.deps.Config.Decisions.Delegate.LLMEnabled() {
		return nil
	}
	return s.deps.DelegateDecider
}

// decideDelegated is delegatedecision.Run, with the decider turn consulted
// when the rules return the reversible default. The notes say whether the
// model decided or why it did not.
func (s *Server) decideDelegated(ctx context.Context, in delegatedecision.Input) (delegatedecision.Decision, []string) {
	runner := s.delegateDecider()
	if runner == nil || !delegatedecision.IsReversibleDefault(delegatedecision.Decide(in)) {
		return delegatedecision.Run(in), nil
	}
	d, outcome, err := delegatedecision.RunWithLLM(ctx, in, s.gatherDelegatedLLMContext(ctx, in), runner)
	switch outcome {
	case delegatedecision.LLMAccepted:
		return d, []string{"decided by the " + delegatedecision.DeciderProfile + " turn (the rules left the reversible default)"}
	case delegatedecision.LLMFallback:
		msg := "decider turn unavailable"
		if err != nil {
			msg += ": " + err.Error()
		}
		return d, []string{msg + "; kept the rules' reversible default"}
	}
	return d, nil
}

// gatherDelegatedLLMContext reads what the decider sees beyond Input: the
// thread, sentinel findings, linked items, the repo's auto-merge setting and
// the author's association. Every read is best-effort; a failed read leaves
// its field empty rather than failing the job.
func (s *Server) gatherDelegatedLLMContext(ctx context.Context, in delegatedecision.Input) delegatedecision.LLMContext {
	var c delegatedecision.LLMContext
	if s.deps.Config != nil {
		c.AutoMerge = s.deps.Config.RepoAutoMergeEnabled(in.Repo)
	}
	if strings.TrimSpace(in.SentinelReason) != "" {
		c.SentinelFindings = append(c.SentinelFindings, strings.TrimSpace(in.SentinelReason))
	}
	if s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return c
	}
	api := s.deps.GHClient.GoGitHub()
	owner, repoName, _ := strings.Cut(in.Repo, "/")
	if issue, _, err := api.Issues.Get(ctx, owner, repoName, in.Number); err == nil {
		c.AuthorAssociation = issue.GetAuthorAssociation()
	}
	refText := []string{in.Body}
	comments, _, err := api.Issues.ListComments(ctx, owner, repoName, in.Number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedLLMMaxComments}})
	if err == nil {
		sentinel := strings.ToLower(s.sentinelLabel())
		for _, cm := range comments {
			body := strings.TrimSpace(cm.GetBody())
			if body == "" {
				continue
			}
			c.Thread = append(c.Thread, "@"+cm.GetUser().GetLogin()+": "+body)
			refText = append(refText, body)
			lower := strings.ToLower(body)
			if strings.Contains(lower, "sentinel") || (sentinel != "" && strings.Contains(lower, sentinel)) {
				c.SentinelFindings = append(c.SentinelFindings, body)
			}
		}
	}
	for _, n := range delegatedItemRefs(strings.Join(refText, "\n"), in.Number) {
		linked, _, err := api.Issues.Get(ctx, owner, repoName, n)
		if err != nil {
			continue
		}
		kind := "issue"
		if linked.IsPullRequest() {
			kind = "PR"
		}
		c.Linked = append(c.Linked, fmt.Sprintf("#%d %s (%s %s)", n, linked.GetTitle(), kind, linked.GetState()))
	}
	return c
}

// delegatedItemRefs lists the distinct same-repo #N references in text,
// excluding self, ascending, capped at delegatedLLMMaxLinked.
func delegatedItemRefs(text string, self int) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range delegatedItemRefRE.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n == self || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	if len(out) > delegatedLLMMaxLinked {
		out = out[:delegatedLLMMaxLinked]
	}
	return out
}
