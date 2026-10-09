package compliance

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// PostureStatus is the outcome of one posture check run.
type PostureStatus string

const (
	// PosturePass: the hive is in the checked posture.
	PosturePass PostureStatus = "pass"
	// PostureFail: the check found evidence the hive is not.
	PostureFail PostureStatus = "fail"
	// PostureSkip: the check could not apply (no GitHub client, nothing
	// configured to check, no selected profile maps it). Never a pass.
	PostureSkip PostureStatus = "skip"
	// PostureError: the evidence could not be gathered (GitHub API error).
	PostureError PostureStatus = "error"
)

// Result is one posture check's outcome at one point in time
// (hivecommons/hive#11079). Pass is true only for PosturePass.
type Result struct {
	CheckID      string        `json:"check_id"`
	Title        string        `json:"title"`
	ControlIDs   []string      `json:"control_ids"`
	At           time.Time     `json:"at"`
	Pass         bool          `json:"pass"`
	Status       PostureStatus `json:"status"`
	Detail       string        `json:"detail"`
	EvidenceRefs []string      `json:"evidence_refs,omitempty"`
}

// PostureCheck is one continuous check the hive runs against itself. Run
// fills Status, Detail and EvidenceRefs; the runner stamps the identity
// fields, At and Pass.
type PostureCheck struct {
	ID         string
	Title      string
	ControlIDs []string
	Run        func(ctx context.Context, deps PostureDeps) Result
}

// PostureMergedPR is one merged pull request as the history-based checks see
// it. Reviewers are the logins with a submitted (not pending, not dismissed)
// review.
type PostureMergedPR struct {
	Repo        string
	Number      int
	URL         string
	Author      string
	MergedBy    string
	MergedByBot bool
	MergedAt    time.Time
	Reviewers   []string
}

// PostureGitHub is the read-only GitHub evidence the history-based checks
// need. pkg/dashboard adapts the hive's GitHub client to it.
type PostureGitHub interface {
	MergedPRsSince(ctx context.Context, repo string, since time.Time) ([]PostureMergedPR, error)
	RepoLabels(ctx context.Context, repo string) ([]string, error)
}

// PostureDeps is everything a check may read. Zero-valued fields fall back
// to safe defaults (see withDefaults); a nil GitHub makes the GitHub-backed
// checks skip.
type PostureDeps struct {
	Config *config.Config
	Getenv func(string) string
	Now    func() time.Time
	GitHub PostureGitHub
	// Repos are the monitored repositories, "owner/name" or bare names
	// qualified with Config.Project.Org.
	Repos []string
	// HoldLabel is the hive's hold label (github.CanonicalHiveHoldLabel).
	HoldLabel string
	// ConfigFiles are the hive.yaml and overlay paths scanned for literal
	// credentials. Missing files are ignored.
	ConfigFiles []string
	ReadFile    func(string) ([]byte, error)
	// Window is the look-back of the merged-PR checks.
	Window time.Duration
}

func (d PostureDeps) withDefaults() PostureDeps {
	if d.Config == nil {
		d.Config = &config.Config{}
	}
	if d.Getenv == nil {
		d.Getenv = func(string) string { return "" }
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.Window <= 0 {
		d.Window = d.Config.Compliance.PostureWindowOrDefault()
	}
	if strings.TrimSpace(d.HoldLabel) == "" {
		d.HoldLabel = "hold"
	}
	return d
}

// qualifiedRepos returns Repos as "owner/name", deduplicated, in order.
func (d PostureDeps) qualifiedRepos() []string {
	org := strings.TrimSpace(d.Config.Project.Org)
	seen := map[string]bool{}
	var out []string
	for _, r := range d.Repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.Contains(r, "/") && org != "" {
			r = org + "/" + r
		}
		if seen[strings.ToLower(r)] {
			continue
		}
		seen[strings.ToLower(r)] = true
		out = append(out, r)
	}
	return out
}

// Bounds on what one result carries, so the history stays small.
const (
	maxDetailLen    = 600
	maxEvidenceRefs = 20
)

// PostureChecks returns the registered checks in a stable order.
func PostureChecks() []PostureCheck {
	return append([]PostureCheck(nil), postureChecks...)
}

// RunPostureCheck runs one check, stamping its identity and time, deriving
// Pass from Status, bounding Detail and EvidenceRefs and turning a panic into
// an error result so one broken check cannot stop the pass.
func RunPostureCheck(ctx context.Context, c PostureCheck, deps PostureDeps) (res Result) {
	deps = deps.withDefaults()
	at := deps.Now().UTC()
	defer func() {
		if p := recover(); p != nil {
			res = Result{Status: PostureError, Detail: fmt.Sprintf("check panicked: %v", p)}
		}
		res.CheckID, res.Title, res.ControlIDs, res.At = c.ID, c.Title, append([]string(nil), c.ControlIDs...), at
		switch res.Status {
		case PosturePass, PostureFail, PostureSkip, PostureError:
		default:
			res.Status = PostureError
			if res.Detail == "" {
				res.Detail = "check returned no status"
			}
		}
		res.Pass = res.Status == PosturePass
		if len(res.Detail) > maxDetailLen {
			res.Detail = res.Detail[:maxDetailLen-3] + "..."
		}
		if len(res.EvidenceRefs) > maxEvidenceRefs {
			res.EvidenceRefs = res.EvidenceRefs[:maxEvidenceRefs]
		}
	}()
	if c.Run == nil {
		return Result{Status: PostureError, Detail: "check has no Run function"}
	}
	if err := ctx.Err(); err != nil {
		return Result{Status: PostureError, Detail: "cancelled: " + err.Error()}
	}
	return c.Run(ctx, deps)
}

// RunPostureChecks runs every check in order.
func RunPostureChecks(ctx context.Context, checks []PostureCheck, deps PostureDeps) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		out = append(out, RunPostureCheck(ctx, c, deps))
	}
	return out
}

// PostureSummary counts results by status.
type PostureSummary struct {
	Pass  int `json:"pass"`
	Fail  int `json:"fail"`
	Skip  int `json:"skip"`
	Error int `json:"error"`
}

// SummarizePosture counts results by status.
func SummarizePosture(results []Result) PostureSummary {
	var s PostureSummary
	for _, r := range results {
		switch r.Status {
		case PosturePass:
			s.Pass++
		case PostureFail:
			s.Fail++
		case PostureSkip:
			s.Skip++
		default:
			s.Error++
		}
	}
	return s
}

// PostureCheckInfo describes one registered check for the API and UI.
type PostureCheckInfo struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	ControlIDs []string `json:"control_ids"`
}

// PostureCatalogue lists the registered checks.
func PostureCatalogue() []PostureCheckInfo {
	out := make([]PostureCheckInfo, 0, len(postureChecks))
	for _, c := range postureChecks {
		out = append(out, PostureCheckInfo{ID: c.ID, Title: c.Title, ControlIDs: append([]string(nil), c.ControlIDs...)})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
