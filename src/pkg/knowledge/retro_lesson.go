package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	RetroLessonMinChars   = 25
	RetroLessonMaxChars   = 500
	retroLessonConfidence = 0.7
	retroLessonLayer      = "project"
	retroLessonSearchCap  = 5
)

// RetroLesson is a durable, model-generated process lesson produced by the
// retro lane from a completed bead/PR trajectory.
type RetroLesson struct {
	Lesson     string
	SourceBead string
	SourcePR   string
	SourceDate time.Time
}

// IngestRetroLesson quality-gates, deduplicates, and stores one retro lesson
// as a normal knowledge fact. It returns created=false for rejected or duplicate
// lessons so retro analysis can fail open without affecting deterministic beads.
func (k *KnowledgeAPI) IngestRetroLesson(ctx context.Context, lesson RetroLesson) (slug string, created bool, err error) {
	if k == nil {
		return "", false, nil
	}
	body := strings.TrimSpace(lesson.Lesson)
	if !ValidRetroLesson(body) {
		return "", false, nil
	}
	key := normalizedRetroLessonKey(body)
	slug = "retro-lesson-" + shortHash(key)
	if _, err := k.VaultFact(slug); err == nil {
		return slug, false, nil
	}
	for _, existing := range k.SearchAllWithVaults(ctx, body, "", retroLessonSearchCap) {
		if nearRetroLessonKey(normalizedRetroLessonKey(existing.Body), key) || nearRetroLessonKey(normalizedRetroLessonKey(existing.Title), key) {
			return existing.Slug, false, nil
		}
	}

	title := retroLessonTitle(body)
	fact := ExtractedFact{
		Title:      title,
		Body:       body,
		Type:       FactPattern,
		Confidence: retroLessonConfidence,
		Tags:       []string{"retro", "lesson"},
		SourcePR:   retroSource(lesson),
		SourceDate: lesson.SourceDate,
	}
	if fact.SourceDate.IsZero() {
		fact.SourceDate = time.Now().UTC()
	}

	if client := k.clientForLayer(LayerType(retroLessonLayer)); client != nil {
		if err := client.IngestFacts(ctx, []ExtractedFact{fact}); err != nil {
			return "", false, err
		}
		k.addRetroGraphTriple(slug, lesson)
		return slug, true, nil
	}

	if err := k.writeRetroLessonFile(slug, fact); err != nil {
		return "", false, err
	}
	k.addRetroGraphTriple(slug, lesson)
	return slug, true, nil
}

func ValidRetroLesson(lesson string) bool {
	lesson = strings.TrimSpace(lesson)
	n := len([]rune(lesson))
	if n < RetroLessonMinChars || n > RetroLessonMaxChars {
		return false
	}
	return !logscrub.TokenPattern.MatchString(lesson)
}

func (k *KnowledgeAPI) writeRetroLessonFile(slug string, fact ExtractedFact) error {
	dir := filepath.Join(knowledgeBaseDir, retroLessonLayer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating retro lesson dir: %w", err)
	}
	path := filepath.Join(dir, strings.ReplaceAll(slug+".md", "/", "_"))
	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: %s\n", fact.Title)
	fmt.Fprintf(&buf, "type: %s\n", string(fact.Type))
	fmt.Fprintf(&buf, "layer: %s\n", retroLessonLayer)
	fmt.Fprintf(&buf, "confidence: %.2f\n", fact.Confidence)
	fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(fact.Tags, ", "))
	fmt.Fprintf(&buf, "source: %s\n", fact.SourcePR)
	fmt.Fprintf(&buf, "synthesized: %s\n", fact.SourceDate.UTC().Format(time.RFC3339))
	buf.WriteString("---\n\n")
	buf.WriteString(fact.Body)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("writing retro lesson file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming retro lesson file: %w", err)
	}
	k.triggerVaultReindex(dir)
	return nil
}

func (k *KnowledgeAPI) addRetroGraphTriple(slug string, lesson RetroLesson) {
	gs := k.GraphStore()
	if gs == nil {
		return
	}
	source := strings.TrimSpace(lesson.SourcePR)
	if source == "" {
		source = strings.TrimSpace(lesson.SourceBead)
	}
	if source == "" {
		return
	}
	if err := gs.AddTriple(Triple{Subject: slug, Predicate: PredicateDerivedFrom, Object: "retro:" + source}); err != nil {
		k.logger.Warn("failed to add retro lesson graph triple", "slug", slug, "source", source, "error", err)
	}
}

func retroLessonTitle(body string) string {
	words := strings.Fields(body)
	if len(words) > 12 {
		words = words[:12]
	}
	title := strings.Join(words, " ")
	title = strings.Trim(title, " .")
	if title == "" {
		title = "Generalized retro lesson"
	}
	return "Retro lesson: " + title
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

func normalizedRetroLessonKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlphaNum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func nearRetroLessonKey(existing, candidate string) bool {
	if existing == "" || candidate == "" {
		return false
	}
	return existing == candidate || strings.Contains(existing, candidate) || strings.Contains(candidate, existing)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func retroSource(lesson RetroLesson) string {
	parts := []string{"retro"}
	if lesson.SourceBead != "" {
		parts = append(parts, "bead:"+lesson.SourceBead)
	}
	if lesson.SourcePR != "" {
		parts = append(parts, "pr:"+lesson.SourcePR)
	}
	return strings.Join(parts, " ")
}
