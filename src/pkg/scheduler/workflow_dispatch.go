package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Workflow-model agent dispatch, scheduler side (cncf/techdocs#395).
//
// A bound model hands some of its states to an agent profile
// (workflowmodel.Profile.Dispatch): TechDocs hands an Accepted request to
// techdocs-drafter and a Drafting PR, on the reviewer's /verify, to
// techdocs-verifier. Such an item is the profile's, not general work:
//
//   - WithholdWorkflowOwned takes it out of the set every generic lane's
//     kick is built from, so the scanner or a feature agent never grabs an
//     Accepted request before the drafter;
//   - PlanWorkflowDispatch picks, per item, the hive agent that fills the
//     profile and the idempotency key of the hand-off (item, profile,
//     state, and the triggering reply for a reply-started profile), so a
//     hand-off happens once however many cycles or restarts pass;
//   - BuildWorkflowDispatchPrompt renders what the agent is told.
//
// cmd/hive/workflow_dispatch.go wires these to the claim ledger, the kick
// dispatcher and the durable WorkflowDispatchLog.

// WorkflowDispatchLookup answers "which profile owns this item now?" for
// an enumerated issue or pull request. *github.Client implements it.
type WorkflowDispatchLookup interface {
	WorkflowDispatchForIssue(issue github.Issue) (github.WorkflowDispatch, bool)
	WorkflowDispatchForPR(pr github.PullRequest) (github.WorkflowDispatch, bool)
}

// WithholdWorkflowOwned returns actionable without the issues and pull
// requests whose bound model hands their current state to an agent
// profile, and how many it withheld. The input is never mutated: the
// result is a shallow copy with fresh item slices whenever anything is
// withheld, and actionable itself otherwise.
func WithholdWorkflowOwned(actionable *github.ActionableResult, lookup WorkflowDispatchLookup) (*github.ActionableResult, int) {
	if actionable == nil || lookup == nil {
		return actionable, 0
	}
	issues := make([]github.Issue, 0, len(actionable.Issues.Items))
	withheld := 0
	for _, issue := range actionable.Issues.Items {
		if _, owned := lookup.WorkflowDispatchForIssue(issue); owned {
			withheld++
			continue
		}
		issues = append(issues, issue)
	}
	prs := make([]github.PullRequest, 0, len(actionable.PRs.Items))
	prWithheld := 0
	for _, pr := range actionable.PRs.Items {
		if _, owned := lookup.WorkflowDispatchForPR(pr); owned {
			prWithheld++
			continue
		}
		prs = append(prs, pr)
	}
	if withheld+prWithheld == 0 {
		return actionable, 0
	}
	out := *actionable
	out.Issues.Items = issues
	out.Issues.Count -= withheld
	if out.Issues.Count < 0 {
		out.Issues.Count = 0
	}
	out.PRs.Items = prs
	out.PRs.Count -= prWithheld
	if out.PRs.Count < 0 {
		out.PRs.Count = 0
	}
	return &out, withheld + prWithheld
}

// WorkflowDispatchKey is the idempotency key of one hand-off: the item,
// the profile and the state, plus the triggering comment for a profile
// started by a reply (each new /verify is a new pass).
func WorkflowDispatchKey(d github.WorkflowDispatch, trigger *github.DispatchReply) string {
	key := d.Ref() + "|" + d.Target.Profile + "|" + d.State
	if trigger != nil && trigger.CommentID > 0 {
		key += "|" + strconv.FormatInt(trigger.CommentID, 10)
	}
	return key
}

// WorkflowDispatchCandidate is one hand-off the dispatch pass may make.
type WorkflowDispatchCandidate struct {
	Dispatch github.WorkflowDispatch
	Agent    string
	Key      string
	Trigger  *github.DispatchReply
}

// WorkflowDispatchSkip is an item the plan did not hand off, and why.
type WorkflowDispatchSkip struct {
	Ref    string
	Reason string
}

// Skip reasons.
const (
	WorkflowSkipNoAgent    = "no agent fills the profile"
	WorkflowSkipIneligible = "agent paused or not eligible"
	WorkflowSkipNoReply    = "waiting for the reply"
	WorkflowSkipReplyError = "reply lookup failed"
	WorkflowSkipDone       = "already dispatched"
)

// WorkflowDispatchPlanDeps are the decisions PlanWorkflowDispatch defers.
type WorkflowDispatchPlanDeps struct {
	// Agent resolves the hive agent that fills a profile.
	Agent func(profile string) (string, bool)
	// Eligible reports whether the agent may be kicked now (not paused,
	// governor gate open). Nil admits every agent.
	Eligible func(agent string) bool
	// Reply returns the newest reply that starts a reply-triggered
	// profile on the item; it is called only for such items.
	Reply func(d github.WorkflowDispatch) (github.DispatchReply, bool, error)
	// Done reports whether a key was already dispatched (durably or in
	// flight).
	Done func(key string) bool
}

// PlanWorkflowDispatch decides which owned items to hand off now. Items
// are de-duplicated by reference (an item seen twice is planned once) and
// returned in reference order, so the plan is deterministic.
func PlanWorkflowDispatch(items []github.WorkflowDispatch, deps WorkflowDispatchPlanDeps) ([]WorkflowDispatchCandidate, []WorkflowDispatchSkip) {
	seen := map[string]bool{}
	sorted := append([]github.WorkflowDispatch(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ref() < sorted[j].Ref() })
	var out []WorkflowDispatchCandidate
	var skips []WorkflowDispatchSkip
	for _, d := range sorted {
		ref := d.Ref()
		if seen[ref] {
			continue
		}
		seen[ref] = true
		agent, ok := "", false
		if deps.Agent != nil {
			agent, ok = deps.Agent(d.Target.Profile)
		}
		if !ok {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipNoAgent})
			continue
		}
		if deps.Eligible != nil && !deps.Eligible(agent) {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipIneligible})
			continue
		}
		var trigger *github.DispatchReply
		if d.Target.Reply != "" {
			if deps.Reply == nil {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipNoReply})
				continue
			}
			r, found, err := deps.Reply(d)
			if err != nil {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipReplyError + ": " + err.Error()})
				continue
			}
			if !found {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipNoReply})
				continue
			}
			trigger = &r
		}
		key := WorkflowDispatchKey(d, trigger)
		if deps.Done != nil && deps.Done(key) {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Reason: WorkflowSkipDone})
			continue
		}
		out = append(out, WorkflowDispatchCandidate{Dispatch: d, Agent: agent, Key: key, Trigger: trigger})
	}
	return out, skips
}

// BuildWorkflowDispatchPrompt renders the kick for one hand-off. A
// non-empty template replaces the built-in prompt; ${VAR} placeholders
// (REF, REPO, NUMBER, TITLE, URL, KIND, MODEL, STATE, STATE_LABEL, PROFILE,
// AGENT, TRIGGER, INSTRUCTIONS, DELIVERABLE) are expanded in either.
func BuildWorkflowDispatchPrompt(c WorkflowDispatchCandidate, template string) string {
	d := c.Dispatch
	vars := workflowPromptVars(c)
	if strings.TrimSpace(template) == "" {
		template = defaultWorkflowDispatchTemplate(d)
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "${"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(template)
}

func workflowPromptVars(c WorkflowDispatchCandidate) map[string]string {
	d := c.Dispatch
	trigger := "entering the state"
	if c.Trigger != nil {
		trigger = fmt.Sprintf("`%s` from %s (%s)", d.Target.Reply, c.Trigger.Author, c.Trigger.URL)
	}
	label := d.Target.StateLabel
	if label == "" {
		label = d.State
	}
	return map[string]string{
		"REF":          d.Ref(),
		"REPO":         d.Repo,
		"NUMBER":       strconv.Itoa(d.Number),
		"TITLE":        d.Title,
		"URL":          d.URL,
		"KIND":         workflowKindWord(d.Kind),
		"MODEL":        d.Model,
		"STATE":        d.State,
		"STATE_LABEL":  label,
		"PROFILE":      d.Target.Profile,
		"AGENT":        c.Agent,
		"TRIGGER":      trigger,
		"INSTRUCTIONS": bulletList(d.Target.Instructions),
		"DELIVERABLE":  bulletList(workflowDeliverableLines(d)),
	}
}

func workflowKindWord(k workflowmodel.ItemKind) string {
	if k == workflowmodel.KindPR {
		return "pull request"
	}
	return "issue"
}

func defaultWorkflowDispatchTemplate(d github.WorkflowDispatch) string {
	var b strings.Builder
	b.WriteString("## Workflow hand-off: ${PROFILE} for ${REF}\n\n")
	b.WriteString("${REPO} is bound to the workflow model `${MODEL}`. The ${KIND} ${REF} (\"${TITLE}\", ${URL}) is in the model state **${STATE_LABEL}** (`${STATE}`), which the model hands to the `${PROFILE}` profile. You are that profile for this item. ")
	if d.Kind == workflowmodel.KindIssue {
		b.WriteString("The hive claimed the issue for you before this kick, and no other lane is offered it.")
	} else {
		b.WriteString("No other lane is offered it.")
	}
	b.WriteString("\n\nStarted by: ${TRIGGER}.\n\n")
	if len(d.Target.Instructions) > 0 {
		b.WriteString("What this state asks for:\n${INSTRUCTIONS}\n\n")
	}
	if len(d.Target.Tools) > 0 {
		fmt.Fprintf(&b, "Tools the model grants this profile: %s.\n", strings.Join(d.Target.Tools, ", "))
	}
	if len(d.Target.Produces) > 0 {
		fmt.Fprintf(&b, "It may produce: %s.\n", strings.Join(d.Target.Produces, ", "))
	}
	b.WriteString("\nDeliverable:\n${DELIVERABLE}\n\n")
	b.WriteString("Read the ${KIND} and every comment on it first. Stop after the deliverable: each later step is a person's gate.\n")
	return b.String()
}

// workflowDeliverableLines is what the profile hands back, derived from the
// model: a pull request in the bound repository with the labels its
// target state requires, or a report on the item for a reviewing profile.
func workflowDeliverableLines(d github.WorkflowDispatch) []string {
	t := d.Target
	var lines []string
	guarded := guardedLabels(d.Profile)
	if t.Deliverable != nil {
		labels := append([]string(nil), t.Deliverable.Labels...)
		labels = append(labels, carriedLabels(d, labels)...)
		pr := "a pull request"
		if t.Deliverable.Draft {
			pr = "a DRAFT pull request"
		}
		line := fmt.Sprintf("Work on a topic branch in %s and open %s", d.Repo, pr)
		if len(labels) > 0 {
			line += " labeled " + backtickJoin(labels)
		}
		lines = append(lines, line+".")
		if d.Kind == workflowmodel.KindIssue {
			lines = append(lines,
				fmt.Sprintf("Name the issue in the PR request (`hive-open-pr --issues %d`) and reference it in the body with `Refs #%d`, not a closing keyword: the model closes the issue itself.", d.Number, d.Number))
		}
		if t.Deliverable.Draft {
			lines = append(lines, "Do not mark the pull request ready for review.")
		}
	} else {
		lines = append(lines, fmt.Sprintf("Do the work on %s and post the result as one comment on it.", d.Ref()))
		if !producesAny(t.Produces, "push", "pull-request", "pr") {
			lines = append(lines, "Do not push commits and do not open a pull request.")
		}
	}
	if len(guarded) > 0 {
		lines = append(lines, "Do not apply "+backtickJoin(guarded)+": those labels record a person's gate.")
	}
	return lines
}

// carriedLabels are the item's labels the model declares with no state of
// their own (TechDocs' phase/* labels), minus the ones already required.
func carriedLabels(d github.WorkflowDispatch, already []string) []string {
	if d.Profile == nil {
		return nil
	}
	plain := map[string]bool{}
	for _, e := range d.Profile.Labels.Entries {
		if e.State == "" {
			plain[strings.ToLower(e.Name)] = true
		}
	}
	have := map[string]bool{}
	for _, l := range already {
		have[strings.ToLower(l)] = true
	}
	for _, l := range guardedLabels(d.Profile) {
		have[strings.ToLower(l)] = true
	}
	var out []string
	for _, l := range d.Labels {
		k := strings.ToLower(l)
		if plain[k] && !have[k] {
			have[k] = true
			out = append(out, l)
		}
	}
	return out
}

func guardedLabels(p *workflowmodel.Profile) []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range p.GuardedLabels {
		if l == "" || strings.ContainsAny(l, "*?[") || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

func producesAny(produces []string, want ...string) bool {
	for _, p := range produces {
		for _, w := range want {
			if strings.EqualFold(strings.TrimSpace(p), w) {
				return true
			}
		}
	}
	return false
}

func backtickJoin(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = "`" + s + "`"
	}
	return strings.Join(q, ", ")
}

func bulletList(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- " + strings.TrimSpace(l))
	}
	return b.String()
}

// WorkflowDispatchEntry is one durable hand-off record.
type WorkflowDispatchEntry struct {
	Key     string    `json:"key"`
	Ref     string    `json:"ref"`
	Profile string    `json:"profile"`
	State   string    `json:"state"`
	Agent   string    `json:"agent"`
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at"`
}

// workflowDispatchLogMax bounds the durable log; past it the oldest
// records are dropped (an item that old is long out of its state).
const workflowDispatchLogMax = 4096

// WorkflowDispatchLog is the durable record of hand-offs, so a restart
// never repeats one. A nil log or an empty path keeps records in memory.
type WorkflowDispatchLog struct {
	path    string
	mu      sync.Mutex
	entries map[string]WorkflowDispatchEntry
}

// OpenWorkflowDispatchLog loads the log at path. A missing file is an
// empty log; an unreadable or corrupt one is returned empty with the error,
// so the caller can warn and keep dispatching (a lost record can repeat a
// hand-off once; a refused open would stop them all).
func OpenWorkflowDispatchLog(path string) (*WorkflowDispatchLog, error) {
	l := &WorkflowDispatchLog{path: path, entries: map[string]WorkflowDispatchEntry{}}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("reading workflow dispatch log %s: %w", path, err)
	}
	var list []WorkflowDispatchEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return l, fmt.Errorf("parsing workflow dispatch log %s: %w", path, err)
	}
	for _, e := range list {
		if e.Key != "" {
			l.entries[e.Key] = e
		}
	}
	return l, nil
}

// Done reports whether key was dispatched.
func (l *WorkflowDispatchLog) Done(key string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.entries[key]
	return ok
}

// Record stores e and persists the log atomically.
func (l *WorkflowDispatchLog) Record(e WorkflowDispatchEntry) error {
	if l == nil || e.Key == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[e.Key] = e
	list := make([]WorkflowDispatchEntry, 0, len(l.entries))
	for _, v := range l.entries {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].At.Equal(list[j].At) {
			return list[i].At.After(list[j].At)
		}
		return list[i].Key < list[j].Key
	})
	if len(list) > workflowDispatchLogMax {
		for _, v := range list[workflowDispatchLogMax:] {
			delete(l.entries, v.Key)
		}
		list = list[:workflowDispatchLogMax]
	}
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o750); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Entries returns the records, newest first.
func (l *WorkflowDispatchLog) Entries() []WorkflowDispatchEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]WorkflowDispatchEntry, 0, len(l.entries))
	for _, v := range l.entries {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}
