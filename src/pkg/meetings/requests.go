package meetings

import (
	"context"
	"errors"
	"time"
)

// Platform join requests (quorum.contract/v1 section 1): a Discord
// `/hive join` arrives on the all-meetings event stream as a `requested`
// state event with the guild, channel and requester. Hive applies its
// owner-only invite policy: Options.AutoApprove may approve it at once,
// otherwise the meeting waits in requested until the owner approves it in
// the dashboard (meeting.join for that channel) or declines it
// (meeting.leave).

// ErrNotRequested is returned when approving or declining a meeting that is
// not a pending platform request.
var ErrNotRequested = errors.New("meetings: meeting is not a pending join request")

// StartRequests follows the all-meetings event stream for platform join
// requests until Close, reconnecting with backoff.
func (m *Manager) StartRequests() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		backoff := m.opts.ReconnectMin
		for m.ctx.Err() == nil {
			err := m.opts.Quorum.Events(m.ctx, "", 0, func(ev Event) bool {
				backoff = m.opts.ReconnectMin
				if ev.Type == EventState && ev.State == StateRequested && ev.RequestedBy != nil {
					m.onRequest(ev)
				}
				return true
			})
			if m.ctx.Err() != nil {
				return
			}
			m.log.Warn("[meetings] request stream dropped; reconnecting", "error", err, "backoff", backoff.String())
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, m.opts.ReconnectMax)
		}
	}()
}

// findNative returns the meeting hive records for a Quorum meeting id.
func (m *Manager) findNative(nativeID string) (Meeting, bool) {
	list, err := m.opts.Store.List()
	if err != nil {
		return Meeting{}, false
	}
	for _, mt := range list {
		if mt.NativeID == nativeID {
			return mt, true
		}
	}
	return Meeting{}, false
}

func (m *Manager) onRequest(ev Event) {
	if ev.MeetingID == "" {
		return
	}
	if _, ok := m.findNative(ev.MeetingID); ok {
		return
	}
	now := m.opts.Now()
	platform := ev.Platform
	if platform == "" {
		platform = PlatformDiscord
	}
	who := *ev.RequestedBy
	mt := Meeting{
		ID: newMeetingID(), Platform: platform, GuildID: ev.GuildID, ChannelID: ev.ChannelID,
		NativeID: ev.MeetingID, InvitedBy: who.Name, RequestedBy: &who, State: StateRequested,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.opts.Store.Save(mt); err != nil {
		m.log.Warn("[meetings] recording join request", "error", err)
		return
	}
	m.log.Info("[meetings] join request from the platform", "meeting", mt.ID, "platform", platform, "by", who.Name)
	m.emit(Update{Meeting: mt, Event: &ev})
	if m.opts.AutoApprove != nil && m.opts.AutoApprove(who) {
		ctx, cancel := m.callCtx()
		defer cancel()
		if _, err := m.ApproveRequest(ctx, mt.ID, false); err != nil {
			m.log.Warn("[meetings] auto-approving join request", "meeting", mt.ID, "error", err)
		}
	}
}

func (m *Manager) pendingRequest(id string) (Meeting, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return Meeting{}, err
	}
	if mt.State != StateRequested || mt.RequestedBy == nil {
		return mt, ErrNotRequested
	}
	return mt, nil
}

// ApproveRequest joins the channel a platform request named (the same
// Quorum meeting) and starts following it.
func (m *Manager) ApproveRequest(ctx context.Context, id string, voice bool) (Meeting, error) {
	mt, err := m.pendingRequest(id)
	if err != nil {
		return mt, err
	}
	capable := voice || m.opts.VoiceOnAddress
	res, err := m.opts.Quorum.Join(ctx, JoinArgs{
		DisplayName: m.opts.BotName, Platform: mt.Platform, GuildID: mt.GuildID, ChannelID: mt.ChannelID, Voice: capable,
	})
	if err != nil {
		return mt, err
	}
	mt, err = m.update(id, nil, func(x *Meeting) {
		if res.MeetingID != "" {
			x.NativeID = res.MeetingID
		}
		x.VoiceEnabled, x.VoiceCapable = voice, capable
		x.State = StateJoining
		if knownStates[res.State] && res.State != StateRequested {
			x.State = res.State
		}
	})
	if err != nil {
		return mt, err
	}
	m.watch(mt)
	return mt, nil
}

// DeclineRequest refuses a platform request with meeting.leave.
func (m *Manager) DeclineRequest(ctx context.Context, id string) (Meeting, error) {
	mt, err := m.pendingRequest(id)
	if err != nil {
		return mt, err
	}
	if _, err := m.opts.Quorum.Leave(ctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
		return mt, err
	}
	return m.finish(id, StateFailed, ReasonDeclined, nil)
}
