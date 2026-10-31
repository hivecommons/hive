package spoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hubactivity"
	spokesig "github.com/hivecommons/hive/pkg/spoke"
)

// Activity digest publisher (RFC hivecommons/hive#15861, #15963). The spoke
// builds the bounded hubactivity.Digest in process from its stage tracker
// (ActivitySource; no HTTP self-fetch, no GitHub call of its own) and
// publishes it two ways:
//
//   - On every heartbeat: the full digest, or only its etag and generated_at
//     when the hub already acknowledged that etag (no redundant snapshots).
//   - Out of band on a tracker publish: an activity-only update on the
//     heartbeat endpoint and bearer, at most one per
//     activityUpdateMinInterval. Publishes that arrive while one is waiting
//     or in flight merge into it; the rest are dropped (the next heartbeat
//     carries the digest anyway).

// ActivityUpdateHeader marks an activity-only update on POST /api/heartbeat.
const ActivityUpdateHeader = "X-Hive-Activity-Update"

// activityUpdateMinInterval is the owner-decided ceiling on out-of-band
// updates (#15861): no more than one per 15 seconds.
const activityUpdateMinInterval = 15 * time.Second

var (
	// activityItemsTimeout bounds reading the tracker so a slow build never
	// holds a heartbeat; the beat then goes out without an activity block
	// and the hub keeps the one it holds. A var so tests can shorten it.
	activityItemsTimeout = 5 * time.Second
	// activityUpdateBackoff pauses out-of-band updates after a hub answered
	// one as unsupported; heartbeats keep carrying the digest meanwhile.
	activityUpdateBackoff = 10 * time.Minute
)

// ActivityOptions are the resolved hub.activity settings.
type ActivityOptions struct {
	Enabled      bool
	MaxItems     int
	SharePrivate bool
}

// ActivitySource is the StartHeartbeat callback that feeds the activity
// digest. Options is read on every build so a config reload applies to the
// next beat. Items returns the tracked items with Private set for
// private-repository rows (ok=false: no tracker yet). Subscribe, optional,
// registers a non-blocking hook run on every tracker publish.
type ActivitySource struct {
	Options   func() ActivityOptions
	Items     func(ctx context.Context) (items []hubactivity.Item, ok bool)
	Subscribe func(notify func())
}

// ActivityUpdatePayload is the body of an activity-only update. hive_id sits
// inside activity_update, not at the top level, so a hub that predates the
// update path rejects the body ("hive_id required") instead of recording it
// as an empty heartbeat that would blank the hive's registry entry.
type ActivityUpdatePayload struct {
	Update *ActivityUpdate `json:"activity_update"`
}

// ActivityUpdate is the hive identity and the digest it publishes.
type ActivityUpdate struct {
	HiveID   string              `json:"hive_id"`
	Activity *hubactivity.Digest `json:"activity"`
}

// activityPostFunc sends one activity-only update. ok is true only for an
// accepted, verified response; status is the HTTP status (0 = no response).
type activityPostFunc func(ctx context.Context, d *hubactivity.Digest) (ack string, status int, ok bool)

type activityPublisher struct {
	src    ActivitySource
	logger *slog.Logger
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	post   activityPostFunc
	// kick holds at most one pending tracker publish: the coalescing buffer.
	kick chan struct{}

	// mu serializes build + post + acknowledge across the heartbeat and the
	// out-of-band update, so acknowledgements (and the signed responses'
	// sequence numbers) are applied in the order they were sent.
	mu         sync.Mutex
	acked      string
	lastUpdate time.Time
	backoff    time.Time
}

// activityPub is the running heartbeat loop's publisher (nil when no
// ActivitySource was registered). A hive runs at most one heartbeat loop.
var activityPub atomic.Pointer[activityPublisher]

func newActivityPublisher(src ActivitySource, hubURL string, logger *slog.Logger) *activityPublisher {
	if logger == nil {
		logger = slog.Default()
	}
	p := &activityPublisher{
		src:    src,
		logger: logger,
		now:    time.Now,
		after:  time.After,
		kick:   make(chan struct{}, 1),
	}
	p.post = func(ctx context.Context, d *hubactivity.Digest) (string, int, bool) {
		return postActivityUpdate(ctx, hubURL, heartbeatHiveID(), d, logger)
	}
	return p
}

// build returns the current digest, redacted for hub.heartbeat_omit.
// ok is false when publishing is off or the tracker has nothing to give.
func (p *activityPublisher) build(ctx context.Context) (hubactivity.Digest, bool) {
	if p.src.Options == nil || p.src.Items == nil {
		return hubactivity.Digest{}, false
	}
	opts := p.src.Options()
	if !opts.Enabled {
		return hubactivity.Digest{}, false
	}
	items, ok := activityItemsWithTimeout(ctx, p.src.Items, activityItemsTimeout, p.logger)
	if !ok {
		return hubactivity.Digest{}, false
	}
	d := hubactivity.Build(items, hubactivity.BuildOptions{MaxItems: opts.MaxItems, SharePrivate: opts.SharePrivate, Now: p.now()})
	return redactActivity(d, omitSet()), true
}

// heartbeatDigestLocked is the activity block for a heartbeat: nil when
// there is nothing to publish, an unchanged marker when the hub already
// holds this etag, else the full digest. Callers hold p.mu.
func (p *activityPublisher) heartbeatDigestLocked(ctx context.Context) *hubactivity.Digest {
	d, ok := p.build(ctx)
	if !ok {
		return nil
	}
	if d.ETag != "" && d.ETag == p.acked {
		return &hubactivity.Digest{ETag: d.ETag, GeneratedAt: d.GeneratedAt}
	}
	return &d
}

// acknowledgeLocked records the etag the hub reported holding. Callers hold
// p.mu and call it only for an accepted, verified response.
func (p *activityPublisher) acknowledgeLocked(etag string) {
	p.acked = etag
}

// notify signals a tracker publish without blocking: a publish arriving
// while one is already pending merges into it.
func (p *activityPublisher) notify() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// run sends coalesced out-of-band updates until ctx ends.
func (p *activityPublisher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		}
		p.mu.Lock()
		wait := p.lastUpdate.Add(activityUpdateMinInterval).Sub(p.now())
		p.mu.Unlock()
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.after(wait):
			}
		}
		// A publish signalled while waiting is covered by this update.
		select {
		case <-p.kick:
		default:
		}
		p.update(ctx)
	}
}

// update sends one activity-only update when the digest differs from what
// the hub acknowledged. It reports whether an update was accepted.
func (p *activityPublisher) update(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if now.Before(p.backoff) {
		return false
	}
	d, ok := p.build(ctx)
	if !ok || d.ETag == p.acked {
		return false
	}
	p.lastUpdate = now
	ack, status, sent := p.post(ctx, &d)
	if !sent {
		if activityUpdateUnsupported(status) {
			p.backoff = now.Add(activityUpdateBackoff)
		}
		return false
	}
	p.acknowledgeLocked(ack)
	return true
}

// activityUpdateUnsupported reports a status meaning the hub does not take
// activity-only updates (a hub before #15963 answers 400 "hive_id
// required").
func activityUpdateUnsupported(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
}

// redactActivity applies hub.heartbeat_omit to a digest and re-stamps its
// etag: withheld repos drop every item (totals stay), withheld task titles
// blank the titles.
func redactActivity(d hubactivity.Digest, omit map[string]bool) hubactivity.Digest {
	switch {
	case omit[config.HeartbeatOmitRepos]:
		d.Items = nil
		d.Truncated = 0
	case omit[config.HeartbeatOmitTaskTitles]:
		items := slices.Clone(d.Items)
		for i := range items {
			items[i].Title = ""
		}
		d.Items = items
	default:
		return d
	}
	d.ETag = hubactivity.ContentETag(d)
	return d
}

// activityItemsWithTimeout runs items with an upper bound, recovering a
// panic, so the tracker can never stall or crash the heartbeat loop.
func activityItemsWithTimeout(ctx context.Context, items func(context.Context) ([]hubactivity.Item, bool), timeout time.Duration, logger *slog.Logger) ([]hubactivity.Item, bool) {
	type result struct {
		items []hubactivity.Item
		ok    bool
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("hub activity digest: tracker read panicked; sending no activity block", "recover", r)
				done <- result{}
			}
		}()
		its, ok := items(cctx)
		done <- result{items: its, ok: ok}
	}()
	select {
	case r := <-done:
		return r.items, r.ok
	case <-cctx.Done():
		logger.Warn("hub activity digest: tracker read timed out; sending no activity block", "timeout", timeout.String())
		return nil, false
	}
}

// heartbeatHiveID is the hive ID from the published heartbeat identity.
func heartbeatHiveID() string {
	heartbeatIdentity.Lock()
	defer heartbeatIdentity.Unlock()
	if heartbeatIdentity.payload == nil {
		return ""
	}
	return heartbeatIdentity.payload.HiveID
}

// postActivityUpdate sends an activity-only update with the heartbeat
// bearer and verifies the hub's signed acknowledgement the same way as a
// heartbeat response.
func postActivityUpdate(ctx context.Context, hubURL, hiveID string, d *hubactivity.Digest, logger *slog.Logger) (string, int, bool) {
	if hubURL == "" || hiveID == "" || d == nil {
		return "", 0, false
	}
	body, err := json.Marshal(ActivityUpdatePayload{Update: &ActivityUpdate{HiveID: hiveID, Activity: d}})
	if err != nil {
		return "", 0, false
	}
	reqCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+"/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ActivityUpdateHeader, "1")
	if secret := SpokeHeartbeatKey(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("hub activity update unreachable", "error", err)
		return "", 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		logger.Debug("hub activity update rejected", "status", resp.StatusCode)
		return "", resp.StatusCode, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes))
	if err != nil {
		return "", resp.StatusCode, false
	}
	result := spokeHeartbeatVerifier(logger).Verify(SpokeSSOPublicKeys(), hiveID, raw, resp.Header.Get(spokesig.SigHeader), time.Now())
	if !result.Accepted {
		logger.Warn("hub activity update response REJECTED by signature verification", "reason", result.Reason, "hive_id", hiveID)
		return "", resp.StatusCode, false
	}
	var hbResp HeartbeatResponse
	if err := json.Unmarshal(raw, &hbResp); err != nil {
		return "", resp.StatusCode, false
	}
	return hbResp.ActivityETag, resp.StatusCode, true
}
