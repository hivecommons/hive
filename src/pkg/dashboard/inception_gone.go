package dashboard

import "net/http"

const (
	// inceptionGoneMessage is the body of every retired /api/inception/*
	// route. The step-by-step inception workflow (start, scan, questions,
	// answers, facts, scaffold, approve, reset, wiki import and rename) was
	// retired in favour of Inception (spek) sessions.
	inceptionGoneMessage = "The inception API was retired. Use Inception (spek) sessions instead: the dashboard's Inception (spek) section, or /api/sessions. Facts from past inception runs stay in the knowledge base."
	// inceptionGoneDocs points callers at the migration notes.
	inceptionGoneDocs = "https://github.com/hivecommons/hive/blob/v5/src/docs/inception.md"
	// inceptionGoneReplacement is the API that replaces it.
	inceptionGoneReplacement = "/api/sessions"
)

// handleInceptionGone answers every former /api/inception/* route with 410
// Gone for one release, so a script or bookmark that still calls it gets a
// pointer to Sessions instead of a bare 404. It reads nothing and changes
// nothing.
func (s *Server) handleInceptionGone(w http.ResponseWriter, _ *http.Request) {
	jsonStatusResponse(w, http.StatusGone, map[string]any{
		"ok":          false,
		"error":       inceptionGoneMessage,
		"replacement": inceptionGoneReplacement,
		"docs":        inceptionGoneDocs,
	})
}
