package knowledge

import (
	"strings"
	"time"
)

// ExtractedFact is a fact candidate submitted to the wiki ingest API
// (POST /api/ingest, see api.go and client.IngestFacts).
type ExtractedFact struct {
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Type       FactType  `json:"type"`
	Confidence float64   `json:"confidence"`
	Tags       []string  `json:"tags"`
	Related    []string  `json:"related,omitempty"`
	SourcePR   string    `json:"source_pr"`
	SourceDate time.Time `json:"source_date"`
}

// containsAny reports whether s contains any of the needles. Used by the
// bead synthesizer's keyword classifier (classifyByKeywords).
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// extractTags derives topic tags from free-form text by keyword matching.
func extractTags(comment string) []string {
	lower := strings.ToLower(comment)
	var tags []string

	tagKeywords := map[string]string{
		"typescript": "typescript",
		"react":      "react",
		"go ":        "go",
		"golang":     "go",
		"test":       "testing",
		"ci":         "ci",
		"docker":     "docker",
		"kubernetes": "kubernetes",
		"k8s":        "kubernetes",
		"helm":       "helm",
		"security":   "security",
		"auth":       "auth",
	}

	seen := make(map[string]bool)
	for keyword, tag := range tagKeywords {
		if strings.Contains(lower, keyword) && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	return tags
}
