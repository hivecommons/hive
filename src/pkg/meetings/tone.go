package meetings

import (
	"context"
	"time"
)

// The listening tone: a short chime Betty plays when she was addressed and
// is about to answer. Quorum owns the sound (the bot synthesizes it); hive
// only asks for it, once, and never waits on it.

// ToneTimeout bounds the say.tone call. The tone is a courtesy, so a slow
// or broken audio path is abandoned quickly.
const ToneTimeout = 2 * time.Second

// Toner is a Quorum that can play the listening tone (say.tone). The real
// Client implements it; an older Quorum answers invalid_argument for the
// unknown tool, which the caller drops.
type Toner interface {
	SayTone(ctx context.Context, nativeID string) error
}

// toneArgs is say.tone's input.
type toneArgs struct {
	MeetingID string `json:"meeting_id"`
}

// SayTone calls say.tone: Quorum plays the listening tone into the meeting
// at once, or drops it when the bot is speaking or the audio path is busy.
func (c *Client) SayTone(ctx context.Context, nativeID string) error {
	return c.Call(ctx, "say.tone", toneArgs{nativeID}, nil)
}

// Tone plays the listening tone in a live meeting. It never speaks text and
// is not recorded as a line. A Quorum without say.tone is a silent no-op;
// the caller applies the voice policy and ignores the error.
func (m *Manager) Tone(ctx context.Context, id string) error {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return err
	}
	if mt.State != StateLive {
		return ErrNotLive
	}
	if mt.ConsentPostedAt == nil {
		return ErrConsentPending
	}
	t, ok := m.opts.Quorum.(Toner)
	if !ok {
		return nil
	}
	return t.SayTone(ctx, mt.NativeID)
}
