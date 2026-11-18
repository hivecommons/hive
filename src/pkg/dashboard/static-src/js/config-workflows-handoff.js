    // The Settings → Repos line for a hand-off an agent did not start
    // (hivecommons/hive#18749), with where its reason came from
    // (hivecommons/hive#18916). The reason is text an agent wrote in a session
    // that may be over: it carries its age and session, and once a newer
    // hand-off of the item exists (`stale`) it is shown as an earlier
    // decline, never as the item's current state. Pure: the caller escapes
    // the result when it puts it in markup.
    function workflowHandoffNote(h, profile) {
      const ref = String(h && h.ref || '');
      const reason = String(h && h.reason || 'the agent did not act');
      const age = h && h.reason_age ? String(h.reason_age) + ' ago' : 'at an unknown time';
      const session = h && h.reason_session ? String(h.reason_session) : 'unknown (before provenance was recorded)';
      const origin = 'Decided ' + age + ' in session ' + session + '.';
      if (h && h.stale) {
        return 'Earlier hand-off of ' + ref + ' (stale): ' + reason + '. ' + origin + ' A newer hand-off of this item exists, so this is history, not its state now.';
      }
      return 'Hand-off of ' + ref + ' not started: ' + reason + '. ' + origin + ' ' + (h && h.retrying ? 'The hive hands it to another agent that may do the work, if there is one.' : 'It waits for an owner: use Send to ' + String(profile) + ' on the item\'s row in the Activity Monitor.');
    }
