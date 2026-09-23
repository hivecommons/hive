package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	contributeWallFileName             = "contribute_wall.json"
	contributeWallMaxTextRunes         = 500
	contributeWallMaxTagRunes          = 100
	contributeWallDefaultRetentionDays = 90
	contributeWallMaxRetentionDays     = 3650
	contributeWallDefaultPostsPerHour  = 6
	contributeWallMaxPosts             = 2000
)

type ContributeWallPost struct {
	ID                string               `json:"id"`
	ParentID          string               `json:"parent_id,omitempty"`
	Author            string               `json:"author"`
	Text              string               `json:"text"`
	Tags              map[string]string    `json:"tags,omitempty"`
	CreatedAt         string               `json:"created_at"`
	Deleted           bool                 `json:"deleted,omitempty"`
	DeletedAt         string               `json:"deleted_at,omitempty"`
	Hidden            bool                 `json:"hidden,omitempty"`
	HiddenAt          string               `json:"hidden_at,omitempty"`
	HiddenBy          string               `json:"hidden_by,omitempty"`
	Flags             []WallFlag           `json:"flags,omitempty"`
	AuthorAvatarURL   string               `json:"author_avatar_url,omitempty"`
	AuthorTrustTier   string               `json:"author_trust_tier,omitempty"`
	AuthorVerifiedPRs int                  `json:"author_verified_prs"`
	ModelEvidence     *WallModelStats      `json:"model_evidence,omitempty"`
	Replies           []ContributeWallPost `json:"replies,omitempty"`
}

type WallFlag struct {
	By string `json:"by"`
	At string `json:"at"`
}

type WallMute struct {
	Username string `json:"username"`
	By       string `json:"by"`
	At       string `json:"at"`
}

type contributeWallDisk struct {
	Posts []ContributeWallPost `json:"posts"`
	Mutes map[string]WallMute  `json:"mutes,omitempty"`
}

type contributeWallStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	posts  []ContributeWallPost
	mutes  map[string]WallMute
	rate   map[string][]time.Time
}

type WallModelStats struct {
	Model           string  `json:"model"`
	Runs            int     `json:"runs"`
	VerifiedPRShare float64 `json:"verified_pr_share"`
	FailureRate     float64 `json:"failure_rate"`
}

type wallContext struct {
	enabled bool
	viewer  string
	admin   bool
	owner   bool
}

var wallStores sync.Map // contributors dir -> *contributeWallStore

func (s *Server) contributeWallStore() *contributeWallStore {
	dir := s.contributorsDirOrDefault()
	if v, ok := wallStores.Load(dir); ok {
		return v.(*contributeWallStore)
	}
	st := &contributeWallStore{path: filepath.Join(dir, contributeWallFileName), mutes: map[string]WallMute{}, rate: map[string][]time.Time{}}
	actual, _ := wallStores.LoadOrStore(dir, st)
	return actual.(*contributeWallStore)
}

func (s *Server) wallRetentionDays() int {
	if s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.ContributeWallRetentionDays > 0 {
		n := s.deps.Config.Hub.ContributeWallRetentionDays
		if n > contributeWallMaxRetentionDays {
			return contributeWallMaxRetentionDays
		}
		return n
	}
	return contributeWallDefaultRetentionDays
}

func (s *Server) wallEnabled() bool {
	return s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.ContributeWallEnabled
}

func wallNow() time.Time { return time.Now().UTC() }

func (st *contributeWallStore) loadLocked(retention time.Duration) {
	if st.loaded {
		return
	}
	st.loaded = true
	st.mutes = map[string]WallMute{}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var disk contributeWallDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return
	}
	st.posts = disk.Posts
	if disk.Mutes != nil {
		st.mutes = disk.Mutes
	}
	st.pruneLocked(wallNow(), retention)
}

func (st *contributeWallStore) pruneLocked(now time.Time, retention time.Duration) bool {
	if retention <= 0 {
		return false
	}
	cutoff := now.Add(-retention)
	kept := st.posts[:0]
	changed := false
	for _, p := range st.posts {
		created, err := time.Parse(time.RFC3339, p.CreatedAt)
		if err == nil && created.Before(cutoff) {
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) > contributeWallMaxPosts {
		kept = kept[len(kept)-contributeWallMaxPosts:]
		changed = true
	}
	st.posts = kept
	return changed
}

func (st *contributeWallStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(contributeWallDisk{Posts: st.posts, Mutes: st.mutes}, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, contributorProfileFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func sanitizeWallText(s string, max int) string { return sanitizeDossierText(s, max) }

func sanitizeWallTags(in map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"model", "backend", "repo"} {
		v := sanitizeDossierText(in[k], contributeWallMaxTagRunes)
		if v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) wallCtx(r *http.Request) wallContext {
	role := r.Header.Get("X-Hive-Role")
	return wallContext{
		enabled: s.wallEnabled(),
		viewer:  s.resolveViewerUsername(r),
		admin:   role == config.RoleOwner || role == config.RoleReadWrite,
		owner:   role == config.RoleOwner && r.Header.Get(ownerRoleVerifiedHeader) == "true",
	}
}

func (s *Server) wallVisiblePosts(ctx wallContext, author string, flaggedOnly bool) ([]ContributeWallPost, []WallMute) {
	st := s.contributeWallStore()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	st.loadLocked(retention)
	if st.pruneLocked(wallNow(), retention) {
		_ = st.saveLocked()
	}
	posts := append([]ContributeWallPost(nil), st.posts...)
	mutes := make([]WallMute, 0, len(st.mutes))
	for _, m := range st.mutes {
		mutes = append(mutes, m)
	}
	st.mu.Unlock()

	profiles := map[string]*ContributorProfile{}
	for i := range posts {
		if _, ok := profiles[strings.ToLower(posts[i].Author)]; !ok {
			profiles[strings.ToLower(posts[i].Author)] = findContributor(posts[i].Author)
		}
	}
	byModel, _ := s.readWallModelStats(0)
	visible := make([]ContributeWallPost, 0, len(posts))
	for _, p := range posts {
		if p.Deleted {
			continue
		}
		if author != "" && !strings.EqualFold(p.Author, author) {
			continue
		}
		if flaggedOnly && len(p.Flags) == 0 {
			continue
		}
		isAuthor := ctx.viewer != "" && strings.EqualFold(ctx.viewer, p.Author)
		if p.Hidden && !isAuthor && !ctx.admin {
			continue
		}
		if prof := profiles[strings.ToLower(p.Author)]; prof != nil {
			p.AuthorAvatarURL = prof.AvatarURL
			p.AuthorTrustTier = prof.TrustTier
			p.AuthorVerifiedPRs = prof.TasksWithPR
		}
		if model := p.Tags["model"]; model != "" {
			if st, ok := byModel[strings.ToLower(model)]; ok {
				cp := st
				p.ModelEvidence = &cp
			}
		}
		if !ctx.admin || !flaggedOnly {
			p.Flags = nil
		}
		visible = append(visible, p)
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].CreatedAt > visible[j].CreatedAt })
	if flaggedOnly {
		sort.Slice(mutes, func(i, j int) bool { return mutes[i].Username < mutes[j].Username })
	}
	return nestWallReplies(visible), mutes
}

func nestWallReplies(posts []ContributeWallPost) []ContributeWallPost {
	parents := []ContributeWallPost{}
	idx := map[string]int{}
	for _, p := range posts {
		p.Replies = nil
		if p.ParentID == "" {
			idx[p.ID] = len(parents)
			parents = append(parents, p)
		}
	}
	for _, p := range posts {
		if p.ParentID == "" {
			continue
		}
		if i, ok := idx[p.ParentID]; ok {
			parents[i].Replies = append(parents[i].Replies, p)
		}
	}
	return parents
}

func (s *Server) handleContributeWall(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleContributeWallList(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handleContributeWallCreate(w, r)
		return
	}
	jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleContributeWallList(w http.ResponseWriter, r *http.Request) {
	ctx := s.wallCtx(r)
	if !ctx.enabled {
		jsonResponse(w, map[string]any{"enabled": false, "posts": []ContributeWallPost{}})
		return
	}
	flagged := r.URL.Query().Get("flagged") == "1"
	if flagged && !ctx.admin {
		jsonError(w, "your permissions on this hive are read-only", http.StatusForbidden)
		return
	}
	posts, mutes := s.wallVisiblePosts(ctx, r.URL.Query().Get("author"), flagged)
	resp := map[string]any{"enabled": true, "posts": posts, "max_chars": contributeWallMaxTextRunes}
	if ctx.admin {
		resp["mutes"] = mutes
	}
	jsonResponse(w, resp)
}

func (s *Server) handleContributeWallCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if !s.wallEnabled() {
		jsonError(w, "contributor wall is disabled", http.StatusForbidden)
		return
	}
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to post.", http.StatusUnauthorized)
		return
	}
	profile := findContributor(username)
	if profile == nil || profile.TrustTier == "revoked" {
		jsonError(w, "You need an active contributor profile to post.", http.StatusForbidden)
		return
	}
	var body struct {
		Text     string            `json:"text"`
		ParentID string            `json:"parent_id"`
		Tags     map[string]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	text := sanitizeWallText(body.Text, contributeWallMaxTextRunes)
	if text == "" {
		jsonError(w, "post text is required", http.StatusBadRequest)
		return
	}
	st := s.contributeWallStore()
	now := wallNow()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked(retention)
	if _, muted := st.mutes[strings.ToLower(username)]; muted {
		jsonError(w, "You are muted from posting on this wall.", http.StatusForbidden)
		return
	}
	if body.ParentID != "" && !st.parentExistsLocked(body.ParentID) {
		jsonError(w, "parent post not found", http.StatusNotFound)
		return
	}
	if !st.allowPostLocked(username, now) {
		jsonError(w, "wall posting rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	post := ContributeWallPost{ID: "cw-" + randomHex(8), ParentID: body.ParentID, Author: profile.GitHubUsername, Text: text, Tags: sanitizeWallTags(body.Tags), CreatedAt: now.Format(time.RFC3339)}
	st.posts = append(st.posts, post)
	st.pruneLocked(now, retention)
	if err := st.saveLocked(); err != nil {
		jsonError(w, "could not save post", http.StatusInternalServerError)
		return
	}
	st.recordPostLocked(username, now)
	s.auditFromRequest(r, "contribute_wall_post", auditDetail("id", post.ID, "author", profile.GitHubUsername), "")
	s.broadcastWallPost(post, false)
	jsonResponse(w, post)
}

func (st *contributeWallStore) parentExistsLocked(id string) bool {
	for _, p := range st.posts {
		if p.ID == id && p.ParentID == "" && !p.Deleted {
			return true
		}
	}
	return false
}

func (st *contributeWallStore) allowPostLocked(username string, now time.Time) bool {
	cutoff := now.Add(-time.Hour)
	key := strings.ToLower(username)
	kept := st.rate[key][:0]
	for _, t := range st.rate[key] {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	st.rate[key] = kept
	return len(kept) < contributeWallDefaultPostsPerHour
}
func (st *contributeWallStore) recordPostLocked(username string, now time.Time) {
	st.rate[strings.ToLower(username)] = append(st.rate[strings.ToLower(username)], now)
}

func (s *Server) handleContributeWallDelete(w http.ResponseWriter, r *http.Request) {
	if !s.wallEnabled() {
		jsonError(w, "contributor wall is disabled", http.StatusForbidden)
		return
	}
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to delete posts.", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	st := s.contributeWallStore()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked(retention)
	for i := range st.posts {
		if st.posts[i].ID != id {
			continue
		}
		if !strings.EqualFold(st.posts[i].Author, username) {
			jsonError(w, "only the author can delete this post", http.StatusForbidden)
			return
		}
		st.posts[i].Deleted = true
		st.posts[i].DeletedAt = wallNow().Format(time.RFC3339)
		if err := st.saveLocked(); err != nil {
			jsonError(w, "could not save post", http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "contribute_wall_delete", auditDetail("id", id, "author", username), "")
		s.broadcastWallPost(st.posts[i], true)
		okResponse(w, map[string]string{"status": "deleted"})
		return
	}
	jsonError(w, "post not found", http.StatusNotFound)
}

func (s *Server) handleContributeWallHide(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	s.wallModerate(w, r, "hide")
}

func (s *Server) handleContributeWallFlag(w http.ResponseWriter, r *http.Request) {
	if !s.wallEnabled() {
		jsonError(w, "contributor wall is disabled", http.StatusForbidden)
		return
	}
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to flag posts.", http.StatusUnauthorized)
		return
	}
	if p := findContributor(username); p == nil || p.TrustTier == "revoked" {
		jsonError(w, "You need an active contributor profile to flag posts.", http.StatusForbidden)
		return
	}
	st := s.contributeWallStore()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked(retention)
	id := r.PathValue("id")
	for i := range st.posts {
		if st.posts[i].ID != id || st.posts[i].Deleted {
			continue
		}
		for _, f := range st.posts[i].Flags {
			if strings.EqualFold(f.By, username) {
				okResponse(w, map[string]string{"status": "flagged"})
				return
			}
		}
		st.posts[i].Flags = append(st.posts[i].Flags, WallFlag{By: username, At: wallNow().Format(time.RFC3339)})
		if err := st.saveLocked(); err != nil {
			jsonError(w, "could not save post", http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "contribute_wall_flag", auditDetail("id", id, "by", username), "")
		okResponse(w, map[string]string{"status": "flagged"})
		return
	}
	jsonError(w, "post not found", http.StatusNotFound)
}

func (s *Server) wallModerate(w http.ResponseWriter, r *http.Request, action string) {
	if !s.wallEnabled() {
		jsonError(w, "contributor wall is disabled", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	st := s.contributeWallStore()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked(retention)
	for i := range st.posts {
		if st.posts[i].ID != id || st.posts[i].Deleted {
			continue
		}
		st.posts[i].Hidden = true
		st.posts[i].HiddenAt = wallNow().Format(time.RFC3339)
		st.posts[i].HiddenBy = r.Header.Get("X-Hive-User")
		if err := st.saveLocked(); err != nil {
			jsonError(w, "could not save post", http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "contribute_wall_hide", auditDetail("id", id, "author", st.posts[i].Author), "")
		s.broadcastWallPost(st.posts[i], true)
		okResponse(w, map[string]string{"status": "hidden"})
		return
	}
	jsonError(w, "post not found", http.StatusNotFound)
}

func (s *Server) handleContributorWallMute(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.wallEnabled() {
		jsonError(w, "contributor wall is disabled", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var body struct {
		Muted *bool `json:"muted"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	muted := true
	if body.Muted != nil {
		muted = *body.Muted
	}
	st := s.contributeWallStore()
	retention := time.Duration(s.wallRetentionDays()) * 24 * time.Hour
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked(retention)
	key := strings.ToLower(p.GitHubUsername)
	if muted {
		st.mutes[key] = WallMute{Username: p.GitHubUsername, By: r.Header.Get("X-Hive-User"), At: wallNow().Format(time.RFC3339)}
	} else {
		delete(st.mutes, key)
	}
	if err := st.saveLocked(); err != nil {
		jsonError(w, "could not save mute", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "contribute_wall_mute", auditDetail("username", p.GitHubUsername, "muted", strconv.FormatBool(muted)), "")
	jsonResponse(w, map[string]any{"ok": true, "username": p.GitHubUsername, "muted": muted})
}

func (s *Server) broadcastWallPost(post ContributeWallPost, hidden bool) {
	if s == nil || s.contributeHub == nil || s.contributeHub.sse == nil {
		return
	}
	typ := "wall_post"
	if hidden {
		typ = "wall_hidden"
		post = ContributeWallPost{ID: post.ID, Hidden: post.Hidden, Deleted: post.Deleted}
	}
	s.contributeHub.sse.broadcast(sseEvent{Type: typ, WallPost: &post})
}

func (s *Server) readWallModelStats(days int) (map[string]WallModelStats, error) {
	path := taskRunLogPath
	if s != nil && s.contributeHub != nil {
		path = s.contributeHub.taskRunLogPath()
	} else if s != nil {
		path = filepath.Join(s.contributorsDirOrDefault(), taskRunLogFileName)
	}
	if days < 0 || days > 365 {
		days = 0
	}
	return readTaskRunModelStats(path, time.Duration(days)*24*time.Hour)
}

func readTaskRunModelStats(path string, window time.Duration) (map[string]WallModelStats, error) {
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]WallModelStats{}, nil
		}
		return nil, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	type agg struct {
		WallModelStats
		verified, failed int
	}
	by := map[string]*agg{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TaskRunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if cutoff != "" && rec.TS < cutoff {
			continue
		}
		model := strings.TrimSpace(rec.Model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		a := by[key]
		if a == nil {
			a = &agg{WallModelStats: WallModelStats{Model: model}}
			by[key] = a
		}
		a.Runs++
		if rec.PRVerified {
			a.verified++
		}
		if rec.Outcome == outcomeFailed {
			a.failed++
		}
	}
	out := map[string]WallModelStats{}
	for k, a := range by {
		if a.Runs > 0 {
			a.VerifiedPRShare = float64(a.verified) / float64(a.Runs)
			a.FailureRate = float64(a.failed) / float64(a.Runs)
		}
		out[k] = a.WallModelStats
	}
	return out, nil
}

func wallStatsList(m map[string]WallModelStats) []WallModelStats {
	out := make([]WallModelStats, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model) })
	return out
}

func (s *Server) handleContributeRunStatsByModel(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 365 {
			days = n
		}
	}
	stats, err := s.readWallModelStats(days)
	if err != nil {
		http.Error(w, "task-run log unreadable", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"window_days": days, "models": wallStatsList(stats)})
}
