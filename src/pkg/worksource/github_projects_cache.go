package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// GitHub Projects v2 reconcile cache (#13379). FromConfig builds a fresh
// githubProjectsSource every eval cycle, so the cache is process-wide and
// keyed by board (API base, organization, project number). The full GraphQL
// listing is the authoritative writer and runs on the reconcile cadence;
// projects_v2_item webhook deliveries are hints that re-read one item and
// patch it in between.
const (
	// githubProjectsReconcileInterval is how often the full listing runs.
	githubProjectsReconcileInterval = 15 * time.Minute
	// githubProjectsLastGoodMaxAge bounds how long a failed re-list may
	// serve the previous listing before the error surfaces to the caller.
	githubProjectsLastGoodMaxAge = 2 * time.Hour
)

var errProjectsItemGone = errors.New("worksource/github_projects: project item not found")

type projectsCacheEntry struct {
	mu        sync.Mutex
	items     []gqlItem
	projectID string
	fetchedAt time.Time
	listed    bool
	stale     bool
	// src is the most recent source built for this board; the webhook
	// patch reads through it so it uses the current token.
	src *githubProjectsSource
}

func (e *projectsCacheEntry) fresh(now time.Time) bool {
	return e.listed && now.Sub(e.fetchedAt) < githubProjectsReconcileInterval
}

func (e *projectsCacheEntry) serveLastGood(now time.Time) bool {
	return e.listed && now.Sub(e.fetchedAt) < githubProjectsLastGoodMaxAge
}

func (e *projectsCacheEntry) replace(items []gqlItem, projectID string, now time.Time, src *githubProjectsSource) {
	e.items = items
	if projectID != "" {
		e.projectID = projectID
	}
	e.fetchedAt = now
	e.listed = true
	e.stale = false
	e.src = src
}

func (e *projectsCacheEntry) index(nodeID string) int {
	for i, it := range e.items {
		if it.ID == nodeID {
			return i
		}
	}
	return -1
}

func (e *projectsCacheEntry) remove(nodeID string) bool {
	i := e.index(nodeID)
	if i < 0 {
		return false
	}
	e.items = append(e.items[:i], e.items[i+1:]...)
	return true
}

func (e *projectsCacheEntry) upsert(item gqlItem) {
	if i := e.index(item.ID); i >= 0 {
		e.items[i] = item
		return
	}
	e.items = append(e.items, item)
}

var (
	projectsCacheMu sync.Mutex
	projectsCaches  = map[string]*projectsCacheEntry{}
)

func (s *githubProjectsSource) cacheKey() string {
	return strings.ToLower(s.endpoint() + "|" + s.cfg.Org + "|" + fmt.Sprint(s.cfg.ProjectNumber))
}

// cacheEntry returns the board's cache entry, creating it on first use, and
// records this source as the one to read through.
func (s *githubProjectsSource) cacheEntry() *projectsCacheEntry {
	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	key := s.cacheKey()
	e := projectsCaches[key]
	if e == nil {
		e = &projectsCacheEntry{}
		projectsCaches[key] = e
	}
	e.mu.Lock()
	e.src = s
	e.mu.Unlock()
	return e
}

// ResetGitHubProjectsCacheForTest empties the process-wide board cache.
func ResetGitHubProjectsCacheForTest() {
	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	projectsCaches = map[string]*projectsCacheEntry{}
}

func allProjectsCaches() []*projectsCacheEntry {
	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	out := make([]*projectsCacheEntry, 0, len(projectsCaches))
	for _, e := range projectsCaches {
		out = append(out, e)
	}
	return out
}

// ProjectsV2ItemPatch reports what a projects_v2_item delivery did to the
// cache.
type ProjectsV2ItemPatch struct {
	Action string
	// Matched is true when a cached board owns the item's project.
	Matched bool
	// Removed, Upserted and Unchanged describe the patch applied.
	Removed   bool
	Upserted  bool
	Unchanged bool
}

func (p ProjectsV2ItemPatch) String() string {
	switch {
	case !p.Matched:
		return p.Action + ": no cached board"
	case p.Removed:
		return p.Action + ": removed"
	case p.Upserted:
		return p.Action + ": upserted"
	default:
		return p.Action + ": unchanged"
	}
}

// ProjectsV2ItemActions are the projects_v2_item webhook actions. All but
// deleted re-read the item; deleted removes it from the cache without a call.
var ProjectsV2ItemActions = map[string]bool{
	"created": true, "edited": true, "deleted": true, "reordered": true,
	"archived": true, "restored": true, "converted": true,
}

type projectsV2ItemPayload struct {
	Action string `json:"action"`
	Item   struct {
		NodeID        string `json:"node_id"`
		ProjectNodeID string `json:"project_node_id"`
		ContentType   string `json:"content_type"`
	} `json:"projects_v2_item"`
}

// ApplyProjectsV2ItemEvent applies one signature-verified projects_v2_item
// delivery to the cached board that owns the item's project. The payload is
// a hint: it names the item and the project, never the item's state, so
// every action except deleted re-reads the item with one GraphQL call and
// upserts what GitHub returns (an archived item or one whose content is
// gone is removed). A board that has not listed yet, or that the delivery's
// project does not match, is left to its next listing.
func ApplyProjectsV2ItemEvent(ctx context.Context, payload []byte) (ProjectsV2ItemPatch, error) {
	var p projectsV2ItemPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ProjectsV2ItemPatch{}, fmt.Errorf("worksource/github_projects: decode projects_v2_item payload: %w", err)
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	patch := ProjectsV2ItemPatch{Action: action}
	if !ProjectsV2ItemActions[action] {
		return patch, fmt.Errorf("worksource/github_projects: unknown projects_v2_item action %q", p.Action)
	}
	if p.Item.NodeID == "" || p.Item.ProjectNodeID == "" {
		return patch, fmt.Errorf("worksource/github_projects: projects_v2_item payload names no item or project")
	}
	for _, e := range allProjectsCaches() {
		e.mu.Lock()
		if !e.listed || e.projectID != p.Item.ProjectNodeID || e.src == nil {
			e.mu.Unlock()
			continue
		}
		patch.Matched = true
		if action == "deleted" {
			patch.Removed = e.remove(p.Item.NodeID)
			patch.Unchanged = !patch.Removed
			e.mu.Unlock()
			return patch, nil
		}
		src := e.src
		e.mu.Unlock()
		item, err := src.fetchItem(ctx, p.Item.NodeID)
		e.mu.Lock()
		switch {
		case errors.Is(err, errProjectsItemGone):
			patch.Removed = e.remove(p.Item.NodeID)
			patch.Unchanged = !patch.Removed
		case err != nil:
			e.mu.Unlock()
			return patch, err
		case item.IsArchived || item.Project.ID != p.Item.ProjectNodeID:
			patch.Removed = e.remove(p.Item.NodeID)
			patch.Unchanged = !patch.Removed
		default:
			e.upsert(item)
			patch.Upserted = true
		}
		e.mu.Unlock()
		return patch, nil
	}
	return patch, nil
}
