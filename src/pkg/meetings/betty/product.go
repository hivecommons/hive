package betty

import (
	"context"
	"strings"
)

// The product brief: what Hive is, who it is for, how its docs position it
// next to adjacent tools, the inbox voice sources and the meeting platforms.
// It is offered as context for questions about the product, so "which voice
// memo providers does hive support" or "who are hive's competitors" is
// answered from the project's own docs instead of missing retrieval and
// going to research. Keep every line true to the docs it names; it claims
// nothing they do not.

// ProductBriefSource is the Snippet.Source of the product brief.
const ProductBriefSource = "hive product brief"

// maxProductBriefChars bounds the whole brief (every section together).
const maxProductBriefChars = 2300

// Brief section titles.
const (
	briefTitleOverview    = "Short overview of Hive, with an offer to go deeper"
	briefTitleFacts       = "What Hive is and who it is for (README.md, src/docs/what-is-hive.md)"
	briefTitleVoice       = "Inbox voice sources (src/docs/inbox-voice-sources.md)"
	briefTitleMeetings    = "Live meetings (src/docs/meetings.md)"
	briefTitlePositioning = "How Hive differs from adjacent tools (src/docs/landscape.md)"
)

// Brief section texts.
const (
	briefOverview = "Hive is the flagship of Hive Commons, a CNCF sandbox project, and helps people and AI agents work together. " +
		"It sends short-lived coding agents to triage, fix and review a repo's issues and pull requests, within set limits. " +
		"People keep the decisions; I can go deeper on agents, the inbox or meetings."

	briefFacts = "Hive Commons is the sandbox project, Hive its flagship (open source, Apache-2.0). " +
		"It is for an operator running a group of coding and review agents with shared budgets and operator-controlled permissions. " +
		"Agents: Claude, Copilot, Codex, Antigravity, Goose and others, a fresh one per task. " +
		"Limits: six ACMM trust levels, gates and an audit log. " +
		"A spoke is one running hive (Kubernetes, Podman or Docker Compose); a hub manages a fleet of spokes; the contributor relay (ClankeR) lets contributors donate agent compute. " +
		"Code: GitHub or GitHub Enterprise. Tasks: GitHub Issues, GitHub Projects, Linear or Jira."

	briefVoice = "Supported voice sources: Plaud, Wispr Flow, Zoom, Microsoft Teams, Discord, Google Meet, Webex, Slack huddles, Fireflies.ai, Fathom, Limitless Pendant, Granola and Otter.ai, plus files uploaded to the Inbox. " +
		"Connect a source in Knowledge, Integrations; its recordings land in the Inbox, where each can become an RFC, go to DIBS or stay as knowledge."

	briefMeetings = "The hive joins Google Meet, Microsoft Teams and Zoom links and Discord voice channels, takes notes and answers as the meeting attendee. " +
		"The owner sends it from Meetings in the dashboard, or adds the hive's address to a calendar invite. " +
		"Zoom now refuses web-client bots; the desktop companion lets the attendee follow a call from a participant's computer."

	briefPositioning = "The docs call these comparisons, not competitors. " +
		"Fullsend is \"the closest open-source comparison\", lighter for one repo where Actions is already trusted. " +
		"gh-aw or Jules Action: bounded agent jobs, no standing fleet. " +
		"Agent HQ / Copilot coding agent: the quickest hosted path for GitHub issues. " +
		"Symphony: spec-first, one workspace per ticket. " +
		"Hosted services (Devin-class): vendor-run autonomy over self-hosted control. " +
		"Hive: operating a live agent fleet with many runtimes, ACMM gates, hub/spoke provisioning, contributor compute and cost visibility. " +
		"The page is dated; a web search can add newer names."
)

// productNames name the product. "hives" is how speech-to-text writes
// "hive's".
var productNames = []string{
	"hive", "hives", "hive commons", "hivecommons", "clanker", "contributor relay", "acmm",
}

// Words that pick a brief section.
var (
	briefVoiceWords = []string{
		"voice", "memo", "memos", "recorder", "recorders", "recording", "recordings", "dictation", "dictations",
		"inbox", "plaud", "transcription",
	}
	briefMeetingWords = []string{
		"meeting", "meetings", "meet", "zoom", "teams", "discord", "calendar", "invite", "platform", "platforms",
	}
	briefPositioningWords = []string{
		"competitor", "competitors", "competition", "competing", "rival", "rivals", "alternative", "alternatives",
		"compare", "compares", "compared", "comparison", "comparisons", "versus", "vs", "differ", "differs",
		"different", "difference", "positioning", "landscape",
	}
	// briefCueWords make a question with a section word, but no product
	// name, a question about the product ("which voice memo providers do
	// you support").
	briefCueWords = []string{
		"you", "your", "we", "our", "support", "supports", "supported", "integrate", "integrates", "integration",
		"integrations", "provider", "providers",
	}
)

// briefBroadWords ask about the product as a whole.
var briefBroadWords = []string{
	"know", "about", "overview", "what is", "what s", "whats", "explain", "describe", "everything", "summary", "summarize", "summarise",
}

// briefWanted reports whether a question that the live state already
// answers also wants the brief: it asks broadly, or about a brief section.
func briefWanted(text string) bool {
	plain := plainText(text)
	return hasPhrase(plain, briefBroadWords) || hasPhrase(plain, briefVoiceWords) || hasPhrase(plain, briefMeetingWords) ||
		hasPhrase(plain, briefPositioningWords)
}

// IsProductQuestion reports whether text asks about the Hive product: what
// it is, what it supports, how it compares.
func IsProductQuestion(text string) bool {
	plain := plainText(text)
	if hasPhrase(plain, productNames) {
		return true
	}
	section := hasPhrase(plain, briefVoiceWords) || hasPhrase(plain, briefMeetingWords) || hasPhrase(plain, briefPositioningWords)
	return section && hasPhrase(plain, briefCueWords)
}

// ProductBrief is the brief's sections that bear on a product question: the
// short overview always, then the sections the question's words pick, or
// the general facts when none does.
func ProductBrief(text string) []Snippet {
	if !IsProductQuestion(text) {
		return nil
	}
	plain := plainText(text)
	out := []Snippet{{Source: ProductBriefSource, Title: briefTitleOverview, Text: briefOverview}}
	picked := false
	add := func(words []string, title, body string) {
		if hasPhrase(plain, words) {
			out = append(out, Snippet{Source: ProductBriefSource, Title: title, Text: body})
			picked = true
		}
	}
	add(briefVoiceWords, briefTitleVoice, briefVoice)
	add(briefMeetingWords, briefTitleMeetings, briefMeetings)
	add(briefPositioningWords, briefTitlePositioning, briefPositioning)
	if !picked {
		out = append(out, Snippet{Source: ProductBriefSource, Title: briefTitleFacts, Text: briefFacts})
	}
	return out
}

// productRetrieverName is ProductRetriever's name when it wraps nothing.
const productRetrieverName = "product"

// ProductRetriever offers the product brief for product questions. Live,
// when set, is the hive's live status retriever: both run in one retriever
// because Gather stops waiting as soon as live state arrives, and a
// question such as "tell me all you know about hive right now" needs the
// state and the brief together.
type ProductRetriever struct {
	Live Retriever
}

// Name implements Retriever.
func (p ProductRetriever) Name() string {
	if p.Live != nil {
		return p.Live.Name()
	}
	return productRetrieverName
}

// Retrieve implements Retriever.
func (p ProductRetriever) Retrieve(ctx context.Context, query string, terms []string, limit int) []Snippet {
	var out []Snippet
	if p.Live != nil {
		out = p.Live.Retrieve(ctx, query, terms, limit)
	}
	if len(out) > 0 && !briefWanted(query) {
		// A plain status question ("how many open issues does hive have")
		// is answered by the live state alone.
		return out
	}
	return append(out, ProductBrief(query)...)
}

// WithProductBrief adds the product brief to a persona's retrievers: it
// wraps the hive status retriever when there is one, and otherwise goes
// first.
func WithProductBrief(rs []Retriever) []Retriever {
	statusName := HiveStatusRetriever{}.Name()
	out := make([]Retriever, 0, len(rs)+1)
	wrapped := false
	for _, r := range rs {
		if !wrapped && r != nil && r.Name() == statusName {
			out = append(out, ProductRetriever{Live: r})
			wrapped = true
			continue
		}
		out = append(out, r)
	}
	if !wrapped {
		out = append([]Retriever{ProductRetriever{}}, out...)
	}
	return out
}

// hasSource reports whether a snippet has the source.
func hasSource(snips []Snippet, source string) bool {
	for _, s := range snips {
		if s.Source == source {
			return true
		}
	}
	return false
}

// Notes for the answer prompt, by what the context holds.
const (
	productBriefNote = "Context marked \"" + ProductBriefSource + "\" is this project's own documentation: answer questions about Hive from it directly, in its framing, and set needs_research to false. " +
		"For a broad question (\"what do you know about hive\") give its short overview and offer to go deeper. " +
		"Name only the tools it names; if it does not answer the question, say in one sentence what you do know and offer to look it up.\n"
	unavailableNote = "Context marked \"" + WeatherUnavailableSource + "\" means live weather could not be read: say in one short sentence that you cannot get current conditions right now, state no temperature or conditions, and set needs_research to false.\n"
	needsPlaceNote  = "Context marked \"" + WeatherNeedsPlaceSource + "\" means the question named no place and you cannot know where the speaker is: ask which city or town they mean, name no place yourself, and set needs_research to false.\n"
)

// LocalAnswerNotes are the answer prompt's lines for context that settles
// how to answer without research: the product brief and a weather question
// with no place. (A web search that was down or empty has its own note,
// webSearchNote in promptnotes.go.)
func LocalAnswerNotes(snips []Snippet) string {
	var b strings.Builder
	if hasSource(snips, ProductBriefSource) {
		b.WriteString(productBriefNote)
	}
	if hasSource(snips, WeatherNeedsPlaceSource) {
		b.WriteString(needsPlaceNote)
	}
	if hasSource(snips, WeatherUnavailableSource) {
		b.WriteString(unavailableNote)
	}
	return b.String()
}

// AnsweredLocally reports whether the context already settles the answer,
// so a weak model reply must not become a deep dive: the product brief
// answers product questions, and a weather question with no place needs a
// place from the speaker, not research.
func AnsweredLocally(snips []Snippet) bool {
	return hasSource(snips, ProductBriefSource) || hasSource(snips, WeatherNeedsPlaceSource) || hasSource(snips, WeatherUnavailableSource)
}
