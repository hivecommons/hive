package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The scanner's CI-triage and rebase notes on a PR are one comment per PR,
// edited in place, and silent when nothing changed (hivecommons/hive#18451).
// Before this, every scanner pass posted a fresh free-form comment: PR #18254
// got 17 near-identical triage comments on the same head in nine hours.
//
// The relay is the enforcement point because every agent comment goes
// through it (bin/gh-wrapper.sh routes `gh pr comment` to `hive-open-issue
// comment`, and the write-surface gate refuses a direct POST), and a rule
// that lives only in the prompt drifts.
const (
	// ScannerNoteMarkerName is the HTML-comment marker name on the one
	// scanner note per PR. The full marker records the head the note
	// describes, a fingerprint of that head's failing checks, and the head
	// the PR author was last told about in a new comment:
	// `<!-- hive:scanner-note head=<sha> checks=<fp> told=<sha> -->`.
	ScannerNoteMarkerName = "hive:scanner-note"

	// scannerNoteEnv turns the upsert off when set to a false value
	// ("0", "false", "off", "no"). Unset or anything else means on.
	scannerNoteEnv = "HIVE_SCANNER_NOTE_UPSERT"

	// scannerNoteLane is the agent lane whose PR notes are upserted.
	scannerNoteLane = "scanner"

	// scannerNoteConflictCheck stands in the failing set for a PR whose
	// mergeable state is dirty, so a rebase note is keyed like a red check.
	scannerNoteConflictCheck = "merge-conflict"

	// scannerNoteDCOCheck stands in the failing set for a failed DCO context
	// (SummarizeCommitChecks reports DCO apart from the red list).
	scannerNoteDCOCheck = "DCO"

	// scannerNoteFingerprintLen is how many hex characters of the failing
	// set's SHA-256 the marker keeps; enough to tell sets apart on one PR.
	scannerNoteFingerprintLen = 12

	// scannerNoteUnknownChecks is the fingerprint recorded when the head's
	// checks could not be read. It never matches a later reading, so an
	// unreadable head is never a reason to drop a note.
	scannerNoteUnknownChecks = "unknown"

	// mergeableStateDirty is GitHub's mergeable_state for a merge conflict.
	mergeableStateDirty = "dirty"
)

var (
	scannerNoteMarkerRE = regexp.MustCompile(`<!--\s*` + regexp.QuoteMeta(ScannerNoteMarkerName) + `\b([^>]*?)\s*-->\n?`)
	scannerNoteFieldRE  = regexp.MustCompile(`(\w+)=(\S+)`)
	agentReplicaSuffix  = regexp.MustCompile(`-\d+$`)
)

// scannerNoteFalseValues are the scannerNoteEnv spellings that disable it.
var scannerNoteFalseValues = map[string]bool{"0": true, "false": true, "off": true, "no": true}

// scannerNoteEnabled reports whether scanner PR notes are upserted. It reads
// the environment on every call so a restart is not needed to flip it in a
// test, and it costs nothing next to the GitHub calls it guards.
func scannerNoteEnabled() bool {
	return !scannerNoteFalseValues[strings.ToLower(strings.TrimSpace(os.Getenv(scannerNoteEnv)))]
}

// isScannerAgent reports whether agent is the scanner lane or one of its
// replicas (scanner-2, …).
func isScannerAgent(agent string) bool {
	return agentReplicaSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(agent)), "") == scannerNoteLane
}

// scannerNoteCandidate reports whether a relayed comment body is a scanner
// free-text note the upsert may own. A body carrying any dedicated hive
// marker other than the identity footer's origin marker keeps its own path:
// a shared-CI incident reference must stay a separate, never-edited comment,
// and blocker, verification or status markers have their own writers.
// Ad-hoc markers the agent invents (`<!-- hive-scanner-triage -->`) do not
// make a note special and are folded in like the rest of the text.
func scannerNoteCandidate(body string) bool {
	if strings.Contains(body, "<!-- hive-shared-ci-") || verificationMarker(body) != "" {
		return false
	}
	rest := strings.ReplaceAll(body, "<!-- "+OriginMarkerName+" ", "")
	return !strings.Contains(rest, "<!-- hive:")
}

// scannerNoteState is what the marker on the current note records.
type scannerNoteState struct {
	Head   string
	Checks string
	Told   string
}

// marker renders the state as the note's first line.
func (s scannerNoteState) marker() string {
	m := "<!-- " + ScannerNoteMarkerName + " head=" + s.Head + " checks=" + s.Checks
	if s.Told != "" {
		m += " told=" + s.Told
	}
	return m + " -->"
}

// parseScannerNoteState reads the marker on body; ok is false when there is
// none.
func parseScannerNoteState(body string) (scannerNoteState, bool) {
	m := scannerNoteMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return scannerNoteState{}, false
	}
	var s scannerNoteState
	for _, f := range scannerNoteFieldRE.FindAllStringSubmatch(m[1], -1) {
		switch f[1] {
		case "head":
			s.Head = f[2]
		case "checks":
			s.Checks = f[2]
		case "told":
			s.Told = f[2]
		}
	}
	return s, true
}

// renderScannerNote is the note comment: the marker line, then the agent's
// body as relayed (identity footer and attribution trailer included).
func renderScannerNote(s scannerNoteState, body string) string {
	return s.marker() + "\n" + stripScannerNoteMarker(body)
}

// stripScannerNoteMarker removes any scanner-note marker from body, so a
// superseded note stops being the PR's current one and an agent that copied
// a marker into its text cannot forge state.
func stripScannerNoteMarker(body string) string {
	return scannerNoteMarkerRE.ReplaceAllString(body, "")
}

// scannerNoteFingerprint is a stable digest of a failing-check set: order
// and duplicates do not matter, and the empty set has its own value.
func scannerNoteFingerprint(failing []string) string {
	set := map[string]bool{}
	for _, name := range failing {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])[:scannerNoteFingerprintLen]
}

// scannerNoteOutcome is what upsertScannerNote did with a note.
type scannerNoteOutcome int

const (
	// scannerNotePassThrough: not a PR (or the PR could not be read); the
	// caller posts the comment the usual way.
	scannerNotePassThrough scannerNoteOutcome = iota
	// scannerNoteDropped: same head and same failing checks as the current
	// note; nothing was written.
	scannerNoteDropped
	// scannerNoteEdited: the current note was rewritten in place.
	scannerNoteEdited
	// scannerNoteCreated: a new comment was posted (the first note, or the
	// author must act and has not been told for this head).
	scannerNoteCreated
)

// upsertScannerNote delivers a scanner note on a PR as the one marked note
// per PR. It drops the note when the PR's head and failing checks match the
// current note, posts a new comment for the first note and when the PR
// author must act (a non-hive author, something red) and has not been told
// for this head, and otherwise edits the current note in place. A new
// comment for the author supersedes the old note: the old one loses its
// marker so there is still exactly one current note.
func (c *Client) upsertScannerNote(ctx context.Context, repo string, number int, agent, body string) (scannerNoteOutcome, error) {
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		if !githubStatusError(err, http.StatusNotFound) {
			c.logger.Warn("scanner note: PR lookup failed, posting the comment unchanged",
				slog.String("repo", repo), slog.Int("number", number), slog.String("error", err.Error()))
		}
		return scannerNotePassThrough, nil
	}
	head := pr.GetHead().GetSHA()
	if head == "" {
		return scannerNotePassThrough, nil
	}
	failing, checksKnown := c.scannerNoteFailingChecks(ctx, owner, name, pr)
	cur := scannerNoteState{Head: head, Checks: scannerNoteUnknownChecks}
	if checksKnown {
		cur.Checks = scannerNoteFingerprint(failing)
	}

	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return scannerNotePassThrough, err
	}
	var existing *gh.IssueComment
	var prev scannerNoteState
	for _, cm := range comments {
		if cm == nil || !c.markerAuthorTrusted(cm.GetUser()) {
			continue
		}
		if s, ok := parseScannerNoteState(cm.GetBody()); ok {
			existing, prev = cm, s // the latest marked comment is the current note
		}
	}

	if existing != nil && checksKnown && prev.Head == head && prev.Checks == cur.Checks {
		c.logger.Info("scanner note: unchanged head and failing checks, not re-posting",
			slog.String("repo", repo), slog.Int("number", number), slog.String("agent", agent),
			slog.String("head", head), slog.String("checks", cur.Checks), slog.Int64("comment_id", existing.GetID()))
		return scannerNoteDropped, nil
	}

	mustAct := len(failing) > 0 && c.knowsOwnAccounts() && !c.isHiveAccount(pr.GetUser())
	if existing == nil || (mustAct && prev.Told != head) {
		if mustAct {
			cur.Told = head
		}
		if err := c.CreateIssueCommentBudgeted(ctx, owner+"/"+name, number, renderScannerNote(cur, body)); err != nil {
			return scannerNotePassThrough, err
		}
		if existing != nil {
			// Best effort: a stale marker left behind only means the next
			// pass still finds the newer note, which is listed later.
			if _, _, err := c.client.Issues.EditComment(ctx, owner, name, existing.GetID(), &gh.IssueComment{Body: gh.Ptr(stripScannerNoteMarker(existing.GetBody()))}); err != nil { // writeback:solicited the scanner asked for this note (#18451)
				c.logger.Warn("scanner note: could not retire the superseded note's marker",
					slog.String("repo", repo), slog.Int("number", number), slog.String("error", err.Error()))
			} else {
				c.noteCommentEdited()
			}
		}
		return scannerNoteCreated, nil
	}

	cur.Told = prev.Told
	if _, _, err := c.client.Issues.EditComment(ctx, owner, name, existing.GetID(), &gh.IssueComment{Body: gh.Ptr(renderScannerNote(cur, body))}); err != nil { // writeback:solicited the scanner asked for this note (#18451)
		return scannerNotePassThrough, err
	}
	c.noteCommentEdited()
	return scannerNoteEdited, nil
}

// scannerNoteFailingChecks is the PR head's failing set: the red gating
// checks, a failed DCO context, and a merge conflict. ok is false when the
// checks could not be read.
func (c *Client) scannerNoteFailingChecks(ctx context.Context, owner, name string, pr *gh.PullRequest) ([]string, bool) {
	summary, err := SummarizeCommitChecks(ctx, c.client, owner, name, pr.GetHead().GetSHA())
	if err != nil {
		c.logger.Warn("scanner note: reading head checks failed",
			slog.String("repo", owner+"/"+name), slog.Int("number", pr.GetNumber()), slog.String("error", err.Error()))
		return nil, false
	}
	failing := append([]string(nil), summary.Red...)
	if summary.DCO < 0 {
		failing = append(failing, scannerNoteDCOCheck)
	}
	if strings.EqualFold(pr.GetMergeableState(), mergeableStateDirty) {
		failing = append(failing, scannerNoteConflictCheck)
	}
	return failing, true
}

// relayScannerNote routes a relayed comment request through the scanner note
// upsert when it applies: the toggle is on, the scanner lane sent it, it is
// not a blocked-on-PR park note, and the body is a plain note. handled is
// false when the caller should post the comment the usual way. A dropped
// note is reported to the agent as already existing, so it knows nothing
// new was needed.
func (c *Client) relayScannerNote(ctx context.Context, req IssueRequest, body string, resp *IssueResponse) (handled bool, err error) {
	if !scannerNoteEnabled() || !isScannerAgent(req.Agent) || req.BlockedOnPR > 0 || !scannerNoteCandidate(body) {
		return false, nil
	}
	outcome, err := c.upsertScannerNote(ctx, req.Repo, req.Number, req.Agent, body)
	if err != nil {
		return true, err
	}
	switch outcome {
	case scannerNotePassThrough:
		return false, nil
	case scannerNoteDropped:
		resp.AlreadyExisted = true
		c.noteCommentSuppressed()
	case scannerNoteEdited:
		c.noteCommentSuppressed()
	case scannerNoteCreated:
	}
	return true, nil
}
