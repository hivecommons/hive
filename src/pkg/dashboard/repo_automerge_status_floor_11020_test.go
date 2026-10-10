package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#11020: the repo-card auto-merge switch snapped back to its
// old state after a toggle. The handler kicked off an async rebuild but never
// returned the post-mutation StatusSeq floor (#4348), so the browser's
// immediate /api/status refetch got the pre-mutation snapshot and rendered the
// old autoMerge value. Pause/resume had the same gap.

func publishedStatusSeq(t *testing.T, srv *Server) uint64 {
	t.Helper()
	srv.UpdateStatus(&StatusPayload{})
	srv.statusMu.RLock()
	defer srv.statusMu.RUnlock()
	return srv.status.StatusSeq
}

func assertMinStatusSeqAbove(t *testing.T, body map[string]any, preSeq uint64) {
	t.Helper()
	raw, ok := body["minStatusSeq"]
	if !ok {
		t.Fatalf("response %v carries no minStatusSeq — the browser cannot reject the pre-mutation /api/status", body)
	}
	got, ok := raw.(float64)
	if !ok || uint64(got) <= preSeq {
		t.Fatalf("minStatusSeq = %v, want > pre-mutation snapshot seq %d", raw, preSeq)
	}
}

func TestRepoAutoMergeResponseCarriesMinStatusSeq(t *testing.T) {
	srv := newFullServer(t)
	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level

	for _, enabled := range []string{"false", "true"} {
		preSeq := publishedStatusSeq(t, srv)
		w := postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":`+enabled+`}`, true)
		if w.Code != 200 {
			t.Fatalf("auto-merge enabled=%s: code = %d body = %s", enabled, w.Code, w.Body.String())
		}
		assertMinStatusSeqAbove(t, decodeRepoPauseBody(t, w), preSeq)
	}
}

func TestRepoPauseResumeResponsesCarryMinStatusSeq(t *testing.T) {
	srv := newFullServer(t)

	preSeq := publishedStatusSeq(t, srv)
	w := postRepoPause(t, srv, "/api/repos/pause", `{"repo":"testrepo","reason":"freeze"}`, true)
	if w.Code != 200 {
		t.Fatalf("pause: code = %d body = %s", w.Code, w.Body.String())
	}
	assertMinStatusSeqAbove(t, decodeRepoPauseBody(t, w), preSeq)

	preSeq = publishedStatusSeq(t, srv)
	w = postRepoPause(t, srv, "/api/repos/resume", `{"repo":"testrepo"}`, true)
	if w.Code != 200 {
		t.Fatalf("resume: code = %d body = %s", w.Code, w.Body.String())
	}
	assertMinStatusSeqAbove(t, decodeRepoPauseBody(t, w), preSeq)
}

func TestRepoToggleClientsRaiseStatusFloor(t *testing.T) {
	html := indexHTML(t)
	for _, fn := range []string{"toggleRepoAutoMerge", "toggleRepoPause"} {
		body := jsFunc(t, html, fn)
		note := strings.Index(body, "noteStatusMutation(data.minStatusSeq)")
		refetch := strings.Index(body, "fetch('/api/status')")
		if note < 0 || refetch < 0 || note > refetch {
			t.Fatalf("%s must call noteStatusMutation(data.minStatusSeq) before refetching /api/status", fn)
		}
	}
	modal := jsFunc(t, html, "maybeShowLevelAutoMergeActiveModal")
	note := strings.Index(modal, "noteStatusMutation(resBody.minStatusSeq)")
	refetch := strings.Index(modal, "fetch('/api/status')")
	if note < 0 || refetch < 0 || note > refetch {
		t.Fatal("Level-6 modal auto-merge switch must raise the status floor before refetching /api/status")
	}
}

// A stale /api/status (statusSeq below the returned minStatusSeq) must not
// repaint the switch back to its pre-toggle state, in either direction.
func TestRepoAutoMergeStaleStatusDoesNotRepaintSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the auto-merge stale-snapshot rule was NOT executed by this run")
	}
	html := indexHTML(t)
	script := "let _statusSeqFloor = 0;\n" +
		"function showToast(){}\n" +
		jsFunc(t, html, "noteStatusMutation") + "\n" +
		jsFunc(t, html, "toggleRepoAutoMerge") + "\n" +
		autoMergeStaleSnapshotAssertions
	path := filepath.Join(t.TempDir(), "automerge-floor.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("auto-merge stale-snapshot check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const autoMergeStaleSnapshotAssertions = `
let renders = 0;
let btn;
let staleAutoMerge;
function makeSwitch(on) {
  const classes = new Set(['config-toggle-switch']);
  if (on) classes.add('on');
  return {
    dataset: {}, textContent: '', attrs: {},
    classList: {
      contains: c => classes.has(c),
      toggle: (c, force) => { if (force) classes.add(c); else classes.delete(c); },
    },
    setAttribute(k, v) { this.attrs[k] = v; },
  };
}
// Mirrors the render() stale-snapshot guard, then repaints the switch from the
// snapshot exactly as renderRepos would.
function render(data) {
  if (data.statusSeq != null && data.statusSeq < _statusSeqFloor) return;
  renders++;
  const on = data.repos[0].autoMerge;
  btn.classList.toggle('on', on);
  btn.setAttribute('aria-checked', on ? 'true' : 'false');
}
function fetch(url, opts) {
  if (opts) return Promise.resolve({ ok: true, json: () => Promise.resolve({ ok: true, minStatusSeq: 9 }) });
  return Promise.resolve({ ok: true, json: () => Promise.resolve({ statusSeq: 8, repos: [{ autoMerge: staleAutoMerge }] }) });
}
const flush = () => new Promise(r => setTimeout(r, 0));
(async () => {
  for (const was of [true, false]) {
    _statusSeqFloor = 0; renders = 0; staleAutoMerge = was;
    btn = makeSwitch(was);
    await toggleRepoAutoMerge('o/r', was, btn);
    await flush(); await flush();
    const on = btn.classList.contains('on');
    if (_statusSeqFloor !== 9 || renders !== 0 || on === was || btn.attrs['aria-checked'] !== String(!was)) {
      console.log(JSON.stringify({ was, floor: _statusSeqFloor, renders, on, attrs: btn.attrs }));
      process.exit(1);
    }
  }
  console.log('ok');
})().catch(e => { console.log(e && e.stack || e); process.exit(1); });
`
