    // The attendee's own lines (hivecommons/hive Betty round 2): every line
    // she says or posts is in the meeting's transcript as a `reply` event
    // ({type:'reply', author, text, at, reply:{kind, trigger, source,
    // delivered, undelivered, fallback, timing}}). The meeting view and the
    // Inbox show them among the participants' lines, set apart, with how
    // each went out (spoken or chat), what it was made from, and the ones
    // that never reached the meeting.
    const MEETINGS_REPLY_SPOKEN_MARK = '🔊';
    const MEETINGS_REPLY_CHAT_MARK = '💬';
    const MEETINGS_REPLY_UNDELIVERED_MARK = '⚠';
    // A rendered transcript labels her lines "Name (assistant, spoken; source: x): text".
    const MEETINGS_ASSISTANT_LINE = /^([^\n:]*?) \(assistant, (spoken|chat|not delivered)([^)]*)\): /;

    function meetingsReplyMark(how) {
      if (how === 'spoken') return MEETINGS_REPLY_SPOKEN_MARK;
      if (how === 'not delivered') return MEETINGS_REPLY_UNDELIVERED_MARK;
      return MEETINGS_REPLY_CHAT_MARK;
    }

    function meetingsReplyHow(r) {
      if (!r.delivered) return 'not delivered';
      return r.kind === 'voice' ? 'spoken' : 'chat';
    }

    // One of her lines in the live meeting view.
    function meetingsReplyLineHtml(ev) {
      const r = ev.reply || {};
      const who = (ev.author && ev.author.name) || 'hive';
      const how = meetingsReplyHow(r);
      const meta = [how + (r.fallback ? ' (fallback)' : ''), r.source ? 'source: ' + r.source : '', r.trigger || ''].filter(Boolean).join(' · ');
      const why = !r.delivered && r.undelivered ? ' <span class="meetings-reply-meta">' + escapeHtml(r.undelivered) + '</span>' : '';
      return '<p class="meetings-line is-reply' + (r.delivered ? '' : ' is-undelivered') + '" data-reply-kind="' + escapeHtml(how) + '">' +
        '<span class="meetings-who">' + meetingsReplyMark(how) + ' ' + escapeHtml(who) + '</span>' +
        '<span class="meetings-reply-meta">' + escapeHtml(meta) + '</span>' + escapeHtml(ev.text || '') + why + '</p>';
    }

    // The "can't reach participants" notice: replies are failing in chat
    // and by voice. The Leave button on the meeting's row removes the bot;
    // it leaves by itself if this lasts.
    function meetingsReachHtml(live) {
      const n = (live && live.undelivered) || 0;
      if (!n) return '';
      const since = live.unreachable_since ? ' since ' + meetingsFormatTime(live.unreachable_since) : '';
      const why = live.undelivered_reason ? ' Last reason: ' + live.undelivered_reason + '.' : '';
      if (!live.unreachable) {
        return '<p class="muted-xs meetings-reach">' + n + ' repl' + (n === 1 ? 'y' : 'ies') + ' could not be delivered' + escapeHtml(since) + '; kept below as not delivered.' + escapeHtml(why) + '</p>';
      }
      return '<p class="meetings-reach is-unreachable">Can\'t reach participants: ' + n + ' replies in a row were delivered neither in chat nor by voice' + escapeHtml(since) + '.' + escapeHtml(why) +
        ' They are kept below as not delivered. Use Leave on this meeting to remove the bot; it leaves by itself if this continues.</p>';
    }

    // A meeting's rendered transcript (the Inbox item's Transcript view):
    // her lines are set apart from the participants', marked spoken or chat.
    function meetingsTranscriptTextHtml(text) {
      return String(text || '').split('\n').map(line => {
        const m = line.match(MEETINGS_ASSISTANT_LINE);
        if (!m) return escapeHtml(line);
        return '<span class="inbox-assistant-line' + (m[2] === 'not delivered' ? ' is-undelivered' : '') + '" data-reply-kind="' + escapeHtml(m[2]) + '">' +
          meetingsReplyMark(m[2]) + ' ' + escapeHtml(line) + '</span>';
      }).join('\n');
    }
