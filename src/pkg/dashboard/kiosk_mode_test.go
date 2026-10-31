package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestKioskModeWiring guards the `?kiosk=1` display mode the headless Twitch
// streamer (src/deploy/stream) loads: the flag is read before first paint, the
// chrome and every non-tracker section are removed from flow, and the two
// dialogs that would otherwise park on an unattended screen are suppressed.
func TestKioskModeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`searchParams.get('kiosk')==='1'`,
		`window.hiveKioskMode=function(){ return !!window.HIVE_KIOSK; };`,
		`document.body.classList.add('kiosk-mode');`,
		`#section-activity-monitor-section'`,
		`body.kiosk-mode .oc-sidebar, body.kiosk-mode .oc-sidebar-resize, body.kiosk-mode .oc-topbar,`,
		// Must out-specify theme.go's layoutGuardCSS, which forces the topbar visible with id specificity.
		`html body.kiosk-mode #oc-topbar[id] { display: none !important; }`,
		`body.kiosk-mode #hive-dashboard-root > [data-dashboard-section]:not(#activity-monitor-section) { display: none !important; }`,
		`if (!_welcomeState.dismissed && !(window.hiveKioskMode && window.hiveKioskMode())) openWelcomeDialog();`,
		`if (window.hiveKioskMode && window.hiveKioskMode()) return;`,
		`if (!el._countdownWired && !(window.hiveKioskMode && window.hiveKioskMode())) {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing kiosk-mode wiring %q", want)
		}
	}
	if strings.Contains(html, `localStorage.setItem('hive-kiosk`) {
		t.Error("kiosk mode must not persist state in localStorage")
	}
}

// TestDisplayModeWiring guards the attended `?display=1` wall view (#15925):
// boot.js reads the flag before first paint and sets body.display-mode, kiosk
// wins when both flags are present so the headless streamer's output never
// changes, the 📺 header tool is a .ham-header-tool that kiosk hides, and
// entering display mode never requests full screen or a wake lock.
func TestDisplayModeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`display=u.searchParams.get('display')==='1';`,
		`window.HIVE_DISPLAY=display&&!kiosk;`,
		`window.hiveDisplayMode=function(){ return !!window.HIVE_DISPLAY; };`,
		"if(window.HIVE_DISPLAY){\n    document.body.classList.add('display-mode');",
		`class="config-info section-settings-gear ham-header-tool ham-display-btn" id="activity-monitor-display"`,
		`body.kiosk-mode .ham-header-tool, body.kiosk-mode .ham-popout-return { display: none !important; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing display-mode wiring %q", want)
		}
	}
	// Kiosk keeps its own branch: display mode never adds popout-mode or
	// touches the kiosk class.
	if !strings.Contains(html, "if(kiosk){\n    document.body.classList.add('kiosk-mode');\n  }else if(window.HIVE_POPOUT){") {
		t.Error("boot.js kiosk/popout branch changed")
	}
	for _, fn := range []string{"toggleStageTrackerDisplay", "stageTrackerDisplayUrl", "stageTrackerDisplayHotkey"} {
		if body := jsFunc(t, html, fn); strings.Contains(body, "requestFullscreen") || strings.Contains(body, "wakeLock") {
			t.Errorf("%s must not request full screen or a wake lock:\n%s", fn, body)
		}
	}
	if strings.Contains(html, `localStorage.setItem('hive-display`) {
		t.Error("display mode must not persist state in localStorage")
	}
}

// TestDisplayOptionsWiring guards the display-mode wall options (#15931):
// `density=tv` and `shift=1` set their body classes only inside the
// display-mode branch of boot.js, so `?density=tv` or `?shift=1` without
// `display=1` (and any kiosk URL, where display mode is off) renders exactly
// as before, and `density` accepts only `tv`.
func TestDisplayOptionsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`density=u.searchParams.get('density')==='tv'?'tv':'';`,
		`shift=u.searchParams.get('shift')==='1';`,
		`window.HIVE_DISPLAY_DENSITY=window.HIVE_DISPLAY?density:'';`,
		`window.HIVE_DISPLAY_SHIFT=window.HIVE_DISPLAY&&shift;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing display option wiring %q", want)
		}
	}
	branch := "if(window.HIVE_DISPLAY){\n    document.body.classList.add('display-mode');\n    document.body.classList.add('ham-chromeless');\n" +
		"    if(window.HIVE_DISPLAY_DENSITY==='tv') document.body.classList.add('display-density-tv');\n" +
		"    if(window.HIVE_DISPLAY_SHIFT) document.body.classList.add('display-shift');\n"
	if !strings.Contains(html, branch) {
		t.Error("display-density-tv / display-shift must be added only inside boot.js's display-mode branch")
	}
	for _, cls := range []string{"display-density-tv", "display-shift"} {
		if n := strings.Count(html, "classList.add('"+cls+"')"); n != 1 {
			t.Errorf("%s added %d times; want exactly once, in the display-mode branch", cls, n)
		}
	}
}

// TestKioskSoundDefault guards the streamer's audio path: in kiosk mode the
// tracker sounds are on unless the URL says `&sound=off`, the context is
// resumed at init without waiting for a gesture (the streamer's Chromium
// allows autoplay), the held-back toast never reaches the stream, and no
// sound/volume preference is written to localStorage.
func TestKioskSoundDefault(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`sound=new URL(window.location.href).searchParams.get('sound')!=='off';`,
		`window.HIVE_KIOSK_SOUND=kiosk&&sound;`,
		`window.hiveKioskSoundOn=function(){ return !!window.HIVE_KIOSK_SOUND; };`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing kiosk sound wiring %q", want)
		}
	}
	if init := jsFunc(t, html, "stageTrackerSoundInit"); !strings.Contains(init, "if (window.hiveKioskMode && window.hiveKioskMode()) { stageTrackerKioskSoundInit(); return; }") {
		t.Errorf("stageTrackerSoundInit must branch to the kiosk path before touching localStorage:\n%s", init)
	}
	kioskInit := jsFunc(t, html, "stageTrackerKioskSoundInit")
	if strings.Contains(kioskInit, "localStorage") || !strings.Contains(kioskInit, "stageTrackerResumeContext(") {
		t.Errorf("stageTrackerKioskSoundInit must resume the context and never read localStorage:\n%s", kioskInit)
	}
	if held := jsFunc(t, html, "stageTrackerSoundHeldNotice"); !strings.Contains(held, "if (window.HIVE_KIOSK_SOUND) return;") {
		t.Errorf("stageTrackerSoundHeldNotice must be a no-op on a kiosk:\n%s", held)
	}
	for _, fn := range []string{"stageTrackerSetSoundOn", "stageTrackerSetVolume"} {
		if body := jsFunc(t, html, fn); !strings.Contains(body, "if (!(window.hiveKioskMode && window.hiveKioskMode())) { try { localStorage.setItem(") {
			t.Errorf("%s must not write localStorage in kiosk mode:\n%s", fn, body)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — kiosk sound default was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "stageTrackerKioskSoundDefault"),
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1'), true, 'kiosk default is on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1&sound=off'), false, 'sound=off silences');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1&sound=on'), true, 'any other value is on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, ''), true, 'no query → on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(false, '?sound=on'), false, 'never outside kiosk mode');",
	}, "\n")
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node kiosk sound default fixture failed: %v\n%s", err, out)
	}
}
