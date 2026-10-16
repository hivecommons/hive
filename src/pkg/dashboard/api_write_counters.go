package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

// writeCountersPath is proxy.WriteCountersPath, the durable attempted-vs-blocked
// agent write counters (#12069). It is written only by the hive process.
var writeCountersPath = "/var/run/hive-metrics/write-counters.json"

// intentCountersPath is intent.CountersPath, the durable evaluated-vs-would-block
// intent counters (#12362). It is written only by the hive process.
var intentCountersPath = "/var/run/hive-metrics/intent-counters.json"

// handleWriteCounters serves GET /api/evidence/write-counters: the durable
// counters by intent tier and block reason, with a weekly series. Read-only
// and observation-only; it never gates anything.
func (s *Server) handleWriteCounters(w http.ResponseWriter, r *http.Request) {
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	data, err := os.ReadFile(writeCountersPath)
	if errors.Is(err, os.ErrNotExist) {
		jsonResponse(w, map[string]interface{}{"totals": map[string]interface{}{}, "weeks": map[string]interface{}{}})
		return
	}
	if err != nil {
		jsonError(w, "write counters unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !json.Valid(data) {
		jsonError(w, "write counters file is corrupt", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
