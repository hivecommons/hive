package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	recheckReasonCadence = "cadence"
	recheckReasonManual  = "manual"
	recheckTaskPrefix    = "spek-recheck-"
	gitHeadTimeout       = 2 * time.Second
)

// RecheckEvidenceSource is the extension point for evidence gathered before a
// recheck revision starts. Discovery snapshots remain evidence only.
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
		if errors.Is(err, errRecheckUnavailable) {
			status = http.StatusNotImplemented
		} else if errors.Is(err, errRecheckDisabled) {
			status = http.StatusNotFound
		} else if errors.Is(err, errRecheckInFlight) {
			status = http.StatusConflict
		} else if errors.Is(err, errRecheckNotFound) {
			status = http.StatusNotFound
		}
		jsonError(w, err.Error(), status)
		return
	}
	jsonResponse(w, campaignRecheckResponse{OK: true, Campaign: campaign, Message: "Spektacular recheck revision created"})
}

var (
	errRecheckUnavailable = errors.New("Spek continuous convergence recheck is disabled on v6 pending #10734")
	errRecheckDisabled    = errors.New("campaign recheck is not enabled")
	errRecheckInFlight    = errors.New("campaign recheck revision already in flight")
	errRecheckNotFound    = errors.New("campaign not found")
)

var spekRecheckDisabledOnV6 = true

func (s *Server) TickCampaignRechecks(ctx context.Context, now time.Time) {
	if spekRecheckDisabledOnV6 {
		return
	}
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

func (s *Server) triggerCampaignRecheck(ctx context.Context, id, actor, reason string, force bool, now time.Time) (Campaign, error) {
	if spekRecheckDisabledOnV6 {
		return Campaign{}, errRecheckUnavailable
	}
	if s == nil || s.deps == nil || s.deps.Inception == nil || s.contributeHub == nil {
		return Campaign{}, errors.New("campaign store unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
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
	source := firstRunNonEmpty(base.RunKey, base.Source, base.ID)
	baseArchive, err := s.deps.Inception.UpsertExternalCampaign(base.ID, base.Title, source, base.Engine, base.Type, base.Repos, now)
	if err != nil {
		return Campaign{}, err
	}
	revision, err := s.deps.Inception.ReviseExternalCampaign(baseArchive.ID, base.Title, source, base.Engine, base.Type, actor, base.Repos, now)
	if err != nil {
		return Campaign{}, err
	}
	interval := s.recheckInterval(base)
	revision.Recheck = &knowledge.CampaignRecheck{Enabled: true, Interval: interval}
	evidence, _ := (codebaseHeadEvidenceSource{}).Evidence(ctx, base, priorCampaignHead(base))
	evidence.PriorRevision = firstRunNonEmpty(base.RevisionOf, base.ID)
	evidence.RecheckReason = reason
	external, failures := s.collectRecheckDiscovery(ctx, base, parseCampaignTime(base.LastActivity))
	revision.Drift = &knowledge.CampaignDrift{
		DriftSource:     s.discoverRecheckSources(ctx),
		CodebaseChanged: evidence.CodebaseChanged,
		PriorHeadSHA:    evidence.PriorHeadSHA,
		CurrentHeadSHA:  evidence.CurrentHeadSHA,
		PriorRevision:   evidence.PriorRevision,
		DeltaCount:      evidence.DeltaCount,
		RecheckReason:   evidence.RecheckReason,
		External:        external,
		ExternalCount:   len(external),
		SourcesFailed:   failures,
	}
	if _, err := s.deps.Inception.SetCampaignRecheck(revision.ID, revision.Recheck); err != nil {
		return Campaign{}, err
	}
	if _, err := s.deps.Inception.SetCampaignDrift(revision.ID, revision.Drift); err != nil {
		return Campaign{}, err
	}
	baseMeta := &knowledge.CampaignRecheck{Enabled: true, Interval: interval, LastAt: now, LastDeltaCount: recheckLastDelta(base)}
	if _, err := s.deps.Inception.SetCampaignRecheck(baseArchive.ID, baseMeta); err == nil {
		base.Recheck = campaignRecheckFromKnowledge(baseMeta, false, now)
	}
	repo := firstCampaignRepo(base)
	leaseKey := revision.ID + ":" + StageSpec
	if repo != "" {
		leaseKey = repo + "!" + leaseKey
	}
	taskID := recheckTaskPrefix + sanitizeReceiptSegment(revision.ID)
	if err := s.contributeHub.recordLeaseForKeyStage(runAdmissionIdentity, taskID, repo, 0, leaseKey, "triage", StageSpec, 1, now); err != nil {
		return Campaign{}, err
	}
	if s.audit != nil {
		s.audit.Log(actor, "campaign_recheck", auditDetail("campaign", base.ID, "revision", revision.ID, "reason", reason), "")
	}
	return campaignFromInceptionArchive(*revision), nil
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
	inFlight := map[string]bool{}
	for _, c := range campaigns {
		if c.RevisionOf != "" && c.CurrentStage != "completed" && c.Status != "shipped" {
			inFlight[c.RevisionOf] = true
		}
	}
	for id, c := range campaigns {
		if c.Recheck == nil {
			interval := s.deps.Config.Runs.Spektacular.DefaultRecheckInterval()
			c.Recheck = &CampaignRecheck{Enabled: defaultEnabled, Interval: interval.String()}
		}
		c.Recheck.InFlight = c.Recheck.InFlight || inFlight[id]
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
		DriftSource:     archive.Drift.DriftSource,
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
