package github

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Label-driven approval (hivecommons/hive#11313).
//
// On a repo with project.repo_policies[].label_driven set, the organisation's
// own acceptance label (`triage/accepted` unless configured) approves work for
// the hive the way `/hive approve` does: Hive records the approval
// (`approved-direction`), removes the park labels it added itself, and leaves
// every other label to the organisation's people and bots. Nothing in this
// file runs for a repo that has not opted in: every entry point first asks the
// policy callback, and a nil callback or a false answer is a no-op.

// LabelApprovalPolicy is one repo's label-driven approval configuration, as
// resolved by config.LabelDrivenApprovalLabels and friends.
type LabelApprovalPolicy struct {
	// Labels are the acceptance labels whose trusted addition approves an
	// issue. Empty disables label approval for the repo.
	Labels []string
	// TrustOrgMembers lets public members of the repo's owning organisation
	// approve by label, which is Prow's own rule for `triage/accepted`.
	TrustOrgMembers bool
	// TrustedBots are bot logins whose label additions count: the
	// organisation's command bot has already applied its own authorisation.
	TrustedBots []string
}

// Enabled reports whether the policy maps at least one label to approval.
func (p LabelApprovalPolicy) Enabled() bool { return len(p.Labels) > 0 }

func (p LabelApprovalPolicy) hasLabel(name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

func (p LabelApprovalPolicy) trustedBot(login string) bool {
	for _, b := range p.TrustedBots {
		if strings.EqualFold(strings.TrimSpace(b), strings.TrimSpace(login)) {
			return true
		}
	}
	return false
}

// labelApprovalRoles are the collaborator `role_name` values that may approve
// by label: GitHub's triage role and everything above it. The legacy
// `permission` field reports triage as "read", which is why it is not used.
var labelApprovalRoles = map[string]bool{"triage": true, "write": true, "maintain": true, "admin": true}

// SetLabelApprovalPolicy installs the live per-repo resolver for label-driven
// approval. ok=false (or a nil fn) means the repo did not opt in and every
// label-approval path stays off for it.
func (c *Client) SetLabelApprovalPolicy(fn func(repo string) (LabelApprovalPolicy, bool)) {
	if c == nil {
		return
	}
	c.labelApprovalMu.Lock()
	defer c.labelApprovalMu.Unlock()
	c.labelApprovalPolicy = fn
}

func (c *Client) labelApprovalPolicyFor(repo string) (LabelApprovalPolicy, bool) {
	if c == nil {
		return LabelApprovalPolicy{}, false
	}
	c.labelApprovalMu.RLock()
	fn := c.labelApprovalPolicy
	c.labelApprovalMu.RUnlock()
	if fn == nil {
		return LabelApprovalPolicy{}, false
	}
	p, ok := fn(repo)
	if !ok || !p.Enabled() {
		return LabelApprovalPolicy{}, false
	}
	return p, true
}

// isHiveAccount reports whether a login is one of this hive's own accounts:
// the GitHub App bot, the configured ai_author, or an additional hive bot
// login. It is narrower than !isHumanAuthor, which is also true for every
// other bot; a park label another organisation's bot added is not Hive's.
// It also matches the login resolved from GET /user (see resolveSelfLogin),
// but never triggers that lookup itself.
func (c *Client) isHiveAccount(user *gh.User) bool {
	login := strings.TrimSpace(user.GetLogin())
	if login == "" || c == nil {
		return false
	}
	if isHiveBotLogin(c.hiveBotLogins(), login) {
		return true
	}
	if self := c.cachedSelfLogin(); self != "" && strings.EqualFold(self, login) {
		return true
	}
	return c.getHiveIdentity().Matches(login)
}

// knowsOwnAccounts reports whether the client can name at least one of its
// own accounts, either from configuration or from its own login resolved via
// GET /user when none is configured. Checks that require hive authorship fall
// back to the legacy any-author rule only when neither is available, so a PAT
// hive that cannot reach /user keeps answering each `/hive` comment once
// rather than every cycle.
func (c *Client) knowsOwnAccounts() bool {
	if c == nil {
		return false
	}
	if len(c.hiveBotLogins()) > 0 {
		return true
	}
	id := c.getHiveIdentity()
	if id.AIAuthor != "" || id.AppLogin != "" {
		return true
	}
	return c.resolveSelfLogin(context.Background()) != ""
}

// issueLabelEvent is one `labeled` or `unlabeled` entry from an issue's
// timeline.
type issueLabelEvent struct {
	ID    int64
	Event string
	Label string
	Actor *gh.User
	At    time.Time
}

// labelTimelinePageSize is the per-page size for timeline reads; the sweep
// paginates, so this only bounds the number of calls per issue.
const labelTimelinePageSize = 100

// listIssueLabelEvents returns the issue's label events oldest first. Every
// other timeline entry is dropped.
func (c *Client) listIssueLabelEvents(ctx context.Context, owner, repo string, number int) ([]issueLabelEvent, error) {
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	var out []issueLabelEvent
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("reading timeline of %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, ev := range page {
			if ev == nil {
				continue
			}
			kind := strings.ToLower(ev.GetEvent())
			if kind != "labeled" && kind != "unlabeled" {
				continue
			}
			out = append(out, issueLabelEvent{
				ID:    ev.GetID(),
				Event: kind,
				Label: ev.GetLabel().GetName(),
				Actor: ev.GetActor(),
				At:    ev.GetCreatedAt().Time,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// lastLabeledEvent returns the most recent `labeled` event for label. An
// unlabel-then-relabel therefore evaluates only the newest addition.
func lastLabeledEvent(events []issueLabelEvent, label string) (issueLabelEvent, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Event == "labeled" && strings.EqualFold(strings.TrimSpace(ev.Label), strings.TrimSpace(label)) {
			return ev, true
		}
	}
	return issueLabelEvent{}, false
}

// labelApplierTrusted decides whether the account that added an acceptance
// label may approve for the hive. The tiers, in order: one of the hive's own
// accounts never counts (A16); an allowlisted bot counts; any other bot does
// not; a collaborator whose role_name is triage or above counts; a public
// member of the owning organisation counts when the policy allows it. A
// lookup failure is returned, not read as trust: not knowing is never
// permission.
func (c *Client) labelApplierTrusted(ctx context.Context, owner, repo string, actor *gh.User, policy LabelApprovalPolicy) (string, bool, error) {
	login := strings.TrimSpace(actor.GetLogin())
	if login == "" {
		return "", false, nil
	}
	if c.isHiveAccount(actor) {
		return "", false, nil
	}
	if policy.trustedBot(login) {
		return "trusted bot", true, nil
	}
	if strings.EqualFold(actor.GetType(), "Bot") || strings.HasSuffix(login, "[bot]") {
		return "", false, nil
	}
	level, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil && !githubStatusError(err, http.StatusNotFound) {
		return "", false, fmt.Errorf("permission lookup for %s on %s/%s: %w", login, owner, repo, err)
	}
	if err == nil {
		role := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
		if labelApprovalRoles[role] {
			return role + " collaborator", true, nil
		}
	}
	if policy.TrustOrgMembers {
		member, _, err := c.client.Organizations.IsMember(ctx, owner, login)
		if err != nil {
			return "", false, fmt.Errorf("org membership lookup for %s in %s: %w", login, owner, err)
		}
		if member {
			return "member of " + owner, true, nil
		}
	}
	return "", false, nil
}

// labelApprovalFinding is what one issue's timeline says about label-driven
// approval.
type labelApprovalFinding struct {
	// Accepted is true when the issue carries an acceptance label whose
	// newest addition was made by a trusted account.
	Accepted bool
	// Label and Actor identify that acceptance; TrustReason says why the
	// actor counted.
	Label       string
	Actor       string
	TrustReason string
	// At is when the acceptance label was added.
	At time.Time
	// AcceptanceID is the timeline event ID, used in the once-only marker.
	AcceptanceID int64
	// HiveParkLabels are the park labels the hive itself added before the
	// acceptance, which the acceptance may remove.
	HiveParkLabels []string
	// LaterHiveParkLabels are park labels the hive added after the
	// acceptance; they stay (R14).
	LaterHiveParkLabels []string
	// KeptParkLabels are hive-added park labels no approval lifts:
	// `needs-human` (unparkRemovableLabels). The approval is still recorded,
	// but the issue is not released while one is present.
	KeptParkLabels []string
	// BlockingParkLabels are park labels whose newest addition was not by the
	// hive (a person, another bot, or unreadable). While any is present the
	// acceptance withholds approval (R3, A4, A29).
	BlockingParkLabels []labelAttribution
	// Holds lists labels containing "hold", which are never touched.
	Holds []labelAttribution
	// Acted is true when the timeline already shows the hive acting on this
	// acceptance (an `approved-direction` it added after the acceptance).
	Acted bool
}

// labelAttribution names a label and who last added it ("unknown" when the
// timeline does not say).
type labelAttribution struct {
	Label string
	Actor string
}

func (a labelAttribution) String() string {
	if a.Actor == "" {
		return fmt.Sprintf("`%s` (added by an account Hive could not read)", a.Label)
	}
	return fmt.Sprintf("`%s` (added by @%s)", a.Label, a.Actor)
}

// Released reports whether acting on the finding leaves the issue with no
// park label, so agents may pick it up.
func (f labelApprovalFinding) Released() bool {
	return f.Accepted && len(f.BlockingParkLabels) == 0 && len(f.LaterHiveParkLabels) == 0 && len(f.KeptParkLabels) == 0
}

// Blocked reports whether a non-Hive park label withholds the approval.
func (f labelApprovalFinding) Blocked() bool { return len(f.BlockingParkLabels) > 0 }

// evaluateLabelApproval reads one issue's timeline and reports whether a
// trusted acceptance is present and what it may change. It never writes.
// The label must still be on the issue (R11: a withdrawn acceptance is not
// acted on) and only the newest `labeled` event for it is considered.
func (c *Client) evaluateLabelApproval(ctx context.Context, owner, repo string, issue *gh.Issue, policy LabelApprovalPolicy) (labelApprovalFinding, error) {
	var f labelApprovalFinding
	labels := issueLabelNames(issue.Labels)
	var present []string
	for _, l := range labels {
		if policy.hasLabel(l) {
			present = append(present, l)
		}
	}
	if len(present) == 0 {
		return f, nil
	}
	events, err := c.listIssueLabelEvents(ctx, owner, repo, issue.GetNumber())
	if err != nil {
		return f, err
	}
	// Newest trusted acceptance among the configured labels present.
	for _, name := range present {
		ev, ok := lastLabeledEvent(events, name)
		if !ok {
			continue
		}
		if f.Accepted && !ev.At.After(f.At) {
			continue
		}
		reason, trusted, err := c.labelApplierTrusted(ctx, owner, repo, ev.Actor, policy)
		if err != nil {
			return labelApprovalFinding{}, err
		}
		if !trusted {
			continue
		}
		f.Accepted = true
		f.Label = ev.Label
		f.Actor = safeGetLogin(ev.Actor)
		f.TrustReason = reason
		f.At = ev.At
		f.AcceptanceID = ev.ID
	}
	if !f.Accepted {
		return f, nil
	}
	for _, name := range labels {
		lower := strings.ToLower(strings.TrimSpace(name))
		isPark := isIssueParkingLabel(lower)
		isHold := strings.Contains(lower, "hold")
		if !isPark && !isHold {
			continue
		}
		ev, ok := lastLabeledEvent(events, name)
		attr := labelAttribution{Label: name}
		if ok {
			attr.Actor = safeGetLogin(ev.Actor)
		}
		if isHold {
			f.Holds = append(f.Holds, attr)
			continue
		}
		switch {
		case !ok || !c.isHiveAccount(ev.Actor):
			f.BlockingParkLabels = append(f.BlockingParkLabels, attr)
		case isUnparkKeptLabel(name):
			f.KeptParkLabels = append(f.KeptParkLabels, name)
		case ev.At.After(f.At):
			f.LaterHiveParkLabels = append(f.LaterHiveParkLabels, name)
		default:
			f.HiveParkLabels = append(f.HiveParkLabels, name)
		}
	}
	if ack, ok := lastLabeledEvent(events, HumanAckLabel); ok && c.isHiveAccount(ack.Actor) && !ack.At.Before(f.At) {
		f.Acted = true
	}
	return f, nil
}

// labelApprovalMarkerPrefix tags the comment the hive posts when an
// acceptance releases or partially releases a parked issue. The acceptance
// event ID is appended so each acceptance is acted on once (R13). Only a
// marker written by one of the hive's own accounts counts (A13).
const labelApprovalMarkerPrefix = "<!-- hive:label-approval:11313 event="

func labelApprovalMarker(eventID int64) string {
	return fmt.Sprintf("%s%d -->", labelApprovalMarkerPrefix, eventID)
}

// labelApprovalAlreadyCommented reports whether a hive-authored comment
// carries the marker for this acceptance event.
func (c *Client) labelApprovalAlreadyCommented(comments []*gh.IssueComment, eventID int64) bool {
	marker := labelApprovalMarker(eventID)
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), marker) {
			continue
		}
		if c.markerAuthorTrusted(cm.GetUser()) {
			return true
		}
	}
	return false
}

// markerAuthorTrusted reports whether a once-only marker found in a comment
// may be believed: the comment must come from one of the hive's own accounts.
// The legacy any-author rule applies only when the hive has no identity
// configured and could not resolve its own login either.
func (c *Client) markerAuthorTrusted(user *gh.User) bool {
	if !c.knowsOwnAccounts() {
		return true
	}
	return c.isHiveAccount(user)
}

// renderLabelApprovalReply is the comment posted when an acceptance acts on a
// parked issue (R7). No line starts with a bot command (R18): the labels are
// quoted inline.
func renderLabelApprovalReply(f labelApprovalFinding, removed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approved via `%s`, added by @%s (%s).", f.Label, f.Actor, f.TrustReason)
	if len(removed) > 0 {
		fmt.Fprintf(&b, " Removed Hive's own park labels %s and applied `%s`.", quoteLabels(removed), HumanAckLabel)
	} else {
		fmt.Fprintf(&b, " `%s` is applied; no label Hive added before the acceptance remained to remove.", HumanAckLabel)
	}
	var kept []string
	for _, l := range f.LaterHiveParkLabels {
		kept = append(kept, fmt.Sprintf("`%s` (added by Hive after the acceptance; answer it with `/hive approve` or `/hive decision <option>`)", l))
	}
	for _, l := range f.KeptParkLabels {
		kept = append(kept, fmt.Sprintf("`%s` (held for a person; only a person removes it)", l))
	}
	for _, a := range f.Holds {
		kept = append(kept, a.String())
	}
	if len(kept) > 0 {
		fmt.Fprintf(&b, "\n\nLeft in place: %s.", strings.Join(kept, ", "))
	}
	if f.Released() && len(f.Holds) == 0 {
		b.WriteString("\n\nThis issue is now released: Hive goes with its recommendation. To choose another option, reply `/hive decision <option>`.")
	} else {
		b.WriteString("\n\nThis issue is not yet offered to agents while the labels above remain.")
	}
	fmt.Fprintf(&b, "\n\n_Removing `%s` later will not stop Hive; adding a label containing `hold` to this issue, and to any pull request already opened for it, will._", f.Label)
	return b.String()
}

// renderLabelApprovalPendingReply is the one comment posted while a person's
// park label withholds the approval (R13).
func renderLabelApprovalPendingReply(f labelApprovalFinding) string {
	var blocking []string
	for _, a := range f.BlockingParkLabels {
		blocking = append(blocking, a.String())
	}
	var own string
	if len(f.HiveParkLabels)+len(f.LaterHiveParkLabels) > 0 {
		own = fmt.Sprintf(" Hive's own %s stay until then.", quoteLabels(append(append([]string{}, f.HiveParkLabels...), f.LaterHiveParkLabels...)))
	}
	return fmt.Sprintf("`%s` was added by @%s (%s), but approval is withheld while %s is on this issue: Hive never removes a park label it did not add.%s Remove that label to let the acceptance complete, or reply `/hive approve`.",
		f.Label, f.Actor, f.TrustReason, strings.Join(blocking, " and "), own)
}

func quoteLabels(labels []string) string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, "`"+l+"`")
	}
	return strings.Join(out, ", ")
}
