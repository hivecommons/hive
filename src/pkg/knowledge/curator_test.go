package knowledge

import "testing"

func TestExtractTags(t *testing.T) {
	comment := "This React component uses TypeScript and needs a test with Docker."
	tags := extractTags(comment)

	expected := map[string]bool{
		"react":      false,
		"typescript": false,
		"testing":    false,
		"docker":     false,
	}

	for _, tag := range tags {
		if _, ok := expected[tag]; ok {
			expected[tag] = true
		}
	}

	for tag, found := range expected {
		if !found {
			t.Errorf("expected tag %q not found in %v", tag, tags)
		}
	}
}

func TestExtractTagsDedup(t *testing.T) {
	comment := "go and golang are the same language for testing purposes"
	tags := extractTags(comment)

	goCount := 0
	for _, tag := range tags {
		if tag == "go" {
			goCount++
		}
	}
	if goCount != 1 {
		t.Errorf("expected 1 'go' tag, got %d in %v", goCount, tags)
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("always use guards", "always", "never") {
		t.Error("should match 'always'")
	}
	if containsAny("looks good", "always", "never") {
		t.Error("should not match")
	}
}
