package betty

import "time"

// The listening tone: a soft two-note chime, like a smart speaker's wake
// sound, played when she decided she WAS addressed and will answer, so the
// room knows she heard her name. The engine only asks (TriggerTone); the
// session applies the voice rules and the setting, and the bot owns the
// sound.

// toneMinGap is the least time between two tone requests. One line that
// holds two questions is split into two addressed lines (onLine); the room
// hears one chime.
const toneMinGap = 4 * time.Second

// listening asks for the tone for a spoken request she will answer. A chat
// line, a line that does not address her and a request the engine drops
// never get here: it is called only on the paths that go on to reply.
func (e *Engine) listening(l Line, trig Trigger, now time.Time) {
	if !e.s.ListeningTone || !l.Spoken || !trig.VoiceWanted {
		return
	}
	if !e.lastToneAt.IsZero() && now.Sub(e.lastToneAt) < toneMinGap {
		return
	}
	if e.emit(Reply{Kind: KindVoice, Trigger: TriggerTone, Asker: l.Who, VoiceWanted: true}) {
		e.lastToneAt = now
	}
}
