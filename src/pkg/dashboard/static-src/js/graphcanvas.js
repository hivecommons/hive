    // ── Graph view: one person, the artefacts they touched, and the other
    // people on those artefacts (hivecommons/hive#18742). Pure layout and an
    // SVG string; styling is by class, so the page's tokens apply. The
    // knowledge graph canvas is animated and drawn in a canvas element; this
    // is a small static drawing that stays readable on a phone.
    const GRAPHCANVAS_W = 640;
    const GRAPHCANVAS_H = 420;
    const GRAPHCANVAS_RING_ARTEFACT = 120;
    const GRAPHCANVAS_RING_PEOPLE = 185;
    const GRAPHCANVAS_MAX_ARTEFACTS = 24;
    const GRAPHCANVAS_LABEL_MAX = 26;

    function graphcanvasRound(n) { return Math.round(n * 10) / 10; }

    function graphcanvasLabel(s) {
      s = String(s == null ? '' : s);
      return s.length > GRAPHCANVAS_LABEL_MAX ? s.slice(0, GRAPHCANVAS_LABEL_MAX - 1) + '…' : s;
    }

    // graphcanvasLayout places the person in the middle, their artefacts on
    // a ring, and each other person on an outer ring at the average angle of
    // the artefacts they share. edges is the GraphQL Person.edges.nodes list.
    function graphcanvasLayout(person, edges) {
      const cx = GRAPHCANVAS_W / 2, cy = GRAPHCANVAS_H / 2;
      const center = { id: person.id, kind: 'person', label: person.name || person.id, x: cx, y: cy, me: true };
      const nodes = [center], links = [];
      const artefacts = [], byRef = {};
      (edges || []).forEach(e => {
        const a = e && e.artefact;
        if (!a || !a.ref) return;
        if (!byRef[a.ref]) {
          if (artefacts.length >= GRAPHCANVAS_MAX_ARTEFACTS) return;
          byRef[a.ref] = { ref: a.ref, kind: a.kind || '', predicates: [], people: a.people || [] };
          artefacts.push(byRef[a.ref]);
        }
        if (e.predicate && byRef[a.ref].predicates.indexOf(e.predicate) < 0) byRef[a.ref].predicates.push(e.predicate);
      });
      const others = {}, order = [];
      artefacts.forEach((a, i) => {
        const angle = (2 * Math.PI * i) / artefacts.length - Math.PI / 2;
        a.angle = angle;
        a.x = graphcanvasRound(cx + GRAPHCANVAS_RING_ARTEFACT * Math.cos(angle));
        a.y = graphcanvasRound(cy + GRAPHCANVAS_RING_ARTEFACT * Math.sin(angle) * 0.8);
        nodes.push({ id: a.ref, kind: 'artefact', label: a.ref, x: a.x, y: a.y, artefactKind: a.kind });
        links.push({ from: center.id, to: a.ref, label: a.predicates.join(', ') });
        a.people.forEach(ap => {
          const p = ap && ap.person;
          if (!p || !p.id || p.id === person.id) return;
          if (!others[p.id]) { others[p.id] = { id: p.id, name: p.name || p.id, sin: 0, cos: 0, refs: [] }; order.push(p.id); }
          others[p.id].sin += Math.sin(angle);
          others[p.id].cos += Math.cos(angle);
          others[p.id].refs.push({ ref: a.ref, predicate: ap.predicate });
        });
      });
      order.forEach(id => {
        const o = others[id];
        const angle = Math.atan2(o.sin, o.cos);
        nodes.push({ id: o.id, kind: 'other', label: o.name, x: graphcanvasRound(cx + GRAPHCANVAS_RING_PEOPLE * Math.cos(angle)), y: graphcanvasRound(cy + GRAPHCANVAS_RING_PEOPLE * Math.sin(angle) * 0.8) });
        o.refs.forEach(r => links.push({ from: r.ref, to: o.id, label: r.predicate || '' }));
      });
      return { nodes: nodes, links: links, width: GRAPHCANVAS_W, height: GRAPHCANVAS_H };
    }

    // graphcanvasSvg draws a layout. Text is escaped; another person's node
    // opens their profile.
    function graphcanvasSvg(layout) {
      const pos = {};
      layout.nodes.forEach(n => { pos[n.id] = n; });
      const lines = layout.links.filter(l => pos[l.from] && pos[l.to]).map(l =>
        '<line class="gv-link" x1="' + pos[l.from].x + '" y1="' + pos[l.from].y + '" x2="' + pos[l.to].x + '" y2="' + pos[l.to].y + '"><title>' + escapeHtml(String(l.label || '').replace(/_/g, ' ')) + '</title></line>').join('');
      const dots = layout.nodes.map(n => {
        const r = n.kind === 'person' ? 14 : (n.kind === 'other' ? 10 : 6);
        const open = n.kind === 'other' ? ' data-action="customersOpen" data-arg0="' + escapeHtml(n.id) + '"' : '';
        const dy = n.kind === 'person' ? 26 : 16;
        return '<g class="gv-node gv-' + n.kind + '"' + open + '><circle cx="' + n.x + '" cy="' + n.y + '" r="' + r + '"><title>' + escapeHtml(n.label) + '</title></circle>' +
          '<text class="gv-label" x="' + n.x + '" y="' + graphcanvasRound(n.y + dy) + '" text-anchor="middle">' + escapeHtml(graphcanvasLabel(n.label)) + '</text></g>';
      }).join('');
      return '<svg class="gv" viewBox="0 0 ' + layout.width + ' ' + layout.height + '" role="img" aria-label="Graph of ' + escapeHtml(layout.nodes[0].label) + ' and what they touched">' + lines + dots + '</svg>';
    }
