package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	if err := s.AdmitRun(repo, number, strings.TrimSpace(req.Title), time.Now()); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := worksource.Ref{Repo: repo, Number: number}.Key()
	s.auditFromRequest(r, "run_spec_start", auditDetail("run", key), "")
	jsonResponse(w, map[string]any{"ok": true, "key": key, "stage": StageSpec})
}
