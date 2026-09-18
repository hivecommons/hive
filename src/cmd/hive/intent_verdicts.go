package main

// Intent verdicts: judging whether an agent's pull request actually matches the
// issue it claims to resolve -- gathering PR and issue evidence, summarising the
// alignment, recording the advisory, and writing the intent-verdicts report.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

func writeIntentVerdicts(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	logger *slog.Logger,
) map[string]intent.Verdict {
	verdicts := make(map[string]intent.Verdict)
	if cfg == nil || actionable == nil {
		return verdicts
	}
	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)
	aiAuthor := strings.TrimSpace(cfg.EffectiveAIAuthor())
	intentCfg := intent.Config{
		TestPathPatterns:      cfg.Intent.TestPathPatterns,
		DocsPathPatterns:      cfg.Intent.DocsPathPatterns,
		GuardrailPathPatterns: cfg.Intent.GuardrailPathPatterns,
		FeatureSignals:        cfg.Intent.FeatureSignals,
	}
	var alignmentReviewer *intent.AlignmentReviewer
	if strings.TrimSpace(cfg.Intent.AlignmentModel) != "" {
		endpoint, apiKey, _ := cfg.Governor.ResolveReviewer()
		var err error
		alignmentReviewer, err = intent.NewAlignmentReviewer(intent.AlignmentReviewerConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    cfg.Intent.AlignmentModel,
		})
		if err != nil {
			logger.Warn("intent alignment reviewer disabled", "error", err)
		}
	}
	type verdictRecord struct {
		Repo       string         `json:"repo"`
		Number     int            `json:"number"`
		Title      string         `json:"title"`
		Author     string         `json:"author"`
		Enforced   bool           `json:"enforced"`
		Verdict    intent.Verdict `json:"verdict"`
		Classify   string         `json:"classification_reason"`
		FetchError string         `json:"fetch_error,omitempty"`
	}
	records := make([]verdictRecord, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		key := fmt.Sprintf("%s/%d", fullRepo, pr.Number)
		agentPR := aiAuthor != "" && strings.EqualFold(pr.Author, aiAuthor)
		record := verdictRecord{
			Repo:     fullRepo,
			Number:   pr.Number,
			Title:    pr.Title,
			Author:   pr.Author,
			Enforced: cfg.Intent.Enforce,
		}
		if !agentPR {
			class := intent.Classify(intent.PR{Title: pr.Title, Labels: pr.Labels, Author: pr.Author, AgentAuthor: false}, intentCfg)
			verdict := intent.Evaluate(class, intent.Evidence{})
			verdicts[key] = verdict
			record.Verdict = verdict
			record.Classify = class.Reason
			records = append(records, record)
			continue
		}
		body, files, approved, err := fetchIntentPREvidence(ctx, ghClient, fullRepo, pr.Number)
		if err != nil {
			verdict := intent.Verdict{
				Tier:       intent.Tier1,
				Authorized: false,
				Reason:     "intent evidence unavailable: " + err.Error(),
				AgentPR:    true,
			}
			verdicts[key] = verdict
			record.Verdict = verdict
			record.FetchError = err.Error()
			records = append(records, record)
			logger.Warn("intent verification evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", err)
			continue
		}
		class := intent.Classify(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, intentCfg)
		evidence := intent.BuildEvidenceForRepo(body, fullRepo, beadStores, approved)
		verdict := intent.Evaluate(class, evidence)
		issueTexts, issueErr := fetchIntentIssueTexts(ctx, ghClient, fullRepo, body)
		if issueErr != nil {
			logger.Warn("intent alignment issue evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", issueErr)
		}
		refs := intent.LinkedIssueRefs(body, fullRepo)
		alignCtx := intent.BuildAlignmentContext(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, issueTexts, beadStores, refs)
		alignment := intent.EvaluateAlignment(alignCtx, class.Tier, intentCfg)
		if alignmentReviewer != nil {
			modelVerdict, err := alignmentReviewer.Review(ctx, alignCtx)
			if err != nil {
				logger.Warn("intent alignment model review failed open", "repo", fullRepo, "number", pr.Number, "error", err)
				alignment = intent.MergeAlignment(alignment, nil, err)
			} else {
				alignment = intent.MergeAlignment(alignment, &modelVerdict, nil)
			}
		}
		verdict.Alignment = &alignment
		verdicts[key] = verdict
		record.Verdict = verdict
		record.Classify = class.Reason
		records = append(records, record)
		if !verdict.Authorized {
			logger.Info("intent authorization denied", "repo", fullRepo, "number", pr.Number, "tier", verdict.Tier, "reason", verdict.Reason, "enforce", cfg.Intent.Enforce)
		}
		if alignment.Misaligned() {
			logger.Info("intent alignment denied", "repo", fullRepo, "number", pr.Number, "reason", alignment.Rationale, "enforce", cfg.Intent.Enforce)
			recordIntentAlignmentAdvisory(beadStores, fullRepo, pr.Number, alignment, logger)
		}
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"enforced":     cfg.Intent.Enforce,
		"verdicts":     records,
	}
	if data, err := json.Marshal(payload); err == nil {
		atomicWrite(intentVerdictsPath, data)
	} else {
		logger.Warn("failed to marshal intent verdicts", "error", err)
	}
	return verdicts
}

func fetchIntentPREvidence(ctx context.Context, ghClient *github.Client, repo string, number int) (string, []intent.ChangedFile, bool, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return "", nil, false, github.ErrNoGitHubClient
	}
	owner, repoName, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || repoName == "" {
		return "", nil, false, fmt.Errorf("invalid repo %q", repo)
	}
	client := ghClient.GoGitHub()
	pr, _, err := client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return "", nil, false, fmt.Errorf("getting PR: %w", err)
	}
	var files []intent.ChangedFile
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repoName, number, fileOpts)
		if err != nil {
			return "", nil, false, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, intent.ChangedFile{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return "", nil, false, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d; intent alignment requires the complete changed-file list", reported, len(files))
	}
	approved, err := hasMaintainerApproval(ctx, client, owner, repoName, number)
	if err != nil {
		return "", nil, false, err
	}
	return pr.GetBody(), files, approved, nil
}

func fetchIntentIssueTexts(ctx context.Context, ghClient *github.Client, defaultRepo, body string) ([]intent.TextEvidence, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return nil, github.ErrNoGitHubClient
	}
	client := ghClient.GoGitHub()
	refs := intent.LinkedIssueRefs(body, defaultRepo)
	out := make([]intent.TextEvidence, 0, len(refs))
	for _, ref := range refs {
		repo := ref.Repo
		if repo == "" {
			repo = defaultRepo
		}
		owner, repoName, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || repoName == "" {
			continue
		}
		issue, _, err := client.Issues.Get(ctx, owner, repoName, ref.Number)
		if err != nil {
			return out, fmt.Errorf("getting linked issue %s#%d: %w", repo, ref.Number, err)
		}
		out = append(out, intent.TextEvidence{
			Source: fmt.Sprintf("issue %s#%d", repo, ref.Number),
			Title:  issue.GetTitle(),
			Body:   issue.GetBody(),
		})
	}
	return out, nil
}

func recordIntentAlignmentAdvisory(stores map[string]*beads.Store, repo string, number int, alignment intent.AlignmentVerdict, logger *slog.Logger) {
	store := stores["intent"]
	if store == nil {
		store = stores["quality"]
	}
	if store == nil {
		for _, candidate := range stores {
			if candidate != nil {
				store = candidate
				break
			}
		}
	}
	if store == nil {
		return
	}
	title := fmt.Sprintf("Intent alignment drift in %s#%d", repo, number)
	// "<owner>/<repo>#<n>", NOT "gh-<owner>/<repo>#<n>". The old form fused the
	// source prefix into the org when the digest built its URL, so every one of
	// these rendered a link to a github.com/gh-<owner> that does not exist
	// (#6080). The renderer strips the prefix defensively for beads already
	// written this way; this stops writing new ones.
	ref := fmt.Sprintf("%s#%d", repo, number)
	// Beads created before that change carry the prefixed form. Matching both
	// keeps this idempotent across the change: without it the first run after
	// upgrading would fail to recognise the existing bead and open a duplicate.
	legacyRef := "gh-" + ref
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeAdvisory && b.Title == title &&
			(b.ExternalRef == ref || b.ExternalRef == legacyRef) &&
			b.Status != beads.StatusClosed && b.Status != beads.StatusDone {
			return
		}
	}
	b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "intent", ref)
	if err != nil {
		logger.Warn("failed to record intent alignment advisory", "repo", repo, "number", number, "error", err)
		return
	}
	_ = store.Update(b.ID, func(bead *beads.Bead) {
		bead.Notes = alignmentSummary(alignment)
	})
}

func alignmentSummary(alignment intent.AlignmentVerdict) string {
	var parts []string
	if alignment.Rationale != "" {
		parts = append(parts, alignment.Rationale)
	}
	for _, f := range alignment.DeterministicFindings {
		if f.Status == intent.AlignmentStatusMisaligned {
			parts = append(parts, f.Code+": "+f.Reason+" ("+strings.Join(f.Files, ", ")+")")
		}
	}
	if alignment.Model != nil && alignment.Model.Status == intent.AlignmentStatusMisaligned {
		parts = append(parts, "model: "+alignment.Model.Rationale)
	}
	if len(parts) == 0 {
		return "intent alignment check reported misalignment"
	}
	return strings.Join(parts, "\n")
}

func hasMaintainerApproval(ctx context.Context, client *gh.Client, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := make(map[string]string)
	maintainer := make(map[string]bool)
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing PR reviews: %w", err)
		}
		for _, review := range reviews {
			login := review.GetUser().GetLogin()
			if login == "" {
				continue
			}
			if maintainerAssociation(review.GetAuthorAssociation()) {
				switch review.GetState() {
				case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
					latest[login] = review.GetState()
				}
				maintainer[login] = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	approved := false
	for login, state := range latest {
		if !maintainer[login] {
			continue
		}
		switch state {
		case "CHANGES_REQUESTED":
			return false, nil
		case "APPROVED":
			approved = true
		}
	}
	return approved, nil
}

func maintainerAssociation(association string) bool {
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}
