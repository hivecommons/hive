package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Verdict card and manual Promote (RFC #11585 part 6, #11699).
//
// When a campaign ends, the engine (pkg/nousengine) produces a Verdict:
// deploy, deploy with caveats or fall back to baseline, with the score
// against the baseline, citations and caveats. The dashboard mirrors it
// into GET /api/nous/status as the "verdict" object so the Experiment Lab
// can render a verdict card, including the "not measured against baseline"
// flag for campaigns that had no objective or no baseline measurements.
//
// Nothing acts on a verdict by itself. The only way a verdict becomes work
// is POST /api/nous/promote: an owner (and only an owner) picks a target —
// an issue in the target project, or a Spektacular run against that issue
// — confirms in the dashboard, and the hive performs the action once,
// records it (NousState.PromotionStore → pkg/nousstore) and audits who
// promoted which campaign to which target. A repeated Promote of the same
// target answers 409 with the existing issue or run rather than opening a
// second one. The automatic `output` modes (issue / issue+pr /
// spektacular-run) that used to fire on approve are gone; the `output`
// config section now only carries the label toggles.
//
// Like GateStore and KnowledgeStore, the seams here keep this package free
// of storage concerns: cmd/hive wires pkg/nousengine and pkg/nousstore.

// NousVerdict is a campaign's final recommendation as served by the panel.
// Recommendation uses the engine contract's vocabulary (NousVerdict*).
type NousVerdict struct {
	Campaign       string  `json:"campaign"`
	Recommendation string  `json:"recommendation"`
	Score          float64 `json:"score"`
	Baseline       float64 `json:"baseline"`
	// MeasuredAgainstBaseline is false when the campaign had no objective or
	// no baseline measurements, so Score says nothing about whether the
	// change beats what is running today (RFC §4).
	MeasuredAgainstBaseline bool     `json:"measured_against_baseline"`
	Citations               []string `json:"citations,omitempty"`
	Caveats                 []string `json:"caveats,omitempty"`
	Summary                 string   `json:"summary,omitempty"`
	// TargetRepo is the owner/name the campaign experimented on; the
	// default repo a promotion lands in.
	TargetRepo string `json:"target_repo,omitempty"`
	// Report is the path of the campaign report, when one exists.
	Report string `json:"report,omitempty"`
}

// Verdict recommendations. Mirrors nousengine's constants without
// importing the package.
const (
	NousVerdictDeploy      = "deploy"
	NousVerdictWithCaveats = "deploy_with_caveats"
	NousVerdictFallBack    = "fall_back_to_baseline"
)

// Promotion targets. Mirrors nousstore's constants without importing the
// package; cmd/hive's tests pin the two sets equal.
const (
	NousPromoteTargetIssue          = "issue"
	NousPromoteTargetSpektacularRun = "spektacular-run"
)

// ValidNousPromoteTarget reports whether s is a known promotion target.
func ValidNousPromoteTarget(s string) bool {
	switch s {
	case NousPromoteTargetIssue, NousPromoteTargetSpektacularRun:
		return true
	}
	return false
}

// NousPromotion is one owner decision to promote a verdict, as recorded.
type NousPromotion struct {
	Campaign       string    `json:"campaign"`
	Target         string    `json:"target"`
	Recommendation string    `json:"recommendation,omitempty"`
	PromotedBy     string    `json:"promoted_by"`
	PromotedAt     time.Time `json:"promoted_at"`
	Repo           string    `json:"repo,omitempty"`
	Number         int       `json:"number,omitempty"`
	URL            string    `json:"url,omitempty"`
	RunKey         string    `json:"run_key,omitempty"`
}

// NousVerdictStore serves the current campaign verdict.
type NousVerdictStore interface {
	// LoadVerdict returns the verdict and true, or false while the campaign
	// has none yet. An error means a verdict file exists but is unusable.
	LoadVerdict() (NousVerdict, bool, error)
}

// NousPromotionStore is the durable, append-only promotion record.
type NousPromotionStore interface {
	RecordPromotion(p NousPromotion) error
	Promotions() ([]NousPromotion, error)
}

// Keys of the "verdict" object served by GET /api/nous/status.
const (
	nousStatusKeyVerdict        = "verdict"
	nousVerdictKeyPromotions    = "promotions"
	nousVerdictKeyTargets       = "targets"
	nousVerdictKeyMeasured      = "measured_against_baseline"
	nousVerdictKeyRecommend     = "recommendation"
	nousVerdictKeyScore         = "score"
	nousVerdictKeyBaseline      = "baseline"
	nousVerdictKeyCampaign      = "campaign"
	nousVerdictKeyCitations     = "citations"
	nousVerdictKeyCaveats       = "caveats"
	nousVerdictKeySummary       = "summary"
	nousVerdictKeyTargetRepo    = "target_repo"
	nousVerdictKeyReport        = "report"
	nousPromotionKeyTarget      = "target"
	nousPromotionKeyPromotedBy  = "promoted_by"
	nousPromotionKeyPromotedAt  = "promoted_at"
	nousPromotionKeyRepo        = "repo"
	nousPromotionKeyNumber      = "number"
	nousPromotionKeyURL         = "url"
	nousPromotionKeyRunKey      = "run_key"
	nousPromotionKeyRecommended = "recommendation"
)

// Audit actions and response statuses of the promote path.
const (
	nousAuditPromote         = "nous_promote"
	nousAuditPromoteFailed   = "nous_promote_failed"
	nousPromoteStatusDone    = "promoted"
	nousPromoteStatusAlready = "already_promoted"
)

// Issue content constants for the issue target.
const (
	nousIssueLabelHold        = "hold"
	nousIssueTitlePrefix      = "Experiment Lab verdict"
	nousIssueTimeout          = 30 * time.Second
	nousIssueFlow             = "nous-promote"
	nousIssueUnnamedCampaign  = "campaign"
	nousIssueNotMeasuredLabel = "not measured against baseline"
)

// nousIssueFiler is the slice of *github.Client the issue target uses. A
// seam so tests need no forge.
type nousIssueFiler interface {
	CreateIssue(ctx context.Context, req ghpkg.CreateIssueRequest) (ghpkg.CreateIssueResult, error)
}

// nousIssueFilerFor returns the filer to use, or nil when this hive has no
// forge client.
func (s *Server) nousIssueFilerFor() nousIssueFiler {
	if s.nousIssueFiler != nil {
		return s.nousIssueFiler
	}
	if s.deps == nil || s.deps.GHClient == nil {
		return nil
	}
	return s.deps.GHClient
}

// SyncVerdict refreshes Verdict from VerdictStore and Promotions from
// PromotionStore. Each is a no-op without its store. Callers must NOT hold
// Mu. The returned error is for logging; the state keeps the last good
// values when a store fails.
func (ns *NousState) SyncVerdict() error {
	if ns == nil {
		return nil
	}
	var errs []error
	if ns.VerdictStore != nil {
		v, ok, err := ns.VerdictStore.LoadVerdict()
		if err != nil {
			errs = append(errs, err)
		} else {
			ns.Mu.Lock()
			if ok {
				ns.Verdict = &v
			} else {
				ns.Verdict = nil
			}
			ns.Mu.Unlock()
		}
	}
	if ns.PromotionStore != nil {
		all, err := ns.PromotionStore.Promotions()
		if err != nil {
			errs = append(errs, err)
		} else {
			ns.Mu.Lock()
			ns.Promotions = all
			ns.Mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// syncNousVerdict refreshes the verdict and promotions before a handler
// reads them, logging (not failing on) a store error.
func (s *Server) syncNousVerdict() {
	if s.deps == nil || s.deps.Nous == nil {
		return
	}
	if err := s.deps.Nous.SyncVerdict(); err != nil && s.logger != nil {
		s.logger.Warn("nous: verdict or promotions unreadable, serving last good copy", "error", err)
	}
}

// verdictMap renders the verdict plus its promotions as the "verdict"
// object of GET /api/nous/status. Callers must hold Mu. Nil when the
// campaign has no verdict.
func (ns *NousState) verdictMap() map[string]interface{} {
	if ns.Verdict == nil {
		return nil
	}
	v := ns.Verdict
	m := map[string]interface{}{
		nousVerdictKeyCampaign:  v.Campaign,
		nousVerdictKeyRecommend: v.Recommendation,
		nousVerdictKeyScore:     v.Score,
		nousVerdictKeyBaseline:  v.Baseline,
		nousVerdictKeyMeasured:  v.MeasuredAgainstBaseline,
		nousVerdictKeyTargets:   []string{NousPromoteTargetIssue, NousPromoteTargetSpektacularRun},
	}
	if len(v.Citations) > 0 {
		m[nousVerdictKeyCitations] = v.Citations
	}
	if len(v.Caveats) > 0 {
		m[nousVerdictKeyCaveats] = v.Caveats
	}
	if v.Summary != "" {
		m[nousVerdictKeySummary] = v.Summary
	}
	if v.TargetRepo != "" {
		m[nousVerdictKeyTargetRepo] = v.TargetRepo
	}
	if v.Report != "" {
		m[nousVerdictKeyReport] = v.Report
	}
	promotions := map[string]interface{}{}
	for _, p := range ns.Promotions {
		if p.Campaign != v.Campaign {
			continue
		}
		promotions[p.Target] = promotionMap(p)
	}
	m[nousVerdictKeyPromotions] = promotions
	return m
}

// promotionMap renders one promotion for the API.
func promotionMap(p NousPromotion) map[string]interface{} {
	m := map[string]interface{}{
		nousPromotionKeyTarget:     p.Target,
		nousPromotionKeyPromotedBy: p.PromotedBy,
	}
	if !p.PromotedAt.IsZero() {
		m[nousPromotionKeyPromotedAt] = p.PromotedAt.UTC().Format(time.RFC3339)
	}
	if p.Recommendation != "" {
		m[nousPromotionKeyRecommended] = p.Recommendation
	}
	if p.Repo != "" {
		m[nousPromotionKeyRepo] = p.Repo
	}
	if p.Number > 0 {
		m[nousPromotionKeyNumber] = p.Number
	}
	if p.URL != "" {
		m[nousPromotionKeyURL] = p.URL
	}
	if p.RunKey != "" {
		m[nousPromotionKeyRunKey] = p.RunKey
	}
	return m
}

// promotionOf returns the recorded promotion of the current verdict to
// target, if any. Callers must hold Mu.
func (ns *NousState) promotionOf(target string) (NousPromotion, bool) {
	if ns.Verdict == nil {
		return NousPromotion{}, false
	}
	for i := len(ns.Promotions) - 1; i >= 0; i-- {
		p := ns.Promotions[i]
		if p.Campaign == ns.Verdict.Campaign && p.Target == target {
			return p, true
		}
	}
	return NousPromotion{}, false
}

// recordPromotion persists p (when a store is wired) and mirrors it in
// memory. Callers must NOT hold Mu.
func (ns *NousState) recordPromotion(p NousPromotion) error {
	if ns.PromotionStore != nil {
		if err := ns.PromotionStore.RecordPromotion(p); err != nil {
			return err
		}
	}
	ns.Mu.Lock()
	ns.Promotions = append(ns.Promotions, p)
	ns.Mu.Unlock()
	return nil
}

// nousPromoteError carries the HTTP status a promote failure maps to.
type nousPromoteError struct {
	status int
	msg    string
}

func (e *nousPromoteError) Error() string { return e.msg }

func nousPromoteStatus(err error) int {
	var pe *nousPromoteError
	if errors.As(err, &pe) {
		return pe.status
	}
	return http.StatusInternalServerError
}

// nousPromoteRequest is the body of POST /api/nous/promote.
type nousPromoteRequest struct {
	Target string `json:"target"`
	// Repo overrides the verdict's target repo (owner/name or bare name,
	// qualified with the project org).
	Repo string `json:"repo"`
}

// nousPromoteResult is what a successful (or already-done) promote reports.
type nousPromoteResult struct {
	promotion NousPromotion
	existing  bool
}

func (s *Server) handleNousPromote(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body nousPromoteRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	body.Target = sanitizeString(body.Target)
	body.Repo = sanitizeString(body.Repo)
	if !ValidNousPromoteTarget(body.Target) {
		jsonError(w, fmt.Sprintf("target must be %s or %s", NousPromoteTargetIssue, NousPromoteTargetSpektacularRun), http.StatusBadRequest)
		return
	}

	res, err := s.promoteNousVerdict(r.Context(), body, requestUser(r))
	if err != nil {
		s.auditFromRequest(r, nousAuditPromoteFailed, auditDetail("target", body.Target, "error", err.Error()), "")
		jsonError(w, err.Error(), nousPromoteStatus(err))
		return
	}
	p := res.promotion
	if res.existing {
		s.auditFromRequest(r, nousAuditPromoteFailed, auditDetail("campaign", p.Campaign, "target", p.Target, "error", nousPromoteStatusAlready), "")
		jsonStatusResponse(w, http.StatusConflict, map[string]interface{}{
			"ok": false, "error": nousPromoteStatusAlready, "status": nousPromoteStatusAlready, "promotion": promotionMap(p),
		})
		return
	}
	s.auditFromRequest(r, nousAuditPromote, auditDetail("campaign", p.Campaign, "target", p.Target, "recommendation", p.Recommendation, "repo", p.Repo, "number", strconv.Itoa(p.Number), "run_key", p.RunKey), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "status": nousPromoteStatusDone, "promotion": promotionMap(p)})
}

// promoteNousVerdict performs the owner's chosen target once. The issue
// target opens (or reuses) the verdict issue; the Spektacular target opens
// the same issue, then admits a spec run against it.
func (s *Server) promoteNousVerdict(ctx context.Context, req nousPromoteRequest, who string) (nousPromoteResult, error) {
	ns := s.deps.Nous
	s.syncNousVerdict()

	ns.Mu.Lock()
	verdict := ns.Verdict
	if verdict == nil {
		ns.Mu.Unlock()
		return nousPromoteResult{}, &nousPromoteError{status: http.StatusConflict, msg: "no verdict to promote"}
	}
	v := *verdict
	if existing, ok := ns.promotionOf(req.Target); ok {
		ns.Mu.Unlock()
		return nousPromoteResult{promotion: existing, existing: true}, nil
	}
	issue, hasIssue := ns.promotionOf(NousPromoteTargetIssue)
	holdLabel := nousConfigAutoHold(ns.Config)
	configuredRepo := nousConfiguredRepo(ns.Config)
	ns.Mu.Unlock()

	repo := req.Repo
	if repo == "" {
		repo = v.TargetRepo
	}
	if repo == "" {
		repo = configuredRepo
	}
	if s.deps.Config != nil {
		repo = config.QualifyRepo(s.deps.Config.Project.Org, repo)
	}
	if repo == "" {
		return nousPromoteResult{}, &nousPromoteError{status: http.StatusBadRequest, msg: "repo is required: the verdict names no target repo"}
	}
	if err := validateSpekHubRepoPath(repo); err != nil || !strings.Contains(repo, "/") {
		return nousPromoteResult{}, &nousPromoteError{status: http.StatusBadRequest, msg: fmt.Sprintf("repo %q must be owner/name", repo)}
	}

	// The Spektacular target needs its preconditions checked before the
	// issue is opened, so a disabled runner cannot leave a stray issue.
	if req.Target == NousPromoteTargetSpektacularRun {
		if err := s.nousRunPreconditions(); err != nil {
			return nousPromoteResult{}, err
		}
	}

	p := NousPromotion{
		Campaign:       v.Campaign,
		Target:         req.Target,
		Recommendation: v.Recommendation,
		PromotedBy:     who,
		PromotedAt:     ns.nousGateNow(),
	}
	if hasIssue && issue.Repo == repo && issue.Number > 0 {
		p.Repo, p.Number, p.URL = issue.Repo, issue.Number, issue.URL
	} else {
		filer := s.nousIssueFilerFor()
		if filer == nil {
			return nousPromoteResult{}, &nousPromoteError{status: http.StatusServiceUnavailable, msg: "GitHub client not available"}
		}
		title, body := nousVerdictIssueContent(v, who)
		meta := ghpkg.InvocationMeta{Agent: ghpkg.AttributionAgentDashboard}
		if s.deps.Config != nil && s.deps.Config.Governor.AttributionTrailerEnabled() {
			body = ghpkg.AppendTrailer(body, meta)
		}
		var labels []string
		if holdLabel {
			labels = []string{nousIssueLabelHold}
		}
		ctx, cancel := context.WithTimeout(ctx, nousIssueTimeout)
		defer cancel()
		created, err := filer.CreateIssue(ctx, ghpkg.CreateIssueRequest{Repo: repo, Title: title, Body: body, Labels: labels, Origin: config.OriginSourceDashboard}) // writeback:solicited authenticated operator explicitly promotes this verdict to an issue.
		if err != nil {
			return nousPromoteResult{}, &nousPromoteError{status: http.StatusBadGateway, msg: "create issue: " + err.Error()}
		}
		p.Repo, p.Number, p.URL = repo, created.Number, created.URL
		s.audit.Log(who, ghpkg.AuditActionHiveIssueCreated, meta.AuditDetail("repo", repo, "number", strconv.Itoa(created.Number), "url", created.URL, "flow", nousIssueFlow), "")
	}

	if req.Target == NousPromoteTargetSpektacularRun {
		title, _ := nousVerdictIssueContent(v, who)
		// Lease TTLs are wall-clock, so admission uses time.Now even when
		// the state clock is injected.
		if err := s.AdmitRun(p.Repo, p.Number, title, time.Now()); err != nil {
			return nousPromoteResult{}, &nousPromoteError{status: http.StatusInternalServerError, msg: "admit run: " + err.Error()}
		}
		p.RunKey = worksource.Ref{Repo: p.Repo, Number: p.Number}.Key()
	}

	if err := ns.recordPromotion(p); err != nil {
		return nousPromoteResult{}, &nousPromoteError{status: http.StatusInternalServerError, msg: "record promotion: " + err.Error()}
	}
	return nousPromoteResult{promotion: p}, nil
}

// nousRunPreconditions reports why a Spektacular run cannot be admitted:
// the runner is disabled (409) or the lease registry is unavailable (503).
func (s *Server) nousRunPreconditions() error {
	if s.deps.Config == nil || !s.deps.Config.Runs.Spektacular.Enabled {
		return &nousPromoteError{status: http.StatusConflict, msg: "runs.spektacular.enabled is required to admit a run"}
	}
	if s.contributeHub == nil {
		return &nousPromoteError{status: http.StatusServiceUnavailable, msg: "run lease registry unavailable"}
	}
	return nil
}

// nousConfigAutoHold reads the output section's autoHold toggle.
func nousConfigAutoHold(cfg map[string]interface{}) bool {
	output, ok := cfg["output"].(map[string]interface{})
	if !ok {
		return false
	}
	hold, _ := output["autoHold"].(bool)
	return hold
}

// nousConfiguredRepo returns the single configured repo target, or "" when
// none or several are configured (the owner must then name one).
func nousConfiguredRepo(cfg map[string]interface{}) string {
	section, ok := cfg["repos"].(map[string]interface{})
	if !ok {
		return ""
	}
	repos, ok := section["repos"].([]interface{})
	if !ok || len(repos) != 1 {
		return ""
	}
	repo, _ := repos[0].(string)
	return strings.TrimSpace(repo)
}

// nousVerdictIssueContent renders the promoted verdict as an issue: the
// recommendation, score vs. baseline (or the not-measured flag), citations,
// caveats and the report path, plus who promoted it.
func nousVerdictIssueContent(v NousVerdict, who string) (title, body string) {
	campaign := strings.TrimSpace(v.Campaign)
	if campaign == "" {
		campaign = nousIssueUnnamedCampaign
	}
	title = fmt.Sprintf("%s: %s (%s)", nousIssueTitlePrefix, nousVerdictLabel(v.Recommendation), campaign)

	var b strings.Builder
	fmt.Fprintf(&b, "## Experiment Lab verdict\n\n")
	fmt.Fprintf(&b, "**Campaign:** `%s`  \n", campaign)
	fmt.Fprintf(&b, "**Verdict:** %s  \n", nousVerdictLabel(v.Recommendation))
	if v.MeasuredAgainstBaseline {
		fmt.Fprintf(&b, "**Score vs. baseline:** %s vs. %s  \n", formatNousScore(v.Score), formatNousScore(v.Baseline))
	} else {
		fmt.Fprintf(&b, "**Score:** %s — ⚠️ %s. The campaign had no objective or no baseline measurements, so this score does not show whether the change beats what is running today.  \n", formatNousScore(v.Score), nousIssueNotMeasuredLabel)
	}
	if v.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(v.Summary))
	}
	if len(v.Citations) > 0 {
		b.WriteString("\n### Evidence\n\n")
		for _, c := range v.Citations {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(v.Caveats) > 0 {
		b.WriteString("\n### Caveats\n\n")
		for _, c := range v.Caveats {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if v.Report != "" {
		fmt.Fprintf(&b, "\n**Report:** `%s`\n", v.Report)
	}
	fmt.Fprintf(&b, "\n---\nPromoted by hand from the Experiment Lab by %s. Nothing in this campaign was applied automatically.\n", who)
	return title, b.String()
}

// nousVerdictLabel is the human wording of a recommendation.
func nousVerdictLabel(recommendation string) string {
	switch recommendation {
	case NousVerdictDeploy:
		return "deploy"
	case NousVerdictWithCaveats:
		return "deploy with caveats"
	case NousVerdictFallBack:
		return "fall back to baseline"
	}
	return strings.ReplaceAll(recommendation, "_", " ")
}

func formatNousScore(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
