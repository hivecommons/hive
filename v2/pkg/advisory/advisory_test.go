package advisory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestBuildDigest(t *testing.T) {
	findings := []Finding{
		{Agent: "scanner", Severity: "high", Title: "bug1", Type: "bug"},
		{Agent: "scanner", Severity: "low", Title: "bug2", Type: "style"},
		{Agent: "quality", Severity: "medium", Title: "bug3", Type: "perf"},
	}
	d := BuildDigest(findings, "busy")
	if d.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", d.TotalCount)
	}
	if d.Mode != "busy" {
		t.Errorf("Mode = %q, want %q", d.Mode, "busy")
	}
	if len(d.ByAgent["scanner"]) != 2 {
		t.Errorf("scanner findings = %d, want 2", len(d.ByAgent["scanner"]))
	}
	if len(d.ByAgent["quality"]) != 1 {
		t.Errorf("quality findings = %d, want 1", len(d.ByAgent["quality"]))
	}
}

func TestBuildDigestEmpty(t *testing.T) {
	d := BuildDigest(nil, "idle")
	if d.TotalCount != 0 {
		t.Errorf("TotalCount = %d, want 0", d.TotalCount)
	}
	if len(d.ByAgent) != 0 {
		t.Errorf("ByAgent should be empty")
	}
}

func TestFormatDigestMarkdown(t *testing.T) {
	findings := []Finding{
		{Agent: "scanner", Severity: "high", Title: "SQL injection", Type: "security", File: "api.go", Line: 42},
		{Agent: "quality", Severity: "low", Title: "typo in docs", Type: "style"},
	}
	d := BuildDigest(findings, "busy")
	md := FormatDigestMarkdown(d, "", "")
	if md == "" {
		t.Fatal("expected non-empty markdown")
	}
	if !contains(md, "SQL injection") {
		t.Error("missing finding title")
	}
	if !contains(md, "`api.go:42`") {
		t.Error("missing file:line reference")
	}
	if !contains(md, "Advisory Digest") {
		t.Error("missing header")
	}
}

func TestFormatDigestMarkdownEmpty(t *testing.T) {
	d := BuildDigest(nil, "idle")
	md := FormatDigestMarkdown(d, "", "")
	if md != "" {
		t.Errorf("expected empty markdown for 0 findings, got %d chars", len(md))
	}
}

func TestFormatDigestMarkdownWithResolved(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		Mode:        "busy",
		ByAgent:     map[string][]Finding{"scanner": {{Agent: "scanner", Severity: "high", Title: "fixed bug", Type: "bug"}}},
		TotalCount:  1,
		RecentlyResolved: []ResolvedFinding{
			{Agent: "scanner", Title: "old bug", ClosedAt: time.Now(), File: "old.go"},
		},
	}
	md := FormatDigestMarkdown(d, "", "")
	if !contains(md, "Recently Resolved") {
		t.Error("missing Recently Resolved section")
	}
	if !contains(md, "old bug") {
		t.Error("missing resolved finding")
	}
}

func TestFormatDigestMarkdownLinkifiesRefs(t *testing.T) {
	findings := []Finding{
		// gh-N ExternalRef with a repo hint in the title (kubestellar/hive#1914)
		{Agent: "scanner", Severity: "critical", Type: "bug", File: "gh-585",
			Title: "llm-d-fast-model-actuation#585: launcher-populator holds node slots"},
		// gh-N ExternalRef with no hint — falls back to the primary repo
		{Agent: "scanner", Severity: "high", Type: "bug", File: "gh-42", Title: "requester explosion"},
		// owner-qualified ref in detail text
		{Agent: "quality", Severity: "low", Type: "style", Title: "typo",
			Detail: "see other-org/other-repo#7 for context"},
		// file refs keep the code-span rendering
		{Agent: "quality", Severity: "medium", Type: "perf", Title: "slow loop", File: "pkg/a.go", Line: 12},
	}
	d := BuildDigest(findings, "busy")
	md := FormatDigestMarkdown(d, "llm-d-incubation", "llm-d-fast-model-actuation")

	wantLinks := []string{
		"[llm-d-fast-model-actuation#585](https://github.com/llm-d-incubation/llm-d-fast-model-actuation/issues/585)",
		"[#585](https://github.com/llm-d-incubation/llm-d-fast-model-actuation/issues/585)",
		"[#42](https://github.com/llm-d-incubation/llm-d-fast-model-actuation/issues/42)",
		"[other-org/other-repo#7](https://github.com/other-org/other-repo/issues/7)",
		"`pkg/a.go:12`",
	}
	for _, w := range wantLinks {
		if !contains(md, w) {
			t.Errorf("digest missing %q\n%s", w, md)
		}
	}
	if contains(md, "`gh-585`") || contains(md, "`gh-42`") {
		t.Error("gh-N refs should render as links, not code spans")
	}
}

func TestLinkifyRefs(t *testing.T) {
	const org = "kubestellar"
	tests := []struct {
		name, in, want string
	}{
		{"bare repo ref", "fix console#12 now",
			"fix [console#12](https://github.com/kubestellar/console/issues/12) now"},
		{"owner-qualified ref", "see a/b#3",
			"see [a/b#3](https://github.com/a/b/issues/3)"},
		{"bare number left for GitHub autolink", "see #635", "see #635"},
		{"url anchor untouched", "https://github.com/a/b#3", "https://github.com/a/b#3"},
		{"existing link untouched", "[console#12](https://x)", "[console#12](https://x)"},
		{"code span untouched", "`console#12`", "`console#12`"},
		{"no refs", "plain text", "plain text"},
	}
	for _, tt := range tests {
		if got := linkifyRefs(tt.in, org); got != tt.want {
			t.Errorf("%s: linkifyRefs(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
	if got := linkifyRefs("console#12", ""); got != "console#12" {
		t.Errorf("empty org should disable linkification, got %q", got)
	}
}

func TestFormatFindingRef(t *testing.T) {
	const org, repo = "kubestellar", "console"
	tests := []struct {
		name, ref, title, want string
		line                   int
	}{
		{name: "gh-N falls back to primary repo", ref: "gh-9",
			want: " [#9](https://github.com/kubestellar/console/issues/9)"},
		{name: "gh-N uses title repo hint", ref: "gh-9", title: "docs#9: broken link",
			want: " [#9](https://github.com/kubestellar/docs/issues/9)"},
		{name: "repo-qualified ref", ref: "docs#4",
			want: " [docs#4](https://github.com/kubestellar/docs/issues/4)"},
		{name: "file with line", ref: "a.go", line: 3, want: " `a.go:3`"},
		{name: "file without line", ref: "a.go", want: " `a.go`"},
		{name: "empty", ref: "", want: ""},
	}
	for _, tt := range tests {
		if got := formatFindingRef(tt.ref, tt.line, org, repo, tt.title); got != tt.want {
			t.Errorf("%s: formatFindingRef(%q) = %q, want %q", tt.name, tt.ref, got, tt.want)
		}
	}
	if got := formatFindingRef("gh-9", 0, "", "", ""); got != " `gh-9`" {
		t.Errorf("empty org should keep code-span rendering, got %q", got)
	}
}

func TestSeverityIcon(t *testing.T) {
	tests := []struct {
		sev  string
		want string
	}{
		{"critical", "🔴"},
		{"high", "🟠"},
		{"medium", "🟡"},
		{"low", "🔵"},
		{"info", "⚪"},
		{"unknown", "⚪"},
	}
	for _, tt := range tests {
		got := severityIcon(tt.sev)
		if got != tt.want {
			t.Errorf("severityIcon(%q) = %q, want %q", tt.sev, got, tt.want)
		}
	}
}

func TestSeverityToPriority(t *testing.T) {
	tests := []struct {
		sev  string
		want beads.Priority
	}{
		{"critical", beads.PriorityCritical},
		{"high", beads.PriorityHigh},
		{"medium", beads.PriorityMedium},
		{"low", beads.PriorityLow},
		{"unknown", beads.PriorityMinor},
	}
	for _, tt := range tests {
		got := severityToPriority(tt.sev)
		if got != tt.want {
			t.Errorf("severityToPriority(%q) = %d, want %d", tt.sev, got, tt.want)
		}
	}
}

func TestBeadPriorityToSeverity(t *testing.T) {
	tests := []struct {
		p    beads.Priority
		want string
	}{
		{beads.PriorityCritical, "critical"},
		{beads.PriorityHigh, "high"},
		{beads.PriorityMedium, "medium"},
		{beads.PriorityLow, "low"},
		{beads.PriorityMinor, "info"},
		{99, "info"},
	}
	for _, tt := range tests {
		got := beadPriorityToSeverity(tt.p)
		if got != tt.want {
			t.Errorf("beadPriorityToSeverity(%d) = %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestStoreReadNewFindings(t *testing.T) {
	dir := t.TempDir()
	store := &Store{
		dir:         dir,
		lastReadPos: make(map[string]int64),
	}

	f1 := Finding{Agent: "scanner", Severity: "high", Title: "bug1", Timestamp: time.Now()}
	data, _ := json.Marshal(f1)
	os.WriteFile(filepath.Join(dir, "scanner.jsonl"), append(data, '\n'), 0o644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if findings[0].Title != "bug1" {
		t.Errorf("Title = %q, want %q", findings[0].Title, "bug1")
	}

	findings2, err := store.ReadNewFindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings2) != 0 {
		t.Errorf("second read should return 0 new findings, got %d", len(findings2))
	}
}

func TestStoreReadNewFindingsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	store := &Store{
		dir:         dir,
		lastReadPos: make(map[string]int64),
	}

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0", len(findings))
	}
}

func TestStoreReadNewFindingsNonExistentDir(t *testing.T) {
	store := &Store{
		dir:         "/tmp/nonexistent-advisory-dir-" + t.Name(),
		lastReadPos: make(map[string]int64),
	}

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatal(err)
	}
	if findings != nil {
		t.Errorf("expected nil findings for non-existent dir")
	}
}

func TestStoreLatestDigest(t *testing.T) {
	store := &Store{
		dir:         t.TempDir(),
		lastReadPos: make(map[string]int64),
	}

	if store.LatestDigest() != nil {
		t.Error("expected nil initial digest")
	}

	d := &Digest{Mode: "test", TotalCount: 5}
	store.SetLatestDigest(d)

	got := store.LatestDigest()
	if got == nil {
		t.Fatal("expected non-nil digest")
	}
	if got.TotalCount != 5 {
		t.Errorf("TotalCount = %d, want 5", got.TotalCount)
	}
}

func TestIsAdvisoryBeadType(t *testing.T) {
	if !isAdvisoryBeadType(beads.TypeAdvisory) {
		t.Error("TypeAdvisory should be advisory")
	}
	if !isAdvisoryBeadType(beads.TypeBug) {
		t.Error("TypeBug should be advisory")
	}
	if !isAdvisoryBeadType(beads.TypeFeature) {
		t.Error("TypeFeature should be advisory")
	}
	if isAdvisoryBeadType(beads.TypeTask) {
		t.Error("TypeTask should NOT be advisory")
	}
}

func TestPersistAsBeads(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"scanner": store}

	findings := []Finding{
		{Agent: "scanner", Severity: "high", Title: "test finding", Type: "bug", File: "a.go", Line: 10, Detail: "details here"},
	}

	created := PersistAsBeads(findings, stores)
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}

	created2 := PersistAsBeads(findings, stores)
	if created2 != 0 {
		t.Errorf("duplicate should not create, got %d", created2)
	}
}

func TestPersistAsBeadsMissingStore(t *testing.T) {
	stores := map[string]*beads.Store{}
	findings := []Finding{
		{Agent: "unknown", Severity: "low", Title: "no store"},
	}
	created := PersistAsBeads(findings, stores)
	if created != 0 {
		t.Errorf("should create 0 for missing store, got %d", created)
	}
}

func TestBuildDigestFromBeads(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	b1, err := store.Create("sql injection", beads.TypeAdvisory, beads.PriorityHigh, "scanner", "api.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetMetadata(b1.ID, "finding_type", "security")
	_ = store.SetMetadata(b1.ID, "detail", "found SQL injection")

	_, err = store.Create("internal task", beads.TypeTask, beads.PriorityLow, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}

	b3, err := store.Create("old bug", beads.TypeBug, beads.PriorityMedium, "scanner", "old.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close(b3.ID)

	stores := map[string]*beads.Store{"scanner": store}
	d := BuildDigestFromBeads(stores, "busy")

	if d.TotalCount != 1 {
		t.Errorf("TotalCount = %d, want 1 (only advisory types, open only)", d.TotalCount)
	}
	if len(d.ByAgent["scanner"]) != 1 {
		t.Errorf("scanner findings = %d, want 1", len(d.ByAgent["scanner"]))
	}
	if d.ByAgent["scanner"][0].Type != "security" {
		t.Errorf("finding type = %q, want %q", d.ByAgent["scanner"][0].Type, "security")
	}
	if len(d.RecentlyResolved) != 1 {
		t.Errorf("recently resolved = %d, want 1", len(d.RecentlyResolved))
	}
}

func TestBuildDigestFromBeadsEmpty(t *testing.T) {
	stores := map[string]*beads.Store{}
	d := BuildDigestFromBeads(stores, "idle")
	if d.TotalCount != 0 {
		t.Errorf("TotalCount = %d, want 0", d.TotalCount)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
