package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

var runSpecTargetRE = regexp.MustCompile(`^([^\s#]+/[^\s#]+)#([1-9][0-9]*)$`)

func parseRunSpecTarget(raw string) (string, int, error) {
	m := runSpecTargetRE.FindStringSubmatch(strings.TrimSpace(raw))
	if len(m) != 3 {
		return "", 0, fmt.Errorf("target must be owner/repo#number")
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("issue number must be positive")
	}
	return m[1], n, nil
}

func (s *Server) handleRunSpecStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req struct {
		Target string `json:"target"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		Mode   string `json:"mode"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	repo := strings.TrimSpace(req.Repo)
	number := req.Number
	if req.Target != "" {
		var err error
		repo, number, err = parseRunSpecTarget(req.Target)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if repo == "" || number <= 0 {
		jsonError(w, "repo and issue number are required", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Mode), "design") {
		store, _ := s.planEpicStore()
		if store == nil {
			jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = repo + "#" + strconv.Itoa(number)
		}
		epic, key, err := s.startDesignSpektacular(r.Context(), store, github.Issue{Repo: repo, Number: number, Title: title}, "", true)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "run_design_start", auditDetail("run", key, "epic", epic.ID), "")
		jsonResponse(w, map[string]any{"ok": true, "key": key, "stage": StageSpec, "epic_id": epic.ID, "via": planning.DesignViaSpektacular})
		return
	}
	if err := s.AdmitRun(repo, number, strings.TrimSpace(req.Title), time.Now()); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := worksource.Ref{Repo: repo, Number: number}.Key()
	s.auditFromRequest(r, "run_spec_start", auditDetail("run", key), "")
	jsonResponse(w, map[string]any{"ok": true, "key": key, "stage": StageSpec})
}
