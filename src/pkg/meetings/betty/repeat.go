package betty

import (
	"strings"
	"time"
)

// Repeated spoken requests (Meet test): "Betty, please introduce yourself"
// said twice, the second because the first answer was slow, got a second
// intro and a research ack that talked over the first. The same speaker
// saying the same request again within repeatWindow is dropped: the first
// answer is on its way.

// repeatWindow is how long a spoken request counts as still being
// answered.
const repeatWindow = 60 * time.Second

// introKey is the request key of every intro: "introduce yourself" and
// "who are you" are one request.
const introKey = "\x00intro"

// spokenRequest is one speaker's last spoken request to her.
type spokenRequest struct {
	key string
	at  time.Time
}

// requestKey is what a request asks, for spotting a repeat.
func requestKey(t Trigger) string {
	if t.Intro {
		return introKey
	}
	return strings.Join(strings.Fields(strings.Trim(t.Rest, " .?!,")), " ")
}

// repeatedRequest reports whether who just asked key and records it. A
// repeat does not extend the window: a third ask after it is answered.
func (e *Engine) repeatedRequest(who, key string, now time.Time) bool {
	k := strings.ToLower(strings.TrimSpace(who))
	prev, ok := e.lastSpoken[k]
	if ok && prev.key == key && now.Sub(prev.at) < repeatWindow {
		return true
	}
	e.lastSpoken[k] = spokenRequest{key: key, at: now}
	return false
}

// repeatPending reports whether a partial line from who is the start of
// the request they asked within repeatWindow (no early ack for it).
func (e *Engine) repeatPending(who, rest string, now time.Time) bool {
	prev, ok := e.lastSpoken[strings.ToLower(strings.TrimSpace(who))]
	if !ok || now.Sub(prev.at) >= repeatWindow || prev.key == introKey {
		return false
	}
	part := strings.Join(strings.Fields(strings.Trim(rest, " .?!,")), " ")
	return part != "" && strings.HasPrefix(prev.key, part)
}

// introStarts are the openings of an intro request, so a partial line
// still being said ("please introduce") is known to be one.
var introStarts = []string{"introduce", "who are", "who're", "tell us about yourself", "what are you", "what do you do"}

// mayBecomeIntro reports whether a partial request is an intro being said.
func mayBecomeIntro(rest string) bool {
	r := " " + strings.Join(strings.Fields(rest), " ") + " "
	for _, p := range introStarts {
		if strings.Contains(r, " "+p+" ") || strings.HasSuffix(strings.TrimSpace(r), p) {
			return true
		}
	}
	return false
}
