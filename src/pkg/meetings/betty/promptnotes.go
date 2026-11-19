package betty

import "fmt"

// Answer prompt notes for web search requests and for requests whose
// words were corrected after speech-to-text (normalise.go).

// Web search notes. Asked to search the web with nothing found, she used to
// say "I can't search the web from this answer": she now says what the
// search did, in one short sentence.
const (
	webAskedNote = "When asked to search the web or to look something up: if the context has web results (a source starting \"" + webSource + "\", or a named public source), answer from them and name the source in a few words. " +
		"Never say you cannot search the web from this answer or that you have no web access.\n"
	webEmptyNote = "If asked to search and the context has no such results, say in one short sentence that the web search returned nothing just now, offer to dig deeper, and set needs_research to true.\n"
	webDownNote  = "Context marked \"" + WebUnavailableSource + "\" says what happened to the web search. If it says the search is unavailable, say plainly in one short sentence that web search is down right now. " +
		"If it says nothing was found, say the web search returned nothing just now, offer to dig deeper, and set needs_research to true.\n"
)

// hasWebUnavailable reports whether the context carries the note that the
// web search was down or empty.
func hasWebUnavailable(snips []Snippet) bool {
	for _, s := range snips {
		if s.Source == WebUnavailableSource {
			return true
		}
	}
	return false
}

// webSearchNote is the answer prompt's instruction for a request to search
// the web, by what the context holds.
func webSearchNote(snips []Snippet) string {
	if hasWebUnavailable(snips) {
		return webAskedNote + webDownNote
	}
	return webAskedNote + webEmptyNote
}

// Notes for a request whose words were corrected.
const (
	heardMeantNote = "Speech-to-text often mishears the hive's own words (hive, hive queue, pull request, Kubernetes, KubeCon). The data has a line \"Heard: ... Most likely meant: ...\": answer the reading most likely meant, never the words as heard, and do not mention the correction.\n"
	heardUnsureFmt = "You are not sure what was asked. Reply with only this short question and nothing else, with confidence 1 and needs_research false: %s\n"
)

// heardNote is the answer prompt's instruction for a corrected request:
// answer the corrected reading, or ask the short clarifying question when
// the correction is unsure.
func heardNote(n Normalised) string {
	if !n.Changed() {
		return ""
	}
	if q := n.Clarify(); q != "" {
		return heardMeantNote + fmt.Sprintf(heardUnsureFmt, q)
	}
	return heardMeantNote
}
