package hub

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// hubWidgetHistoryBucket matches the 15-minute sampling of the registry's
// per-hive IssueHistory/PRHistory, so per-hive series line up when summed.
const hubWidgetHistoryBucket = 15 * 60

type hubWidgetHive struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	URL           string `json:"url,omitempty"`
	Online        bool   `json:"online"`
	ACMMLevel     int    `json:"acmm_level"`
	Issues        int    `json:"issues"`
	PRs           int    `json:"prs"`
	ActIssues     int    `json:"act_issues"`
	ActPRs        int    `json:"act_prs"`
	AgentsRunning int    `json:"agents_running"`
	AgentsTotal   int    `json:"agents_total"`
	LastHeartbeat string `json:"last_heartbeat"`
}

type hubWidgetTotals struct {
	Hives         int `json:"hives"`
	Online        int `json:"online"`
	NeedYou       int `json:"need_you"`
	Issues        int `json:"issues"`
	PRs           int `json:"prs"`
	ActIssues     int `json:"act_issues"`
	ActPRs        int `json:"act_prs"`
	AgentsRunning int `json:"agents_running"`
	AgentsTotal   int `json:"agents_total"`
}

type hubWidgetHistoryPoint struct {
	T         int64 `json:"t"`
	ActIssues int   `json:"act_issues"`
	ActPRs    int   `json:"act_prs"`
}

type hubWidgetResponse struct {
	GeneratedAt string                  `json:"generated_at"`
	Totals      hubWidgetTotals         `json:"totals"`
	Hives       []hubWidgetHive         `json:"hives"`
	History     []hubWidgetHistoryPoint `json:"history"`
}

// handleHubWidget serves GET /api/hub/widget for the iPhone fleet widget
// (hivecommons/hive#12148): every hive the signed-in user can see, with the
// counts from its last heartbeat, plus fleet totals and a fleet-wide
// actionable-work history for sparklines.
func (s *HubServer) handleHubWidget(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	resp := s.buildHubWidget(username, time.Now())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// hubWidgetVisible applies the My Hives rule: hub admins see every hive;
// everyone else sees hives they own or hold a role on.
func hubWidgetVisible(h RegistryEntry, username string, user *SaaSUser, isAdmin bool) bool {
	if isAdmin || canonicalEqual(h.Owner, username) {
		return true
	}
	if user == nil {
		return false
	}
	_, ok := user.Hives[h.ID]
	return ok
}

func (s *HubServer) buildHubWidget(username string, now time.Time) hubWidgetResponse {
	user := loadSaaSUser(username)
	isAdmin := isHubAdmin(username)

	s.mu.Lock()
	var visible []RegistryEntry
	for _, h := range s.registry.Hives {
		if !hubWidgetVisible(h, username, user, isAdmin) || isPlaceholderEntry(MyHiveEntry{RegistryEntry: h}) {
			continue
		}
		e := h
		// The history slices alias the registry; copy before releasing the lock.
		e.IssueHistory = append([]SparkPoint(nil), h.IssueHistory...)
		e.PRHistory = append([]SparkPoint(nil), h.PRHistory...)
		visible = append(visible, e)
	}
	s.mu.Unlock()

	resp := hubWidgetResponse{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Hives:       make([]hubWidgetHive, 0, len(visible)),
	}
	for _, h := range visible {
		project := h.ProjectName
		if project == "" {
			project = h.Name
		}
		if project == "" && h.Org != "" {
			project = h.Org + "/" + h.PrimaryRepo
		}
		hive := hubWidgetHive{
			ID:            h.ID,
			Project:       project,
			URL:           h.DashboardURL,
			Online:        h.Online,
			ACMMLevel:     h.ACMMLevel,
			Issues:        h.OpenIssues,
			PRs:           h.OpenPRs,
			ActIssues:     h.ActionableIssues,
			ActPRs:        h.ActionablePRs,
			AgentsRunning: h.AgentCount,
			AgentsTotal:   len(h.Agents),
			LastHeartbeat: h.LastHeartbeat,
		}
		resp.Hives = append(resp.Hives, hive)

		t := &resp.Totals
		t.Hives++
		if hive.Online {
			t.Online++
		}
		if hive.ActIssues+hive.ActPRs > 0 {
			t.NeedYou++
		}
		t.Issues += hive.Issues
		t.PRs += hive.PRs
		t.ActIssues += hive.ActIssues
		t.ActPRs += hive.ActPRs
		t.AgentsRunning += hive.AgentsRunning
		t.AgentsTotal += hive.AgentsTotal
	}
	sort.SliceStable(resp.Hives, func(i, j int) bool {
		a, b := resp.Hives[i], resp.Hives[j]
		if wa, wb := a.ActIssues+a.ActPRs, b.ActIssues+b.ActPRs; wa != wb {
			return wa > wb
		}
		return a.ID < b.ID
	})
	resp.History = hubWidgetFleetHistory(visible)
	return resp
}

// hubWidgetFleetHistory sums the hives' persisted actionable issue/PR series
// into one fleet series. Hives sample independently, so each hive's last value
// is carried forward into buckets where it has no sample of its own.
func hubWidgetFleetHistory(hives []RegistryEntry) []hubWidgetHistoryPoint {
	bucketSet := map[int64]bool{}
	type series map[int64]int
	issues := make([]series, len(hives))
	prs := make([]series, len(hives))
	for i, h := range hives {
		issues[i] = series{}
		prs[i] = series{}
		for _, p := range h.IssueHistory {
			b := p.T / hubWidgetHistoryBucket
			issues[i][b] = p.V
			bucketSet[b] = true
		}
		for _, p := range h.PRHistory {
			b := p.T / hubWidgetHistoryBucket
			prs[i][b] = p.V
			bucketSet[b] = true
		}
	}
	if len(bucketSet) == 0 {
		return []hubWidgetHistoryPoint{}
	}
	buckets := make([]int64, 0, len(bucketSet))
	for b := range bucketSet {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	lastIssues := make([]int, len(hives))
	lastPRs := make([]int, len(hives))
	actIssues := make([]SparkPoint, 0, len(buckets))
	actPRs := make([]SparkPoint, 0, len(buckets))
	for _, b := range buckets {
		sumIssues, sumPRs := 0, 0
		for i := range hives {
			if v, ok := issues[i][b]; ok {
				lastIssues[i] = v
			}
			if v, ok := prs[i][b]; ok {
				lastPRs[i] = v
			}
			sumIssues += lastIssues[i]
			sumPRs += lastPRs[i]
		}
		t := b * hubWidgetHistoryBucket
		actIssues = append(actIssues, SparkPoint{T: t, V: sumIssues})
		actPRs = append(actPRs, SparkPoint{T: t, V: sumPRs})
	}
	actIssues = downsampleSpark(actIssues, sparkWirePoints)
	actPRs = downsampleSpark(actPRs, sparkWirePoints)
	out := make([]hubWidgetHistoryPoint, len(actIssues))
	for i := range actIssues {
		out[i] = hubWidgetHistoryPoint{T: actIssues[i].T, ActIssues: actIssues[i].V, ActPRs: actPRs[i].V}
	}
	return out
}
