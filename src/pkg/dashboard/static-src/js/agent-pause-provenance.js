    // Pause provenance on the agent card (hivecommons/hive#18916).
    //
    // A paused agent used to show only "Paused". GET /api/status now carries
    // `pauseProvenance` per agent: `pause` (kind, label, at, reason, line) says
    // who or what paused it, when and why, and `lastResume` says who or what
    // last resumed it. These helpers only turn that into text; the callers put
    // it through esc(). The reason is operator- or hive-written text, already
    // redacted and capped server-side, and is still escaped where it is shown.
    const PAUSE_UNKNOWN_LABEL = 'unknown (before provenance was recorded)';

    function pauseProvenancePause(a) {
      const p = a && a.pauseProvenance;
      return p && p.pause ? p.pause : null;
    }

    function pauseProvenanceResume(a) {
      const p = a && a.pauseProvenance;
      return p && p.lastResume ? p.lastResume : null;
    }

    function pauseProvenanceWhen(at) {
      if (!at) return '';
      return typeof formatDashboardDateTime === 'function' ? formatDashboardDateTime(at) : String(at);
    }

    // "paused by dashboard user alice at <time>: <reason>". An agent whose
    // pause predates provenance reads "paused: unknown (before provenance was
    // recorded)": never a guess at who did it.
    function pauseProvenanceSentence(a) {
      const p = pauseProvenancePause(a);
      if (!p) return '';
      const label = String(p.label || PAUSE_UNKNOWN_LABEL);
      let s = p.kind === 'unknown' ? 'paused: ' + label : 'paused by ' + label;
      const when = pauseProvenanceWhen(p.at);
      if (when) s += ' at ' + when;
      if (p.reason) s += ': ' + String(p.reason);
      return s;
    }

    // "last resumed by <actor> at <time>".
    function pauseResumeSentence(a) {
      const r = pauseProvenanceResume(a);
      if (!r) return '';
      const label = String(r.label || PAUSE_UNKNOWN_LABEL);
      let s = r.kind === 'unknown' ? 'last resumed: ' + label : 'last resumed by ' + label;
      const when = pauseProvenanceWhen(r.at);
      if (when) s += ' at ' + when;
      return s;
    }

    // The text a resume control's tooltip leads with, ending in a space, or ''
    // when nothing is known.
    function pauseProvenanceTip(a) {
      const parts = [pauseProvenanceSentence(a), pauseResumeSentence(a)].filter(Boolean);
      return parts.length ? parts.join('. ') + '. ' : '';
    }

    // The visible line on a paused agent's card: who or what paused it, when
    // and why, then who or what last resumed it. Empty for an agent that is
    // not paused.
    function pauseCardLineHtml(a) {
      if (!a || !a.paused) return '';
      const text = pauseProvenanceSentence(a) || 'paused: ' + PAUSE_UNKNOWN_LABEL;
      const resumed = pauseResumeSentence(a);
      return '<div class="agent-pause-line" data-status="warn">' + esc(text) + (resumed ? '<span class="agent-pause-resumed"> · ' + esc(resumed) + '</span>' : '') + '</div>';
    }
