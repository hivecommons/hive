package meetings

import (
	"strings"
	"sync"
	"time"
)

// A platform can show hive's own chat line under a different author than
// the bot's configured name (a guest rename, a truncated display name), and
// a line hive spoke can come back transcribed. The author check then lets
// it through as a participant's line, and its own introduction (it names the
// leave phrase) would take the bot out of the meeting. So hive remembers
// the text of its recent lines and treats an equal incoming line as its own.

const (
	// ownEchoWindow is how long after hive said or posted a line an equal
	// incoming line still counts as its echo.
	ownEchoWindow = time.Minute
	// ownEchoKeep bounds the remembered lines per meeting.
	ownEchoKeep = 32
)

type ownLine struct {
	text string
	at   time.Time
}

// ownEchoes is a Store's memory of the bot's recent lines per meeting.
type ownEchoes struct {
	mu    sync.Mutex
	lines map[string][]ownLine
}

// echoKey folds case and whitespace so a platform's re-wrapping of a line
// does not hide it.
func echoKey(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func (o *ownEchoes) remember(id, text string, at time.Time) {
	key := echoKey(text)
	if key == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lines == nil {
		o.lines = map[string][]ownLine{}
	}
	kept := append(o.lines[id], ownLine{text: key, at: at})
	if len(kept) > ownEchoKeep {
		kept = kept[len(kept)-ownEchoKeep:]
	}
	o.lines[id] = kept
}

func (o *ownEchoes) match(id, text string, now time.Time) bool {
	key := echoKey(text)
	if key == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, l := range o.lines[id] {
		if l.text == key && now.Sub(l.at) <= ownEchoWindow && !now.Before(l.at.Add(-ownEchoWindow)) {
			return true
		}
	}
	return false
}

// IsOwnEcho reports whether text equals a line the bot said or posted in
// the meeting within ownEchoWindow of now.
func (s *Store) IsOwnEcho(id, text string, now time.Time) bool {
	return s.echoes.match(id, text, now)
}
