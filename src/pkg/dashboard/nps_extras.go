package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// NPS prompt options beyond the core submit path (issue #9610):
//
//   - operator timing overrides (hub.nps_timing), handed to the dashboard in
//     the status response;
//   - optional GA4 funnel events (HIVE_NPS_GA4_MEASUREMENT_ID, off by
//     default): the ID is handed to the dashboard, and the CSP is widened for
//     Google's tag hosts only while it is set;
//   - an opt-in PUBLIC issue for detractors (hub.nps_detractor_issues, off by
//     default), filed by this hive's App through the existing
//     github.Client.CreateIssue path, only with explicit consent, a minimum
//     length, and the mention sanitizer applied.
//
// Nothing here touches the spoke-to-hub forwarding in nps.go.

const (
	// npsDetractorScore is the only score allowed to open a public issue.
	npsDetractorScore = 1
	// npsIssueMinFeedbackRunes is the minimum free-text length (after
	// sanitizing) for a public issue, as in kubestellar/console. A one-word
	// complaint is not actionable and would only add noise to a public tracker.
	npsIssueMinFeedbackRunes = 20
	// npsIssueMaxPerUserPerWindow / npsIssueMaxPerHivePerWindow cap public
	// issues per npsRateWindow. Tighter than the NPS caps: every one is a
	// public artifact in someone's repository.
	npsIssueMaxPerUserPerWindow = 1
	npsIssueMaxPerHivePerWindow = 3
	// npsIssueTimeout bounds the forge round-trips (dedupe scan + create).
	npsIssueTimeout = 30 * time.Second
	// npsIssueTitlePrefix starts every detractor issue title.
	npsIssueTitlePrefix = "Hive dashboard feedback: "
	// npsIssueTitleSnippetRunes caps how much of the feedback goes into the
	// title (the whole text is in the body).
	npsIssueTitleSnippetRunes = 60
	// npsIssueTitleEllipsis marks a truncated title snippet.
	npsIssueTitleEllipsis = "..."

	// npsGA4ScriptSources / npsGA4ConnectSources are the CSP sources gtag.js
	// needs, per Google's published CSP guidance for GA4. Added to the policy
	// only while a GA4 measurement ID is configured.
	npsGA4ScriptSources  = "https://*.googletagmanager.com"
	npsGA4ConnectSources = "https://*.google-analytics.com https://*.analytics.google.com https://*.googletagmanager.com"

	msPerSecond = 1000
)

// npsTimingPayload is the effective eligibility timing, in the units the
// dashboard's npsTiming() uses.
type npsTimingPayload struct {
	MinSessions                  int   `json:"min_sessions"`
	SecondSessionMinEngagementMS int64 `json:"second_session_min_engagement_ms"`
	ReturningMinEngagementMS     int64 `json:"returning_min_engagement_ms"`
	RepromptDays                 int   `json:"reprompt_days"`
	DismissRetryDays             int   `json:"dismiss_retry_days"`
	MaxDismissals                int   `json:"max_dismissals"`
}

// npsDetractorIssueStatus tells the dashboard to offer the consent checkbox,
// and names the repository so the user knows where the issue would appear.
type npsDetractorIssueStatus struct {
	Enabled          bool   `json:"enabled"`
	Score            int    `json:"score"`
	MinFeedbackChars int    `json:"min_feedback_chars"`
	Repo             string `json:"repo"`
}

func npsTimingFromConfig(t config.NPSTimingConfig) *npsTimingPayload {
	return &npsTimingPayload{
		MinSessions:                  t.MinSessions,
		SecondSessionMinEngagementMS: int64(t.SecondSessionEngagementSeconds) * msPerSecond,
		ReturningMinEngagementMS:     int64(t.ReturningEngagementSeconds) * msPerSecond,
		RepromptDays:                 t.RepromptDays,
		DismissRetryDays:             t.DismissRetryDays,
		MaxDismissals:                t.MaxDismissals,
	}
}

// npsStatusExtras fills the optional parts of the status response. Called
// only for a viewer who can submit, so an anonymous or read-only viewer
// learns nothing about the hive's analytics or issue settings.
func (s *Server) npsStatusExtras(resp *npsStatusResponse) {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	resp.Timing = npsTimingFromConfig(s.deps.Config.Hub.EffectiveNPSTiming())
	resp.GA4MeasurementID = config.NPSGA4MeasurementID()
	if repo := s.npsDetractorIssueRepo(); repo != "" {
		resp.DetractorIssue = &npsDetractorIssueStatus{
			Enabled:          true,
			Score:            npsDetractorScore,
			MinFeedbackChars: npsIssueMinFeedbackRunes,
			Repo:             repo,
		}
	}
}

// npsGA4CSPSources returns the extra script-src and connect-src sources (each
// with a leading space) gtag.js needs, or two empty strings when GA4 is not
// configured, which leaves the policy byte-for-byte unchanged.
func npsGA4CSPSources() (script, connect string) {
	if config.NPSGA4MeasurementID() == "" {
		return "", ""
	}
	return " " + npsGA4ScriptSources, " " + npsGA4ConnectSources
}

// npsIssueFiler is the slice of *github.Client the detractor-issue path uses:
// the App's existing issue-creation path (dedupe, label and canary guards,
// log scrubbing included). A seam so tests need no forge.
type npsIssueFiler interface {
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (ghpkg.CreateIssueResult, error)
}

// npsIssueFilerFor returns the filer to use, or nil when this hive has no
// forge client.
func (s *Server) npsIssueFilerFor() npsIssueFiler {
	if s.npsIssueFiler != nil {
		return s.npsIssueFiler
	}
	if s.deps == nil || s.deps.GHClient == nil {
		return nil
	}
	return s.deps.GHClient
}

// npsDetractorIssueRepo returns the repository detractor issues go to, or ""
// when the feature is unavailable: NPS off, the opt-in off, or no forge client.
func (s *Server) npsDetractorIssueRepo() string {
	if !s.npsEnabled() || s.npsIssueFilerFor() == nil {
		return ""
	}
	return s.deps.Config.Hub.NPSDetractorIssueRepo()
}

// npsIssueRequest is the browser's body for POST /api/feedback/nps/issue.
type npsIssueRequest struct {
	Score    *int   `json:"score"`
	Feedback string `json:"feedback"`
	// Consent must be exactly true: the user ticked "open a public issue".
	Consent bool `json:"consent"`
}

// npsIssueTitle builds a single-line title from the feedback, with mentions
// neutralized.
func npsIssueTitle(feedback string) string {
	line := strings.Join(strings.Fields(feedback), " ")
	if runes := []rune(line); len(runes) > npsIssueTitleSnippetRunes {
		line = strings.TrimSpace(string(runes[:npsIssueTitleSnippetRunes])) + npsIssueTitleEllipsis
	}
	return advisory.NeutralizeMentions(npsIssueTitlePrefix + line)
}

// npsIssueBody renders the public issue body: the feedback as a quote and a
// footer saying how it got there. It carries no user identity and no hive ID,
// and every @mention is neutralized so filing it notifies nobody.
func npsIssueBody(feedback, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A hive dashboard user rated the hive **%d of %d (Not great)** and asked for their feedback to be filed as a public issue.\n\n", npsDetractorScore, npsScoreMax)
	for _, line := range strings.Split(feedback, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	b.WriteString("Filed by this hive's NPS feedback prompt with the user's explicit consent. Who sent it is not recorded.")
	if v := strings.TrimSpace(version); v != "" {
		fmt.Fprintf(&b, " Dashboard version: `%s`.", v)
	}
	b.WriteString("\n")
	return advisory.NeutralizeMentions(b.String())
}

// handleNPSIssue answers POST /api/feedback/nps/issue: a detractor, having
// ticked the consent box, turns their feedback into a public issue filed by
// this hive's App. Off unless hub.nps_detractor_issues is enabled.
func (s *Server) handleNPSIssue(w http.ResponseWriter, r *http.Request) {
	user, role := s.npsActor(r)
	if user == "" {
		jsonError(w, "you must be signed in to open a feedback issue", http.StatusForbidden)
		return
	}
	if role == "read" {
		jsonError(w, "read-only users cannot open a feedback issue", http.StatusForbidden)
		return
	}
	repo := s.npsDetractorIssueRepo()
	if repo == "" {
		jsonError(w, "public feedback issues are not enabled on this hive", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, npsMaxBodyBytes+1))
	if err != nil {
		jsonError(w, "could not read request", http.StatusBadRequest)
		return
	}
	if len(body) > npsMaxBodyBytes {
		jsonError(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req npsIssueRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Score == nil || *req.Score != npsDetractorScore {
		jsonError(w, fmt.Sprintf("only a score of %d can open a public issue", npsDetractorScore), http.StatusBadRequest)
		return
	}
	if !req.Consent {
		jsonError(w, "consent is required to open a public issue", http.StatusBadRequest)
		return
	}
	feedback := npsSanitizeFeedback(req.Feedback)
	if len([]rune(feedback)) < npsIssueMinFeedbackRunes {
		jsonError(w, fmt.Sprintf("please write at least %d characters to open a public issue", npsIssueMinFeedbackRunes), http.StatusBadRequest)
		return
	}

	release, err := s.npsIssueLimiter.reserveWith(strings.ToLower(user), time.Now(), npsIssueMaxPerUserPerWindow, npsIssueMaxPerHivePerWindow)
	if err != nil {
		msg := "this hive has opened its share of public feedback issues for today - please try again tomorrow"
		if err == errNPSUserLimited {
			msg = "you already opened a public feedback issue recently - thank you"
		}
		jsonError(w, msg, http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), npsIssueTimeout)
	defer cancel()
	res, err := s.npsIssueFilerFor().CreateIssue(ctx, repo, npsIssueTitle(feedback), npsIssueBody(feedback, versionShort), nil)
	if err != nil {
		release()
		if s.logger != nil {
			s.logger.Warn("nps: detractor issue create failed", "repo", repo, "error", err)
		}
		jsonError(w, "could not open the issue - please try again later", http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "number": res.Number, "url": res.URL})
}
