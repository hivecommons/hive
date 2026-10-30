package dashboard

import (
	"strings"
	"testing"
)

// TestToastCountdownBarWiredToDismissTimer asserts the dashboard's timed
// toasts carry a countdown bar whose animation duration is the same number
// of milliseconds as the dismiss timer: both come from toastCountdownBar, so
// a 12 s toast drains over 12 s and a 3 s toast over 3 s. Hover or focus
// inside the toast pauses the bar and the timer together (banking the
// remainder), leaving resumes both. Sticky errors and persistent spinner
// toasts must not grow a bar; reduced motion keeps the (already linear) bar.
func TestToastCountdownBarWiredToDismissTimer(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	helper := extractJSFunction(t, html, "toastCountdownBar")
	for _, want := range []string{
		"toastCountdownCancel(el)",
		"bar.className = 'toast-bar'",
		"bar.setAttribute('aria-hidden', 'true')",
		"el.style.setProperty('--toast-ttl', ms + 'ms')",
		"cd.timer = setTimeout(fire, cd.remaining)",
		"bar.style.animationPlayState = 'running'",
		"cd.remaining = Math.max(0, cd.remaining - (Date.now() - cd.startedAt))",
		"bar.style.animationPlayState = 'paused'",
		"el.addEventListener('mouseenter'",
		"el.addEventListener('mouseleave'",
		"el.addEventListener('focusin'",
		"el.addEventListener('focusout'",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("toastCountdownBar missing %q — the bar and the dismiss timer must share one duration and pause together", want)
		}
	}
	if strings.Contains(helper, "animationDuration") {
		t.Error("toastCountdownBar must publish the lifetime as --toast-ttl on the toast, not an inline animation-duration")
	}

	show := extractJSFunction(t, html, "showToast")
	for _, want := range []string{
		"toastCountdownBar(el, autoDismissMs > 0 ? autoDismissMs : TOAST_DURATION_MS, () => dismissToast(el))",
		"if (autoDismissMs > 0) toastCountdownBar(el, autoDismissMs, () => dismissToast(el, null, null, true))",
	} {
		if !strings.Contains(show, want) {
			t.Errorf("showToast missing %q", want)
		}
	}
	if strings.Contains(show, "setTimeout(() => dismissToast(el)") {
		t.Error("showToast still arms a bare dismiss timer; timed toasts must go through toastCountdownBar")
	}
	// The persistent (spinner) branch keeps its safety-net timeout but no bar.
	persistent := show[strings.Index(show, "if (persistent) {"):strings.Index(show, "if (type === 'error') {")]
	if strings.Contains(persistent, "toastCountdownBar(") {
		t.Error("persistent toasts have no fixed lifetime and must not show a countdown bar")
	}

	dismiss := extractJSFunction(t, html, "dismissToast")
	if !strings.Contains(dismiss, "toastCountdownBar(el, TOAST_DURATION_MS,") {
		t.Error("dismissToast(el, newMsg) rewrites a toast into a timed result and must arm the countdown bar")
	}
	if !strings.Contains(dismiss, "toastCountdownCancel(el);") {
		t.Error("dismissToast must cancel a pending countdown so a stale timer cannot fire after a rewrite or manual close")
	}

	for _, want := range []string{
		".toast-bar { position: absolute;",
		"transform-origin: left center;",
		"animation: toast-bar-drain var(--toast-ttl, 3000ms) linear forwards;",
		"@keyframes toast-bar-drain { from { transform: scaleX(1); } to { transform: scaleX(0); } }",
		"@media (prefers-reduced-motion: reduce) { .toast-bar { animation-timing-function: linear; } }",
		".toast.success .toast-bar { background: color-mix(in srgb, var(--green) 40%, transparent); }",
		".toast.error .toast-bar { background: color-mix(in srgb, var(--red) 40%, transparent); }",
		".toast.warning .toast-bar { background: color-mix(in srgb, var(--status-warn) 40%, transparent); }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("toast countdown CSS missing %q", want)
		}
	}
	if strings.Contains(html, ".toast-bar { display: none; }") {
		t.Error("reduced motion must keep the countdown bar: it is information, not decoration")
	}

	// Stage-tracker action-result toasts: informational ones close after
	// STAGE_TRACKER_RESULT_TOAST_MS on the same countdown; errors and
	// toasts offering next actions stay sticky.
	if !strings.Contains(html, "const STAGE_TRACKER_RESULT_TOAST_MS = 15000;") {
		t.Error("result toasts need a named 15s lifetime constant")
	}
	result := extractJSFunction(t, html, "stageTrackerResultToast")
	if !strings.Contains(result, "if (stageTrackerResultToastTimed(text)) toastCountdownBar(el, STAGE_TRACKER_RESULT_TOAST_MS, () => dismissToast(el, null, null, true));") {
		t.Error("stageTrackerResultToast must arm the shared countdown for timed results, dismissing through the sticky guard")
	}
	timed := extractJSFunction(t, html, "stageTrackerResultToastTimed")
	if !strings.Contains(timed, "text.type !== 'error' && !(text.options && text.options.length)") {
		t.Error("error results and results offering next actions must stay sticky")
	}
}
