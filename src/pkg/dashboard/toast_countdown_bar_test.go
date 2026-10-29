package dashboard

import (
	"strings"
	"testing"
)

// TestToastCountdownBarWiredToDismissTimer asserts the dashboard's timed
// toasts carry a countdown bar whose animation duration is the same number
// of milliseconds as the dismiss timer: both come from toastCountdownBar, so
// a 12 s toast drains over 12 s and a 3 s toast over 3 s. Sticky errors and
// persistent spinner toasts must not grow a bar, and reduced-motion hides it.
func TestToastCountdownBarWiredToDismissTimer(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	helper := extractJSFunction(t, html, "toastCountdownBar")
	for _, want := range []string{
		"bar.className = 'toast-bar'",
		"bar.style.animationDuration = ms + 'ms'",
		"setTimeout(() => { if (el.parentNode) onExpire(); }, ms)",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("toastCountdownBar missing %q — the bar and the dismiss timer must share one duration", want)
		}
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

	for _, want := range []string{
		".toast-bar { position: absolute;",
		"animation: toast-bar-drain linear forwards;",
		"@keyframes toast-bar-drain { from { transform: scaleX(1); } to { transform: scaleX(0); } }",
		"@media (prefers-reduced-motion: reduce) { .toast-bar { display: none; } }",
		".toast.success .toast-bar { background: var(--green); }",
		".toast.error .toast-bar { background: var(--red); }",
		".toast.warning .toast-bar { background: var(--status-warn); }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("toast countdown CSS missing %q", want)
		}
	}
}
