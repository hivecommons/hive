package serve

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

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

// followLogs sends the last replay ring-buffer lines, then streams new ones
// as api.EventLog records until the client goes away or the runtime stops
// answering.
func (s *Server) followLogs(w http.ResponseWriter, r *http.Request, name string, replay int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "streaming is not supported on this connection")
		return
	}
	// prev stays the whole first read so the first poll diffs against all
	// of it, not only the replayed tail.
	prev, err := s.Runtime.GetBufferOutput(name, max(followWindow, replay))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, fmt.Sprintf("logs %s: %v", name, err))
		return
	}
	s.startStream(w)
	flusher.Flush()
	if initial := lastLines(prev, replay); len(initial) > 0 {
		if api.WriteEvent(w, api.EventLog, api.LogChunk{Agent: name, Lines: initial}) != nil {
			return
		}
		flusher.Flush()
	}
	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			cur, err := s.Runtime.GetBufferOutput(name, followWindow)
			if err != nil {
				_ = api.WriteEvent(w, api.EventState, api.Event{Time: s.now(), Type: api.EventState, Agent: name, Detail: err.Error()})
				flusher.Flush()
				return
			}
			if fresh := newTail(prev, cur); len(fresh) > 0 {
				if api.WriteEvent(w, api.EventLog, api.LogChunk{Agent: name, Lines: fresh}) != nil {
					return
				}
				flusher.Flush()
			}
			prev = cur
		}
	}
}

// lastLines is the last n of lines (all of them when there are fewer).
func lastLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// newTail returns the lines of cur that were not already seen in prev,
// treating both as windows onto the same append-only buffer: the longest
// suffix of prev that is a prefix of cur is the overlap. It is the CLI's
// stream rule (crux/cli.newTail) applied server side.
func newTail(prev, cur []string) []string {
	max := len(prev)
	if len(cur) < max {
		max = len(cur)
	}
	for k := max; k > 0; k-- {
		if equalLines(prev[len(prev)-k:], cur[:k]) {
			return cur[k:]
		}
	}
	return cur
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
