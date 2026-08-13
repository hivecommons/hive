package advisory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/logscrub"
)

const advisoryDir = "/data/advisory"

// Finding represents a single advisory finding from an agent.
type Finding struct {
	Agent     string    `json:"agent"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	File      string    `json:"file,omitempty"`
	Line      int       `json:"line,omitempty"`
}

// ResolvedFinding is a closed advisory bead shown in the "Recently Resolved" section.
type ResolvedFinding struct {
	Agent    string    `json:"agent"`
	Title    string    `json:"title"`
	ClosedAt time.Time `json:"closed_at"`
	File     string    `json:"file,omitempty"`
}

// Digest is a consolidated summary of findings across agents.
type Digest struct {
	GeneratedAt      time.Time            `json:"generated_at"`
	Mode             string               `json:"mode"`
	ByAgent          map[string][]Finding `json:"by_agent"`
	TotalCount       int                  `json:"total_count"`
	RecentlyResolved []ResolvedFinding    `json:"recently_resolved,omitempty"`
}

// Store manages advisory findings on disk.
type Store struct {
	dir          string
	mu           sync.Mutex
	lastReadPos  map[string]int64
	latestDigest *Digest
}

func NewStore() *Store {
	_ = os.MkdirAll(advisoryDir, 0o755)
	return &Store{
		dir:         advisoryDir,
		lastReadPos: make(map[string]int64),
	}
}

// ReadNewFindings reads all findings written since the last read for each agent.
func (s *Store) ReadNewFindings() ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading advisory dir: %w", err)
	}

	var all []Finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		agentName := strings.TrimSuffix(e.Name(), ".jsonl")
		path := filepath.Join(s.dir, e.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		lastPos := s.lastReadPos[agentName]
		if int64(len(data)) <= lastPos {
			continue
		}

		newData := string(data[lastPos:])
		s.lastReadPos[agentName] = int64(len(data))

		for _, line := range strings.Split(newData, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var f Finding
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				continue
			}
			if f.Agent == "" {
				f.Agent = agentName
			}
			all = append(all, f)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all, nil
}

// BuildDigest creates a digest from the given findings.
func BuildDigest(findings []Finding, mode string) *Digest {
	byAgent := make(map[string][]Finding)
	for _, f := range findings {
		byAgent[f.Agent] = append(byAgent[f.Agent], f)
	}
	return &Digest{
		GeneratedAt: time.Now(),
		Mode:        mode,
		ByAgent:     byAgent,
		TotalCount:  len(findings),
	}
}

// isAdvisoryBeadType returns true for bead types that represent actionable
// findings for repo owners. Internal agent work items (task, decision) are
// excluded so the advisory digest stays high-signal.
func isAdvisoryBeadType(t beads.BeadType) bool {
	switch t {
	case beads.TypeAdvisory, beads.TypeBug, beads.TypeFeature:
		return true
	default:
		return false
	}
}

const recentlyResolvedWindow = 48 * time.Hour

// BuildDigestFromBeads creates a digest by reading open advisory beads from all
// agent bead stores. Only advisory/bug/feature beads are included — task and
// decision beads are internal agent work items, not findings for repo owners.
// Beads closed within the last 48 hours are included in RecentlyResolved.
func BuildDigestFromBeads(stores map[string]*beads.Store, mode string) *Digest {
	byAgent := make(map[string][]Finding)
	var resolved []ResolvedFinding
	total := 0
	cutoff := time.Now().Add(-recentlyResolvedWindow)
	for agentName, store := range stores {
		seen := make(map[string]bool)
		items := store.List(beads.ListFilter{})
		sortAdvisoryBeads(items)
		for _, b := range items {
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if b.Title == "" {
				continue
			}
			// Both closed AND done beads are resolved: agents retire findings
			// with either status, and a "done" finding lingering in the digest
			// as if still open is exactly the staleness #2575 is about.
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				// Done beads carry no ClosedAt (only Close sets it); their
				// UpdatedAt is when the agent marked them done.
				resolvedAt := b.UpdatedAt.Time
				if b.ClosedAt != nil {
					resolvedAt = b.ClosedAt.Time
				}
				if resolvedAt.After(cutoff) {
					resolved = append(resolved, ResolvedFinding{
						Agent:    agentName,
						Title:    b.Title,
						ClosedAt: resolvedAt,
						File:     b.ExternalRef,
					})
				}
				continue
			}
			if seen[b.Title] {
				continue
			}
			seen[b.Title] = true
			f := Finding{
				Agent:     agentName,
				Timestamp: b.CreatedAt.Time,
				Type:      string(b.Type),
				Severity:  beadPriorityToSeverity(b.Priority),
				Title:     b.Title,
				Detail:    b.Notes,
				File:      b.ExternalRef,
			}
			if ft := b.Meta("finding_type"); ft != "" {
				f.Type = ft
			}
			if d := b.Meta("detail"); d != "" && f.Detail == "" {
				f.Detail = d
			}
			byAgent[agentName] = append(byAgent[agentName], f)
			total++
		}
	}
	sort.Slice(resolved, func(i, j int) bool {
		if !resolved[i].ClosedAt.Equal(resolved[j].ClosedAt) {
			return resolved[i].ClosedAt.After(resolved[j].ClosedAt)
		}
		return resolvedFindingSortKey(resolved[i]) < resolvedFindingSortKey(resolved[j])
	})
	const maxRecentlyResolved = 100
	if len(resolved) > maxRecentlyResolved {
		resolved = resolved[:maxRecentlyResolved]
	}
	return &Digest{
		GeneratedAt:      time.Now(),
		Mode:             mode,
		ByAgent:          byAgent,
		TotalCount:       total,
		RecentlyResolved: resolved,
	}
}

// sortAdvisoryBeads preserves the existing one-open-finding-per-title digest
// contract while making its representative deterministic. Store.List orders by
// CreatedAt, but distinct beads imported in one batch can share that timestamp;
// their map-derived tie order otherwise changes across process restarts.
func sortAdvisoryBeads(items []*beads.Bead) {
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if !left.CreatedAt.Equal(right.CreatedAt.Time) {
			return left.CreatedAt.Before(right.CreatedAt.Time)
		}
		return advisoryBeadSortKey(left) < advisoryBeadSortKey(right)
	})
}

func advisoryBeadSortKey(b *beads.Bead) string {
	return strings.Join([]string{
		b.Title,
		b.ExternalRef,
		b.ID,
		string(b.Type),
		strconv.Itoa(int(b.Priority)),
		b.Actor,
		b.Notes,
	}, "\x00")
}

func resolvedFindingSortKey(f ResolvedFinding) string {
	return strings.Join([]string{f.Agent, f.Title, f.File}, "\x00")
}

func beadPriorityToSeverity(p beads.Priority) string {
	switch p {
	case beads.PriorityCritical:
		return "critical"
	case beads.PriorityHigh:
		return "high"
	case beads.PriorityMedium:
		return "medium"
	case beads.PriorityLow:
		return "low"
	default:
		return "info"
	}
}

var (
	// ghNumRefPattern matches ExternalRefs of the form "gh-123" — an issue/PR
	// number without a repo. The repo is resolved from the finding title when
	// it mentions the same number, falling back to the hive's primary repo.
	ghNumRefPattern = regexp.MustCompile(`^gh-(\d+)$`)
	// inlineRefPattern matches "repo#123" or "owner/repo#123" tokens inside
	// free text. Bare "#123" is intentionally not matched — GitHub already
	// autolinks it against the repo the digest is posted to.
	inlineRefPattern = regexp.MustCompile(`([A-Za-z0-9][A-Za-z0-9_.-]*/)?[A-Za-z0-9][A-Za-z0-9_.-]*#[0-9]+\b`)
)

// issueURL builds the canonical GitHub URL for an issue or PR reference.
// The /issues/ path also resolves for pull requests (GitHub redirects).
func issueURL(owner, repo string, num int) string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, num)
}

// splitInlineRef parses a "repo#123" or "owner/repo#123" token, defaulting the
// owner to org when absent. Returns ok=false if the token is malformed.
func splitInlineRef(ref, org string) (owner, repo string, num int, ok bool) {
	hash := strings.LastIndex(ref, "#")
	if hash < 0 {
		return "", "", 0, false
	}
	num, err := strconv.Atoi(ref[hash+1:])
	if err != nil {
		return "", "", 0, false
	}
	owner = org
	repo = ref[:hash]
	if slash := strings.Index(repo, "/"); slash >= 0 {
		owner = repo[:slash]
		repo = repo[slash+1:]
	}
	if owner == "" || repo == "" {
		return "", "", 0, false
	}
	return owner, repo, num, true
}

// linkifyRefs rewrites "repo#123" / "owner/repo#123" tokens in free text as
// explicit markdown links. GitHub only autolinks bare "#123" (same repo) and
// fully qualified "owner/repo#123" — agent findings usually write "repo#123",
// which renders as dead text (kubestellar/hive#1914). Tokens preceded by '/',
// '[' or '`' are left alone: they are part of a URL, an existing markdown
// link, or a code span.
func linkifyRefs(text, org string) string {
	if org == "" || text == "" {
		return text
	}
	matches := inlineRefPattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > 0 {
			switch text[start-1] {
			case '/', '[', '`':
				continue
			}
		}
		ref := text[start:end]
		owner, repo, num, ok := splitInlineRef(ref, org)
		if !ok {
			continue
		}
		b.WriteString(text[last:start])
		fmt.Fprintf(&b, "[%s](%s)", ref, issueURL(owner, repo, num))
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

// formatFindingRef renders a finding's ExternalRef as a leading-space suffix
// for the digest line. GitHub issue/PR refs ("gh-123", "repo#123",
// "owner/repo#123") become markdown links; file references keep the previous
// `file:line` code-span rendering. Returns "" when ref is empty.
func formatFindingRef(ref string, line int, org, primaryRepo, title string) string {
	if ref == "" {
		return ""
	}
	if org != "" {
		if m := ghNumRefPattern.FindStringSubmatch(ref); m != nil && primaryRepo != "" {
			num, _ := strconv.Atoi(m[1])
			owner, repo := org, primaryRepo
			// A "gh-123" ref carries no repo; if the title names the same
			// number ("some-repo#123"), that repo is the better target.
			if o, r, ok := repoHintFromTitle(title, num, org); ok {
				owner, repo = o, r
			}
			return fmt.Sprintf(" [#%d](%s)", num, issueURL(owner, repo, num))
		}
		if inlineRefPattern.FindString(ref) == ref {
			if owner, repo, num, ok := splitInlineRef(ref, org); ok {
				return fmt.Sprintf(" [%s](%s)", ref, issueURL(owner, repo, num))
			}
		}
	}
	if line > 0 {
		return fmt.Sprintf(" `%s:%d`", ref, line)
	}
	return fmt.Sprintf(" `%s`", ref)
}

// repoHintFromTitle scans a finding title for a "repo#num" token matching num
// and returns that owner/repo.
func repoHintFromTitle(title string, num int, org string) (string, string, bool) {
	for _, ref := range inlineRefPattern.FindAllString(title, -1) {
		owner, repo, n, ok := splitInlineRef(ref, org)
		if ok && n == num {
			return owner, repo, true
		}
	}
	return "", "", false
}

// FormatDigestMarkdown formats a digest as markdown for posting to GitHub.
// Findings are grouped by severity (high→low) with a summary table, then
// listed with their source agent — this gives repo owners a quick "what matters"
// view without reading per-agent sections. org and primaryRepo identify where
// the digest is posted; they are used to turn issue/PR references in findings
// into working links, and may be empty to skip linkification.
func FormatDigestMarkdown(d *Digest, org, primaryRepo string) string {
	if d.TotalCount == 0 {
		// No open findings. If nothing was recently resolved either, there is
		// nothing to say — return "" so no digest comment is created. But when
		// findings WERE just resolved, an updated digest must still be posted:
		// otherwise the pinned comment freezes on its last non-empty state and
		// keeps showing healed findings forever (#2575).
		if len(d.RecentlyResolved) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("## 🐝 Advisory Digest — %s\n\n", d.GeneratedAt.Format("2006-01-02 15:04 MST")))
		b.WriteString("> Automated code review findings from [Hive](https://github.com/kubestellar/hive) agents. ")
		b.WriteString("This comment is updated periodically.\n\n")
		b.WriteString("**Findings:** 0 — all previously reported findings are resolved. ✅\n\n")
		writeRecentlyResolved(&b, d, org, primaryRepo)
		return NeutralizeMentions(b.String())
	}

	var all []Finding
	for _, findings := range d.ByAgent {
		all = append(all, findings...)
	}

	bySeverity := map[string][]Finding{}
	for _, f := range all {
		sev := f.Severity
		if sev == "" {
			sev = "info"
		}
		bySeverity[sev] = append(bySeverity[sev], f)
	}

	sevOrder := []string{"critical", "high", "medium", "low", "info"}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 🐝 Advisory Digest — %s\n\n", d.GeneratedAt.Format("2006-01-02 15:04 MST")))
	b.WriteString("> Automated code review findings from [Hive](https://github.com/kubestellar/hive) agents. ")
	b.WriteString("Each finding includes a file reference and suggested fix. This comment is updated periodically.\n\n")
	b.WriteString(fmt.Sprintf("**Findings:** %d\n\n", d.TotalCount))

	b.WriteString("| Severity | Count |\n|----------|-------|\n")
	for _, sev := range sevOrder {
		items := bySeverity[sev]
		if len(items) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("| %s %s | %d |\n", severityIcon(sev), sev, len(items)))
	}
	b.WriteString("\n")

	for _, sev := range sevOrder {
		items, ok := bySeverity[sev]
		if !ok {
			continue
		}
		sort.Slice(items, func(i, j int) bool {
			return findingSortKey(items[i]) < findingSortKey(items[j])
		})
		icon := severityIcon(sev)
		b.WriteString(fmt.Sprintf("### %s %s (%d)\n\n", icon, strings.ToUpper(sev), len(items)))
		for _, f := range items {
			loc := formatFindingRef(logscrub.ScrubString(f.File), f.Line, org, primaryRepo, f.Title)
			title := logscrub.ScrubString(f.Title)
			detail := logscrub.ScrubString(f.Detail)
			b.WriteString(fmt.Sprintf("- **[%s]** %s%s _%s_\n", f.Type, linkifyRefs(title, org), loc, f.Agent))
			if detail != "" {
				b.WriteString(fmt.Sprintf("  > %s\n", linkifyRefs(detail, org)))
			}
		}
		b.WriteString("\n")
	}

	writeRecentlyResolved(&b, d, org, primaryRepo)

	// Findings quote agent output verbatim and routinely name humans
	// ("PR #665, by @omerap12"). The digest comment is rewritten every cycle,
	// so any raw mention would re-notify that person on every refresh —
	// neutralize the whole body before it goes anywhere near GitHub.
	return NeutralizeMentions(b.String())
}

// findingSortKey orders every rendered field so an unchanged advisory set
// produces byte-identical markdown even when bead-store iteration order changes
// across process restarts. Timestamp is intentionally excluded because it is not
// rendered in the digest.
func findingSortKey(f Finding) string {
	return strings.Join([]string{
		f.Agent,
		f.Type,
		f.Title,
		f.File,
		strconv.Itoa(f.Line),
		f.Detail,
	}, "\x00")
}

// writeRecentlyResolved renders the "Recently Resolved" digest section. Shared
// by the normal and the zero-findings ("all clear") digest renderings.
func writeRecentlyResolved(b *strings.Builder, d *Digest, org, primaryRepo string) {
	if len(d.RecentlyResolved) == 0 {
		return
	}
	b.WriteString(fmt.Sprintf("### ✅ Recently Resolved (%d)\n\n", len(d.RecentlyResolved)))
	for _, r := range d.RecentlyResolved {
		loc := formatFindingRef(r.File, 0, org, primaryRepo, r.Title)
		b.WriteString(fmt.Sprintf("- ~~%s~~%s _%s — resolved %s_\n", linkifyRefs(logscrub.ScrubString(r.Title), org), loc, r.Agent, r.ClosedAt.Format("Jan 2")))
	}
	b.WriteString("\n")
}

// SetLatestDigest stores the most recent digest for dashboard access.
func (s *Store) SetLatestDigest(d *Digest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestDigest = d
}

// LatestDigest returns the most recent digest.
func (s *Store) LatestDigest() *Digest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latestDigest
}

// severityToPriority maps advisory severity strings to bead priority values.
func severityToPriority(sev string) beads.Priority {
	switch sev {
	case "critical":
		return beads.PriorityCritical
	case "high":
		return beads.PriorityHigh
	case "medium":
		return beads.PriorityMedium
	case "low":
		return beads.PriorityLow
	default:
		return beads.PriorityMinor
	}
}

// PersistAsBeads stores advisory findings as beads in the given bead stores,
// keyed by agent name. Findings are deduplicated by title — if a bead with the
// same title already exists for an agent, it is skipped.
func PersistAsBeads(findings []Finding, stores map[string]*beads.Store) (created int) {
	for _, f := range findings {
		store, ok := stores[f.Agent]
		if !ok {
			continue
		}

		existing := store.List(beads.ListFilter{})
		dup := false
		for _, b := range existing {
			// Only OPEN beads suppress a duplicate. A resolved (closed/done)
			// bead must not: if the underlying condition recurs after healing —
			// e.g. repo permissions were fixed, the finding auto-closed, and
			// the permissions later regressed — the re-reported finding has to
			// become a fresh open bead or the digest would stay silent about a
			// genuinely-failing condition (#2575).
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			if b.Title == f.Title && b.Type == beads.TypeAdvisory {
				dup = true
				break
			}
		}
		if dup {
			continue
		}

		ref := ""
		if f.File != "" {
			ref = logscrub.ScrubString(f.File)
			if f.Line > 0 {
				ref = fmt.Sprintf("%s:%d", ref, f.Line)
			}
		}

		meta := map[string]string{
			"severity":       f.Severity,
			"finding_type":   f.Type,
			"advisory_agent": f.Agent,
		}
		if f.Detail != "" {
			meta["detail"] = logscrub.ScrubString(f.Detail)
		}

		b, err := store.Create(logscrub.ScrubString(f.Title), beads.TypeAdvisory, severityToPriority(f.Severity), f.Agent, ref)
		if err != nil {
			continue
		}
		for k, v := range meta {
			_ = store.SetMetadata(b.ID, k, v)
		}
		created++
	}
	return created
}

func severityIcon(sev string) string {
	switch sev {
	case "critical":
		return "🔴"
	case "high":
		return "🟠"
	case "medium":
		return "🟡"
	case "low":
		return "🔵"
	default:
		return "⚪"
	}
}
