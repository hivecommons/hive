package github

// Golden fixtures for IssueStageFor (ADR-0025, PR-A, hivecommons/hive#13006).
//
// These freeze TODAY's issue-side stage derivation byte-for-byte so the
// central stage engine can prove parity before it takes the derivation over
// (plan: src/docs/stage-engine-plan.md, PR-B/PR-F). The corpus is
// generated combinatorially — bucket × park/exempt label × claim kind ×
// filtered reason, plus the covered-by-PR cross of claim context × covering
// PR stage — and completed by hand-picked regressions from recent PRs.
//
// Regenerate with
//
//	go test ./pkg/github -run TestIssueStageGoldens -update
//
// and commit the result; a change in the goldens IS a semantic change and
// needs its own review. Without -update the test fails on any difference,
// and on a corpus that no longer matches the generator (a new dimension
// needs the fixtures regenerated).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

var updateIssueStageGoldens = flag.Bool("update", false, "rewrite the issue stage golden fixtures from the current derivation")

const issueStageGoldenDir = "testdata/issue_stage_goldens"

// issueStageGoldenInput is the serialised IssueStageInput plus the covering
// PR's stage as the shared store would answer it ("" = no resolver, "none"
// = resolver with no record).
type issueStageGoldenInput struct {
	Repo      string   `json:"repo"`
	Number    int      `json:"number"`
	Labels    []string `json:"labels,omitempty"`
	Bucket    string   `json:"bucket,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	ClaimKind string   `json:"claim_kind,omitempty"`
	ClaimedBy string   `json:"claimed_by,omitempty"`
	// PRStarted mirrors IssueStageInput.PRStarted: the live claim carries
	// PR evidence (hivecommons/hive#13069).
	PRStarted bool `json:"pr_started,omitempty"`
	// ClaimPR / ClaimPRRepo mirror IssueClaimContext.PRNumber / PRRepo; a
	// zero ClaimPR with ClaimPresent means a context without a PR.
	ClaimPresent bool   `json:"claim_present,omitempty"`
	ClaimPR      int    `json:"claim_pr,omitempty"`
	ClaimPRRepo  string `json:"claim_pr_repo,omitempty"`
	// CoveringPRStage is what prStage returns for any (repo, number);
	// CoveringPRLookup records the (repo, number) it was asked for.
	CoveringPRStage string `json:"covering_pr_stage,omitempty"`
	// AuthorKind mirrors IssueStageInput.AuthorKind ("" = Unknown, read
	// as human-filed) for the 015-triage-needed derivation.
	AuthorKind string `json:"author_kind,omitempty"`
}

type issueStageGoldenWant struct {
	Stage   string `json:"stage"`
	Covered bool   `json:"covered"`
	Reason  string `json:"reason"`
	// Lookup is the "repo#number" prStage was asked for, "" when it was not.
	Lookup string `json:"lookup,omitempty"`
}

type issueStageGoldenCase struct {
	Name  string                `json:"name"`
	Note  string                `json:"note,omitempty"`
	Input issueStageGoldenInput `json:"input"`
	Want  issueStageGoldenWant  `json:"want"`
}

func (in issueStageGoldenInput) toInput() IssueStageInput {
	out := IssueStageInput{Repo: in.Repo, Number: in.Number, Labels: in.Labels, Bucket: in.Bucket, Reason: in.Reason, ClaimKind: in.ClaimKind, ClaimedBy: in.ClaimedBy, PRStarted: in.PRStarted, AuthorKind: authorkind.Kind(in.AuthorKind)}
	if in.ClaimPresent {
		out.Claim = &IssueClaimContext{PRNumber: in.ClaimPR, PRRepo: in.ClaimPRRepo}
	}
	return out
}

// runIssueStageGolden evaluates one fixture input with the current code.
func runIssueStageGolden(in issueStageGoldenInput) issueStageGoldenWant {
	var want issueStageGoldenWant
	var prStage func(string, int) stagelabels.Stage
	if in.CoveringPRStage != "" {
		s := stageFromGoldenString(in.CoveringPRStage)
		prStage = func(repo string, n int) stagelabels.Stage {
			want.Lookup = repo + "#" + fmt.Sprint(n)
			return s
		}
	}
	stage, covered, reason := IssueStageFor(in.toInput(), prStage)
	want.Stage, want.Covered, want.Reason = stage.String(), covered, reason
	return want
}

func stageFromGoldenString(s string) stagelabels.Stage {
	for _, d := range stagelabels.Table {
		if d.Stage.String() == s {
			return d.Stage
		}
	}
	return stagelabels.None
}

// generateIssueStageGoldens builds the corpus. Every slice below is in a
// fixed order and the output is sorted by name, so the generator is
// deterministic.
func generateIssueStageGoldens() map[string][]issueStageGoldenCase {
	const repo = "hivecommons/hive"
	buckets := []string{"", workBucketClaimed, inProgressWorkBucket, workBucketClaimedByPR, workBucketFiltered, "draft"}
	labelSets := map[string][]string{
		"nolabel":          nil,
		"bug":              {"bug", "good first issue"},
		"likely-done":      {LikelyDoneLabel},
		"needs-human":      {issueNeedsHumanLabel},
		"stage-045":        {NeedsDecisionStageLabel},
		"stage-085":        {legacyIssueNeedsDecisionStageLabel},
		"needs-spec":       {issueNeedsSpecLabel},
		"needs-decision":   {issueNeedsDecisionLabel},
		"needs-dir":        {issueNeedsDirectionLabel},
		"park+stage030":    {issueNeedsDirectionLabel, stagelabels.PRInProgress.Label()},
		"human+done":       {issueNeedsHumanLabel, LikelyDoneLabel},
		"mixed-case":       {"Needs-Human "},
		"epic":             {"Epic", "help wanted"},
		"epic+stage030":    {"epic", stagelabels.PRInProgress.Label()},
		"epic+needs-human": {"Epic", issueNeedsHumanLabel},
		"blocked":          {BlockedLabel, "help wanted"},
		"blocked+stage030": {"Blocked", stagelabels.PRInProgress.Label()},
	}
	claimKinds := []string{"", string(claims.KindAgent), string(claims.KindHuman), string(claims.KindContributor), "AGENT"}
	reasons := []string{"", issueNeedsHumanLabel, issueNeedsSpecLabel, NeedsDecisionStageLabel, issueNeedsDirectionLabel, "wontfix", " needs-human "}

	out := map[string][]issueStageGoldenCase{}
	add := func(file string, c issueStageGoldenCase) {
		c.Want = runIssueStageGolden(c.Input)
		out[file] = append(out[file], c)
	}

	labelNames := make([]string, 0, len(labelSets))
	for k := range labelSets {
		labelNames = append(labelNames, k)
	}
	sort.Strings(labelNames)

	for _, bucket := range buckets {
		bname := bucket
		if bname == "" {
			bname = "actionable"
		}
		for _, ln := range labelNames {
			for _, ck := range claimKinds {
				for _, reason := range reasons {
					// Reasons only mean something on the filtered bucket;
					// elsewhere one representative keeps the corpus small.
					if bucket != workBucketFiltered && reason != "" && reason != "wontfix" {
						continue
					}
					ckName := ck
					if ckName == "" {
						ckName = "noclaim"
					}
					rName := strings.TrimSpace(reason)
					if rName == "" {
						rName = "noreason"
					} else if reason != rName {
						rName += "-padded"
					}
					add("buckets.json", issueStageGoldenCase{
						Name:  fmt.Sprintf("bucket=%s/labels=%s/claim=%s/reason=%s", bname, ln, ckName, rName),
						Input: issueStageGoldenInput{Repo: repo, Number: 1, Labels: labelSets[ln], Bucket: bucket, Reason: reason, ClaimKind: ck, ClaimedBy: "someone"},
					})
					// PR evidence on the claim only means something on the
					// claim-shaped buckets (hivecommons/hive#13069).
					if bucket == workBucketClaimed || bucket == inProgressWorkBucket {
						add("buckets.json", issueStageGoldenCase{
							Name:  fmt.Sprintf("bucket=%s/labels=%s/claim=%s/reason=%s/pr_started", bname, ln, ckName, rName),
							Input: issueStageGoldenInput{Repo: repo, Number: 1, Labels: labelSets[ln], Bucket: bucket, Reason: reason, ClaimKind: ck, ClaimedBy: "someone", PRStarted: true},
						})
					}
				}
			}
		}
	}

	// Covered-by-PR: claim context × covering PR stage × park label.
	type claimShape struct {
		name    string
		present bool
		pr      int
		prRepo  string
	}
	claimShapes := []claimShape{
		{name: "nil"}, {name: "nopr", present: true}, {name: "pr42", present: true, pr: 42}, {name: "pr42-other", present: true, pr: 42, prRepo: "hivecommons/other"},
	}
	prStages := []string{"", "none"}
	for _, d := range stagelabels.Table {
		prStages = append(prStages, d.Stage.String())
	}
	for _, cs := range claimShapes {
		for _, ps := range prStages {
			for _, ln := range []string{"nolabel", "needs-human", "likely-done"} {
				psName := ps
				if psName == "" {
					psName = "noresolver"
				}
				add("covered.json", issueStageGoldenCase{
					Name:  fmt.Sprintf("covered/claim=%s/prstage=%s/labels=%s", cs.name, psName, ln),
					Input: issueStageGoldenInput{Repo: repo, Number: 7, Labels: labelSets[ln], Bucket: workBucketClaimedByPR, ClaimPresent: cs.present, ClaimPR: cs.pr, ClaimPRRepo: cs.prRepo, CoveringPRStage: ps},
				})
			}
		}
	}

	// Hand-picked regressions.
	for _, c := range []issueStageGoldenCase{
		{
			Name:  "regression/12481-needs-direction-with-stale-030",
			Note:  "hivecommons/hive#12481 carried needs-direction and stage/030 for five hours; the park label must win (now 045, #12856).",
			Input: issueStageGoldenInput{Repo: repo, Number: 12481, Labels: []string{"needs-direction", stagelabels.PRInProgress.Label()}, Bucket: workBucketFiltered, Reason: "needs-direction"},
		},
		{
			Name:  "regression/12856-issue-wearing-085-restages-to-045",
			Note:  "hivecommons/hive#12856: 085 is PR-only; an issue found wearing it derives 045 (the label is the reason).",
			Input: issueStageGoldenInput{Repo: repo, Number: 12856, Labels: []string{legacyIssueNeedsDecisionStageLabel, "bug"}, Bucket: workBucketFiltered, Reason: legacyIssueNeedsDecisionStageLabel},
		},
		{
			Name:  "regression/12856-needs-human-derives-045-not-085",
			Note:  "hivecommons/hive#12856: needs-human parks the issue at 045 whatever the bucket says.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12857, Labels: []string{issueNeedsHumanLabel}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)},
		},
		{
			Name:  "regression/12451-covered-issue-never-mirrors-pr-stage",
			Note:  "hivecommons/hive#12451 wore stage/050 next to its own stage; the PR stage is narrated in the reason only.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12451, Bucket: workBucketClaimedByPR, ClaimPresent: true, ClaimPR: 12460, CoveringPRStage: stagelabels.ReviewPending.String()},
		},
		{
			Name:  "regression/covered-pr-before-040-is-not-covered",
			Note:  "A covering PR the store still has at an issue stage (<040) does not count as staged coverage.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12452, Bucket: workBucketClaimedByPR, ClaimPresent: true, ClaimPR: 12461, CoveringPRStage: stagelabels.Claimed.String()},
		},
		{
			Name:  "regression/covered-pr-in-other-repo-is-looked-up-there",
			Note:  "IssueClaimContext.PRRepo overrides the issue's repo for the store lookup.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12453, Bucket: workBucketClaimedByPR, ClaimPresent: true, ClaimPR: 5, ClaimPRRepo: "hivecommons/docs", CoveringPRStage: stagelabels.MergeQueue.String()},
		},
		{
			Name:  "regression/likely-done-beats-park",
			Note:  "hive/likely-done leaves the pipeline even when a park label is present.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12454, Labels: []string{LikelyDoneLabel, issueNeedsHumanLabel}, Bucket: workBucketFiltered, Reason: LikelyDoneLabel},
		},
		{
			Name:  "regression/filtered-renamed-park-label-via-reason",
			Note:  "An operator-renamed park label is only visible through the filtered bucket's reason.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12455, Labels: []string{"custom/parked"}, Bucket: workBucketFiltered, Reason: issueNeedsSpecLabel},
		},
		{
			Name:  "regression/agent-claim-kind-is-case-insensitive",
			Input: issueStageGoldenInput{Repo: repo, Number: 12456, Bucket: workBucketClaimed, ClaimKind: "Agent"},
		},
		{
			Name:  "regression/13069-bare-agent-claim-derives-020",
			Note:  "hivecommons/hive#13069 wore stage/030-pr-in-progress on the scanner's `hive-open-issue claim` with no PR, branch or PR request anywhere; an agent claim without PR evidence is 020-claimed.",
			Input: issueStageGoldenInput{Repo: repo, Number: 13069, Labels: []string{"kind/bug", stagelabels.PRInProgress.Label(), "hive/claimed-by-scanner"}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), ClaimedBy: "scanner"},
		},
		{
			Name:  "regression/13069-agent-start-mark-derives-020",
			Note:  "The in-progress sweep's live kind=agent marker is the same bare claim: 020 until pr_started= is on the marker.",
			Input: issueStageGoldenInput{Repo: repo, Number: 13069, Labels: []string{stagelabels.PRInProgress.Label(), "hive/claimed-by-scanner"}, Bucket: inProgressWorkBucket, ClaimKind: string(claims.KindAgent), ClaimedBy: "scanner"},
		},
		{
			Name:  "regression/13069-agent-claim-with-pr-evidence-derives-030",
			Note:  "A `hive-open-pr --issues` request (or a start marker naming a branch) on the claim is the PR evidence 030 promises.",
			Input: issueStageGoldenInput{Repo: repo, Number: 13069, Labels: []string{stagelabels.Claimed.Label(), "hive/claimed-by-scanner"}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), ClaimedBy: "scanner", PRStarted: true},
		},
		{
			Name:  "regression/12999-epic-with-agent-claim-stays-010",
			Note:  "hivecommons/hive#12999 (label Epic) wore stage/030-pr-in-progress on a scanner claim; an umbrella issue never derives 020/030 from a claim.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12999, Labels: []string{"Epic", "help wanted", stagelabels.PRInProgress.Label()}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), ClaimedBy: "scanner"},
		},
		{
			Name:  "regression/12999-epic-with-human-claim-stays-010",
			Note:  "A human claim on an epic is not staged 020 either.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12999, Labels: []string{"epic"}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindHuman), ClaimedBy: "alice"},
		},
		{
			Name:  "regression/12999-epic-in-progress-mark-stays-010",
			Note:  "An in-progress start mark on an epic is not staged 030.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12999, Labels: []string{"Epic"}, Bucket: inProgressWorkBucket, ClaimKind: string(claims.KindAgent), ClaimedBy: "scanner"},
		},
		{
			Name:  "regression/epic-parked-still-045",
			Note:  "The park family outranks the umbrella rule: a parked epic derives 045.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12998, Labels: []string{"Epic", NeedsDecisionStageLabel}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)},
		},
		{
			Name:  "regression/epic-covered-by-pr-still-040",
			Note:  "A PR that names the epic still covers it; only claim/in-progress buckets are suppressed.",
			Input: issueStageGoldenInput{Repo: repo, Number: 12997, Labels: []string{"Epic"}, Bucket: workBucketClaimedByPR, ClaimPresent: true, ClaimPR: 13054, CoveringPRStage: stagelabels.ReviewPending.String()},
		},
	} {
		add("regressions.json", c)
	}

	// 015-triage-needed: every author kind × triage-evidence label set ×
	// the buckets that reach the actionable rule (hivecommons/hive#13268).
	triageLabels := map[string][]string{
		"nolabel":               nil,
		"needs-triage":          {triageAwaitingLabel},
		"triage-accepted":       {config.DefaultUntrustedRequireLabels[0]},
		"verified-open":         {VerifiedOpenLabel},
		"approved-direction":    {HumanAckLabel},
		"needs-triage+accepted": {triageAwaitingLabel, config.DefaultUntrustedRequireLabels[0]},
		"epic":                  {"Epic"},
		"epic+verified-open":    {"Epic", VerifiedOpenLabel},
	}
	triageLabelNames := make([]string, 0, len(triageLabels))
	for k := range triageLabels {
		triageLabelNames = append(triageLabelNames, k)
	}
	sort.Strings(triageLabelNames)
	for _, kind := range []authorkind.Kind{authorkind.Unknown, authorkind.Human, authorkind.Hive, authorkind.Clanker} {
		kname := string(kind)
		if kname == "" {
			kname = "unknown"
		}
		for _, bucket := range []string{"", workBucketReporterTriage, workBucketClaimed} {
			bname := bucket
			if bname == "" {
				bname = "actionable"
			}
			ck := ""
			if bucket == workBucketClaimed {
				ck = string(claims.KindAgent)
			}
			for _, ln := range triageLabelNames {
				add("triage.json", issueStageGoldenCase{
					Name:  "triage/" + kname + "/" + bname + "/" + ln,
					Input: issueStageGoldenInput{Repo: repo, Number: 1, Labels: triageLabels[ln], Bucket: bucket, ClaimKind: ck, AuthorKind: string(kind)},
				})
			}
		}
	}
	add("triage.json", issueStageGoldenCase{
		Name:  "triage/13262-hive-filed-review-backlog-stays-010",
		Note:  "hivecommons/hive#13262 (a hive-filed 'Review backlog (security)' issue): hive agents do not need triage, so who filed it never derives 015; it is 010.",
		Input: issueStageGoldenInput{Repo: repo, Number: 13262, Labels: []string{"kind/review-backlog", stagelabels.Actionable.Label()}, AuthorKind: string(authorkind.Hive)},
	})
	add("triage.json", issueStageGoldenCase{
		Name:  "triage/12999-human-epic-with-verified-open-stays-010",
		Note:  "hivecommons/hive#12999 was filed by a maintainer and carries hive/verified-open: triaged, 010.",
		Input: issueStageGoldenInput{Repo: repo, Number: 12999, Labels: []string{"Epic", VerifiedOpenLabel}, AuthorKind: string(authorkind.Human)},
	})

	for file := range out {
		sort.Slice(out[file], func(i, j int) bool { return out[file][i].Name < out[file][j].Name })
	}
	return out
}

// marshalGoldenCases renders one case per line so diffs stay readable and
// the encoding is byte-stable.
func marshalGoldenCases[T any](cases []T) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, c := range cases {
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		if i < len(cases)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("]\n")
	return buf.Bytes(), nil
}

func TestIssueStageGoldens(t *testing.T) {
	generated := generateIssueStageGoldens()
	if *updateIssueStageGoldens {
		if err := os.RemoveAll(issueStageGoldenDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(issueStageGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		total := 0
		for file, cases := range generated {
			b, err := marshalGoldenCases(cases)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(issueStageGoldenDir, file), b, 0o644); err != nil {
				t.Fatal(err)
			}
			total += len(cases)
		}
		t.Logf("wrote %d issue stage goldens across %d files", total, len(generated))
		return
	}

	files, err := filepath.Glob(filepath.Join(issueStageGoldenDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures under %s; run with -update", issueStageGoldenDir)
	}
	seen := map[string]bool{}
	total := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cases []issueStageGoldenCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		file := filepath.Base(path)
		gen := map[string]issueStageGoldenCase{}
		for _, c := range generated[file] {
			gen[c.Name] = c
		}
		if len(gen) != len(cases) {
			t.Errorf("%s: fixture holds %d cases, generator produces %d; run with -update", file, len(cases), len(gen))
		}
		for _, c := range cases {
			total++
			if seen[c.Name] {
				t.Errorf("%s: duplicate case %q", file, c.Name)
			}
			seen[c.Name] = true
			g, ok := gen[c.Name]
			if !ok {
				t.Errorf("%s: case %q is not produced by the generator; run with -update", file, c.Name)
				continue
			}
			if gb, cb := mustJSON(t, g.Input), mustJSON(t, c.Input); gb != cb {
				t.Errorf("%s: case %q input drifted from the generator:\n fixture   %s\n generator %s", file, c.Name, cb, gb)
			}
			got := runIssueStageGolden(c.Input)
			if got != c.Want {
				t.Errorf("%s: case %q\n got  %+v\n want %+v", file, c.Name, got, c.Want)
			}
		}
		// The file must be byte-identical to what -update would write.
		if want, err := marshalGoldenCases(cases); err != nil || !bytes.Equal(want, raw) {
			t.Errorf("%s: not in canonical form; run with -update", file)
		}
	}
	t.Logf("replayed %d issue stage goldens", total)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestIssueStageGoldensCoverEveryIssueStage guards the corpus itself: every
// issue-side stage and the None outcome appear at least once, so a stage
// nobody can reach would be noticed here rather than in production.
func TestIssueStageGoldensCoverEveryIssueStage(t *testing.T) {
	reached := map[string]bool{}
	for _, cases := range generateIssueStageGoldens() {
		for _, c := range cases {
			reached[c.Want.Stage] = true
		}
	}
	want := []string{stagelabels.None.String()}
	for _, d := range stagelabels.Table {
		if d.Issue {
			want = append(want, d.Stage.String())
		}
	}
	for _, s := range want {
		if !reached[s] {
			t.Errorf("no golden reaches %s", s)
		}
	}
	for _, d := range stagelabels.Table {
		if !d.Issue && reached[d.Stage.String()] {
			t.Errorf("a golden derived PR-only stage %s for an issue", d.Stage)
		}
	}
}
