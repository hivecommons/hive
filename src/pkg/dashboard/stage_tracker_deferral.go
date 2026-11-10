package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A hive deferral of a red PR (hivecommons/hive#17347). The scanner can
// decide a head will never go green as it stands, file the successor
// issues and post "Deferring." on the PR; the relay audits that as
// agent_pr_deferred, bound to the head it was posted on. Without it the PR
// in progress chevron read "Unassigned · next: …" — nobody looked — and
// sent the operator to investigate work the hive had already finished.

// codeStateDeferred: the hive deferred the current head; the deferral and
// its successors are the PR's state until a later pickup or a new head.
const codeStateDeferred = "deferred"

// prProgressDeferredWord leads the deferred chevron word.
const prProgressDeferredWord = "Deferred"

// prProgressSharedCIWord leads the word of a deferral that waits on a
// [shared-ci] incident: "Waiting on shared CI · #N" (hivecommons/hive#17500).
// "Deferred · by scanner" said nothing about why; the incident is the why,
// and it lifts on its own when the incident closes.
const prProgressSharedCIWord = "Waiting on shared CI"

// prDeferralAuditLookback bounds how far back a deferral is read. A
// deferral stands while its head does; a fortnight-old head nobody moved is
// the same decision.
const prDeferralAuditLookback = 14 * 24 * time.Hour

// prDeferralAuditPath is where the tracker scans the on-disk audit log for
// deferrals; "" means the production path. Tests point it at a temp dir.
var prDeferralAuditPath = ""

// StageTrackerDeferral is the hive's standing deferral of the PR's current
// head: who deferred it, when, why, the successor issues / PRs it waits on
// and whether it says a maintainer may close the PR now. Set on a row only
// while the PR in progress chevron reads Deferred.
type StageTrackerDeferral struct {
	Actor string     `json:"actor,omitempty"`
	Since *time.Time `json:"since,omitempty"`
	Head  string     `json:"head"`
	// Reason is the deferral's own words, bounded by the relay.
	Reason string `json:"reason,omitempty"`
	// Successors are "#N" (same repository) or "owner/repo#N" references in
	// the order the deferral named them.
	Successors []string `json:"successors,omitempty"`
	// SuccessorURL links the first successor on GitHub.
	SuccessorURL string `json:"successor_url,omitempty"`
	// URL is the deferral comment.
	URL string `json:"url,omitempty"`
	// CloseOK is true when the deferral says a maintainer may close the PR
	// now: the row then offers Close now.
	CloseOK bool `json:"close_ok,omitempty"`
	// Incident is the [shared-ci] incident issue (same repository) the
	// deferral waits on, from the relay's incident pair; 0 when the
	// deferral is the agent's own decision.
	Incident int `json:"incident,omitempty"`
	// IncidentURL links the incident issue on GitHub.
	IncidentURL string `json:"incident_url,omitempty"`
}

func (s *Server) prDeferralAuditEntries(now time.Time) []AuditEntry {
	if s == nil || s.audit == nil {
		return nil
	}
	since := now.Add(-prDeferralAuditLookback)
	if s.audit.HasOnDiskLog(prDeferralAuditPath) {
		return s.audit.ActionsWithPrefixSince(since, github.AuditActionAgentPRDeferred, prDeferralAuditPath)
	}
	return s.audit.RecentWithPrefixSince(since, github.AuditActionAgentPRDeferred)
}

// prDeferralDetailKey finds where each "k=v" pair of an audit detail starts.
// The reason is free text, so a pair boundary is ", " followed by a key and
// "=", never a bare comma.
var prDeferralDetailKey = regexp.MustCompile(`(?:^|, )([a-z_]+)=`)

// parsePRDeferralAuditDetail splits the detail into its pairs. The first
// occurrence of a key wins: the relay writes the typed pairs (head,
// close_ok, url, …) before the free-text reason, so a ", close_ok=true"
// quoted inside the reason can never override them.
func parsePRDeferralAuditDetail(detail string) map[string]string {
	out := map[string]string{}
	idx := prDeferralDetailKey.FindAllStringSubmatchIndex(detail, -1)
	for i, m := range idx {
		key := detail[m[2]:m[3]]
		if _, dup := out[key]; dup {
			continue
		}
		end := len(detail)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[key] = strings.TrimSpace(detail[m[1]:end])
	}
	return out
}

// stageTrackerDeferrals replays agent_pr_deferred entries oldest first: the
// latest deferral of each PR wins. Keys are github.ReviewQueueKey of the
// qualified repository.
func stageTrackerDeferrals(entries []AuditEntry, qualify func(string) string) map[string]StageTrackerDeferral {
	out := map[string]StageTrackerDeferral{}
	for _, e := range entries {
		if e.Action != github.AuditActionAgentPRDeferred || e.Target <= 0 {
			continue
		}
		repo := strings.TrimSpace(e.Repo)
		if qualify != nil {
			repo = qualify(repo)
		}
		kv := parsePRDeferralAuditDetail(e.Detail)
		head := kv[github.PRDeferralPairHead]
		if repo == "" || head == "" {
			continue
		}
		d := StageTrackerDeferral{Actor: e.Agent, Head: head, Reason: kv[github.PRDeferralPairReason], URL: kv[github.PRDeferralPairURL]}
		if at, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			d.Since = &at
		}
		d.Successors = strings.Fields(kv[github.PRDeferralPairSuccessors])
		d.CloseOK, _ = strconv.ParseBool(kv[github.PRDeferralPairCloseOK])
		if n, err := strconv.Atoi(kv[github.PRDeferralPairIncident]); err == nil && n > 0 {
			d.Incident = n
		}
		out[github.ReviewQueueKey(repo, e.Target)] = d
	}
	return out
}

// stageTrackerDeferralFor is the deferral standing on the row's current
// head, with its first successor linked; nil when none does. A new head
// clears a deferral: the hive has to decide again.
func stageTrackerDeferralFor(it *StageTrackerItem, deferrals map[string]StageTrackerDeferral, githubBaseURL string) *StageTrackerDeferral {
	if it == nil || it.Kind != "pr" || it.Head == "" || len(deferrals) == 0 {
		return nil
	}
	d, ok := deferrals[github.ReviewQueueKey(it.Repo, it.Number)]
	if !ok || !strings.EqualFold(d.Head, it.Head) {
		return nil
	}
	if len(d.Successors) > 0 {
		d.SuccessorURL = stageTrackerRefURL(it.Repo, d.Successors[0], githubBaseURL)
	}
	if d.Incident > 0 {
		d.IncidentURL = stageTrackerRefURL(it.Repo, "#"+strconv.Itoa(d.Incident), githubBaseURL)
	}
	d.Successors = append([]string(nil), d.Successors...)
	return &d
}

// stageTrackerRefURL is the GitHub page of ref ("#N" on repo, "owner/repo#N"
// elsewhere); "" when ref has no positive number. GitHub redirects an issues
// URL to the pull request when N is one.
func stageTrackerRefURL(repo, ref, githubBaseURL string) string {
	refRepo, n, ok := strings.Cut(ref, "#")
	num, err := strconv.Atoi(n)
	if !ok || err != nil || num <= 0 {
		return ""
	}
	if refRepo == "" {
		refRepo = repo
	}
	return overviewItemURL("", refRepo, num, "issue", githubBaseURL)
}

// stageTrackerDeferredActivity is the PR in progress code activity of a
// deferred head. A deferral to a shared-CI incident reads "Waiting on shared
// CI · #17463", linked to the incident, and its detail names the failing
// check and that it lifts when the incident closes. Any other deferral
// reads "Deferred · waiting on #17345 +1" (first successor, then how many
// more) or "Deferred · by scanner" when it names none, and its detail leads
// with the agent's own reason.
func stageTrackerDeferredActivity(it *StageTrackerItem, d *StageTrackerDeferral, redWord string, redSince time.Time) *StageTrackerActivity {
	out := &StageTrackerActivity{Head: it.Head, State: codeStateDeferred, Actor: d.Actor, Source: "hive-deferral", Since: d.Since, URL: d.SuccessorURL}
	if out.URL == "" {
		out.URL = d.URL
	}
	who := d.Actor
	if who == "" {
		who = "the hive"
	}
	facts := who + " deferred head " + stageTrackerShortSHA(it.Head)
	if d.Since != nil {
		facts += " at " + d.Since.UTC().Format(time.RFC3339)
	}
	redAt := ""
	if !redSince.IsZero() {
		redAt = " at " + redSince.UTC().Format(time.RFC3339)
	}
	standing := redWord + redAt + " stands on this head; a fix kick, claim or push after the deferral returns it to Remediating, and a new head clears it."
	if d.Incident > 0 {
		ref := "#" + strconv.Itoa(d.Incident)
		out.Caption = prProgressSharedCIWord + " · " + ref
		if d.IncidentURL != "" {
			out.URL = d.IncidentURL
		}
		detail := stageTrackerFailingChecksPhrase(it) + " is failing fleet-wide; tracked in " + ref + ". Lifts automatically when that issue closes."
		detail += " " + facts + "."
		if d.Reason != "" {
			detail += " " + stageTrackerFirstSentence(d.Reason)
		}
		out.Detail = detail + " " + standing
		return out
	}
	out.Caption = prProgressDeferredWord
	switch {
	case len(d.Successors) > 0:
		out.Caption += " · waiting on " + d.Successors[0]
		if more := len(d.Successors) - 1; more > 0 {
			out.Caption += " +" + strconv.Itoa(more)
		}
	case d.Actor != "":
		out.Caption += " · by " + d.Actor
	}
	detail := ""
	if d.Reason != "" {
		// The why leads: the word alone ("Deferred · by scanner") says
		// nothing about it (hivecommons/hive#17500).
		detail = stageTrackerFirstSentence(d.Reason) + " "
	}
	detail += facts + "."
	if len(d.Successors) > 0 {
		detail += " Waiting on " + strings.Join(d.Successors, ", ") + "."
	}
	detail += " " + standing
	if d.CloseOK {
		detail += " The deferral says a maintainer may close it now."
	}
	out.Detail = detail
	return out
}

// stageTrackerFailingChecksPhrase names the red checks on the row's head
// in backticks ("`mutation`", "`lint` and `test`"); "CI" when the CI
// activity records none by name.
func stageTrackerFailingChecksPhrase(it *StageTrackerItem) string {
	a := it.CIActivity
	if a == nil || !strings.EqualFold(a.Head, it.Head) || len(a.ChecksFailing) == 0 {
		return "CI"
	}
	names := make([]string, len(a.ChecksFailing))
	for i, n := range a.ChecksFailing {
		names[i] = "`" + n + "`"
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// stageTrackerFirstSentence is the first sentence of a deferral reason,
// ending in a period: the tooltip's one-line why. A dot inside a token
// ("docs/a.md", "#17463.") does not end it; a bare "Deferring." is kept as
// is.
func stageTrackerFirstSentence(reason string) string {
	s := strings.TrimSpace(reason)
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && s[i] != '!' && s[i] != '?' {
			continue
		}
		if i+1 == len(s) {
			return s
		}
		if s[i+1] == ' ' || s[i+1] == '\n' {
			return s[:i+1]
		}
	}
	return strings.TrimRight(s, " ") + "."
}
