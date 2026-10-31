package serve

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/crux/api"
)

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	q := r.URL.Query()
	lines := api.DefaultLogLines
	// replay is how many buffered lines a follow sends before streaming:
	// n when the client gave one, else the whole follow window.
	replay := followWindow
	if v := q.Get(api.QueryLines); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, api.QueryLines+" must be a non-negative integer")
			return
		}
		lines, replay = n, n
	}
	if isTrue(q.Get(api.QueryFollow)) {
		s.followLogs(w, r, name, replay)
		return
	}
	source := q.Get(api.QuerySource)
	if source == "" {
		source = api.LogSourcePane
	}
	out := api.Logs{Agent: name, Source: source}
	var err error
	switch source {
	case api.LogSourcePane:
		out.Lines, err = s.Runtime.GetOutput(name, lines)
	case api.LogSourceBuffer:
		out.Lines, err = s.Runtime.GetBufferOutput(name, lines)
	case api.LogSourceFull:
		out.Header = s.Runtime.FullLogHeader(name)
		out.Content, err = s.Runtime.CaptureFullLog(name)
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("%s must be %s, %s or %s", api.QuerySource, api.LogSourcePane, api.LogSourceBuffer, api.LogSourceFull))
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, fmt.Sprintf("logs %s: %v", name, err))
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}
