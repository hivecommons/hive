package commands

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/spf13/cobra"
)

// `hivectl evidence export` (hivecommons/hive#12367, RFC #12062 proposal B):
// the raw per-PR evidence off a spoke as CSV or JSON Lines, one row per PR
// (every bundle of a PR folded by evidence.FoldPRs and windowed on PR open
// time, the same rule GET /api/evidence/summary applies), with the column set
// the dashboard Evidence charts aggregate so the numbers reconcile. Local
// filesystem only: no network, no hive session, and nothing here reads into
// or alters merge eligibility.

const (
	evidenceExportFormatJSONL = "jsonl"
	evidenceExportFormatCSV   = "csv"
	// evidenceExportListSep joins list-valued columns in CSV output.
	evidenceExportListSep = ";"
	// evidenceExportFieldSep joins the fields of one verdict in CSV output.
	evidenceExportFieldSep = ":"
	evidenceOutcomeMerged  = "merged"
	evidenceOutcomeClosed  = "closed"
	evidenceOutcomeOpen    = "open"
)

// evidenceExportNow is injectable so tests can pin a --since duration.
var evidenceExportNow = time.Now

// evidenceExportVerdict is one reviewer verdict as the charts see it.
type evidenceExportVerdict struct {
	Perspective string  `json:"perspective"`
	Verdict     string  `json:"verdict"`
	Model       string  `json:"model"`
	Backend     string  `json:"backend"`
	Confidence  float64 `json:"confidence"`
}

// evidenceExportRow is one PR's folded bundles (evidence.PR) flattened to the
// chart schema.
// Field order is the CSV column order.
type evidenceExportRow struct {
	Repo           string                  `json:"repo"`
	PR             int                     `json:"pr"`
	HeadSHA        string                  `json:"head_sha"`
	BundleID       string                  `json:"bundle_id"`
	AuthorLogin    string                  `json:"author_login"`
	AuthorKind     string                  `json:"author_kind"`
	AuthorModel    string                  `json:"author_model,omitempty"`
	ACMMLevel      int                     `json:"acmm_level"`
	Perspectives   []string                `json:"perspectives"`
	ReviewerModels []string                `json:"reviewer_models"`
	Verdicts       []evidenceExportVerdict `json:"verdicts"`
	CIConclusion   string                  `json:"ci_conclusion,omitempty"`
	CICapturedAt   *time.Time              `json:"ci_captured_at,omitempty"`
	Outcome        string                  `json:"outcome"`
	MergedAt       *time.Time              `json:"merged_at,omitempty"`
	RevertOf       []string                `json:"revert_of"`
	RevertedBy     []string                `json:"reverted_by"`
	FollowupOf     []string                `json:"followup_of"`
	FollowedUpBy   []string                `json:"followed_up_by"`
	// Shadow flags are nil when the gate was not evaluated for this head.
	ShadowReviewWouldHaveHeld    *bool     `json:"shadow_review_would_have_held"`
	ShadowIntentWouldHaveBlocked *bool     `json:"shadow_intent_would_have_blocked"`
	SentinelHits                 int       `json:"sentinel_hits"`
	Signed                       bool      `json:"signed"`
	CreatedAt                    time.Time `json:"created_at"`
	UpdatedAt                    time.Time `json:"updated_at"`
	// OpenedAt is the PR open time --since windows on.
	OpenedAt time.Time `json:"opened_at"`
}

// evidenceExportColumns is the CSV header; it must match evidenceExportRow.csv.
var evidenceExportColumns = []string{
	"repo", "pr", "head_sha", "bundle_id",
	"author_login", "author_kind", "author_model", "acmm_level",
	"perspectives", "reviewer_models", "verdicts",
	"ci_conclusion", "ci_captured_at",
	"outcome", "merged_at",
	"revert_of", "reverted_by", "followup_of", "followed_up_by",
	"shadow_review_would_have_held", "shadow_intent_would_have_blocked",
	"sentinel_hits", "signed", "created_at", "updated_at", "opened_at",
}

func newEvidenceCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "evidence",
		Short:   "Per-PR review evidence: export stored bundles or backfill them from forge history",
		Long:    "Work with the review evidence bundles Hive stores under the evidence root (default /data/evidence).\nexport reads the local filesystem only; backfill talks to the GitHub API. Neither contacts a hive and nothing here\ninfluences a merge.",
		Example: "  hivectl evidence export --since 168h --format csv --out evidence.csv\n  hivectl evidence export --since 2026-10-01T00:00:00Z --repo hivecommons/hive\n  hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --dry-run",
	}
	cmd.AddCommand(newEvidenceExportCommand(env))
	cmd.AddCommand(newEvidenceBackfillCommand(env))
	cmd.AddCommand(newEvidenceBackfillIssuesCommand(env))
	return cmd
}

func newEvidenceExportCommand(_ *commandEnv) *cobra.Command {
	var root, since, format, repo, out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export one folded evidence row per PR as CSV or JSON Lines",
		Long: "Export one row per pull request from the schema v1 evidence bundles stored under --root\n" +
			"(<root>/<org>/<repo>/<pr>/<sha>.json), folding every bundle of a PR exactly as GET /api/evidence/summary\n" +
			"does, so exported counts reconcile with the dashboard Evidence charts: verdicts, CI, sentinel, shadow flags and\n" +
			"the identity columns come from the newest bundle; outcome, merged_at, the link lists and opened_at come from\n" +
			"whichever bundle of the PR recorded them (a live bundle and a backfilled one are one row). Columns: repo, pr,\n" +
			"head_sha, bundle_id, author login/kind/model, policy.acmm_level, perspectives, reviewer models, per-perspective\n" +
			"verdicts (perspective, verdict, model, backend, confidence), ci conclusion and captured_at, outcome\n" +
			"(merged/closed/open) and merged_at, revert_of / reverted_by / followup_of / followed_up_by, shadow review-gate\n" +
			"would_have_held, shadow intent would_have_blocked, sentinel hit count, signed, created_at, updated_at, opened_at.\n\n" +
			"--since takes an RFC3339 timestamp or a Go duration (e.g. 72h) and keeps PRs opened at or after it.\n" +
			"Rows are ordered by repo then PR number. Bundles that do not parse are skipped and counted on stderr, never\n" +
			"fabricated. outcome is closed only when a backfilled bundle records a close without merge.\n" +
			"Reads the local filesystem only; no network, no hive session, and nothing here affects merge eligibility.",
		Example: "  hivectl evidence export                                  # all PRs, JSON Lines on stdout\n" +
			"  hivectl evidence export --since 168h --format csv --out evidence.csv\n" +
			"  hivectl evidence export --since 2026-10-01T00:00:00Z --repo hivecommons/hive\n" +
			"  hivectl evidence export --root ./evidence --format csv | column -ts,",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			format = strings.ToLower(strings.TrimSpace(format))
			if format != evidenceExportFormatJSONL && format != evidenceExportFormatCSV {
				return &usageError{message: fmt.Sprintf("invalid --format %q; expected csv or jsonl", format)}
			}
			cutoff, err := parseEvidenceSince(since, evidenceExportNow())
			if err != nil {
				return &usageError{message: err.Error()}
			}
			repo = strings.TrimSpace(repo)
			if repo != "" && strings.Count(repo, "/") != 1 {
				return &usageError{message: fmt.Sprintf("invalid --repo %q; expected owner/name", repo)}
			}

			rows, unreadable, err := exportEvidenceRows(root, cutoff, repo)
			if err != nil {
				return err
			}
			if unreadable > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d unreadable or non-v1 bundle(s) under %s\n", unreadable, root)
			}

			w := cmd.OutOrStdout()
			var f *os.File
			if out != "" && out != "-" {
				f, err = os.Create(out)
				if err != nil {
					return fmt.Errorf("writing %s: %w", out, err)
				}
				w = f
			}
			if format == evidenceExportFormatCSV {
				err = writeEvidenceCSV(w, rows)
			} else {
				err = writeEvidenceJSONL(w, rows)
			}
			if f != nil {
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d row(s) to %s\n", len(rows), out)
				}
			}
			if err != nil {
				return fmt.Errorf("writing export: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", ghpkg.ReviewEvidenceRoot, "evidence root holding <org>/<repo>/<pr>/<sha>.json bundles")
	cmd.Flags().StringVar(&since, "since", "", "only bundles created at or after this RFC3339 time or within this Go duration (e.g. 72h)")
	cmd.Flags().StringVar(&format, "format", evidenceExportFormatJSONL, "output format: csv or jsonl")
	cmd.Flags().StringVar(&repo, "repo", "", "only this owner/name repository (case-insensitive)")
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout (\"-\" is stdout)")
	return cmd
}

// parseEvidenceSince accepts an RFC3339 timestamp or a Go duration back from
// now. Empty means no cutoff.
func parseEvidenceSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid --since %q; expected an RFC3339 time or a Go duration such as 72h", s)
	}
	return now.Add(-d), nil
}

// exportEvidenceRows walks root, folds the readable v1 bundles in repo
// (empty = all) per PR, keeps the PRs opened at or after cutoff (zero = all
// time), and returns rows ordered by repo then PR number plus the unreadable
// count.
func exportEvidenceRows(root string, cutoff time.Time, repo string) ([]evidenceExportRow, int, error) {
	var bundles []*evidence.Bundle
	unreadable := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		b, lerr := ghpkg.LoadReviewEvidence(path)
		if lerr != nil || b.SchemaVersion != evidence.SchemaVersion || b.Repo == "" || b.Number <= 0 {
			unreadable++
			return nil
		}
		bundles = append(bundles, b)
		return nil
	})
	if err != nil {
		return nil, unreadable, fmt.Errorf("reading evidence under %s: %w", root, err)
	}
	prs := evidence.FoldPRs(bundles, repo, "")
	rows := make([]evidenceExportRow, 0, len(prs))
	for _, p := range prs {
		if !cutoff.IsZero() && p.OpenedAt.Before(cutoff) {
			continue
		}
		rows = append(rows, evidenceRowFromPR(p))
	}
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := strings.ToLower(rows[i].Repo), strings.ToLower(rows[j].Repo)
		if ri != rj {
			return ri < rj
		}
		return rows[i].PR < rows[j].PR
	})
	return rows, unreadable, nil
}

// evidenceRowFromPR flattens one folded PR: the newest bundle's verdicts,
// CI, shadow flags and identity, with the outcome, links and open time from
// whichever bundle of the PR recorded them.
func evidenceRowFromPR(p *evidence.PR) evidenceExportRow {
	b := p.Bundle
	row := evidenceExportRow{
		Repo: b.Repo, PR: b.Number, HeadSHA: b.HeadSHA, BundleID: b.ID,
		AuthorLogin: b.Author.Login, AuthorKind: b.Author.Kind, AuthorModel: b.Author.Model,
		ACMMLevel:    b.Policy.ACMMLevel,
		Perspectives: nonNil(b.Policy.Perspectives),
		Verdicts:     make([]evidenceExportVerdict, 0, len(b.Verdicts)),
		CIConclusion: b.CI.Status,
		Outcome:      evidenceOutcomeOpen,
		RevertOf:     nonNil(p.RevertOf), RevertedBy: nonNil(p.RevertedBy),
		FollowupOf: nonNil(p.FollowupOf), FollowedUpBy: nonNil(p.FollowedUpBy),
		SentinelHits: len(b.Sentinel),
		Signed:       b.Signed,
		CreatedAt:    b.CreatedAt, UpdatedAt: b.UpdatedAt, OpenedAt: p.OpenedAt,
	}
	if !b.CI.CapturedAt.IsZero() {
		t := b.CI.CapturedAt
		row.CICapturedAt = &t
	}
	switch {
	case p.Merge != nil:
		row.Outcome = evidenceOutcomeMerged
		t := p.Merge.At
		row.MergedAt = &t
	case p.Closed:
		row.Outcome = evidenceOutcomeClosed
	}
	models := map[string]bool{}
	for _, v := range b.Verdicts {
		row.Verdicts = append(row.Verdicts, evidenceExportVerdict{Perspective: v.Perspective, Verdict: v.Verdict, Model: v.Model, Backend: v.Backend, Confidence: v.Confidence})
		if v.Model != "" {
			models[v.Model] = true
		}
	}
	sort.SliceStable(row.Verdicts, func(i, j int) bool { return row.Verdicts[i].Perspective < row.Verdicts[j].Perspective })
	row.ReviewerModels = make([]string, 0, len(models))
	for m := range models {
		row.ReviewerModels = append(row.ReviewerModels, m)
	}
	sort.Strings(row.ReviewerModels)
	if sh := b.Shadow; sh != nil {
		if sh.ReviewGate != nil {
			row.ShadowReviewWouldHaveHeld = sh.ReviewGate.WouldHaveHeld
		}
		if sh.Intent != nil {
			row.ShadowIntentWouldHaveBlocked = sh.Intent.WouldHaveBlocked
		}
	}
	return row
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeEvidenceJSONL(w io.Writer, rows []evidenceExportRow) error {
	enc := json.NewEncoder(w)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeEvidenceCSV(w io.Writer, rows []evidenceExportRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(evidenceExportColumns); err != nil {
		return err
	}
	for i := range rows {
		if err := cw.Write(rows[i].csv()); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csv renders the row in evidenceExportColumns order. Lists join with ";" and
// each verdict is perspective:verdict:model:backend:confidence; nil shadow
// flags and absent timestamps are empty cells.
func (r evidenceExportRow) csv() []string {
	optTime := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	optBool := func(b *bool) string {
		if b == nil {
			return ""
		}
		return strconv.FormatBool(*b)
	}
	verdicts := make([]string, 0, len(r.Verdicts))
	for _, v := range r.Verdicts {
		verdicts = append(verdicts, strings.Join([]string{v.Perspective, v.Verdict, v.Model, v.Backend, strconv.FormatFloat(v.Confidence, 'f', -1, 64)}, evidenceExportFieldSep))
	}
	return []string{
		r.Repo, strconv.Itoa(r.PR), r.HeadSHA, r.BundleID,
		r.AuthorLogin, r.AuthorKind, r.AuthorModel, strconv.Itoa(r.ACMMLevel),
		strings.Join(r.Perspectives, evidenceExportListSep), strings.Join(r.ReviewerModels, evidenceExportListSep), strings.Join(verdicts, evidenceExportListSep),
		r.CIConclusion, optTime(r.CICapturedAt),
		r.Outcome, optTime(r.MergedAt),
		strings.Join(r.RevertOf, evidenceExportListSep), strings.Join(r.RevertedBy, evidenceExportListSep), strings.Join(r.FollowupOf, evidenceExportListSep), strings.Join(r.FollowedUpBy, evidenceExportListSep),
		optBool(r.ShadowReviewWouldHaveHeld), optBool(r.ShadowIntentWouldHaveBlocked),
		strconv.Itoa(r.SentinelHits), strconv.FormatBool(r.Signed), r.CreatedAt.UTC().Format(time.RFC3339), r.UpdatedAt.UTC().Format(time.RFC3339),
		r.OpenedAt.UTC().Format(time.RFC3339),
	}
}
