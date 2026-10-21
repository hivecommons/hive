package github

// Soak-evidence triage for ops-tracking issues (hivecommons/hive#13311).
//
// A maintainer's post-hoc soak checklist (hivecommons/hive#13267: "24h after
// promotion: hub dashboard shows hosted stable hives healthy on the new
// digest…") carries `<!-- hive:ops-tracking -->` and a
// `<!-- hive:recheck-after <RFC3339> -->` deadline and wears `blocked`. Once
// the deadline passes, ReconcileBlockedIssues hands such an issue on the
// hive's own project to runRecheckSoak instead of lifting `blocked`: the hub's
// hive list (or, with no hub, the spoke's own /api/version) is probed for every
// hive on the stable channel, ONE evidence comment is posted, the checklist
// lines the evidence proves are ticked, and `<!-- hive:recheck-done -->` is
// appended to the body so the sweep does not repeat. The issue stays open
// and blocked until a maintainer closes it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	// recheckEvidenceMarkerPrefix opens the soak-evidence comment marker,
	// `<!-- hive:recheck-evidence at=<RFC3339> -->`; any comment carrying it
	// means the evidence was already posted.
	recheckEvidenceMarkerPrefix = "<!-- hive:recheck-evidence"
	// RecheckDoneMarker is appended to the issue body once the soak triage
	// has run, so later sweeps leave the issue to its maintainer.
	RecheckDoneMarker = "<!-- hive:recheck-done -->"

	// soakHeartbeatMaxAge is the oldest heartbeat a healthy hive may report.
	soakHeartbeatMaxAge = 15 * time.Minute
	// soakRestartLoopThreshold is the restart count read as a restart loop.
	soakRestartLoopThreshold = 3
	// soakStableChannel is the release channel the soak checklist covers.
	soakStableChannel = "stable"
	// soakProbeBodyLimit caps the probe response read.
	soakProbeBodyLimit = 4 << 20
)

// recheckSoakRepo is the hive's own project: the only repo whose
// ops-tracking issues get soak triage. Tests override it.
var recheckSoakRepo = fleetReportRepo

// recheckSoakProbe reads the fleet for the soak triage; nil leaves
// ops-tracking issues alone. SetRecheckSoakProbe installs it.
var recheckSoakProbe SoakProbe

// SoakHive is one hive as the soak probe sees it.
type SoakHive struct {
	Channel string
	// SHA is the running commit, full or short; Digest the image digest.
	SHA    string
	Digest string
	// LastHeartbeat is zero when the source does not report one.
	LastHeartbeat time.Time
	Restarts      int
	RestartLoop   bool
}

// SoakProbe reports the hives to judge and the source it read them from.
type SoakProbe func(ctx context.Context) (source string, hives []SoakHive, err error)

// SetRecheckSoakProbe installs the probe the blocked sweep uses for
// ops-tracking soak triage; nil disables the triage.
func SetRecheckSoakProbe(p SoakProbe) { recheckSoakProbe = p }

// isRecheckSoakRepo reports whether repo is the hive's own project.
func isRecheckSoakRepo(repo string) bool {
	return strings.EqualFold(strings.TrimSpace(repo), recheckSoakRepo)
}

// NewHTTPSoakProbe probes hubURL's /api/hub/hives when hubURL is set, else
// the spoke's own /api/version at spokeURL. hc nil uses a 15s-timeout client.
func NewHTTPSoakProbe(hubURL, spokeURL string, hc *http.Client) SoakProbe {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	spokeURL = strings.TrimRight(strings.TrimSpace(spokeURL), "/")
	return func(ctx context.Context) (string, []SoakHive, error) {
		if hubURL != "" {
			raw, err := soakProbeGet(ctx, hc, hubURL+"/api/hub/hives")
			if err != nil {
				return "hub /api/hub/hives", nil, err
			}
			hives, err := decodeHubSoakHives(raw)
			return "hub /api/hub/hives", hives, err
		}
		if spokeURL == "" {
			return "", nil, fmt.Errorf("soak probe: no hub or spoke URL configured")
		}
		raw, err := soakProbeGet(ctx, hc, spokeURL+"/api/version")
		if err != nil {
			return "spoke /api/version", nil, err
		}
		hive, err := decodeSpokeSoakHive(raw)
		if err != nil {
			return "spoke /api/version", nil, err
		}
		return "spoke /api/version", []SoakHive{hive}, nil
	}
}

func soakProbeGet(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, soakProbeBodyLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("soak probe: GET %s: HTTP %d", url, resp.StatusCode)
	}
	return raw, nil
}

// wireSoakHive tolerates the field spellings the hub and spoke use.
type wireSoakHive struct {
	Channel       string `json:"channel"`
	SHA           string `json:"sha"`
	GitSHA        string `json:"git_sha"`
	Hash          string `json:"hash"`
	Short         string `json:"short"`
	Digest        string `json:"digest"`
	ImageDigest   string `json:"image_digest"`
	ImageRef      string `json:"imageRef"`
	Image         string `json:"image"`
	LastHeartbeat string `json:"last_heartbeat"`
	LastSeen      string `json:"last_seen"`
	HeartbeatAt   string `json:"lastHeartbeat"`
	Restarts      int    `json:"restarts"`
	RestartCount  int    `json:"restart_count"`
	RestartLoop   bool   `json:"restart_loop"`
}

var soakDigestRE = regexp.MustCompile(`(?i)sha256:[0-9a-f]{12,64}`)

func (w wireSoakHive) hive() SoakHive {
	h := SoakHive{
		Channel:     strings.TrimSpace(w.Channel),
		SHA:         firstNonEmpty(w.SHA, w.GitSHA, w.Hash, w.Short),
		Digest:      firstNonEmpty(w.Digest, w.ImageDigest),
		Restarts:    max(w.Restarts, w.RestartCount),
		RestartLoop: w.RestartLoop,
	}
	if h.Digest == "" {
		h.Digest = soakDigestRE.FindString(firstNonEmpty(w.ImageRef, w.Image))
	}
	for _, ts := range []string{w.LastHeartbeat, w.HeartbeatAt, w.LastSeen} {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts)); err == nil {
			h.LastHeartbeat = t
			break
		}
	}
	return h
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// decodeHubSoakHives reads a bare array or {"hives": [...]}.
func decodeHubSoakHives(raw []byte) ([]SoakHive, error) {
	var list []wireSoakHive
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapped struct {
			Hives []wireSoakHive `json:"hives"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, fmt.Errorf("soak probe: decode hub hives: %w", err)
		}
		list = wrapped.Hives
	}
	out := make([]SoakHive, 0, len(list))
	for _, w := range list {
		out = append(out, w.hive())
	}
	return out, nil
}

func decodeSpokeSoakHive(raw []byte) (SoakHive, error) {
	var w wireSoakHive
	if err := json.Unmarshal(raw, &w); err != nil {
		return SoakHive{}, fmt.Errorf("soak probe: decode spoke version: %w", err)
	}
	return w.hive(), nil
}

// soakCandidate is the promoted build an ops-tracking issue names.
type soakCandidate struct {
	Digest   string
	ShortSHA string
}

var (
	soakCandidateDigestRE = regexp.MustCompile("(?i)\\bcandidate\\s+`?(sha256:[0-9a-f]{12,64})")
	soakShortSHARE        = regexp.MustCompile("\\bv\\d+\\s+`?([0-9a-f]{7,40})`?(?:[^0-9a-zA-Z]|$)")
)

// parseSoakCandidate extracts the candidate digest (the one introduced by
// "candidate", else the first sha256 in the body) and the `v5 <short-sha>`
// token, as hivecommons/hive#13267 writes them.
func parseSoakCandidate(body string) soakCandidate {
	var c soakCandidate
	if m := soakCandidateDigestRE.FindStringSubmatch(body); m != nil {
		c.Digest = strings.ToLower(m[1])
	} else {
		c.Digest = strings.ToLower(soakDigestRE.FindString(body))
	}
	if m := soakShortSHARE.FindStringSubmatch(body); m != nil {
		c.ShortSHA = strings.ToLower(m[1])
	}
	return c
}

func (c soakCandidate) known() bool { return c.Digest != "" || c.ShortSHA != "" }

func (c soakCandidate) String() string {
	var parts []string
	if c.Digest != "" {
		parts = append(parts, "`"+c.Digest+"`")
	}
	if c.ShortSHA != "" {
		parts = append(parts, "`"+c.ShortSHA+"`")
	}
	if len(parts) == 0 {
		return "not named in the issue body"
	}
	return strings.Join(parts, " / ")
}

// matches reports whether h runs the candidate: a digest or commit prefix
// match in either direction (the body may abbreviate, the hive may too).
func (c soakCandidate) matches(h SoakHive) bool {
	if c.Digest != "" && h.Digest != "" && hexPrefixMatch(c.Digest, h.Digest) {
		return true
	}
	return c.ShortSHA != "" && h.SHA != "" && hexPrefixMatch(c.ShortSHA, h.SHA)
}

func hexPrefixMatch(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// soakEvidence is the verdict one probe supports.
type soakEvidence struct {
	Source    string
	Candidate soakCandidate
	Stable    int
	Healthy   int
	OnCand    int
	// FleetHealthy: at least one stable hive, every one with a fresh
	// heartbeat, no restart loop, and running the candidate.
	FleetHealthy bool
	// VersionMatched: at least one stable hive runs the candidate.
	VersionMatched bool
}

func evaluateSoak(source string, cand soakCandidate, hives []SoakHive, now time.Time) soakEvidence {
	ev := soakEvidence{Source: source, Candidate: cand}
	for _, h := range hives {
		if !strings.EqualFold(h.Channel, soakStableChannel) {
			continue
		}
		ev.Stable++
		if soakHiveHealthy(h, now) {
			ev.Healthy++
		}
		if cand.known() && cand.matches(h) {
			ev.OnCand++
		}
	}
	ev.VersionMatched = ev.OnCand > 0
	ev.FleetHealthy = ev.Stable > 0 && ev.Healthy == ev.Stable && ev.OnCand == ev.Stable
	return ev
}

func soakHiveHealthy(h SoakHive, now time.Time) bool {
	if h.LastHeartbeat.IsZero() || now.Sub(h.LastHeartbeat) > soakHeartbeatMaxAge {
		return false
	}
	return !h.RestartLoop && h.Restarts < soakRestartLoopThreshold
}

var (
	soakChecklistOpenRE = regexp.MustCompile(`^(\s*[-*+]\s+)\[ \](\s.*)$`)
	soakHealthLineRE    = regexp.MustCompile(`(?i)\bhealthy\b|heartbeat|restart`)
	soakVersionLineRE   = regexp.MustCompile(`(?i)/api/version|\bdigest\b|\bmatches\b`)
)

// tickSoakChecklist ticks the open checklist lines ev proves: a fleet-health
// line (healthy / heartbeat / restart loops) when the stable fleet is healthy
// on the candidate, a version line (/api/version, digest, "matches") when a
// stable hive runs the candidate. Every other line is left as it was.
func tickSoakChecklist(body string, ev soakEvidence) (string, int) {
	lines := strings.Split(body, "\n")
	ticked := 0
	for i, line := range lines {
		m := soakChecklistOpenRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		prove := false
		switch {
		case soakHealthLineRE.MatchString(m[2]):
			prove = ev.FleetHealthy
		case soakVersionLineRE.MatchString(m[2]) || (ev.Candidate.ShortSHA != "" && strings.Contains(strings.ToLower(m[2]), ev.Candidate.ShortSHA)):
			prove = ev.VersionMatched
		}
		if prove {
			lines[i] = m[1] + "[x]" + strings.TrimPrefix(line, m[1]+"[ ]")
			ticked++
		}
	}
	return strings.Join(lines, "\n"), ticked
}

// recheckEvidenceComment renders the one evidence comment. It reports
// counts, not hive identities: the issue is public.
func recheckEvidenceComment(deadline time.Time, ev soakEvidence, ticked int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at=%s -->\n", recheckEvidenceMarkerPrefix, now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**Soak re-check:** deadline %s passed.\n\n", deadline.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Source: %s\n", ev.Source)
	fmt.Fprintf(&b, "- Candidate: %s\n", ev.Candidate)
	fmt.Fprintf(&b, "- Hives on `%s`: %d\n", soakStableChannel, ev.Stable)
	fmt.Fprintf(&b, "- Healthy (heartbeat within %s, no restart loop): %d/%d\n", soakHeartbeatMaxAge, ev.Healthy, ev.Stable)
	fmt.Fprintf(&b, "- Running the candidate: %d/%d\n", ev.OnCand, ev.Stable)
	fmt.Fprintf(&b, "\nTicked %d checklist item(s) the evidence proves; the rest are left for a maintainer. The issue stays open and `blocked` until a maintainer closes it.", ticked)
	return b.String()
}

// runRecheckSoak posts soak evidence on an ops-tracking issue whose
// recheck-after deadline has passed, ticks the checklist lines it proves and
// stamps RecheckDoneMarker into the body. `blocked` stays on. A probe or
// GitHub failure leaves the body unstamped so the next cycle retries; an
// evidence comment already on the issue is not posted again.
func runRecheckSoak(ctx context.Context, client *Client, t blockedIssueTarget, evidenceNoted bool, lookupErr error, now time.Time, res *BlockedIssueResult, logger *slog.Logger) {
	if t.recheckDone {
		return
	}
	deadline := t.recheckAfter.UTC().Format(time.RFC3339)
	if lookupErr != nil {
		res.Errors++
		logger.Warn("blocked sweep: comment lookup failed; soak triage skipped", "repo", t.repo, "issue", t.number, "recheck_after", deadline, "error", lookupErr)
		return
	}
	probe := recheckSoakProbe
	if probe == nil {
		logger.Info("blocked sweep: deadline passed on an ops-tracking issue; no soak probe configured, left blocked", "repo", t.repo, "issue", t.number, "recheck_after", deadline)
		return
	}
	owner, name := client.splitRepo(t.repo)
	issue, _, err := client.client.Issues.Get(ctx, owner, name, t.number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: soak triage issue read failed", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	body := issue.GetBody()
	if blockedRecheckDone(body) {
		return
	}
	source, hives, err := probe(ctx)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: soak probe failed; will retry", "repo", t.repo, "issue", t.number, "source", source, "error", err)
		return
	}
	ev := evaluateSoak(source, parseSoakCandidate(body), hives, now)
	newBody, ticked := tickSoakChecklist(body, ev)
	if !evidenceNoted {
		if err := client.CreateIssueCommentBudgeted(ctx, t.repo, t.number, recheckEvidenceComment(t.recheckAfter, ev, ticked, now)); err != nil {
			res.Errors++
			logger.Warn("blocked sweep: soak evidence not posted", "repo", t.repo, "issue", t.number, "error", err)
			return
		}
	}
	newBody = strings.TrimRight(newBody, "\n") + "\n\n" + RecheckDoneMarker + "\n"
	if _, _, err := client.client.Issues.Edit(ctx, owner, name, t.number, &gh.IssueRequest{Body: gh.Ptr(newBody)}); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: soak checklist not updated", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	res.SoakTriaged++
	logger.Info("blocked sweep: soak evidence posted; left blocked", "repo", t.repo, "issue", t.number, "recheck_after", deadline, "stable", ev.Stable, "healthy", ev.Healthy, "on_candidate", ev.OnCand, "ticked", ticked)
}
