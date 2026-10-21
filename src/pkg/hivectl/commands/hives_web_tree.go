package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

// Each tab reviews a private frozen snapshot. The opaque session ID and edit
// version prevent an Apply from consuming a different tab's or request's diff.
// Staging lives only in memory; the shared layer alone computes diffs/receipts.
type commonsWebStage struct {
	edits             *hivectl.StagedEdits
	revision          string
	version           uint64
	touched           time.Time
	projectionPending bool
	stale             bool
}

type commonsTreeResponse struct {
	Session           string                    `json:"session"`
	Revision          string                    `json:"revision"`
	Version           uint64                    `json:"version"`
	Tree              *hivectl.ContributionTree `json:"tree,omitempty"`
	Diff              hivectl.ContributionDiff  `json:"diff"`
	Receipt           *hivectl.ApplyReceipt     `json:"receipt,omitempty"`
	Error             string                    `json:"error,omitempty"`
	Complete          bool                      `json:"complete"`
	ProjectionPending bool                      `json:"projection_pending"`
}

type commonsTreeEdit struct {
	Session    string `json:"session"`
	Version    uint64 `json:"version"`
	Operation  string `json:"operation"`
	Target     string `json:"target"`
	Repository string `json:"repository"`
	Hub        bool   `json:"hub"`
	Value      bool   `json:"value"`
}

func commonsWebOrigin(raw string) string {
	origin, _ := hivectl.NormalizeHiveID(raw)
	return origin
}

// Loading the page must not migrate contributor.env or rewrite consent.
func (s *commonsWebServer) reviewSet() (*hivectl.ProfileSet, error) {
	set, err := s.deps.store.LoadForReview()
	if errors.Is(err, hivectl.ErrNoProfiles) || err == nil && set == nil {
		return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}, nil
	}
	if err != nil {
		return nil, errors.New("cannot read contribution profiles")
	}
	return set, nil
}

// Refresh uses the shared public discovery adapters; see
// hivectl.DiscoverContributionTree.
func discoverCommonsTree(ctx context.Context, set *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
	return hivectl.DiscoverContributionTree(ctx, set, 10*time.Second)
}

func (s *commonsWebServer) handleTree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := r.URL.Query().Get("session"); id != "" {
		stage := s.stages[id]
		if stage == nil {
			http.Error(w, "review expired; reload and review again", http.StatusConflict)
			return
		}
		s.writeTree(w, id, stage, nil, nil, false)
		return
	}
	// Bound abandoned browser sessions without silently evicting active reviews.
	for id, stage := range s.stages {
		if time.Since(stage.touched) > 8*time.Hour {
			delete(s.stages, id)
		}
	}
	if len(s.stages) >= 64 {
		http.Error(w, "too many open reviews; discard an unused review first", http.StatusTooManyRequests)
		return
	}
	set, err := s.reviewSet()
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	hubs, repos := hivectl.SavedTreeHubs(set), map[string]hivectl.TreeRepositorySnapshot(nil)
	if r.URL.Query().Get("discover") == "1" {
		hubs, repos = s.discover(r.Context(), set)
	}
	edits, err := hivectl.NewStagedEdits(set, hubs, repos)
	if err != nil {
		http.Error(w, "cannot construct contribution review", http.StatusBadRequest)
		return
	}
	id, err := randomCommonsWebToken()
	if err != nil {
		http.Error(w, "cannot create contribution review", http.StatusInternalServerError)
		return
	}
	stage := &commonsWebStage{edits: edits, revision: set.Revision(), touched: time.Now()}
	s.stages[id] = stage
	s.writeTree(w, id, stage, nil, nil, false)
}

func (s *commonsWebServer) handleTreeEdit(w http.ResponseWriter, r *http.Request) {
	// Never echo decoder errors: an unknown field can itself contain a secret.
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, commonsWebReadBytes))
	dec.DisallowUnknownFields()
	var req commonsTreeEdit
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid contribution edit", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid contribution edit", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stage := s.stages[req.Session]
	if stage == nil {
		http.Error(w, "review expired; reload and review again", http.StatusConflict)
		return
	}
	// Header presence matters even when reviewing a not-yet-created profile file.
	match, present := r.Header["If-Match"]
	if !present || len(match) != 1 || strings.Trim(match[0], `"`) != stage.revision || req.Version != stage.version {
		http.Error(w, "review changed; reload and review again", http.StatusConflict)
		return
	}
	if r.URL.Path == "/api/tree/discard" {
		if err := stage.edits.Discard(); err != nil {
			s.writeTree(w, req.Session, stage, nil, err, false)
			return
		}
		delete(s.stages, req.Session)
		writeCommonsJSON(w, map[string]bool{"discarded": true})
		return
	}
	if stage.stale {
		s.writeTree(w, req.Session, stage, nil, hivectl.ErrStaleRevision, false)
		return
	}
	// Diff never silently rebases over CLI/TUI/other-tab changes. Apply also checks
	// inside CommitExpecting's cross-process lock, including projection retries.
	if !stage.projectionPending {
		set, err := s.reviewSet()
		if err != nil {
			writeCommonsError(w, err)
			return
		}
		if set.Revision() != stage.revision {
			s.writeTree(w, req.Session, stage, nil, hivectl.ErrStaleRevision, false)
			return
		}
	}
	if r.URL.Path == "/api/tree/apply" {
		receipt, err := stage.edits.Apply(r.Context(), s.deps.store)
		stage.projectionPending = receipt.Saved == hivectl.StageProfilesSaved
		complete := receipt.Saved == hivectl.StageSaved || receipt.Saved == hivectl.StageUnchanged
		s.writeTree(w, req.Session, stage, &receipt, err, complete)
		if complete {
			delete(s.stages, req.Session)
		}
		return
	}
	var err error
	switch req.Operation {
	case "repository":
		err = stage.edits.SelectRepository(req.Target, req.Repository, req.Value)
	case "current":
		err = stage.edits.SelectCurrent(req.Target, req.Hub, req.Value)
	case "exclude":
		err = stage.edits.SetExcluded(req.Target, req.Repository, req.Value)
	case "follow":
		err = stage.edits.SetFollow(req.Target, req.Value)
	case "enabled":
		if req.Hub {
			err = stage.edits.SetHubEnabled(req.Target, req.Value)
		} else {
			err = stage.edits.SetHiveEnabled(req.Target, req.Value)
		}
	case "restrict":
		err = stage.edits.ReviewAndRestrict(req.Target)
	default:
		err = errors.New("invalid contribution operation")
	}
	if err == nil {
		stage.version++
	}
	s.writeTree(w, req.Session, stage, nil, err, false)
}

func (s *commonsWebServer) writeTree(w http.ResponseWriter, id string, stage *commonsWebStage, receipt *hivectl.ApplyReceipt, err error, complete bool) {
	stage.touched = time.Now()
	tree, treeErr := stage.edits.Tree()
	if treeErr != nil {
		http.Error(w, "cannot read contribution tree", http.StatusBadRequest)
		return
	}
	response := commonsTreeResponse{Session: id, Revision: stage.revision, Version: stage.version, Tree: tree, Diff: stage.edits.Diff(), Receipt: receipt, Complete: complete, ProjectionPending: stage.projectionPending}
	response.Diff.Stale = response.Diff.Stale || stage.stale
	code := http.StatusOK
	if err != nil {
		code = http.StatusBadRequest
		response.Error = "cannot stage contribution edit; review the target and try again"
		for _, safe := range []error{hivectl.ErrStageIncomplete, hivectl.ErrStageLegacy, hivectl.ErrStageSave, hivectl.ErrStageProjection, hivectl.ErrStageReload} {
			if errors.Is(err, safe) {
				response.Error = safe.Error()
				break
			}
		}
		if errors.Is(err, hivectl.ErrStaleRevision) {
			code = http.StatusConflict
			stage.stale = true
			response.Diff.Stale = true
			response.Error = "profiles changed; discard this stale review and review again before Apply"
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(response)
}
