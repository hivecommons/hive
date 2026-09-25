package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/hivecommons/hive/pkg/classify"
)

func (s *Server) handleClassifierStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(classify.CurrentStats())
}
