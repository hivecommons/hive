package dashboard

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyPublicAssetsMatchEmbeddedPublicAllowlist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "proxy", "dashboard_public_assets.json"))
	if err != nil {
		t.Fatalf("reading proxy public asset list: %v", err)
	}
	var proxyAssets []string
	if err := json.Unmarshal(raw, &proxyAssets); err != nil {
		t.Fatalf("parsing proxy public asset list: %v", err)
	}
	if len(proxyAssets) == 0 {
		t.Fatal("proxy public asset list is empty")
	}

	proxySet := map[string]bool{}
	for _, p := range proxyAssets {
		if !strings.HasPrefix(p, "/") {
			t.Fatalf("proxy asset path %q must be absolute", p)
		}
		if strings.ContainsAny(p, "?#") {
			t.Fatalf("proxy asset path %q must not include query or fragment", p)
		}
		if proxySet[p] {
			t.Fatalf("proxy asset path %q is duplicated", p)
		}
		proxySet[p] = true
		if !isPublicPath(p) {
			t.Fatalf("proxy asset path %q is not public in dashboard allowlist", p)
		}
		if _, err := fs.Stat(staticFS, "static"+p); err != nil {
			t.Fatalf("proxy asset path %q is not embedded in dashboard static FS: %v", p, err)
		}
		if !strings.Contains(contributeDashboardAssetLinksHTML, `href="`+p+`"`) {
			t.Fatalf("contribute landing page does not link proxy asset path %q", p)
		}
	}

	if err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		routePath := "/" + strings.TrimPrefix(p, "static/")
		if routePath == "/index.html" || !isPublicPath(routePath) {
			return nil
		}
		if !proxySet[routePath] {
			t.Errorf("embedded public dashboard asset %q is missing from proxy/dashboard_public_assets.json", routePath)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking dashboard static FS: %v", err)
	}
}
