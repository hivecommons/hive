package github

import (
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

// WebhookItemBucket is where a re-read item belongs in the actionable
// snapshot, decided from the same label gates the full enumeration applies
// (fetchIssues / fetchPRsWithDetails). The list-wide passes that need the
// whole repository in view (split-parent inheritance, "blocked by" edges,
// reporter-trust parking, clanker parking, claim annotation) are not
// repeated here: the next reconcile pass is authoritative for those.
type WebhookItemBucket string

const (
	// WebhookBucketUnknown means the single read could not place the item
	// (for example a stranger's issue under reporter trust, which the full
	// pass parks with a label write). An existing row is patched in place
	// and a new item waits for the reconcile pass.
	WebhookBucketUnknown WebhookItemBucket = ""
	// WebhookBucketActionable items enter Issues.Items / PRs.Items.
	WebhookBucketActionable WebhookItemBucket = "actionable"
	// WebhookBucketHeld items enter Hold.Items (and PRs.Held for a
	// non-draft PR).
	WebhookBucketHeld WebhookItemBucket = "held"
	// WebhookBucketSidelined items are open but not work (exempt, draft,
	// filtered, standing meta): they leave the actionable and held sets and
	// are listed in WorkDetailsByRepo for the activity monitor.
	WebhookBucketSidelined WebhookItemBucket = "sidelined"
)

// WebhookItemPatch is the governor-side half of a webhook re-read: the
// classified item ready to be upserted into an ActionableResult.
type WebhookItemPatch struct {
	Bucket WebhookItemBucket `json:"bucket,omitempty"`
	Issue  *Issue            `json:"issue,omitempty"`
	PR     *PullRequest      `json:"pr,omitempty"`
	Hold   *HoldItem         `json:"hold,omitempty"`
	// WorkIssue / WorkPR are the display-only records for a sidelined item.
	WorkIssue *RepoWorkIssue `json:"work_issue,omitempty"`
	WorkPR    *RepoWorkPR    `json:"work_pr,omitempty"`
}

// classifyWebhookIssue places one open issue the way fetchIssues would,
// minus the list-wide passes. Side effects (reporter-trust label writes) are
// deliberately not performed here.
func (c *Client) classifyWebhookIssue(repo string, issue *gh.Issue, labels []string, now time.Time) WebhookItemPatch {
	issueFilter := c.getIssueFilter()
	author := safeGetLogin(issue.GetUser())
	sidelined := func(bucket, reason string) WebhookItemPatch {
		row := c.workIssue(repo, issue, labels, bucket, reason)
		return WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkIssue: &row}
	}
	switch standingMetaIssueKindFor(issue.GetTitle(), author, labels) {
	case standingMetaHiveAdvisory:
		return sidelined("hive_advisory", "Hive advisory report")
	case standingMetaDependencyDashboard:
		return sidelined("dependency_dashboard", "Dependency dashboard control panel")
	}
	if reason := standingMetaIssueReason(issue.GetTitle(), author, labels); reason != "" {
		return WebhookItemPatch{Bucket: WebhookBucketSidelined}
	}
	if c.isHeld(labels) {
		return WebhookItemPatch{Bucket: WebhookBucketHeld, Hold: &HoldItem{
			Number:            issue.GetNumber(),
			Repo:              repo,
			Title:             issue.GetTitle(),
			Type:              "issue",
			CreatedAt:         issue.GetCreatedAt().Time,
			URL:               issue.GetHTMLURL(),
			Labels:            labels,
			Assignees:         extractAssignees(issue.Assignees),
			HumanAcknowledged: c.issueHasCheapHumanAcknowledgement(issue),
		}}
	}
	if c.isExempt(labels) {
		return sidelined("filtered", "Exempt label")
	}
	if _, tracking := isTrackingIssue(issue); tracking {
		return sidelined("filtered", trackingFilterReason)
	}
	if hasBlockedLabel(labels) {
		row := c.workIssue(repo, issue, labels, "filtered", blockedFilterReason(issue.GetBody()))
		row.RecheckAfter, row.OpsTracking = blockedRecheck(issue.GetBody())
		row.RecheckDone = blockedRecheckDone(issue.GetBody())
		return WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkIssue: &row}
	}
	if labelPresent(labels, issueNeedsReporterConfirmationLabel) {
		return sidelined("filtered", issueNeedsReporterConfirmationLabel)
	}
	if suppress := hardSuppressIssueBucket(issueFilter, labels); suppress != "" {
		return sidelined("filtered", suppress)
	}
	authorIsHuman := c.isHumanAuthor(issue.GetUser())
	if ra, ok := issueFilter.(ReporterAdmitter); ok && ra.ReporterTrustEnabled() && authorIsHuman {
		if !ra.AdmitsReporter(labels, author, issue.GetAuthorAssociation()) {
			// The full pass parks the issue with a label write; a display
			// read must not. Placement of a listed row is left to the
			// reconcile pass, but the row the pass would list (the
			// reporter_triage bucket) rides along so a brand-new issue
			// is visible from its `opened` delivery rather than from the
			// next enumeration (hivecommons/hive#15094).
			row := c.workIssue(repo, issue, labels, workBucketReporterTriage, reporterTriageAwaitingReason)
			return WebhookItemPatch{Bucket: WebhookBucketUnknown, WorkIssue: &row}
		}
	}
	if !issueFilter.Admits(labels) {
		return sidelined("filtered", "Project issue filter")
	}
	return WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &Issue{
		Repo:              repo,
		Number:            issue.GetNumber(),
		Title:             issue.GetTitle(),
		Body:              issue.GetBody(),
		Author:            author,
		AuthorIsHuman:     authorIsHuman,
		HumanAcknowledged: c.issueHasCheapHumanAcknowledgement(issue),
		Labels:            labels,
		Assignees:         extractAssignees(issue.Assignees),
		Reopened:          strings.EqualFold(issue.GetStateReason(), issueStateReasonReopened),
		CreatedAt:         issue.GetCreatedAt().Time,
		UpdatedAt:         issue.GetUpdatedAt().Time,
		AgeMinutes:        int(now.Sub(issue.GetCreatedAt().Time).Minutes()),
		URL:               issue.GetHTMLURL(),
		IsTracker:         isTracker(issue.GetTitle(), labels, issue.GetBody()),
	}}
}

// classifyWebhookPR places one open PR the way fetchPRsWithDetails would,
// minus the clanker / reporter-trust parking writes and the CI enrichment
// (EnrichCIStatus fills Mergeable and checks on the cycle that uses it).
func (c *Client) classifyWebhookPR(repo string, pr *gh.PullRequest, labels []string, now time.Time) WebhookItemPatch {
	owner, repoName := c.splitRepo(repo)
	attrMeta, hasAttr := ParseAttributionTrailer(pr.GetBody())
	runKey, planRef := ParseRunTrailers(pr.GetBody())
	scopeContract := reviewScopeContract(pr.GetTitle(), pr.GetBody(), owner+"/"+repoName, runKey, planRef, pr.GetHead().GetRef(), c.linearScopeTeams(owner+"/"+repoName))
	author := safeGetLogin(pr.GetUser())
	headRef, headRepo, fromFork := prHeadOrigin(pr)
	reqLogins, reqTeams := prRequestedReviews(pr)
	full := PullRequest{
		Repo:                repo,
		Number:              pr.GetNumber(),
		Title:               pr.GetTitle(),
		Author:              author,
		AppAuthored:         strings.EqualFold(author, c.appBotLogin) && c.appBotLogin != "",
		AuthorAssociation:   pr.GetAuthorAssociation(),
		AuthorNonHuman:      !c.isHumanAuthor(pr.GetUser()),
		Labels:              labels,
		Draft:               pr.GetDraft(),
		CreatedAt:           pr.GetCreatedAt().Time,
		UpdatedAt:           pr.GetUpdatedAt().Time,
		State:               pr.GetState(),
		URL:                 pr.GetHTMLURL(),
		ReviewClass:         ClassifyReviewClass(pr.GetTitle(), labels),
		HiveAttributed:      hasAttr,
		HiveAgent:           attrMeta.Agent,
		HiveBackend:         attrMeta.Backend,
		HiveModel:           attrMeta.Model,
		HiveRun:             runKey,
		HivePlan:            planRef,
		ScopeContract:       scopeContract,
		BodyDigest:          review.PullRequestBodyDigest(pr.GetTitle(), pr.GetBody()),
		Supersedes:          prSupersedesNumberOrZero(pr.GetBody()),
		MergeableState:      pr.GetMergeableState(),
		HeadSHA:             prHeadSHA(pr),
		HeadRef:             headRef,
		HeadRepo:            headRepo,
		FromFork:            fromFork,
		MaintainerCanModify: pr.GetMaintainerCanModify(),
		BaseRef:             prBaseRef(pr),
		RequestedReviewers:  reqLogins,
		RequestedTeams:      reqTeams,
		ReferencedIssues:    PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber()),
	}
	if pr.GetBase() != nil {
		full.BaseSHA = pr.GetBase().GetSHA()
	}
	if c.isHeld(labels) {
		patch := WebhookItemPatch{Bucket: WebhookBucketHeld, Hold: &HoldItem{
			Number:      pr.GetNumber(),
			Repo:        repo,
			Title:       pr.GetTitle(),
			Type:        "pr",
			CreatedAt:   pr.GetCreatedAt().Time,
			HeadSHA:     prHeadSHA(pr),
			Author:      author,
			ReviewClass: ClassifyReviewClass(pr.GetTitle(), labels),
			URL:         pr.GetHTMLURL(),
			Labels:      labels,
		}}
		if pr.GetDraft() {
			row := c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRDraft, "Held draft")
			patch.WorkPR = &row
		} else {
			patch.PR = &full
		}
		return patch
	}
	if c.isExempt(labels) {
		row := c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRFiltered, "Exempt label")
		return WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkPR: &row}
	}
	if pr.GetDraft() {
		row := c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRDraft, "Draft")
		return WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkPR: &row}
	}
	return WebhookItemPatch{Bucket: WebhookBucketActionable, PR: &full}
}

// Clone returns a copy of the result whose item lists and per-repo maps are
// independent of the receiver, so a patch can be applied copy-on-write while
// readers of the old pointer are unaffected. Items themselves are copied by
// value (their own slices are shared, which patches never mutate).
func (r *ActionableResult) Clone() *ActionableResult {
	if r == nil {
		return nil
	}
	out := *r
	out.Issues.Items = append([]Issue(nil), r.Issues.Items...)
	out.PRs.Items = append([]PullRequest(nil), r.PRs.Items...)
	out.PRs.Held = append([]PullRequest(nil), r.PRs.Held...)
	out.PRs.StaleDrafts = append([]PullRequest(nil), r.PRs.StaleDrafts...)
	out.PRs.Attributed = append([]PullRequest(nil), r.PRs.Attributed...)
	out.Hold.Items = append([]HoldItem(nil), r.Hold.Items...)
	out.StaleRepos = append([]string(nil), r.StaleRepos...)
	if r.ItemPatches != nil {
		out.ItemPatches = make(map[string]ItemPatchMeta, len(r.ItemPatches))
		for k, v := range r.ItemPatches {
			out.ItemPatches[k] = v
		}
	}
	if r.WebhookPatches != nil {
		out.WebhookPatches = make(map[string]WebhookItemSnapshot, len(r.WebhookPatches))
		for k, v := range r.WebhookPatches {
			out.WebhookPatches[k] = v
		}
	}
	if r.TotalByRepo != nil {
		out.TotalByRepo = make(map[string]RepoCounts, len(r.TotalByRepo))
		for k, v := range r.TotalByRepo {
			out.TotalByRepo[k] = v
		}
	}
	if r.WorkBreakdownByRepo != nil {
		out.WorkBreakdownByRepo = make(map[string]RepoWorkBreakdown, len(r.WorkBreakdownByRepo))
		for k, v := range r.WorkBreakdownByRepo {
			out.WorkBreakdownByRepo[k] = v
		}
	}
	if r.WorkDetailsByRepo != nil {
		out.WorkDetailsByRepo = make(map[string]RepoWorkDetails, len(r.WorkDetailsByRepo))
		for k, v := range r.WorkDetailsByRepo {
			out.WorkDetailsByRepo[k] = RepoWorkDetails{
				Issues: append([]RepoWorkIssue(nil), v.Issues...),
				PRs:    append([]RepoWorkPR(nil), v.PRs...),
			}
		}
	}
	return &out
}

// MarkAllStale flags every repo in the result as served from a previous
// enumeration. The governor applies it to the snapshot it keeps when a whole
// reconcile pass fails, so the dashboard shows last-good counts as stale
// rather than a drop to zero.
func (r *ActionableResult) MarkAllStale() {
	if r == nil {
		return
	}
	repos := make([]string, 0, len(r.TotalByRepo))
	for repo := range r.TotalByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	r.StaleRepos = repos
	if r.StaleSince.IsZero() {
		r.StaleSince = r.GeneratedAt
	}
}

// ApplyWebhookItem upserts one re-read item into the result and reports
// whether anything changed. The receiver is mutated; callers that publish
// the result copy-on-write should Clone first.
//
//   - A closed or merged item leaves every list it was in and its repo's
//     open count drops by one.
//   - An open item already listed is replaced where it lives; when its
//     bucket changed (a hold label added or removed, a draft marked ready)
//     it moves between the actionable, held and sidelined sets.
//   - An open item the snapshot never listed is appended to the set its
//     bucket names and counted as new open work for its repo. An item of
//     unknown bucket is left for the reconcile pass.
//   - The upsert is monotonic: a read whose updated_at is older than the
//     copy already held (two deliveries' re-reads landing out of order) is
//     dropped, so the snapshot never moves backwards.
//
// Each applied read is recorded in ItemPatches (fetch time, updated_at,
// head SHA, delivery id) for staleness bounds and audit. Counts
// (Issues.Count, PRs.Count, Hold.*, SLA violations) are recomputed from the
// lists; ordering follows the enumeration's sort rules.
func (r *ActionableResult) ApplyWebhookItem(snap WebhookItemSnapshot) bool {
	if r == nil || snap.Number <= 0 || snap.Repo == "" {
		return false
	}
	if r.olderThanHeld(snap) {
		return false
	}
	applied := r.applyWebhookItem(snap)
	if applied {
		r.recordItemPatch(snap)
	}
	return applied
}

// olderThanHeld reports whether the read is older than the copy the result
// already holds for the same item, by GitHub's updated_at.
func (r *ActionableResult) olderThanHeld(snap WebhookItemSnapshot) bool {
	if snap.UpdatedAt.IsZero() {
		return false
	}
	key := ItemPatchKey(snap.Repo, snap.Number, snap.IsPR)
	if meta, ok := r.ItemPatches[key]; ok && !meta.UpdatedAt.IsZero() {
		return meta.UpdatedAt.After(snap.UpdatedAt)
	}
	held := r.heldUpdatedAt(snap.Repo, snap.Number, snap.IsPR)
	return !held.IsZero() && held.After(snap.UpdatedAt)
}

// heldUpdatedAt is the updated_at of the listed copy of an item, zero when
// the item is not listed or the list has no timestamp for it.
func (r *ActionableResult) heldUpdatedAt(repo string, number int, isPR bool) time.Time {
	if isPR {
		for _, list := range [][]PullRequest{r.PRs.Items, r.PRs.Held} {
			for _, pr := range list {
				if sameItem(pr.Repo, pr.Number, repo, number) {
					return pr.UpdatedAt
				}
			}
		}
		return time.Time{}
	}
	for _, issue := range r.Issues.Items {
		if sameItem(issue.Repo, issue.Number, repo, number) {
			return issue.UpdatedAt
		}
	}
	return time.Time{}
}

func (r *ActionableResult) recordItemPatch(snap WebhookItemSnapshot) {
	if r.ItemPatches == nil {
		r.ItemPatches = map[string]ItemPatchMeta{}
	}
	// A removed item keeps the record too: it is the audit trail of which
	// delivery took the row out, and the monotonic check for a late read.
	key := ItemPatchKey(snap.Repo, snap.Number, snap.IsPR)
	r.ItemPatches[key] = ItemPatchMeta{
		FetchedAt: snap.FetchedAt, UpdatedAt: snap.UpdatedAt, DeliveryID: snap.DeliveryID, HeadSHA: snap.HeadSHA,
	}
	if r.WebhookPatches == nil {
		r.WebhookPatches = map[string]WebhookItemSnapshot{}
	}
	if _, held := r.WebhookPatches[key]; !held && len(r.WebhookPatches) >= maxRememberedWebhookPatches {
		r.evictOldestWebhookPatch()
	}
	r.WebhookPatches[key] = snap
}

// maxRememberedWebhookPatches bounds WebhookPatches; the oldest read goes
// first. Far above what one reconcile interval sees on any tracked set.
const maxRememberedWebhookPatches = 4096

func (r *ActionableResult) evictOldestWebhookPatch() {
	oldestKey := ""
	var oldest time.Time
	for k, v := range r.WebhookPatches {
		if oldestKey == "" || v.FetchedAt.Before(oldest) {
			oldestKey, oldest = k, v.FetchedAt
		}
	}
	if oldestKey != "" {
		delete(r.WebhookPatches, oldestKey)
	}
}

// ReplayWebhookPatches carries the webhook re-reads prev received after the
// receiver's enumeration started (snap.FetchedAt after r.GeneratedAt) onto
// the receiver, and returns how many changed it. A read is replayed only
// when the receiver lists the item with an older (or no) updated_at — the
// enumeration's own copy is never moved backwards — and a closed read only
// when the item is still listed. It also hands every newer remembered read
// to the receiver, so a later pass can replay the same way. The governor
// calls it before publishing a reconcile pass's result in place of the
// patched snapshot it replaces (hivecommons/hive#14952: #14863 stayed `blocked`
// 40 minutes after the hive itself removed the label).
func (r *ActionableResult) ReplayWebhookPatches(prev *ActionableResult) int {
	if r == nil || prev == nil || prev == r || len(prev.WebhookPatches) == 0 {
		return 0
	}
	keys := make([]string, 0, len(prev.WebhookPatches))
	for k := range prev.WebhookPatches {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	applied := 0
	for _, k := range keys {
		snap := prev.WebhookPatches[k]
		if snap.FetchedAt.IsZero() || !snap.FetchedAt.After(r.GeneratedAt) {
			continue
		}
		held, listed := r.listedUpdatedAt(snap.Repo, snap.Number, snap.IsPR)
		if !listed || (!held.IsZero() && !snap.UpdatedAt.IsZero() && !held.Before(snap.UpdatedAt)) {
			continue
		}
		if r.ApplyWebhookItem(snap) {
			applied++
		}
	}
	return applied
}

// listedUpdatedAt reports whether the result lists an item anywhere
// (actionable, held, drafts, attributed or a work-detail bucket) and the
// updated_at of that copy (zero when the list has none).
func (r *ActionableResult) listedUpdatedAt(repo string, number int, isPR bool) (time.Time, bool) {
	if isPR {
		for _, list := range [][]PullRequest{r.PRs.Items, r.PRs.Held, r.PRs.StaleDrafts, r.PRs.Attributed} {
			for _, pr := range list {
				if sameItem(pr.Repo, pr.Number, repo, number) {
					return pr.UpdatedAt, true
				}
			}
		}
		for _, d := range r.WorkDetailsByRepo {
			for _, w := range d.PRs {
				if sameItem(w.Repo, w.Number, repo, number) {
					return w.UpdatedAt, true
				}
			}
		}
		return time.Time{}, false
	}
	for _, issue := range r.Issues.Items {
		if sameItem(issue.Repo, issue.Number, repo, number) {
			return issue.UpdatedAt, true
		}
	}
	for _, d := range r.WorkDetailsByRepo {
		for _, w := range d.Issues {
			if sameItem(w.Repo, w.Number, repo, number) {
				return w.UpdatedAt, true
			}
		}
	}
	return time.Time{}, false
}

func (r *ActionableResult) applyWebhookItem(snap WebhookItemSnapshot) bool {
	repoKey := r.repoKeyFor(snap.Repo)
	prev, wasListed := r.removeItem(snap.Repo, snap.Number, snap.IsPR)
	if !snap.Open() {
		if wasListed {
			r.adjustRepoCount(repoKey, snap.IsPR, -1)
			r.recount()
		}
		return wasListed
	}
	p := snap.Patch
	if snap.IsPR {
		p = p.withLinkedIssues(prev.linkedIssues)
	}
	added := false
	switch p.Bucket {
	case WebhookBucketActionable:
		if snap.IsPR && p.PR != nil {
			r.PRs.Items = append(r.PRs.Items, *p.PR)
			SortPullRequestsForReview(r.PRs.Items)
			added = true
		} else if !snap.IsPR && p.Issue != nil {
			r.Issues.Items = append(r.Issues.Items, *p.Issue)
			RankActionableIssues(r.Issues.Items)
			added = true
		}
	case WebhookBucketHeld:
		if p.Hold != nil {
			r.Hold.Items = append(r.Hold.Items, *p.Hold)
			SortHoldItemsForReview(r.Hold.Items)
			added = true
		}
		if snap.IsPR && p.PR != nil {
			r.PRs.Held = append(r.PRs.Held, *p.PR)
			SortPullRequestsForReview(r.PRs.Held)
		}
		if snap.IsPR && p.WorkPR != nil {
			r.addWorkPR(repoKey, *p.WorkPR)
		}
	case WebhookBucketSidelined:
		if p.WorkIssue != nil {
			r.addWorkIssue(repoKey, *p.WorkIssue)
			added = true
		}
		if p.WorkPR != nil {
			r.addWorkPR(repoKey, *p.WorkPR)
			added = true
		}
	default:
		if !wasListed {
			// Unknown bucket, never listed: an `opened` delivery for an
			// item the single read could not place. It is still an open
			// item the hive is responsible for, so it enters the sidelined
			// set with whatever row the classifier could offer rather than
			// staying invisible until the next enumeration
			// (hivecommons/hive#15094).
			switch {
			case !snap.IsPR && p.WorkIssue != nil:
				r.addWorkIssue(repoKey, *p.WorkIssue)
			case snap.IsPR && p.WorkPR != nil:
				r.addWorkPR(repoKey, *p.WorkPR)
			default:
				return false
			}
			r.adjustRepoCount(repoKey, snap.IsPR, 1)
			r.recount()
			return true
		}
		// Unknown bucket: keep the row where it was, with fresh fields.
		return r.restoreUnknown(snap, prev)
	}
	if added && !wasListed {
		r.adjustRepoCount(repoKey, snap.IsPR, 1)
	}
	r.recount()
	return added || wasListed
}

// restoreUnknown is the in-place fallback for an item whose bucket the single
// read could not decide: removeItem already took the stale row out, so the
// previous entry is re-added with the display fields refreshed.
func (r *ActionableResult) restoreUnknown(snap WebhookItemSnapshot, prev removedItem) bool {
	switch {
	case prev.issue != nil:
		it := *prev.issue
		it.Title, it.Labels, it.UpdatedAt = snap.Title, append([]string(nil), snap.Labels...), snap.UpdatedAt
		r.Issues.Items = append(r.Issues.Items, it)
		RankActionableIssues(r.Issues.Items)
	case prev.pr != nil:
		it := *prev.pr
		it.Title, it.Labels, it.Draft, it.UpdatedAt = snap.Title, append([]string(nil), snap.Labels...), snap.Draft, snap.UpdatedAt
		if prev.prHeld {
			r.PRs.Held = append(r.PRs.Held, it)
			SortPullRequestsForReview(r.PRs.Held)
		} else {
			r.PRs.Items = append(r.PRs.Items, it)
			SortPullRequestsForReview(r.PRs.Items)
		}
	}
	if prev.hold != nil {
		it := *prev.hold
		it.Title, it.Labels = snap.Title, append([]string(nil), snap.Labels...)
		r.Hold.Items = append(r.Hold.Items, it)
		SortHoldItemsForReview(r.Hold.Items)
	}
	if prev.workIssue != nil {
		it := *prev.workIssue
		it.Title, it.Labels, it.UpdatedAt = snap.Title, append([]string(nil), snap.Labels...), snap.UpdatedAt
		r.addWorkIssue(prev.repoKey, it)
	}
	if prev.workPR != nil {
		it := *prev.workPR
		it.Title, it.Labels, it.Draft, it.UpdatedAt = snap.Title, append([]string(nil), snap.Labels...), snap.Draft, snap.UpdatedAt
		r.addWorkPR(prev.repoKey, it)
	}
	r.recount()
	return true
}

// withLinkedIssues returns the patch with known carried onto its PR and
// WorkPR rows when the single read left them without closing references.
// The webhook re-read is one REST GET and cannot see GitHub's
// closingIssuesReferences (those come from the per-repo GraphQL query in
// EnrichReviewSignalsAndWorkPRs), so a PR that moves bucket keeps the linked
// issues the previous row had instead of rendering "no linked issue" until
// the next reconcile tick (hivecommons/hive#13448). The rows are copied, so
// the caller's patch is never mutated.
func (p WebhookItemPatch) withLinkedIssues(known []PRLinkedIssue) WebhookItemPatch {
	if len(known) == 0 {
		return p
	}
	if p.PR != nil && len(p.PR.LinkedIssues) == 0 {
		pr := *p.PR
		pr.LinkedIssues = known
		p.PR = &pr
	}
	if p.WorkPR != nil && len(p.WorkPR.LinkedIssues) == 0 {
		w := *p.WorkPR
		w.LinkedIssues = known
		p.WorkPR = &w
	}
	return p
}

// removedItem is what removeItem took out, so restoreUnknown can put it back.
// linkedIssues is the first non-empty closing-reference list seen on any
// removed PR row, carried onto the re-bucketed row by applyWebhookItem.
type removedItem struct {
	repoKey      string
	issue        *Issue
	pr           *PullRequest
	prHeld       bool
	hold         *HoldItem
	workIssue    *RepoWorkIssue
	workPR       *RepoWorkPR
	linkedIssues []PRLinkedIssue
}

func (rem *removedItem) noteLinkedIssues(li []PRLinkedIssue) {
	if len(rem.linkedIssues) == 0 && len(li) > 0 {
		rem.linkedIssues = li
	}
}

// repoKeyFor finds the TotalByRepo / WorkDetailsByRepo key matching repo
// case-insensitively (configured repos may differ in case from the webhook's
// full_name); falls back to repo itself.
func (r *ActionableResult) repoKeyFor(repo string) string {
	for k := range r.TotalByRepo {
		if strings.EqualFold(k, repo) {
			return k
		}
	}
	for k := range r.WorkDetailsByRepo {
		if strings.EqualFold(k, repo) {
			return k
		}
	}
	return repo
}

func sameItem(itemRepo string, itemNumber int, repo string, number int) bool {
	return itemNumber == number && strings.EqualFold(itemRepo, repo)
}

// removeItem drops every entry naming (repo, number) from the lists of the
// given kind and returns what it removed plus whether anything was listed.
func (r *ActionableResult) removeItem(repo string, number int, isPR bool) (removedItem, bool) {
	rem := removedItem{repoKey: r.repoKeyFor(repo)}
	found := false
	if isPR {
		r.PRs.Items = filterPRs(r.PRs.Items, repo, number, func(pr PullRequest) {
			cp := pr
			rem.pr = &cp
			rem.noteLinkedIssues(pr.LinkedIssues)
			found = true
		})
		r.PRs.Held = filterPRs(r.PRs.Held, repo, number, func(pr PullRequest) {
			cp := pr
			rem.pr = &cp
			rem.prHeld = true
			rem.noteLinkedIssues(pr.LinkedIssues)
			found = true
		})
		r.PRs.StaleDrafts = filterPRs(r.PRs.StaleDrafts, repo, number, func(pr PullRequest) {
			rem.noteLinkedIssues(pr.LinkedIssues)
			found = true
		})
		r.Hold.Items = filterHold(r.Hold.Items, repo, number, "pr", func(h HoldItem) { cp := h; rem.hold = &cp; found = true })
		if d, ok := r.WorkDetailsByRepo[rem.repoKey]; ok {
			var kept []RepoWorkPR
			for _, w := range d.PRs {
				if sameItem(w.Repo, w.Number, repo, number) {
					cp := w
					rem.workPR = &cp
					rem.noteLinkedIssues(w.LinkedIssues)
					found = true
					continue
				}
				kept = append(kept, w)
			}
			d.PRs = kept
			r.WorkDetailsByRepo[rem.repoKey] = d
		}
	} else {
		var kept []Issue
		for _, it := range r.Issues.Items {
			if sameItem(it.Repo, it.Number, repo, number) {
				cp := it
				rem.issue = &cp
				found = true
				continue
			}
			kept = append(kept, it)
		}
		r.Issues.Items = kept
		r.Hold.Items = filterHold(r.Hold.Items, repo, number, "issue", func(h HoldItem) { cp := h; rem.hold = &cp; found = true })
		if d, ok := r.WorkDetailsByRepo[rem.repoKey]; ok {
			var keptW []RepoWorkIssue
			for _, w := range d.Issues {
				if sameItem(w.Repo, w.Number, repo, number) {
					cp := w
					rem.workIssue = &cp
					found = true
					continue
				}
				keptW = append(keptW, w)
			}
			d.Issues = keptW
			r.WorkDetailsByRepo[rem.repoKey] = d
		}
	}
	return rem, found
}

func filterPRs(list []PullRequest, repo string, number int, onHit func(PullRequest)) []PullRequest {
	var kept []PullRequest
	for _, pr := range list {
		if sameItem(pr.Repo, pr.Number, repo, number) {
			onHit(pr)
			continue
		}
		kept = append(kept, pr)
	}
	return kept
}

func filterHold(list []HoldItem, repo string, number int, typ string, onHit func(HoldItem)) []HoldItem {
	var kept []HoldItem
	for _, h := range list {
		if h.Type == typ && sameItem(h.Repo, h.Number, repo, number) {
			onHit(h)
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

func (r *ActionableResult) addWorkIssue(repoKey string, w RepoWorkIssue) {
	if r.WorkDetailsByRepo == nil {
		r.WorkDetailsByRepo = map[string]RepoWorkDetails{}
	}
	d := r.WorkDetailsByRepo[repoKey]
	d.Issues = append(d.Issues, w)
	r.WorkDetailsByRepo[repoKey] = d
}

func (r *ActionableResult) addWorkPR(repoKey string, w RepoWorkPR) {
	if r.WorkDetailsByRepo == nil {
		r.WorkDetailsByRepo = map[string]RepoWorkDetails{}
	}
	d := r.WorkDetailsByRepo[repoKey]
	d.PRs = append(d.PRs, w)
	r.WorkDetailsByRepo[repoKey] = d
}

// adjustRepoCount moves the repo's open issue or PR total by delta, never
// below zero. A repo the snapshot has no row for is left alone: the
// reconcile pass will count it.
func (r *ActionableResult) adjustRepoCount(repoKey string, isPR bool, delta int) {
	counts, ok := r.TotalByRepo[repoKey]
	if !ok {
		return
	}
	if isPR {
		counts.PRs = max(0, counts.PRs+delta)
	} else {
		counts.Issues = max(0, counts.Issues+delta)
	}
	r.TotalByRepo[repoKey] = counts
}

// recount derives the summary counters from the lists.
func (r *ActionableResult) recount() {
	r.Issues = IssueResultFromItems(r.Issues.Items)
	r.PRs.Count = len(r.PRs.Items)
	r.Hold.Issues, r.Hold.PRs = 0, 0
	for _, h := range r.Hold.Items {
		if h.Type == "issue" {
			r.Hold.Issues++
		} else {
			r.Hold.PRs++
		}
	}
	r.Hold.Total = len(r.Hold.Items)
}
