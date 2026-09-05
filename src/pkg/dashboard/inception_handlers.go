package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

const maxInceptionBodyBytes = 64 * 1024

func (s *Server) handleInceptionStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Idea  string `json:"idea"`
		Force bool   `json:"force,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Force {
		_ = s.deps.Inception.Reset()
		// Don't kill the session — kickBrainstorm will SendKick to the
		// running agent, which is faster than kill+restart. Killing a
		// busy session causes the next RestartWithBootstrap to race
		// with shell initialization, producing the alternating pattern.
		if s.deps.AgentMgr != nil {
			_ = s.deps.AgentMgr.Pause("brainstorm", "inception-force-reset", "forced reset before new inception")
		}
		if store, ok := s.deps.BeadStores["brainstorm"]; ok {
			s.clearInceptionBeads(store)
		}
	}

	state, err := s.deps.Inception.Start(req.Idea)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.kickBrainstorm()

	s.auditFromRequest(r, "inception_start", "", "")
	jsonResponse(w, map[string]interface{}{
		"ok":    true,
		"state": state,
	})
}

func (s *Server) handleInceptionScan(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		RepoURL string `json:"repo_url"`
		Force   bool   `json:"force,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Force {
		_ = s.deps.Inception.Reset()
		if s.deps.AgentMgr != nil {
			_ = s.deps.AgentMgr.Pause("brainstorm", "inception-force-reset", "forced reset before brownfield scan")
		}
		if store, ok := s.deps.BeadStores["brainstorm"]; ok {
			s.clearInceptionBeads(store)
		}
	}

	state, err := s.deps.Inception.StartBrownfield(req.RepoURL)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.kickBrainstorm()

	s.auditFromRequest(r, "inception_scan", auditDetail("repo_url", req.RepoURL), "")
	jsonResponse(w, map[string]interface{}{
		"ok":    true,
		"state": state,
	})
}

func (s *Server) handleInceptionState(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	state := s.deps.Inception.GetState()
	jsonResponse(w, map[string]interface{}{
		"ok":     true,
		"state":  state,
		"active": state != nil,
	})
}

func (s *Server) handleInceptionSetQuestions(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Questions []knowledge.Question `json:"questions"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.deps.Inception.SetQuestions(req.Questions); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "inception_set_questions", auditDetail("count", strconv.Itoa(len(req.Questions))), "")
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *Server) handleInceptionAnswer(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Answers map[string]string `json:"answers"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	state, err := s.deps.Inception.SubmitAnswers(req.Answers)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Use SendKick (not RestartWithBootstrap) for the structure-phase kick.
	// The agent is already running from the capture kick — restarting it
	// kills its context and can revert the phase back to capture (bug #117).
	s.sendKickBrainstorm()

	s.auditFromRequest(r, "inception_answer", auditDetail("count", strconv.Itoa(len(req.Answers))), "")
	jsonResponse(w, map[string]interface{}{
		"ok":    true,
		"state": state,
	})
}

func (s *Server) handleInceptionRecordFacts(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Facts []knowledge.IdeationFact `json:"facts"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.deps.Inception.RecordFacts(s.deps.Ctx, req.Facts); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "inception_record_facts", auditDetail("count", strconv.Itoa(len(req.Facts))), "")
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *Server) handleInceptionScaffold(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	result, err := s.deps.Inception.ProduceScaffold(s.deps.Ctx)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"ok":       true,
		"scaffold": result,
	})
}

func (s *Server) handleInceptionApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if err := s.deps.Inception.AdvanceToComplete(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Re-pause brainstorm so the governor doesn't kick it with generic
	// messages after inception completes (which can revert the phase).
	if s.deps.AgentMgr != nil {
		_ = s.deps.AgentMgr.Pause("brainstorm", "inception-complete", "inception complete — on-demand only")
	}

	s.auditFromRequest(r, "inception_approve", "", "")
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *Server) clearInceptionBeads(store *beads.Store) {
	all := store.List(beads.ListFilter{})
	closed := 0
	for _, b := range all {
		if b.Status != beads.StatusOpen {
			continue
		}
		if b.Actor == "brainstorm" || b.Actor == "inception" {
			_ = store.Close(b.ID)
			closed++
		}
	}
	if closed > 0 {
		s.logger.Info("cleared inception beads on force-reset", "closed", closed)
	}
}

func (s *Server) handleInceptionReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if err := s.deps.Inception.Reset(); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if store, ok := s.deps.BeadStores["brainstorm"]; ok {
		s.clearInceptionBeads(store)
	}

	if s.deps.AgentMgr != nil {
		_ = s.deps.AgentMgr.Pause("brainstorm", "inception-reset", "inception reset — on-demand only")
	}

	s.auditFromRequest(r, "inception_reset", "", "")
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *Server) handleInceptionIdeationFacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil && s.deps.Inception == nil {
		jsonError(w, "knowledge API not initialized", http.StatusServiceUnavailable)
		return
	}

	var facts []knowledge.Fact
	if s.deps.Knowledge != nil {
		facts = s.deps.Knowledge.ListIdeationFacts(s.deps.Ctx)
	}

	if len(facts) == 0 && s.deps.Inception != nil {
		facts = s.deps.Inception.GatherFactsPublic(s.deps.Ctx)
	}

	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, map[string]interface{}{
		"ok":    true,
		"facts": facts,
	})
}

func (s *Server) handleInceptionDownload(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	result, err := s.deps.Inception.ProduceScaffold(s.deps.Ctx)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	if result == nil || len(result.Files) == 0 {
		jsonError(w, "no scaffold files to download", http.StatusNotFound)
		return
	}

	projectName := "inception-project"
	state := s.deps.Inception.GetState()
	if state != nil && state.IdeaSlug != "" {
		slug := state.IdeaSlug
		if len(slug) > 40 {
			slug = slug[:40]
		}
		projectName = slug
	}

	w.Header().Set("Content-Type", "application/zip")
	safeName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, projectName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, safeName))

	zw := zip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil {
			s.logger.Warn("inception scaffold archive close failed", "error", err)
		}
	}()

	for _, f := range result.Files {
		clean := filepath.Clean(f.Path)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		fw, err := zw.Create(filepath.ToSlash(clean))
		if err != nil {
			s.logger.Warn("inception scaffold archive entry creation failed", "path", clean, "error", err)
			continue
		}
		if _, err := fw.Write([]byte(f.Content)); err != nil {
			s.logger.Warn("inception scaffold archive write failed", "path", clean, "error", err)
			return
		}
	}
}

func (s *Server) handleInceptionHasFiles(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inception == nil {
		jsonResponse(w, map[string]interface{}{"ok": true, "has_files": false})
		return
	}
	hasFiles := s.deps.Inception.HasWikiFiles()
	jsonResponse(w, map[string]interface{}{"ok": true, "has_files": hasFiles})
}

const maxWikiNameLen = 80

func (s *Server) handleInceptionRenameWiki(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(req.Name) > maxWikiNameLen {
		jsonError(w, fmt.Sprintf("name must be %d characters or fewer", maxWikiNameLen), http.StatusBadRequest)
		return
	}
	if s.deps.Knowledge == nil || s.deps.Inception == nil {
		jsonError(w, "knowledge or inception not initialized", http.StatusServiceUnavailable)
		return
	}
	store := s.deps.Knowledge.GetVaultStore(s.deps.Inception.WikiDir())
	if store == nil {
		jsonError(w, "inception wiki vault not found", http.StatusNotFound)
		return
	}
	store.SetName(req.Name)

	// Persist the name in inception state
	if state := s.deps.Inception.GetState(); state != nil {
		s.deps.Inception.SetWikiName(req.Name)
	}

	s.auditFromRequest(r, "inception_rename_wiki", auditDetail("name", req.Name), "")
	s.logger.Info("inception wiki renamed", "name", req.Name)
	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name})
}

func (s *Server) handleInceptionImport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Inception == nil {
		jsonError(w, "inception engine not initialized", http.StatusServiceUnavailable)
		return
	}

	const maxZipUploadBytes = 10 << 20 // 10 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxZipUploadBytes)

	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "file upload required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "failed to read upload", http.StatusBadRequest)
		return
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		jsonError(w, "invalid zip file", http.StatusBadRequest)
		return
	}

	wikiDir := s.deps.Inception.WikiDir()
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		jsonError(w, "failed to create wiki directory", http.StatusInternalServerError)
		return
	}

	imported := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Normalize backslash paths (zip files from Windows use \)
		cleanName := strings.ReplaceAll(f.Name, "\\", "/")
		baseName := filepath.Base(cleanName)
		if strings.Contains(baseName, "..") || !strings.HasSuffix(baseName, ".md") {
			continue
		}
		const maxFileBytes = 1 << 20 // 1 MiB per file
		rc, err := f.Open()
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, maxFileBytes))
		closeErr := rc.Close()
		if readErr != nil || closeErr != nil {
			s.logger.Warn("inception wiki archive entry read failed", "file", baseName, "read_error", readErr, "close_error", closeErr)
			continue
		}

		outPath := filepath.Join(wikiDir, baseName)
		if err := os.WriteFile(outPath, content, 0o644); err != nil {
			continue
		}
		imported++
	}

	// Reconnect vault to pick up imported files
	if s.deps.Knowledge != nil {
		if err := s.deps.Knowledge.ConnectVault(wikiDir, "inception-wiki"); err != nil {
			s.logger.Warn("inception wiki vault reconnect failed", "error", err)
			jsonError(w, "files imported but reconnecting the wiki vault failed", http.StatusInternalServerError)
			return
		}
		if store := s.deps.Knowledge.GetVaultStore(wikiDir); store != nil {
			store.Reindex()
		}
	}

	s.logger.Info("inception wiki imported", "files", imported)
	jsonResponse(w, map[string]interface{}{"ok": true, "imported": imported})
}

func (s *Server) kickBrainstorm() {
	if s.deps.AgentMgr == nil || s.deps.Scheduler == nil {
		return
	}

	// Guard: only kick if inception is in a phase that needs the agent.
	// Kicking during scaffold/complete is harmful — it restarts the agent
	// and can revert the phase back to capture.
	if s.deps.Inception != nil {
		state := s.deps.Inception.GetState()
		if state != nil && (state.Phase == knowledge.PhaseScaffold || state.Phase == knowledge.PhaseComplete) {
			s.logger.Debug("skipping brainstorm kick — inception already past structure",
				"phase", state.Phase)
			return
		}
	}

	// Unpause brainstorm if it was paused (e.g., by a force-reset that
	// preceded this start). Without this, the governor re-pauses the
	// agent on its next eval cycle and the inception stalls in capture.
	if s.deps.AgentMgr != nil {
		_ = s.deps.AgentMgr.Resume(s.deps.Ctx, "brainstorm", "inception-start", "inception started — agent needed")
	}

	go func() {
		if s.kickBrainstormDoneFn != nil {
			defer s.kickBrainstormDoneFn()
		}
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in kickBrainstorm — recovered", "panic", r)
			}
		}()

		msg := s.deps.Scheduler.BuildAgentMessage("brainstorm", nil, s.deps.Scheduler.GetLastActionable())

		// Try SendKick to the running session — send /clear first to
		// reset CLI context, then the inception prompt. No restart needed.
		// This avoids the shell initialization race that causes the
		// alternating responsive/unresponsive pattern.
		clearErr := s.sendBrainstormKick("brainstorm", "/clear")
		if clearErr == nil {
			time.Sleep(kickBrainstormClearDelay)
			if err := s.sendBrainstormKick("brainstorm", msg); err != nil {
				s.logger.Warn("SendKick after /clear failed, trying RestartWithBootstrap", "error", err)
				if err2 := s.restartBrainstormWithBootstrap(s.deps.Ctx, "brainstorm", msg); err2 != nil {
					s.logger.Warn("failed to restart brainstorm for inception", "error", err2)
				}
			} else {
				s.logger.Info("inception kick sent via /clear + SendKick")
			}
		} else {
			// No running session — use RestartWithBootstrap
			s.logger.Debug("no running session, using RestartWithBootstrap", "error", clearErr)
			if err := s.restartBrainstormWithBootstrap(s.deps.Ctx, "brainstorm", msg); err != nil {
				s.logger.Warn("failed to restart brainstorm for inception", "error", err)
			}
		}

		if s.deps.Governor != nil {
			s.deps.Governor.RecordKick("brainstorm")
		}
	}()
}

var kickBrainstormClearDelay = time.Second

func (s *Server) sendBrainstormKick(name, msg string) error {
	if s.kickBrainstormSendKickFn != nil {
		return s.kickBrainstormSendKickFn(name, msg)
	}
	return s.deps.AgentMgr.SendKick(name, msg)
}

func (s *Server) restartBrainstormWithBootstrap(ctx context.Context, name, prompt string) error {
	if s.kickBrainstormRestartFn != nil {
		return s.kickBrainstormRestartFn(ctx, name, prompt)
	}
	return s.deps.AgentMgr.RestartWithBootstrap(ctx, name, prompt)
}

// sendKickBrainstorm sends an inception-specific prompt to the RUNNING
// brainstorm agent via SendKick (no restart). Includes the user's Q&A
// so the agent knows to extract facts and create fact beads.
func (s *Server) sendKickBrainstorm() {
	if s.deps.AgentMgr == nil || s.deps.Inception == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in sendKickBrainstorm — recovered", "panic", r)
			}
		}()

		state := s.deps.Inception.GetState()
		if state == nil {
			return
		}

		msg := s.buildStructureKickMessage(state)
		if err := s.deps.AgentMgr.SendKick("brainstorm", msg); err != nil {
			s.logger.Warn("sendKick for structure phase failed, trying pluk send", "error", err)
			if err2 := plukSendKick("brainstorm", msg); err2 != nil {
				s.logger.Warn("pluk send also failed", "error", err2)
			}
		}

		if s.deps.Governor != nil {
			s.deps.Governor.RecordKick("brainstorm")
		}
	}()
}

func (s *Server) buildStructureKickMessage(state *knowledge.InceptionState) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("INCEPTION TASK: Structure phase for idea: %q\n", state.IdeaText))
	sb.WriteString("The user answered your clarification questions. Create fact beads NOW.\n\n")
	for _, q := range state.Questions {
		ans := state.Answers[q.ID]
		if ans != "" {
			sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n\n", q.Text, ans))
		}
	}
	sb.WriteString("Create at least 3 fact beads. For EACH fact, run:\n\n")
	sb.WriteString(fmt.Sprintf("bd create --title \"<fact title>\" --type advisory --priority 1 --actor brainstorm --external-ref \"inception/%s\"\n", state.IdeaSlug))
	sb.WriteString("bd update <bead-id> --set-metadata fact_type=\"<vision|constitution|requirement|constraint|stakeholder|acceptance>\"\n")
	sb.WriteString("bd update <bead-id> --set-metadata fact_body=\"<detailed fact content>\"\n\n")
	sb.WriteString("Required facts: 1 vision, 1 constitution, 2+ requirements. Start creating beads IMMEDIATELY.")
	return sb.String()
}

// plukSendKick sends the inception prompt via pluk's send subcommand.
// This is more reliable than tmux send-keys + $(cat file) because pluk send
// writes to a named FIFO and confirms delivery via a command_received event.
// Returns error if pluk is not installed or the send fails.
func plukSendKick(session, message string) error {
	plukPath, err := exec.LookPath("pluk")
	if err != nil {
		return fmt.Errorf("pluk not found: %w", err)
	}

	cmd := exec.Command(plukPath,
		"send", "hive-"+session,
		"--text="+message,
		"--enter",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pluk send failed: %w (output: %s)", err, string(out))
	}

	return nil
}

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInceptionBodyBytes+1))
	if err != nil {
		return err
	}
	defer closeHTTPBody(r.Body)
	if int64(len(body)) > maxInceptionBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxInceptionBodyBytes)
	}
	return json.Unmarshal(body, v)
}
