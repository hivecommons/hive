// Example patch for handleKick and associated mutation endpoints in src/pkg/dashboard/api.go:

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handleModelSet(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handleResetRestarts(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
    if !s.requireOwnerRole(w, r) {
        return
    }
    // existing handler logic...
}