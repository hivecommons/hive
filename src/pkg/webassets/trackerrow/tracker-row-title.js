    // Every hive-filed issue title leads with its filing agent in brackets
    // (`[scanner] …`, `[reviewer] Review backlog (security): …`). The tag is
    // drawn once, as a chip ahead of the title, and the title text that
    // follows no longer repeats it; the full title stays in the tooltip. A
    // bracket that is not an agent slug (`[ACMM L0]`, `[Tests]`) is part of
    // the subject and is left in the title.
    function stageTrackerSplitTitleTag(title) {
      const m = /^\s*\[([a-z0-9][a-z0-9_.-]*)\]\s*(.*)$/s.exec(String(title || ''));
      if (!m || !m[2]) return { tag: '', text: String(title || '') };
      return { tag: m[1], text: m[2] };
    }
    function stageTrackerTitleHtml(title, extraClass) {
      const full = String(title || '');
      const { tag, text } = stageTrackerSplitTitleTag(full);
      const cls = 'ham-title' + (extraClass ? ' ' + extraClass : '');
      const chip = tag ? `<span class="ham-badge ham-title-tag" title="${esc('Filed by [' + tag + ']')}">${esc(tag)}</span> ` : '';
      return `${chip}<span class="${cls}" title="${esc(full)}">${esc(text)}</span>`;
    }
