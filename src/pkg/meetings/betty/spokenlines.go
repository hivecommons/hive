package betty

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
)

// FixedSpokenLines are the persona's fixed spoken sentences, in her
// language: the spoken intro, the acknowledgements, the leave
// confirmation, the deep dive lines, the "rest is in chat" note, the plain
// fallbacks and the clarifying questions. The session can ask the speech
// service to pre-render them when a meeting starts, so they play at once.
// Each is the exact text she says: a name or any other prefix would miss
// the speech cache. Lines longer than config.MeetingsSpokenLineMaxChars
// (a custom intro with no short form) are included; the caller may skip
// them.
func FixedSpokenLines(p config.ResolvedMeetingsPersona) []string {
	ph := phrasesFor(p.Language)
	lines := []string{p.SpokenIntro()}
	lines = append(lines, ph.acks...)
	lines = append(lines, meetings.LeavingText, ph.still, ph.digIn)
	rest, ok := restInChat[primaryTag(p.Language)]
	if !ok {
		rest = restInChat["en"]
	}
	lines = append(lines, rest, fallbackHello, fallbackUnsure, fallbackNoModel)
	lines = append(lines, clarifyLines()...)
	out := make([]string, 0, len(lines))
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}
