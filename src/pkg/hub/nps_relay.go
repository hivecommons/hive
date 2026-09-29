package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// NPS relay pull, hub side (issue #9619).
//
// A standalone hive has no hub link, so an opted-in one posts its NPS
// responses to a hivecommons-operated relay instead (a Netlify function). The
// relay has no public read path: the only way to get entries out is this
// authenticated pull, keyed by a hub pull secret. Every cycle the hub:
//
//  1. GETs <relay>/pending (oldest first, at most npsRelayPullBatchSize),
//  2. validates each entry and merges the good ones into the NPS store tagged
//     source=relay, deduplicated by the relay's stable entry id,
//  3. POSTs the ids it has handled to <relay>/ack so the relay deletes them.
//
// Step 3 runs only after step 2 has persisted, so a crash or an ack failure
// can only cause a re-pull, and the dedupe in step 2 makes that re-pull a
// no-op. The poller is disabled unless both the relay URL and the pull secret
// are configured (hub.nps_relay_url / HIVE_NPS_RELAY_URL and
// hub.nps_relay_pull_secret / HIVE_NPS_RELAY_PULL_SECRET).

const (
	// npsRelayPullInterval is how often the hub pulls from the relay. The
	// admin view is a trend view, so minutes of lag are fine.
	npsRelayPullInterval = 15 * time.Minute
	// npsRelayPullStartupDelay lets the hub finish booting before the first
	// pull.
	npsRelayPullStartupDelay = time.Minute
	// npsRelayPullBatchSize is the most entries requested (and processed) per
	// pull request. The relay enforces the same ceiling.
	npsRelayPullBatchSize = 100
	// npsRelayMaxBatchesPerCycle bounds one cycle's work; anything left is
	// picked up next cycle.
	npsRelayMaxBatchesPerCycle = 10
	// npsRelayRequestTimeout bounds each relay round-trip.
	npsRelayRequestTimeout = 20 * time.Second
	// npsRelayMaxPullResponseBytes caps the bytes read from one pull
	// response: a full batch of maximal entries fits with room to spare.
	npsRelayMaxPullResponseBytes = 1 << 20
	// npsRelayMaxAckResponseBytes caps the bytes read from an ack response.
	npsRelayMaxAckResponseBytes = 1 << 12
	// npsRelayMaxInstallIDRunes caps the stored install label.
	npsRelayMaxInstallIDRunes = 64
	// npsRelayMaxClockSkew is how far in the future a relay timestamp may be
	// before the hub replaces it with its own receipt time.
	npsRelayMaxClockSkew = 5 * time.Minute

	// npsRelayPendingPath and npsRelayAckPath are appended to the relay base
	// URL for the two hub-only endpoints.
	npsRelayPendingPath = "/pending"
	npsRelayAckPath     = "/ack"
	// npsRelayLimitParam names the batch-size query parameter.
	npsRelayLimitParam = "limit"

	// npsSourceRelay tags records merged from the relay.
	npsSourceRelay = "relay"
)

// npsRelayIDPattern is the shape of a relay entry id. Anything else is not an
// id this hub can safely store or echo back in an ack.
var npsRelayIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// npsRelayEntry is one entry as the relay returns it.
type npsRelayEntry struct {
	ID               string `json:"id"`
	InstallID        string `json:"install_id"`
	HiveID           string `json:"hive_id"`
	Score            int    `json:"score"`
	Feedback         string `json:"feedback,omitempty"`
	DashboardVersion string `json:"dashboard_version,omitempty"`
	Timestamp        string `json:"timestamp"`
}

type npsRelayPendingResponse struct {
	Entries []npsRelayEntry `json:"entries"`
}

type npsRelayAckRequest struct {
	IDs []string `json:"ids"`
}

// npsRelayPullSettings resolves the relay URL and pull secret: env first,
// then the hub's config file (when one is set). Either empty disables the
// pull.
func (s *HubServer) npsRelayPullSettings() (relayURL, secret string) {
	var hubCfg config.HubConfig
	if strings.TrimSpace(s.configPath) != "" {
		cfg, err := config.LoadWithDashboardOverlayForHub(s.configPath)
		switch {
		case err == nil:
			hubCfg = cfg.Hub
		case !errors.Is(err, os.ErrNotExist) && s.logger != nil:
			s.logger.Warn("nps relay pull: could not load hub config; using env only", "path", s.configPath, "error", err)
		}
	}
	return hubCfg.EffectiveNPSRelayURL(), hubCfg.EffectiveNPSRelayPullSecret()
}

// StartNPSRelayPull runs the periodic relay pull until ctx is cancelled. It
// returns immediately when the relay is not configured.
func (s *HubServer) StartNPSRelayPull(ctx context.Context) {
	relayURL, secret := s.npsRelayPullSettings()
	if relayURL == "" || secret == "" {
		if s.logger != nil {
			s.logger.Info("nps relay pull disabled (no relay URL or pull secret configured)")
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("nps relay pull started", "relay", relayURL, "interval", npsRelayPullInterval.String())
	}
	client := npsRelayHTTPClient()
	timer := time.NewTimer(npsRelayPullStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			merged, err := s.pullNPSRelay(ctx, client, relayURL, secret)
			if s.logger != nil {
				if err != nil {
					s.logger.Warn("nps relay pull failed", "merged", merged, "error", err)
				} else if merged > 0 {
					s.logger.Info("nps relay pull merged responses", "merged", merged)
				}
			}
			timer.Reset(npsRelayPullInterval)
		}
	}
}

// npsRelayHTTPClient never follows redirects: the pull secret must reach only
// the configured relay.
func npsRelayHTTPClient() *http.Client {
	return &http.Client{
		Timeout: npsRelayRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// pullNPSRelay runs one pull cycle and returns how many new responses it
// merged. On any error it stops; whatever was not acked is re-pulled next
// cycle and deduplicated then.
func (s *HubServer) pullNPSRelay(ctx context.Context, client *http.Client, relayURL, secret string) (int, error) {
	merged := 0
	for batch := 0; batch < npsRelayMaxBatchesPerCycle; batch++ {
		entries, err := npsRelayFetch(ctx, client, relayURL, secret)
		if err != nil {
			return merged, err
		}
		if len(entries) == 0 {
			return merged, nil
		}
		recs, ackIDs, dropped := s.npsRelayRecords(entries, time.Now())
		if dropped > 0 && s.logger != nil {
			s.logger.Warn("nps relay pull: dropped invalid entries", "dropped", dropped)
		}
		added, err := hubNPS.mergeRelay(recs)
		if err != nil {
			// Not acked: the entries stay on the relay for the next cycle.
			return merged, fmt.Errorf("store: %w", err)
		}
		merged += added
		if err := npsRelayAck(ctx, client, relayURL, secret, ackIDs); err != nil {
			return merged, err
		}
		if len(entries) < npsRelayPullBatchSize {
			return merged, nil
		}
	}
	return merged, nil
}

// npsRelayFetch GETs one batch of pending entries.
func npsRelayFetch(ctx context.Context, client *http.Client, relayURL, secret string) ([]npsRelayEntry, error) {
	q := url.Values{}
	q.Set(npsRelayLimitParam, strconv.Itoa(npsRelayPullBatchSize))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL+npsRelayPendingPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build pull request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, npsRelayMaxPullResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read pull response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay answered %d to pull", resp.StatusCode)
	}
	if len(body) > npsRelayMaxPullResponseBytes {
		return nil, errors.New("relay pull response too large")
	}
	var out npsRelayPendingResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode pull response: %w", err)
	}
	if len(out.Entries) > npsRelayPullBatchSize {
		// The rest stay un-acked on the relay and come back next time.
		out.Entries = out.Entries[:npsRelayPullBatchSize]
	}
	return out.Entries, nil
}

// npsRelayAck tells the relay to delete ids. Nothing to ack is a no-op.
func npsRelayAck(ctx context.Context, client *http.Client, relayURL, secret string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	data, err := json.Marshal(npsRelayAckRequest{IDs: ids})
	if err != nil {
		return fmt.Errorf("marshal ack: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+npsRelayAckPath, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build ack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, npsRelayMaxAckResponseBytes))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("relay answered %d to ack", resp.StatusCode)
	}
	return nil
}

// npsRelayRecords validates relay entries with the same rules the direct
// ingest applies and converts the good ones to store records. It returns the
// records, the ids to ack (every entry with a well-formed id, whether it was
// kept or dropped, so a bad entry cannot clog the relay), and how many
// entries were dropped.
//
// An entry claiming the id of a hive in this hub's registry is dropped: a hub
// linked hive never uses the relay, so such an entry can only be an attempt
// to put words in a registered hive's mouth.
func (s *HubServer) npsRelayRecords(entries []npsRelayEntry, now time.Time) (recs []npsRecord, ackIDs []string, dropped int) {
	for _, e := range entries {
		if !npsRelayIDPattern.MatchString(e.ID) {
			dropped++
			continue
		}
		ackIDs = append(ackIDs, e.ID)
		hiveID := sanitizeHeartbeatField(e.HiveID)
		if hiveID == "" || hiveID != e.HiveID || !isValidName(hiveID) ||
			e.Score < npsScoreMin || e.Score > npsScoreMax ||
			s.npsHiveRegistered(hiveID) {
			dropped++
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || ts.After(now.Add(npsRelayMaxClockSkew)) {
			ts = now
		}
		recs = append(recs, npsRecord{
			HiveID:           hiveID,
			Score:            e.Score,
			Feedback:         npsSanitizeFeedback(e.Feedback),
			Timestamp:        ts.UTC().Format(time.RFC3339),
			DashboardVersion: truncateRunes(sanitizeHeartbeatField(e.DashboardVersion), npsMaxVersionRunes),
			Source:           npsSourceRelay,
			RelayID:          e.ID,
			InstallID:        truncateRunes(sanitizeHeartbeatField(e.InstallID), npsRelayMaxInstallIDRunes),
		})
	}
	return recs, ackIDs, dropped
}

// npsRecordTime parses a record timestamp; an unparseable one sorts first.
func npsRecordTime(r npsRecord) time.Time {
	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// mergeRelay adds relay records the store does not already hold (by RelayID),
// keeps the log oldest-first (relay entries can predate direct ones), applies
// the rolling caps and persists. It returns how many records were new. The
// per-hive ingest rate limit does not apply: the relay enforces its own
// per-IP limit, and dropping an already-accepted entry here would lose it.
// On a persist failure the in-memory log is rolled back.
func (st *npsStore) mergeRelay(recs []npsRecord) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(st.records))
	for _, r := range st.records {
		if r.RelayID != "" {
			seen[r.RelayID] = true
		}
	}
	prev := st.records
	next := make([]npsRecord, 0, len(prev)+len(recs))
	next = append(next, prev...)
	grown := map[string]bool{}
	added := 0
	for _, rec := range recs {
		if rec.RelayID == "" || seen[rec.RelayID] {
			continue
		}
		seen[rec.RelayID] = true
		next = append(next, rec)
		grown[rec.HiveID] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	sort.SliceStable(next, func(i, j int) bool {
		return npsRecordTime(next[i]).Before(npsRecordTime(next[j]))
	})
	hives := make([]string, 0, len(grown))
	for id := range grown {
		hives = append(hives, id)
	}
	sort.Strings(hives)
	for _, id := range hives {
		next = npsTrim(next, id)
	}
	st.records = next
	if err := st.saveLocked(); err != nil {
		st.records = prev
		return 0, err
	}
	return added, nil
}
