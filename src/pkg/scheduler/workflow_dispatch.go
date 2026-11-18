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
//   - BuildWorkflowDispatchPrompt renders what the agent is told;
//   - a delivered kick is only a hand-off made, not work started
//     (workflow_dispatch_handoff.go): ReviewWorkflowHandoff decides, from
//     how the agent's turn ended and whether the deliverable showed up,
//     whether the hand-off started, and a hand-off that did not is planned
//     once more for a different agent (hivecommons/hive#18749).
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
	// ManualBy names the person who sent the item from the dashboard
	// ("Send to <profile>"); empty for an automatic hand-off.
	ManualBy string
	// AgentMode is the interaction mode Agent's session runs in
	// (ISSUES_AND_PRS, ...), which the prompt states; empty when unknown.
	AgentMode string
	// Prior is the earlier hand-off of the same key that was not started,
	// when this one is its retry.
	Prior *WorkflowDispatchEntry
}

// WorkflowDispatchSkip is an item the plan did not hand off, and why.
type WorkflowDispatchSkip struct {
	Ref string
	// Profile is the model profile the item is handed to.
	Profile string
	Reason  string
	// Replayed is the not-started hand-off whose stored reason Reason
	// repeats (WorkflowSkipNotStarted); nil for any other skip.
	Replayed *WorkflowDispatchEntry
}

// Skip reasons.
const (
	WorkflowSkipNoAgent    = "no agent fills the profile"
	WorkflowSkipIneligible = "agent paused or not eligible"
	WorkflowSkipNoReply    = "waiting for the reply"
	WorkflowSkipReplyError = "reply lookup failed"
	WorkflowSkipDone       = "already dispatched"
	// WorkflowSkipNotStarted prefixes the reason of an item whose earlier
	// hand-off was not started and that no other agent can take.
	WorkflowSkipNotStarted = "earlier hand-off not started and no other agent qualifies"
)

// WorkflowDispatchPlanDeps are the decisions PlanWorkflowDispatch defers.
type WorkflowDispatchPlanDeps struct {
	// Agent resolves the hive agent that fills the item's profile. It is
	// given the item so the pick can depend on its repository and on
	// whether the profile opens a pull request or reviews one.
	Agent func(d github.WorkflowDispatch) (string, bool)
	// Eligible reports whether the agent may be kicked now (not paused,
	// governor gate open). Nil admits every agent.
	Eligible func(agent string) bool
	// Reply returns the newest reply that starts a reply-triggered
	// profile on the item; it is called only for such items.
	Reply func(d github.WorkflowDispatch) (github.DispatchReply, bool, error)
	// Done reports whether a key was already dispatched (durably or in
	// flight). A hand-off that was not started and may still be retried
	// is not done.
	Done func(key string) bool
	// Prior returns the earlier hand-off of key that was not started and
	// may be retried automatically (WorkflowDispatchEntry.Retryable).
	Prior func(key string) (WorkflowDispatchEntry, bool)
	// RetryAgent resolves the agent for the retry of a hand-off the agents
	// in tried did not start: never one of them.
	RetryAgent func(d github.WorkflowDispatch, tried []string) (string, bool)
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
			agent, ok = deps.Agent(d)
		}
		if !ok {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipNoAgent})
			continue
		}
		if deps.Eligible != nil && !deps.Eligible(agent) {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipIneligible})
			continue
		}
		var trigger *github.DispatchReply
		if d.Target.Reply != "" {
			if deps.Reply == nil {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipNoReply})
				continue
			}
			r, found, err := deps.Reply(d)
			if err != nil {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipReplyError + ": " + err.Error()})
				continue
			}
			if !found {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipNoReply})
				continue
			}
			trigger = &r
		}
		key := WorkflowDispatchKey(d, trigger)
		if deps.Done != nil && deps.Done(key) {
			skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipDone})
			continue
		}
		var prior *WorkflowDispatchEntry
		if deps.Prior != nil {
			if p, retry := deps.Prior(key); retry {
				prior = &p
			}
		}
		if prior != nil && len(prior.Tried) > 0 {
			// The retry goes to a different agent, or to nobody.
			agent, ok = "", false
			if deps.RetryAgent != nil {
				agent, ok = deps.RetryAgent(d, prior.Tried)
			}
			if !ok {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipNotStarted + ": " + prior.Reason, Replayed: prior})
				continue
			}
			if deps.Eligible != nil && !deps.Eligible(agent) {
				skips = append(skips, WorkflowDispatchSkip{Ref: ref, Profile: d.Target.Profile, Reason: WorkflowSkipIneligible})
				continue
			}
		}
		out = append(out, WorkflowDispatchCandidate{Dispatch: d, Agent: agent, Key: key, Trigger: trigger, Prior: prior})
	}
	return out, skips
}

// BuildWorkflowDispatchPrompt renders the kick for one hand-off. A
// non-empty template replaces the built-in prompt; ${VAR} placeholders
// (REF, REPO, NUMBER, TITLE, URL, KIND, MODEL, STATE, STATE_LABEL, PROFILE,
// AGENT, AGENT_MODE, TRIGGER, INSTRUCTIONS, DELIVERABLE) are expanded in
// either.
func BuildWorkflowDispatchPrompt(c WorkflowDispatchCandidate, template string) string {
	vars := workflowPromptVars(c)
	if strings.TrimSpace(template) == "" {
		template = defaultWorkflowDispatchTemplate(c)
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
	switch {
	case c.ManualBy != "":
		trigger = fmt.Sprintf("%s, a hive owner, from the dashboard (Send to %s)", c.ManualBy, d.Target.Profile)
	case c.Trigger != nil:
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
		"AGENT_MODE":   c.AgentMode,
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

// WorkflowDeclineMarker is how a kicked agent says it will not take a
// hand-off: a final line that starts with it. It is the stand-down the kick
// outcome classifier already recognises (crux KickOutcomeStandDown), so the
// decline reaches the hand-off review with its reason.
const WorkflowDeclineMarker = "STAND DOWN"

func defaultWorkflowDispatchTemplate(c WorkflowDispatchCandidate) string {
	d := c.Dispatch
	var b strings.Builder
	b.WriteString("## Workflow hand-off: ${PROFILE} for ${REF}\n\n")
	b.WriteString("${REPO} is bound to the workflow model `${MODEL}`. The ${KIND} ${REF} (\"${TITLE}\", ${URL}) is in the model state **${STATE_LABEL}** (`${STATE}`), which the model hands to the `${PROFILE}` profile. The hive picked you to do that profile's work on this item. ")
	if d.Kind == workflowmodel.KindIssue {
		b.WriteString("The hive claimed the issue for you before this kick, and no other lane is offered it.")
	} else {
		b.WriteString("No other lane is offered it.")
	}
	b.WriteString("\n\nStarted by: ${TRIGGER}.\n")
	if c.Prior != nil && c.Prior.Agent != "" && c.Prior.Agent != c.Agent {
		fmt.Fprintf(&b, "An earlier hand-off of this item to %s was not started (%s), so it comes to you.\n", c.Prior.Agent, c.Prior.Reason)
	}
	b.WriteString("\n")
	if len(d.Target.Instructions) > 0 {
		b.WriteString("What this state asks for:\n${INSTRUCTIONS}\n\n")
	}
	b.WriteString(workflowPermissionLines(c))
	b.WriteString("\nDeliverable:\n${DELIVERABLE}\n\n")
	b.WriteString("Read the ${KIND} and every comment on it first. Stop after the deliverable: each later step is a person's gate.\n")
	return b.String()
}

// workflowPermissionLines says what the kicked session may do, and only
// that. The model's grant (its tools and outputs) describes the profile's
// role; it gives the session nothing. The session acts with its own mode,
// which the resolver checked against the deliverable before picking the
// agent, so the prompt never tells an agent to act beyond what it holds
// (hivecommons/hive#18749).
func workflowPermissionLines(c WorkflowDispatchCandidate) string {
	d := c.Dispatch
	t := d.Target
	var b strings.Builder
	var role []string
	if len(t.Tools) > 0 {
		role = append(role, "uses "+strings.Join(t.Tools, ", "))
	}
	if len(t.Produces) > 0 {
		role = append(role, "produces "+strings.Join(t.Produces, ", "))
	}
	if len(role) > 0 {
		fmt.Fprintf(&b, "The model describes the role: it %s. That is a description of the work, not a permission: it grants your session nothing.\n", strings.Join(role, " and "))
	}
	needs := "push a branch and open a pull request through `hive-open-pr`"
	if !t.ProducesChange() {
		needs = "comment on the item"
	}
	if c.AgentMode != "" {
		fmt.Fprintf(&b, "You act with your own session's permissions, nothing more. Your session runs in mode `%s`, which lets it %s; that is why the hive picked you.\n", c.AgentMode, needs)
	} else {
		fmt.Fprintf(&b, "You act with your own session's permissions, nothing more. The deliverable needs a session that may %s.\n", needs)
	}
	if t.ProducesChange() {
		// The write path is shell commands, not a tool the CLI lists: a
		// session that looked for a tool named hive-open-pr, found none and
		// stood down never ran a command (hivecommons/hive#18777).
		fmt.Fprintf(&b, "`git` and `hive-open-pr` are commands in your shell, not entries in your tool list, so do not look for a tool by that name. In a checkout of %s on a new topic branch: commit with `git commit -s`, run `git push origin <branch>`, then run `hive-open-pr --repo %s --head <branch> --title \"<title>\" --body-file <file>`", d.Repo, d.Repo)
		if d.Kind == workflowmodel.KindIssue {
			fmt.Fprintf(&b, " with `--issues %d`", d.Number)
		}
		b.WriteString(". `command -v hive-open-pr` shows where it is.\n")
	}
	// The marker is spelled as two separately quoted words so that this
	// instruction, echoed into the pane and wrapped, can never itself read
	// as a stated stand-down.
	b.WriteString("If the hive's write gates refuse what the deliverable needs, do not work around them: end your reply with one line that starts with the words `STAND` `DOWN` (upper case, one space between them), then a colon and the reason, and stop. The hive then hands the item to another agent. Stand down only after you ran the command and it was refused (quote its output); an empty tool list is not a refusal.\n")
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
	// Status is one of the WorkflowHandoff* values. Empty is a record
	// written before outcomes were tracked (hivecommons/hive#18749): the
	// kick landed and nothing more is known.
	Status string `json:"status,omitempty"`
	// Reason says why a hand-off was not started.
	Reason string `json:"reason,omitempty"`
	// ReasonAt is when Reason was decided and ReasonSession the hand-off
	// session that decided it (WorkflowHandoffSession); both empty on a
	// record written before provenance was kept.
	ReasonAt      *time.Time `json:"reason_at,omitempty"`
	ReasonSession string     `json:"reason_session,omitempty"`
	// Attempts counts the hand-offs of this key to an agent that could do
	// the work; Tried names those agents. A hand-off to an agent that may
	// not do what the profile needs counts for neither.
	Attempts int      `json:"attempts,omitempty"`
	Tried    []string `json:"tried,omitempty"`
	// CheckedAt is when Status was last decided.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
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

// Done reports whether key was dispatched and needs no further hand-off: a
// record exists and it is not a hand-off that was not started and may still
// be retried (WorkflowDispatchEntry.Retryable).
func (l *WorkflowDispatchLog) Done(key string) bool {
	e, ok := l.Get(key)
	return ok && !e.Retryable()
}

// Get returns the record of key.
func (l *WorkflowDispatchLog) Get(key string) (WorkflowDispatchEntry, bool) {
	if l == nil {
		return WorkflowDispatchEntry{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	return e, ok
}

// Record stores e and persists the log atomically.
func (l *WorkflowDispatchLog) Record(e WorkflowDispatchEntry) error {
	return l.RecordAll([]WorkflowDispatchEntry{e})
}

// RecordAll stores every entry that has a key and persists the log once,
// atomically.
func (l *WorkflowDispatchLog) RecordAll(entries []WorkflowDispatchEntry) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	stored := 0
	for _, e := range entries {
		if e.Key == "" {
			continue
		}
		l.entries[e.Key] = e
		stored++
	}
	if stored == 0 {
		return nil
	}
	return l.persistLocked()
}

// persistLocked trims the log to workflowDispatchLogMax (oldest first) and
// writes it atomically. l.mu is held.
func (l *WorkflowDispatchLog) persistLocked() error {
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

// qualifyDispatchRef qualifies the repository of an "repo#N" reference (or
// of a key that opens with one) with org. A reference that already names an
// owner, or has no "#", is returned unchanged, so the rewrite is idempotent.
func qualifyDispatchRef(org, s string) string {
	hash := strings.Index(s, "#")
	if org == "" || hash <= 0 || strings.Contains(s[:hash], "/") {
		return s
	}
	return org + "/" + s
}

// mergeMigratedEntries picks the record two spellings of one key collapse
// to. A hand-off that was made (pending or started) is never displaced by a
// not-started one; otherwise the newest record (At) wins. Attempts and the
// agents tried are carried over from both so a retry limit is not reset.
func mergeMigratedEntries(a, b WorkflowDispatchEntry) WorkflowDispatchEntry {
	win, lose := a, b
	switch {
	case a.Status == WorkflowHandoffNotStarted && b.Status != WorkflowHandoffNotStarted:
		win, lose = b, a
	case b.Status == WorkflowHandoffNotStarted && a.Status != WorkflowHandoffNotStarted:
	case b.At.After(a.At):
		win, lose = b, a
	}
	if lose.Attempts > win.Attempts {
		win.Attempts = lose.Attempts
	}
	tried := append([]string(nil), win.Tried...)
	for _, t := range lose.Tried {
		tried = appendUnique(tried, t)
	}
	win.Tried = tried
	return win
}

// MigrateQualifiedRefs rewrites records stored under the short repository
// form ("techdocs#395|...") to the canonical qualified one
// ("cncf/techdocs#395|..."), the form github.WorkflowDispatch.Ref now
// builds on every path. Before it, the owner's manual hand-off wrote the
// qualified key and the governor's pass the short one, so neither saw the
// other's record. Where both forms exist for one item the records collapse
// by mergeMigratedEntries. It is idempotent (a qualified record is left
// alone), bounded by the log's own size, persists only when something
// changed, and returns how many records it rewrote or merged away.
func (l *WorkflowDispatchLog) MigrateQualifiedRefs(org string) (int, error) {
	if l == nil || org == "" {
		return 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := 0
	out := make(map[string]WorkflowDispatchEntry, len(l.entries))
	// Qualified records first, so a short one merges into them rather than
	// the other way round; map order then cannot change the result.
	keys := make([]string, 0, len(l.entries))
	for k := range l.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for pass := 0; pass < 2; pass++ {
		for _, k := range keys {
			e := l.entries[k]
			nk := qualifyDispatchRef(org, k)
			if (nk != k) != (pass == 1) {
				continue
			}
			if nk != k {
				e.Key, e.Ref = nk, qualifyDispatchRef(org, e.Ref)
				changed++
			}
			if prev, dup := out[nk]; dup {
				e = mergeMigratedEntries(prev, e)
			}
			out[nk] = e
		}
	}
	if changed == 0 {
		return 0, nil
	}
	l.entries = out
	return changed, l.persistLocked()
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
