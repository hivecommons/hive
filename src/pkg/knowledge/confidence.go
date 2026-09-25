package knowledge

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

const legacyFlatConfidence = 0.6

type confidenceInput struct {
	Raw        float64
	HasRaw     bool
	Type       FactType
	Layer      LayerType
	Status     string
	Tags       []string
	Related    []string
	Sources    []Source
	Source     string
	SourceURL  string
	SourceDate time.Time
	ModTime    time.Time
	Accesses   int
}

func applyConfidence(f Fact, in confidenceInput) Fact {
	if in.Type == "" {
		in.Type = f.Type
	}
	if in.Layer == "" {
		in.Layer = f.Layer
	}
	if in.Status == "" {
		in.Status = f.Status
	}
	if len(in.Tags) == 0 {
		in.Tags = f.Tags
	}
	if len(in.Related) == 0 {
		in.Related = f.Related
	}
	if len(in.Sources) == 0 {
		in.Sources = f.Sources
	}
	if in.Raw == 0 && f.Confidence > 0 {
		in.Raw = f.Confidence
		in.HasRaw = true
	}
	f.Confidence, f.ConfidenceScored, f.ConfidenceReason = scoreConfidence(in)
	return f
}

func scoreConfidence(in confidenceInput) (float64, bool, string) {
	sourceKind := confidenceSourceKind(in)
	rawLooksLegacy := in.HasRaw && math.Abs(in.Raw-legacyFlatConfidence) < 0.0001

	if in.HasRaw &&
		sourceKind != "bead" &&
		!(sourceKind == "doc" && rawLooksLegacy) &&
		!(sourceKind == "import" && rawLooksLegacy) &&
		!(sourceKind == "" && rawLooksLegacy) {
		score := clampConfidence(in.Raw)
		return score, true, confidenceReason("stored confidence", in, score)
	}

	var base float64
	switch sourceKind {
	case "doc":
		base = 0.72
		if strings.HasPrefix(strings.ToLower(in.SourceURL), "context7://") {
			base = 0.74
		} else if isHTTPSURL(in.SourceURL) {
			base = 0.73
		}
	case "bead":
		if in.HasRaw && in.Raw > 0 {
			base = in.Raw
		} else {
			base = 0.56
		}
	case "manual", "obsidian":
		if in.HasRaw && in.Raw > 0 {
			score := clampConfidence(in.Raw)
			return score, true, confidenceReason(sourceKind+" frontmatter", in, score)
		}
		return 0, false, "unscored: no confidence or validation signal"
	default:
		if in.HasRaw && in.Raw > 0 && !rawLooksLegacy {
			score := clampConfidence(in.Raw)
			return score, true, confidenceReason("stored confidence", in, score)
		}
		if hasWorkflowConfidenceSignal(in) {
			score := clampConfidence(applyConfidenceAdjustments(0.5, in))
			return score, true, confidenceReason("workflow signal", in, score)
		}
		return 0, false, "unscored: no confidence or validation signal"
	}

	score := clampConfidence(applyConfidenceAdjustments(base, in))
	return score, true, confidenceReason(sourceKind+" provenance", in, score)
}

func confidenceSourceKind(in confidenceInput) string {
	source := strings.ToLower(strings.TrimSpace(in.Source))
	sourceURL := strings.ToLower(strings.TrimSpace(in.SourceURL))
	switch {
	case strings.HasPrefix(source, "doc:"), hasTag(in.Tags, "doc-import"), strings.HasPrefix(sourceURL, "context7://"), isHTTPSURL(sourceURL):
		return "doc"
	case strings.HasPrefix(source, "bead:"):
		return "bead"
	case strings.HasPrefix(source, "obsidian:"):
		return "obsidian"
	case source == "manual":
		return "manual"
	case source == "import":
		return "import"
	default:
		return ""
	}
}

func applyConfidenceAdjustments(score float64, in confidenceInput) float64 {
	switch strings.ToLower(in.Status) {
	case "verified", "validated", "approved":
		score += 0.12
	case "published":
		score += 0.06
	case "draft":
		score -= 0.08
	}

	switch in.Layer {
	case LayerCommunity:
		score += 0.04
	case LayerOrg:
		score += 0.03
	case LayerProject:
		score += 0.02
	}

	if n := len(in.Related); n > 0 {
		score += math.Min(0.06, float64(n)*0.02)
	}
	if n := len(in.Sources); n > 1 {
		score += math.Min(0.06, float64(n-1)*0.02)
	}
	if in.Accesses > 0 {
		score += math.Min(0.05, float64(in.Accesses)*0.01)
	}

	if when := confidenceTime(in); !when.IsZero() {
		age := time.Since(when)
		switch {
		case age < 30*24*time.Hour:
			score += 0.03
		case age < 180*24*time.Hour:
			score += 0.01
		case age > 365*24*time.Hour:
			score -= 0.04
		}
	}
	return score
}

func hasWorkflowConfidenceSignal(in confidenceInput) bool {
	switch strings.ToLower(in.Status) {
	case "verified", "validated", "approved", "published", "draft":
		return true
	}
	return len(in.Related) > 0 || len(in.Sources) > 0 || in.Accesses > 0
}

func confidenceReason(prefix string, in confidenceInput, score float64) string {
	parts := []string{prefix}
	if in.Layer != "" {
		parts = append(parts, "layer "+string(in.Layer))
	}
	if in.Status != "" {
		parts = append(parts, "status "+in.Status)
	}
	if len(in.Related) > 0 {
		parts = append(parts, fmt.Sprintf("%d related link(s)", len(in.Related)))
	}
	if len(in.Sources) > 0 {
		parts = append(parts, fmt.Sprintf("%d source(s)", len(in.Sources)))
	}
	if in.Accesses > 0 {
		parts = append(parts, fmt.Sprintf("%d access(es)", in.Accesses))
	}
	if when := confidenceTime(in); !when.IsZero() {
		parts = append(parts, "freshness "+freshnessBucket(when))
	}
	return fmt.Sprintf("%.0f%%: %s", score*100, strings.Join(parts, ", "))
}

func confidenceTime(in confidenceInput) time.Time {
	if !in.SourceDate.IsZero() {
		return in.SourceDate
	}
	return in.ModTime
}

func freshnessBucket(when time.Time) string {
	age := time.Since(when)
	switch {
	case age < 30*24*time.Hour:
		return "<30d"
	case age < 180*24*time.Hour:
		return "<180d"
	case age > 365*24*time.Hour:
		return ">365d"
	default:
		return "neutral"
	}
}

func clampConfidence(v float64) float64 {
	if v < 0.05 {
		return 0.05
	}
	if v > 0.99 {
		return 0.99
	}
	return math.Round(v*100) / 100
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(tag, want) {
			return true
		}
	}
	return false
}

func isHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Host != ""
}
