package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/nous"
)

// Self-report publisher (hivecommons/hive#11868). Each governor cycle it takes
// the fleetreport.Result the dashboard attached to /api/status and decides,
// per report, whether to write upstream:
//
//   - dry-run → nothing (preview stays on the dashboard);
//   - Nous observations → the hub relay only; the spoke GitHub client is never used;
//   - the API budget is not normal → defer ordinary reports (observations still relay);
//   - fingerprint already written by this hive → at most one "also seen"
//     comment per 24h, otherwise skip;
//   - fingerprint closed upstream inside the 7-day cooldown → record and skip
//     (known / fixed pending rollout);
//   - otherwise join an existing issue (fingerprint, hive/selfreport+class,
//     or class keywords) or create one, subject to the per-day cap and the
//     1h gap between NEW filings of different classes;
//   - a 403/404 from the report repo → relay to the hub, which files under
//     the App identity.
//
// Cleared runtime conditions get a single "cleared on <hive>" comment; the
// issue is never auto-closed.

// selfReportHubRelayPath is the hub ingest the spoke falls back to.
const selfReportHubRelayPath = "/api/selfreport/ingest"
const selfReportObservationClass = "observation"

// selfReportRelayTimeout bounds one relay POST.
const selfReportRelayTimeout = 20 * time.Second

// selfReportWriter is the GitHub seam the publisher needs; *github.Client
// satisfies it and tests supply a fake.
type selfReportWriter interface {
	EnsureFleetReportWithOptions(ctx context.Context, report fleetreport.Report, opts github.FleetReportOptions) (github.FleetReportResult, error)
	PostFleetReportRecovery(ctx context.Context, issueNumber int, report fleetreport.Report, closeIfOwner bool) error
	PostFleetReportCleared(ctx context.Context, repo string, issueNumber int, body string) error
	FleetReportIssue(ctx context.Context, fingerprint string) (github.FleetReportResult, bool, error)
}

// selfReportRelay POSTs a report to the hub. nil = no hub.
type selfReportRelay func(ctx context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error)

// selfReportRelayRequest is the wire shape shared with pkg/hub.
type selfReportRelayRequest struct {
	Action      string   `json:"action,omitempty"`
	HiveID      string   `json:"hive_id"`
	Fingerprint string   `json:"fingerprint"`
	Class       string   `json:"class,omitempty"`
	Title       string   `json:"title,omitempty"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels,omitempty"`
	Version     string   `json:"version,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	InstanceID  string   `json:"instance_id,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	// Owners / OwnersHidden name the hive's owner login(s) for the
	// affected block; see fleetreport.Report.
	Owners       []string                        `json:"owners,omitempty"`
	OwnersHidden bool                            `json:"owners_hidden,omitempty"`
	Evidence     fleetreport.Evidence            `json:"evidence"`
	Observation  *fleetreport.ObservationFinding `json:"observation,omitempty"`
	// HiveState is the reporting hive's 24 h / 7 d state trends (#12339).
	HiveState *hivestate.Trends `json:"hive_state,omitempty"`
}

type selfReportRelayResponse struct {
	OK     bool   `json:"ok"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Joined bool   `json:"joined"`
	Reason string `json:"reason,omitempty"`
}

type selfReportPublishDeps struct {
	dryRun       bool
	gh           selfReportWriter
	relay        selfReportRelay
	repo         string
	hiveSlug     string
	version      string
	cluster      string
	limits       fleetreport.FilingLimits
	now          func() time.Time
	tracker      *fleetreport.RuntimeTracker
	budgetOK     func() bool
	hubLinked    bool
	observations *nous.ObservationLedger
}

func newSelfReportPublishDeps(cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server) selfReportPublishDeps {
	version, _ := dashboardBuildInfo()
	d := selfReportPublishDeps{
		dryRun:       true,
		repo:         config.DefaultFleetReportRepo,
		limits:       fleetreport.DefaultFilingLimits,
		now:          time.Now,
		tracker:      fleetreport.DefaultTracker(),
		version:      version,
		cluster:      os.Getenv("HIVE_CLUSTER_ID"),
		observations: nous.NewObservationLedger(nous.DefaultObservationLedgerPath),
	}
	if ghClient != nil {
		d.gh = ghClient
		d.budgetOK = func() bool {
			mode, _ := ghClient.APIBudgetMode()
			return mode == github.APIBudgetNormal
		}
	}
	if dashSrv != nil {
		d.dryRun = !dashSrv.FleetReportFilesUpstream()
		d.hiveSlug = dashSrv.FleetReportHiveSlug()
		d.hubLinked = dashSrv.FleetReportHubManaged()
	}
	if cfg != nil {
		d.repo = cfg.Governor.FleetReport.EffectiveRepo()
		d.limits = d.limits.WithMaxPerDay(cfg.Governor.FleetReport.EffectiveMaxPerDay())
		if d.hubLinked && strings.TrimSpace(cfg.Hub.URL) != "" {
			d.relay = hubSelfReportRelay(strings.TrimRight(strings.TrimSpace(cfg.Hub.URL), "/"), spoke.SpokeHeartbeatKey)
		}
	}
	return d
}

// dashboardBuildInfo mirrors dashboard.SetFleetReportBuildInfo's inputs.
func dashboardBuildInfo() (string, string) { return reportedVersion(), gitShort }

func publishFleetReportsWith(ctx context.Context, logger *slog.Logger, dashSrv *dashboard.Server, res *fleetreport.Result, deps selfReportPublishDeps) {
	if res == nil || deps.dryRun || dashSrv == nil || (deps.gh == nil && deps.relay == nil) {
		return
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	budgetNormal := true
	if deps.budgetOK != nil {
		budgetNormal = deps.budgetOK()
	}
	hasObservation := false
	for _, report := range res.Reports {
		if isObservationReport(report) {
			hasObservation = true
			break
		}
	}
	if !budgetNormal && !hasObservation {
		logger.Info("fleet report: API budget not normal; deferring self-reports", "reports", len(res.Reports))
		return
	}
	now := deps.now()
	state := dashSrv.FleetReportState()
	var observationRecoveries []nous.RecoveryRequest
	if deps.observations != nil {
		active := make([]nous.ActiveObservation, 0)
		for _, report := range res.Reports {
			if !isObservationReport(report) {
				continue
			}
			before, after := observationBeforeAfter(report)
			open := state.Open[report.Fingerprint]
			issueNumber, issueURL := open.Number, open.URL
			if open.Recovered || !open.ClosedAt.IsZero() {
				issueNumber, issueURL = 0, ""
			}
			active = append(active, nous.ActiveObservation{
				Fingerprint: report.Fingerprint,
				IssueNumber: issueNumber,
				URL:         issueURL,
				Before:      before,
				After:       after,
				Evidence:    report.Body,
			})
		}
		var err error
		observationRecoveries, err = deps.observations.Reconcile(active, now)
		if err != nil {
			logger.Warn("fleet report: observation lifecycle ledger failed", "error", err)
		}
	}
	for _, report := range res.Reports {
		isObservation := isObservationReport(report)
		if !isObservation && !budgetNormal {
			logger.Info("fleet report: API budget not normal; deferring non-observation self-report", "fingerprint", report.Fingerprint)
			continue
		}
		open := state.Open[report.Fingerprint]
		if isObservation && open.Recovered {
			open = fleetreport.OpenIssue{}
		}
		if fleetreport.ClosedRecently(open.ClosedAt, now, deps.limits) {
			continue
		}
		seenAllowed := fleetreport.AllowSeenComment(open, now, deps.limits)
		if open.Number > 0 && !open.PostedAt.IsZero() && !seenAllowed {
			continue
		}

		var write github.FleetReportResult
		var err error
		if isObservation {
			// Observations are hub-owned reports. Never fall back to the
			// spoke's GitHub client, even when it can write another repo.
			if deps.relay == nil {
				err = errSelfReportObservationNoRelay
			} else {
				logger.Info("fleet report: relaying Nous observation to hub", "fingerprint", report.Fingerprint, "kind", report.Class)
				var relayed selfReportRelayResponse
				relayed, err = deps.relay(ctx, selfReportRelayRequest{
					HiveID: deps.hiveSlug, Fingerprint: report.Fingerprint, Class: fleetreport.TriggerObservation,
					Title: report.Title, Body: report.Body, Labels: report.Labels, Version: deps.version,
					Repo: config.DefaultFleetReportRepo, Evidence: firstEvidence(report),
					Observation: report.Observation, HiveState: report.HiveState,
				})
				if err == nil {
					write = github.FleetReportResult{
						Number: relayed.Number, URL: relayed.URL, Created: !relayed.Joined,
						Joined: relayed.Joined, Commented: relayed.Joined,
					}
				}
			}
		} else {
			opts := github.FleetReportOptions{
				Repo: deps.repo, HiveSlug: deps.hiveSlug, Version: deps.version, Cluster: deps.cluster,
				Now: deps.now, JoinExisting: report.Trigger == fleetreport.TriggerRuntimeError, SeenCommentAllowed: seenAllowed,
			}
			// The cap and gap apply to NEW issues only; joining never counts.
			// We cannot know before the search whether this will be a create, so
			// gate the create path with a pre-check and record afterwards.
			allowNew, reason := fleetreport.AllowNewFiling(state, now, deps.limits)
			if deps.gh != nil {
				write, err = deps.gh.EnsureFleetReportWithOptions(ctx, report, withCreateGate(opts, allowNew))
			} else {
				err = errSelfReportNoDirectWriter
			}
			if err != nil && errors.Is(err, errSelfReportCreateDeferred) {
				logger.Info("fleet report: new filing deferred", "fingerprint", report.Fingerprint, "class", report.Class, "reason", reason)
				dashSrv.UpdateFleetReportState(func(st *fleetreport.State) {
					o := st.Open[report.Fingerprint]
					o.Deferred = reason
					st.Open[report.Fingerprint] = o
				})
				continue
			}
			if err != nil && deps.relay != nil && selfReportShouldRelay(err) {
				logger.Info("fleet report: no write access to report repo; relaying to hub", "fingerprint", report.Fingerprint, "error", err)
				var relayed selfReportRelayResponse
				relayed, err = deps.relay(ctx, selfReportRelayRequest{
					HiveID: deps.hiveSlug, Fingerprint: report.Fingerprint, Class: report.Class, Title: report.Title, Body: report.Body,
					Labels: report.Labels, Version: deps.version, Repo: deps.repo, Evidence: firstEvidence(report), HiveState: report.HiveState,
					InstanceID: report.InstanceID, Repos: report.Repos, Owners: report.Owners, OwnersHidden: report.OwnersHidden,
				})
				if err == nil {
					write = github.FleetReportResult{Number: relayed.Number, URL: relayed.URL, Created: !relayed.Joined, Joined: relayed.Joined, Commented: relayed.Joined}
				}
			}
		}
		if err != nil {
			logger.Warn("fleet report: upstream write failed", "fingerprint", report.Fingerprint, "class", report.Class, "error", err)
			continue
		}
		if !write.ClosedAt.IsZero() {
			logger.Info("fleet report: fingerprint closed upstream recently; not re-filing", "fingerprint", report.Fingerprint, "issue", write.Number, "closed_at", write.ClosedAt)
			dashSrv.UpdateFleetReportState(func(st *fleetreport.State) {
				o := st.Open[report.Fingerprint]
				o.Number, o.URL, o.ClosedAt = write.Number, write.URL, write.ClosedAt
				st.Open[report.Fingerprint] = o
			})
			continue
		}
		created := write.Created
		dashSrv.UpdateFleetReportState(func(st *fleetreport.State) {
			o := st.Open[report.Fingerprint]
			if isObservation && o.Recovered {
				o = fleetreport.OpenIssue{}
			}
			o.Number, o.URL = write.Number, write.URL
			o.OpenedByHive = o.OpenedByHive || created
			o.Joined = o.Joined || write.Joined
			o.BodyHash = fleetreport.StableBodyHash(report.Body)
			o.PostedAt = now.UTC()
			o.Class, o.Trigger, o.Deferred = report.Class, report.Trigger, ""
			if write.Commented || created {
				o.LastSeenCommentAt = now.UTC()
			}
			st.Open[report.Fingerprint] = o
			if created {
				fleetreport.RecordFiling(st, now)
			}
		})
		if isObservationReport(report) && deps.observations != nil && write.Number > 0 {
			if err := deps.observations.RecordIssue(report.Fingerprint, write.Number, write.URL, now); err != nil {
				logger.Warn("fleet report: observation issue ledger update failed", "fingerprint", report.Fingerprint, "error", err)
			}
		}
		state = dashSrv.FleetReportState()
		logger.Info("fleet report: upstream report recorded", "fingerprint", report.Fingerprint, "class", report.Class, "issue", write.Number, "created", created, "joined", write.Joined, "commented", write.Commented, "body_updated", write.BodyUpdated)
	}

	for _, recovery := range observationRecoveries {
		report := fleetreport.Report{
			Fingerprint: recovery.Fingerprint,
			Trigger:     selfReportObservationClass,
			Class:       selfReportObservationClass,
			Body:        recovery.Comment,
		}
		if _, err := relayObservationRecovery(ctx, deps, report); err != nil {
			logger.Warn("fleet report: observation recovery relay failed", "fingerprint", recovery.Fingerprint, "issue", recovery.IssueNumber, "error", err)
			continue
		}
		if deps.observations != nil {
			if err := deps.observations.MarkClosed(recovery.Fingerprint, now); err != nil {
				logger.Warn("fleet report: observation close ledger update failed", "fingerprint", recovery.Fingerprint, "error", err)
				continue
			}
		}
		dashSrv.MarkFleetReportRecovered(recovery.Fingerprint)
		logger.Info("fleet report: observation recovery recorded", "fingerprint", recovery.Fingerprint, "issue", recovery.IssueNumber)
	}
	for _, report := range res.Recoveries {
		if isObservationReport(report) {
			// Observation recovery is gated by the durable seven-day ledger above.
			continue
		}
		open, ok := state.Open[report.Fingerprint]
		if !ok {
			continue
		}
		if open.Number <= 0 {
			if deps.gh == nil {
				continue
			}
			issue, found, err := deps.gh.FleetReportIssue(ctx, report.Fingerprint)
			if err != nil {
				logger.Warn("fleet report: recovery lookup failed", "fingerprint", report.Fingerprint, "error", err)
				continue
			}
			if !found {
				dashSrv.ClearFleetReportOpen(report.Fingerprint)
				continue
			}
			open.Number, open.URL = issue.Number, issue.URL
		}
		if report.Trigger == fleetreport.TriggerRuntimeError {
			// Runtime errors: one "cleared" comment, never a close — a human
			// confirms the fix.
			if deps.gh == nil {
				continue
			}
			hive := deps.hiveSlug
			if hive == "" {
				hive = report.InstanceID
			}
			if err := deps.gh.PostFleetReportCleared(ctx, deps.repo, open.Number, fleetreport.ClearedComment(report.Fingerprint, hive, deps.version, now)); err != nil {
				logger.Warn("fleet report: cleared comment failed", "fingerprint", report.Fingerprint, "issue", open.Number, "error", err)
				continue
			}
			dashSrv.MarkFleetReportRecovered(report.Fingerprint)
			logger.Info("fleet report: cleared comment recorded", "fingerprint", report.Fingerprint, "class", report.Class, "issue", open.Number)
			continue
		}
		if deps.gh == nil {
			continue
		}
		if err := deps.gh.PostFleetReportRecovery(ctx, open.Number, report, open.OpenedByHive); err != nil {
			logger.Warn("fleet report: recovery write failed", "fingerprint", report.Fingerprint, "issue", open.Number, "error", err)
			continue
		}
		dashSrv.MarkFleetReportRecovered(report.Fingerprint)
		logger.Info("fleet report: recovery recorded", "fingerprint", report.Fingerprint, "issue", open.Number, "closed", open.OpenedByHive)
	}
	if deps.tracker != nil {
		// Conditions that cleared between cycles without ever being filed have
		// nothing to comment on; drain them so the tracker does not grow.
		_ = deps.tracker.TakeCleared()
	}
}

func isObservationReport(report fleetreport.Report) bool {
	return report.Trigger == fleetreport.TriggerObservation || report.Class == fleetreport.TriggerObservation
}

var (
	errSelfReportCreateDeferred     = errors.New("self-report: new filing deferred by rate limit")
	errSelfReportNoDirectWriter     = errors.New("self-report: no GitHub client")
	errSelfReportObservationNoRelay = errors.New("self-report: observation relay unavailable")
)

// withCreateGate returns opts; when allowNew is false the writer is asked to
// fail the create step with errSelfReportCreateDeferred so joins still work
// while new filings are capped.
func withCreateGate(opts github.FleetReportOptions, allowNew bool) github.FleetReportOptions {
	if !allowNew {
		opts.CreateGate = func() error { return errSelfReportCreateDeferred }
	}
	return opts
}

func firstEvidence(report fleetreport.Report) fleetreport.Evidence {
	if len(report.Evidence) > 0 {
		return report.Evidence[0]
	}
	return fleetreport.Evidence{}
}

func observationBeforeAfter(report fleetreport.Report) (string, string) {
	const seriesPrefix = "| series | `"
	start := strings.Index(report.Body, seriesPrefix)
	if start < 0 {
		return "", ""
	}
	seriesStart := start + len(seriesPrefix)
	seriesEnd := strings.IndexByte(report.Body[seriesStart:], '`')
	if seriesEnd < 0 {
		return "", ""
	}
	var series []float64
	if err := json.Unmarshal([]byte(report.Body[seriesStart:seriesStart+seriesEnd]), &series); err != nil || len(series) < 2 {
		return "", ""
	}
	return fmt.Sprintf("%g", series[0]), fmt.Sprintf("%g", series[len(series)-1])
}

func relayObservationRecovery(ctx context.Context, deps selfReportPublishDeps, report fleetreport.Report) (selfReportRelayResponse, error) {
	if deps.relay == nil {
		return selfReportRelayResponse{}, errSelfReportObservationNoRelay
	}
	return deps.relay(ctx, selfReportRelayRequest{
		Action:      "recover",
		HiveID:      deps.hiveSlug,
		Fingerprint: report.Fingerprint,
		Class:       selfReportObservationClass,
		Body:        report.Body,
		Repo:        config.DefaultFleetReportRepo,
	})
}

// selfReportShouldRelay: a 403/404 from the report repo means this hive's
// credential cannot write there (self-hosted PAT scoped to its own repos, or
// the App not installed on the report org). Anything else is a real error.
func selfReportShouldRelay(err error) bool {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return ghErr.Response.StatusCode == http.StatusForbidden || ghErr.Response.StatusCode == http.StatusNotFound
	}
	if errors.Is(err, errSelfReportNoDirectWriter) || errors.Is(err, github.ErrNoGitHubClient) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "403") || strings.Contains(msg, "404") || strings.Contains(msg, "resource not accessible")
}

// hubSelfReportRelay builds the relay that POSTs to the hub with the spoke's
// heartbeat bearer, the same credential /api/feedback/ingest accepts.
func hubSelfReportRelay(hubURL string, bearer func() string) selfReportRelay {
	return func(ctx context.Context, req selfReportRelayRequest) (selfReportRelayResponse, error) {
		key := bearer()
		if key == "" {
			return selfReportRelayResponse{}, errors.New("no hub credential configured")
		}
		data, err := json.Marshal(req)
		if err != nil {
			return selfReportRelayResponse{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, selfReportRelayTimeout)
		defer cancel()
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+selfReportHubRelayPath, bytes.NewReader(data))
		if err != nil {
			return selfReportRelayResponse{}, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(hreq)
		if err != nil {
			return selfReportRelayResponse{}, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return selfReportRelayResponse{}, fmt.Errorf("hub answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var out selfReportRelayResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return selfReportRelayResponse{}, err
		}
		if !out.OK && req.Action != "recover" {
			return selfReportRelayResponse{}, errors.New("hub declined the self-report")
		}
		return out, nil
	}
}
