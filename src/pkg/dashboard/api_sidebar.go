package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
)

func (s *Server) handleSidebarGet(w http.ResponseWriter, r *http.Request) {
	s.sidebarMu.RLock()
	sb := s.sidebar
	s.sidebarMu.RUnlock()
	if sb == nil {
		jsonResponse(w, map[string]interface{}{"sidebar": nil})
		return
	}
	jsonResponse(w, map[string]interface{}{"sidebar": sb})
}

func (s *Server) handleSidebarSet(w http.ResponseWriter, r *http.Request) {
	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.sidebarMu.Lock()
	s.sidebar = body
	s.sidebarMu.Unlock()

	s.auditFromRequest(r, "config_sidebar", "", "")
	s.saveSidebarToDisk(body)
	okResponse(w, map[string]string{"status": "updated"})
}

var sidebarFile = "/data/sidebar.json"

func (s *Server) loadSidebarFromDisk() {
	data, err := os.ReadFile(sidebarFile)
	if err != nil {
		return
	}
	var sb interface{}
	if json.Unmarshal(data, &sb) == nil {
		s.sidebarMu.Lock()
		s.sidebar = sb
		s.sidebarMu.Unlock()
	}
}

func (s *Server) saveSidebarToDisk(sb interface{}) {
	data, err := json.Marshal(sb)
	if err != nil {
		return
	}
	tmpSidebar := sidebarFile + ".tmp"
	if os.WriteFile(tmpSidebar, data, 0o644) == nil {
		_ = os.Rename(tmpSidebar, sidebarFile)
	}
}
