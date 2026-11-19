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
				// The live request carries requested_by; the snapshot a
				// (re)connect opens with carries only the channel.
				if ev.Type == EventState && ev.State == StateRequested && (ev.RequestedBy != nil || ev.GuildID != "") {
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

// knownChannel reports whether a running meeting already targets the
// channel: a dashboard join announces itself on the request stream too,
// possibly before its Quorum id is recorded.
func (m *Manager) knownChannel(guild, channel string) bool {
	if guild == "" || channel == "" {
		return false
	}
	list, err := m.opts.Store.List()
	if err != nil {
		return false
	}
	for _, mt := range list {
		if !mt.State.Terminal() && mt.GuildID == guild && mt.ChannelID == channel {
			return true
		}
	}
	return false
}

func (m *Manager) onRequest(ev Event) {
	if ev.MeetingID == "" {
		return
	}
	if _, ok := m.findNative(ev.MeetingID); ok || m.knownChannel(ev.GuildID, ev.ChannelID) {
		return
	}
	now := m.opts.Now()
	platform := ev.Platform
	if platform == "" {
		platform = PlatformDiscord
	}
	var who Person
	if ev.RequestedBy != nil {
		who = *ev.RequestedBy
	}
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
	if m.autoApproves(platform, who) {
		ctx, cancel := m.callCtx()
		defer cancel()
		approved, err := m.ApproveRequest(ctx, mt.ID, false)
		if err != nil {
			m.log.Warn("[meetings] auto-approving join request", "meeting", mt.ID, "error", err)
			return
		}
		m.log.Info("[meetings] join request approved automatically", "meeting", mt.ID, "platform", platform, "requester_id", who.ID)
		if m.opts.OnAutoApproved != nil {
			m.opts.OnAutoApproved(approved, who)
		}
	}
}

// autoApproves decides whether a request is approved without the owner: the
// AutoApprove hook says so, or the requester's platform user id is on the
// owner's trusted list. A request with no user id (the snapshot a reconnect
// opens with) and a display name are never enough.
func (m *Manager) autoApproves(platform string, who Person) bool {
	if m.opts.AutoApprove != nil && m.opts.AutoApprove(who) {
		return true
	}
	return who.ID != "" && m.opts.Trust.IsTrusted(platform, who.ID)
}

// ApproveAndTrust approves a pending request and trusts its requester by the
// platform user id the request carries, so their later requests are approved
// automatically. The id comes from the request, never from the caller. By
// names the owner who chose it.
func (m *Manager) ApproveAndTrust(ctx context.Context, id string, voice bool, by string) (Meeting, Trusted, error) {
	mt, err := m.pendingRequest(id)
	if err != nil {
		return mt, Trusted{}, err
	}
	// Check the id is trustable before joining, so a refusal joins nothing.
	if m.opts.Trust == nil || mt.Platform != PlatformDiscord || !discordID.MatchString(mt.RequestedBy.ID) {
		return mt, Trusted{}, ErrTrustInvalid
	}
	approved, err := m.ApproveRequest(ctx, id, voice)
	if err != nil {
		return approved, Trusted{}, err
	}
	t, err := m.opts.Trust.Add(Trusted{Platform: mt.Platform, ID: mt.RequestedBy.ID, Name: mt.RequestedBy.Name, By: by, At: m.opts.Now()})
	return approved, t, err
}

// TrustedRequesters lists the people whose join requests are approved
// automatically.
func (m *Manager) TrustedRequesters() ([]Trusted, error) {
	if m.opts.Trust == nil {
		return nil, nil
	}
	return m.opts.Trust.List()
}

// Untrust removes a trusted requester and reports whether they were listed.
func (m *Manager) Untrust(platform, id string) (bool, error) {
	if m.opts.Trust == nil {
		return false, nil
	}
	return m.opts.Trust.Remove(platform, id)
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
		// The request stays open so the owner can approve again once the
		// cause is fixed (a missing Discord permission, say); the reason is
		// kept on it so the panel and the log both name the failure.
		m.log.Warn("[meetings] approving a platform join request failed", "meeting", id, "platform", mt.Platform, "error", err)
		if saved, uerr := m.update(id, nil, func(x *Meeting) { x.StateReason = err.Error() }); uerr == nil {
			mt = saved
		}
		return mt, err
	}
	mt, err = m.update(id, nil, func(x *Meeting) {
		if res.MeetingID != "" {
			x.NativeID = res.MeetingID
		}
		// A platform-initiated join (Discord `/hive join`, auto-approved
		// or approved in the dashboard) talks by default whenever the bot
		// can speak; the owner can still turn voice off for the meeting.
		x.VoiceEnabled, x.VoiceCapable = voice || capable, capable
		x.State, x.StateReason = StateJoining, ""
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
