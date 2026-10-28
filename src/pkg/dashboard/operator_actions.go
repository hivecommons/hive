package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const operatorActionCap = 20

// OperatorAction is the durable outcome of an Activity Monitor row action.
type OperatorAction struct {
	Action  string    `json:"action"`
	Actor   string    `json:"actor"`
	At      time.Time `json:"at"`
	Note    string    `json:"note"`
	Outcome string    `json:"outcome"`
	Head    string    `json:"head"`
	Stage   int       `json:"stage"`
}

// Serialize read-modify-rename across Server instances.
var operatorActionsMu sync.Mutex

func (s *Server) operatorActionsPath() string {
	if s.operatorActionPath != "" {
		return s.operatorActionPath
	}
	return "/data/operator-actions.json"
}

func readOperatorActions(path string) (map[string][]OperatorAction, error) {
	entries := map[string][]OperatorAction{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = map[string][]OperatorAction{}
	}
	return entries, nil
}

func (s *Server) operatorActions(repo string, number int) ([]OperatorAction, error) {
	operatorActionsMu.Lock()
	defer operatorActionsMu.Unlock()
	entries, err := readOperatorActions(s.operatorActionsPath())
	if err != nil {
		return nil, err
	}
	out := append([]OperatorAction{}, entries[fmt.Sprintf("%s#%d", strings.ToLower(repo), number)]...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Server) appendOperatorAction(repo string, number int, entry OperatorAction) error {
	operatorActionsMu.Lock()
	defer operatorActionsMu.Unlock()
	path := s.operatorActionsPath()
	entries, err := readOperatorActions(path)
	if err != nil {
		return err
	} // Never overwrite an unreadable ledger.
	key := fmt.Sprintf("%s#%d", strings.ToLower(repo), number)
	trail := append(entries[key], entry)
	if len(trail) > operatorActionCap {
		trail = trail[len(trail)-operatorActionCap:]
	}
	entries[key] = trail
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *Server) newOperatorAction(repo string, number int, actor, action, note string) OperatorAction {
	entry := OperatorAction{Action: action, Actor: actor, At: time.Now().UTC(), Note: note, Outcome: "ok"}
	// Capture the row before the action changes its stage or head.
	s.statusMu.RLock()
	if s.status != nil {
		rows := s.buildStageTracker(s.status, s.stageStoreOrShared(), entry.At)
		for _, row := range rows.Items {
			if stageTrackerSameRepo(row.Repo, repo) && row.Number == number {
				entry.Head, entry.Stage = row.Head, row.Stage
				break
			}
		}
	}
	s.statusMu.RUnlock()
	return entry
}

func (s *Server) recordOperatorAction(repo string, number int, entry OperatorAction) {
	if err := s.appendOperatorAction(repo, number, entry); err != nil && s.logger != nil {
		s.logger.Warn("failed to persist operator action", "repo", repo, "number", number, "error", err)
	}
	s.invalidateStageTrackerForWebhook()
}

func (s *Server) attachOperatorActions(items []StageTrackerItem) {
	operatorActionsMu.Lock()
	entries, err := readOperatorActions(s.operatorActionsPath())
	operatorActionsMu.Unlock()
	if err != nil {
		return
	}
	for i := range items {
		trail := entries[fmt.Sprintf("%s#%d", strings.ToLower(items[i].Repo), items[i].Number)]
		items[i].OperatorActionCount = len(trail)
		if len(trail) > 0 {
			last := trail[len(trail)-1]
			items[i].LastOperatorAction = &last
		}
	}
}

// Observe the existing response without changing its status or body.
type operatorActionResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *operatorActionResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *operatorActionResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.body.Len() < 16384 {
		remaining := 16384 - w.body.Len()
		if remaining > len(p) {
			remaining = len(p)
		}
		w.body.Write(p[:remaining])
	}
	return w.ResponseWriter.Write(p)
}
func (s *Server) observeOperatorAction(w http.ResponseWriter, r *http.Request, repo string, number int, action, note string) (http.ResponseWriter, func()) {
	entry := s.newOperatorAction(repo, number, strings.TrimSpace(r.Header.Get("X-Hive-User")), action, note)
	response := &operatorActionResponse{ResponseWriter: w}
	return response, func() {
		if response.status >= 400 {
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(response.body.Bytes(), &body)
			entry.Outcome = body.Error
			if entry.Outcome == "" {
				entry.Outcome = http.StatusText(response.status)
			}
		}
		s.recordOperatorAction(repo, number, entry)
	}
}

// handleRepoItem serves the retained ledger, newest first.
func (s *Server) handleRepoItem(w http.ResponseWriter, r *http.Request) {
	owner, repo, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	if !s.prQueueRepoAllowed(owner + "/" + repo) {
		jsonError(w, "repository is not managed by this hive", http.StatusForbidden)
		return
	}
	entries, err := s.operatorActions(owner+"/"+repo, number)
	if err != nil {
		jsonError(w, "operator action ledger unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, entries)
}
