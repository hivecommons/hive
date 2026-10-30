package dashboard

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Write-back containment surfaces: the gate's status block on /api/status,
// the suppressed-write drill-down, and the sentinel findings the gate kept
// off GitHub (plus the ones it let through) on /api/sentinel.

// SetWritebackGate installs the process gate so /api/status and the
// drill-down read live state; nil removes it.
func (s *Server) SetWritebackGate(g *writeback.Gate) {
	if s == nil {
		return
	}
	s.writebackMu.Lock()
	s.writebackGate = g
	s.writebackMu.Unlock()
}

func (s *Server) writebackGateRef() *writeback.Gate {
	if s == nil {
		return nil
	}
	s.writebackMu.RLock()
	defer s.writebackMu.RUnlock()
	return s.writebackGate
}

// WritebackStatus is the /api/status `writeback` block (nil gate → zero
// value with Enabled=false so a hive booted without the gate is visible).
func (s *Server) WritebackStatus() *writeback.Status {
	g := s.writebackGateRef()
	if g == nil {
		return nil
	}
	st := g.Snapshot()
	return &st
}

// handleWritebackSuppressed serves GET /api/writeback/suppressed?repo=&kind=&limit=:
// the gate's suppressed writes of the last 24h, newest first, with what
// each would have written and where.
func (s *Server) handleWritebackSuppressed(w http.ResponseWriter, r *http.Request) {
	g := s.writebackGateRef()
	repo := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("repo")))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 2000 {
			jsonError(w, "limit must be 1 to 2000", http.StatusBadRequest)
			return
		}
		limit = n
	}
	items := []writeback.Suppressed{}
	if g != nil {
		for _, it := range g.Recent() {
			if repo != "" && strings.ToLower(it.Repo) != repo {
				continue
			}
			if kind != "" && string(it.Kind) != kind {
				continue
			}
			items = append(items, it)
			if len(items) >= limit {
				break
			}
		}
	}
	resp := map[string]interface{}{"enabled": g != nil, "items": items, "total_24h": 0, "status": s.WritebackStatus()}
	if g != nil {
		resp["total_24h"] = len(g.Recent())
	}
	jsonResponse(w, resp)
}

// SentinelFinding is one sentinel evaluation the dashboard keeps for the
// Security tab and /api/sentinel, whether or not it reached GitHub.
type SentinelFinding struct {
	At               time.Time `json:"at"`
	Repo             string    `json:"repo"`
	Number           int       `json:"number"`
	Author           string    `json:"author"`
	Title            string    `json:"title,omitempty"`
	HeadSHA          string    `json:"head_sha,omitempty"`
	Rules            []string  `json:"rules"`
	Blocked          bool      `json:"blocked"`
	TrustedReason    string    `json:"trusted_reason,omitempty"`
	Suppressed       bool      `json:"suppressed"`
	SuppressedReason string    `json:"suppressed_reason,omitempty"`
}

const maxSentinelFindings = 1000

// sentinelFindings is the in-memory ring behind /api/sentinel.
type sentinelFindings struct {
	mu    sync.Mutex
	items []SentinelFinding
}

func (f *sentinelFindings) record(ev github.SentinelSweepEvent, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := SentinelFinding{At: now.UTC(), Repo: ev.Repo, Number: ev.Number, Author: ev.Author, Title: ev.Title, HeadSHA: ev.HeadSHA,
		Rules: ev.Rules(), Blocked: ev.Blocked, TrustedReason: ev.TrustedReason, Suppressed: ev.Suppressed, SuppressedReason: ev.SuppressedReason}
	// One row per repo#number@head: a re-evaluation of the same head
	// replaces the earlier row rather than stacking.
	for i := range f.items {
		if f.items[i].Repo == item.Repo && f.items[i].Number == item.Number && f.items[i].HeadSHA == item.HeadSHA {
			f.items[i] = item
			return
		}
	}
	f.items = append(f.items, item)
	if len(f.items) > maxSentinelFindings {
		f.items = append([]SentinelFinding(nil), f.items[len(f.items)-maxSentinelFindings:]...)
	}
}

func (f *sentinelFindings) list() []SentinelFinding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentinelFinding, len(f.items))
	copy(out, f.items)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// RecordSentinelFinding is the sentinel sweep's Audit hook target: every
// finding, written or suppressed, lands here for the Security tab.
func (s *Server) RecordSentinelFinding(ev github.SentinelSweepEvent) {
	if s == nil {
		return
	}
	s.sentinelFindings.record(ev, time.Now())
}

// handleSentinel serves GET /api/sentinel?repo=&suppressed=true|false:
// the sentinel findings the hive holds, including those the write-back
// gate kept off GitHub.
func (s *Server) handleSentinel(w http.ResponseWriter, r *http.Request) {
	repo := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("repo")))
	var wantSuppressed *bool
	if raw := strings.TrimSpace(r.URL.Query().Get("suppressed")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			jsonError(w, "suppressed must be true or false", http.StatusBadRequest)
			return
		}
		wantSuppressed = &b
	}
	items := []SentinelFinding{}
	suppressed := 0
	for _, it := range s.sentinelFindings.list() {
		if it.Suppressed {
			suppressed++
		}
		if repo != "" && strings.ToLower(it.Repo) != repo {
			continue
		}
		if wantSuppressed != nil && it.Suppressed != *wantSuppressed {
			continue
		}
		items = append(items, it)
	}
	jsonResponse(w, map[string]interface{}{
		"findings":   items,
		"total":      len(s.sentinelFindings.list()),
		"suppressed": suppressed,
		"writeback":  s.WritebackStatus(),
	})
}
