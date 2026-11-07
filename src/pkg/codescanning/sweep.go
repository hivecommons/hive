package codescanning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoAccess is returned by Forge.ListAlerts when GitHub refuses the read:
// the App lacks "Code scanning alerts: read" (security_events), answered as
// a 403, or a 404 GitHub uses for "no access".
var ErrNoAccess = errors.New("code scanning alerts are not readable")

// ErrNotEnabled is returned by Forge.ListAlerts when the repository has no
// code scanning analysis at all.
var ErrNotEnabled = errors.New("code scanning is not set up")

// Notice kinds on Status.
const (
	NoticeMissingPermission = "missing_permission"
	NoticeNotEnabled        = "not_enabled"
)

// TrackedIssue is one of the hive's own code scanning issues, open or
// closed, as the forge listed it.
type TrackedIssue struct {
	Number int
	Open   bool
	Body   string
	Marker Marker
}

// Forge is what Run needs from GitHub. pkg/github implements it; every write
// goes through the write-back gate there.
type Forge interface {
	// ListAlerts lists the repo's alerts in state ("open" or "dismissed"),
	// reading at most maxPages pages. complete is false when the bound
	// stopped the listing early.
	ListAlerts(ctx context.Context, repo, state string, maxPages int) (alerts []Alert, complete bool, err error)
	// ListIssues lists every issue the hive filed with a code scanning
	// marker, open and closed.
	ListIssues(ctx context.Context, repo string) ([]TrackedIssue, error)
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (int, error)
	EditIssueBody(ctx context.Context, repo string, number int, body string) error
	// CloseIssue comments, then closes with stateReason ("completed" or
	// "not_planned").
	CloseIssue(ctx context.Context, repo string, number int, comment, stateReason string) error
	// FileAt reads a text file at ref ("" = the default branch HEAD).
	// found is false when the file does not exist there.
	FileAt(ctx context.Context, repo, path, ref string) (content string, found bool, err error)
	// Upstream returns the fork parent ("owner/repo"), or "" when repo is
	// not a fork.
	Upstream(ctx context.Context, repo string) (string, error)
	// LastCommit returns the newest commit on the default branch that
	// touched path.
	LastCommit(ctx context.Context, repo, path string) (string, error)
}

// Options configures one repo's pass.
type Options struct {
	Repo   string
	Policy Policy
	// MaxPages bounds the open-alert listing (100 per page). 0 means
	// DefaultMaxPages.
	MaxPages int
	// Labels go on every new issue.
	Labels []string
	// WorkflowsLane is the lane that can push workflow files in this repo
	// ("" when none can); WorkflowsLaneLabel routes an issue to it.
	WorkflowsLane      string
	WorkflowsLaneLabel string
	Now                time.Time
}

// DefaultMaxPages bounds the open-alert listing at 1,000 alerts.
const DefaultMaxPages = 10

// dismissedMaxPages bounds the dismissed-alert listing, read only when a
// tracked alert has left the open list.
const dismissedMaxPages = 2

// Status is one repo's code scanning picture after a pass, for the
// dashboard.
type Status struct {
	Repo        string    `json:"repo"`
	SecurityURL string    `json:"security_url"`
	CheckedAt   time.Time `json:"checked_at"`
	// OpenAlerts and BySeverity count every open alert, filed or not, on
	// the sweep's scale (critical, high, medium, low, note).
	OpenAlerts int            `json:"open_alerts"`
	BySeverity map[string]int `json:"by_severity"`
	// Qualifying are open alerts the repo's filter would file.
	Qualifying int `json:"qualifying"`
	// BelowThreshold are below min_severity; Excluded are left out by
	// tool or rule (Scorecard's repo-posture checks by default).
	BelowThreshold int `json:"below_threshold"`
	Excluded       int `json:"excluded"`
	// Tracked are open alerts listed on an open hive issue.
	Tracked int `json:"tracked"`
	// TrackedClosed are open alerts whose hive issue was closed (by a
	// person, or before the analysis caught up); they are not filed again.
	TrackedClosed int `json:"tracked_closed"`
	// Untracked are qualifying alerts with no issue yet, waiting for the
	// per-pass cap.
	Untracked  int `json:"untracked"`
	OpenIssues int `json:"open_issues"`
	// Filed, Updated and Closed are this pass's writes.
	Filed   int `json:"filed"`
	Updated int `json:"updated"`
	Closed  int `json:"closed"`
	// Truncated is true when the open-alert listing hit its page bound;
	// fixed-alert detection and closing are skipped for that pass.
	Truncated bool `json:"truncated,omitempty"`
	// Notice is an operator-facing problem (a missing App permission, code
	// scanning not set up) and NoticeKind its machine name.
	Notice     string `json:"notice,omitempty"`
	NoticeKind string `json:"notice_kind,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SecurityURL is the repo's code scanning page.
func SecurityURL(repo string) string {
	return "https://github.com/" + repo + "/security/code-scanning"
}

// NoticeFor is the dashboard text for a listing error, or "" when the error
// is not a notice.
func NoticeFor(repo string, err error) (kind, text string) {
	switch {
	case errors.Is(err, ErrNoAccess):
		return NoticeMissingPermission, fmt.Sprintf("The hive cannot read code scanning alerts in %s. Give the GitHub App the repository permission \"Code scanning alerts: Read-only\" (security_events) and accept the permission update on the installation.", repo)
	case errors.Is(err, ErrNotEnabled):
		return NoticeNotEnabled, fmt.Sprintf("%s has no code scanning analysis. Set up CodeQL or another SARIF uploader, or turn the code scanning sweep off for this repo.", repo)
	}
	return "", ""
}

// Run makes one pass over a repo: list open alerts and the hive's own
// issues, close or update issues whose alerts changed, add new alerts to the
// issue already covering their rule and file, and file at most
// Policy.MaxNewIssues new grouped issues. A failed read writes nothing.
func Run(ctx context.Context, f Forge, o Options) (Status, error) {
	st := Status{Repo: o.Repo, SecurityURL: SecurityURL(o.Repo), CheckedAt: o.Now, BySeverity: map[string]int{}}
	maxPages := o.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	open, complete, err := f.ListAlerts(ctx, o.Repo, "open", maxPages)
	if err != nil {
		st.NoticeKind, st.Notice = NoticeFor(o.Repo, err)
		return st, err
	}
	st.Truncated = !complete
	openByNum := map[int]Alert{}
	for _, a := range open {
		openByNum[a.Number] = a
		st.OpenAlerts++
		if l := a.Level(); l != LevelNone {
			st.BySeverity[l.String()]++
		}
	}
	issues, err := f.ListIssues(ctx, o.Repo)
	if err != nil {
		// Never file without the dedupe state.
		return st, fmt.Errorf("listing code scanning issues: %w", err)
	}
	seen := map[int]bool{}
	openTracked := map[int]bool{}
	var openIssues []*TrackedIssue
	for i := range issues {
		is := &issues[i]
		for _, n := range is.Marker.Alerts {
			seen[n] = true
			if is.Open {
				openTracked[n] = true
			}
		}
		if is.Open {
			openIssues = append(openIssues, is)
		}
	}

	// Alerts that left the open list since an open issue was written.
	var dismissed map[int]bool
	if complete {
		for _, is := range openIssues {
			for _, n := range is.Marker.Alerts {
				if _, ok := openByNum[n]; !ok && dismissed == nil {
					ds, _, derr := f.ListAlerts(ctx, o.Repo, "dismissed", dismissedMaxPages)
					if derr != nil {
						return st, fmt.Errorf("listing dismissed alerts: %w", derr)
					}
					dismissed = map[int]bool{}
					for _, d := range ds {
						dismissed[d.Number] = true
					}
				}
			}
		}
	}

	// New qualifying alerts: join an open issue that covers them, or wait
	// for a new group.
	appended := map[int][]Alert{}
	var pool []Alert
	for _, a := range open {
		switch o.Policy.Judge(a) {
		case BelowThreshold:
			st.BelowThreshold++
		case Excluded:
			st.Excluded++
		default:
			st.Qualifying++
		}
		switch {
		case openTracked[a.Number]:
			st.Tracked++
			continue
		case seen[a.Number]:
			st.TrackedClosed++
			continue
		}
		if o.Policy.Judge(a) != Qualifies {
			continue
		}
		joined := false
		for _, is := range openIssues {
			if is.Marker.covers(a) {
				appended[is.Number] = append(appended[is.Number], a)
				joined = true
				break
			}
		}
		if joined {
			st.Tracked++
			continue
		}
		pool = append(pool, a)
	}

	// Existing open issues: close when every alert is gone, else refresh the
	// table when something changed.
	for _, is := range openIssues {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		var still []Alert
		gone := map[int]AlertStatus{}
		for _, n := range is.Marker.Alerts {
			if a, ok := openByNum[n]; ok {
				still = append(still, a)
				continue
			}
			if !complete {
				continue
			}
			if dismissed[n] {
				gone[n] = StatusDismissed
			} else {
				gone[n] = StatusFixed
			}
		}
		add := appended[is.Number]
		if complete && len(still) == 0 && len(add) == 0 {
			reason := "completed"
			allDismissed := true
			for _, s := range gone {
				allDismissed = allDismissed && s == StatusDismissed
			}
			if allDismissed {
				reason = "not_planned"
			}
			if err := f.CloseIssue(ctx, o.Repo, is.Number, CloseComment(gone), reason); err != nil { // writeback:gated — codeScanningForge (pkg/github) consults WritebackAllowed
				return st, fmt.Errorf("closing #%d: %w", is.Number, err)
			}
			st.Closed++
			continue
		}
		st.OpenIssues++
		m := is.Marker
		for _, a := range add {
			m.Alerts = mergeInts(m.Alerts, []int{a.Number})
			if p := a.Path(); p != "" && !containsString(m.Paths, p) {
				m.Paths = append(m.Paths, p)
			}
		}
		rows := append(append([]Alert(nil), still...), add...)
		sortAlerts(rows)
		if !complete {
			// Without the full open list, absent alerts are unknown: keep
			// their old rows as they were.
			for n, row := range previousRows(is.Body) {
				if _, ok := openByNum[n]; !ok && containsInt(m.Alerts, n) {
					gone[n] = AlertStatus(rowStatus(row))
				}
			}
		}
		body := rewriteBody(is.Body, m, alertsSection(o.Repo, rows, gone, previousRows(is.Body)))
		if strings.TrimSpace(body) == strings.TrimSpace(is.Body) {
			continue
		}
		if err := f.EditIssueBody(ctx, o.Repo, is.Number, body); err != nil { // writeback:gated — codeScanningForge (pkg/github) consults WritebackAllowed
			return st, fmt.Errorf("updating #%d: %w", is.Number, err)
		}
		st.Updated++
	}

	// New groups, highest level first, at most the cap.
	groups := Cluster(pool)
	limit := o.Policy.maxNewIssues()
	var upstream string
	upstreamRead := false
	for _, g := range groups {
		if st.Filed >= limit {
			st.Untracked += len(g.Alerts)
			continue
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		in := BodyInput{Repo: o.Repo, Group: g}
		if !g.WorkflowOnly() || o.WorkflowsLane == "" {
			in.WorkflowsLane = ""
		} else {
			in.WorkflowsLane = o.WorkflowsLane
		}
		var source string
		if g.Package == "" && len(g.Paths) > 0 {
			first := g.Alerts[0]
			if content, found, ferr := f.FileAt(ctx, o.Repo, first.Path(), first.Instance.CommitSHA); ferr == nil && found {
				source = content
				in.Excerpt, in.ExcerptPath = Excerpt(content, g.Alerts, first.Path()), first.Path()
			}
			if !upstreamRead {
				upstreamRead = true
				upstream, _ = f.Upstream(ctx, o.Repo)
			}
			if upstream != "" && source != "" {
				in.Upstream = upstreamNote(ctx, f, upstream, first.Path(), source, g.Alerts)
			}
		}
		labels := append([]string(nil), o.Labels...)
		if in.WorkflowsLane != "" && o.WorkflowsLaneLabel != "" {
			labels = append(labels, o.WorkflowsLaneLabel)
		}
		if _, err := f.CreateIssue(ctx, o.Repo, Title(g), Body(in), labels); err != nil { // writeback:gated — codeScanningForge (pkg/github) consults WritebackAllowed
			return st, fmt.Errorf("filing %s: %w", Title(g), err)
		}
		st.Filed++
		st.OpenIssues++
		st.Tracked += len(g.Alerts)
	}
	return st, nil
}

func containsInt(vs []int, n int) bool {
	for _, v := range vs {
		if v == n {
			return true
		}
	}
	return false
}

func rowStatus(row string) string {
	parts := strings.SplitN(row, " | ", 3)
	if len(parts) < 2 {
		return string(StatusOpen)
	}
	return strings.TrimSpace(parts[1])
}

// excerptContext is the lines shown around each flagged line.
const excerptContext = 3

// maxExcerptLines bounds the excerpt.
const maxExcerptLines = 40

// Excerpt returns numbered source lines around the group's alerts in file,
// at most maxExcerptLines of them.
func Excerpt(content string, alerts []Alert, file string) string {
	lines := strings.Split(content, "\n")
	show := map[int]bool{}
	flagged := map[int]bool{}
	for _, a := range alerts {
		if a.Path() != file {
			continue
		}
		start, end := a.Instance.Location.StartLine, a.Instance.Location.EndLine
		if start <= 0 || start > len(lines) {
			continue
		}
		end = max(end, start)
		for l := start; l <= end; l++ {
			flagged[l] = true
		}
		for l := max(1, start-excerptContext); l <= min(len(lines), end+excerptContext); l++ {
			show[l] = true
		}
	}
	var b strings.Builder
	shown, last := 0, 0
	for l := 1; l <= len(lines) && shown < maxExcerptLines; l++ {
		if !show[l] {
			continue
		}
		if last != 0 && l != last+1 {
			b.WriteString("...\n")
		}
		mark := " "
		if flagged[l] {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s%5d  %s\n", mark, l, strings.ReplaceAll(lines[l-1], "```", "` ` `"))
		shown++
		last = l
	}
	return b.String()
}

// upstreamNote is the fork check: does the fork's parent still have the
// flagged lines at HEAD? Two reads at most: the upstream file, and, when the
// lines are gone, the last upstream commit to touch it.
func upstreamNote(ctx context.Context, f Forge, upstream, file, source string, alerts []Alert) string {
	content, found, err := f.FileAt(ctx, upstream, file, "")
	if err != nil {
		return ""
	}
	if !found {
		return fmt.Sprintf("This repository is a fork of `%s`, where `%s` no longer exists at HEAD (moved or removed). Check upstream before fixing it here; the fix may be a port.", upstream, file)
	}
	lines := strings.Split(source, "\n")
	var flagged, present int
	for _, a := range alerts {
		if a.Path() != file {
			continue
		}
		n := a.Instance.Location.StartLine
		if n <= 0 || n > len(lines) {
			continue
		}
		text := strings.TrimSpace(lines[n-1])
		if len(text) < 6 {
			continue
		}
		flagged++
		if strings.Contains(content, text) {
			present++
		}
	}
	switch {
	case flagged == 0:
		return ""
	case present == flagged:
		return fmt.Sprintf("This repository is a fork of `%s`. Upstream HEAD still has the flagged code in `%s`, so there is no upstream fix to port; a fix made here may be worth offering upstream.", upstream, file)
	}
	sha, err := f.LastCommit(ctx, upstream, file)
	if err != nil || sha == "" {
		return fmt.Sprintf("This repository is a fork of `%s`, and upstream has changed the flagged code in `%s`. Check whether upstream already fixed it and port that change rather than writing a new fix.", upstream, file)
	}
	return fmt.Sprintf("This repository is a fork of `%s`, and upstream has changed the flagged code in `%s` (last upstream change: %s). If that change fixes the alert, port upstream %s rather than writing a new fix: https://github.com/%s/commit/%s", upstream, file, shortSHA(sha), shortSHA(sha), upstream, sha)
}
