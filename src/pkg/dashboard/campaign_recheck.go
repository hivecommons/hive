package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	recheckReasonCadence = "cadence"
	recheckReasonManual  = "manual"
	gitHeadTimeout       = 2 * time.Second
)

// RecheckEvidenceSource is the extension point for evidence gathered before a
// recheck revision starts. Outward discovery is gathered separately by
// collectRecheckDiscovery (recheck_discovery.go).
type RecheckEvidenceSource interface {
	Evidence(ctx context.Context, campaign Campaign, priorHead string) (CampaignDrift, error)
}

type codebaseHeadEvidenceSource struct{}

func (codebaseHeadEvidenceSource) Evidence(ctx context.Context, _ Campaign, priorHead string) (CampaignDrift, error) {
	current := currentGitHead(ctx)
	return CampaignDrift{
		CodebaseChanged: priorHead != "" && current != "" && priorHead != current,
		PriorHeadSHA:    priorHead,
		CurrentHeadSHA:  current,
	}, nil
}

type campaignRecheckResponse struct {
	OK       bool     `json:"ok"`
	Campaign Campaign `json:"campaign"`
	Message  string   `json:"message,omitempty"`
}

func (s *Server) handleCampaignRecheck(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	if id == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	campaign, err := s.triggerCampaignRecheck(r.Context(), id, requestUser(r), recheckReasonManual, r.URL.Query().Get("force") == "true", time.Now())
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errRecheckDisabled) {
			status = http.StatusNotFound
		} else if errors.Is(err, errRecheckInFlight) || errors.Is(err, knowledge.ErrCampaignLeaseHeld) {
			status = http.StatusConflict
		} else if errors.Is(err, errRecheckNotFound) {
			status = http.StatusNotFound
		}
		jsonError(w, err.Error(), status)
		return
	}
	jsonResponse(w, campaignRecheckResponse{OK: true, Campaign: campaign, Message: "Spektacular recheck rewound the run to a new generation"})
}

var (
	errRecheckDisabled = errors.New("campaign recheck is not enabled")
	errRecheckInFlight = errors.New("campaign recheck already in flight")
	errRecheckNotFound = errors.New("campaign not found")
)

// campaignRecheckMu serialises rewinds so the stage-lease check, the archive
// rewind and the spec admission of one recheck cannot interleave with another.
var campaignRecheckMu sync.Mutex

func (s *Server) TickCampaignRechecks(ctx context.Context, now time.Time) {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.Runs.Spektacular.Recheck.Enabled {
		return
	}
	campaigns, err := s.allCampaigns(nil)
	if err != nil {
		return
	}
	for _, campaign := range campaigns {
		if campaign.CurrentStage != "completed" && campaign.Status != "shipped" {
			continue
		}
		recheck := campaign.Recheck
		if recheck == nil || !recheck.Enabled || recheck.InFlight || recheck.NextAt == "" {
			continue
		}
		next, err := time.Parse(time.RFC3339, recheck.NextAt)
		if err != nil || now.Before(next) {
			continue
		}
		if _, err := s.triggerCampaignRecheck(ctx, campaign.ID, "system", recheckReasonCadence, false, now); err != nil && s.logger != nil {
			s.logger.Warn("[spektacular] campaign recheck skipped", "campaign", campaign.ID, "error", err)
		}
	}
}

// triggerCampaignRecheck rewinds the campaign's existing run to a new
// generation (ADR 0020 v6 addendum): the same archive and run key get
// Revision N+1 and a fresh spec stage admitted on the run's own lease key.
func (s *Server) triggerCampaignRecheck(ctx context.Context, id, actor, reason string, force bool, now time.Time) (Campaign, error) {
	if s == nil || s.deps == nil || s.deps.Inception == nil || s.contributeHub == nil {
		return Campaign{}, errors.New("campaign store unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
	campaignRecheckMu.Lock()
	defer campaignRecheckMu.Unlock()
	campaigns, err := s.allCampaigns(nil)
	if err != nil {
		return Campaign{}, err
	}
	var base Campaign
	for _, campaign := range campaigns {
		if campaign.ID == id || campaign.RunKey == id {
			base = campaign
			break
		}
	}
	if base.ID == "" {
		return Campaign{}, errRecheckNotFound
	}
	enabled := force || (base.Recheck != nil && base.Recheck.Enabled)
	if !enabled {
		return Campaign{}, errRecheckDisabled
	}
	if base.Recheck != nil && base.Recheck.InFlight {
		return Campaign{}, errRecheckInFlight
	}
	repo := firstCampaignRepo(base)
	runKey := runKeyOfLease(firstRunNonEmpty(base.RunKey, base.Source, base.ID), repo)
	if s.contributeHub.runHasLiveStageLease(runKey, now) {
		return Campaign{}, errRecheckInFlight
	}
	interval := s.recheckInterval(base)
	evidence, _ := (codebaseHeadEvidenceSource{}).Evidence(ctx, base, priorCampaignHead(base))
	external, failures := s.collectRecheckDiscovery(ctx, base, parseCampaignTime(base.LastActivity))
	archive, err := s.deps.Inception.RewindExternalCampaign(base.ID, knowledge.CampaignRewind{
		Title: base.Title, Source: runKey, Engine: base.Engine, Type: base.Type, Repos: base.Repos,
		Actor: actor, Reason: reason, LastGen: base.RunGen,
		Recheck: &knowledge.CampaignRecheck{Enabled: true, Interval: interval, LastAt: now, LastDeltaCount: recheckLastDelta(base)},
		Drift: &knowledge.CampaignDrift{
			CodebaseChanged: evidence.CodebaseChanged,
			PriorHeadSHA:    evidence.PriorHeadSHA,
			CurrentHeadSHA:  evidence.CurrentHeadSHA,
			DeltaCount:      evidence.DeltaCount,
			RecheckReason:   reason,
			External:        external,
			ExternalCount:   len(external),
			SourcesFailed:   failures,
		},
	}, now)
	if err != nil {
		if errors.Is(err, knowledge.ErrCampaignRewindInFlight) {
			return Campaign{}, errRecheckInFlight
		}
		return Campaign{}, err
	}
	gen := base.RunGen + 1
	if err := s.contributeHub.admitRecheckGeneration(repo, runKey, base.Title, gen, now); err != nil {
		return Campaign{}, err
	}
	// The revise lease only bridges the rewind until the spec stage lease
	// exists; from here on Drift carries the in-flight state (R2).
	if released, err := s.deps.Inception.ReleaseCampaignArchive(archive.ID, actor, now); err == nil && released != nil {
		archive = released
	}
	if s.audit != nil {
		s.audit.Log(actor, "campaign_recheck", auditDetail("campaign", base.ID, "revision", strconv.Itoa(archive.Revision), "reason", reason), "")
	}
	out := campaignFromInceptionArchive(*archive)
	if out.Recheck != nil {
		out.Recheck.InFlight = true
	}
	return out, nil
}

// runHasLiveStageLease reports whether any unexpired stage lease belongs to
// runKey; a rewind is refused while one is live (R3).
func (h *ContributeWSHub) runHasLiveStageLease(runKey string, now time.Time) bool {
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	return h.runHasLiveStageLeaseLocked(runKey, now)
}

func (h *ContributeWSHub) runHasLiveStageLeaseLocked(runKey string, now time.Time) bool {
	for _, l := range h.leases {
		if l != nil && l.stage != "" && runKeyOfLease(l.key, l.repo) == runKey && !l.expiresAt.IsZero() && now.Before(l.expiresAt) {
			return true
		}
	}
	return false
}

// admitRecheckGeneration admits the rewound generation's spec stage on the
// run's own admission lease key (<repo>!<runKey>:spec, task run-admit-<runKey>)
// with a gen past every gen the run already used, so prior receipts survive.
func (h *ContributeWSHub) admitRecheckGeneration(repo, runKey, title string, gen uint64, now time.Time) error {
	stageKey := runKey + ":" + StageSpec
	if repo != "" {
		stageKey = repo + "!" + stageKey
	}
	taskID := runAdmissionTaskPrefix + sanitizeReceiptSegment(runKey)
	ref, _ := worksource.ParseKey(runKey)
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.retiredRuns[runKey] == runRetiredAbandoned {
		return fmt.Errorf("run %s was abandoned", runKey)
	}
	if h.runHasLiveStageLeaseLocked(runKey, now) {
		return errRecheckInFlight
	}
	if next := nextRunAdmissionGen(runKey); next > gen {
		gen = next
	}
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	lease := &taskLease{
		identity:  runAdmissionIdentity,
		taskID:    taskID,
		repo:      repo,
		number:    ref.Number,
		key:       stageKey,
		title:     title,
		tier:      "triage",
		stage:     StageSpec,
		gen:       gen,
		expiresAt: now.Add(leaseTTL),
	}
	if ref.Repo != "" {
		lease.workItem = worksource.WorkItemContextFromRef(ref, title).Normalized()
	}
	k := leaseKey(runAdmissionIdentity, taskID)
	prev := h.leases[k]
	h.leases[k] = lease
	if err := h.saveLeasesLocked(); err != nil {
		if prev != nil {
			h.leases[k] = prev
		} else {
			delete(h.leases, k)
		}
		return fmt.Errorf("persisting recheck spec lease for %s: %w", taskID, err)
	}
	return nil
}

func (s *Server) recheckInterval(c Campaign) time.Duration {
	if c.Recheck != nil && c.Recheck.Interval != "" {
		if d, err := time.ParseDuration(c.Recheck.Interval); err == nil && d > 0 {
			return d
		}
	}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Runs.Spektacular.DefaultRecheckInterval()
	}
	return config.DefaultSpektacularRecheckInterval
}

func (s *Server) decorateCampaignRechecks(campaigns map[string]Campaign) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return
	}
	defaultEnabled := s.deps.Config.Runs.Spektacular.Recheck.Enabled
	now := time.Now()
	for id, c := range campaigns {
		if c.Recheck == nil {
			interval := s.deps.Config.Runs.Spektacular.DefaultRecheckInterval()
			c.Recheck = &CampaignRecheck{Enabled: defaultEnabled, Interval: interval.String()}
		}
		c.Recheck.InFlight = c.Recheck.InFlight || campaignRecheckInFlight(c)
		if c.Recheck.Enabled && c.Recheck.NextAt == "" {
			last := parseCampaignTime(c.LastActivity)
			if last.IsZero() {
				last = now
			}
			if c.Recheck.LastAt != "" {
				if t, err := time.Parse(time.RFC3339, c.Recheck.LastAt); err == nil {
					last = t
				}
			}
			if d, err := time.ParseDuration(c.Recheck.Interval); err == nil && d > 0 {
				c.Recheck.NextAt = last.Add(d).Format(time.RFC3339)
			}
		}
		campaigns[id] = c
	}
}

// campaignRecheckInFlight is the ADR 0020 v6 in-flight predicate: the revise
// lease is held, or Drift is set and the rewound generation has not completed.
func campaignRecheckInFlight(c Campaign) bool {
	if c.reviseLeaseHeld {
		return true
	}
	return c.Drift != nil && c.CurrentStage != "completed" && c.Status != "shipped"
}

func campaignRecheckFromArchive(archive knowledge.InceptionCampaignArchive, inFlight bool) *CampaignRecheck {
	if archive.Recheck == nil && archive.RecheckInterval > 0 {
		return campaignRecheckFromKnowledge(&knowledge.CampaignRecheck{Enabled: true, Interval: archive.RecheckInterval}, inFlight, archive.ArchivedAt)
	}
	return campaignRecheckFromKnowledge(archive.Recheck, inFlight, archive.ArchivedAt)
}

func campaignRecheckFromKnowledge(recheck *knowledge.CampaignRecheck, inFlight bool, basis time.Time) *CampaignRecheck {
	if recheck == nil {
		return nil
	}
	out := &CampaignRecheck{
		Enabled:        recheck.Enabled,
		Interval:       recheck.Interval.String(),
		LastAt:         formatRunTime(recheck.LastAt),
		InFlight:       inFlight,
		LastDeltaCount: recheck.LastDeltaCount,
	}
	if recheck.Enabled && recheck.Interval > 0 {
		last := recheck.LastAt
		if last.IsZero() {
			last = basis
		}
		if !last.IsZero() {
			out.NextAt = last.Add(recheck.Interval).Format(time.RFC3339)
		}
	}
	return out
}

func parseCampaignTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func campaignDriftFromArchive(archive knowledge.InceptionCampaignArchive) *CampaignDrift {
	if archive.Drift == nil {
		return nil
	}
	return &CampaignDrift{
		CodebaseChanged: archive.Drift.CodebaseChanged,
		PriorHeadSHA:    archive.Drift.PriorHeadSHA,
		CurrentHeadSHA:  archive.Drift.CurrentHeadSHA,
		PriorRevision:   archive.Drift.PriorRevision,
		DeltaCount:      archive.Drift.DeltaCount,
		RecheckReason:   archive.Drift.RecheckReason,
		External:        campaignExternalEvidenceFromKnowledge(archive.Drift.External),
		ExternalCount:   archive.Drift.ExternalCount,
		SourcesFailed:   campaignSourceFailuresFromKnowledge(archive.Drift.SourcesFailed),
	}
}

func campaignExternalEvidenceFromKnowledge(in []knowledge.CampaignExternalEvidence) []CampaignExternalEvidence {
	if len(in) == 0 {
		return nil
	}
	out := make([]CampaignExternalEvidence, 0, len(in))
	for _, ev := range in {
		out = append(out, CampaignExternalEvidence{
			Source:      ev.Source,
			Kind:        ev.Kind,
			Title:       ev.Title,
			URL:         ev.URL,
			PublishedAt: formatRunTime(ev.PublishedAt),
			Summary:     ev.Summary,
			SHA256:      ev.SHA256,
		})
	}
	return out
}

func campaignSourceFailuresFromKnowledge(in []knowledge.CampaignSourceFailure) []CampaignSourceFailure {
	if len(in) == 0 {
		return nil
	}
	out := make([]CampaignSourceFailure, 0, len(in))
	for _, fail := range in {
		out = append(out, CampaignSourceFailure{Name: fail.Name, Reason: fail.Reason})
	}
	return out
}

func firstCampaignRepo(c Campaign) string {
	if len(c.Repos) > 0 && strings.TrimSpace(c.Repos[0]) != "" {
		return strings.TrimSpace(c.Repos[0])
	}
	if c.RunKey != "" {
		if ref, ok := splitRunRepo(c.RunKey); ok {
			return ref
		}
	}
	return ""
}

func splitRunRepo(key string) (string, bool) {
	if i := strings.Index(key, "#"); i > 0 {
		return key[:i], true
	}
	if i := strings.Index(key, "!"); i > 0 {
		return key[:i], true
	}
	return "", false
}

func priorCampaignHead(c Campaign) string {
	if c.Drift != nil && c.Drift.CurrentHeadSHA != "" {
		return c.Drift.CurrentHeadSHA
	}
	return ""
}

func recheckLastDelta(c Campaign) int {
	if c.Recheck != nil {
		return c.Recheck.LastDeltaCount
	}
	if c.Drift != nil {
		return c.Drift.DeltaCount
	}
	return 0
}

func currentGitHead(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, gitHeadTimeout)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
