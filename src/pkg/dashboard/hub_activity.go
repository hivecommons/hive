package dashboard

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hubactivity"
)

// Hub activity digest source (RFC hivecommons/hive#15861, #15963). The
// spoke's heartbeat builds its bounded activity digest from the stage
// tracker this server already memoises: no HTTP self-fetch and no GitHub
// call of its own.

// stageTrackerPublishHooks maps a *Server to the hook run on every tracker
// invalidation (status publish or webhook re-read). Kept beside the Server
// rather than in it so the hook stays a pure addition.
var stageTrackerPublishHooks sync.Map

// OnStageTrackerPublish registers fn to run on every tracker publish,
// replacing any earlier hook; nil removes it. fn runs on the publish and
// webhook paths and must not block.
func (s *Server) OnStageTrackerPublish(fn func()) {
	if s == nil {
		return
	}
	if fn == nil {
		stageTrackerPublishHooks.Delete(s)
		return
	}
	stageTrackerPublishHooks.Store(s, fn)
}

func (s *Server) notifyStageTrackerPublish() {
	if v, ok := stageTrackerPublishHooks.Load(s); ok {
		v.(func())()
	}
}

// HubActivityItems returns every tracked item as a hub activity row, with
// Private set unless the repo is known public (hub.is_public project repos
// or HIVE_PUBLIC_REPOS, the same set the Battle Log trusts). ok is false
// while the spoke has no status or the tracker cannot be built. Reading
// does not count as a tracker client, so it adds no SSE nudges.
func (s *Server) HubActivityItems(ctx context.Context) ([]hubactivity.Item, bool) {
	if s == nil {
		return nil, false
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return nil, false
	}
	body, _, _, _ := s.stageTrackerPayloadFrom(ctx, status, time.Now().UTC(), false)
	if body == nil {
		return nil, false
	}
	var tracker struct {
		Items []StageTrackerItem `json:"items"`
	}
	if err := json.Unmarshal(body, &tracker); err != nil {
		return nil, false
	}
	return hubActivityItems(tracker.Items, s.publicRepos()), true
}

// hubActivityItems maps tracker rows to digest rows. A row the tracker
// flags NeedsHuman (a stage-only human wait such as ready-for-human-merge
// included) carries the needs-human modifier so the digest's urgency order
// matches the tracker's "Needs attention".
func hubActivityItems(items []StageTrackerItem, public publicRepoSet) []hubactivity.Item {
	out := make([]hubactivity.Item, 0, len(items))
	for _, it := range items {
		mods := slices.Clone(it.Modifiers)
		if it.NeedsHuman && !slices.Contains(mods, hubactivity.ModNeedsHuman) && !slices.Contains(mods, hubactivity.ModNeedsDecision) {
			mods = append([]string{hubactivity.ModNeedsHuman}, mods...)
		}
		out = append(out, hubactivity.Item{
			Repo:        it.Repo,
			Number:      it.Number,
			Kind:        it.Kind,
			Title:       it.Title,
			AuthorKind:  it.AuthorKind,
			Stage:       it.StageSlug,
			StageNumber: it.Stage,
			Since:       it.StageSince,
			Mods:        mods,
			Private:     !public.allows(it.Repo),
		})
	}
	return out
}
