package dashboard

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestFAQConfigKeysExistInSchema(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static/index.html: %v", err)
	}
	panel := faqPanel(t, string(html))
	keys := faqConfigKeys(panel)
	if len(keys) == 0 {
		t.Fatal("FAQ panel did not reference any dotted config keys")
	}

	known := map[string]struct{}{}
	collectYAMLKeys("", reflect.TypeOf(config.Config{}), known)
	for _, key := range keys {
		if _, ok := known[key]; !ok {
			t.Errorf("FAQ references config key %q, but config.Config has no matching yaml tag path", key)
		}
	}
}

func faqPanel(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, `<div id="faq-panel"`)
	if start < 0 {
		t.Fatal("static/index.html is missing #faq-panel")
	}
	end := strings.Index(html[start:], `</div><!-- /#hive-dashboard-root -->`)
	if end < 0 {
		t.Fatal("static/index.html is missing dashboard root close after #faq-panel")
	}
	return html[start : start+end]
}

func faqConfigKeys(panel string) []string {
	re := regexp.MustCompile(`<code>([a-z0-9_]+(?:\.[a-z0-9_]+)+)</code>`)
	matches := re.FindAllStringSubmatch(panel, -1)
	seen := map[string]struct{}{}
	for _, match := range matches {
		seen[match[1]] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	return keys
}

func collectYAMLKeys(prefix string, typ reflect.Type, keys map[string]struct{}) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("yaml")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" || name == "inline" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		keys[key] = struct{}{}
		next := field.Type
		for next.Kind() == reflect.Pointer || next.Kind() == reflect.Slice || next.Kind() == reflect.Array {
			next = next.Elem()
		}
		if next.Kind() == reflect.Map {
			next = next.Elem()
		}
		collectYAMLKeys(key, next, keys)
	}
}
