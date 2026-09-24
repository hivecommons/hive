package theme

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed themes/*.yaml themes/README.md
var embeddedThemes embed.FS

var legacyThemeAliases = map[string]string{
	"openclaw":       "hive-dark",
	"openclaw-light": "hive-light",
	"honeycomb":      "hive",
	"graphite":       "hive-dark",
	"dracula":        "cyberpunk",
	"github-light":   "hive-light",
	"high-contrast":  "terminal",
}

var (
	catalogOnce sync.Once
	catalog     []Theme
	catalogErr  error
)

func Catalog() []Theme {
	catalogOnce.Do(func() { catalog, catalogErr = LoadCatalog() })
	out := make([]Theme, len(catalog))
	copy(out, catalog)
	return out
}

func CatalogError() error {
	catalogOnce.Do(func() { catalog, catalogErr = LoadCatalog() })
	return catalogErr
}

func LoadCatalog() ([]Theme, error) {
	entries, err := embeddedThemes.ReadDir("themes")
	if err != nil {
		return nil, fmt.Errorf("read embedded themes: %w", err)
	}
	seen := map[string]string{}
	themes := make([]Theme, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		file := path.Join("themes", name)
		b, err := embeddedThemes.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		var th Theme
		if err := yaml.Unmarshal(b, &th); err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		if th.ID == "" {
			th.ID = strings.TrimSuffix(name, ".yaml")
		}
		if prev := seen[th.ID]; prev != "" {
			return nil, fmt.Errorf("duplicate theme id %q in %s and %s", th.ID, prev, file)
		}
		seen[th.ID] = file
		if err := ValidateTheme(th); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		themes = append(themes, th)
	}
	if len(themes) == 0 {
		return nil, fmt.Errorf("no embedded themes found")
	}
	sort.Slice(themes, func(i, j int) bool { return themes[i].ID < themes[j].ID })
	return themes, nil
}

func CanonicalID(id string) string {
	return canonicalThemeID(id)
}

func canonicalThemeID(id string) string {
	id = strings.TrimSpace(id)
	if alias := legacyThemeAliases[id]; alias != "" {
		return alias
	}
	return id
}
