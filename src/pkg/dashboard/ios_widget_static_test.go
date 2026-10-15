package dashboard

import (
	"io/fs"
	"strings"
	"testing"
)

// The avatar menu's "iPhone widget" flow fetches the Scriptable script from the
// spoke and hands it to scriptable:///add, so the script must stay embedded and
// the menu entry must keep pointing at the same path.
func TestIOSWidgetScriptEmbeddedAndWired(t *testing.T) {
	script, err := fs.ReadFile(staticFS, "static/ios-widget/hive-widget.js")
	if err != nil {
		t.Fatalf("widget script not embedded: %v", err)
	}
	for _, want := range []string{"queryParameters", "Keychain.set(KEYCHAIN_URL_KEY", "config.runsInWidget"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("widget script missing %q (dashboard deep link relies on it)", want)
		}
	}
	index, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	html := string(index)
	for _, want := range []string{
		`data-action="openIPhoneWidgetModal"`,
		`IPHONE_WIDGET_SCRIPT_PATH = '/ios-widget/hive-widget.js'`,
		`scriptable:///add?scriptName=`,
		`scriptable:///run/`,
		`apps.apple.com/app/scriptable/id1405459188`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
