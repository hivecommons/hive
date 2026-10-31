package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// matrixLinesNudgeInterval is the minimum spacing between `code-lines` SSE frames.
const matrixLinesNudgeInterval = time.Second

const (
	matrixMinSinceMax = 1
	matrixMaxSinceMax = 500
)

// matrixLinesNudger coalesces code-lines nudges to at most one frame per
// second: the first frame goes out immediately, and later ones inside the 1 s
// window collapse into a single trailing frame carrying the newest seq.
type matrixLinesNudger struct {
	mu       sync.Mutex
	last     time.Time
	armed    bool
	seq      uint64
	now      func() time.Time
	newTimer func(d time.Duration, f func()) trackerRefreshTimer
}

func (n *matrixLinesNudger) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

func (n *matrixLinesNudger) nudge(seq uint64, send func(seq uint64)) {
	n.mu.Lock()
	n.seq = seq
	if n.armed {
		n.mu.Unlock()
		return
	}
	now := n.clock()
	if wait := matrixLinesNudgeInterval - now.Sub(n.last); !n.last.IsZero() && wait > 0 {
		n.armed = true
		newTimer := n.newTimer
		if newTimer == nil {
			newTimer = func(d time.Duration, f func()) trackerRefreshTimer { return time.AfterFunc(d, f) }
		}
		newTimer(wait, func() {
			n.mu.Lock()
			n.armed = false
			n.last = n.clock()
			seq := n.seq
			n.mu.Unlock()
			send(seq)
		})
		n.mu.Unlock()
		return
	}
	n.last = now
	n.mu.Unlock()
	send(seq)
}

func matrixCodeLinesSSEFrame(seq uint64) string {
	return "event: code-lines\ndata: {\"seq\":" + strconv.FormatUint(seq, 10) + "}\n\n"
}

// nudgeMatrixLines broadcasts the coalesced `code-lines` SSE frame.
func (s *Server) nudgeMatrixLines(seq uint64) {
	if s == nil {
		return
	}
	cfg := s.currentMatrixConfig()
	if !cfg.Active() {
		return
	}
	s.matrixNudge.nudge(seq, func(seq uint64) {
		if s.currentMatrixConfig().Active() {
			s.broadcastFrame(matrixCodeLinesSSEFrame(seq))
		}
	})
}

// currentMatrixConfig returns the effective MatrixConfig.
func (s *Server) currentMatrixConfig() config.MatrixConfig {
	if s == nil {
		return config.MatrixConfig{}
	}
	if s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Dashboard.Matrix
	}
	s.matrixMu.Lock()
	defer s.matrixMu.Unlock()
	if s.matrix != nil {
		if cfg := s.matrix.cfg.Load(); cfg != nil {
			return *cfg
		}
	}
	return config.MatrixConfig{}
}

// matrixActiveSource returns the active matrix line source and its active config,
// or (nil, nil) if the source is nil or the flag is off/inactive.
func (s *Server) matrixActiveSource() (*matrixSource, *config.MatrixConfig) {
	if s == nil {
		return nil, nil
	}
	cfg := s.currentMatrixConfig()
	if !cfg.Active() {
		return nil, nil
	}
	s.matrixMu.Lock()
	defer s.matrixMu.Unlock()
	if s.matrix == nil {
		return nil, nil
	}
	mCfg := s.matrix.cfg.Load()
	if mCfg == nil || !mCfg.Active() {
		return nil, nil
	}
	return s.matrix, &cfg
}

// viewerCanReadMatrix reports whether the caller has at least read role to view matrix lines.
func (s *Server) viewerCanReadMatrix(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role != "" {
		return config.RoleAtLeast(role, config.RoleRead)
	}
	if s != nil && s.authToken != "" {
		token := r.Header.Get("Authorization")
		if !secureCompare(token, "Bearer "+s.authToken) && !secureCompare(token, s.authToken) {
			return false
		}
	}
	return true
}

type matrixLinesResponse struct {
	Seq    uint64       `json:"seq"`
	Source string       `json:"source"`
	Lines  []matrixLine `json:"lines"`
}

// handleMatrixLines serves GET /api/matrix/lines?since=<seq>&max=<n>
func (s *Server) handleMatrixLines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	src, cfg := s.matrixActiveSource()
	if src == nil || cfg == nil || !cfg.Active() {
		jsonError(w, "matrix mode disabled", http.StatusNotFound)
		return
	}

	var since uint64
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if val, err := strconv.ParseUint(sinceStr, 10, 64); err == nil {
			since = val
		}
	}

	limit := matrixDefaultSinceMax // 200
	if r.URL.Query().Has("max") {
		n, err := strconv.Atoi(r.URL.Query().Get("max"))
		if err != nil || n < matrixMinSinceMax {
			limit = matrixMinSinceMax
		} else if n > matrixMaxSinceMax {
			limit = matrixMaxSinceMax
		} else {
			limit = n
		}
	} else if since == 0 {
		limit = matrixBacklogCap // 2000
	}

	rawLines, latest := src.Since(since, limit)

	lines := make([]matrixLine, 0, len(rawLines))
	if s.viewerCanReadMatrix(r) {
		for _, l := range rawLines {
			if cfg.RepoAllowed(l.Repo) {
				lines = append(lines, l)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := matrixLinesResponse{
		Seq:    latest,
		Source: "pushed-lines",
		Lines:  lines,
	}
	_ = json.NewEncoder(w).Encode(resp)
}
