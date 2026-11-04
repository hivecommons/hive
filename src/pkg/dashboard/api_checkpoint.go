package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// handleCheckpointRegions runs behind handleMatrixLines' existing read gate.
// Initializing the memory-only projection here gives an enabled cold source a
// truthful empty 200, without waiting for or initiating a sentinel pass.
func (s *Server) handleCheckpointRegions(w http.ResponseWriter, r *http.Request) {
	s.MatrixObserver(s.currentMatrixConfig())
	src, cfg := s.matrixActiveSource()
	if src == nil || cfg == nil {
		jsonError(w, "matrix mode disabled", http.StatusNotFound)
		return
	}
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit := matrixDefaultSinceMax
	if r.URL.Query().Has("max") {
		n, err := strconv.Atoi(r.URL.Query().Get("max"))
		if err != nil || n < matrixMinSinceMax {
			n = matrixMinSinceMax
		}
		limit = min(n, matrixMaxSinceMax)
	}
	resp := src.checkpoints.since(since, limit)
	if last, next := src.ingestStatus(src.now()); !last.IsZero() {
		resp.LastIngestAt = &last
		resp.NextIngestInS = next
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
